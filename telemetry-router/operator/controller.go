package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"strings"
	"text/template"
	"time"
)

// -----------------------------------------------------------------------------
// Reconciler
// -----------------------------------------------------------------------------

// Reconciler implements the controller-runtime Reconciler interface pattern.
// In production it would embed a controller-runtime client and scheme; here we
// stub the k8s calls with clear comments so the logic is transparent.
type Reconciler struct {
	// k8sClient would be a sigs.k8s.io/controller-runtime/pkg/client.Client
	// in production. We stub it with a simple interface below.
	k8sClient K8sClient
}

// NewReconciler creates a Reconciler with the given k8s client stub.
func NewReconciler(client K8sClient) *Reconciler {
	return &Reconciler{k8sClient: client}
}

// Reconcile is the core function called by controller-runtime whenever a
// TelemetryRouter CR is created, updated, or deleted — or when any resource
// owned by the CR changes (e.g. a Vector pod becomes unhealthy).
//
// The reconcile loop is idempotent: calling it multiple times with the same
// desired state always converges to the same outcome.
func (r *Reconciler) Reconcile(ctx context.Context, tr *TelemetryRouter) error {
	log.Printf("[operator] reconcile start: %s/%s", tr.Namespace, tr.Name)

	// -------------------------------------------------------------------------
	// 1. Validate region compliance before touching any cluster resources.
	//    This is the second enforcement layer (the REST API is the first).
	// -------------------------------------------------------------------------
	if err := r.enforceRegionPolicy(tr); err != nil {
		log.Printf("[operator] region policy violation: %v", err)
		r.setCondition(tr, "RegionCompliant", "False", "PolicyViolation", err.Error())
		return r.patchStatus(ctx, tr)
	}
	r.setCondition(tr, "RegionCompliant", "True", "Compliant", "all destinations within allowed regions")

	// -------------------------------------------------------------------------
	// 2. Resolve k8s Secrets for each destination's credentials.
	//    Credentials are injected as env vars into the Vector pod — they never
	//    appear in the Vector TOML config on disk.
	// -------------------------------------------------------------------------
	credEnvVars, err := r.resolveSecrets(ctx, tr)
	if err != nil {
		log.Printf("[operator] secret resolution failed: %v", err)
		r.setCondition(tr, "SecretsResolved", "False", "SecretError", err.Error())
		return r.patchStatus(ctx, tr)
	}
	r.setCondition(tr, "SecretsResolved", "True", "Resolved", "all secrets resolved")

	// -------------------------------------------------------------------------
	// 3. Render the Vector TOML config from the CRD spec.
	// -------------------------------------------------------------------------
	vectorConfig, err := renderVectorConfig(tr)
	if err != nil {
		log.Printf("[operator] config render failed: %v", err)
		r.setCondition(tr, "ConfigRendered", "False", "RenderError", err.Error())
		return r.patchStatus(ctx, tr)
	}

	configHash := fmt.Sprintf("%x", sha256.Sum256([]byte(vectorConfig)))

	// Skip apply if config hasn't changed (avoid unnecessary pod restarts).
	if tr.Status.VectorConfigHash == configHash {
		log.Printf("[operator] config unchanged (hash=%s), skipping apply", configHash[:8])
		return nil
	}

	// -------------------------------------------------------------------------
	// 4. Apply the Vector config as a ConfigMap, then update the Deployment.
	//    controller-runtime's SetControllerReference ensures garbage collection
	//    when the TelemetryRouter CR is deleted.
	// -------------------------------------------------------------------------
	if err := r.k8sClient.ApplyConfigMap(ctx, tr.Namespace, tr.Name+"-vector-config", vectorConfig); err != nil {
		log.Printf("[operator] configmap apply failed: %v", err)
		r.setCondition(tr, "ConfigApplied", "False", "ApplyError", err.Error())
		return r.patchStatus(ctx, tr)
	}

	if err := r.k8sClient.ApplyDeployment(ctx, tr.Namespace, tr.Name, tr.Spec.Replicas, credEnvVars); err != nil {
		log.Printf("[operator] deployment apply failed: %v", err)
		r.setCondition(tr, "DeploymentReady", "False", "DeployError", err.Error())
		return r.patchStatus(ctx, tr)
	}

	// -------------------------------------------------------------------------
	// 5. Update status to reflect the successfully applied config.
	// -------------------------------------------------------------------------
	tr.Status.Phase = "Ready"
	tr.Status.VectorConfigHash = configHash
	tr.Status.LastReconcileAt = time.Now().UTC()
	r.setCondition(tr, "ConfigApplied", "True", "Applied", "Vector config applied successfully")
	r.setCondition(tr, "DeploymentReady", "True", "Ready", fmt.Sprintf("%d replica(s) ready", tr.Spec.Replicas))

	log.Printf("[operator] reconcile complete: hash=%s", configHash[:8])
	return r.patchStatus(ctx, tr)
}

