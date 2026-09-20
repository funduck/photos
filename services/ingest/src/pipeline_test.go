package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// The destination marker is what proves the external drive is really mounted:
// on macOS an unmounted /Volumes/... leaves an empty writable directory behind.
func TestDestReady(t *testing.T) {
	dir := t.TempDir()

	if err := destReady(dir, ".ingest-dest"); err == nil {
		t.Error("a directory without the marker must not be considered ready")
	}
	if err := os.WriteFile(filepath.Join(dir, ".ingest-dest"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := destReady(dir, ".ingest-dest"); err != nil {
		t.Errorf("with the marker present: %v", err)
	}
	if err := destReady(filepath.Join(dir, "gone"), ".ingest-dest"); err == nil {
		t.Error("a missing destination must not be considered ready")
	}
}

func TestCopyFilePreservesMtimeAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mp4")
	dst := filepath.Join(dir, "sub", "out.mp4")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)

	n, err := copyFile(context.Background(), src, dst, ".ingest.", mtime)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("copied %d bytes, want 5", n)
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Immich falls back to mtime when the metadata carries no date.
	if !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime = %v, want %v", fi.ModTime(), mtime)
	}

	// Nothing partial may be visible to Immich or rclone.
	if _, err := os.Stat(partPath(".ingest.", dst)); !os.IsNotExist(err) {
		t.Error("the temp file should be gone after a successful copy")
	}
}

func TestCopyFileLeavesNoTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.mp4")

	if _, err := copyFile(context.Background(), filepath.Join(dir, "missing.mp4"), dst, ".ingest.", time.Time{}); err == nil {
		t.Fatal("expected an error for a missing source")
	}
	if _, err := os.Stat(partPath(".ingest.", dst)); !os.IsNotExist(err) {
		t.Error("a failed copy must not leave a temp file behind")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("a failed copy must not create the destination")
	}
}

func TestSidecarNaming(t *testing.T) {
	got := sidecarPath(".ingest.", "/src/Oleg/VID.mp4", markerTranscoded)
	want := "/src/Oleg/.ingest.VID.mp4.transcoded"
	if got != want {
		t.Errorf("sidecarPath = %q, want %q", got, want)
	}
}

// ffmpeg picks the output container from the extension, so the temp file it
// writes must keep the real extension LAST — a name ending in ".part" fails
// with "use a standard extension for the filename".
func TestPartPathKeepsExtensionLast(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/dst/Oleg/VID.mp4", "/dst/Oleg/.ingest.VID.part.mp4"},
		{"/dst/VID.MOV", "/dst/.ingest.VID.part.MOV"},
		{"/dst/my.holiday.video.mp4", "/dst/.ingest.my.holiday.video.part.mp4"},
		{"/dst/noext", "/dst/.ingest.noext.part"},
	}
	for _, tc := range tests {
		if got := partPath(".ingest.", tc.in); got != tc.want {
			t.Errorf("partPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// A part file is debris, never a record.
	if isRecord(".ingest.", ".ingest.VID.part.mp4") {
		t.Error("a .part file must not count as a record")
	}
	for _, name := range []string{".ingest.VID.mp4.transcoded", ".ingest.VID.mp4.failed"} {
		if !isRecord(".ingest.", name) {
			t.Errorf("isRecord(%q) = false, want true", name)
		}
	}
}

// A success must clear a stale failure record, so what is on disk is the
// current state rather than a history.
func TestSidecarFailureLifecycle(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "VID.mp4")
	failed := sidecarPath(".ingest.", src, markerFailed)

	if err := writeFailure(".ingest.", src, stageTranscode, errors.New("encoder blew up")); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(failed)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{stageTranscode, "encoder blew up"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("failure record is missing %q: %s", want, body)
		}
	}

	// A second attempt appends rather than replacing.
	if err := writeFailure(".ingest.", src, stageCopy, errors.New("drive went away")); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(failed)
	if !strings.Contains(string(body), "encoder blew up") || !strings.Contains(string(body), "drive went away") {
		t.Errorf("both attempts should be recorded: %s", body)
	}

	if err := clearFailure(".ingest.", src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Error("a success should clear the failure record")
	}
	// Clearing a record that is not there is not an error.
	if err := clearFailure(".ingest.", src); err != nil {
		t.Errorf("clearing a missing record: %v", err)
	}
}

func TestMarkTranscoded(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "VID.mp4")

	if err := markTranscoded(".ingest.", src); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sidecarPath(".ingest.", src, markerTranscoded))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Error("the marker should be empty — its presence is the signal")
	}
}

// A file still being written must not be ingested half-finished — but a file
// that has not been touched in a long time must not cost a settle wait either,
// or every scan of a real library takes hours.
func TestSettled(t *testing.T) {
	newPipeline := func(settle time.Duration) *Pipeline {
		cfg := defaultConfig()
		cfg.Scan.Settle = Duration(settle)
		return &Pipeline{cfg: cfg, log: quietLogger()}
	}

	t.Run("old file skips the wait", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "VID.mp4")
		if err := os.WriteFile(src, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(src, old, old); err != nil {
			t.Fatal(err)
		}

		p := newPipeline(30 * time.Second)
		start := time.Now()
		size, ok, err := p.settled(context.Background(), src)
		if err != nil || !ok {
			t.Fatalf("a long-quiescent file should settle: ok=%v err=%v", ok, err)
		}
		if size != 7 {
			t.Errorf("size = %d, want 7", size)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("waited %v for a file untouched for an hour", elapsed)
		}
	})

	t.Run("growing file is not settled", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "VID.mp4")
		if err := os.WriteFile(src, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}

		// Grow it during the settle window.
		go func() {
			time.Sleep(20 * time.Millisecond)
			f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return
			}
			defer f.Close()
			f.WriteString("more data")
		}()

		p := newPipeline(200 * time.Millisecond)
		if _, ok, _ := p.settled(context.Background(), src); ok {
			t.Error("a growing file must not be reported as settled")
		}
	})

	t.Run("recently written but stable file settles", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "VID.mp4")
		if err := os.WriteFile(src, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}

		p := newPipeline(20 * time.Millisecond)
		size, ok, err := p.settled(context.Background(), src)
		if err != nil || !ok {
			t.Fatalf("a stable file should settle: ok=%v err=%v", ok, err)
		}
		if size != 7 {
			t.Errorf("size = %d, want 7", size)
		}
	})
}

