package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sidecar markers.
//
// The source tree only ever receives the two small records, written after the
// work is finished: an empty one for success, and one carrying the error text
// for a failure. The authority on what has been ingested is the state DB —
// these exist so you can see it while standing in the directory.
//
//	find "$SRC" -name '.ingest.*.failed'    # what went wrong, and where
//	find "$SRC" -name '.ingest.*' -delete   # forget everything
//
// markerPart is the one that holds payload, and it lives in the destination:
// both ffmpeg and the plain copy write there, then rename into place.
const (
	markerTranscoded = "transcoded" // this file was re-encoded; empty, presence is the signal
	markerFailed     = "failed"     // what went wrong, one line per attempt
	markerPart       = "part"       // destination temp file, never in the source tree
)

// sidecarPath builds the sidecar for a file, beside the file itself.
func sidecarPath(prefix, file, marker string) string {
	dir, name := filepath.Split(file)
	return filepath.Join(dir, prefix+name+"."+marker)
}

// partPath names the in-progress destination file, which both ffmpeg and the
// plain copy write to before it is renamed into place.
//
// It keeps the original extension LAST, because ffmpeg picks the output
// container from it and a name ending in ".part" fails with "use a standard
// extension for the filename". So the marker goes before the extension:
// .ingest.VID.part.mp4
func partPath(prefix, file string) string {
	dir, name := filepath.Split(file)
	ext := extOf(name)
	return filepath.Join(dir, prefix+name[:len(name)-len(ext)]+"."+markerPart+ext)
}

// isRecord reports whether a base name is one of the two records worth keeping.
// Everything else carrying the prefix is working debris from an interrupted
// run, safe to delete.
func isRecord(prefix, name string) bool {
	return strings.HasPrefix(prefix, ".") &&
		strings.HasPrefix(name, prefix) &&
		(strings.HasSuffix(name, "."+markerTranscoded) || strings.HasSuffix(name, "."+markerFailed))
}

// writeFailure appends one line describing a failed attempt. Appending rather
// than replacing means a file that fails repeatedly for changing reasons shows
// the whole story, which is usually what you want when debugging one.
func writeFailure(prefix, file, stage string, cause error) error {
	p := sidecarPath(prefix, file, markerFailed)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%s\t%v\n", time.Now().Format(time.RFC3339), stage, cause)
	return err
}

// clearFailure removes a stale failure record after a success, so what is on
// disk is the current state rather than a history.
func clearFailure(prefix, file string) error {
	return removeIfExists(sidecarPath(prefix, file, markerFailed))
}

// markTranscoded leaves the empty marker that says ffmpeg actually ran. Files
// copied through untouched deliberately get nothing, so the marker means exactly
// what it says.
func markTranscoded(prefix, file string) error {
	f, err := os.OpenFile(sidecarPath(prefix, file, markerTranscoded), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func removeIfExists(p string) error {
	err := os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