// -----------------------------------------------------------------------------
// Region policy enforcement
// -----------------------------------------------------------------------------

var euRegions = map[string]bool{
	"eu-west-1": true, "eu-central-1": true, "eu-north-1": true,
	"de-txl-1": true, "de-fra-1": true, "at-vie-1": true,
}

func (r *Reconciler) enforceRegionPolicy(tr *TelemetryRouter) error {
	if tr.Spec.AllowNonEU {
		// Customer has explicitly opted in; log for audit trail.
		log.Printf("[operator] WARN: instance %s has AllowNonEU=true — non-EU routing permitted",
			tr.Spec.InstanceID)
		return nil
	}
	for _, dest := range tr.Spec.Destinations {
		if dest.Type == "S3" && dest.S3BucketRegion != "" {
			if !euRegions[strings.ToLower(dest.S3BucketRegion)] {
				return fmt.Errorf(
					"destination %q targets region %q which is outside the EU; "+
						"set spec.allowNonEU=true to opt in",
					dest.Name, dest.S3BucketRegion)
			}
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Secret resolution
// -----------------------------------------------------------------------------

// EnvVar is a name=value pair injected into the Vector pod.
type EnvVar struct {
	Name  string
	Value string
}

// resolveSecrets fetches each destination's k8s Secret and converts it into
// env vars to be injected into the Vector pod via envFrom/env in the
// Deployment spec. This keeps credentials out of the ConfigMap (and thus out
// of etcd in plaintext).
func (r *Reconciler) resolveSecrets(ctx context.Context, tr *TelemetryRouter) ([]EnvVar, error) {
	var envVars []EnvVar
	for _, dest := range tr.Spec.Destinations {
		if dest.SecretRef == "" {
			continue
		}
		secret, err := r.k8sClient.GetSecret(ctx, tr.Namespace, dest.SecretRef)
		if err != nil {
			return nil, fmt.Errorf("secret %q for destination %q: %w", dest.SecretRef, dest.Name, err)
		}
		// Convention: prefix env var names with the destination ID (sanitised)
		// so multiple destinations can have secrets without key collisions.
		prefix := strings.ToUpper(strings.ReplaceAll(dest.DestinationID, "-", "_"))
		for k, v := range secret {
			envVars = append(envVars, EnvVar{
				Name:  fmt.Sprintf("DEST_%s_%s", prefix, strings.ToUpper(k)),
				Value: v,
			})
		}
	}
	return envVars, nil
}

// -----------------------------------------------------------------------------
// Vector config rendering
// -----------------------------------------------------------------------------

// vectorConfigTemplate is the Go text/template for generating the Vector TOML.
// In production this would reference the secret env vars via ${ENV_VAR} syntax
// rather than hardcoding values.
const vectorConfigTemplate = `
# Auto-generated by TelemetryRouter operator — DO NOT EDIT MANUALLY
# Instance: {{ .Spec.InstanceID }}
# Config hash is stored in status.vectorConfigHash for drift detection.

[api]
  enabled = true
  address = "0.0.0.0:8686"

##############################################################################
# SOURCE: accept OTLP over gRPC (standard port 4317) and HTTP (4318)
##############################################################################
[sources.otlp_in]
  type = "opentelemetry"
  grpc.address = "0.0.0.0:4317"
  http.address = "0.0.0.0:4318"

##############################################################################
# TRANSFORM: redact PII fields from log bodies before fan-out
##############################################################################
[transforms.pii_redact]
  type = "remap"
  inputs = ["otlp_in"]
  source = '''
    # Redact email addresses
    .message = replace(.message, r'\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b', "[REDACTED_EMAIL]")
    # Redact IPv4 addresses
    .message = replace(.message, r'\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b', "[REDACTED_IP]")
    # Tag with instance metadata for downstream routing
    .stackit.instance_id = "{{ .Spec.InstanceID }}"
    .stackit.project_id  = "{{ .Spec.ProjectID }}"
  '''
{{ range .Spec.Destinations }}
##############################################################################
# SINK: {{ .Name }} ({{ .Type }})
##############################################################################
{{ if eq .Type "OTLP" -}}
[sinks.{{ sanitize .DestinationID }}]
  type    = "opentelemetry"
  inputs  = ["pii_redact"]
  endpoint = "{{ .OTLPEndpoint }}"
  encoding.codec = "json"

  [sinks.{{ sanitize .DestinationID }}.buffer]
    type       = "memory"
    max_events = 10000
    when_full  = "drop_newest"   # isolates this sink; never blocks other sinks

  [sinks.{{ sanitize .DestinationID }}.request]
    retry_attempts      = 5
    retry_initial_backoff_secs = 1
    retry_max_duration_secs    = 16

  # Credentials injected via env var — never hardcoded in this file.
  {{ if .SecretRef -}}
  [sinks.{{ sanitize .DestinationID }}.auth]
    strategy = "bearer"
    token    = "${DEST_{{ upper .DestinationID }}_API_KEY}"
  {{- end }}
{{ else if eq .Type "S3" -}}
[sinks.{{ sanitize .DestinationID }}]
  type   = "aws_s3"
  inputs = ["pii_redact"]
  bucket = "{{ .S3Bucket }}"
  key_prefix = "{{ .S3Prefix }}%Y/%m/%d/"
  region = "{{ .S3BucketRegion }}"
  endpoint = "{{ .S3Endpoint }}"
  compression = "gzip"
  encoding.codec = "json"

  [sinks.{{ sanitize .DestinationID }}.buffer]
    type       = "memory"
    max_events = 10000
    when_full  = "drop_newest"

  # Credentials from env vars resolved from k8s Secret "{{ .SecretRef }}"
  # Vector picks these up automatically via the standard AWS env var names.
  # DEST_{{ upper .DestinationID }}_ACCESS_KEY_ID     → AWS_ACCESS_KEY_ID (remapped in pod spec)
  # DEST_{{ upper .DestinationID }}_SECRET_ACCESS_KEY → AWS_SECRET_ACCESS_KEY
{{ end -}}
{{ end }}
`

var tmplFuncs = template.FuncMap{
	"sanitize": func(s string) string {
		return strings.ReplaceAll(s, "-", "_")
	},
	"upper": func(s string) string {
		return strings.ToUpper(strings.ReplaceAll(s, "-", "_"))
	},
}

func renderVectorConfig(tr *TelemetryRouter) (string, error) {
	tmpl, err := template.New("vector").Funcs(tmplFuncs).Parse(vectorConfigTemplate)
	if err != nil {
		return "", fmt.Errorf("template parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, tr); err != nil {
		return "", fmt.Errorf("template execute: %w", err)
	}
	return buf.String(), nil
}

// -----------------------------------------------------------------------------
// Status helpers
// -----------------------------------------------------------------------------

func (r *Reconciler) setCondition(tr *TelemetryRouter, condType, status, reason, msg string) {
	now := time.Now().UTC()
	for i, c := range tr.Status.Conditions {
		if c.Type == condType {
			tr.Status.Conditions[i] = Condition{
				Type:               condType,
				Status:             status,
				Reason:             reason,
				Message:            msg,
				LastTransitionTime: now,
			}
			return
		}
	}
	tr.Status.Conditions = append(tr.Status.Conditions, Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	})
}

func (r *Reconciler) patchStatus(ctx context.Context, tr *TelemetryRouter) error {
	// In production: r.k8sClient.Status().Patch(ctx, tr, patch)
	return r.k8sClient.PatchStatus(ctx, tr)
}

// -----------------------------------------------------------------------------
// K8sClient interface (stubbed for testing / this demo)
// -----------------------------------------------------------------------------

// K8sClient abstracts the controller-runtime client calls the operator needs.
// This makes the reconciler unit-testable without a real cluster.
type K8sClient interface {
	GetSecret(ctx context.Context, namespace, name string) (map[string]string, error)
	ApplyConfigMap(ctx context.Context, namespace, name, data string) error
	ApplyDeployment(ctx context.Context, namespace, name string, replicas int32, envVars []EnvVar) error
	PatchStatus(ctx context.Context, tr *TelemetryRouter) error
}

// StubK8sClient is a no-op implementation for unit tests and local dev.
type StubK8sClient struct {
	Secrets map[string]map[string]string // namespace/name → key → value
}

func (s *StubK8sClient) GetSecret(ctx context.Context, namespace, name string) (map[string]string, error) {
	key := namespace + "/" + name
	secret, ok := s.Secrets[key]
	if !ok {
		return nil, fmt.Errorf("secret %s not found", key)
	}
	return secret, nil
}

func (s *StubK8sClient) ApplyConfigMap(_ context.Context, namespace, name, data string) error {
	log.Printf("[stub-k8s] ConfigMap %s/%s applied (%d bytes)", namespace, name, len(data))
	return nil
}

func (s *StubK8sClient) ApplyDeployment(_ context.Context, namespace, name string, replicas int32, env []EnvVar) error {
	log.Printf("[stub-k8s] Deployment %s/%s applied (replicas=%d, env_vars=%d)",
		namespace, name, replicas, len(env))
	return nil
}

func (s *StubK8sClient) PatchStatus(_ context.Context, tr *TelemetryRouter) error {
	log.Printf("[stub-k8s] Status patched: phase=%s hash=%s", tr.Status.Phase, tr.Status.VectorConfigHash)
	return nil
}
