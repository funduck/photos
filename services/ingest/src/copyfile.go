package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// copyFile copies src to dst through a temp file in the destination directory,
// then renames it into place. Immich and rclone both watch this tree, so they
// must never see a partially written file — and the temp name carries the
// .ingest. prefix so a crash leaves something recognisable rather than junk.
func copyFile(ctx context.Context, src, dst, sidecarPrefix string, mtime time.Time) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}

	tmp := partPath(sidecarPrefix, dst)
	// A leftover temp file from a previous crash would otherwise be appended to.
	if err := removeIfExists(tmp); err != nil {
		return 0, err
	}

	n, err := writeTemp(ctx, src, tmp)
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

func writeTemp(ctx context.Context, src, tmp string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	// Copy through a context-aware reader: a multi-gigabyte copy to a USB drive
	// takes long enough that finishing it after Ctrl-C would feel like a hang.
	// The partial file is cleaned up by the caller either way.
	n, err := io.Copy(out, &ctxReader{ctx: ctx, r: in})
	if err != nil {
		return 0, err
	}
	// Fsync before the rename: the destination is an external drive that can be
	// unplugged, and a rename that outlives its data is the one way this could
	// leave a truncated file looking complete.
	if err := out.Sync(); err != nil {
		return 0, err
	}
	return n, out.Close()
}

// ctxReader makes a copy abandonable: Read fails as soon as the context is
// done, rather than at the end of the file.
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

// destReady reports whether the destination is really mounted and is the
// directory we think it is. On macOS an unmounted /Volumes/... leaves behind an
// empty writable directory, so "the path exists" proves nothing — only the
// hand-created marker file does.
func destReady(dest, marker string) error {
	fi, err := os.Stat(dest)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dest)
	}
	if _, err := os.Stat(filepath.Join(dest, marker)); err != nil {
		return fmt.Errorf("marker file %s is missing", marker)
	}
	return nil
}
