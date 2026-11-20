// Package store persists users, invites, memberships, sessions and the audit
// log in SQLite. The deployment runs a single replica with a PVC.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrUsernameTaken  = errors.New("username already taken")
	ErrInviteUnusable = errors.New("invite is expired, revoked or already used")
)

type Store struct{ db *sql.DB }

func sqliteDSN(path string) string {
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", path)
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY
	// between our own goroutines and keeps transactions simple.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrations are applied in order; PRAGMA user_version records progress.
// Append new entries, never edit existing ones.
var migrations = []string{`
CREATE TABLE users (
	id             INTEGER PRIMARY KEY,
	username       TEXT NOT NULL UNIQUE COLLATE NOCASE,
	email          TEXT NOT NULL DEFAULT '',
	password_hash  TEXT NOT NULL,
	totp_secret    BLOB NOT NULL,
	totp_last_step INTEGER NOT NULL DEFAULT 0,
	is_admin       INTEGER NOT NULL DEFAULT 0,
	disabled       INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL
);
CREATE TABLE recovery_codes (
	user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	code_hash TEXT NOT NULL,
	used_at   INTEGER,
	PRIMARY KEY (user_id, code_hash)
);
CREATE TABLE memberships (
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	namespace  TEXT NOT NULL,
	role       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, namespace)
);
CREATE TABLE invites (
	id           INTEGER PRIMARY KEY,
	token_hash   TEXT NOT NULL UNIQUE,
	email        TEXT NOT NULL DEFAULT '',
	namespace    TEXT NOT NULL DEFAULT '',
	role         TEXT NOT NULL DEFAULT '',
	make_admin   INTEGER NOT NULL DEFAULT 0,
	pending_totp BLOB,
	created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	used_at      INTEGER,
	used_by      INTEGER REFERENCES users(id) ON DELETE SET NULL,
	revoked_at   INTEGER
);
CREATE TABLE sessions (
	id_hash    TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE audit (
	id        INTEGER PRIMARY KEY,
	at        INTEGER NOT NULL,
	actor_id  INTEGER,
	actor     TEXT NOT NULL,
	action    TEXT NOT NULL,
	namespace TEXT NOT NULL DEFAULT '',
	target    TEXT NOT NULL DEFAULT '',
	ip        TEXT NOT NULL DEFAULT ''
);
`, `
CREATE INDEX audit_at ON audit(at);
`}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback() // the Exec error is the one worth returning
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			_ = tx.Rollback() // the Exec error is the one worth returning
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func isUnique(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

func unix(t time.Time) int64 { return t.Unix() }

func fromUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback() // fn's error is the one worth returning
		return err
	}
	return tx.Commit()
}
