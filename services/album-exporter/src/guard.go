package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// checkNotIngested refuses a destination that is, or lies inside, an ingest
// source. Ingest mirrors its source into the library Immich reads, so
// exporting there would copy album photos back into the library as new
// assets — each one a duplicate, and with the album being re-exported, a loop.
//
// An ingest source is recognised by the .ingest directory at its root. Its
// ignore rules are deliberately not consulted: one edit to that config would
// be enough to start the loop, so any .ingest on the way up is a refusal.
func checkNotIngested(dest string) error {
	for dir := filepath.Clean(dest); ; {
		fi, err := os.Stat(filepath.Join(dir, ".ingest"))
		switch {
		case err == nil && fi.IsDir():
			return fmt.Errorf("%s is inside ingest source %s (it has .ingest/): exporting there would feed the album back into the library", dest, dir)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return err
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}
