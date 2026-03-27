// Package fanout implements the parallel multi-sink delivery engine.
// Each sink gets its own bounded Go channel and goroutine so that a slow or
// failing destination cannot backpressure or block any other sink.
package fanout

import "time"

// LogRecord represents a single OTLP log record received from a customer
// workload. In production this would be the OTLP protobuf type.
type LogRecord struct {
	EventID    string            `json:"eventId"`
	InstanceID string            `json:"instanceId"`
	Timestamp  time.Time         `json:"timestamp"`
	Body       string            `json:"body"`
	Attributes map[string]string `json:"attributes"`
	// Resource attributes (k8s.pod.name, k8s.namespace, etc.)
	Resource map[string]string `json:"resource"`
}

// SinkConfig carries the runtime config for one delivery destination.
type SinkConfig struct {
	DestinationID string
	Type          string // "OTLP" | "S3"
	Endpoint      string // for OTLP sinks
	Headers       map[string]string
	BucketName    string // for S3 sinks
	Region        string
	// Credentials are injected from k8s Secrets at operator render time.
	AccessKey string
	SecretKey string
}

// DeliveryResult captures the outcome of one fan-out attempt to one sink.
type DeliveryResult struct {
	DestinationID string
	EventID       string
	AttemptNumber int
	Success       bool
	Error         error
	DurationMs    int64
}

// FanoutResult is the aggregate outcome for a single LogRecord across all sinks.
type FanoutResult struct {
	EventID string
	Results []DeliveryResult
}
