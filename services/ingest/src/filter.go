package main

import (
	"fmt"
	"path"
	"path/filepath"
)

// Filter decides what in the source tree ingest is allowed to look at.
//
// Name globs are matched against the base name of every path component, so an
// ignored directory hides everything beneath it — the walk never descends into
// it and the watcher never registers it.
type Filter struct {
	names   []string
	paths   []string
	minSize int64
}

func NewFilter(c IgnoreConfig) (*Filter, error) {
	// filepath.Match only reports a syntax error when it actually walks the
	// pattern, so validate each one up front against a throwaway name.
	for _, p := range c.Names {
		if _, err := filepath.Match(p, "x"); err != nil {
			return nil, fmt.Errorf("ignore.names: bad pattern %q: %w", p, err)
		}
	}
	for _, p := range c.Paths {
		if _, err := path.Match(p, "x"); err != nil {
			return nil, fmt.Errorf("ignore.paths: bad pattern %q: %w", p, err)
		}
	}
	return &Filter{names: c.Names, paths: c.Paths, minSize: c.MinSize}, nil
}

// IgnoreName reports whether a single path component (file or directory base
// name) is ignored.
func (f *Filter) IgnoreName(base string) bool {
	for _, p := range f.names {
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
	}
	return false
}

// IgnoreRel reports whether a source-relative path is ignored, either because
// one of its components matches a name rule or because the whole path matches a
// path rule. rel is slash-separated.
func (f *Filter) IgnoreRel(rel string) bool {
	for _, p := range f.paths {
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
	}
	for rest := rel; rest != "" && rest != "."; {
		dir, base := path.Split(rest)
		if f.IgnoreName(base) {
			return true
		}
		rest = path.Clean(dir)
		if rest == "/" {
			break
		}
	}
	return false
}

// IgnoreSize reports whether a file is too small to bother with. Zero-byte files
// are usually a sync artifact rather than a photo.
func (f *Filter) IgnoreSize(size int64) bool { return size < f.minSize }
