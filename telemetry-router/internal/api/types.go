package api

import "time"

// DestinationType enumerates the supported sink kinds.
type DestinationType string

const (
	DestinationTypeOTLP DestinationType = "OTLP"
	DestinationTypeS3   DestinationType = "S3"
)

// Region represents an EU-compliant cloud region.
type Region string

const (
	RegionEUWest1   Region = "eu-west-1"
	RegionEUCentral Region = "eu-central-1"
	RegionEUNorth1  Region = "eu-north-1"
)

// isEURegion returns true if the region is EU-sovereign.
func isEURegion(r Region) bool {
	switch r {
	case RegionEUWest1, RegionEUCentral, RegionEUNorth1:
		return true
	}
	return false
}

// DestinationStatus represents lifecycle state.
type DestinationStatus string

const (
	StatusPending  DestinationStatus = "PENDING"
	StatusActive   DestinationStatus = "ACTIVE"
	StatusFailed   DestinationStatus = "FAILED"
	StatusDeleting DestinationStatus = "DELETING"
)

// HealthStatus represents realtime delivery health.
type HealthStatus string

const (
	HealthHealthy   HealthStatus = "HEALTHY"
	HealthDegraded  HealthStatus = "DEGRADED"
	HealthUnhealthy HealthStatus = "UNHEALTHY"
)

// -----------------------------------------------------------------------------
// Request / Response types
// -----------------------------------------------------------------------------

// OTLPConfig holds config for an OTLP destination.
type OTLPConfig struct {
	Endpoint       string            `json:"endpoint"`                 // e.g. "https://ingest.mysiem.com:4317"
	Headers        map[string]string `json:"headers,omitempty"`        // e.g. Authorization header
	TLSSkipVerify  bool              `json:"tlsSkipVerify,omitempty"`  // not recommended
	SecretRef      string            `json:"secretRef,omitempty"`      // k8s Secret name holding API key
}

// S3Config holds config for an S3-compatible destination.
type S3Config struct {
	Endpoint        string `json:"endpoint"`                  // e.g. "https://s3.eu-central-1.stackit.cloud"
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`          // optional key prefix
	Region          Region `json:"region"`
	SecretRef       string `json:"secretRef"`                 // k8s Secret with access/secret keys
	AllowNonEU      bool   `json:"allowNonEU,omitempty"`      // explicit customer opt-in
}

// CreateDestinationRequest is the body for POST /destinations.
type CreateDestinationRequest struct {
	Name        string          `json:"name"`
	Type        DestinationType `json:"type"`
	OTLP        *OTLPConfig     `json:"otlp,omitempty"`
	S3          *S3Config       `json:"s3,omitempty"`
}

// Destination is the canonical resource returned by the API.
type Destination struct {
	DestinationID string            `json:"destinationId"`
	InstanceID    string            `json:"instanceId"`
	Name          string            `json:"name"`
	Type          DestinationType   `json:"type"`
	Status        DestinationStatus `json:"status"`
	OTLP          *OTLPConfig       `json:"otlp,omitempty"`
	S3            *S3Config         `json:"s3,omitempty"`
	CreatedAt     time.Time         `json:"createdAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
}

// UpdateDestinationRequest allows partial update of a destination.
type UpdateDestinationRequest struct {
	Name string      `json:"name,omitempty"`
	OTLP *OTLPConfig `json:"otlp,omitempty"`
	S3   *S3Config   `json:"s3,omitempty"`
}

// DestinationHealth is returned by GET /destinations/{id}/health.
type DestinationHealth struct {
	DestinationID       string       `json:"destinationId"`
	Health              HealthStatus `json:"health"`
	LastDeliveredAt     *time.Time   `json:"lastDeliveredAt,omitempty"`
	Last1HourSuccessful int          `json:"last1HourSuccessful"`
	Last1HourFailed     int          `json:"last1HourFailed"`
	LastError           string       `json:"lastError,omitempty"`
}

// AsyncRef is returned for 202 Accepted responses.
type AsyncRef struct {
	TaskID  string `json:"taskId"`
	Status  string `json:"status"`
	HRef    string `json:"href"`
}

// APIError is the standard error envelope.
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
