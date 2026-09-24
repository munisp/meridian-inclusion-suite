package store

// pg_tx.go — explicit-transaction support for the Postgres backend (R4-9b).
// Multi-replica races cannot be closed by per-process mutexes; operations
// whose check-then-act must serialise ACROSS replicas (e.g. the onboarding
// hierarchy Attach cycle check + parent-link write) run inside a single
// database transaction with the involved rows locked via SELECT ... FOR
// UPDATE in a deterministic (id-sorted) order to avoid deadlock.
//
// These paths only exist on the Postgres backend (IsPostgres()); the
// embedded dev store keeps relying on the in-process mutexes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PgTx is a handle to an open Postgres transaction over meridian_docs.
type PgTx struct {
	tx   pgx.Tx
	done bool
}

// BeginTx opens a transaction on the Postgres backend. It returns
// pgx.ErrNoRows-shaped misuse protection: callers must check IsPostgres()
// first; calling BeginTx on the embedded backend is an error.
func (s *Store) BeginTx(ctx context.Context) (*PgTx, error) {
	if s.pool == nil {
		return nil, errors.New("store: BeginTx requires the Postgres backend")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &PgTx{tx: tx}, nil
}

// GetForUpdate reads a document inside the transaction and takes a row lock
// on it, serialising against any other transaction locking the same row.
// Callers MUST lock the rows of a multi-row operation in a deterministic
// (e.g. id-sorted) order across all code paths so concurrent transactions
// cannot deadlock.
func (t *PgTx) GetForUpdate(ctx context.Context, coll, id string, v any) (bool, error) {
	var raw []byte
	err := t.tx.QueryRow(ctx,
		`SELECT doc FROM meridian_docs WHERE collection=$1 AND id=$2 FOR UPDATE`,
		coll, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// Get is an ordinary (unlocked) read inside the transaction snapshot.
func (t *PgTx) Get(ctx context.Context, coll, id string, v any) (bool, error) {
	var raw []byte
	err := t.tx.QueryRow(ctx,
		`SELECT doc FROM meridian_docs WHERE collection=$1 AND id=$2`,
		coll, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// List decodes every record of a collection inside the transaction, so
// whole-collection validations (cycle/depth scans) see one consistent
// snapshot alongside the locked rows.
func (t *PgTx) List(ctx context.Context, coll string, out any) error {
	rows, err := t.tx.Query(ctx,
		`SELECT doc FROM meridian_docs WHERE collection=$1`, coll)
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanDocs(rows, out)
}

// ListWhere decodes the records of coll whose JSON field equals value,
// inside the transaction (indexed expression query; perf H3: Attach's
// cycle/depth validation now walks only the relevant child rows instead of
// full-table scans while holding FOR UPDATE locks).
func (t *PgTx) ListWhere(ctx context.Context, coll, field, value string, out any) error {
	if !validDocField(field) {
		return fmt.Errorf("store: invalid document field %q", field)
	}
	rows, err := t.tx.Query(ctx,
		`SELECT doc FROM meridian_docs WHERE collection=$1 AND doc->>$2=$3`,
		coll, field, value)
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanDocs(rows, out)
}

// Put upserts a document inside the transaction.
func (t *PgTx) Put(ctx context.Context, coll, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx,
		`INSERT INTO meridian_docs (collection, id, doc) VALUES ($1,$2,$3)
		 ON CONFLICT (collection, id) DO UPDATE SET doc = EXCLUDED.doc, updated_at = now()`,
		coll, id, b)
	return err
}

// Commit commits the transaction; safe to call once.
func (t *PgTx) Commit(ctx context.Context) error {
	t.done = true
	return t.tx.Commit(ctx)
}

// Rollback rolls the transaction back. It is idempotent and a no-op after
// Commit, so it can (and should) be deferred right after BeginTx.
func (t *PgTx) Rollback() {
	if t.done {
		return
	}
	t.done = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = t.tx.Rollback(ctx)
}
