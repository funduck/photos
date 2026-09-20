package main

import "testing"

func testFilter(t *testing.T) *Filter {
	t.Helper()
	f, err := NewFilter(defaultConfig().Ignore)
	if err != nil {
		t.Fatalf("NewFilter: %v", err)
	}
	return f
}

func TestFilterIgnoreRel(t *testing.T) {
	f := testFilter(t)

	tests := []struct {
		rel  string
		want bool
	}{
		// The junk this pipeline actually produces.
		{".DS_Store", true},
		{".stfolder", true},
		{".stversions/nested/x.jpg", true},
		{"@eaDir", true},
		{"Oleg/oleg-pixel/@eaDir/thumb.jpg", true},
		{"~syncthing~VID.mp4.tmp", true},
		{"Oleg/oleg-pixel/~syncthing~VID.mp4.tmp", true},

		// Our own sidecars must never feed back into the scanner.
		{".ingest.VID.mp4.transcoding", true},
		{".ingest.VID.mp4.transcoded", true},
		{".ingest.VID.mp4.failed", true},
		{"Oleg/oleg-pixel/.ingest.VID.mp4.part", true},

		// Real media.
		{"VID.mp4", false},
		{"Oleg/oleg-pixel/VID_0001.mp4", false},
		{"Kate/kate-samsung/Camera/IMG_0002.jpg", false},
		{"Oleg/oleg-pixel/VID.MOV", false},

		// A dot inside the name is not a dotfile.
		{"Oleg/my.holiday.video.mp4", false},
	}

	for _, tc := range tests {
		if got := f.IgnoreRel(tc.rel); got != tc.want {
			t.Errorf("IgnoreRel(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

func TestFilterIgnoreName(t *testing.T) {
	f := testFilter(t)

	// Directory pruning relies on this: an ignored directory hides its whole
	// subtree, rather than only being skipped itself.
	for _, name := range []string{".stversions", "@eaDir", ".ingest.x.transcoding"} {
		if !f.IgnoreName(name) {
			t.Errorf("IgnoreName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"Oleg", "oleg-pixel", "Camera"} {
		if f.IgnoreName(name) {
			t.Errorf("IgnoreName(%q) = true, want false", name)
		}
	}
}

func TestFilterPaths(t *testing.T) {
	f, err := NewFilter(IgnoreConfig{Paths: []string{"Kate/*/Screenshots/*"}})
	if err != nil {
		t.Fatalf("NewFilter: %v", err)
	}
	if !f.IgnoreRel("Kate/kate-samsung/Screenshots/a.png") {
		t.Error("path rule did not match")
	}
	if f.IgnoreRel("Kate/kate-samsung/Camera/a.jpg") {
		t.Error("path rule matched too much")
	}
}

func TestFilterIgnoreSize(t *testing.T) {
	f := testFilter(t)
	if !f.IgnoreSize(0) {
		t.Error("zero-byte file should be ignored")
	}
	if f.IgnoreSize(1) {
		t.Error("1-byte file should not be ignored")
	}
}

func TestNewFilterRejectsBadPattern(t *testing.T) {
	if _, err := NewFilter(IgnoreConfig{Names: []string{"["}}); err == nil {
		t.Error("expected an error for a malformed glob")
	}
}
