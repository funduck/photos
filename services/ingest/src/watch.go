package main

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher turns filesystem events into queue entries. It is strictly an
// optimisation over the periodic rescan: every event it drops costs at most one
// scan interval of latency, so it is allowed to be lossy and must never be
// allowed to take the service down.
type Watcher struct {
	source   string
	filter   *Filter
	queue    *Queue
	log      *slog.Logger
	debounce time.Duration

	w *fsnotify.Watcher

	mu     sync.Mutex
	timers map[string]*time.Timer
}

func NewWatcher(source string, filter *Filter, queue *Queue, debounce time.Duration, log *slog.Logger) (*Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		source: source, filter: filter, queue: queue, log: log,
		debounce: debounce, w: w, timers: map[string]*time.Timer{},
	}, nil
}

// Run registers the tree and processes events until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	defer w.w.Close()
	defer w.stopTimers()

	if err := w.addTree(w.source); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case err, ok := <-w.w.Errors:
			if !ok {
				return nil
			}
			// Degrade rather than exit: the rescan still covers everything.
			w.log.Warn("watcher error, relying on the periodic rescan", "err", err)

		case ev, ok := <-w.w.Events:
			if !ok {
				return nil
			}
			w.handle(ev)
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Rename) {
		return
	}
	name := filepath.Base(ev.Name)
	if w.filter.IgnoreName(name) {
		return
	}

	st, err := os.Stat(ev.Name)
	if err != nil {
		// Removed again, or a rename we saw the old name of; nothing to do.
		return
	}

	if st.IsDir() {
		// Files can land inside a new directory before the watch registers, so
		// walk it immediately rather than waiting for events that already fired.
		if err := w.addTree(ev.Name); err != nil {
			w.log.Debug("could not watch new directory", "path", ev.Name, "err", err)
		}
		w.enqueueTree(ev.Name)
		return
	}
	if !st.Mode().IsRegular() {
		return
	}
	w.schedule(ev.Name)
}

// schedule debounces a path: a file being written fires many events, and only
// the quiet period afterwards is worth acting on.
func (w *Watcher) schedule(path string) {
	rel, err := relSlash(w.source, path)
	if err != nil {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[rel]; ok {
		t.Reset(w.debounce)
		return
	}
	w.timers[rel] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.timers, rel)
		w.mu.Unlock()
		// Dropping here is fine — the rescan is the safety net.
		if !w.queue.TryAdd(rel) {
			w.log.Debug("queue full, leaving it to the rescan", "rel", rel)
		}
	})
}

func (w *Watcher) stopTimers() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, t := range w.timers {
		t.Stop()
	}
	w.timers = map[string]*time.Timer{}
}

// addTree registers root and every non-ignored directory beneath it.
func (w *Watcher) addTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // a vanished directory is not a watcher failure
		}
		if p != root && w.filter.IgnoreName(d.Name()) {
			return fs.SkipDir
		}
		if aerr := w.w.Add(p); aerr != nil {
			// kqueue on macOS costs a descriptor per watched directory; running
			// out is survivable, the rescan still covers this subtree.
			w.log.Warn("could not watch directory", "path", p, "err", aerr)
			return fs.SkipDir
		}
		return nil
	})
}

// enqueueTree queues every file under root, for a directory that appeared whole.
func (w *Watcher) enqueueTree(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}
		if d.IsDir() {
			if p != root && w.filter.IgnoreName(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && !w.filter.IgnoreName(d.Name()) {
			w.schedule(p)
		}
		return nil
	})
}
