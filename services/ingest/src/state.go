package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Status values. "pending" doubles as the crash marker: a row still pending at
// discovery time means a previous run died mid-flight.
const (
	StatusPending         = "pending"
	StatusDone            = "done"
	StatusFailed          = "failed"
	StatusFailedPermanent = "failed_permanent"
)

// Actions recorded against a done row.
const (
	ActionCopy      = "copy"
	ActionTranscode = "transcode"
	ActionAdopted   = "adopted"
)

const schemaVersion = 2

// Record is one row of the files table.
type Record struct {
	RelPath   string
	Size      int64
	Status    string
	Action    string
	Reason    string
	DestSize  int64
	Attempts  int
	LastError string
	NextRetry int64
	UpdatedAt int64
	Bitrate   int64
}

type Store struct {
	db   *sql.DB
	path string
}

// StateDBPath returns where the state DB for this source tree lives. An explicit
// state.path wins; otherwise the DB is namespaced by a slug of the source path,
// so pointing a second instance at a different tree cannot reuse this one's state.
func StateDBPath(c StateConfig, source string) (string, error) {
	if c.Path != "" {
		return c.Path, nil
	}
	if c.Dir == "" {
		return "", errors.New("state.dir or state.path is required")
	}
	return filepath.Join(c.Dir, sourceSlug(source), "state.db"), nil
}

// sourceSlug turns an absolute path into one readable directory name. Readable
// beats a bare hash here: you can tell at a glance which state belongs to which
// source. A hash suffix is only appended when the name would be unwieldy or
// ambiguous after sanitizing.
func sourceSlug(source string) string {
	clean := filepath.Clean(source)
	trimmed := strings.Trim(filepath.ToSlash(clean), "/")

	var b strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	slug := b.String()

	sum := sha256.Sum256([]byte(clean))
	short := hex.EncodeToString(sum[:])[:8]

	// Turning separators into dashes is the expected, lossless-enough rewrite.
	// Anything beyond that means characters were folded away, so two different
	// sources could now produce the same name — disambiguate with a hash.
	expected := strings.ReplaceAll(trimmed, "/", "-")

	switch {
	case slug == "":
		return short
	case len(slug) > 100:
		return slug[:100] + "-" + short
	case slug != expected:
		return slug + "-" + short
	default:
		return slug
	}
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
	// The write volume is a few rows per file. A single connection costs nothing
	// and removes every SQLITE_BUSY class of bug from a multi-worker writer.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS files (
			rel_path   TEXT    NOT NULL,
			size       INTEGER NOT NULL,
			status     TEXT    NOT NULL,
			action     TEXT    NOT NULL DEFAULT '',
			reason     TEXT    NOT NULL DEFAULT '',
			dest_size  INTEGER NOT NULL DEFAULT 0,
			attempts   INTEGER NOT NULL DEFAULT 0,
			last_error TEXT    NOT NULL DEFAULT '',
			first_seen INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			next_retry INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (rel_path, size)
		) WITHOUT ROWID;
		CREATE INDEX IF NOT EXISTS files_status_idx ON files(status, next_retry);`,
		`ALTER TABLE files ADD COLUMN bitrate INTEGER NOT NULL DEFAULT 0;`,
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

// Lookup returns the record for (rel, size), if any. Size is part of the key on
// purpose: a file replaced at the same path is a different file and gets
// reprocessed, while an mtime touch changes nothing.
func (s *Store) Lookup(ctx context.Context, rel string, size int64) (Record, bool, error) {
	r := Record{RelPath: rel, Size: size}
	err := s.db.QueryRowContext(ctx,
		`SELECT status, action, reason, dest_size, attempts, last_error, next_retry, updated_at, bitrate
		   FROM files WHERE rel_path = ? AND size = ?`, rel, size,
	).Scan(&r.Status, &r.Action, &r.Reason, &r.DestSize, &r.Attempts, &r.LastError, &r.NextRetry, &r.UpdatedAt, &r.Bitrate)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return r, true, nil
}

// Claim marks a file as in-flight and bumps its attempt counter, returning the
// new count.
func (s *Store) Claim(ctx context.Context, rel string, size int64) (int, error) {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO files (rel_path, size, status, attempts, first_seen, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)
		 ON CONFLICT(rel_path, size) DO UPDATE SET
		   status     = excluded.status,
		   attempts   = files.attempts + 1,
		   updated_at = excluded.updated_at`,
		rel, size, StatusPending, now, now)
	if err != nil {
		return 0, err
	}
	var attempts int
	err = s.db.QueryRowContext(ctx,
		`SELECT attempts FROM files WHERE rel_path = ? AND size = ?`, rel, size).Scan(&attempts)
	return attempts, err
}

