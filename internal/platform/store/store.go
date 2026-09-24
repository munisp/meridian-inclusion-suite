// Package store is the embedded dev store (§1.3: every service runs with zero
// external deps in dev mode). JSON-document collections with optional file
// persistence (STORE_FILE env); when unset it is purely in-memory.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]map[string]json.RawMessage
	pool *pgxpool.Pool // non-nil => Postgres backend (DATABASE_URL, see pg.go)

	// Secondary indexes (perf H2/H4): registered (collection, field) pairs
	// are maintained in memory on every write so ListWhere is a point
	// lookup instead of a full-collection scan on the embedded backend.
	// On Postgres ListWhere is served by the matching expression indexes
	// (see pgDDL) and these maps stay unused.
	idxFields map[string][]string                       // coll -> indexed fields
	idx       map[string]map[string]map[string][]string // coll -> field -> value -> ids
	idxByID   map[string]map[string]map[string]string   // coll -> id -> field -> value

	// Debounced file persistence (perf H5), OPT-IN via
	// EnableDebouncedPersistence: writes coalesce into at most one
	// full-file flush per persistDelay instead of rewriting the whole DB
	// file on every Put. Default remains synchronous write-through.
	debounce     bool
	persistTimer *time.Timer
	persistErr   error // sticky async flush error, surfaced by Flush

	// faultHook, when non-nil (tests only), is consulted before each op; a
	// non-nil return simulates a database fault (timeout, deadlock) and the
	// op is refused WITHOUT mutating state (assurance R7 §6.3 db-fault
	// cells). op is one of "put"|"get"|"delete"|"list".
	faultHook func(op, coll, id string) error
}

// SetFaultHook installs a test-only fault-injection hook; nil clears it.
func (s *Store) SetFaultHook(h func(op, coll, id string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faultHook = h
}

func (s *Store) fault(op, coll, id string) error {
	s.mu.RLock()
	h := s.faultHook
	s.mu.RUnlock()
	if h == nil {
		return nil
	}
	return h(op, coll, id)
}

// Open loads (or creates) a JSON file store. path "" => in-memory only.
func Open(path string) (*Store, error) {
	s := &Store{
		path:      path,
		data:      map[string]map[string]json.RawMessage{},
		idxFields: map[string][]string{},
		idx:       map[string]map[string]map[string][]string{},
		idxByID:   map[string]map[string]map[string]string{},
	}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// OpenFromEnv uses STORE_FILE (defaults in-memory).
func OpenFromEnv() (*Store, error) { return Open(os.Getenv("STORE_FILE")) }

// persistDelay is the debounce window for STORE_FILE persistence: rapid
// write bursts (e.g. 2-3 session Puts per USSD step) coalesce into a single
// full-file flush instead of one full rewrite per Put (perf H5, measured
// 9 ms/Put @5k docs before).
const persistDelay = 100 * time.Millisecond

// EnableDebouncedPersistence switches STORE_FILE persistence from
// synchronous write-per-Put to a coalesced flush at most every persistDelay
// (perf H5: measured 9 ms/Put @5k docs synchronous; debounced, a USSD
// step's 2-3 session Puts cost one flush). Trade-off: up to persistDelay of
// recent writes can be lost on a hard crash — appropriate for volatile
// session stores, NOT for ledgers/registries (those keep the synchronous
// default, which the restart-durability tests pin).
func (s *Store) EnableDebouncedPersistence() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.debounce = true
}

// persistLocked persists the store to s.path: synchronously by default, or
// by scheduling a coalesced async flush when debounce is enabled. Callers
// must hold s.mu. Flush forces an immediate synchronous write.
func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if s.persistErr != nil {
		return s.persistErr
	}
	if !s.debounce {
		return s.writeFileLocked()
	}
	if s.persistTimer != nil {
		return nil // a flush is already pending; it will pick up this write
	}
	s.persistTimer = time.AfterFunc(persistDelay, func() {
		// The flush snapshots s.data under the write lock, so it includes
		// every write that landed before it starts; any write landing after
		// persistTimer is cleared schedules a fresh flush.
		s.mu.Lock()
		defer s.mu.Unlock()
		s.persistTimer = nil
		if err := s.writeFileLocked(); err != nil {
			s.persistErr = err
		}
	})
	return nil
}

