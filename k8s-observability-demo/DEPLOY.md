# k8s-observability-demo — Deployment Guide

A Kubernetes application that generates OpenTelemetry signals (traces, metrics, logs) and demonstrates the key patterns you'll be discussing in your STACKIT Telemetry Router interview.

---

## What You're Deploying

```
┌─────────────────────────────────────────────────────────────┐
│  signal-forge (Go app)                                       │
│  ├─ REST API  ──── traces/metrics (OTLP gRPC) ──►           │
│  └─ JSON logs (stdout)                                       │
└───────────────────┬─────────────────────┬───────────────────┘
                    │                     │
                    ▼                     ▼
         ┌──────────────────┐   ┌────────────────────┐
         │  OTel Collector  │   │  Vector DaemonSet  │
         │                  │   │                    │
         │  tail_sampling   │   │  kubernetes_logs   │
         │  batch processor │   │  parse JSON (VRL)  │
         │  sending_queue   │   │  disk buffer       │
         └────────┬─────────┘   └────────┬───────────┘
                  │                       │
                  └──────────┬────────────┘
                             ▼
                    ┌──────────────────┐
                    │     SigNoz       │
                    │  (open source)   │
                    │  Traces / Logs / │
                    │  Metrics UI      │
                    └──────────────────┘

  SignalPolicy CR ──► operator ──► patch ConfigMaps ──► rolling restart
```

**Key components:**

| Component | Purpose | OTel Signal |
|---|---|---|
| `signal-forge` | Demo app with REST API | Traces + Metrics |
| `otel-collector` | Tail sampling + batching + backpressure | Receives OTLP, forwards to SigNoz |
| `vector` DaemonSet | Pod log collection | Reads `/var/log/pods`, forwards to SigNoz |
| `SignalPolicy` CRD | Runtime control of sampling/batching | — |
| `operator` | Watches SignalPolicy CRs, patches configs | — |
| SigNoz | Observability UI | Traces + Logs + Metrics |

---

## Prerequisites

