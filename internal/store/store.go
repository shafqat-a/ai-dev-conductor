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
);

CREATE TABLE IF NOT EXISTS share_links (
  id          TEXT PRIMARY KEY,                 -- short public handle for list/revoke
  token_hash  BLOB NOT NULL UNIQUE,             -- sha256 of the raw token (the URL secret)
  session_id  TEXT NOT NULL,
  mode        TEXT NOT NULL DEFAULT 'read',
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  revoked     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_share_links_session ON share_links(session_id);`

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

// ShareLink is a public, time-boxed, read-only link to a session.
type ShareLink struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Mode      string `json:"mode"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	Revoked   bool   `json:"revoked"`
}

// MintShare records a new share link. tokenHash is sha256 of the raw token
// (the URL secret); only the hash is stored.
func (s *Store) MintShare(id string, tokenHash []byte, sessionID, mode string, createdAt, expiresAt int64) error {
	_, err := s.db.Exec(`
		INSERT INTO share_links (id, token_hash, session_id, mode, created_at, expires_at, revoked)
		VALUES (?, ?, ?, ?, ?, ?, 0)`,
		id, tokenHash, sessionID, mode, createdAt, expiresAt)
	return err
}

// RedeemShare resolves a token hash to its (sessionID, mode) if the link exists,
// is not revoked, and has not expired.
func (s *Store) RedeemShare(tokenHash []byte, now int64) (sessionID, mode string, ok bool, err error) {
	row := s.db.QueryRow(`
		SELECT session_id, mode FROM share_links
		WHERE token_hash = ? AND revoked = 0 AND expires_at > ?`, tokenHash, now)
	err = row.Scan(&sessionID, &mode)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return sessionID, mode, true, nil
}

// SharesForSession lists a session's share links (newest first).
func (s *Store) SharesForSession(sessionID string) ([]ShareLink, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, mode, created_at, expires_at, revoked
		FROM share_links WHERE session_id = ? ORDER BY created_at DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShareLink
	for rows.Next() {
		var sl ShareLink
		var revoked int
		if err := rows.Scan(&sl.ID, &sl.SessionID, &sl.Mode, &sl.CreatedAt, &sl.ExpiresAt, &revoked); err != nil {
			return nil, err
		}
		sl.Revoked = revoked != 0
		out = append(out, sl)
	}
	return out, rows.Err()
}

// RevokeShare marks a share link revoked by its public id.
func (s *Store) RevokeShare(id string) error {
	_, err := s.db.Exec(`UPDATE share_links SET revoked = 1 WHERE id = ?`, id)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}
