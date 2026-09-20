package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if _, found, err := s.Lookup(ctx, "a/b.mp4", 100); err != nil || found {
		t.Fatalf("Lookup on an empty DB: found=%v err=%v", found, err)
	}

	attempts, err := s.Claim(ctx, "a/b.mp4", 100)
	if err != nil || attempts != 1 {
		t.Fatalf("Claim: attempts=%d err=%v", attempts, err)
	}
	if err := s.MarkDone(ctx, "a/b.mp4", 100, ActionTranscode, "", 40); err != nil {
		t.Fatal(err)
	}

	rec, found, err := s.Lookup(ctx, "a/b.mp4", 100)
	if err != nil || !found {
		t.Fatalf("Lookup after MarkDone: found=%v err=%v", found, err)
	}
	if rec.Status != StatusDone || rec.Action != ActionTranscode || rec.DestSize != 40 {
		t.Errorf("unexpected record: %+v", rec)
	}
}

// The state key is path+size, with no mtime: a syncthing touch must not cause a
// re-copy, but a genuinely different file at the same path must be reprocessed.
func TestStoreKeyIsPathAndSize(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if _, err := s.Claim(ctx, "a/b.mp4", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(ctx, "a/b.mp4", 100, ActionCopy, "", 100); err != nil {
		t.Fatal(err)
	}

	if _, found, _ := s.Lookup(ctx, "a/b.mp4", 200); found {
		t.Error("a different size must not hit the existing row")
	}
	if _, found, _ := s.Lookup(ctx, "a/b.mp4", 100); !found {
		t.Error("the original row should still be there")
	}
}

func TestStoreFailureAndRetry(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c := defaultConfig().State
	c.MaxAttempts = 2

	attempts, err := s.Claim(ctx, "a/b.mp4", 100)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.MarkFailed(ctx, "a/b.mp4", 100, attempts, errors.New("ffmpeg blew up"), c)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusFailed {
		t.Errorf("first failure = %q, want %q", status, StatusFailed)
	}

	rec, _, err := s.Lookup(ctx, "a/b.mp4", 100)
	if err != nil {
		t.Fatal(err)
	}
	if rec.NextRetry <= time.Now().Unix() {
		t.Error("a retryable failure should be scheduled in the future")
	}
	if rec.LastError == "" {
		t.Error("the error text should be recorded")
	}

	// Second attempt exhausts max_attempts and parks the file.
	attempts, err = s.Claim(ctx, "a/b.mp4", 100)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	status, err = s.MarkFailed(ctx, "a/b.mp4", 100, attempts, errors.New("again"), c)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusFailedPermanent {
		t.Errorf("final failure = %q, want %q", status, StatusFailedPermanent)
	}

	failures, err := s.Failures(ctx)
	if err != nil || len(failures) != 1 {
		t.Fatalf("Failures: %d rows, err=%v", len(failures), err)
	}
}

func TestStoreAdopt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if err := s.Adopt(ctx, "a/b.jpg", 100, 100, "dest-exists-same-size"); err != nil {
		t.Fatal(err)
	}
	rec, found, err := s.Lookup(ctx, "a/b.jpg", 100)
	if err != nil || !found {
		t.Fatalf("Lookup after Adopt: found=%v err=%v", found, err)
	}
	if rec.Status != StatusDone || rec.Action != ActionAdopted {
		t.Errorf("unexpected record: %+v", rec)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for i := 0; i < 2; i++ {
		s, err := OpenStore(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		s.Close()
	}
}

func TestBackoffIsCapped(t *testing.T) {
	c := defaultConfig().State
	if got := backoff(c, 1); got != c.RetryBackoff.D() {
		t.Errorf("first backoff = %v, want %v", got, c.RetryBackoff.D())
	}
	if got := backoff(c, 100); got != c.RetryMax.D() {
		t.Errorf("backoff should cap at %v, got %v", c.RetryMax.D(), got)
	}
}

// Each source tree gets its own DB, so pointing a second instance somewhere
// else cannot read or clobber this one's state.
func TestStateDBPathIsPerSource(t *testing.T) {
	c := StateConfig{Dir: "/state"}

	a, err := StateDBPath(c, "/Users/oleg/SyncPhones")
	if err != nil {
		t.Fatal(err)
	}
	b, err := StateDBPath(c, "/Users/oleg/OtherPhotos")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two sources share a DB path: %s", a)
	}

	again, _ := StateDBPath(c, "/Users/oleg/SyncPhones")
	if again != a {
		t.Errorf("not stable across calls: %s vs %s", a, again)
	}

	c.Path = "/explicit/state.db"
	if got, _ := StateDBPath(c, "/Users/oleg/SyncPhones"); got != c.Path {
		t.Errorf("explicit path should win, got %s", got)
	}
}

func TestSourceSlugIsReadable(t *testing.T) {
	if got := sourceSlug("/Users/oleg/SyncPhones"); got != "Users-oleg-SyncPhones" {
		t.Errorf("sourceSlug = %q, want a readable name", got)
	}
}

// MarkPlanned is what an inventory run records. Unlike Claim it must not touch
// the attempt counter — repeating an inventory pass should never eat into
// max_attempts — and it must not drag a finished file back to pending.
func TestMarkPlanned(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if err := s.MarkPlanned(ctx, "a/b.mp4", 100, ActionTranscode, "", 19_000_000); err != nil {
		t.Fatal(err)
	}
	rec, found, err := s.Lookup(ctx, "a/b.mp4", 100)
	if err != nil || !found {
		t.Fatalf("Lookup: found=%v err=%v", found, err)
	}
	if rec.Status != StatusPending {
		t.Errorf("status = %q, want %q", rec.Status, StatusPending)
	}
	if rec.Action != ActionTranscode {
		t.Errorf("action = %q, want %q", rec.Action, ActionTranscode)
	}
	if rec.Bitrate != 19_000_000 {
		t.Errorf("bitrate = %d, want 19000000", rec.Bitrate)
	}
	if rec.Attempts != 0 {
		t.Errorf("attempts = %d, want 0 — no work was attempted", rec.Attempts)
	}
	// A pending row is not awaiting retry, so a later run picks it up at once.
	if rec.NextRetry != 0 {
		t.Errorf("next_retry = %d, want 0", rec.NextRetry)
	}

	// Repeating the inventory stays idempotent, and re-planning updates the plan.
	for i := 0; i < 3; i++ {
		if err := s.MarkPlanned(ctx, "a/b.mp4", 100, ActionCopy, "low-bitrate", 4_000_000); err != nil {
			t.Fatal(err)
		}
	}
	rec, _, _ = s.Lookup(ctx, "a/b.mp4", 100)
	if rec.Attempts != 0 {
		t.Errorf("attempts = %d after repeated inventory runs, want 0", rec.Attempts)
	}
	if rec.Action != ActionCopy || rec.Reason != "low-bitrate" {
		t.Errorf("plan was not updated: action=%q reason=%q", rec.Action, rec.Reason)
	}

	// A finished file must not be dragged back to pending.
	if _, err := s.Claim(ctx, "c/d.mp4", 200); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(ctx, "c/d.mp4", 200, ActionTranscode, "", 50); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPlanned(ctx, "c/d.mp4", 200, ActionCopy, "", 0); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = s.Lookup(ctx, "c/d.mp4", 200)
	if rec.Status != StatusDone || rec.Action != ActionTranscode {
		t.Errorf("a done file must stay done: status=%q action=%q", rec.Status, rec.Action)
	}
}

// Pending is what -report aggregates, so a plan can be read back later.
func TestPendingSummary(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if err := s.MarkPlanned(ctx, "a.mp4", 1000, ActionTranscode, "", 19_000_000); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPlanned(ctx, "b.mp4", 2000, ActionTranscode, "", 20_000_000); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPlanned(ctx, "c.jpg", 500, ActionCopy, "no-rule", 0); err != nil {
		t.Fatal(err)
	}

	work, err := s.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]PlannedWork{}
	for _, w := range work {
		got[w.Action] = w
	}
	if w := got[ActionTranscode]; w.Files != 2 || w.Bytes != 3000 {
		t.Errorf("transcode = %d files / %d bytes, want 2 / 3000", w.Files, w.Bytes)
	}
	if w := got[ActionCopy]; w.Files != 1 || w.Bytes != 500 {
		t.Errorf("copy = %d files / %d bytes, want 1 / 500", w.Files, w.Bytes)
	}

	// Finishing one takes it out of the pending summary.
	if _, err := s.Claim(ctx, "a.mp4", 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(ctx, "a.mp4", 1000, ActionTranscode, "", 400); err != nil {
		t.Fatal(err)
	}
	work, _ = s.Pending(ctx)
	for _, w := range work {
		if w.Action == ActionTranscode && w.Files != 1 {
			t.Errorf("transcode = %d files after one finished, want 1", w.Files)
		}
	}
}