// Adopt records a file whose destination copy already exists, without copying
// anything. This is what stops the first run from re-ingesting the whole library.
func (s *Store) Adopt(ctx context.Context, rel string, size, destSize int64, reason string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO files (rel_path, size, status, action, reason, dest_size, first_seen, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(rel_path, size) DO UPDATE SET
		   status = excluded.status, action = excluded.action, reason = excluded.reason,
		   dest_size = excluded.dest_size, updated_at = excluded.updated_at`,
		rel, size, StatusDone, ActionAdopted, reason, destSize, now, now)
	return err
}

// MarkPlanned records that a file needs work, along with what an inventory run
// decided should happen to it.
//
// The plan is advisory. A later run decides again from the current config, so
// action/reason on a pending row means "what we expect to do" while the same
// columns on a done row mean "what was actually done" — the status tells you
// which you are reading.
//
// Unlike Claim it leaves the attempt counter alone: nothing has been attempted,
// so repeating an inventory pass must never eat into max_attempts. An existing
// row that is not pending keeps its status — a done or failed file must not be
// dragged backwards.
func (s *Store) MarkPlanned(ctx context.Context, rel string, size int64, action, reason string, bitrate int64) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO files (rel_path, size, status, action, reason, bitrate, first_seen, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(rel_path, size) DO UPDATE SET
		   action = excluded.action, reason = excluded.reason,
		   bitrate = excluded.bitrate, updated_at = excluded.updated_at
		 WHERE files.status = ?`,
		rel, size, StatusPending, action, reason, bitrate, now, now, StatusPending)
	return err
}

// PendingFiles lists the outstanding rows individually, grouped by planned
// action and largest first — the big files are the ones worth looking at
// before committing an evening of encoding to them.
func (s *Store) PendingFiles(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rel_path, size, action, reason, bitrate
		   FROM files WHERE status = ? ORDER BY action, size DESC`, StatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.RelPath, &r.Size, &r.Action, &r.Reason, &r.Bitrate); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PlannedWork groups the outstanding pending rows by the action an inventory
// run expects to take.
type PlannedWork struct {
	Action string
	Files  int
	Bytes  int64
}

// Pending returns what is queued up but not yet done, newest decision first.
func (s *Store) Pending(ctx context.Context) ([]PlannedWork, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT CASE WHEN action = '' THEN 'undecided' ELSE action END, count(*), coalesce(sum(size), 0)
		   FROM files WHERE status = ? GROUP BY 1 ORDER BY 2 DESC`, StatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PlannedWork
	for rows.Next() {
		var w PlannedWork
		if err := rows.Scan(&w.Action, &w.Files, &w.Bytes); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) MarkDone(ctx context.Context, rel string, size int64, action, reason string, destSize int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE files SET status = ?, action = ?, reason = ?, dest_size = ?,
		                  last_error = '', next_retry = 0, updated_at = ?
		 WHERE rel_path = ? AND size = ?`,
		StatusDone, action, reason, destSize, time.Now().Unix(), rel, size)
	return err
}

// MarkFailed records a failure and returns the status it settled on, so the
// caller can tell a retryable failure from a parked one.
func (s *Store) MarkFailed(ctx context.Context, rel string, size int64, attempts int, cause error, c StateConfig) (string, error) {
	status := StatusFailed
	var nextRetry int64
	if attempts >= c.MaxAttempts {
		status = StatusFailedPermanent
	} else {
		nextRetry = time.Now().Add(backoff(c, attempts)).Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE files SET status = ?, last_error = ?, next_retry = ?, updated_at = ?
		 WHERE rel_path = ? AND size = ?`,
		status, cause.Error(), nextRetry, time.Now().Unix(), rel, size)
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

// Failures lists everything currently parked or awaiting retry, newest first.
func (s *Store) Failures(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rel_path, size, status, attempts, last_error, next_retry, updated_at
		   FROM files WHERE status IN (?, ?) ORDER BY updated_at DESC`,
		StatusFailed, StatusFailedPermanent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.RelPath, &r.Size, &r.Status, &r.Attempts, &r.LastError, &r.NextRetry, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Counts returns the number of rows per status, for the startup banner.
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*) FROM files GROUP BY status`)
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