// The source tree must be left holding only the two small records; anything
// else carrying the prefix is debris from a run that died.
func TestSweepKeepsOnlyRecords(t *testing.T) {
	dir := t.TempDir()
	debris := []string{
		partPath(".ingest.", filepath.Join(dir, "VID.mp4")), // interrupted encode/copy
		filepath.Join(dir, ".ingest.VID.mp4.transcoding"),   // legacy name
		filepath.Join(dir, ".ingest.VID.transcoding.mp4"),   // legacy name
	}
	keep := []string{
		filepath.Join(dir, "VID.mp4"),
		filepath.Join(dir, ".ingest.VID.mp4.transcoded"),
		filepath.Join(dir, ".ingest.VID.mp4.failed"),
		filepath.Join(dir, ".ingest-dest"), // the destination marker, not a sidecar
	}
	for _, p := range append(append([]string{}, keep...), debris...) {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	n, err := Sweep(dir, ".ingest.")
	if err != nil {
		t.Fatal(err)
	}
	if n != len(debris) {
		t.Errorf("swept %d files, want %d", n, len(debris))
	}
	for _, p := range debris {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", filepath.Base(p))
		}
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should have been left alone", filepath.Base(p))
		}
	}
}

func TestQueueDeduplicates(t *testing.T) {
	q := NewQueue(4)
	ctx := context.Background()

	if err := q.Add(ctx, "a.mp4"); err != nil {
		t.Fatal(err)
	}
	// The watcher and a concurrent rescan both finding the same file must not
	// process it twice.
	if q.TryAdd("a.mp4") {
		t.Error("an already-queued path should be dropped")
	}

	rel, ok := q.Next(ctx)
	if !ok || rel != "a.mp4" {
		t.Fatalf("Next = %q, %v", rel, ok)
	}
	if q.TryAdd("a.mp4") {
		t.Error("an in-flight path should be dropped")
	}
	if q.Idle() {
		t.Error("a path in flight means the queue is not idle")
	}

	q.Done("a.mp4")
	if !q.Idle() {
		t.Error("the queue should be idle once the path is done")
	}
	if !q.TryAdd("a.mp4") {
		t.Error("a finished path should be acceptable again")
	}
}

// The watcher is an optimisation over the rescan, so it drops rather than
// blocking when the queue is full.
func TestQueueTryAddDropsWhenFull(t *testing.T) {
	q := NewQueue(1)
	if !q.TryAdd("a.mp4") {
		t.Fatal("the first add should fit")
	}
	if q.TryAdd("b.mp4") {
		t.Error("a full queue should drop rather than block")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Adoption is what stops a first run — or any run after the state DB is lost —
// from re-ingesting the whole library. A transcode is deliberately a different
// size from its source, so size alone can never adopt one; the marker is the
// evidence that the destination file is our own output.
func TestAdoptable(t *testing.T) {
	const prefix = ".ingest."

	tests := []struct {
		name     string
		destSize int
		marker   bool
		want     string
	}{
		{"same size", 100, false, "dest-exists-same-size"},
		{"transcoded, marker present", 30, true, "dest-exists-transcoded"},
		{"different size, no marker", 30, false, ""},
		{"no destination file", -1, true, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srcDir := filepath.Join(dir, "src")
			dstDir := filepath.Join(dir, "dst")
			for _, d := range []string{srcDir, dstDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			src := filepath.Join(srcDir, "VID.mp4")
			if err := os.WriteFile(src, make([]byte, 100), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.destSize >= 0 {
				dst := filepath.Join(dstDir, "VID.mp4")
				if err := os.WriteFile(dst, make([]byte, tc.destSize), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.marker {
				if err := markTranscoded(prefix, src); err != nil {
					t.Fatal(err)
				}
			}

			cfg := defaultConfig()
			cfg.Source, cfg.Dest = srcDir, dstDir
			p := &Pipeline{cfg: cfg, log: quietLogger()}

			if got := p.adoptable("VID.mp4", src, 100); got != tc.want {
				t.Errorf("adoptable = %q, want %q", got, tc.want)
			}
		})
	}
}

// Ctrl-C must abandon a copy in progress rather than finish it: on a USB drive
// a multi-gigabyte file takes long enough that running to completion would be
// indistinguishable from a hang.
func TestCopyFileStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.mp4")
	dst := filepath.Join(dir, "out", "big.mp4")
	if err := os.WriteFile(src, make([]byte, 8<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the copy must not even start

	if _, err := copyFile(ctx, src, dst, ".ingest.", time.Time{}); err == nil {
		t.Fatal("expected the copy to be abandoned")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("an abandoned copy must not create the destination")
	}
	if _, err := os.Stat(partPath(".ingest.", dst)); !os.IsNotExist(err) {
		t.Error("an abandoned copy must not leave a temp file behind")
	}
}
