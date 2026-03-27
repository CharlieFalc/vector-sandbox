// Package api implements the STACKIT-style REST API for Telemetry Router
// destination management.
//
// URL scheme:
//
//	/v2/projects/{projectId}/regions/{region}/instances/{instanceId}/destinations[/{destinationId}[/health]]
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"telemetry-router/internal/store"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	store *store.Store
	// In production this would also hold a k8s client to trigger operator
	// reconcile loops after mutations.
}

func NewHandler(s *store.Store) *Handler {
	return &Handler{store: s}
}

// -----------------------------------------------------------------------------
// POST /destinations  →  201 Created
// -----------------------------------------------------------------------------

func (h *Handler) CreateDestination(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	instanceID := vars["instanceId"]
	region := Region(vars["region"])

	var req CreateDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// --- Sovereign cloud compliance check ---
	// S3 destinations targeting non-EU regions require explicit customer opt-in.
	if req.Type == DestinationTypeS3 && req.S3 != nil {
		if !isEURegion(req.S3.Region) && !req.S3.AllowNonEU {
			writeError(w, http.StatusUnprocessableEntity,
				"destination region is outside the EU; set allowNonEU=true to explicitly opt in")
			return
		}
	}

	// --- Basic validation ---
	switch req.Type {
	case DestinationTypeOTLP:
		if req.OTLP == nil || req.OTLP.Endpoint == "" {
			writeError(w, http.StatusBadRequest, "otlp.endpoint is required for type OTLP")
			return
		}
	case DestinationTypeS3:
		if req.S3 == nil || req.S3.Bucket == "" {
			writeError(w, http.StatusBadRequest, "s3.bucket is required for type S3")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported destination type: %s", req.Type))
		return
	}
	_ = region // would be used to route to the correct regional control plane

	destID := uuid.NewString()
	now := time.Now().UTC()

	rec := &store.DestinationRecord{
		DestinationID: destID,
		InstanceID:    instanceID,
		Name:          req.Name,
		Type:          string(req.Type),
		// Starts as PENDING; the operator reconcile loop sets it to ACTIVE
		// once it has successfully applied the Vector config.
		Status:    string(StatusPending),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := h.store.CreateDestination(rec); err != nil {
		log.Printf("store.CreateDestination: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to persist destination")
		return
	}

	// Publish event to kick off operator reconcile (stubbed — in production
	// this would write to a message bus or patch the CR).
	go h.triggerReconcile(instanceID, destID, "CREATE")

	dest := recordToDestination(rec, req.OTLP, req.S3)
	writeJSON(w, http.StatusCreated, dest)
}

// -----------------------------------------------------------------------------
// GET /destinations  →  200 OK
// -----------------------------------------------------------------------------

func (h *Handler) ListDestinations(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	instanceID := vars["instanceId"]

	recs, err := h.store.ListDestinations(instanceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list destinations")
		return
	}

	out := make([]*Destination, 0, len(recs))
	for _, rec := range recs {
		out = append(out, recordToDestination(rec, nil, nil))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

// -----------------------------------------------------------------------------
// GET /destinations/{destinationId}  →  200 OK
// -----------------------------------------------------------------------------

func (h *Handler) GetDestination(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	destID := vars["destinationId"]

	rec, err := h.store.GetDestination(destID)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("destination %s not found", destID))
		return
	}
	writeJSON(w, http.StatusOK, recordToDestination(rec, nil, nil))
}

// -----------------------------------------------------------------------------
// PUT /destinations/{destinationId}  →  202 Accepted
//
// We return 202 because the update triggers an async operator reconcile loop
// to regenerate and apply the Vector config. The caller should poll the task
// href or destination status to know when it's active.
// -----------------------------------------------------------------------------

func (h *Handler) UpdateDestination(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	instanceID := vars["instanceId"]
	destID := vars["destinationId"]

	var req UpdateDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if _, err := h.store.GetDestination(destID); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("destination %s not found", destID))
		return
	}

	fields := map[string]interface{}{"name": req.Name, "status": string(StatusPending)}
	if err := h.store.UpdateDestination(destID, fields); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update destination")
		return
	}

	taskID := uuid.NewString()
	h.store.CreateTask(taskID)
	go h.triggerReconcile(instanceID, destID, "UPDATE")

	writeJSON(w, http.StatusAccepted, AsyncRef{
		TaskID: taskID,
		Status: "PENDING",
		HRef:   fmt.Sprintf("/v2/tasks/%s", taskID),
	})
}

