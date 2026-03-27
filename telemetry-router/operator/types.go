// Package operator contains the Kubernetes controller-runtime reconcile logic
// for the TelemetryRouter CRD. In production this is built with
// sigs.k8s.io/controller-runtime; here we replicate the structure using
// standard types so the pattern is clear without the full dependency tree.
package operator

import (
	"time"
)

// -----------------------------------------------------------------------------
// TelemetryRouter CRD schema
// -----------------------------------------------------------------------------

// TelemetryRouterSpec is the desired state declared by the customer.
// Maps to spec: in the CRD YAML.
type TelemetryRouterSpec struct {
	// InstanceID ties this CR to the control-plane record.
	InstanceID string `json:"instanceId"`

	// ProjectID is the STACKIT project that owns this instance.
	ProjectID string `json:"projectId"`

	// Region the instance runs in. Must be EU unless AllowNonEU is set.
	Region string `json:"region"`

	// Replicas is the desired number of Vector pods (horizontal scale-out).
	Replicas int32 `json:"replicas"`

	// Destinations is the ordered list of configured sinks.
	// The operator generates one Vector sink block per entry.
	Destinations []DestinationSpec `json:"destinations"`

	// AllowNonEU opts the customer into routing data outside the EU.
	// Must be explicitly set to true; defaults false.
	AllowNonEU bool `json:"allowNonEU,omitempty"`

	// Resources controls the CPU/memory requests and limits for Vector pods.
	Resources ResourceSpec `json:"resources,omitempty"`
}

// DestinationSpec is one sink inside the CR spec.
type DestinationSpec struct {
	// DestinationID matches the ID returned by the REST API.
	DestinationID string `json:"destinationId"`
	Name          string `json:"name"`
	Type          string `json:"type"` // "OTLP" | "S3"

	// OTLP-specific fields.
	OTLPEndpoint string            `json:"otlpEndpoint,omitempty"`
	OTLPHeaders  map[string]string `json:"otlpHeaders,omitempty"`

	// S3-specific fields.
	S3Endpoint   string `json:"s3Endpoint,omitempty"`
	S3Bucket     string `json:"s3Bucket,omitempty"`
	S3BucketRegion string `json:"s3BucketRegion,omitempty"`
	S3Prefix     string `json:"s3Prefix,omitempty"`

	// SecretRef is the name of the k8s Secret in the same namespace that holds
	// credentials (OTLP API key, S3 access/secret keys).
	// The operator resolves this at render time and injects env vars into the
	// Vector pod — credentials never appear in the CR spec or Vector TOML.
	SecretRef string `json:"secretRef"`
}

// TelemetryRouterStatus reflects the observed state written back by the operator.
type TelemetryRouterStatus struct {
	// Phase: "Pending" | "Reconciling" | "Ready" | "Degraded" | "Failed"
	Phase string `json:"phase"`

	// VectorConfigHash is the SHA-256 of the last successfully applied
	// Vector TOML. Used to detect drift without re-rendering.
	VectorConfigHash string `json:"vectorConfigHash,omitempty"`

	// ReadyReplicas is the number of Vector pods currently Ready.
	ReadyReplicas int32 `json:"readyReplicas"`

	// Conditions follow the standard k8s condition pattern.
	Conditions []Condition `json:"conditions,omitempty"`

	LastReconcileAt time.Time `json:"lastReconcileAt,omitempty"`
}

// Condition mirrors the standard metav1.Condition shape.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"` // "True" | "False" | "Unknown"
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// ResourceSpec mirrors k8s ResourceRequirements.
type ResourceSpec struct {
	RequestsCPU    string `json:"requestsCpu,omitempty"`
	RequestsMemory string `json:"requestsMemory,omitempty"`
	LimitsCPU      string `json:"limitsCpu,omitempty"`
	LimitsMemory   string `json:"limitsMemory,omitempty"`
}

// TelemetryRouter is the full CRD object.
type TelemetryRouter struct {
	// Standard k8s metadata (name, namespace, labels, annotations, finalizers).
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels,omitempty"`

	Spec   TelemetryRouterSpec   `json:"spec"`
	Status TelemetryRouterStatus `json:"status"`
}
