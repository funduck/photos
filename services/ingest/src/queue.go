package main

import (
	"context"
	"sync"
)

// Queue is the single place where the watcher and the rescan meet, so neither
// has to care that the other exists. A path already queued or in flight is
// dropped; the state DB is the second, authoritative check.
type Queue struct {
	ch chan string

	mu     sync.Mutex
	queued map[string]struct{}
	active map[string]struct{}
}

func NewQueue(size int) *Queue {
	return &Queue{
		ch:     make(chan string, size),
		queued: make(map[string]struct{}),
		active: make(map[string]struct{}),
	}
}

func (q *Queue) reserve(rel string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.queued[rel]; ok {
		return false
	}
	if _, ok := q.active[rel]; ok {
		return false
	}
	q.queued[rel] = struct{}{}
	return true
}

func (q *Queue) release(rel string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.queued, rel)
}

// Add blocks until there is room. The rescan uses this: a first walk of a large
// tree must not drop anything, and backpressure is the simplest way to say so.
func (q *Queue) Add(ctx context.Context, rel string) error {
	if !q.reserve(rel) {
		return nil
	}
	select {
	case q.ch <- rel:
		return nil
	case <-ctx.Done():
		q.release(rel)
		return ctx.Err()
	}
}

// TryAdd drops the path if the queue is full. The watcher uses this: it is an
// optimisation over the rescan, never a source of truth, so losing an event
// under load costs at most one scan interval of latency.
func (q *Queue) TryAdd(rel string) bool {
	if !q.reserve(rel) {
		return false
	}
	select {
	case q.ch <- rel:
		return true
	default:
		q.release(rel)
		return false
	}
}

// Next hands a path to a worker and marks it in flight until Done is called.
func (q *Queue) Next(ctx context.Context) (string, bool) {
	select {
	case rel, ok := <-q.ch:
		if !ok {
			return "", false
		}
		q.mu.Lock()
		delete(q.queued, rel)
		q.active[rel] = struct{}{}
		q.mu.Unlock()
		return rel, true
	case <-ctx.Done():
		return "", false
	}
}

func (q *Queue) Done(rel string) {
	q.mu.Lock()
	delete(q.active, rel)
	q.mu.Unlock()
}

// Close stops Next from blocking once the channel drains. Only -once mode uses
// it; the daemon runs until it is signalled.
func (q *Queue) Close() { close(q.ch) }

// Idle reports whether nothing is queued or in flight.
func (q *Queue) Idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queued) == 0 && len(q.active) == 0
}
