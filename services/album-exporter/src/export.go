package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"
)

// Exporter runs passes over the configured albums.
type Exporter struct {
	cfg    Config
	immich *Immich
	store  *Store
	log    *slog.Logger
	now    func() time.Time
}

// PassResult counts what one pass over one job did.
type PassResult struct {
	Exported, Planned, Failed, Skipped int
}

// Sweep removes temp files left in every destination by a crash. It runs once
// at startup, before anything could be mid-write.
func (e *Exporter) Sweep() {
	for _, j := range e.cfg.Jobs {
		removed, err := sweepParts(j.Dest)
		if err != nil {
			e.log.Warn("sweep failed", "job", j.Name, "dest", j.Dest, "err", err)
		}
		for _, p := range removed {
			e.log.Info("removed leftover temp file", "job", j.Name, "path", p)
		}
	}
}

// Pass exports whatever in the job's album has not been exported yet. Assets
// that were exported and later deleted from the destination, or removed from
// the album, are left alone: nothing is ever deleted or copied twice.
func (e *Exporter) Pass(ctx context.Context, job Job) (PassResult, error) {
	var res PassResult
	log := e.log.With("job", job.Name)

	// Checked every pass, dry run included: ingest may be set up on a parent
	// directory long after this job was configured.
	if err := checkNotIngested(job.Dest); err != nil {
		return res, err
	}

	albumID, err := e.immich.AlbumID(ctx, job.Album)
	if err != nil {
		return res, err
	}
	assets, err := e.immich.AlbumAssets(ctx, albumID)
	if err != nil {
		return res, fmt.Errorf("list album %q: %w", job.Album, err)
	}
	log.Debug("album listed", "album", job.Album, "assets", len(assets))

	for _, a := range assets {
		if err := e.export(ctx, log, job, a, &res); err != nil {
			return res, err
		}
		if !job.LivePhotoVideo || a.LivePhotoVideoID == "" {
			continue
		}
		// Check the state before asking Immich for the video's details, so a
		// steady-state pass costs one listing and no per-asset requests.
		if skip, err := e.skip(ctx, job, a.LivePhotoVideoID); err != nil {
			return res, err
		} else if skip {
			res.Skipped++
			continue
		}
		v, err := e.immich.Asset(ctx, a.LivePhotoVideoID)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			log.Warn("cannot read live photo video", "asset", a.ID, "video", a.LivePhotoVideoID, "err", err)
			res.Failed++
			continue
		}
		if err := e.export(ctx, log, job, v, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// skip reports whether an asset needs no work this pass: it is done, parked,
// or waiting out its retry backoff.
func (e *Exporter) skip(ctx context.Context, job Job, assetID string) (bool, error) {
	rec, ok, err := e.store.Lookup(ctx, job.Name, assetID)
	if err != nil || !ok {
		return false, err
	}
	switch rec.Status {
	case StatusDone, StatusFailedPermanent:
		return true, nil
	case StatusFailed:
		return rec.NextRetry > e.now().Unix(), nil
	}
	return false, nil
}

// export downloads one asset unless it needs no work. Only a state-DB error or
// cancellation is returned; a failed download is recorded and the pass goes on.
func (e *Exporter) export(ctx context.Context, log *slog.Logger, job Job, a Asset, res *PassResult) error {
	if skip, err := e.skip(ctx, job, a.ID); err != nil {
		return err
	} else if skip {
		res.Skipped++
		return nil
	}

	if e.cfg.DryRun {
		log.Info("would export", "asset", a.ID, "file", a.OriginalFileName)
		res.Planned++
		return nil
	}

	attempts, err := e.store.Claim(ctx, job.Name, a.ID)
	if err != nil {
		return err
	}
	name, size, err := e.download(ctx, job, a)
	if err != nil {
		if ctx.Err() != nil {
			// Left pending on purpose: the next run retries it.
			return ctx.Err()
		}
		status, serr := e.store.MarkFailed(ctx, job.Name, a.ID, attempts, err, e.cfg.State)
		if serr != nil {
			return serr
		}
		log.Warn("export failed", "asset", a.ID, "file", a.OriginalFileName,
			"attempt", attempts, "status", status, "err", err)
		res.Failed++
		return nil
	}
	if err := e.store.MarkDone(ctx, job.Name, a.ID, name, size); err != nil {
		return err
	}
	log.Info("exported", "asset", a.ID, "file", name, "bytes", size)
	res.Exported++
	return nil
}

func (e *Exporter) download(ctx context.Context, job Job, a Asset) (string, int64, error) {
	name, err := fileName(job.Dest, a.OriginalFileName, a.ID)
	if err != nil {
		return "", 0, err
	}
	body, err := e.immich.Original(ctx, a.ID)
	if err != nil {
		return "", 0, err
	}
	defer body.Close()

	// The capture time as mtime: gallery apps on the phone sort by it when a
	// file carries no EXIF date, and a download time would bunch the album up.
	n, err := writeFile(ctx, body, filepath.Join(job.Dest, name), a.FileCreatedAt)
	if err != nil {
		return "", 0, err
	}
	return name, n, nil
}
