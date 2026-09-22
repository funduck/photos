package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeImmich serves the endpoints the exporter uses from an in-memory album.
type fakeImmich struct {
	mu        sync.Mutex
	albums    []album
	inAlbum   map[string][]string // album ID -> asset IDs, in order
	assets    map[string]Asset
	content   map[string]string
	failFor   map[string]bool // asset IDs whose download returns 500
	pageSize  int
	downloads []string
}

func newFake() *fakeImmich {
	return &fakeImmich{
		albums:   []album{{ID: "alb-1", AlbumName: "For Max"}},
		inAlbum:  map[string][]string{},
		assets:   map[string]Asset{},
		content:  map[string]string{},
		failFor:  map[string]bool{},
		pageSize: 2,
	}
}

func (f *fakeImmich) add(albumID, id, name, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assets[id] = Asset{ID: id, OriginalFileName: name,
		FileCreatedAt: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)}
	f.content[id] = body
	if albumID != "" {
		f.inAlbum[albumID] = append(f.inAlbum[albumID], id)
	}
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

	case r.Method == http.MethodPost && r.URL.Path == "/api/search/metadata":
		var req struct {
			AlbumIDs []string `json:"albumIds"`
			Page     int      `json:"page"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		ids := f.inAlbum[req.AlbumIDs[0]]
		start := min((req.Page-1)*f.pageSize, len(ids))
		end := min(start+f.pageSize, len(ids))
		var resp struct {
			Assets struct {
				Items    []Asset `json:"items"`
				NextPage *string `json:"nextPage"`
			} `json:"assets"`
		}
		resp.Assets.Items = []Asset{}
		for _, id := range ids[start:end] {
			resp.Assets.Items = append(resp.Assets.Items, f.assets[id])
		}
		if end < len(ids) {
			next := strconv.Itoa(req.Page + 1)
			resp.Assets.NextPage = &next
		}
		json.NewEncoder(w).Encode(resp)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/assets/"):
		rest := strings.TrimPrefix(r.URL.Path, "/api/assets/")
		id, original := strings.CutSuffix(rest, "/original")
		a, ok := f.assets[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if !original {
			json.NewEncoder(w).Encode(a)
			return
		}
		f.downloads = append(f.downloads, id)
		if f.failFor[id] {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		io.WriteString(w, f.content[id])

	default:
		http.NotFound(w, r)
	}
}

func (f *fakeImmich) downloaded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.downloads)
}

type harness struct {
	t    *testing.T
	fake *fakeImmich
	e    *Exporter
	job  Job
	now  time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	fake := newFake()
	srv := httptest.NewTestServer(t, fake)

	cfg := defaultConfig()
	cfg.APIKey = "key"
	cfg.DryRun = false
	cfg.State.Path = filepath.Join(dir, "state", "state.db")
	cfg.Jobs = []Job{{Name: "for-max", Album: "For Max", Dest: filepath.Join(dir, "for-max")}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(cfg.State.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	h := &harness{t: t, fake: fake, job: cfg.Jobs[0], now: time.Now()}
	h.e = &Exporter{
		cfg:    cfg,
		immich: NewImmich("http://immich", "key", srv.Client()),
		store:  store,
		log:    slog.New(slog.DiscardHandler),
		now:    func() time.Time { return h.now },
	}
	return h
}

func (h *harness) pass() PassResult {
	h.t.Helper()
	res, err := h.e.Pass(context.Background(), h.job)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *harness) files() []string {
	h.t.Helper()
	entries, err := os.ReadDir(h.job.Dest)
	if err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (h *harness) read(name string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.job.Dest, name))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func TestExportsNewAssetsAcrossPages(t *testing.T) {
	h := newHarness(t)
	h.fake.add("alb-1", "a1", "IMG_1.jpg", "one")
	h.fake.add("alb-1", "a2", "IMG_2.jpg", "two")
	h.fake.add("alb-1", "a3", "VID_3.mp4", "three")
	h.fake.add("", "other", "NOT_IN_ALBUM.jpg", "no")

	res := h.pass()
	if res.Exported != 3 {
		t.Fatalf("exported %d, want 3", res.Exported)
	}
	if got, want := h.files(), []string{"IMG_1.jpg", "IMG_2.jpg", "VID_3.mp4"}; !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	if got := h.read("VID_3.mp4"); got != "three" {
		t.Fatalf("content %q", got)
	}
	fi, err := os.Stat(filepath.Join(h.job.Dest, "IMG_1.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC); !fi.ModTime().Equal(want) {
		t.Fatalf("mtime %v, want %v", fi.ModTime(), want)
	}
}

func TestSecondPassDownloadsNothing(t *testing.T) {
	h := newHarness(t)
	h.fake.add("alb-1", "a1", "IMG_1.jpg", "one")
	h.pass()

	res := h.pass()
	if res.Exported != 0 || res.Skipped != 1 {
		t.Fatalf("second pass %+v", res)
	}
	if got := h.fake.downloaded(); len(got) != 1 {
		t.Fatalf("downloads %v, want one", got)
	}
}

func TestDeletedFileIsNotExportedAgain(t *testing.T) {
	h := newHarness(t)
	h.fake.add("alb-1", "a1", "IMG_1.jpg", "one")
	h.pass()

	if err := os.Remove(filepath.Join(h.job.Dest, "IMG_1.jpg")); err != nil {
		t.Fatal(err)
	}
	h.fake.add("alb-1", "a2", "IMG_2.jpg", "two")

	res := h.pass()
	if res.Exported != 1 {
		t.Fatalf("exported %d, want only the new asset", res.Exported)
	}
	if got, want := h.files(), []string{"IMG_2.jpg"}; !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
}

func TestNameCollisionGetsSuffix(t *testing.T) {
	h := newHarness(t)
	h.fake.add("alb-1", "aaaaaaaa-1111", "IMG_1.jpg", "first")
	h.fake.add("alb-1", "bbbbbbbb-2222", "IMG_1.jpg", "second")
	if err := os.MkdirAll(h.job.Dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file put there by hand must survive untouched.
	if err := os.WriteFile(filepath.Join(h.job.Dest, "IMG_1.jpg"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.pass()
	if got := h.read("IMG_1.jpg"); got != "mine" {
		t.Fatalf("existing file overwritten: %q", got)
	}
	if got := h.read("IMG_1-aaaaaaaa.jpg"); got != "first" {
		t.Fatalf("first asset: %q", got)
	}
	if got := h.read("IMG_1-bbbbbbbb.jpg"); got != "second" {
		t.Fatalf("second asset: %q", got)
	}
}

func TestFailedDownloadIsRetriedAfterBackoff(t *testing.T) {
	h := newHarness(t)
	h.fake.add("alb-1", "a1", "IMG_1.jpg", "one")
	h.fake.failFor["a1"] = true

	if res := h.pass(); res.Failed != 1 {
		t.Fatalf("first pass %+v", res)
	}
	if got := h.files(); len(got) != 0 {
		t.Fatalf("a failed download left files: %v", got)
	}

	// Still inside the backoff window: not attempted.
	h.fake.failFor["a1"] = false
	if res := h.pass(); res.Skipped != 1 {
		t.Fatalf("pass inside backoff %+v", res)
	}

	h.now = h.now.Add(time.Hour)
	if res := h.pass(); res.Exported != 1 {
		t.Fatalf("pass after backoff %+v", res)
	}
	if got := h.read("IMG_1.jpg"); got != "one" {
		t.Fatalf("content %q", got)
	}
}

func TestFailuresPark(t *testing.T) {
	h := newHarness(t)
	h.e.cfg.State.MaxAttempts = 2
	h.fake.add("alb-1", "a1", "IMG_1.jpg", "one")
	h.fake.failFor["a1"] = true

	h.pass()
	h.now = h.now.Add(24 * time.Hour)
	h.pass()
	h.now = h.now.Add(24 * time.Hour)
	h.pass()

	if got := h.fake.downloaded(); len(got) != 2 {
		t.Fatalf("downloads %v, want max_attempts of them", got)
	}
	rec, _, err := h.e.store.Lookup(context.Background(), h.job.Name, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusFailedPermanent {
		t.Fatalf("status %q", rec.Status)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.e.cfg.DryRun = true
	h.fake.add("alb-1", "a1", "IMG_1.jpg", "one")

	if res := h.pass(); res.Planned != 1 {
		t.Fatalf("dry run %+v", res)
	}
	if got := h.files(); len(got) != 0 {
		t.Fatalf("dry run wrote %v", got)
	}
	if _, ok, _ := h.e.store.Lookup(context.Background(), h.job.Name, "a1"); ok {
		t.Fatal("dry run recorded state")
	}
}

func TestLivePhotoVideo(t *testing.T) {
	h := newHarness(t)
	h.fake.add("alb-1", "a1", "IMG_1.HEIC", "still")
	h.fake.add("", "v1", "IMG_1.MOV", "motion")
	a := h.fake.assets["a1"]
	a.LivePhotoVideoID = "v1"
	h.fake.assets["a1"] = a

	h.pass()
	if got, want := h.files(), []string{"IMG_1.HEIC"}; !slices.Equal(got, want) {
		t.Fatalf("without live_photo_video: %v", got)
	}

	h.job.LivePhotoVideo = true
	h.pass()
	if got, want := h.files(), []string{"IMG_1.HEIC", "IMG_1.MOV"}; !slices.Equal(got, want) {
		t.Fatalf("with live_photo_video: %v, want %v", got, want)
	}
}

func TestUnknownAlbumFails(t *testing.T) {
	h := newHarness(t)
	h.job.Album = "Nope"
	if _, err := h.e.Pass(context.Background(), h.job); err == nil || !strings.Contains(err.Error(), "no album named") {
		t.Fatalf("err %v", err)
	}
}

func TestBadKeyFails(t *testing.T) {
	h := newHarness(t)
	h.e.immich.key = "wrong"
	if _, err := h.e.Pass(context.Background(), h.job); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err %v", err)
	}
}

func TestSweepRemovesOnlyParts(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{".album-export.IMG_1.part.jpg", "IMG_2.jpg", ".stfolder"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := sweepParts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed %v", removed)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("left %d entries", len(entries))
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"IMG_1.jpg":                "IMG_1.jpg",
		"../../etc/passwd":         "passwd",
		`C:\DCIM\IMG_2.jpg`:        "IMG_2.jpg",
		"..":                       "",
		"":                         "",
		".album-export.x.part.jpg": "",
		"dir/":                     "",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}
