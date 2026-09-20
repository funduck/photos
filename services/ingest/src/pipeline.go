package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StatsSnapshot is the per-run tally, mirroring what
// scripts/transcode-videos.sh prints so the two are directly comparable during
// the cutover.
type StatsSnapshot struct {
	Transcoded, Copied, Adopted, Skipped, Pending, Failed int
	PlanTranscode, PlanCopy                               int
	BytesIn, BytesOut                                     int64
}

// Stats is the live counter behind a snapshot. It is kept separate so a
// snapshot can be passed around by value without copying the mutex.
type Stats struct {
	mu   sync.Mutex
	snap StatsSnapshot
}

func (s *Stats) add(f func(*StatsSnapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.snap)
}

func (s *Stats) Snapshot() StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

func (s StatsSnapshot) LogArgs() []any {
	args := []any{
		"transcoded", s.Transcoded, "copied", s.Copied, "adopted", s.Adopted,
		"skipped", s.Skipped, "failed", s.Failed,
		"bytes_in", s.BytesIn, "bytes_out", s.BytesOut, "saved", s.BytesIn - s.BytesOut,
	}
	if s.Pending > 0 {
		args = append(args, "planned", s.Pending,
			"plan_transcode", s.PlanTranscode, "plan_copy", s.PlanCopy)
	}
	return args
}

type Pipeline struct {
	cfg    Config
	store  *Store
	filter *Filter
	tr     *Transcoder
	log    *slog.Logger
	stats  *Stats

	// sema bounds concurrent ffmpeg runs independently of the worker count:
	// VideoToolbox is a single hardware engine, so a second encode splits the
	// same throughput while a second copy does not.
	sema chan struct{}
}

func NewPipeline(cfg Config, store *Store, filter *Filter, tr *Transcoder, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg: cfg, store: store, filter: filter, tr: tr, log: log,
		stats: &Stats{},
		sema:  make(chan struct{}, cfg.TranscodeWorkers),
	}
}

func (p *Pipeline) Stats() *Stats { return p.stats }

// errDestGone means the destination vanished mid-run. It is not the file's
// fault, so it never lands in the state DB or a sidecar.
var errDestGone = errors.New("destination unavailable")

// Process runs one file all the way through. rel is source-relative and
// slash-separated.
func (p *Pipeline) Process(ctx context.Context, rel string) {
	src := filepath.Join(p.cfg.Source, filepath.FromSlash(rel))
	log := p.log.With("rel", rel)

	size, ok, err := p.settled(ctx, src)
	if err != nil || !ok {
		// Still growing, or gone again. Either way a later event or the next
		// rescan will pick it up; nothing is recorded.
		return
	}
	if p.filter.IgnoreSize(size) {
		return
	}

	switch skip, err := p.shouldSkip(ctx, rel, src, size); {
	case err != nil:
		log.Error("state lookup failed", "err", err)
		return
	case skip != "":
		p.stats.add(func(s *StatsSnapshot) { s.Skipped++ })
		log.Debug("skip", "reason", skip)
		return
	}

	if err := p.run(ctx, rel, src, size, log); err != nil {
		if errors.Is(err, errDestGone) || ctx.Err() != nil {
			return
		}
		p.stats.add(func(s *StatsSnapshot) { s.Failed++ })
		log.Error("ingest failed", "err", err)
	}
}

