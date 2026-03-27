// Package store provides a thread-safe in-memory implementation of the
// Transform Registry datastore.
//
// In production this would be backed by PostgreSQL (or equivalent); the
// Store interface is designed so the handler layer is storage-agnostic and
// the implementation can be swapped without touching a single handler.
package store

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"transform-registry/gen"
)

// ErrNotFound is returned when a requested transform does not exist.
var ErrNotFound = fmt.Errorf("transform not found")

// ErrConflict is returned when a uniqueness constraint is violated.
var ErrConflict = fmt.Errorf("conflict")

// Store is the interface the handler layer depends on.
// Keeping it as an interface makes the handlers trivially testable with a
// mock implementation.
type Store interface {
	// Create persists a new transform record.
	// Returns ErrConflict if a transform with the same Name already exists.
	Create(t gen.Transform) error

	// Get returns the transform with the given ID.
	// Returns ErrNotFound if it does not exist.
	Get(id string) (gen.Transform, error)

	// List returns all transforms, optionally filtered by status and/or name
	// substring (case-insensitive). Both filters are AND-combined.
	// A nil filter value means "no filter on this field".
	List(statusFilter *gen.TransformStatus, nameFilter *string) []gen.Transform

	// UpdateStatus atomically sets the status (and optional fields) of the
	// transform with the given ID.
	// Returns ErrNotFound if it does not exist.
	UpdateStatus(id string, status gen.TransformStatus, compiledAt *time.Time, errMsg *string) error

	// Delete removes the transform with the given ID.
	// Returns ErrNotFound if it does not exist.
	Delete(id string) error

	// ExistsByName returns true if a transform with the given name already exists.
	ExistsByName(name string) bool
}

// -----------------------------------------------------------------------------
// MemoryStore — in-memory implementation
// -----------------------------------------------------------------------------

// MemoryStore is a thread-safe, in-memory Store backed by a plain Go map.
// All methods acquire the appropriate read or write lock so concurrent
// HTTP requests are handled correctly.
type MemoryStore struct {
	mu         sync.RWMutex
	transforms map[string]gen.Transform // transformId → Transform
}

// NewMemoryStore returns an initialised, empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		transforms: make(map[string]gen.Transform),
	}
}

// Create persists t to the store.
// Returns ErrConflict if a transform with t.Name already exists.
func (s *MemoryStore) Create(t gen.Transform) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.transforms {
		if existing.Name == t.Name {
			return fmt.Errorf("%w: a transform named %q already exists", ErrConflict, t.Name)
		}
	}
	s.transforms[t.TransformId] = t
	return nil
}

// Get returns the transform for the given ID.
func (s *MemoryStore) Get(id string) (gen.Transform, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.transforms[id]
	if !ok {
		return gen.Transform{}, ErrNotFound
	}
	return t, nil
}

// List returns all transforms, applying optional status and name filters.
func (s *MemoryStore) List(statusFilter *gen.TransformStatus, nameFilter *string) []gen.Transform {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]gen.Transform, 0, len(s.transforms))
	for _, t := range s.transforms {
		if statusFilter != nil && t.Status != *statusFilter {
			continue
		}
		if nameFilter != nil && *nameFilter != "" {
			if !strings.Contains(strings.ToLower(t.Name), strings.ToLower(*nameFilter)) {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// UpdateStatus atomically updates the status of the transform with the given ID.
// compiledAt and errMsg are optional — pass nil to leave them unchanged.
func (s *MemoryStore) UpdateStatus(
	id string,
	status gen.TransformStatus,
	compiledAt *time.Time,
	errMsg *string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.transforms[id]
	if !ok {
		return ErrNotFound
	}
	t.Status = status
	if compiledAt != nil {
		t.CompiledAt = compiledAt
	}
	if errMsg != nil {
		t.ErrorMessage = errMsg
	}
	s.transforms[id] = t
	return nil
}

// Delete removes the transform with the given ID.
func (s *MemoryStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.transforms[id]; !ok {
		return ErrNotFound
	}
	delete(s.transforms, id)
	return nil
}

// ExistsByName returns true if any transform in the store has the given name.
func (s *MemoryStore) ExistsByName(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.transforms {
		if t.Name == name {
			return true
		}
	}
	return false
}
