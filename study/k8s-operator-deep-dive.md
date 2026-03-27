# Kubernetes Operator Deep Dive
### STACKIT Telemetry Router Interview Prep — Mike Falcone

---

## Why This Section Matters

The JD says: *"You develop Kubernetes operators to automate the life cycle of the Telemetry Router service and its configuration."* and *"go and k8s being your bread and butter — k8s operators preferably also a part of your arsenal."*

You have reconcile loop experience from NetApp (mTLS enablement, Helm, Envoy sidecars). This section makes that experience crisp and maps it directly to the Telemetry Router context.

---

## Part 1: What is a Kubernetes Operator?

An **operator** is a Kubernetes-native application that automates the management of complex stateful workloads. It combines:

- A **Custom Resource Definition (CRD)** — extends the Kubernetes API with your own object types
- A **Controller** — a control loop that watches those objects and reconciles desired state with actual state

The pattern: *observe → diff → act → repeat*

**Key insight for the interview:** A Kubernetes operator is just a Kubernetes controller that manages application-specific CRDs. The "operator pattern" is nothing more than encoding operational knowledge (how to deploy, scale, upgrade, heal) into a controller instead of a runbook.

---

## Part 2: The TelemetryRouter CRD — Design Decisions

```yaml
# This is what a customer-facing TelemetryRouter CR looks like in production.
# Every field here is a design decision worth defending.
apiVersion: telemetry.stackit.cloud/v1alpha1
kind: TelemetryRouter
metadata:
  name: my-router
  namespace: stackit-telemetry
  annotations:
    # Finalizer prevents deletion until the operator drains in-flight events
    finalizers:
      - telemetry.stackit.cloud/drain-finalizer
spec:
  instanceId: "inst-abc123"
  projectId: "proj-xyz789"
  region: "eu-central-1"
  replicas: 2
  allowNonEU: false           # explicit sovereignty opt-in; defaults false

  destinations:
    - destinationId: "dest-001"
      name: "Production SIEM"
      type: OTLP
      otlpEndpoint: "https://ingest.siem.example.com:4317"
      secretRef: "siem-api-key"   # k8s Secret name — never inline credentials

    - destinationId: "dest-002"
      name: "S3 Archive"
      type: S3
      s3Bucket: "telemetry-archive-prod"
      s3BucketRegion: "eu-central-1"
      s3Endpoint: "https://object.storage.eu-central-1.stackit.cloud"
      secretRef: "s3-credentials"

  resources:
    requestsCpu: "500m"
    requestsMemory: "512Mi"
    limitsCpu: "2000m"
    limitsMemory: "2Gi"

status:
  # Written by the operator — NEVER by the customer
  phase: Ready
  vectorConfigHash: "a3f9b2c1..."   # SHA-256 of applied config; detects drift
  readyReplicas: 2
  lastReconcileAt: "2026-03-22T14:00:00Z"
  conditions:
    - type: RegionCompliant
      status: "True"
      reason: Compliant
    - type: SecretsResolved
      status: "True"
      reason: Resolved
    - type: ConfigApplied
      status: "True"
      reason: Applied
    - type: DeploymentReady
      status: "True"
      reason: Ready
      message: "2 of 2 replicas ready"
```

### CRD Design Decisions — Be Ready to Defend These

**Why `secretRef` instead of inline credentials in the spec?**
k8s Secrets are etcd-encrypted at rest and access-controlled with RBAC. Inline credentials in a CRD spec appear in etcd in plaintext, in `kubectl get` output, and in audit logs. `secretRef` means the operator resolves the Secret at reconcile time and injects it as an env var — it never leaves the Secret object except transiently in-memory.

**Why `allowNonEU: false` as a default?**
Opt-out compliance is always weaker than opt-in. By requiring explicit `true`, a misconfiguration defaults to the safer behavior. The operator also validates this at reconcile time (Layer 2), so even a direct CRD patch can't bypass it.

**Why `vectorConfigHash` in status?**
The operator can detect config drift without re-rendering. If the hash in status matches the hash of the current desired spec, skip the apply (no unnecessary pod restarts). If they diverge (e.g., someone manually edited the ConfigMap), the next reconcile detects it and re-applies.

**Why use `conditions` rather than just `phase`?**
`phase` is a coarse summary. `conditions` follow the standard k8s condition pattern (matching `metav1.Condition`) and allow independent tracking of multiple state dimensions. A router can be `RegionCompliant: True` but `DeploymentReady: False` (replicas not yet scheduled). `kubectl describe` shows all conditions, making debugging deterministic.

**Why a finalizer (`drain-finalizer`)?**
Without a finalizer, deleting the CR immediately removes the Vector pod, dropping any in-flight events. The finalizer prevents deletion until the operator has: (1) stopped accepting new events, (2) flushed all per-sink buffers, (3) confirmed all delivery attempts are persisted. Only then does the operator remove the finalizer, allowing k8s to complete the deletion.

---

## Part 3: The Reconcile Loop — Step by Step

The reconcile loop is the heart of the operator. Here's the **complete sequence** for the Telemetry Router:

