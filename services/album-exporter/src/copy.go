package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// partPrefix marks this service's temp files. The destination is synced to a
// phone, so a half-written file must never appear under its real name, and a
// crash must leave something recognisable to sweep rather than junk.
const partPrefix = ".album-export."

// partPath is the temp name for dst: extension last, so anything that sniffs
// by extension still sees the right type.
func partPath(dst string) string {
	dir, base := filepath.Split(dst)
	ext := filepath.Ext(base)
	return filepath.Join(dir, partPrefix+strings.TrimSuffix(base, ext)+".part"+ext)
}

// writeFile streams r to dst through a temp file in the same directory, then
// renames it into place.
func writeFile(ctx context.Context, r io.Reader, dst string, mtime time.Time) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}

	tmp := partPath(dst)
	// A leftover temp file from a previous crash would otherwise be appended to.
	if err := removeIfExists(tmp); err != nil {
		return 0, err
	}

	n, err := writeTemp(ctx, r, tmp)
	if err != nil {
		removeIfExists(tmp)
		return 0, err
	}

	if !mtime.IsZero() {
		if err := os.Chtimes(tmp, mtime, mtime); err != nil {
			removeIfExists(tmp)
			return 0, fmt.Errorf("set mtime: %w", err)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		removeIfExists(tmp)
		return 0, err
	}
	return n, nil
}

func writeTemp(ctx context.Context, r io.Reader, tmp string) (int64, error) {
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	n, err := io.Copy(out, &ctxReader{ctx: ctx, r: r})
	if err != nil {
		return 0, err
	}
	// Fsync before the rename: a rename that outlives its data is the one way
	// this could leave a truncated file looking complete.
	if err := out.Sync(); err != nil {
		return 0, err
	}
	return n, out.Close()
}

// ctxReader makes a download abandonable: Read fails as soon as the context
// is done, rather than at the end of the file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// sweepParts removes temp files a crash left in dir. Only the top level: the
// exporter writes nothing deeper.
func sweepParts(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), partPrefix) {
			p := filepath.Join(dir, e.Name())
			if err := os.Remove(p); err != nil {
				return removed, err
			}
			removed = append(removed, p)
		}
	}
	return removed, nil
}

// fileName picks a name in dir for an asset. It never returns the name of an
// existing file: whatever is there belongs to someone else, since an asset
// already exported is never downloaded again.
func fileName(dir, original, assetID string) (string, error) {
	base := safeName(original)
	if base == "" {
		base = assetID
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	short := assetID
	if len(short) > 8 {
		short = short[:8]
	}
	for _, name := range []string{base, stem + "-" + short + ext, stem + "-" + assetID + ext} {
		_, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("%s: every candidate name is taken", base)
}

// safeName reduces a name that came from a phone to one plain file name, or ""
// when nothing usable is left.
func safeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if _, last, ok := strings.CutLast(name, "/"); ok {
		name = last
	}
	name = strings.TrimSpace(name)
	if name == "." || name == ".." || strings.HasPrefix(name, partPrefix) {
		return ""
	}
	return name
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
