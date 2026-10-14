package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/garyships/kudzu/internal/object"

	_ "modernc.org/sqlite"
)

// Meta is the authoritative store. Changesets are appended to a log with no
// locking beyond the insert itself; refs are moved separately by
// compare-and-swap on a single row. Derived data is a cache keyed by
// (kind, id) that any server may fill in.
type Meta interface {
	// AppendChangeset adds a changeset to the log, or returns the position
	// of the existing entry. Either way the entry's Created time becomes
	// now, so gc's grace period protects a changeset that was just
	// (re)created.
	AppendChangeset(ctx context.Context, id object.ID, raw []byte) (seq int64, err error)
	GetChangeset(ctx context.Context, id object.ID) (raw []byte, seq int64, err error)
	ResolvePrefix(ctx context.Context, hexPrefix string) (object.ID, error)
	WAL(ctx context.Context, since int64, limit int) ([]WALEntry, error)
	// DeleteChangesets removes the given log entries that were created
	// before cutoff (unix seconds) and returns how many it removed. The log
	// keeps its positions: later entries are not renumbered.
	DeleteChangesets(ctx context.Context, ids []object.ID, cutoff int64) (int, error)

	GetRef(ctx context.Context, name string) (object.ID, error)
	ListRefs(ctx context.Context) (map[string]object.ID, error)
	// CASRef moves name from old to new. A zero old means the ref must not
	// exist; a zero new deletes it. It returns ErrConflict if name no longer
	// points at old.
	CASRef(ctx context.Context, name string, old, new object.ID) error

	GetDerived(ctx context.Context, kind string, key object.ID) ([]byte, error)
	PutDerived(ctx context.Context, kind string, key object.ID, value []byte) error
	// DeleteDerived removes kind's values for keys and returns how many
	// existed.
	DeleteDerived(ctx context.Context, kind string, keys []object.ID) (int, error)

	GetMapping(ctx context.Context, ns, key string) (string, error)
	PutMapping(ctx context.Context, ns, key, value string) error
	// Mappings calls fn for every mapping in ns ("" for all namespaces).
	Mappings(ctx context.Context, ns string, fn func(ns, key, value string) error) error
	DeleteMapping(ctx context.Context, ns, key string) error

	Close() error
}

type WALEntry struct {
	Seq     int64
	ID      object.ID
	Raw     []byte
	Created int64 // unix seconds when the entry was last appended
}

type SQLite struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS changesets (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,
	id      BLOB NOT NULL UNIQUE,
	data    BLOB NOT NULL,
	created INTEGER NOT NULL DEFAULT 0
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
	if err := migrateCreated(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

// migrateCreated adds changesets.created to stores made before gc existed.
// Existing entries count as created now, so the first gc gives them a full
// grace period instead of treating them as ancient.
func migrateCreated(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('changesets')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == "created" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE changesets ADD COLUMN created INTEGER NOT NULL DEFAULT 0`); err != nil {
		if strings.Contains(err.Error(), "duplicate column") {
			return nil // another process migrated first
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE changesets SET created = ?`, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) AppendChangeset(ctx context.Context, id object.ID, raw []byte) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO changesets (id, data, created) VALUES (?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET created = excluded.created RETURNING seq`,
		id[:], raw, time.Now().Unix()).Scan(&seq)
	return seq, err
}

func (s *SQLite) DeleteChangesets(ctx context.Context, ids []object.ID, cutoff int64) (int, error) {
	n := 0
	for _, id := range ids {
		res, err := s.db.ExecContext(ctx, `DELETE FROM changesets WHERE id = ? AND created < ?`, id[:], cutoff)
		if err != nil {
			return n, err
		}
		k, err := res.RowsAffected()
		if err != nil {
			return n, err
		}
		n += int(k)
	}
	return n, nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT seq, id, data, created FROM changesets WHERE seq > ? ORDER BY seq LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WALEntry
	for rows.Next() {
		var e WALEntry
		var id []byte
		if err := rows.Scan(&e.Seq, &id, &e.Raw, &e.Created); err != nil {
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

func (s *SQLite) DeleteDerived(ctx context.Context, kind string, keys []object.ID) (int, error) {
	n := 0
	for _, k := range keys {
		res, err := s.db.ExecContext(ctx, `DELETE FROM derived WHERE kind = ? AND key = ?`, kind, k[:])
		if err != nil {
			return n, err
		}
		if k, err := res.RowsAffected(); err == nil {
			n += int(k)
		}
	}
	return n, nil
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

func (s *SQLite) Mappings(ctx context.Context, ns string, fn func(ns, key, value string) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT ns, key, value FROM mapping WHERE ? = '' OR ns = ? ORDER BY ns, key`, ns, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n, k, v string
		if err := rows.Scan(&n, &k, &v); err != nil {
			return err
		}
		if err := fn(n, k, v); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *SQLite) DeleteMapping(ctx context.Context, ns, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mapping WHERE ns = ? AND key = ?`, ns, key)
	return err
}