// shouldSkip returns a non-empty reason when the file needs no work.
func (p *Pipeline) shouldSkip(ctx context.Context, rel, src string, size int64) (string, error) {
	rec, found, err := p.store.Lookup(ctx, rel, size)
	if err != nil {
		return "", err
	}
	if found {
		switch rec.Status {
		case StatusDone:
			return "done", nil
		case StatusFailedPermanent:
			return "failed-permanent", nil
		case StatusFailed, StatusPending:
			// A pending row means a previous run died mid-flight; it is retried
			// like a failure, on the same backoff.
			if rec.NextRetry > time.Now().Unix() {
				return "awaiting-retry", nil
			}
		}
		return "", nil
	}

	// Adoption: the destination already holds this file, so a previous run (or
	// the syncthing hop this service replaces) already delivered it. Without
	// this, a first run — or any run after the state DB is lost — would
	// re-ingest the entire library.
	if p.cfg.State.AdoptExistingDest {
		if reason := p.adoptable(rel, src, size); reason != "" {
			dst := filepath.Join(p.cfg.Dest, filepath.FromSlash(rel))
			if p.cfg.DryRun {
				p.log.Info("would adopt", "rel", rel, "size", size, "reason", reason)
				return "dry-run", nil
			}
			if err := p.store.Adopt(ctx, rel, size, fileSize(dst), reason); err != nil {
				return "", err
			}
			p.stats.add(func(s *StatsSnapshot) { s.Adopted++ })
			return "adopted", nil
		}
	}
	return "", nil
}

