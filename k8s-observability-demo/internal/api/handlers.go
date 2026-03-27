// Package api implements the HTTP handlers for the signal-forge REST API.
//
// Every handler:
//  1. Starts or continues an OTel span (via otelhttp middleware on the mux).
//  2. Adds span attributes relevant to the business operation.
//  3. Emits a structured JSON log line (zerolog) that includes the trace+span IDs
//     so that logs and traces can be correlated in SigNoz.
//  4. Increments the request counter and records latency via OTel metrics.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"signal-forge/internal/models"
	"signal-forge/internal/telemetry"
)

var tracer = otel.Tracer("signal-forge/api")

// Handler holds the API's dependencies.
type Handler struct {
	store *WidgetStore
	sdk   *telemetry.SDK
}

// New creates a Handler wired to the given store and telemetry SDK.
func New(store *WidgetStore, sdk *telemetry.SDK) *Handler {
	return &Handler{store: store, sdk: sdk}
}

// RegisterRoutes attaches all API routes to the provided mux.
// The otelhttp middleware on the mux handles root span creation;
// individual handlers add child spans for interesting sub-operations.
func RegisterRoutes(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("POST /api/v1/widgets", h.CreateWidget)
	mux.HandleFunc("GET /api/v1/widgets", h.ListWidgets)
	mux.HandleFunc("GET /api/v1/widgets/{widgetId}", h.GetWidget)
	mux.HandleFunc("DELETE /api/v1/widgets/{widgetId}", h.DeleteWidget)

	// Simulate a slow/expensive operation — useful for demonstrating
	// tail sampling keeping only the slow requests in SigNoz.
	mux.HandleFunc("GET /api/v1/simulate/slow", h.SimulateSlow)
	mux.HandleFunc("GET /api/v1/simulate/error", h.SimulateError)
}

// ------------------------------------------------------------------
// CreateWidget — POST /api/v1/widgets
// ------------------------------------------------------------------

func (h *Handler) CreateWidget(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx, span := tracer.Start(r.Context(), "CreateWidget",
		trace.WithSpanKind(trace.SpanKindServer),
	)
	defer span.End()

	h.sdk.ActiveRequests.Add(ctx, 1)
	defer h.sdk.ActiveRequests.Add(ctx, -1)

	var req models.CreateWidgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "invalid request body")
		h.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	if req.Name == "" {
		span.SetStatus(codes.Error, "missing name")
		h.writeError(w, http.StatusBadRequest, "MISSING_NAME", "name is required")
		return
	}

	span.SetAttributes(
		attribute.String("widget.name", req.Name),
		attribute.String("widget.color", req.Color),
		attribute.Float64("widget.weight", req.Weight),
	)

	widget, err := h.store.Create(req)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			span.SetStatus(codes.Error, "conflict")
			h.writeError(w, http.StatusConflict, "CONFLICT", "widget name already exists")
		} else {
			span.RecordError(err)
			span.SetStatus(codes.Error, "store error")
			h.writeError(w, http.StatusInternalServerError, "INTERNAL", "store error")
		}
		return
	}

	span.SetAttributes(attribute.String("widget.id", widget.ID))
	span.SetStatus(codes.Ok, "")

	latency := time.Since(start).Seconds()
	h.sdk.RequestCounter.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("method", "POST"),
			attribute.String("path", "/api/v1/widgets"),
			attribute.Int("status_code", http.StatusCreated),
		),
	)
	h.sdk.RequestDuration.Record(ctx, latency,
		metric.WithAttributes(
			attribute.String("method", "POST"),
			attribute.String("path", "/api/v1/widgets"),
		),
	)

	log.Ctx(ctx).Info().
		Str("widget_id", widget.ID).
		Str("widget_name", widget.Name).
		Str("trace_id", span.SpanContext().TraceID().String()).
		Str("span_id", span.SpanContext().SpanID().String()).
		Float64("latency_ms", latency*1000).
		Msg("widget created")

	h.writeJSON(w, http.StatusCreated, widget)
}

// ------------------------------------------------------------------
// ListWidgets — GET /api/v1/widgets?color=<optional>
// ------------------------------------------------------------------

func (h *Handler) ListWidgets(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx, span := tracer.Start(r.Context(), "ListWidgets")
	defer span.End()

	color := r.URL.Query().Get("color")
	span.SetAttributes(attribute.String("filter.color", color))

	widgets, err := h.store.List(color)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "store error")
		h.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	span.SetAttributes(attribute.Int("result.count", len(widgets)))
	span.SetStatus(codes.Ok, "")

	h.sdk.RequestCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("method", "GET"),
		attribute.String("path", "/api/v1/widgets"),
		attribute.Int("status_code", http.StatusOK),
	))
	h.sdk.RequestDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
		attribute.String("method", "GET"),
		attribute.String("path", "/api/v1/widgets"),
	))

	log.Ctx(ctx).Info().
		Int("count", len(widgets)).
		Str("trace_id", span.SpanContext().TraceID().String()).
		Msg("widgets listed")

	h.writeJSON(w, http.StatusOK, models.WidgetList{Items: widgets, Total: len(widgets)})
}

