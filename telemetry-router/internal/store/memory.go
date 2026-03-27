// Package store provides an in-memory datastore that mimics what a real
// PostgreSQL-backed repository would expose. In production this layer would be
// replaced with actual DB calls; the interface remains identical.
package store

import (
	"fmt"
	"sync"
	"time"
)

// DestinationRecord mirrors the DB row from the destinations table.
type DestinationRecord struct {
	DestinationID string
	InstanceID    string
	Name          string
	Type          string
	Status        string
	Config        map[string]interface{} // serialised OTLPConfig or S3Config
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DeliveryAttemptRecord mirrors the delivery_attempts table.
type DeliveryAttemptRecord struct {
	AttemptID     string
	EventID       string
	DestinationID string
	AttemptNumber int
	Status        string // "SUCCESS" | "FAILED" | "IN_FLIGHT"
	ErrorMessage  string
	AttemptedAt   time.Time
	DurationMs    int64
}

// Store is a thread-safe in-memory implementation.
type Store struct {
	mu           sync.RWMutex
	destinations map[string]*DestinationRecord   // destinationId → record
	attempts     map[string]*DeliveryAttemptRecord // attemptId → record
	tasks        map[string]string                 // taskId → status ("PENDING"|"DONE"|"FAILED")
}

func New() *Store {
	return &Store{
		destinations: make(map[string]*DestinationRecord),
		attempts:     make(map[string]*DeliveryAttemptRecord),
		tasks:        make(map[string]string),
	}
}

// --- Destination CRUD ---

func (s *Store) CreateDestination(rec *DestinationRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.destinations[rec.DestinationID]; exists {
		return fmt.Errorf("destination %s already exists", rec.DestinationID)
	}
	s.destinations[rec.DestinationID] = rec
	return nil
}

func (s *Store) GetDestination(id string) (*DestinationRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.destinations[id]
	if !ok {
		return nil, fmt.Errorf("destination %s not found", id)
	}
	return rec, nil
}

func (s *Store) ListDestinations(instanceID string) ([]*DestinationRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*DestinationRecord
	for _, rec := range s.destinations {
		if rec.InstanceID == instanceID {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *Store) UpdateDestination(id string, fields map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.destinations[id]
	if !ok {
		return fmt.Errorf("destination %s not found", id)
	}
	if name, ok := fields["name"].(string); ok && name != "" {
		rec.Name = name
	}
	if cfg, ok := fields["config"].(map[string]interface{}); ok {
		rec.Config = cfg
	}
	if status, ok := fields["status"].(string); ok && status != "" {
		rec.Status = status
	}
	rec.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) DeleteDestination(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.destinations[id]; !ok {
		return fmt.Errorf("destination %s not found", id)
	}
	delete(s.destinations, id)
	return nil
}

// --- Delivery attempts ---

func (s *Store) RecordAttempt(rec *DeliveryAttemptRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[rec.AttemptID] = rec
	return nil
}

// HealthSummary returns success/failure counts for the last hour for a destination.
func (s *Store) HealthSummary(destinationID string) (successes, failures int, lastErr string, lastAt *time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().UTC().Add(-1 * time.Hour)
	for _, a := range s.attempts {
		if a.DestinationID != destinationID || a.AttemptedAt.Before(cutoff) {
			continue
		}
		if a.Status == "SUCCESS" {
			successes++
			if lastAt == nil || a.AttemptedAt.After(*lastAt) {
				t := a.AttemptedAt
				lastAt = &t
			}
		} else if a.Status == "FAILED" {
			failures++
			lastErr = a.ErrorMessage
		}
	}
	return
}

// --- Async task tracking ---

func (s *Store) CreateTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[taskID] = "PENDING"
}

func (s *Store) SetTaskStatus(taskID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[taskID] = status
}

func (s *Store) GetTaskStatus(taskID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.tasks[taskID]
	return st, ok
}
