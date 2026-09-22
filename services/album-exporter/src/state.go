package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Status values. "pending" doubles as the crash marker: a row still pending at
// the next pass means a previous run died mid-download, and it is retried.
const (
	StatusPending         = "pending"
	StatusDone            = "done"
	StatusFailed          = "failed"
	StatusFailedPermanent = "failed_permanent"
)

const schemaVersion = 1

// Record is one row of the exports table.
type Record struct {
	Job       string
	AssetID   string
	Status    string
	FileName  string
	Size      int64
	Attempts  int
	LastError string
	NextRetry int64
	UpdatedAt int64
}

// Store remembers which assets have been exported. It, and not the contents of
// the destination, is the answer to "was this exported already": that is what
// lets files be deleted from the destination without being copied back.
type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// The write volume is a few rows per asset. A single connection costs
	// nothing and removes every SQLITE_BUSY class of bug.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	migrations := []string{
		// Keyed by Immich asset ID, which is stable across renames, edits of
		// metadata and moves in the library, rather than by name or content.
		`CREATE TABLE IF NOT EXISTS exports (
			job        TEXT    NOT NULL,
			asset_id   TEXT    NOT NULL,
			status     TEXT    NOT NULL,
			file_name  TEXT    NOT NULL DEFAULT '',
			size       INTEGER NOT NULL DEFAULT 0,
			attempts   INTEGER NOT NULL DEFAULT 0,
			last_error TEXT    NOT NULL DEFAULT '',
			first_seen INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			next_retry INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (job, asset_id)
		) WITHOUT ROWID;
		CREATE INDEX IF NOT EXISTS exports_status_idx ON exports(job, status);`,
	}
	for i := v; i < len(migrations); i++ {
		if _, err := s.db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if v < schemaVersion {
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return err
		}
	}
	return nil
}

// Lookup returns the record for (job, assetID), if any.
func (s *Store) Lookup(ctx context.Context, job, assetID string) (Record, bool, error) {
	r := Record{Job: job, AssetID: assetID}
	err := s.db.QueryRowContext(ctx,
		`SELECT status, file_name, size, attempts, last_error, next_retry, updated_at
		   FROM exports WHERE job = ? AND asset_id = ?`, job, assetID,
	).Scan(&r.Status, &r.FileName, &r.Size, &r.Attempts, &r.LastError, &r.NextRetry, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return r, true, nil
}

// Claim marks an asset as in flight and bumps its attempt counter, returning
// the new count.
func (s *Store) Claim(ctx context.Context, job, assetID string) (int, error) {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO exports (job, asset_id, status, attempts, first_seen, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)
		 ON CONFLICT(job, asset_id) DO UPDATE SET
		   status     = excluded.status,
		   attempts   = exports.attempts + 1,
		   updated_at = excluded.updated_at`,
		job, assetID, StatusPending, now, now)
	if err != nil {
		return 0, err
	}
	var attempts int
	err = s.db.QueryRowContext(ctx,
		`SELECT attempts FROM exports WHERE job = ? AND asset_id = ?`, job, assetID).Scan(&attempts)
	return attempts, err
}

func (s *Store) MarkDone(ctx context.Context, job, assetID, fileName string, size int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE exports SET status = ?, file_name = ?, size = ?,
		                    last_error = '', next_retry = 0, updated_at = ?
		 WHERE job = ? AND asset_id = ?`,
		StatusDone, fileName, size, time.Now().Unix(), job, assetID)
	return err
}

// MarkFailed records a failure and returns the status it settled on, so the
// caller can tell a retryable failure from a parked one.
func (s *Store) MarkFailed(ctx context.Context, job, assetID string, attempts int, cause error, c StateConfig) (string, error) {
	status := StatusFailed
	var nextRetry int64
	if attempts >= c.MaxAttempts {
		status = StatusFailedPermanent
	} else {
		nextRetry = time.Now().Add(backoff(c, attempts)).Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE exports SET status = ?, last_error = ?, next_retry = ?, updated_at = ?
		 WHERE job = ? AND asset_id = ?`,
		status, cause.Error(), nextRetry, time.Now().Unix(), job, assetID)
	return status, err
}

// backoff grows exponentially from RetryBackoff, capped at RetryMax.
func backoff(c StateConfig, attempts int) time.Duration {
	d := c.RetryBackoff.D()
	for i := 1; i < attempts && d < c.RetryMax.D(); i++ {
		d *= 2
	}
	return min(d, c.RetryMax.D())
}

// Failures lists a job's parked or retrying assets, newest first.
func (s *Store) Failures(ctx context.Context, job string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT asset_id, status, attempts, last_error, next_retry, updated_at
		   FROM exports WHERE job = ? AND status IN (?, ?) ORDER BY updated_at DESC`,
		job, StatusFailed, StatusFailedPermanent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		r := Record{Job: job}
		if err := rows.Scan(&r.AssetID, &r.Status, &r.Attempts, &r.LastError, &r.NextRetry, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Counts returns a job's number of rows per status.
func (s *Store) Counts(ctx context.Context, job string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, count(*) FROM exports WHERE job = ? GROUP BY status`, job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}
