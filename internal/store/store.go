// Package store provides SQLite-backed persistence for session metadata
// (and, in future milestones, users/credentials/audit). It uses
// modernc.org/sqlite — a pure-Go driver — so the project keeps building with
// CGO_ENABLED=0 and shipping static binaries.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Status describes the liveness of a session's underlying shell.
type Status string

const (
	// StatusRunning: the shell process is live in this server instance.
	StatusRunning Status = "running"
	// StatusDetached: metadata persisted but no live PTY (e.g. after a restart,
	// until session-survival lands and can reattach).
	StatusDetached Status = "detached"
	// StatusDead: the shell process has exited.
	StatusDead Status = "dead"
)

// SessionMeta is the persisted annotation for one terminal session. All times
// are unix seconds; LastClientDisconnectAt == 0 means "currently attached or
// never had a client".
type SessionMeta struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	CreatedAt              int64  `json:"createdAt"`
	LastActivityAt         int64  `json:"lastActivityAt"`
	LastClientDisconnectAt int64  `json:"lastClientDisconnectAt"`
	Cols                   int    `json:"cols"`
	Rows                   int    `json:"rows"`
	Status                 Status `json:"status"`
}

// Store wraps the SQLite connection. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions_meta (
  id                         TEXT PRIMARY KEY,
  name                       TEXT    NOT NULL,
  created_at                 INTEGER NOT NULL,
  last_activity_at           INTEGER NOT NULL,
  last_client_disconnect_at  INTEGER NOT NULL DEFAULT 0,
  cols                       INTEGER NOT NULL DEFAULT 0,
  rows                       INTEGER NOT NULL DEFAULT 0,
  status                     TEXT    NOT NULL DEFAULT 'running'
);`

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite is a single writer; one connection serializes writes and avoids
	// "database is locked" for our low-traffic workload.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Upsert inserts or updates a session row. created_at is preserved on update.
func (s *Store) Upsert(m SessionMeta) error {
	_, err := s.db.Exec(`
		INSERT INTO sessions_meta
			(id, name, created_at, last_activity_at, last_client_disconnect_at, cols, rows, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name                      = excluded.name,
			last_activity_at          = excluded.last_activity_at,
			last_client_disconnect_at = excluded.last_client_disconnect_at,
			cols                      = excluded.cols,
			rows                      = excluded.rows,
			status                    = excluded.status`,
		m.ID, m.Name, m.CreatedAt, m.LastActivityAt, m.LastClientDisconnectAt,
		m.Cols, m.Rows, string(m.Status))
	return err
}

// SetName updates a session's display name.
func (s *Store) SetName(id, name string) error {
	_, err := s.db.Exec(`UPDATE sessions_meta SET name = ? WHERE id = ?`, name, id)
	return err
}

// SetActivity records the last time the session produced output.
func (s *Store) SetActivity(id string, ts int64) error {
	_, err := s.db.Exec(`UPDATE sessions_meta SET last_activity_at = ? WHERE id = ?`, ts, id)
	return err
}

// SetClientDisconnect records when the last client detached (0 clients).
func (s *Store) SetClientDisconnect(id string, ts int64) error {
	_, err := s.db.Exec(`UPDATE sessions_meta SET last_client_disconnect_at = ? WHERE id = ?`, ts, id)
	return err
}

// SetSize records the last known terminal dimensions.
func (s *Store) SetSize(id string, cols, rows int) error {
	_, err := s.db.Exec(`UPDATE sessions_meta SET cols = ?, rows = ? WHERE id = ?`, cols, rows, id)
	return err
}

// SetStatus updates a session's liveness status.
func (s *Store) SetStatus(id string, status Status) error {
	_, err := s.db.Exec(`UPDATE sessions_meta SET status = ? WHERE id = ?`, string(status), id)
	return err
}

// MarkAllDetached flips every still-"running" row to "detached". Called on
// startup: a fresh process has no live PTYs yet, so anything previously marked
// running did not survive the restart.
func (s *Store) MarkAllDetached() error {
	_, err := s.db.Exec(`UPDATE sessions_meta SET status = ? WHERE status = ?`,
		string(StatusDetached), string(StatusRunning))
	return err
}

// Delete removes a session row.
func (s *Store) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions_meta WHERE id = ?`, id)
	return err
}

// List returns all session rows ordered by creation time.
func (s *Store) List() ([]SessionMeta, error) {
	rows, err := s.db.Query(`
		SELECT id, name, created_at, last_activity_at, last_client_disconnect_at, cols, rows, status
		FROM sessions_meta ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionMeta
	for rows.Next() {
		var m SessionMeta
		var status string
		if err := rows.Scan(&m.ID, &m.Name, &m.CreatedAt, &m.LastActivityAt,
			&m.LastClientDisconnectAt, &m.Cols, &m.Rows, &status); err != nil {
			return nil, err
		}
		m.Status = Status(status)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}
