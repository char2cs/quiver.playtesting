// Package store is the SQLite-backed implementation of core.Store.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"quiver-playtesting/internal/core"
)

const tokenHashLen = 32

const schema = `
CREATE TABLE IF NOT EXISTS vms (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	host       TEXT NOT NULL,
	port       INTEGER NOT NULL,
	password   TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS links (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	vm_id      INTEGER REFERENCES vms(id) ON DELETE SET NULL,
	vm_name    TEXT NOT NULL,
	label      TEXT NOT NULL,
	token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	revoked    INTEGER NOT NULL DEFAULT 0,
	flagged    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS links_vm ON links(vm_id);
CREATE TABLE IF NOT EXISTS session_log (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	link_id    INTEGER NOT NULL,
	label      TEXT NOT NULL,
	vm_id      INTEGER NOT NULL,
	vm_name    TEXT NOT NULL,
	client_ip  TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	ended_at   INTEGER NOT NULL,
	reason     TEXT NOT NULL,
	recording  TEXT NOT NULL DEFAULT ''
);
`

const linkCols = `l.id, COALESCE(l.vm_id, 0), COALESCE(v.name, l.vm_name), l.label, l.expires_at, l.created_at, l.revoked, l.flagged
FROM links l LEFT JOIN vms v ON v.id = l.vm_id`

type Store struct {
	db *sql.DB
}

var _ core.Store = (*Store)(nil)

func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open db file: %w", err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod db file: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	dsn := (&url.URL{Scheme: "file", Opaque: url.PathEscape(path), RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			_ = os.Chmod(path+suffix, 0o600)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) int64 { return t.UnixMilli() }

func fromTS(n int64) time.Time { return time.UnixMilli(n).UTC() }

func (s *Store) AddVM(ctx context.Context, name, host string, port int, password string) (core.VM, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO vms (name, host, port, password, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, host, port, password, ts(now))
	if err != nil {
		return core.VM{}, fmt.Errorf("insert vm: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.VM{}, err
	}
	return core.VM{ID: id, Name: name, Host: host, Port: port, Password: password, CreatedAt: now}, nil
}

func (s *Store) ListVMs(ctx context.Context) ([]core.VM, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, host, port, created_at FROM vms ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.VM{}
	for rows.Next() {
		var v core.VM
		var created int64
		if err := rows.Scan(&v.ID, &v.Name, &v.Host, &v.Port, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = fromTS(created)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetVM(ctx context.Context, id int64) (core.VM, error) {
	var v core.VM
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, host, port, password, created_at FROM vms WHERE id = ?`, id).
		Scan(&v.ID, &v.Name, &v.Host, &v.Port, &v.Password, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return core.VM{}, core.ErrNotFound
	}
	if err != nil {
		return core.VM{}, err
	}
	v.CreatedAt = fromTS(created)
	return v, nil
}

func (s *Store) RemoveVM(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE links SET revoked = 1 WHERE vm_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM vms WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) CreateLink(ctx context.Context, vmID int64, label string, tokenHash []byte, expires time.Time) (core.Link, error) {
	if len(tokenHash) != tokenHashLen {
		return core.Link{}, errors.New("token hash must be 32 bytes")
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO links (vm_id, vm_name, label, token_hash, expires_at, created_at)
		 SELECT id, name, ?, ?, ?, ? FROM vms WHERE id = ?`,
		label, tokenHash, ts(expires), ts(now), vmID)
	if err != nil {
		return core.Link{}, fmt.Errorf("insert link: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.Link{}, core.ErrNotFound
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Link{}, err
	}
	return s.GetLink(ctx, id)
}

func scanLink(sc interface{ Scan(...any) error }) (core.Link, error) {
	var l core.Link
	var exp, created int64
	var revoked, flagged int
	if err := sc.Scan(&l.ID, &l.VMID, &l.VMName, &l.Label, &exp, &created, &revoked, &flagged); err != nil {
		return core.Link{}, err
	}
	l.ExpiresAt, l.CreatedAt = fromTS(exp), fromTS(created)
	l.Revoked, l.Flagged = revoked != 0, flagged != 0
	return l, nil
}

func (s *Store) ListLinks(ctx context.Context) ([]core.Link, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+linkCols+` ORDER BY l.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Link{}
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetLink(ctx context.Context, id int64) (core.Link, error) {
	l, err := scanLink(s.db.QueryRowContext(ctx, `SELECT `+linkCols+` WHERE l.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Link{}, core.ErrNotFound
	}
	return l, err
}

func (s *Store) LinkByTokenHash(ctx context.Context, tokenHash []byte) (core.Link, error) {
	l, err := scanLink(s.db.QueryRowContext(ctx, `SELECT `+linkCols+` WHERE l.token_hash = ?`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Link{}, core.ErrNotFound
	}
	return l, err
}

func (s *Store) setLinkFlag(ctx context.Context, col string, id int64) error {
	var q string
	switch col {
	case "revoked":
		q = `UPDATE links SET revoked = 1 WHERE id = ?`
	case "flagged":
		q = `UPDATE links SET flagged = 1 WHERE id = ?`
	default:
		return errors.New("bad column")
	}
	res, err := s.db.ExecContext(ctx, q, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	return nil
}

func (s *Store) RevokeLink(ctx context.Context, id int64) error {
	return s.setLinkFlag(ctx, "revoked", id)
}

func (s *Store) FlagLink(ctx context.Context, id int64) error {
	return s.setLinkFlag(ctx, "flagged", id)
}

func (s *Store) LogSession(ctx context.Context, sess core.Session, ended time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_log (session_id, link_id, label, vm_id, vm_name, client_ip, started_at, ended_at, reason, recording)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.LinkID, sess.Label, sess.VMID, sess.VMName, sess.ClientIP, ts(sess.StartedAt), ts(ended), reason, sess.Recording)
	return err
}

// PruneSessionLog deletes log rows that ended before cutoff and returns how many went.
func (s *Store) PruneSessionLog(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM session_log WHERE ended_at < ?`, ts(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// migrate adds columns that databases created by older releases lack.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(session_log)`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "recording" {
			has = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE session_log ADD COLUMN recording TEXT NOT NULL DEFAULT ''`)
	return err
}
