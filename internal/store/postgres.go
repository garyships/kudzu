package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/garyships/kudzu/internal/object"
)

// Postgres is a Meta shared by any number of servers. Refs move by a
// conditional UPDATE, exactly like the SQLite row CAS, so lands from
// different servers race safely; derived data and mappings are plain
// key/value tables any server may fill in.
type Postgres struct {
	pool *pgxpool.Pool
}

const pgSchema = `
CREATE TABLE IF NOT EXISTS changesets (
	seq     BIGSERIAL PRIMARY KEY,
	id      BYTEA NOT NULL UNIQUE,
	data    BYTEA NOT NULL,
	created BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS refs (
	name    TEXT PRIMARY KEY,
	target  BYTEA NOT NULL,
	version BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS derived (
	kind  TEXT NOT NULL,
	key   BYTEA NOT NULL,
	value BYTEA NOT NULL,
	PRIMARY KEY (kind, key)
);
CREATE TABLE IF NOT EXISTS mapping (
	ns    TEXT NOT NULL,
	key   TEXT NOT NULL,
	value TEXT NOT NULL,
	PRIMARY KEY (ns, key)
);
`

// appendLock serializes appends to the changeset log. A sequence hands out
// positions when rows are inserted, not when they commit, so two concurrent
// appends could commit out of order and a /api/wal reader that had already
// passed the later position would never see the earlier one. Holding a
// transaction-scoped advisory lock across the insert makes commit order
// match log order; the critical section is a single-row insert.
const appendLock = 0x6b75647a75 // "kudzu"

// OpenPostgres connects to a database URL (postgres://user:pass@host/db?...)
// and creates the tables if needed. Extra URL parameters such as
// search_path=kudzu are passed to the server, which keeps several
// repositories apart in one database.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	// Two servers starting at once would race to create the tables
	// (CREATE TABLE IF NOT EXISTS is not safe concurrently), so take a lock.
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(appendLock+1)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, pgSchema)
		return err
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}

func (p *Postgres) AppendChangeset(ctx context.Context, id object.ID, raw []byte) (int64, error) {
	var seq int64
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(appendLock)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO changesets (id, data, created) VALUES ($1, $2, $3)
			ON CONFLICT (id) DO UPDATE SET created = EXCLUDED.created RETURNING seq`,
			id[:], raw, time.Now().Unix()).Scan(&seq)
	})
	return seq, err
}

func (p *Postgres) GetChangeset(ctx context.Context, id object.ID) ([]byte, int64, error) {
	var raw []byte
	var seq int64
	err := p.pool.QueryRow(ctx, `SELECT data, seq FROM changesets WHERE id = $1`, id[:]).Scan(&raw, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	return raw, seq, err
}

func (p *Postgres) ResolvePrefix(ctx context.Context, prefix string) (object.ID, error) {
	prefix = strings.ToLower(prefix)
	if len(prefix) == 0 || len(prefix) > 2*object.IDSize {
		return object.Zero, ErrNotFound
	}
	lo, err1 := hex.DecodeString(pad(prefix, '0'))
	hi, err2 := hex.DecodeString(pad(prefix, 'f'))
	if err1 != nil || err2 != nil {
		return object.Zero, ErrNotFound
	}
	rows, err := p.pool.Query(ctx, `SELECT id FROM changesets WHERE id BETWEEN $1 AND $2 LIMIT 2`, lo, hi)
	if err != nil {
		return object.Zero, err
	}
	ids, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (object.ID, error) {
		var b []byte
		var id object.ID
		err := row.Scan(&b)
		copy(id[:], b)
		return id, err
	})
	if err != nil {
		return object.Zero, err
	}
	switch len(ids) {
	case 0:
		return object.Zero, ErrNotFound
	case 1:
		return ids[0], nil
	}
	return object.Zero, ErrAmbiguous
}

func (p *Postgres) WAL(ctx context.Context, since int64, limit int) ([]WALEntry, error) {
	rows, err := p.pool.Query(ctx, `SELECT seq, id, data, created FROM changesets WHERE seq > $1 ORDER BY seq LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (WALEntry, error) {
		var e WALEntry
		var id []byte
		err := row.Scan(&e.Seq, &id, &e.Raw, &e.Created)
		copy(e.ID[:], id)
		return e, err
	})
}

func (p *Postgres) DeleteChangesets(ctx context.Context, ids []object.ID, cutoff int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM changesets WHERE id = ANY($1) AND created < $2`, idSlices(ids), cutoff)
	return int(tag.RowsAffected()), err
}

func (p *Postgres) GetRef(ctx context.Context, name string) (object.ID, error) {
	var b []byte
	err := p.pool.QueryRow(ctx, `SELECT target FROM refs WHERE name = $1`, name).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return object.Zero, ErrNotFound
	}
	var id object.ID
	copy(id[:], b)
	return id, err
}

func (p *Postgres) ListRefs(ctx context.Context) (map[string]object.ID, error) {
	rows, err := p.pool.Query(ctx, `SELECT name, target FROM refs ORDER BY name`)
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

func (p *Postgres) CASRef(ctx context.Context, name string, old, new object.ID) error {
	var n int64
	switch {
	case old.IsZero() && new.IsZero():
		return nil
	case old.IsZero():
		tag, err := p.pool.Exec(ctx, `INSERT INTO refs (name, target, version) VALUES ($1, $2, 1) ON CONFLICT (name) DO NOTHING`, name, new[:])
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
	case new.IsZero():
		tag, err := p.pool.Exec(ctx, `DELETE FROM refs WHERE name = $1 AND target = $2`, name, old[:])
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
	default:
		tag, err := p.pool.Exec(ctx, `UPDATE refs SET target = $1, version = version + 1 WHERE name = $2 AND target = $3`, new[:], name, old[:])
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func (p *Postgres) GetDerived(ctx context.Context, kind string, key object.ID) ([]byte, error) {
	var v []byte
	err := p.pool.QueryRow(ctx, `SELECT value FROM derived WHERE kind = $1 AND key = $2`, kind, key[:]).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (p *Postgres) PutDerived(ctx context.Context, kind string, key object.ID, value []byte) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO derived (kind, key, value) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, kind, key[:], value)
	return err
}

func (p *Postgres) DeleteDerived(ctx context.Context, kind string, keys []object.ID) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM derived WHERE kind = $1 AND key = ANY($2)`, kind, idSlices(keys))
	return int(tag.RowsAffected()), err
}

func (p *Postgres) GetMapping(ctx context.Context, ns, key string) (string, error) {
	var v string
	err := p.pool.QueryRow(ctx, `SELECT value FROM mapping WHERE ns = $1 AND key = $2`, ns, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (p *Postgres) PutMapping(ctx context.Context, ns, key, value string) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO mapping (ns, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (ns, key) DO UPDATE SET value = EXCLUDED.value`, ns, key, value)
	return err
}

func (p *Postgres) Mappings(ctx context.Context, ns string, fn func(ns, key, value string) error) error {
	rows, err := p.pool.Query(ctx, `SELECT ns, key, value FROM mapping WHERE $1 = '' OR ns = $1 ORDER BY ns, key`, ns)
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

func (p *Postgres) DeleteMapping(ctx context.Context, ns, key string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM mapping WHERE ns = $1 AND key = $2`, ns, key)
	return err
}

func idSlices(ids []object.ID) [][]byte {
	out := make([][]byte, len(ids))
	for i := range ids {
		out[i] = ids[i][:]
	}
	return out
}
