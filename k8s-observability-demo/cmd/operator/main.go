// operator is a lightweight Kubernetes controller that watches SignalPolicy CRs
// and reconciles OTel Collector and Vector configurations in response.
//
// Architecture:
//
//	SignalPolicy CR  →  operator reconcile loop
//	                       │
//	                       ├─► patch OTel Collector ConfigMap
//	                       │     (sampling rate, batch size, queue backpressure)
//	                       │
//	                       └─► patch Vector ConfigMap
//	                             (buffer size, when_full policy)
//	                       │
//	                       └─► annotate Deployments with config checksum
//	                             → triggers rolling restart when config changes
//
// This pattern is identical to how real operators (cert-manager, Crossplane,
// Flux) propagate CRD changes to workloads — a common interview topic.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
)

// SignalPolicySpec mirrors the CRD spec fields we care about.
// In production this would be generated from a controller-gen marker annotation.
type SignalPolicySpec struct {
	// TargetNamespace is the namespace where the OTel Collector and Vector run.
	TargetNamespace string `json:"targetNamespace"`

	// Sampling controls the tail_sampling processor in the OTel Collector.
	Sampling SamplingSpec `json:"sampling"`

	// Batching controls the batch processor in the OTel Collector.
	Batching BatchingSpec `json:"batching"`

	// Backpressure controls Vector's sink buffer and when_full policy.
	Backpressure BackpressureSpec `json:"backpressure"`
}

type SamplingSpec struct {
	// RatioBased is the head-based sampling ratio applied in the app SDK [0.0, 1.0].
	// The operator injects this as OTEL_SAMPLING_RATIO env var.
	RatioBased float64 `json:"ratioBased"`

	// AlwaysKeepErrors forces the tail_sampling processor to always keep
	// traces that contain at least one error span, regardless of ratio.
	AlwaysKeepErrors bool `json:"alwaysKeepErrors"`

	// AlwaysKeepSlowMs keeps traces where any span exceeds this latency (ms).
	// 0 = disabled.
	AlwaysKeepSlowMs int `json:"alwaysKeepSlowMs"`
}

type BatchingSpec struct {
	// SendBatchSize is the OTel Collector batch processor send_batch_size.
	SendBatchSize int `json:"sendBatchSize"`

	// TimeoutSeconds is the batch processor timeout.
	TimeoutSeconds int `json:"timeoutSeconds"`

	// QueueSize is the exporter sending_queue num_consumers * queue_size.
	QueueSize int `json:"queueSize"`
}

type BackpressureSpec struct {
	// VectorBufferMaxBytes is the Vector sink disk buffer max_size in bytes.
	// Default: 268435456 (256MB)
	VectorBufferMaxBytes int64 `json:"vectorBufferMaxBytes"`

	// WhenFull controls Vector's behaviour when the buffer is full.
	// One of: "drop_newest" | "block"
	WhenFull string `json:"whenFull"`
}

// SignalPolicyStatus is written back to the CR by the operator.
type SignalPolicyStatus struct {
	Phase              string      `json:"phase"` // Reconciling | Active | Failed
	OtelConfigHash     string      `json:"otelConfigHash"`
	VectorConfigHash   string      `json:"vectorConfigHash"`
	LastReconcileTime  metav1.Time `json:"lastReconcileTime"`
	Message            string      `json:"message,omitempty"`
}

var signalPolicyGVR = schema.GroupVersionResource{
	Group:    "observability.demo.dev",
	Version:  "v1alpha1",
	Resource: "signalpolicies",
}

