package api

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"signal-forge/internal/models"
)

// ErrNotFound is returned when a widget with the given ID does not exist.
var ErrNotFound = errors.New("widget not found")

// ErrConflict is returned when creating a widget with a duplicate name.
var ErrConflict = errors.New("widget name already exists")

// WidgetStore is a thread-safe in-memory store for Widget objects.
type WidgetStore struct {
	mu      sync.RWMutex
	widgets map[string]*models.Widget
	byName  map[string]string // name → id
}

// NewWidgetStore initialises an empty store.
func NewWidgetStore() *WidgetStore {
	return &WidgetStore{
		widgets: make(map[string]*models.Widget),
		byName:  make(map[string]string),
	}
}

// Create inserts a new widget. Returns ErrConflict if the name is taken.
func (s *WidgetStore) Create(req models.CreateWidgetRequest) (*models.Widget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.byName[req.Name]; exists {
		return nil, ErrConflict
	}

	now := time.Now().UTC()
	w := &models.Widget{
		ID:          uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		Color:       req.Color,
		Weight:      req.Weight,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.widgets[w.ID] = w
	s.byName[w.Name] = w.ID
	return w, nil
}

// Get returns the widget with the given ID, or ErrNotFound.
func (s *WidgetStore) Get(id string) (*models.Widget, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	w, ok := s.widgets[id]
	if !ok {
		return nil, ErrNotFound
	}
	copy := *w
	return &copy, nil
}

// List returns all widgets. Optionally filter by color.
func (s *WidgetStore) List(colorFilter string) ([]models.Widget, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]models.Widget, 0, len(s.widgets))
	for _, w := range s.widgets {
		if colorFilter != "" && w.Color != colorFilter {
			continue
		}
		out = append(out, *w)
	}
	return out, nil
}

// Delete removes a widget by ID. Returns ErrNotFound if it doesn't exist.
func (s *WidgetStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.widgets[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.byName, w.Name)
	delete(s.widgets, id)
	return nil
}
