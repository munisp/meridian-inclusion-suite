// Package store is the embedded dev store (§1.3: every service runs with zero
// external deps in dev mode). JSON-document collections with optional file
// persistence (STORE_FILE env); when unset it is purely in-memory.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]map[string]json.RawMessage
	pool *pgxpool.Pool // non-nil => Postgres backend (DATABASE_URL, see pg.go)

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
	s := &Store{path: path, data: map[string]map[string]json.RawMessage{}}
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

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
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
	return s.persistLocked()
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
	var raws []json.RawMessage
	for _, raw := range s.data[coll] {
		raws = append(raws, raw)
	}
	b, err := json.Marshal(raws)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
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
