package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/garyships/kudzu/internal/object"

	_ "modernc.org/sqlite"
)

// Meta is the authoritative store. Changesets are appended to a log with no
// locking beyond the insert itself; refs are moved separately by
// compare-and-swap on a single row. Derived data is a cache keyed by
// (kind, id) that any server may fill in.
type Meta interface {
	AppendChangeset(ctx context.Context, id object.ID, raw []byte) (seq int64, err error)
	GetChangeset(ctx context.Context, id object.ID) (raw []byte, seq int64, err error)
	ResolvePrefix(ctx context.Context, hexPrefix string) (object.ID, error)
	WAL(ctx context.Context, since int64, limit int) ([]WALEntry, error)

	GetRef(ctx context.Context, name string) (object.ID, error)
	ListRefs(ctx context.Context) (map[string]object.ID, error)
	// CASRef moves name from old to new. A zero old means the ref must not
	// exist; a zero new deletes it. It returns ErrConflict if name no longer
	// points at old.
	CASRef(ctx context.Context, name string, old, new object.ID) error

	GetDerived(ctx context.Context, kind string, key object.ID) ([]byte, error)
	PutDerived(ctx context.Context, kind string, key object.ID, value []byte) error

	GetMapping(ctx context.Context, ns, key string) (string, error)
	PutMapping(ctx context.Context, ns, key, value string) error

	Close() error
}

type WALEntry struct {
	Seq int64
	ID  object.ID
	Raw []byte
}

type SQLite struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS changesets (
	seq  INTEGER PRIMARY KEY AUTOINCREMENT,
	id   BLOB NOT NULL UNIQUE,
	data BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS refs (
	name    TEXT PRIMARY KEY,
	target  BLOB NOT NULL,
	version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS derived (
	kind  TEXT NOT NULL,
	key   BLOB NOT NULL,
	value BLOB NOT NULL,
	PRIMARY KEY (kind, key)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS mapping (
	ns    TEXT NOT NULL,
	key   TEXT NOT NULL,
	value TEXT NOT NULL,
	PRIMARY KEY (ns, key)
) WITHOUT ROWID;
`

func OpenSQLite(path string) (*SQLite, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(20000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) AppendChangeset(ctx context.Context, id object.ID, raw []byte) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO changesets (id, data) VALUES (?, ?)`, id[:], raw); err != nil {
		return 0, err
	}
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT seq FROM changesets WHERE id = ?`, id[:]).Scan(&seq)
	return seq, err
}

func (s *SQLite) GetChangeset(ctx context.Context, id object.ID) ([]byte, int64, error) {
	var raw []byte
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT data, seq FROM changesets WHERE id = ?`, id[:]).Scan(&raw, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	return raw, seq, err
}

func (s *SQLite) ResolvePrefix(ctx context.Context, prefix string) (object.ID, error) {
	prefix = strings.ToLower(prefix)
	if len(prefix) == 0 || len(prefix) > 2*object.IDSize {
		return object.Zero, ErrNotFound
	}
	lo, err1 := hex.DecodeString(pad(prefix, '0'))
	hi, err2 := hex.DecodeString(pad(prefix, 'f'))
	if err1 != nil || err2 != nil {
		return object.Zero, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM changesets WHERE id BETWEEN ? AND ? LIMIT 2`, lo, hi)
	if err != nil {
		return object.Zero, err
	}
	defer rows.Close()
	var ids []object.ID
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return object.Zero, err
		}
		var id object.ID
		copy(id[:], b)
		ids = append(ids, id)
	}
	switch len(ids) {
	case 0:
		return object.Zero, ErrNotFound
	case 1:
		return ids[0], nil
	}
	return object.Zero, ErrAmbiguous
}

func pad(prefix string, c byte) string {
	return prefix + strings.Repeat(string(c), 2*object.IDSize-len(prefix))
}

func (s *SQLite) WAL(ctx context.Context, since int64, limit int) ([]WALEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, id, data FROM changesets WHERE seq > ? ORDER BY seq LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WALEntry
	for rows.Next() {
		var e WALEntry
		var id []byte
		if err := rows.Scan(&e.Seq, &id, &e.Raw); err != nil {
			return nil, err
		}
		copy(e.ID[:], id)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) GetRef(ctx context.Context, name string) (object.ID, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT target FROM refs WHERE name = ?`, name).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return object.Zero, ErrNotFound
	}
	var id object.ID
	copy(id[:], b)
	return id, err
}

func (s *SQLite) ListRefs(ctx context.Context) (map[string]object.ID, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, target FROM refs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]object.ID{}
	for rows.Next() {
		var name string
		var b []byte
		if err := rows.Scan(&name, &b); err != nil {
			return nil, err
		}
		var id object.ID
		copy(id[:], b)
		out[name] = id
	}
	return out, rows.Err()
}

func (s *SQLite) CASRef(ctx context.Context, name string, old, new object.ID) error {
	var res sql.Result
	var err error
	switch {
	case old.IsZero() && new.IsZero():
		return nil
	case old.IsZero():
		res, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO refs (name, target, version) VALUES (?, ?, 1)`, name, new[:])
	case new.IsZero():
		res, err = s.db.ExecContext(ctx, `DELETE FROM refs WHERE name = ? AND target = ?`, name, old[:])
	default:
		res, err = s.db.ExecContext(ctx, `UPDATE refs SET target = ?, version = version + 1 WHERE name = ? AND target = ?`, new[:], name, old[:])
	}
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrConflict
	}
	return nil
}

func (s *SQLite) GetDerived(ctx context.Context, kind string, key object.ID) ([]byte, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM derived WHERE kind = ? AND key = ?`, kind, key[:]).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *SQLite) PutDerived(ctx context.Context, kind string, key object.ID, value []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO derived (kind, key, value) VALUES (?, ?, ?)`, kind, key[:], value)
	return err
}

func (s *SQLite) GetMapping(ctx context.Context, ns, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM mapping WHERE ns = ? AND key = ?`, ns, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *SQLite) PutMapping(ctx context.Context, ns, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO mapping (ns, key, value) VALUES (?, ?, ?)`, ns, key, value)
	return err
}
