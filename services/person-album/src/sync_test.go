package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeImmich serves the endpoints the syncer uses from an in-memory library.
type fakeImmich struct {
	mu       sync.Mutex
	albums   []album
	people   []person
	assets   []Asset             // in library order
	faces    map[string][]string // asset ID -> person IDs
	inAlbum  map[string][]string // album ID -> asset IDs
	rejectAs map[string]string   // asset ID -> BulkIdErrorReason
	failPUT  bool                // the whole PUT answers 500
	pageSize int
	searches [][]string // personIds of each search, page 1 only
	sent     []string   // asset IDs in every PUT, in order
}

func newFake() *fakeImmich {
	return &fakeImmich{
		albums:   []album{{ID: "alb-1", AlbumName: "Max"}},
		people:   []person{{ID: "p-max", Name: "Max"}, {ID: "p-kate", Name: "Kate"}},
		faces:    map[string][]string{},
		inAlbum:  map[string][]string{},
		rejectAs: map[string]string{},
		pageSize: 2,
	}
}

func (f *fakeImmich) add(id string, people ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assets = append(f.assets, Asset{ID: id, OriginalFileName: id + ".jpg"})
	f.faces[id] = people
}

func (f *fakeImmich) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("x-api-key") != "key" {
		http.Error(w, `{"message":"Invalid API key"}`, http.StatusUnauthorized)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
		json.NewEncoder(w).Encode(f.albums)

	case r.Method == http.MethodGet && r.URL.Path == "/api/people":
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start := min((page-1)*f.pageSize, len(f.people))
		end := min(start+f.pageSize, len(f.people))
		json.NewEncoder(w).Encode(map[string]any{
			"people":      f.people[start:end],
			"hasNextPage": end < len(f.people),
		})

	case r.Method == http.MethodPost && r.URL.Path == "/api/search/metadata":
		var req struct {
			PersonIDs []string `json:"personIds"`
			Page      int      `json:"page"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Page == 1 {
			f.searches = append(f.searches, req.PersonIDs)
		}
		var ids []Asset
		for _, a := range f.assets {
			if !slices.ContainsFunc(req.PersonIDs, func(p string) bool { return !slices.Contains(f.faces[a.ID], p) }) {
				ids = append(ids, a)
			}
		}
		start := min((req.Page-1)*f.pageSize, len(ids))
		end := min(start+f.pageSize, len(ids))
		var resp struct {
			Assets struct {
				Items    []Asset `json:"items"`
				NextPage *string `json:"nextPage"`
			} `json:"assets"`
		}
		resp.Assets.Items = append([]Asset{}, ids[start:end]...)
		if end < len(ids) {
			next := strconv.Itoa(req.Page + 1)
			resp.Assets.NextPage = &next
		}
		json.NewEncoder(w).Encode(resp)

	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/albums/"):
		albumID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/albums/"), "/assets")
		var req struct {
			IDs []string `json:"ids"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.sent = append(f.sent, req.IDs...)
		if f.failPUT {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		out := []AddResult{}
		for _, id := range req.IDs {
			switch {
			case f.rejectAs[id] != "":
				out = append(out, AddResult{ID: id, Error: f.rejectAs[id]})
			case slices.Contains(f.inAlbum[albumID], id):
				out = append(out, AddResult{ID: id, Error: "duplicate"})
			default:
				f.inAlbum[albumID] = append(f.inAlbum[albumID], id)
				out = append(out, AddResult{ID: id, Success: true})
			}
		}
		json.NewEncoder(w).Encode(out)

	default:
		http.NotFound(w, r)
	}
}

func (f *fakeImmich) album() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.inAlbum["alb-1"])
}

func (f *fakeImmich) removeFromAlbum(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inAlbum["alb-1"] = slices.DeleteFunc(f.inAlbum["alb-1"], func(s string) bool { return s == id })
}

type harness struct {
	t    *testing.T
	fake *fakeImmich
	s    *Syncer
	job  Job
	now  time.Time
}