| Tool | Version | Install |
|---|---|---|
| Docker Desktop | 4.x+ with Kubernetes enabled | [docs.docker.com](https://docs.docker.com/desktop/kubernetes/) |
| kubectl | 1.28+ | `brew install kubectl` |
| helm | 3.x | `brew install helm` |

**Enable Kubernetes in Docker Desktop:**
Preferences → Kubernetes → Enable Kubernetes → Apply & Restart

Verify:
```bash
kubectl config use-context docker-desktop
kubectl get nodes
# NAME             STATUS   ROLES           AGE
# docker-desktop   Ready    control-plane   ...
```

---

## Quick Start (Automated)

```bash
cd k8s-observability-demo/
./scripts/setup.sh
```

This will:
1. Install SigNoz via Helm (takes ~5 minutes on first run)
2. Build Docker images locally
3. Apply all manifests
4. Print port-forward commands

---

## Manual Step-by-Step

### Step 1 — Install SigNoz

SigNoz is the observability UI. It uses ClickHouse internally and exposes an OTLP receiver that all signals land in.

```bash
helm repo add signoz https://charts.signoz.io
helm repo update
helm install signoz signoz/signoz \
  --namespace platform \
  --create-namespace \
  --wait \
  --timeout 10m
```

Verify SigNoz is up:
```bash
kubectl get pods -n platform
# signoz-frontend-xxx    Running
# signoz-otel-collector-xxx  Running
# clickhouse-xxx         Running
```

### Step 2 — Build Docker Images

The manifests use `imagePullPolicy: Never`, so Kubernetes uses your local Docker images directly (no registry needed for Docker Desktop).

```bash
# App image
docker build -t signal-forge:latest -f Dockerfile .

# Operator image
docker build -t signal-forge-operator:latest -f Dockerfile.operator .
```

### Step 3 — Apply CRD First

CRDs must exist before any CR can be created. Apply them separately to avoid race conditions:

```bash
kubectl apply -f k8s/crds/

# Wait for CRD to be established (required before applying SignalPolicy CRs)
kubectl wait --for=condition=Established \
  crd/signalpolicies.observability.demo.dev \
  --timeout=30s
```

### Step 4 — Apply Everything Else

```bash
kubectl apply -k k8s/
```

Verify:
```bash
kubectl get pods -n observability-demo
# NAME                                    READY   STATUS
# signal-forge-xxx                        1/1     Running
# signal-forge-operator-xxx               1/1     Running
# otel-collector-xxx                      1/1     Running
# vector-xxx (DaemonSet — one per node)   1/1     Running

kubectl get signalpolicies -n observability-demo
# NAME      PHASE    OTELHASH   VECTORHASH   AGE
# default   Active   a1b2c3d4   e5f6a7b8     30s
```

---

## Accessing the Services

### SigNoz UI

```bash
kubectl port-forward svc/signoz-frontend 3301:3301 -n platform
open http://localhost:3301
```

Login: `admin@signoz.io` / `password` (change on first login)

### signal-forge API + Swagger UI

```bash
kubectl port-forward svc/signal-forge 8080:8080 -n observability-demo
open http://localhost:8080/docs
```

---

## Generating Signals

### Create Widgets (produces normal traces + metrics)
```bash
# Create a widget
curl -s -X POST http://localhost:8080/api/v1/widgets \
  -H 'Content-Type: application/json' \
  -d '{"name":"my-widget","color":"blue","weight":1.5}' | jq

# List widgets
curl -s http://localhost:8080/api/v1/widgets | jq

# Filter by color
curl -s 'http://localhost:8080/api/v1/widgets?color=blue' | jq
```

### Simulate Slow Trace (tests tail sampling latency policy)
```bash
curl -s http://localhost:8080/api/v1/simulate/slow | jq
# Returns after 2s with the traceId — find it in SigNoz > Traces
```

### Simulate Error Trace (tests tail sampling error policy)
```bash
curl -s http://localhost:8080/api/v1/simulate/error | jq
# Returns 503 — find the error trace in SigNoz > Traces
```

### Generate Traffic in a Loop
```bash
# Continuous traffic for 60 seconds
for i in $(seq 1 30); do
  curl -s -X POST http://localhost:8080/api/v1/widgets \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"widget-$i\",\"color\":\"red\",\"weight\":$i}" > /dev/null
  curl -s http://localhost:8080/api/v1/simulate/slow > /dev/null &
  sleep 2
done
```

---

## Exploring in SigNoz

### Traces
1. SigNoz → APM → Traces
2. Filter by `service.name = signal-forge`
3. Click a slow trace → see the `slowDBQuery` child span with 2s latency
4. Click an error trace → see the `SimulateError` span with `status = ERROR`

### Logs
1. SigNoz → Logs
2. Filter by `k8s.namespace.name = observability-demo`
3. Notice the `traceId` field — click it to jump to the correlated trace
4. This log→trace correlation is powered by zerolog embedding the trace/span ID

### Metrics
1. SigNoz → Metrics → `http.server.request.total`
2. Group by `path` and `status_code`
3. `http.server.active_requests` shows in-flight request gauge

---

## Demo: SignalPolicy Changes

The CRD + operator is the most interview-relevant part. Walk through it:

### Watch operator logs in real time
```bash
kubectl logs -f deployment/signal-forge-operator -n observability-demo
```

### Apply the high-sampling policy
```bash
kubectl apply -f k8s/signalpolicy/high-sampling.yaml

# Watch the operator reconcile
kubectl get signalpolicies -w -n observability-demo
# NAME           PHASE         OTELHASH   VECTORHASH
# high-sampling  Reconciling   ...
# high-sampling  Active        a1b2c3d4   e5f6a7b8
```

The operator:
1. Parsed the SignalPolicy spec
2. Rendered a new OTel Collector config with `sampling_percentage: 10.0`
3. Applied it to the `otel-collector-config` ConfigMap
4. Annotated the `otel-collector` Deployment with the config hash
5. Kubernetes detected the pod template change → triggered rolling restart
6. Patched status on the SignalPolicy CR

### Inspect what changed
```bash
# See the generated OTel Collector config
kubectl get configmap otel-collector-config -n observability-demo \
  -o jsonpath='{.data.config\.yaml}'

# See the config hash annotation on the Deployment
kubectl get deployment otel-collector -n observability-demo \
  -o jsonpath='{.spec.template.metadata.annotations}'
```

### Try the backpressure demo
```bash
kubectl apply -f k8s/signalpolicy/backpressure-demo.yaml
# This sets when_full=block and a tiny 10MB buffer.
# Then flood the endpoint to see Vector apply backpressure:
for i in $(seq 1 100); do
  curl -s http://localhost:8080/api/v1/simulate/slow > /dev/null &
done
kubectl logs -f daemonset/vector -n observability-demo
```

---

## Architecture Deep Dive

### Why tail sampling (OTel Collector) AND head sampling (app SDK)?

**Head sampling** (`TraceIDRatioBased` in the app) is a fast pre-filter. It decides at span creation time without context — cheap, but dumb. All spans in a trace are sampled consistently because the decision is based on TraceID (a stable hash).

**Tail sampling** (OTel Collector `tail_sampling` processor) buffers ALL spans for `decision_wait` seconds, then makes a smarter per-trace decision. It can:
- Always keep traces with error spans (regardless of ratio)
- Always keep slow traces (latency > threshold)
- Sample the remaining healthy/fast traces at any ratio

In this demo, head sampling at 1.0 (keep everything) + tail sampling with smart policies gives you the best of both worlds. Apply `high-sampling` to see 10% head pre-filtering with error/slow overrides.

### Backpressure flow (interview answer)

```
signal-forge spans
       │
       ▼ SDK BatchSpanProcessor (MaxQueueSize=2048)
       │   ← drops NEW spans when queue full (head-drop, non-blocking)
       │
       ▼ OTel Collector memory_limiter
       │   ← drops data when process memory > 512MB
       │
       ▼ OTel Collector tail_sampling (buffers for 10s)
       │
       ▼ OTel Collector batch processor
       │   ← buffers until send_batch_size or timeout
       │
       ▼ OTel Collector sending_queue (queue_size=1000)
       │   ← absorbs SigNoz slowness; drops when full
       │
       ▼ SigNoz

Vector logs:
       │
       ▼ disk buffer (max_size=256MB, when_full=drop_newest|block)
       │   ← absorbs SigNoz downtime; controlled loss vs block tradeoff
       ▼
       SigNoz
```

### Config hash rolling restart (interview answer)

This is the standard pattern for propagating ConfigMap changes to pods:

1. Operator renders new config → computes SHA-256 hash
2. Patches pod template annotation: `observability.demo.dev/config-hash: <hash>`
3. Kubernetes sees pod template changed → triggers rolling restart
4. New pods mount the new ConfigMap version

Without this, Kubernetes won't restart pods when a ConfigMap changes (pods only see ConfigMap changes if they use `envFrom` or if the app watches and reloads, neither of which applies here).

---

## Cleanup

```bash
# Remove demo only (keep SigNoz for reuse)
./scripts/teardown.sh

# Remove everything including SigNoz
./scripts/teardown.sh --all
```