func (s *Store) writeFileLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Flush forces any pending debounced persistence to disk synchronously and
// reports the first async flush error, if any. Call before process exit (or
// in tests) when STORE_FILE durability must be guaranteed at a point in time.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persistErr != nil {
		return s.persistErr
	}
	if s.persistTimer != nil {
		s.persistTimer.Stop()
		s.persistTimer = nil
	}
	if s.path == "" {
		return nil
	}
	return s.writeFileLocked()
}

func (s *Store) Put(coll, id string, v any) error {
	if err := s.fault("put", coll, id); err != nil {
		return err
	}
	if s.pool != nil {
		return s.pgPut(coll, id, v)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[coll] == nil {
		s.data[coll] = map[string]json.RawMessage{}
	}
	s.data[coll][id] = b
	s.reindexLocked(coll, id, b)
	return s.persistLocked()
}

// PutIfAbsent inserts a document only when (collection, id) is unused
// (B3 #7: atomic check-then-act for idempotency claims; mirrors the
// PutIfAbsent precedent in core packages/events/store). Returns false
// when the key already exists. Backed by the PRIMARY KEY +
// INSERT ... ON CONFLICT DO NOTHING in Postgres and by the store mutex
// in the embedded backend.
func (s *Store) PutIfAbsent(coll, id string, v any) (bool, error) {
	if err := s.fault("put", coll, id); err != nil {
		return false, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	if s.pool != nil {
		return s.pgPutIfAbsent(coll, id, b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[coll] != nil {
		if _, ok := s.data[coll][id]; ok {
			return false, nil
		}
	}
	if s.data[coll] == nil {
		s.data[coll] = map[string]json.RawMessage{}
	}
	s.data[coll][id] = b
	s.reindexLocked(coll, id, b)
	return true, s.persistLocked()
}

func (s *Store) Get(coll, id string, v any) (bool, error) {
	if err := s.fault("get", coll, id); err != nil {
		return false, err
	}
	if s.pool != nil {
		return s.pgGet(coll, id, v)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, ok := s.data[coll][id]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}

// Delete removes a record; returns false if absent.
func (s *Store) Delete(coll, id string) (bool, error) {
	if err := s.fault("delete", coll, id); err != nil {
		return false, err
	}
	if s.pool != nil {
		return s.pgDelete(coll, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[coll][id]; !ok {
		return false, nil
	}
	delete(s.data[coll], id)
	s.deindexLocked(coll, id)
	return true, s.persistLocked()
}

// List decodes every record in a collection into out, which must be a
// pointer to a slice of the element type.
func (s *Store) List(coll string, out any) error {
	if err := s.fault("list", coll, ""); err != nil {
		return err
	}
	if s.pool != nil {
		return s.pgList(coll, out)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return unmarshalSliceValues(s.data[coll], out)
}

// unmarshalSliceValues decodes each JSON document in docs directly into a
// new element of the slice out points to — a single JSON pass per document
// (perf H9: the old List did RawMessage -> Marshal -> Unmarshal, i.e. two
// full round trips per document on every scan).
func unmarshalSliceValues(docs map[string]json.RawMessage, out any) error {
	sv, et, err := sliceTarget(out)
	if err != nil {
		return err
	}
	sv.Set(reflect.MakeSlice(sv.Type(), 0, len(docs)))
	for _, raw := range docs {
		ev := reflect.New(et)
		if err := json.Unmarshal(raw, ev.Interface()); err != nil {
			return err
		}
		sv.Set(reflect.Append(sv, ev.Elem()))
	}
	return nil
}

// sliceTarget validates out as a pointer to a slice, RESETS the slice to
// nil (matching the old Marshal/Unmarshal semantics, where an empty
// collection unmarshals to nil — re-using a non-nil slice must not leave
// stale elements), and returns the settable slice value and element type.
func sliceTarget(out any) (reflect.Value, reflect.Type, error) {
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Slice {
		return reflect.Value{}, nil, fmt.Errorf("store: out must be a pointer to a slice, got %T", out)
	}
	sv := rv.Elem()
	sv.Set(reflect.Zero(sv.Type()))
	return sv, sv.Type().Elem(), nil
}

// unmarshalDoc appends one decoded document to the slice out points to.
func unmarshalDoc(raw []byte, sv reflect.Value, et reflect.Type) error {
	ev := reflect.New(et)
	if err := json.Unmarshal(raw, ev.Interface()); err != nil {
		return err
	}
	sv.Set(reflect.Append(sv, ev.Elem()))
	return nil
}

// RegisterIndex declares (coll, field) as a secondary lookup key for the
// embedded backend: every subsequent write to coll maintains an in-memory
// field-value -> ids mapping, and ListWhere answers from it in O(matches)
// instead of scanning the whole collection. Existing documents are indexed
// once at registration. Idempotent. On the Postgres backend this is a no-op
// (ListWhere is served by the expression indexes in pgDDL).
func (s *Store) RegisterIndex(coll, field string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.idxFields[coll] {
		if f == field {
			return
		}
	}
	s.idxFields[coll] = append(s.idxFields[coll], field)
	if s.idx[coll] == nil {
		s.idx[coll] = map[string]map[string][]string{}
	}
	if s.idxByID[coll] == nil {
		s.idxByID[coll] = map[string]map[string]string{}
	}
	s.idx[coll][field] = map[string][]string{}
	for id, raw := range s.data[coll] {
		s.indexDocLocked(coll, id, field, raw)
	}
}

func extractField(raw json.RawMessage, field string) (string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	fv, ok := m[field]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(fv, &s); err != nil {
		return "", false
	}
	return s, true
}

// indexDocLocked adds id to the (coll, field) value index from raw.
func (s *Store) indexDocLocked(coll, id, field string, raw json.RawMessage) {
	val, ok := extractField(raw, field)
	if !ok || val == "" {
		return
	}
	s.idx[coll][field][val] = append(s.idx[coll][field][val], id)
	if s.idxByID[coll][id] == nil {
		s.idxByID[coll][id] = map[string]string{}
	}
	s.idxByID[coll][id][field] = val
}

// reindexLocked refreshes all registered indexes for (coll, id) after a
// write; b is the freshly marshalled document.
func (s *Store) reindexLocked(coll, id string, b json.RawMessage) {
	if len(s.idxFields[coll]) == 0 {
		return
	}
	s.deindexLocked(coll, id)
	for _, field := range s.idxFields[coll] {
		s.indexDocLocked(coll, id, field, b)
	}
}

// deindexLocked removes id from all registered indexes of coll.
func (s *Store) deindexLocked(coll, id string) {
	for field, val := range s.idxByID[coll][id] {
		ids := s.idx[coll][field][val]
		for i, x := range ids {
			if x == id {
				s.idx[coll][field][val] = append(ids[:i], ids[i+1:]...)
				break
			}
		}
		if len(s.idx[coll][field][val]) == 0 {
			delete(s.idx[coll][field], val)
		}
	}
	delete(s.idxByID[coll], id)
}

// ListWhere decodes the records of coll whose JSON field equals value into
// out (pointer to a slice). Embedded backend: answered from the registered
// in-memory index when available, otherwise a full scan with cheap per-doc
// field extraction (register the index via RegisterIndex for hot paths).
// Postgres backend: an indexed expression query (see pgDDL).
func (s *Store) ListWhere(coll, field, value string, out any) error {
	if err := s.fault("list", coll, ""); err != nil {
		return err
	}
	if s.pool != nil {
		return s.pgListWhere(coll, field, value, out)
	}
	sv, et, err := sliceTarget(out)
	if err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.idx[coll][field]; ok {
		ids := s.idx[coll][field][value]
		sv.Set(reflect.MakeSlice(sv.Type(), 0, len(ids)))
		for _, id := range ids {
			if err := unmarshalDoc(s.data[coll][id], sv, et); err != nil {
				return err
			}
		}
		return nil
	}
	// unregistered field: full scan, but only one cheap field extraction
	// per document; matches are decoded fully.
	for _, raw := range s.data[coll] {
		if val, ok := extractField(raw, field); ok && val == value {
			if err := unmarshalDoc(raw, sv, et); err != nil {
				return err
			}
		}
	}
	return nil
}

// Count returns the number of records in a collection.
func (s *Store) Count(coll string) int {
	if s.pool != nil {
		return s.pgCount(coll)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data[coll])
}