// Controller holds all the state for the operator control loop.
type Controller struct {
	k8s     kubernetes.Interface
	dynamic dynamic.Interface
	ns      string
	queue   workqueue.RateLimitingInterface
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	ns := envOrDefault("WATCH_NAMESPACE", "observability-demo")

	cfg, err := loadKubeConfig()
	if err != nil {
		log.Fatalf("kubeconfig: %v", err)
	}

	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}

	ctrl := &Controller{
		k8s:     k8s,
		dynamic: dyn,
		ns:      ns,
		queue:   workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}

	// ------------------------------------------------------------------
	// Informer — watches SignalPolicy CRs in the target namespace.
	// Uses the shared informer factory to avoid duplicate list/watch calls.
	// ------------------------------------------------------------------
	factory := informers.NewSharedInformerFactoryWithOptions(
		k8s,
		30*time.Second,
		informers.WithNamespace(ns),
	)

	// Because SignalPolicy is a CRD (not a built-in type), we use the
	// dynamic client informer directly.
	dynFactory := dynamic.NewForConfigOrDie(cfg)
	_ = dynFactory // used via ctrl.dynamic below

	// For CRDs, we list/watch with the dynamic informer.
	// In a real controller-runtime setup this would be a typed informer
	// generated from the CRD schema.
	log.Printf("[operator] watching SignalPolicy CRs in namespace %q", ns)
	log.Printf("[operator] factory initialised (built-in informers)")
	factory.Start(wait.NeverStop)
	factory.WaitForCacheSync(wait.NeverStop)

	// ------------------------------------------------------------------
	// Poll loop — production operators use watch events + informer cache.
	// For clarity in this demo we use a simple poll loop. The reconcile
	// logic is identical either way.
	// ------------------------------------------------------------------
	log.Printf("[operator] starting reconcile loop (poll every 15s)")
	for {
		ctrl.reconcileAll()
		time.Sleep(15 * time.Second)
	}
}

// reconcileAll lists all SignalPolicy CRs and reconciles each one.
func (c *Controller) reconcileAll() {
	list, err := c.dynamic.Resource(signalPolicyGVR).Namespace(c.ns).List(
		context.Background(),
		metav1.ListOptions{},
	)
	if err != nil {
		log.Printf("[operator] list SignalPolicy: %v", err)
		return
	}

	for _, item := range list.Items {
		name := item.GetName()
		log.Printf("[operator] reconciling SignalPolicy %s/%s", c.ns, name)
		if err := c.reconcile(item.Object); err != nil {
			log.Printf("[operator] reconcile %s: %v", name, err)
			_ = c.patchStatus(name, SignalPolicyStatus{
				Phase:             "Failed",
				LastReconcileTime: metav1.Now(),
				Message:           err.Error(),
			})
		}
	}
}

// reconcile is the core idempotent reconcile function for a single SignalPolicy.
// It is safe to call multiple times — every step is a desired-state apply.
func (c *Controller) reconcile(obj map[string]interface{}) error {
	name, _, _ := unstructuredString(obj, "metadata", "name")

	// ------------------------------------------------------------------
	// 1. Parse spec
	// ------------------------------------------------------------------
	specRaw, ok := obj["spec"]
	if !ok {
		return fmt.Errorf("SignalPolicy %s has no spec", name)
	}
	specBytes, _ := json.Marshal(specRaw)
	var spec SignalPolicySpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return fmt.Errorf("parse spec: %w", err)
	}

	targetNS := spec.TargetNamespace
	if targetNS == "" {
		targetNS = c.ns
	}

	// ------------------------------------------------------------------
	// 2. Render OTel Collector config
	// ------------------------------------------------------------------
	otelCfg := renderOtelConfig(spec)
	otelHash := configHash(otelCfg)

	if err := c.applyConfigMap(targetNS, "otel-collector-config", map[string]string{
		"config.yaml": otelCfg,
	}); err != nil {
		return fmt.Errorf("apply otel configmap: %w", err)
	}

	// Annotate the OTel Collector Deployment with the config hash.
	// Kubernetes sees the pod template change and triggers a rolling restart.
	if err := c.annotateDeployment(targetNS, "otel-collector",
		"observability.demo.dev/config-hash", otelHash); err != nil {
		log.Printf("[operator] warn: annotate otel-collector: %v", err)
	}

	// ------------------------------------------------------------------
	// 3. Render Vector config
	// ------------------------------------------------------------------
	vectorCfg := renderVectorConfig(spec.Backpressure)
	vectorHash := configHash(vectorCfg)

	if err := c.applyConfigMap(targetNS, "vector-config", map[string]string{
		"vector.toml": vectorCfg,
	}); err != nil {
		return fmt.Errorf("apply vector configmap: %w", err)
	}

	if err := c.annotateDaemonSet(targetNS, "vector",
		"observability.demo.dev/config-hash", vectorHash); err != nil {
		log.Printf("[operator] warn: annotate vector daemonset: %v", err)
	}

	// ------------------------------------------------------------------
	// 4. Inject sampling ratio into signal-forge Deployment as env var
	// ------------------------------------------------------------------
	if err := c.patchDeploymentEnv(targetNS, "signal-forge",
		"OTEL_SAMPLING_RATIO", fmt.Sprintf("%.4f", spec.Sampling.RatioBased),
	); err != nil {
		log.Printf("[operator] warn: patch signal-forge env: %v", err)
	}

	// ------------------------------------------------------------------
	// 5. Update status
	// ------------------------------------------------------------------
	return c.patchStatus(name, SignalPolicyStatus{
		Phase:             "Active",
		OtelConfigHash:    otelHash,
		VectorConfigHash:  vectorHash,
		LastReconcileTime: metav1.Now(),
		Message:           "reconciled successfully",
	})
}