```
Trigger: TelemetryRouter CR created / updated / destination pod unhealthy

reconcileOnce(ctx, tr TelemetryRouter):

1. FETCH current state
   └─ Get the TelemetryRouter CR from the API server
   └─ Get the current Deployment (may not exist yet)
   └─ Get the current ConfigMap (may not exist yet)

2. VALIDATE region policy (enforcement layer 2)
   └─ For each destination in spec.destinations:
       └─ If type=S3 && region not in EU && !spec.allowNonEU → FAIL
           → Set condition RegionCompliant: False
           → Return error (requeue with backoff)
   └─ Set condition RegionCompliant: True

3. RESOLVE secrets
   └─ For each destination with secretRef:
       └─ k8sClient.GetSecret(namespace, secretRef)
       └─ Build env var list: DEST_{ID}_API_KEY, DEST_{ID}_ACCESS_KEY_ID, etc.
   └─ If any secret missing → Set condition SecretsResolved: False → return error

4. RENDER Vector config
   └─ Go text/template over spec.destinations → TOML string
   └─ Compute SHA-256 hash of rendered config
   └─ If hash == status.vectorConfigHash → SKIP (no-op, prevent pod restarts)

5. APPLY ConfigMap
   └─ CreateOrUpdate ConfigMap {name}-vector-config with rendered TOML
   └─ SetControllerReference(tr, configMap) → GC on CR deletion

6. APPLY Deployment
   └─ CreateOrUpdate Deployment {name}
       → image: vector:latest-distroless
       → replicas: spec.replicas
       → volumeMount: ConfigMap → /etc/vector/vector.toml
       → env: inject resolved secret values
       → probes: liveness on Vector's /health endpoint (port 8686)
   └─ SetControllerReference(tr, deployment)

7. UPDATE status
   └─ status.phase = Ready
   └─ status.vectorConfigHash = newHash
   └─ status.readyReplicas = deployment.status.readyReplicas
   └─ status.lastReconcileAt = now()
   └─ Set all conditions True
   └─ k8sClient.Status().Patch(ctx, tr, patch)

8. REQUEUE (optional)
   └─ Return ctrl.Result{RequeueAfter: 5 * time.Minute}
      to periodically re-check for drift
```

### What Triggers a Reconcile?

In controller-runtime, you register **watches** in `SetupWithManager()`:

```go
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&v1alpha1.TelemetryRouter{}).          // watch the CR itself
        Owns(&appsv1.Deployment{}).                // watch owned Deployments
        Owns(&corev1.ConfigMap{}).                 // watch owned ConfigMaps
        Watches(                                   // watch referenced Secrets
            &corev1.Secret{},
            handler.EnqueueRequestsFromMapFunc(r.secretToRouterRequests),
        ).
        Complete(r)
}
```

The controller reconciles when:
- A `TelemetryRouter` CR is created, updated, or deleted
- An owned `Deployment` changes (pod becomes unhealthy → readyReplicas drops → reconcile fires → status updated)
- An owned `ConfigMap` changes (drift detection → reconcile re-applies the correct config)
- A referenced `Secret` changes (credentials rotated → reconcile picks up new values)

### Idempotency — The Most Important Property

The reconcile loop **must be idempotent**. Calling `Reconcile()` 10 times with the same desired state must produce the same result as calling it once. This is why:
- `CreateOrUpdate` (not `Create`) is used for all resources
- The config hash check prevents unnecessary restarts
- Status conditions are set to their final value, not incremented
- The loop is safe to call during leader election re-runs

---

## Part 4: Secret Management in Depth

### The Three Anti-Patterns (Never Do These)

1. **Inline in the CRD spec** — appears in etcd plaintext, `kubectl get`, audit logs
2. **Mounted as a file in the Vector config** — config file lands on disk; if a core dump occurs, secret is in the dump
3. **Hardcoded in the container image** — rotated credentials require a new image build

### The Correct Pattern

```
k8s Secret                      Operator Reconcile              Vector Pod
┌─────────────────────┐         ┌──────────────────────┐       ┌────────────────────┐
│ siem-credentials    │──read──▶│ resolveSecrets()      │──────▶│ env:               │
│  api_key: eyJ...    │         │  → build []EnvVar     │       │  - name: DEST_     │
│                     │         │                        │       │    SIEM001_API_KEY  │
└─────────────────────┘         └──────────────────────┘       │    value: eyJ...    │
                                                                 │                    │
k8s Secret                                                       │ vector.toml:       │
┌─────────────────────┐                                         │ auth.token =       │
│ s3-credentials      │──read──▶ same flow ──────────────────▶ │ "${DEST_S3_ACCESS}"│
│  access_key_id: AK  │                                         └────────────────────┘
│  secret_access_key  │
└─────────────────────┘
```

The Vector TOML references `${ENV_VAR_NAME}` — the secret value is in-memory in the process, never written to disk in the config. If you `kubectl exec` into the Vector pod and `cat /etc/vector/vector.toml`, you see the variable reference, not the value.