func newHarness(t *testing.T, job Job) *harness {
	t.Helper()
	fake := newFake()
	srv := httptest.NewTestServer(t, fake)

	cfg := defaultConfig()
	cfg.DryRun = false
	cfg.State.Path = filepath.Join(t.TempDir(), "state", "state.db")
	cfg.State.MaxAttempts = 2
	cfg.Jobs = []Job{job}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(cfg.State.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	h := &harness{t: t, fake: fake, job: job, now: time.Now()}
	h.s = &Syncer{
		cfg:    cfg,
		client: func(key string) *Immich { return NewImmich("http://immich", key, srv.Client()) },
		store:  store,
		log:    slog.New(slog.DiscardHandler),
		now:    func() time.Time { return h.now },
	}
	return h
}

func maxJob() Job {
	return Job{Name: "max", APIKey: "key", People: []string{"Max"}, Match: MatchAny, Album: "Max"}
}

func (h *harness) pass() PassResult {
	h.t.Helper()
	res, err := h.s.Pass(context.Background(), h.job)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func TestAddsMatchesAcrossPages(t *testing.T) {
	h := newHarness(t, maxJob())
	h.fake.add("a1", "p-max")
	h.fake.add("a2", "p-max", "p-kate")
	h.fake.add("a3", "p-kate")
	h.fake.add("a4", "p-max")

	res := h.pass()
	if res.Added != 3 {
		t.Fatalf("added %d, want 3", res.Added)
	}
	if got, want := h.fake.album(), []string{"a1", "a2", "a4"}; !slices.Equal(got, want) {
		t.Fatalf("album %v, want %v", got, want)
	}
}

func TestAnyMergesPeople(t *testing.T) {
	job := maxJob()
	job.People = []string{"Max", "Kate"}
	h := newHarness(t, job)
	h.fake.add("a1", "p-max")
	h.fake.add("a2", "p-max", "p-kate")
	h.fake.add("a3", "p-kate")
	h.fake.add("a4")

	h.pass()
	if got, want := h.fake.album(), []string{"a1", "a2", "a3"}; !slices.Equal(got, want) {
		t.Fatalf("album %v, want %v", got, want)
	}
	if got := h.fake.searches; len(got) != 2 || len(got[0]) != 1 || len(got[1]) != 1 {
		t.Fatalf("searches %v, want one per person", got)
	}
}

func TestAllNeedsEveryone(t *testing.T) {
	job := maxJob()
	job.People = []string{"Max", "p-kate"} // a name and an ID
	job.Match = MatchAll
	h := newHarness(t, job)
	h.fake.add("a1", "p-max")
	h.fake.add("a2", "p-max", "p-kate")
	h.fake.add("a3", "p-kate")

	h.pass()
	if got, want := h.fake.album(), []string{"a2"}; !slices.Equal(got, want) {
		t.Fatalf("album %v, want %v", got, want)
	}
	if got := h.fake.searches; len(got) != 1 || !slices.Equal(got[0], []string{"p-max", "p-kate"}) {
		t.Fatalf("searches %v, want one with both people", got)
	}
}

func TestRemovedStaysRemoved(t *testing.T) {
	h := newHarness(t, maxJob())
	h.fake.add("a1", "p-max")
	h.fake.add("a2", "p-max")
	h.pass()

	h.fake.removeFromAlbum("a1")
	res := h.pass()
	if res.Added != 0 || res.Skipped != 2 {
		t.Fatalf("second pass %+v", res)
	}
	if got := h.fake.album(); !slices.Equal(got, []string{"a2"}) {
		t.Fatalf("album %v, removed photo came back", got)
	}
	if got := h.fake.sent; len(got) != 2 {
		t.Fatalf("sent %v, want only the first pass", got)
	}
}

func TestDuplicateCountsAsDone(t *testing.T) {
	h := newHarness(t, maxJob())
	h.fake.add("a1", "p-max")
	h.fake.inAlbum["alb-1"] = []string{"a1"}

	res := h.pass()
	if res.Added != 0 || res.Failed != 0 || res.Skipped != 1 {
		t.Fatalf("pass %+v", res)
	}
	rec, ok, err := h.s.store.Lookup(context.Background(), "max", "a1")
	if err != nil || !ok || rec.Status != StatusDone {
		t.Fatalf("record %+v, %v, %v", rec, ok, err)
	}
}

func TestRejectedIsRetriedThenParked(t *testing.T) {
	h := newHarness(t, maxJob())
	h.fake.add("a1", "p-max")
	h.fake.add("a2", "p-max")
	h.fake.rejectAs["a1"] = "no_permission"

	if res := h.pass(); res.Added != 1 || res.Failed != 1 {
		t.Fatalf("first pass %+v", res)
	}
	// Still in backoff.
	if res := h.pass(); res.Failed != 0 || res.Skipped != 2 {
		t.Fatalf("second pass %+v", res)
	}

	h.now = h.now.Add(time.Hour)
	if res := h.pass(); res.Failed != 1 {
		t.Fatalf("third pass %+v", res)
	}
	rec, _, _ := h.s.store.Lookup(context.Background(), "max", "a1")
	if rec.Status != StatusFailedPermanent || rec.Attempts != 2 || !strings.Contains(rec.LastError, "no_permission") {
		t.Fatalf("record %+v", rec)
	}

	h.now = h.now.Add(24 * time.Hour)
	if res := h.pass(); res.Failed != 0 || res.Skipped != 2 {
		t.Fatalf("parked asset retried: %+v", res)
	}
}

func TestFailedBatchMarksEveryAsset(t *testing.T) {
	h := newHarness(t, maxJob())
	h.fake.add("a1", "p-max")
	h.fake.add("a2", "p-max")
	h.fake.failPUT = true

	if res := h.pass(); res.Failed != 2 {
		t.Fatalf("pass %+v", res)
	}
	h.fake.failPUT = false
	h.now = h.now.Add(time.Hour)
	if res := h.pass(); res.Added != 2 {
		t.Fatalf("retry pass %+v", res)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	h := newHarness(t, maxJob())
	h.s.cfg.DryRun = true
	h.fake.add("a1", "p-max")

	if res := h.pass(); res.Planned != 1 {
		t.Fatalf("pass %+v", res)
	}
	if len(h.fake.sent) != 0 {
		t.Fatalf("dry run sent %v", h.fake.sent)
	}
	if _, ok, _ := h.s.store.Lookup(context.Background(), "max", "a1"); ok {
		t.Fatal("dry run wrote state")
	}
}

func TestNameResolution(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*harness)
		want string
	}{
		"unknown person": {func(h *harness) { h.job.People = []string{"Nobody"} }, "no person named"},
		"shared name": {func(h *harness) {
			h.fake.people = append(h.fake.people, person{ID: "p-max2", Name: "Max"})
		}, "2 people are named"},
		"unknown album": {func(h *harness) { h.job.Album = "Nope" }, "no album named"},
		// The job's own key is what is sent.
		"other key": {func(h *harness) { h.job.APIKey = "someone-else" }, "401"},
	} {
		h := newHarness(t, maxJob())
		tc.mut(h)
		_, err := h.s.Pass(context.Background(), h.job)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}
