// Package models defines the domain types shared across the signal-forge app.
package models

import "time"

// Widget is the core resource managed by the signal-forge REST API.
// It exists purely to give the app something meaningful to CRUD so that
// traces, metrics, and logs have realistic context for the demo.
type Widget struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Color       string    `json:"color"`
	Weight      float64   `json:"weight"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// CreateWidgetRequest is the request body for POST /api/v1/widgets.
type CreateWidgetRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Color       string  `json:"color"`
	Weight      float64 `json:"weight"`
}

// WidgetList is the response body for GET /api/v1/widgets.
type WidgetList struct {
	Items []Widget `json:"items"`
	Total int      `json:"total"`
}

// APIError is the standard error envelope returned by all endpoints.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SignalEvent is written to stdout as structured JSON by the app.
// Vector collects these from /var/log/pods and forwards to SigNoz.
type SignalEvent struct {
	Level      string    `json:"level"`
	Time       time.Time `json:"time"`
	Service    string    `json:"service"`
	TraceID    string    `json:"traceId,omitempty"`
	SpanID     string    `json:"spanId,omitempty"`
	Method     string    `json:"method,omitempty"`
	Path       string    `json:"path,omitempty"`
	StatusCode int       `json:"statusCode,omitempty"`
	LatencyMs  int64     `json:"latencyMs,omitempty"`
	Message    string    `json:"message"`
}
