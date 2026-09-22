package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckNotIngested(t *testing.T) {
	for name, tc := range map[string]struct {
		ingest string // where .ingest is created, relative to root; "" for nowhere
		file   bool   // create .ingest as a file rather than a directory
		refuse bool
	}{
		"no ingest anywhere":              {},
		"dest is an ingest source":        {ingest: "SyncPhones/for-max", refuse: true},
		"parent is an ingest source":      {ingest: "SyncPhones", refuse: true},
		"grandparent is an ingest source": {ingest: ".", refuse: true},
		"sibling is an ingest source":     {ingest: "SyncPhones/other"},
		".ingest is a file, not a source": {ingest: "SyncPhones", file: true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "SyncPhones", "for-max")
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.ingest != "" {
				p := filepath.Join(root, tc.ingest, ".ingest")
				var err error
				if tc.file {
					err = os.WriteFile(p, nil, 0o644)
				} else {
					err = os.MkdirAll(p, 0o755)
				}
				if err != nil {
					t.Fatal(err)
				}
			}

			err := checkNotIngested(dest)
			if tc.refuse != (err != nil) {
				t.Fatalf("refuse=%v, err %v", tc.refuse, err)
			}
		})
	}
}

func TestPassRefusesIngestedDest(t *testing.T) {
	h := newHarness(t)
	h.fake.add("alb-1", "a1", "IMG_1.jpg", "one")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(h.job.Dest), ".ingest"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := h.e.Pass(t.Context(), h.job); err == nil {
		t.Fatal("pass ran into an ingest source")
	}
	if got := h.fake.downloaded(); len(got) != 0 {
		t.Fatalf("downloaded %v", got)
	}
}