### Secret Rotation Without Pod Restart

Use `reloader` annotations (Reloader controller) or a `secretVersion` annotation on the Deployment pod spec. When a Secret changes, the operator's Secret watch fires, triggering reconcile, which bumps the annotation → rolling restart of Vector pods picks up new credentials.

```go
// In the Deployment template annotations:
"secret-version": hashOfSecrets, // bump this → triggers rolling restart
```

---

## Part 5: Common Interview Questions & Model Answers

### Q: "What happens when a destination pod becomes unhealthy?"

**Model answer:** "The Deployment controller drops the pod's Ready condition, which decrements `readyReplicas`. Since we set `Owns(&appsv1.Deployment{})` in our controller's watch registration, any change to the owned Deployment triggers a reconcile of the parent `TelemetryRouter`. Our reconcile loop reads the current Deployment status, updates `status.readyReplicas`, and sets the `DeploymentReady` condition to `False` with a message like '1 of 2 replicas ready'. This surfaces immediately in `kubectl describe telemet routerrouter my-router`. Kubernetes's Deployment controller is already handling the actual pod restart — our operator just keeps the CR status accurate so customers can see the health signal through the STACKIT API."

### Q: "How do you handle a customer updating their destination list via the REST API?"

**Model answer:** "The REST API handler updates the destination record in the database and marks it as `PENDING`, then triggers an async task that patches the `TelemetryRouter` CR spec via the k8s API — specifically, it updates `spec.destinations` to add/remove/modify the destination entry. This change fires a reconcile. The reconcile loop re-renders the Vector TOML, computes the new hash, and if it differs from `status.vectorConfigHash`, applies the new ConfigMap and triggers a rolling restart of the Vector pods. The REST API returns `202 Accepted` with a task ID immediately; the customer polls `GET /tasks/{taskId}` or watches the destination's status until it transitions from `PENDING` to `ACTIVE`."

### Q: "Why not just use Helm charts instead of an operator?"

**Model answer:** "Helm is great for static configuration — install/upgrade/rollback. But Helm doesn't watch runtime state. If a customer rotates their SIEM credentials at 3am, a Helm chart doesn't detect that and react. An operator does, because it watches the Secret and fires a reconcile. Similarly, if a Vector pod is evicted because the node runs out of memory, the operator can detect `readyReplicas < desiredReplicas` and update the customer-facing status. Helm doesn't have this reactive loop. For a managed service like the Telemetry Router where STACKIT is responsible for the SLA, the operator pattern is the right tool."

### Q: "What is controller-runtime's `Reconcile` return value and when do you use each option?"

**Model answer:**
- `ctrl.Result{}, nil` — reconcile succeeded, no requeue needed. Only fire again when state changes.
- `ctrl.Result{RequeueAfter: 5 * time.Minute}, nil` — succeed but re-check periodically (drift detection, health polling).
- `ctrl.Result{}, err` — reconcile failed; controller-runtime will requeue with exponential backoff. Use for transient errors (network, API server unavailable).
- `ctrl.Result{Requeue: true}, nil` — requeue immediately without delay. Use when you've made a change and need to verify it in the next loop.

**Critical:** Never return an error for business logic failures (e.g., region policy violation). Return `nil` with a degraded status condition — errors trigger requeue, but a region policy violation won't fix itself by retrying. Only return errors for infrastructure/transient problems.

### Q: "How do you prevent multiple reconcile goroutines from conflicting on the same CR?"

**Model answer:** "controller-runtime's work queue is per-resource-key (namespace/name). Multiple events for the same CR are coalesced — if 10 updates come in while a reconcile is in progress, they result in exactly one additional reconcile after the current one finishes. The controller runs one goroutine per concurrent worker (configurable with `MaxConcurrentReconciles`), but each worker holds the queue item exclusively while reconciling. For the `TelemetryRouter`, we use `MaxConcurrentReconciles: 1` — customer instances are independent, but within a single instance, we want sequential consistency so we don't race on config hash comparison."

---

## Part 6: Connect to Your Resume — What to Say

When the interviewer asks about your operator experience:

> *"At NetApp, I built and maintained reconcile loops for Astra — specifically around mTLS enablement and Envoy sidecar injection. We used controller-runtime with `CreateOrUpdate` semantics and Helm chart rendering for service mesh config. The pattern maps directly to what I'd do here: the TelemetryRouter operator watches the CR, resolves secrets, renders a Vector TOML config into a ConfigMap, and does a rolling restart of the Vector pods when config changes. The main addition for STACKIT is the region compliance enforcement in the reconcile loop — I'd model that as an early-exit validation step that sets a `RegionCompliant` condition, so violations are visible through the standard k8s condition API rather than buried in logs."*

Key points to emphasize:
- **You've done this before** (NetApp, reconcile loops, mTLS, Helm, Envoy)
- **You understand idempotency** — "CreateOrUpdate, not Create"
- **You know the secret management pattern** — secretRef, env var injection, never inline
- **You've operated k8s in production** — "You Build It You Run It" at NetApp war rooms