// ------------------------------------------------------------------
// GetWidget — GET /api/v1/widgets/{widgetId}
// ------------------------------------------------------------------

func (h *Handler) GetWidget(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "GetWidget")
	defer span.End()

	id := r.PathValue("widgetId")
	span.SetAttributes(attribute.String("widget.id", id))

	widget, err := h.store.Get(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			span.SetStatus(codes.Error, "not found")
			h.writeError(w, http.StatusNotFound, "NOT_FOUND", "widget not found")
		} else {
			span.RecordError(err)
			span.SetStatus(codes.Error, "store error")
			h.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	span.SetAttributes(attribute.String("widget.name", widget.Name))
	span.SetStatus(codes.Ok, "")

	log.Ctx(ctx).Info().
		Str("widget_id", id).
		Str("trace_id", span.SpanContext().TraceID().String()).
		Msg("widget fetched")

	h.writeJSON(w, http.StatusOK, widget)
}

// ------------------------------------------------------------------
// DeleteWidget — DELETE /api/v1/widgets/{widgetId}
// ------------------------------------------------------------------

func (h *Handler) DeleteWidget(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "DeleteWidget")
	defer span.End()

	id := r.PathValue("widgetId")
	span.SetAttributes(attribute.String("widget.id", id))

	if err := h.store.Delete(id); err != nil {
		if errors.Is(err, ErrNotFound) {
			span.SetStatus(codes.Error, "not found")
			h.writeError(w, http.StatusNotFound, "NOT_FOUND", "widget not found")
		} else {
			span.RecordError(err)
			span.SetStatus(codes.Error, "store error")
			h.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	span.SetStatus(codes.Ok, "")

	h.sdk.RequestCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("method", "DELETE"),
		attribute.String("path", "/api/v1/widgets/{id}"),
		attribute.Int("status_code", http.StatusNoContent),
	))

	log.Ctx(ctx).Info().
		Str("widget_id", id).
		Str("trace_id", span.SpanContext().TraceID().String()).
		Msg("widget deleted")

	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------
// SimulateSlow — GET /api/v1/simulate/slow
// Adds artificial 2s delay. Great for demonstrating tail sampling:
// configure the Collector to always keep traces > 1s latency.
// ------------------------------------------------------------------

func (h *Handler) SimulateSlow(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "SimulateSlow")
	defer span.End()

	span.SetAttributes(attribute.Bool("simulated", true))

	// Child span representing the "slow DB query"
	_, dbSpan := tracer.Start(ctx, "slowDBQuery",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.statement", "SELECT * FROM widgets ORDER BY created_at DESC"),
		),
	)
	time.Sleep(2 * time.Second)
	dbSpan.SetStatus(codes.Ok, "")
	dbSpan.End()

	span.SetStatus(codes.Ok, "")

	log.Ctx(ctx).Warn().
		Str("trace_id", span.SpanContext().TraceID().String()).
		Int("sleep_ms", 2000).
		Msg("slow request simulated — check tail sampling in SigNoz")

	h.writeJSON(w, http.StatusOK, map[string]string{
		"message": "slow request completed — check SigNoz for this trace",
		"traceId": span.SpanContext().TraceID().String(),
	})
}

// ------------------------------------------------------------------
// SimulateError — GET /api/v1/simulate/error
// Records a span with error status + exception event.
// Good for verifying SigNoz error tracking + alerting rules.
// ------------------------------------------------------------------

func (h *Handler) SimulateError(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "SimulateError")
	defer span.End()

	simulatedErr := errors.New("simulated downstream service unavailable")
	span.RecordError(simulatedErr,
		trace.WithAttributes(
			attribute.String("error.source", "downstream"),
			attribute.String("downstream.service", "widget-cache"),
		),
	)
	span.SetStatus(codes.Error, simulatedErr.Error())

	h.sdk.RequestCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("method", "GET"),
		attribute.String("path", "/api/v1/simulate/error"),
		attribute.Int("status_code", http.StatusServiceUnavailable),
	))

	log.Ctx(ctx).Error().
		Err(simulatedErr).
		Str("trace_id", span.SpanContext().TraceID().String()).
		Str("span_id", span.SpanContext().SpanID().String()).
		Msg("simulated error — check SigNoz error tracking")

	h.writeError(w, http.StatusServiceUnavailable, "SIMULATED_ERROR", simulatedErr.Error())
}

// ------------------------------------------------------------------
// Helpers
// ------------------------------------------------------------------

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code, msg string) {
	h.writeJSON(w, status, models.APIError{Code: code, Message: msg})
}
