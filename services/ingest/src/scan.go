package main

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Scanner walks the whole source tree. The watcher is the fast path; this is the
// net that catches anything it missed — dropped events, files that landed while
// the service was down, and failures whose backoff has expired.
type Scanner struct {
	source string
	filter *Filter
	queue  *Queue
	log    *slog.Logger
}

func NewScanner(source string, filter *Filter, queue *Queue, log *slog.Logger) *Scanner {
	return &Scanner{source: source, filter: filter, queue: queue, log: log}
}

// Scan enqueues every candidate file. It blocks when the queue is full rather
// than dropping.
func (s *Scanner) Scan(ctx context.Context) error {
	var found int
	err := filepath.WalkDir(s.source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk is normal here: syncthing is
			// writing into this tree while we read it.
			s.log.Debug("walk error, skipping", "path", p, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		rel, rerr := relSlash(s.source, p)
		if rerr != nil || rel == "." {
			return nil
		}

		if d.IsDir() {
			if s.filter.IgnoreName(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || s.filter.IgnoreName(d.Name()) {
			return nil
		}

		found++
		return s.queue.Add(ctx, rel)
	})
	if err == nil {
		s.log.Debug("scan complete", "candidates", found)
	}
	return err
}

// Sweep removes working debris left by a run that died: half-written .part
// files in the destination, and anything carrying the prefix in the source that
// is not one of the two records. Nothing is running when this is called, so
// every such file is stale by definition.
//
// It is called on both trees, because the source should only ever hold the two
// small records while the destination is where partial payload can accumulate.
func Sweep(root, prefix string) (int, error) {
	var removed int
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a vanished directory is not worth failing a sweep
		}
		name := d.Name()
		if !strings.HasPrefix(name, prefix) || isRecord(prefix, name) {
			return nil
		}
		if rerr := os.Remove(p); rerr == nil {
			removed++
		}
		return nil
	})
	return removed, err
}

// relSlash returns p relative to root, slash-separated, which is the form the
// queue and the state DB both use.
func relSlash(root, p string) (string, error) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}