// -----------------------------------------------------------------------------
// DELETE /destinations/{destinationId}  →  202 Accepted
//
// Same reasoning as PUT — deletion requires the operator to remove the sink
// from the running Vector config and drain in-flight events gracefully before
// the record is purged.
// -----------------------------------------------------------------------------

func (h *Handler) DeleteDestination(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	instanceID := vars["instanceId"]
	destID := vars["destinationId"]

	if _, err := h.store.GetDestination(destID); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("destination %s not found", destID))
		return
	}

	// Mark as deleting immediately so new events skip this sink.
	_ = h.store.UpdateDestination(destID, map[string]interface{}{"status": string(StatusDeleting)})

	taskID := uuid.NewString()
	h.store.CreateTask(taskID)

	go func() {
		h.triggerReconcile(instanceID, destID, "DELETE")
		// Only remove from store after operator confirms drain is complete.
		_ = h.store.DeleteDestination(destID)
		h.store.SetTaskStatus(taskID, "DONE")
	}()

	writeJSON(w, http.StatusAccepted, AsyncRef{
		TaskID: taskID,
		Status: "PENDING",
		HRef:   fmt.Sprintf("/v2/tasks/%s", taskID),
	})
}

// -----------------------------------------------------------------------------
// GET /destinations/{destinationId}/health  →  200 OK
// -----------------------------------------------------------------------------

func (h *Handler) GetDestinationHealth(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	destID := vars["destinationId"]

	if _, err := h.store.GetDestination(destID); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("destination %s not found", destID))
		return
	}

	successes, failures, lastErr, lastAt := h.store.HealthSummary(destID)

	health := HealthHealthy
	if failures > 0 && successes == 0 {
		health = HealthUnhealthy
	} else if failures > 0 {
		health = HealthDegraded
	}

	writeJSON(w, http.StatusOK, DestinationHealth{
		DestinationID:       destID,
		Health:              health,
		LastDeliveredAt:     lastAt,
		Last1HourSuccessful: successes,
		Last1HourFailed:     failures,
		LastError:           lastErr,
	})
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func (h *Handler) triggerReconcile(instanceID, destID, action string) {
	// In production: patch the TelemetryRouter CR or publish to an internal
	// event bus.  The k8s operator picks this up and runs reconcileOnce().
	log.Printf("[operator] reconcile triggered: instance=%s dest=%s action=%s",
		instanceID, destID, action)
	time.Sleep(50 * time.Millisecond) // simulate async dispatch latency
}

// recordToDestination converts a store record into the API response shape.
// otlp/s3 are passed in on create; on read they'd be deserialized from Config.
func recordToDestination(rec *store.DestinationRecord, otlp *OTLPConfig, s3 *S3Config) *Destination {
	return &Destination{
		DestinationID: rec.DestinationID,
		InstanceID:    rec.InstanceID,
		Name:          rec.Name,
		Type:          DestinationType(rec.Type),
		Status:        DestinationStatus(rec.Status),
		OTLP:          otlp,
		S3:            s3,
		CreatedAt:     rec.CreatedAt,
		UpdatedAt:     rec.UpdatedAt,
	}
}

// pathVars is a minimal path variable extractor for the standard library mux.
// In production use gorilla/mux or chi — this is intentionally simple.
func pathVars(r *http.Request) map[string]string {
	vars, ok := r.Context().Value(pathVarsKey{}).(map[string]string)
	if !ok {
		return map[string]string{}
	}
	return vars
}

type pathVarsKey struct{}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, APIError{Code: status, Message: msg})
}