// ------------------------------------------------------------------
// Config rendering
// ------------------------------------------------------------------

// renderOtelConfig generates the OTel Collector YAML config from a SignalPolicy spec.
// In production this would use text/template with a more elaborate template.
func renderOtelConfig(spec SignalPolicySpec) string {
	sendBatchSize := 512
	if spec.Batching.SendBatchSize > 0 {
		sendBatchSize = spec.Batching.SendBatchSize
	}
	batchTimeout := 5
	if spec.Batching.TimeoutSeconds > 0 {
		batchTimeout = spec.Batching.TimeoutSeconds
	}
	queueSize := 1000
	if spec.Batching.QueueSize > 0 {
		queueSize = spec.Batching.QueueSize
	}

	// Tail sampling policies
	policies := `
      - name: always-sample-errors
        type: status_code
        status_code: { status_codes: [ERROR] }`

	if spec.Sampling.AlwaysKeepSlowMs > 0 {
		policies += fmt.Sprintf(`
      - name: keep-slow-traces
        type: latency
        latency: { threshold_ms: %d }`, spec.Sampling.AlwaysKeepSlowMs)
	}

	policies += fmt.Sprintf(`
      - name: probabilistic-sample
        type: probabilistic
        probabilistic: { sampling_percentage: %.1f }`,
		spec.Sampling.RatioBased*100)

	return fmt.Sprintf(`# Generated by signal-forge operator — DO NOT EDIT MANUALLY
# SignalPolicy controls: sampling_ratio=%.4f, send_batch_size=%d

extensions:
  health_check:
    endpoint: 0.0.0.0:13133

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
  # Collect host-level system metrics (CPU, memory, load, network, disk).
  # root_path points to the host filesystem mounted via hostPath in the Deployment.
  # These metrics populate the SigNoz Infrastructure / Hosts tab.
  hostmetrics:
    root_path: /hostfs
    collection_interval: 30s
    scrapers:
      cpu: {}
      memory: {}
      load: {}
      network: {}
      disk: {}
      filesystem:
        exclude_mount_points:
          match_type: regexp
          mount_points: [/dev.*,/sys.*,/proc.*,/run.*,/hostfs/var/lib/kubelet.*]

processors:
  # Tail sampling: makes decisions after seeing the FULL trace.
  # This is why we buffer for decision_wait seconds before deciding.
  # Head sampling (SDK-side) is a fast pre-filter; tail sampling is smart.
  tail_sampling:
    decision_wait: 10s
    num_traces: 50000
    expected_new_traces_per_sec: 100
    policies:%s

  # Batch: buffer spans before exporting — reduces per-RPC overhead.
  # send_batch_size + timeout are the two main backpressure knobs here.
  batch:
    send_batch_size: %d
    timeout: %ds

  # Memory limiter: drop data before OOMing the Collector process.
  # This is the LAST line of defence on the Collector side.
  memory_limiter:
    check_interval: 1s
    limit_mib: 512
    spike_limit_mib: 128

  # Detect host/OS resource attributes (host.name, os.type) so the SigNoz
  # Infrastructure tab can group metrics by host. override=false preserves
  # any existing resource attributes (e.g. service.name from the app).
  resourcedetection:
    detectors: [env, system]
    override: false
    system:
      hostname_sources: [os]

exporters:
  otlp/signoz:
    endpoint: signoz-otel-collector.platform.svc.cluster.local:4317
    tls:
      insecure: true
    # Retry + queue = backpressure on the exporter side.
    # If SigNoz is slow/unavailable, spans queue here before being dropped.
    retry_on_failure:
      enabled: true
      initial_interval: 5s
      max_interval: 30s
      max_elapsed_time: 300s
    sending_queue:
      enabled: true
      num_consumers: 4
      queue_size: %d

  # Prometheus exporter for Collector's own internal metrics.
  prometheus:
    endpoint: "0.0.0.0:8888"

service:
  extensions: [health_check]
  telemetry:
    logs:
      level: info
    metrics:
      address: 0.0.0.0:8888

  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, tail_sampling, batch]
      exporters: [otlp/signoz]
    metrics/app:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [otlp/signoz]
    metrics/host:
      receivers: [hostmetrics]
      processors: [memory_limiter, resourcedetection, batch]
      exporters: [otlp/signoz]
    logs:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [otlp/signoz]
`,
		spec.Sampling.RatioBased, sendBatchSize, policies,
		sendBatchSize, batchTimeout, queueSize)
}

