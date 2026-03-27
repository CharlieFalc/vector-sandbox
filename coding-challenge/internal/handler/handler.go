// Package handler implements the Transform Registry HTTP API using only the
// Go standard library (net/http, encoding/json).
//
// Go 1.22's enhanced ServeMux is used for method-prefixed routes and built-in
// path parameter extraction via r.PathValue("transformId").
//
// Each handler method follows the same structure:
//  1. Parse / validate request
//  2. Call the store
//  3. Launch any background work (compilation goroutine)
//  4. Write the response
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"transform-registry/gen"
	"transform-registry/internal/store"
)

// CompileFunc is the injectable VRL compilation function.
// The default (defaultCompile) simulates compilation with a random delay and
// a 20% failure rate.  Tests substitute a deterministic version.
type CompileFunc func(ctx context.Context, transformID, vrl string) error

// Handler holds the dependencies shared across all HTTP handlers.
type Handler struct {
	store   store.Store
	compile CompileFunc
}

// New returns a Handler wired to the given store and compilation function.
// Pass nil for compile to use the default simulator.
func New(s store.Store, compile CompileFunc) *Handler {
	if compile == nil {
		compile = defaultCompile
	}
	return &Handler{store: s, compile: compile}
}

// RegisterRoutes registers all Transform Registry routes on mux.
// Go 1.22+ ServeMux supports "METHOD /path" patterns natively.
func RegisterRoutes(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("POST /v1/transforms", h.createTransform)
	mux.HandleFunc("GET /v1/transforms", h.listTransforms)
	mux.HandleFunc("GET /v1/transforms/{transformId}", h.getTransform)
	mux.HandleFunc("DELETE /v1/transforms/{transformId}", h.deleteTransform)
}

// =============================================================================
// POST /v1/transforms → 201 Created
// =============================================================================

func (h *Handler) createTransform(w http.ResponseWriter, r *http.Request) {
	var req gen.CreateTransformRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Validate required fields.
	req.Name = strings.TrimSpace(req.Name)
	req.Vrl = strings.TrimSpace(req.Vrl)
	if req.Name == "" || req.Vrl == "" {
		writeError(w, http.StatusBadRequest, "'name' and 'vrl' are required and must be non-empty")
		return
	}

	// Unique-name check — 409 Conflict if already taken.
	if h.store.ExistsByName(req.Name) {
		writeError(w, http.StatusConflict, "a transform with this name already exists")
		return
	}

	// Build the transform record.  Status starts as COMPILING; the background
	// goroutine below transitions it to ACTIVE or FAILED.
	id := "tr-" + uuid.NewString()
	now := time.Now().UTC()

	t := gen.Transform{
		TransformId: id,
		Name:        req.Name,
		Description: req.Description,
		Vrl:         req.Vrl,
		Status:      gen.TransformStatusCOMPILING,
		CreatedAt:   now,
	}

	if err := h.store.Create(t); err != nil {
		// A second concurrent request may have created the same name between
		// the ExistsByName check and the Create call — guard against it.
		if isConflict(err) {
			writeError(w, http.StatusConflict, "a transform with this name already exists")
			return
		}
		log.Printf("[handler] create transform %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "failed to persist transform")
		return
	}

	// Launch the async VRL compilation.  The goroutine carries its own context
	// so it can be cancelled on server shutdown without leaking.
	go h.runCompilation(r.Context(), id, req.Vrl)

	// 201 Created — the resource exists in the DB immediately.
	// The client polls GET /v1/transforms/{id} for the ACTIVE/FAILED transition.
	writeJSON(w, http.StatusCreated, t)
}

// =============================================================================
// GET /v1/transforms → 200 OK
// =============================================================================

func (h *Handler) listTransforms(w http.ResponseWriter, r *http.Request) {
	params := parseListParams(r)
	items := h.store.List(params.Status, params.Name)

	// Ensure a non-nil slice so the JSON response is `[]` not `null`.
	if items == nil {
		items = []gen.Transform{}
	}

	writeJSON(w, http.StatusOK, gen.TransformList{
		Items: items,
		Total: len(items),
	})
}

// =============================================================================
// GET /v1/transforms/{transformId} → 200 OK | 404
// =============================================================================

func (h *Handler) getTransform(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("transformId")

	t, err := h.store.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "transform not found")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// =============================================================================
// DELETE /v1/transforms/{transformId} → 204 No Content | 404 | 409
// =============================================================================

func (h *Handler) deleteTransform(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("transformId")

	// Confirm the transform exists before the in-use check.
	if _, err := h.store.Get(id); err != nil {
		writeError(w, http.StatusNotFound, "transform not found")
		return
	}

	// Reject deletion if the transform is currently referenced by a router
	// instance.  checkInUse is a mock; 30% of calls return true.
	if checkInUse(id) {
		writeError(w, http.StatusConflict,
			"transform is referenced by 1 or more router instances; detach it before deleting")
		return
	}

	if err := h.store.Delete(id); err != nil {
		// Handle the rare race where another goroutine deleted it first.
		writeError(w, http.StatusNotFound, "transform not found")
		return
	}

	// 204 No Content — success with no body.
	w.WriteHeader(http.StatusNoContent)
}

// =============================================================================
// Background VRL compilation
// =============================================================================

// runCompilation simulates VRL compilation and updates the store when done.
// This runs in a goroutine launched by createTransform.
func (h *Handler) runCompilation(ctx context.Context, id, vrl string) {
	log.Printf("[compile] started: transform=%s", id)

	err := h.compile(ctx, id, vrl)

	now := time.Now().UTC()
	if err != nil {
		msg := err.Error()
		log.Printf("[compile] FAILED: transform=%s error=%s", id, msg)
		_ = h.store.UpdateStatus(id, gen.TransformStatusFAILED, nil, &msg)
		return
	}

	log.Printf("[compile] ACTIVE: transform=%s", id)
	_ = h.store.UpdateStatus(id, gen.TransformStatusACTIVE, &now, nil)
}

// defaultCompile simulates VRL compilation:
//   - Sleeps 2–4 seconds to mimic real compilation latency.
//   - Fails with 20% probability to exercise the FAILED path.
func defaultCompile(_ context.Context, _, _ string) error {
	delay := time.Duration(2+rand.Intn(3)) * time.Second // 2s, 3s, or 4s
	time.Sleep(delay)

	if rand.Float32() < 0.20 { // 20% failure rate
		return fmt.Errorf("VRL compilation error: unexpected token '.' at line 1, column 9")
	}
	return nil
}

// =============================================================================
// Mock: checkInUse
// =============================================================================

// checkInUse returns true if the transform is currently referenced by a router
// instance.  In production this would query the instances/destinations tables.
// Here we simulate 30% of deletes being blocked.
func checkInUse(_ string) bool {
	return rand.Float32() < 0.30
}

// =============================================================================
// Helpers
// =============================================================================

func parseListParams(r *http.Request) gen.ListTransformsParams {
	q := r.URL.Query()
	var p gen.ListTransformsParams

	if s := q.Get("status"); s != "" {
		status := gen.TransformStatus(s)
		p.Status = &status
	}
	if n := q.Get("name"); n != "" {
		p.Name = &n
	}
	return p
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[handler] writeJSON encode error: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, gen.APIError{Code: status, Message: msg})
}

func isConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "conflict")
}