// adoptable reports why the destination copy can be taken as already done, or
// "" if it cannot.
func (p *Pipeline) adoptable(rel, src string, size int64) string {
	fi, err := os.Stat(filepath.Join(p.cfg.Dest, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	if fi.Size() == size {
		return "dest-exists-same-size"
	}
	// A transcode is deliberately a different size from its source, so size
	// alone can never adopt one. The marker we wrote beside the source is the
	// evidence that this destination file is our own output — without it, a
	// lost state DB would re-encode and overwrite every video in the library.
	if _, err := os.Stat(sidecarPath(p.cfg.Transcode.SidecarPrefix, src, markerTranscoded)); err == nil {
		return "dest-exists-transcoded"
	}
	return ""
}

func (p *Pipeline) run(ctx context.Context, rel, src string, size int64, log *slog.Logger) error {
	if err := destReady(p.cfg.Dest, p.cfg.DestMarker); err != nil {
		return fmt.Errorf("%w: %w", errDestGone, err)
	}

	rule, bitrate, why := p.decide(ctx, src, size, log)
	if p.cfg.DryRun {
		p.logDryRun(log, size, rule, bitrate, why)
		return nil
	}

	// Inventory mode: record the decision and stop short of acting on it. The
	// plan is advisory — the run that drains this row decides again from the
	// config in force then — so it is safe for it to go stale.
	if p.cfg.StopBeforeAction {
		action := ActionCopy
		if rule != nil {
			action = ActionTranscode
		}
		if err := p.store.MarkPlanned(ctx, rel, size, action, why, bitrate); err != nil {
			return err
		}
		p.stats.add(func(s *StatsSnapshot) {
			s.Pending++
			if action == ActionTranscode {
				s.PlanTranscode++
			} else {
				s.PlanCopy++
			}
		})
		log.Info("planned", "action", action, "reason", why, "size", size, "bitrate", bitrate)
		return nil
	}

	attempts, err := p.store.Claim(ctx, rel, size)
	if err != nil {
		return err
	}

	action, reason, destSize, err := p.deliver(ctx, rule, rel, src, size, attempts, why, log)
	if err != nil {
		if errors.Is(err, errDestGone) || ctx.Err() != nil {
			return err
		}
		stage := stageTranscode
		if errors.Is(err, errMetadata) {
			stage = stageMetadata
		} else if action == stageCopy {
			stage = stageCopy
		}
		if werr := writeFailure(p.cfg.Transcode.SidecarPrefix, src, stage, err); werr != nil {
			log.Warn("could not write failure sidecar", "err", werr)
		}
		status, merr := p.store.MarkFailed(ctx, rel, size, attempts, err, p.cfg.State)
		if merr != nil {
			return merr
		}
		return fmt.Errorf("%s (%s, attempt %d/%d): %w", stage, status, attempts, p.cfg.State.MaxAttempts, err)
	}

	if err := p.store.MarkDone(ctx, rel, size, action, reason, destSize); err != nil {
		return err
	}
	if err := clearFailure(p.cfg.Transcode.SidecarPrefix, src); err != nil {
		log.Warn("could not clear failure sidecar", "err", err)
	}
	if action == ActionTranscode {
		if err := markTranscoded(p.cfg.Transcode.SidecarPrefix, src); err != nil {
			// Bookkeeping must never fail a file that is already delivered.
			log.Warn("could not write transcoded marker", "err", err)
		}
	}

	p.stats.add(func(s *StatsSnapshot) {
		if action == ActionTranscode {
			s.Transcoded++
		} else {
			s.Copied++
		}
		s.BytesIn += size
		s.BytesOut += destSize
	})
	log.Info("ingested", "action", action, "reason", reason,
		"in_bytes", size, "out_bytes", destSize, "pct", pct(destSize, size))
	return nil
}

// decide picks the rule for a file, probing the bitrate only once a rule has
// claimed it. A nil rule means a plain copy, and the returned reason says
// exactly why — "no-rule" alone is not enough to debug a library that came
// through untranscoded.
func (p *Pipeline) decide(ctx context.Context, src string, size int64, log *slog.Logger) (*Rule, int64, string) {
	if !p.cfg.Transcode.Enabled {
		return nil, 0, "transcode-disabled"
	}

	name := filepath.Base(src)
	rule := p.tr.MatchRule(name, size)
	if rule == nil {
		log.Debug("no transcode rule matches", "ext", extOf(name), "size", size)
		return nil, 0, "no-rule"
	}

	bitrate := p.tr.ProbeBitrate(ctx, src)
	log.Debug("transcode rule matched", "rule", rule.Name,
		"bitrate", bitrate, "min_video_bitrate", rule.Match.MinVideoBitrate)

	switch {
	case bitrate == 0:
		// ffprobe could not tell us. Copying through is the safe default, but
		// it must be distinguishable from a genuinely low-bitrate file.
		log.Warn("could not determine bitrate, copying through", "rule", rule.Name)
		return nil, 0, "bitrate-unknown"
	case bitrate < rule.Match.MinVideoBitrate:
		return nil, bitrate, "low-bitrate"
	}
	return rule, bitrate, ""
}

// deliver produces the destination file, transcoding first when a rule applies.
// The returned action is what actually happened, which is not always what the
// rule asked for.
// copyReason explains why no transcode was attempted, and is carried onto the
// resulting copy so the log and the state DB both say what actually happened.
func (p *Pipeline) deliver(ctx context.Context, rule *Rule, rel, src string, size int64, attempts int, copyReason string, log *slog.Logger) (action, reason string, destSize int64, err error) {
	dst := filepath.Join(p.cfg.Dest, filepath.FromSlash(rel))
	mtime := mtimeOf(src)

	if rule != nil {
		ok, reason, terr := p.encode(ctx, rule, src, dst, size, attempts, mtime, log)
		if terr != nil {
			return stageTranscode, "", 0, terr
		}
		if ok {
			// The encode was written straight into the destination, so there is
			// nothing left to copy.
			return ActionTranscode, reason, fileSize(dst), nil
		}
		// The encode declined or fell back; carry its reason onto the copy.
		copyReason = reason
	}

	n, cerr := copyFile(ctx, src, dst, p.cfg.Transcode.SidecarPrefix, mtime)
	if cerr != nil {
		return stageCopy, "", 0, cerr
	}
	return ActionCopy, copyReason, n, nil
}

// encode transcodes src directly into the destination.
//
// ffmpeg writes to a hidden temp file in the destination directory, which is
// renamed into place once it is known to be good. Encoding into the destination
// rather than beside the source saves a whole read-and-write pass of the
// encoded video, keeps the source tree free of payload files, and — because the
// temp file is already on the destination filesystem — keeps the final rename
// atomic, so Immich and rclone never see a partial file.
//
// It reports whether the destination now holds the transcode. False with a
// reason means the file should be copied through unchanged instead.
func (p *Pipeline) encode(ctx context.Context, rule *Rule, src, dst string, size int64, attempts int, mtime time.Time, log *slog.Logger) (bool, string, error) {
	select {
	case p.sema <- struct{}{}:
	case <-ctx.Done():
		return false, "", ctx.Err()
	}
	defer func() { <-p.sema }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, "", err
	}
	// The temp name keeps the real extension last: ffmpeg picks the output
	// container from it and rejects anything it does not recognise.
	out := partPath(p.cfg.Transcode.SidecarPrefix, dst)
	if err := removeIfExists(out); err != nil {
		return false, "", err
	}
	// Nothing below may leave a partial file behind on the destination.
	defer func() { removeIfExists(out) }()

	err := p.tr.Transcode(ctx, rule, src, out)
	switch {
	case err == nil:

	case errors.Is(err, errMetadata) && p.cfg.Transcode.OnMetadataError == onErrorWarn:
		// The encode is good; only the tag copy failed. Keeping it matches
		// scripts/transcode-videos.sh, which warns and moves on.
		log.Warn("metadata copy failed, keeping transcode", "err", err)

	default:
		if p.cfg.Transcode.OnTranscodeError == onErrorFallback && attempts >= p.cfg.State.MaxAttempts {
			// Out of retries: deliver the original rather than nothing.
			log.Warn("transcode failed, copying original instead", "attempts", attempts, "err", err)
			if werr := writeFailure(p.cfg.Transcode.SidecarPrefix, src, stageTranscode, err); werr != nil {
				log.Warn("could not write failure sidecar", "err", werr)
			}
			return false, "transcode-failed", nil
		}
		return false, "", err
	}

	if outSize := fileSize(out); outSize >= size && !p.cfg.Transcode.KeepIfLarger {
		// -q:v is not a rate factor; on an already-efficient source the encode
		// can come out bigger. Keeping it would be strictly worse.
		log.Info("transcode not smaller, copying original", "in_bytes", size, "out_bytes", outSize)
		return false, "transcode-bigger", nil
	}

	if rule.PreserveMtime && !mtime.IsZero() {
		// Immich falls back to mtime when metadata carries no date.
		if err := os.Chtimes(out, mtime, mtime); err != nil {
			log.Warn("could not preserve mtime", "err", err)
		}
	}

	if err := os.Rename(out, dst); err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (p *Pipeline) logDryRun(log *slog.Logger, size int64, rule *Rule, bitrate int64, why string) {
	if rule != nil {
		log.Info("would transcode", "size", size, "bitrate", bitrate, "rule", rule.Name)
		return
	}
	log.Info("would copy", "size", size, "reason", why, "bitrate", bitrate)
}

// settled waits for a file's size to stop changing. Syncthing renames completed
// downloads into place, so this is usually satisfied on the first check — it is
// the safety net for everything that writes in place instead.
func (p *Pipeline) settled(ctx context.Context, src string) (int64, bool, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return 0, false, err
	}
	if !fi.Mode().IsRegular() {
		return 0, false, nil
	}

	// A file untouched for longer than the settle window is not being written
	// to, so there is nothing to wait for. Without this every file on every
	// scan costs a full settle interval, which over a real library is hours.
	if time.Since(fi.ModTime()) > p.cfg.Scan.Settle.D() {
		return fi.Size(), true, nil
	}

	select {
	case <-time.After(p.cfg.Scan.Settle.D()):
	case <-ctx.Done():
		return 0, false, ctx.Err()
	}

	again, err := os.Stat(src)
	if err != nil {
		return 0, false, err
	}
	if again.Size() != fi.Size() {
		p.log.Debug("still being written, deferring", "rel", src, "was", fi.Size(), "now", again.Size())
		return 0, false, nil
	}
	return again.Size(), true, nil
}

func mtimeOf(p string) time.Time {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func pct(out, in int64) int {
	if in == 0 {
		return 0
	}
	return int(out * 100 / in)
}
