package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505) — e.g. a second live payment hitting the
// meridian_payments_levy_uniq partial index. Services map this onto their
// domain 409 errors (R4-9b).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsPostgres reports whether the store is backed by Postgres (DATABASE_URL
// profile); services use it to select the transactional code paths that
// only exist on the pg backend.
func (s *Store) IsPostgres() bool { return s.pool != nil }

// Postgres backend (H1/H3): when DATABASE_URL is set the same Store API is
// served by a pgx/v5 pool over a single jsonb documents table — the storage
// model mirrors the embedded JSON store exactly (collections of keyed JSON
// documents), so no behaviour changes between dev and prod profiles.

// pgDDL is the idempotent, auto-migrated schema (the repo's migration
// convention: IF NOT EXISTS DDL applied at OpenPostgres startup; there are
// no external migration files). Statements:
//  1. the documents table itself;
//  2. R4-9b: a partial UNIQUE index enforcing cross-channel presumptive
//     levy uniqueness at the DATABASE layer — one live (pending-or-posted)
//     payment per (tenant, tin, period, instalment class). The status set
//     mirrors presumptive.duplicateLevyStatuses exactly. The per-process
//     mutex in CreateIntent stays as the fast path / nicer error, but only
//     this index serialises concurrent creates across replicas sharing one
//     Postgres. tenant_id is not yet a Payment field; COALESCE keeps the
//     key stable for forward compatibility (single-tenant rows hash to '').
const pgDDL = `CREATE TABLE IF NOT EXISTS meridian_docs (
	collection TEXT NOT NULL,
	id         TEXT NOT NULL,
	doc        JSONB NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (collection, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS meridian_payments_levy_uniq ON meridian_docs (
	(COALESCE(doc->>'tenant_id', '')),
	(doc->>'tin_hash'),
	(doc->>'period'),
	(COALESCE(doc->>'monthly', 'false'))
) WHERE collection = 'payments' AND doc->>'status' IN (
	'intent', 'pending_authorisation', 'authorised',
	'captured_awaiting_post', 'capture_in_flight',
	'captured', 'settled', 'disputed'
);
-- Perf (H1/H2/H4): additive secondary expression indexes backing
-- Store.ListWhere point queries. All IF NOT EXISTS / additive only.
CREATE INDEX IF NOT EXISTS meridian_payments_tin_hash ON meridian_docs ((doc->>'tin_hash')) WHERE collection = 'payments';
CREATE INDEX IF NOT EXISTS meridian_agents_parent_id ON meridian_docs ((doc->>'parent_id')) WHERE collection = 'agents';
CREATE INDEX IF NOT EXISTS meridian_operators_nin_hash ON meridian_docs ((doc->>'nin_hash')) WHERE collection = 'operators';
CREATE INDEX IF NOT EXISTS meridian_operators_client_ref ON meridian_docs ((doc->>'client_ref')) WHERE collection = 'operators';`

func (s *Store) pgPut(coll, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = s.pool.Exec(ctx,
		`INSERT INTO meridian_docs (collection, id, doc) VALUES ($1,$2,$3)
		 ON CONFLICT (collection, id) DO UPDATE SET doc = EXCLUDED.doc, updated_at = now()`,
		coll, id, b)
	return err
}

// pgPutIfAbsent is the atomic INSERT ... ON CONFLICT DO NOTHING backing
// Store.PutIfAbsent (B3 #7): the (collection, id) PRIMARY KEY is the
// uniqueness constraint; exactly one concurrent claimant wins.
func (s *Store) pgPutIfAbsent(coll, id string, b []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO meridian_docs (collection, id, doc) VALUES ($1,$2,$3)
		 ON CONFLICT (collection, id) DO NOTHING`,
		coll, id, b)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) pgGet(coll, id string, v any) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT doc FROM meridian_docs WHERE collection=$1 AND id=$2`, coll, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

func (s *Store) pgDelete(coll, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM meridian_docs WHERE collection=$1 AND id=$2`, coll, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) pgList(coll string, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT doc FROM meridian_docs WHERE collection=$1`, coll)
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanDocs(rows, out)
}

// pgListWhere is the secondary-index point query backing Store.ListWhere on
// the Postgres backend: served by the per-collection expression indexes in
// pgDDL instead of a full collection scan + Go-side filter (perf H1/H2/H4).
func (s *Store) pgListWhere(coll, field, value string, out any) error {
	if !validDocField(field) {
		return fmt.Errorf("store: invalid document field %q", field)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT doc FROM meridian_docs WHERE collection=$1 AND doc->>$2=$3`,
		coll, field, value)
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanDocs(rows, out)
}

// validDocField guards the one place a field name reaches SQL as a bound
// parameter (doc->>$2 is already parameterised, so this is defence in depth:
// restrict to the JSON field-name shape services actually use).
func validDocField(field string) bool {
	if field == "" || len(field) > 64 {
		return false
	}
	for _, r := range field {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// scanDocs decodes each row directly into a new slice element — a single
// JSON pass per document (perf H9: replaces RawMessage -> Marshal ->
// Unmarshal double round trip on every Postgres list/scan).
func scanDocs(rows pgx.Rows, out any) error {
	sv, et, err := sliceTarget(out)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		if err := unmarshalDoc(raw, sv, et); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) pgCount(coll string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM meridian_docs WHERE collection=$1`, coll).Scan(&n); err != nil {
		return 0
	}
	return n
}

// OpenPostgres connects to DATABASE_URL via pgx/v5 and auto-migrates the
// (idempotent) documents-table DDL on startup.
func OpenPostgres(dsn string) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := pool.Exec(ctx, pgDDL); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{
		pool:      pool,
		data:      map[string]map[string]json.RawMessage{},
		idxFields: map[string][]string{},
		idx:       map[string]map[string]map[string][]string{},
		idxByID:   map[string]map[string]map[string]string{},
	}, nil
}

// OpenFromEnvProfile selects the storage backend per H1: DATABASE_URL set →
// Postgres via pgx/v5 (profile=prod); unset → embedded JSON store at
// STORE_FILE / in-memory (profile=dev). Startup NEVER fails because
// DATABASE_URL is missing; if Postgres is unreachable it falls back to the
// embedded store with a warning so services still start.
func OpenFromEnvProfile() (*Store, error) {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		st, err := OpenPostgres(dsn)
		if err != nil {
			log.Printf("profile=prod component=store postgres unavailable (%v); falling back to embedded store", err)
			return OpenFromEnv()
		}
		log.Printf("profile=prod component=store (postgres)")
		return st, nil
	}
	log.Printf("profile=dev component=store (embedded)")
	return OpenFromEnv()
}