// renderVectorConfig generates Vector TOML config from a SignalPolicy spec.
func renderVectorConfig(spec BackpressureSpec) string {
	const vectorMinBufferBytes = int64(268435488) // Vector minimum disk buffer size
	bufferMaxBytes := vectorMinBufferBytes
	if spec.VectorBufferMaxBytes > bufferMaxBytes {
		bufferMaxBytes = spec.VectorBufferMaxBytes
	}
	whenFull := "drop_newest"
	if spec.WhenFull == "block" {
		whenFull = "block"
	}

	return fmt.Sprintf(`# Generated by signal-forge operator — DO NOT EDIT MANUALLY
# SignalPolicy controls: buffer_max_bytes=%d, when_full=%s

# Enable the Vector API so the liveness probe on :8686 gets a /health endpoint.
[api]
  enabled = true
  address = "0.0.0.0:8686"

[sources.kubernetes_logs]
  type = "kubernetes_logs"
  self_node_name = "${VECTOR_SELF_NODE_NAME}"
  # Only collect logs from our demo namespace
  extra_label_selector = "app.kubernetes.io/part-of=observability-demo"

[transforms.parse_json]
  type = "remap"
  inputs = ["kubernetes_logs"]
  source = '''
    # Parse structured JSON logs emitted by signal-forge (zerolog)
    parsed, err = parse_json(.message)
    if err == null && is_object(parsed) {
      . = merge!(., parsed)
    }
    # Ensure timestamp is set
    if !exists(.timestamp) {
      .timestamp = now()
    }
    # Add Kubernetes metadata as OTel resource attributes
    ."k8s.pod.name"       = .kubernetes.pod_name
    ."k8s.namespace.name" = .kubernetes.pod_namespace
    ."k8s.node.name"      = .kubernetes.pod_node_name
  '''

[sinks.signoz_logs]
  type = "http"
  inputs = ["parse_json"]
  uri = "http://signoz-otel-collector.platform.svc.cluster.local:8082"
  encoding.codec = "json"
  method = "post"
  request.headers."Content-Type" = "application/json"

  # Batch events into a JSON array — httplogreceiver/json expects an array body.
  [sinks.signoz_logs.batch]
    max_events = 100
    timeout_secs = 1

  # Backpressure: disk-backed buffer prevents log loss during SigNoz downtime.
  # The operator patches these values from the SignalPolicy CR.
  [sinks.signoz_logs.buffer]
    type = "disk"
    max_size = %d
    when_full = "%s"
`,
		bufferMaxBytes, whenFull, bufferMaxBytes, whenFull)
}

