package store_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"transform-registry/gen"
	"transform-registry/internal/store"
)

// fixture returns a minimal valid Transform for test setup.
func fixture(id, name string) gen.Transform {
	return gen.Transform{
		TransformId: id,
		Name:        name,
		Vrl:         `.message = "test"`,
		Status:      gen.TransformStatusCOMPILING,
		CreatedAt:   time.Now().UTC(),
	}
}

// -----------------------------------------------------------------------------
// Create
// -----------------------------------------------------------------------------

func TestCreate_Succeeds(t *testing.T) {
	s := store.NewMemoryStore()
	tr := fixture("id-1", "redact-emails")
	if err := s.Create(tr); err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
}

func TestCreate_ConflictOnDuplicateName(t *testing.T) {
	s := store.NewMemoryStore()
	_ = s.Create(fixture("id-1", "redact-emails"))

	err := s.Create(fixture("id-2", "redact-emails")) // same name, different id
	if err == nil {
		t.Fatal("Create: expected ErrConflict, got nil")
	}
}

func TestCreate_AllowsDifferentNames(t *testing.T) {
	s := store.NewMemoryStore()
	_ = s.Create(fixture("id-1", "redact-emails"))
	if err := s.Create(fixture("id-2", "redact-ipv4")); err != nil {
		t.Fatalf("Create: expected success for different name, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Get
// -----------------------------------------------------------------------------

func TestGet_Found(t *testing.T) {
	s := store.NewMemoryStore()
	tr := fixture("id-1", "redact-emails")
	_ = s.Create(tr)

	got, err := s.Get("id-1")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if got.TransformId != "id-1" {
		t.Errorf("Get: expected id-1, got %s", got.TransformId)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := store.NewMemoryStore()
	_, err := s.Get("does-not-exist")
	if err == nil {
		t.Fatal("Get: expected ErrNotFound, got nil")
	}
}

// -----------------------------------------------------------------------------
// List
// -----------------------------------------------------------------------------

func TestList_NoFilter_ReturnsAll(t *testing.T) {
	s := store.NewMemoryStore()
	_ = s.Create(fixture("id-1", "a"))
	_ = s.Create(fixture("id-2", "b"))
	_ = s.Create(fixture("id-3", "c"))

	results := s.List(nil, nil)
	if len(results) != 3 {
		t.Errorf("List: expected 3, got %d", len(results))
	}
}

func TestList_StatusFilter(t *testing.T) {
	s := store.NewMemoryStore()
	_ = s.Create(fixture("id-1", "a"))
	_ = s.Create(fixture("id-2", "b"))

	// Transition id-2 to ACTIVE
	now := time.Now().UTC()
	_ = s.UpdateStatus("id-2", gen.TransformStatusACTIVE, &now, nil)

	active := gen.TransformStatusACTIVE
	results := s.List(&active, nil)
	if len(results) != 1 || results[0].TransformId != "id-2" {
		t.Errorf("List(status=ACTIVE): expected [id-2], got %v", results)
	}
}

func TestList_NameFilter_CaseInsensitive(t *testing.T) {
	s := store.NewMemoryStore()
	_ = s.Create(fixture("id-1", "redact-emails"))
	_ = s.Create(fixture("id-2", "redact-ipv4"))
	_ = s.Create(fixture("id-3", "enrich-service"))

	name := "REDACT"
	results := s.List(nil, &name)
	if len(results) != 2 {
		t.Errorf("List(name=REDACT): expected 2, got %d", len(results))
	}
}

func TestList_Empty(t *testing.T) {
	s := store.NewMemoryStore()
	results := s.List(nil, nil)
	if results == nil {
		t.Error("List on empty store should return empty slice, not nil")
	}
	if len(results) != 0 {
		t.Errorf("List: expected 0, got %d", len(results))
	}
}

// -----------------------------------------------------------------------------
// UpdateStatus
// -----------------------------------------------------------------------------

func TestUpdateStatus_ToActive(t *testing.T) {
	s := store.NewMemoryStore()
	_ = s.Create(fixture("id-1", "a"))

	now := time.Now().UTC()
	if err := s.UpdateStatus("id-1", gen.TransformStatusACTIVE, &now, nil); err != nil {
		t.Fatalf("UpdateStatus: unexpected error: %v", err)
	}

	tr, _ := s.Get("id-1")
	if tr.Status != gen.TransformStatusACTIVE {
		t.Errorf("UpdateStatus: expected ACTIVE, got %s", tr.Status)
	}
	if tr.CompiledAt == nil {
		t.Error("UpdateStatus: CompiledAt should be set after ACTIVE transition")
	}
}

func TestUpdateStatus_ToFailed(t *testing.T) {
	s := store.NewMemoryStore()
	_ = s.Create(fixture("id-1", "a"))

	msg := "VRL compilation error: unexpected token"
	if err := s.UpdateStatus("id-1", gen.TransformStatusFAILED, nil, &msg); err != nil {
		t.Fatalf("UpdateStatus: unexpected error: %v", err)
	}

	tr, _ := s.Get("id-1")
	if tr.Status != gen.TransformStatusFAILED {
		t.Errorf("UpdateStatus: expected FAILED, got %s", tr.Status)
	}
	if tr.ErrorMessage == nil || *tr.ErrorMessage != msg {
		t.Errorf("UpdateStatus: ErrorMessage = %v, want %q", tr.ErrorMessage, msg)
	}
}

func TestUpdateStatus_NotFound(t *testing.T) {
	s := store.NewMemoryStore()
	err := s.UpdateStatus("no-such-id", gen.TransformStatusACTIVE, nil, nil)
	if err == nil {
		t.Fatal("UpdateStatus: expected ErrNotFound, got nil")
	}
}

// -----------------------------------------------------------------------------
// Delete
// -----------------------------------------------------------------------------

func TestDelete_Succeeds(t *testing.T) {
	s := store.NewMemoryStore()
	_ = s.Create(fixture("id-1", "a"))

	if err := s.Delete("id-1"); err != nil {
		t.Fatalf("Delete: unexpected error: %v", err)
	}
	if _, err := s.Get("id-1"); err == nil {
		t.Fatal("Delete: record still accessible after deletion")
	}
}

func TestDelete_NotFound(t *testing.T) {
	s := store.NewMemoryStore()
	if err := s.Delete("no-such-id"); err == nil {
		t.Fatal("Delete: expected ErrNotFound, got nil")
	}
}

// -----------------------------------------------------------------------------
// Concurrency — ensure no races under parallel access
// -----------------------------------------------------------------------------

func TestConcurrentCreateAndGet(t *testing.T) {
	s := store.NewMemoryStore()
	var wg sync.WaitGroup
	const n = 50

	// Concurrent creates with unique names
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("id-%d", i)
			name := fmt.Sprintf("transform-%d", i)
			_ = s.Create(fixture(id, name))
		}(i)
	}

	// Concurrent reads (some will miss, that's fine)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.Get(fmt.Sprintf("id-%d", i))
		}(i)
	}

	wg.Wait()

	results := s.List(nil, nil)
	if len(results) != n {
		t.Errorf("ConcurrentCreate: expected %d transforms, got %d", n, len(results))
	}
}
