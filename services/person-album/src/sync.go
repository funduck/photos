package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// batchSize caps the asset IDs sent in one PUT /api/albums/{id}/assets.
const batchSize = 500

// Syncer runs passes over the configured jobs.
type Syncer struct {
	cfg Config
	// client returns the Immich client for a job's API key.
	client func(apiKey string) *Immich
	store  *Store
	log    *slog.Logger
	now    func() time.Time
}

// PassResult counts what one pass over one job did.
type PassResult struct {
	Added, Planned, Failed, Skipped int
}

// Pass adds whatever matches the job's people and has not been added yet.
// Assets that were added and later removed from the album are left alone.
func (s *Syncer) Pass(ctx context.Context, job Job) (PassResult, error) {
	var res PassResult
	log := s.log.With("job", job.Name)
	c := s.client(job.APIKey)

	albumID, err := c.AlbumID(ctx, job.Album)
	if err != nil {
		return res, err
	}
	personIDs, err := s.personIDs(ctx, c, job)
	if err != nil {
		return res, err
	}
	assets, err := s.matching(ctx, c, job, personIDs)
	if err != nil {
		return res, err
	}
	log.Debug("people matched", "people", job.People, "match", job.Match, "assets", len(assets))

	var todo []Asset
	for _, a := range assets {
		if skip, err := s.skip(ctx, job, a.ID); err != nil {
			return res, err
		} else if skip {
			res.Skipped++
			continue
		}
		todo = append(todo, a)
	}

	if s.cfg.DryRun {
		for _, a := range todo {
			log.Info("would add", "asset", a.ID, "file", a.OriginalFileName, "album", job.Album)
		}
		res.Planned = len(todo)
		return res, nil
	}

	for batch := range slices.Chunk(todo, batchSize) {
		if err := s.add(ctx, c, log, job, albumID, batch, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (s *Syncer) personIDs(ctx context.Context, c *Immich, job Job) ([]string, error) {
	people, err := c.People(ctx)
	if err != nil {
		return nil, fmt.Errorf("list people: %w", err)
	}
	var ids []string
	for _, p := range job.People {
		id, err := PersonID(people, p)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// matching lists the assets the job wants. Immich ANDs person IDs, so "all" is
// one search, and "any" is one search per person merged by asset ID.
func (s *Syncer) matching(ctx context.Context, c *Immich, job Job, personIDs []string) ([]Asset, error) {
	if job.Match == MatchAll {
		assets, err := c.PersonAssets(ctx, personIDs)
		if err != nil {
			return nil, fmt.Errorf("search people %v: %w", job.People, err)
		}
		return assets, nil
	}

	var out []Asset
	seen := map[string]bool{}
	for i, id := range personIDs {
		assets, err := c.PersonAssets(ctx, []string{id})
		if err != nil {
			return nil, fmt.Errorf("search person %q: %w", job.People[i], err)
		}
		for _, a := range assets {
			if !seen[a.ID] {
				seen[a.ID] = true
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// skip reports whether an asset needs no work this pass: it is done, parked,
// or waiting out its retry backoff.
func (s *Syncer) skip(ctx context.Context, job Job, assetID string) (bool, error) {
	rec, ok, err := s.store.Lookup(ctx, job.Name, assetID)
	if err != nil || !ok {
		return false, err
	}
	switch rec.Status {
	case StatusDone, StatusFailedPermanent:
		return true, nil
	case StatusFailed:
		return rec.NextRetry > s.now().Unix(), nil
	}
	return false, nil
}

// add sends one batch. Only a state-DB error or cancellation is returned; a
// rejected batch or asset is recorded and the pass goes on.
func (s *Syncer) add(ctx context.Context, c *Immich, log *slog.Logger, job Job, albumID string, batch []Asset, res *PassResult) error {
	ids := make([]string, len(batch))
	for i, a := range batch {
		ids[i] = a.ID
	}

	results, err := c.AddToAlbum(ctx, albumID, ids)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, a := range batch {
			if err := s.fail(ctx, log, job, a, err, res); err != nil {
				return err
			}
		}
		return nil
	}

	byID := make(map[string]AddResult, len(results))
	for _, r := range results {
		byID[r.ID] = r
	}
	var done []string
	for _, a := range batch {
		r, ok := byID[a.ID]
		var cause error
		switch {
		case !ok:
			cause = errors.New("no result for this asset in the response")
		case r.Success:
			log.Info("added", "asset", a.ID, "file", a.OriginalFileName, "album", job.Album)
			done = append(done, a.ID)
			res.Added++
			continue
		case r.Error == "duplicate":
			// Already in the album, put there by hand or by an earlier run
			// whose state was lost. Recorded, so removing it later sticks.
			log.Debug("already in album", "asset", a.ID, "file", a.OriginalFileName)
			done = append(done, a.ID)
			res.Skipped++
			continue
		default:
			cause = fmt.Errorf("%s %s", r.Error, r.ErrorMessage)
		}
		if err := s.fail(ctx, log, job, a, cause, res); err != nil {
			return err
		}
	}
	return s.store.MarkDone(ctx, job.Name, done)
}

func (s *Syncer) fail(ctx context.Context, log *slog.Logger, job Job, a Asset, cause error, res *PassResult) error {
	status, err := s.store.MarkFailed(ctx, job.Name, a.ID, cause, s.cfg.State)
	if err != nil {
		return err
	}
	log.Warn("add failed", "asset", a.ID, "file", a.OriginalFileName, "status", status, "err", cause)
	res.Failed++
	return nil
}