// ------------------------------------------------------------------
// Kubernetes helpers
// ------------------------------------------------------------------

// applyConfigMap creates or updates a ConfigMap with the given data.
// This is an "apply" (server-side upsert) — idempotent.
func (c *Controller) applyConfigMap(ns, name string, data map[string]string) error {
	ctx := context.Background()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "signal-forge-operator",
			},
		},
		Data: data,
	}

	_, err := c.k8s.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = c.k8s.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	_, err = c.k8s.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// annotateDeployment patches the pod template annotation on a Deployment,
// which causes Kubernetes to trigger a rolling restart.
// This is the standard "config checksum" rolling restart pattern.
func (c *Controller) annotateDeployment(ns, name, key, value string) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		key, value)
	_, err := c.k8s.AppsV1().Deployments(ns).Patch(
		context.Background(),
		name,
		types.MergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)
	return err
}

// annotateDaemonSet patches the pod template annotation on a DaemonSet,
// triggering a rolling update (same checksum pattern as annotateDeployment).
func (c *Controller) annotateDaemonSet(ns, name, key, value string) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		key, value)
	_, err := c.k8s.AppsV1().DaemonSets(ns).Patch(
		context.Background(),
		name,
		types.MergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)
	return err
}

// patchDeploymentEnv patches a single env var in the first container of a Deployment.
// Uses strategic merge patch keyed on container name + env var name, so it targets
// the correct entry regardless of its index in the env array.
func (c *Controller) patchDeploymentEnv(ns, deployName, envKey, envVal string) error {
	patch := fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[{"name":%q,"env":[{"name":%q,"value":%q}]}]}}}}`,
		deployName, envKey, envVal,
	)
	_, err := c.k8s.AppsV1().Deployments(ns).Patch(
		context.Background(),
		deployName,
		types.StrategicMergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// patchStatus writes the status subresource back to the SignalPolicy CR.
func (c *Controller) patchStatus(name string, status SignalPolicyStatus) error {
	statusMap := map[string]interface{}{
		"status": map[string]interface{}{
			"phase":             status.Phase,
			"otelConfigHash":    status.OtelConfigHash,
			"vectorConfigHash":  status.VectorConfigHash,
			"lastReconcileTime": status.LastReconcileTime,
			"message":           status.Message,
		},
	}
	patch, err := json.Marshal(map[string]interface{}{"status": statusMap["status"]})
	if err != nil {
		return err
	}

	_, err = c.dynamic.Resource(signalPolicyGVR).Namespace(c.ns).Patch(
		context.Background(),
		name,
		types.MergePatchType,
		patch,
		metav1.PatchOptions{},
		"status",
	)
	return err
}

// configHash returns a short SHA-256 hash of the config string.
// Used as the rolling restart annotation value.
func configHash(config string) string {
	h := sha256.Sum256([]byte(config))
	return fmt.Sprintf("%x", h[:8])
}

func unstructuredString(obj map[string]interface{}, keys ...string) (string, bool, error) {
	var cur interface{} = obj
	for _, k := range keys {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return "", false, nil
		}
		cur = m[k]
	}
	s, ok := cur.(string)
	return s, ok, nil
}

func loadKubeConfig() (*rest.Config, error) {
	// In-cluster config when running inside Kubernetes.
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	// Fall back to kubeconfig for local development.
	kubeconfig := envOrDefault("KUBECONFIG", os.ExpandEnv("$HOME/.kube/config"))
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Satisfy unused import from runtime package (used for type assertion in production).
var _ runtime.Object
