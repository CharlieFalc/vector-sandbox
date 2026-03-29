package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	telemetryv1 "github.com/stackit-mock/operator/api/v1alpha1"
	"github.com/stackit-mock/operator/internal/vector"
)

const (
	finalizerName = "telemetry.stackit.local/drain-finalizer"
	vectorImage   = "timberio/vector:0.53.0-distroless-libc"

	// Annotation keys written to the Vector ConfigMap.
	annotationConfigHash = "telemetry.stackit.local/config-hash"
	// annotationLKGConfig stores the full TOML of the last config that passed
	// post-rollout health validation. Written before each config overwrite so
	// the operator can restore it if the new config causes a crash-loop.
	annotationLKGConfig = "telemetry.stackit.local/last-known-good-config"
	annotationLKGHash   = "telemetry.stackit.local/last-known-good-hash"

	// rolloutValidationWindow is how long the operator waits after pushing a
	// new config before checking whether Vector has started cleanly.
	// Should comfortably exceed Vector's startup time + readiness probe window.
	// Vector typically passes its /health probe within 5-10s; 20s gives a
	// comfortable buffer for crash-looping to surface before promoting to Ready.
	rolloutValidationWindow = 20 * time.Second
)

// TelemetryRouterReconciler watches TelemetryRouter CRDs and manages
// Vector deployments + ConfigMaps to match the declared spec.
type TelemetryRouterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *TelemetryRouterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&telemetryv1.TelemetryRouter{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=telemetry.stackit.local,resources=telemetryrouters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=telemetry.stackit.local,resources=telemetryrouters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;services,verbs=get;list;watch;create;update;patch;delete

func (r *TelemetryRouterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// ── 1. Fetch the TelemetryRouter CR ──────────────────────────────────────
	var tr telemetryv1.TelemetryRouter
	if err := r.Get(ctx, req.NamespacedName, &tr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// ── 2. Handle deletion (drain finalizer) ─────────────────────────────────
	if !tr.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &tr)
	}

	// ── 3. Add finalizer if missing ───────────────────────────────────────────
	if !containsString(tr.Finalizers, finalizerName) {
		tr.Finalizers = append(tr.Finalizers, finalizerName)
		if err := r.Update(ctx, &tr); err != nil {
			return ctrl.Result{}, err
		}
	}

	// ── 4. Render Vector TOML config ──────────────────────────────────────────
	vectorCfg, err := vector.Render(&tr)
	if err != nil {
		logger.Error(err, "Failed to render Vector config")
		return ctrl.Result{}, r.setStatus(ctx, &tr, "ConfigError", err.Error())
	}
	cfgHash := vector.Hash(vectorCfg)
	logger.Info("Rendered Vector config", "hash", cfgHash)

	// ── 4b. Skip known-bad configs ────────────────────────────────────────────
	// If this exact config hash previously caused a Vector crash-loop, don't
	// re-apply it. The ConfigMap and Deployment already hold the last-known-good
	// config from the rollback. We requeue slowly so the operator stays
	// responsive when the user fixes the spec.
	if cfgHash == tr.Status.BadConfigHash {
		logger.Info("Config hash is flagged bad — skipping apply; fix spec to retry",
			"hash", cfgHash)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// ── 4c. Validation pass (post-rollout health check) ───────────────────────
	// When the operator is in the "Validating" phase it previously pushed a new
	// config and scheduled a requeue. On this pass we inspect the Vector pods:
	//   • CrashLoopBackOff → roll back to the last-known-good config
	//   • All healthy       → promote the new config to last-known-good
	//
	// IMPORTANT: status updates (r.Status().Update) generate a watch event that
	// immediately re-triggers the reconciler. Without the elapsed-time guard
	// below, those watch-triggered reconciles would run this health check far
	// too early — before Vector has had time to either start cleanly or enter
	// CrashLoopBackOff — and would falsely promote a crashing config.
	if tr.Status.Phase == "Validating" && cfgHash == tr.Status.VectorConfigHash {
		// Enforce the validation window before running any health check.
		elapsed := rolloutValidationWindow // default: assume window has passed
		if tr.Status.ValidatingStartedAt != "" {
			if startedAt, parseErr := time.Parse(time.RFC3339, tr.Status.ValidatingStartedAt); parseErr == nil {
				elapsed = time.Since(startedAt)
			}
		}
		if elapsed < rolloutValidationWindow {
			remaining := rolloutValidationWindow - elapsed
			logger.Info("Validation window still open — deferring health check",
				"elapsed", elapsed.Round(time.Second),
				"remaining", remaining.Round(time.Second))
			// Return without a status update — no watch event, no premature loop.
			return ctrl.Result{RequeueAfter: remaining}, nil
		}

		crashing, err := r.isVectorCrashLooping(ctx, &tr)
		if err != nil {
			return ctrl.Result{}, err
		}
		if crashing {
			logger.Info("Crash-loop detected — rolling back config",
				"badHash", cfgHash, "lkgHash", tr.Status.LastKnownGoodHash)

			lkgHash, err := r.rollbackConfig(ctx, &tr)
			if err != nil {
				return ctrl.Result{}, r.setStatus(ctx, &tr, "RollbackFailed",
					fmt.Sprintf("rollback failed: %v", err))
			}
			// Drive the Deployment to pick up the restored ConfigMap hash.
			if err := r.reconcileDeployment(ctx, &tr, lkgHash); err != nil {
				return ctrl.Result{}, err
			}
			tr.Status.Phase           = "RolledBack"
			tr.Status.BadConfigHash   = cfgHash
			tr.Status.VectorConfigHash = lkgHash
			tr.Status.Message         = fmt.Sprintf(
				"config %s caused crash-loop; rolled back to %s — fix spec to retry",
				cfgHash, lkgHash)
			tr.Status.LastReconcileAt = time.Now().UTC().Format(time.RFC3339)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, &tr)
		}
		// Healthy — promote this config to last-known-good.
		logger.Info("Post-rollout health check passed — config promoted",
			"hash", cfgHash)
		tr.Status.LastKnownGoodHash = cfgHash
		// Fall through to the normal Ready status update below.
	}

	// ── 5. Reconcile ConfigMap ────────────────────────────────────────────────
	if err := r.reconcileConfigMap(ctx, &tr, vectorCfg, cfgHash); err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &tr, "ConfigMapError", err.Error())
	}

	// ── 6. Reconcile Deployment ───────────────────────────────────────────────
	if err := r.reconcileDeployment(ctx, &tr, cfgHash); err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &tr, "DeploymentError", err.Error())
	}

	// ── 7. Reconcile Service ──────────────────────────────────────────────────
	if err := r.reconcileService(ctx, &tr); err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &tr, "ServiceError", err.Error())
	}

	// ── 8. Update status ──────────────────────────────────────────────────────
	// If the config hash just changed (new config deployed for the first time,
	// or after a spec edit), enter the "Validating" phase and schedule a
	// requeue after the rollout window. The next reconcile will check pod health.
	if cfgHash != tr.Status.LastKnownGoodHash {
		// Already in Validating for this hash — the status write already happened.
		// Don't re-write it; that would emit another watch event and restart the
		// validation timer (overwriting ValidatingStartedAt).
		if tr.Status.Phase == "Validating" && tr.Status.VectorConfigHash == cfgHash {
			logger.Info("Already in validation window — waiting for timer",
				"hash", cfgHash)
			return ctrl.Result{RequeueAfter: rolloutValidationWindow}, nil
		}
		logger.Info("New config deployed — entering validation window",
			"hash", cfgHash, "window", rolloutValidationWindow)
		tr.Status.Phase              = "Validating"
		tr.Status.VectorConfigHash   = cfgHash
		tr.Status.ValidatingStartedAt = time.Now().UTC().Format(time.RFC3339)
		tr.Status.Message             = fmt.Sprintf(
			"validating config %s — will roll back automatically on crash-loop", cfgHash)
		tr.Status.LastReconcileAt = time.Now().UTC().Format(time.RFC3339)
		return ctrl.Result{RequeueAfter: rolloutValidationWindow}, r.Status().Update(ctx, &tr)
	}

	// Config hash matches LastKnownGoodHash — this is steady state.
	// CRITICAL: skip the status write when we're already reporting Ready with
	// the same hash. r.Status().Update() triggers a watch event on the
	// TelemetryRouter, which immediately re-queues a reconcile. Without this
	// guard every reconcile loop would write a new status (even if only
	// LastReconcileAt changed), causing an infinite reconcile storm.
	if tr.Status.Phase == "Ready" && tr.Status.VectorConfigHash == cfgHash {
		logger.Info("Reconcile complete (steady state — no status change)",
			"name", tr.Name, "hash", cfgHash)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// First time reaching Ready for this hash (e.g. fresh deploy, or just
	// promoted from Validating). Write the status once, then stay quiet.
	tr.Status.Phase              = "Ready"
	tr.Status.VectorConfigHash   = cfgHash
	tr.Status.ValidatingStartedAt = "" // clear — no longer validating
	tr.Status.Message             = "Vector deployment running"
	tr.Status.LastReconcileAt     = time.Now().UTC().Format(time.RFC3339)
	if err := r.Status().Update(ctx, &tr); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Reconcile complete", "name", tr.Name, "hash", cfgHash)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileConfigMap ensures the Vector TOML ConfigMap exists and is up-to-date.
// Before overwriting a changed config it snapshots the current TOML into the
// annotationLKGConfig annotation so rollbackConfig can restore it if the new
// config causes a Vector crash-loop.
func (r *TelemetryRouterReconciler) reconcileConfigMap(
	ctx context.Context, tr *telemetryv1.TelemetryRouter, cfg, hash string,
) error {
	name := vectorConfigMapName(tr.Name)
	cm := &corev1.ConfigMap{}

	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: tr.Namespace}, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: tr.Namespace,
				Labels:    vectorLabels(tr.Name),
				Annotations: map[string]string{
					annotationConfigHash: hash,
					// No LKG snapshot on first create — nothing to roll back to yet.
				},
			},
			Data: map[string]string{"vector.toml": cfg},
		}
		ctrl.SetControllerReference(tr, cm, r.Scheme)
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}

	// Nothing changed — skip the API call.
	if cm.Annotations[annotationConfigHash] == hash {
		return nil
	}

	// Hash changed. Snapshot the current (presumably working) config before
	// overwriting it. If the new config causes a crash-loop, rollbackConfig
	// reads this annotation and writes it back as the active config.
	if prev := cm.Data["vector.toml"]; prev != "" {
		cm.Annotations[annotationLKGConfig] = prev
		cm.Annotations[annotationLKGHash] = cm.Annotations[annotationConfigHash]
	}
	cm.Data["vector.toml"] = cfg
	cm.Annotations[annotationConfigHash] = hash
	return r.Update(ctx, cm)
}

// reconcileDeployment ensures the Vector Deployment exists and references the
// current ConfigMap. A hash annotation on the pod template triggers a rolling
// update whenever the config changes — no operator restart needed.
func (r *TelemetryRouterReconciler) reconcileDeployment(
	ctx context.Context, tr *telemetryv1.TelemetryRouter, cfgHash string,
) error {
	name := vectorDeploymentName(tr.Name)
	dep  := &appsv1.Deployment{}
	replicas := tr.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	desired := r.buildDeployment(tr, replicas, cfgHash)

	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: tr.Namespace}, dep)
	if apierrors.IsNotFound(err) {
		ctrl.SetControllerReference(tr, desired, r.Scheme)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Only update if something actually changed — avoids unnecessary pod churn.
	// The config hash is the primary change signal; we also check replicas and
	// image in case the TelemetryRouter spec was edited directly.
	currentHash := dep.Spec.Template.Annotations["telemetry.stackit.local/config-hash"]
	currentReplicas := int32(1)
	if dep.Spec.Replicas != nil {
		currentReplicas = *dep.Spec.Replicas
	}
	if currentHash == cfgHash &&
		currentReplicas == replicas &&
		dep.Spec.Template.Spec.Containers[0].Image == vectorImage {
		return nil // nothing changed, skip the API call
	}

	dep.Spec.Replicas = &replicas
	dep.Spec.Template.Annotations["telemetry.stackit.local/config-hash"] = cfgHash
	dep.Spec.Template.Spec.Containers[0].Image = vectorImage
	return r.Update(ctx, dep)
}

// reconcileService ensures a ClusterIP Service exists for Vector so other pods
// can send OTLP traffic to it at a stable DNS name.
func (r *TelemetryRouterReconciler) reconcileService(
	ctx context.Context, tr *telemetryv1.TelemetryRouter,
) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: "vector", Namespace: tr.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vector",
				Namespace: tr.Namespace,
				Labels:    vectorLabels(tr.Name),
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "vector", "router": tr.Name},
				Ports: []corev1.ServicePort{
					{Name: "otlp-grpc", Port: 4317, TargetPort: intstr.FromInt(4317)},
					{Name: "otlp-http", Port: 4318, TargetPort: intstr.FromInt(4318)},
					{Name: "metrics",   Port: 9598, TargetPort: intstr.FromInt(9598)},
				},
			},
		}
		ctrl.SetControllerReference(tr, svc, r.Scheme)
		return r.Create(ctx, svc)
	}
	return err
}

func (r *TelemetryRouterReconciler) buildDeployment(
	tr *telemetryv1.TelemetryRouter, replicas int32, cfgHash string,
) *appsv1.Deployment {
	labels := vectorLabels(tr.Name)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vectorDeploymentName(tr.Name),
			Namespace: tr.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						// Changing this hash triggers a rolling update
						"telemetry.stackit.local/config-hash": cfgHash,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "vector",
							Image: vectorImage,
							Args:  []string{"--config", "/etc/vector/vector.toml"},
							Ports: []corev1.ContainerPort{
								{Name: "otlp-grpc", ContainerPort: 4317},
								{Name: "otlp-http", ContainerPort: 4318},
								{Name: "api",       ContainerPort: 8686},
								{Name: "metrics",   ContainerPort: 9598},
							},
							Env: []corev1.EnvVar{
								{Name: "VECTOR_LOG", Value: "info"},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "config", MountPath: "/etc/vector", ReadOnly: true},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/health",
										Port: intstr.FromInt(8686),
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       10,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1000m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: vectorConfigMapName(tr.Name),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (r *TelemetryRouterReconciler) handleDeletion(
	ctx context.Context, tr *telemetryv1.TelemetryRouter,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling deletion — waiting for Vector buffers to drain", "name", tr.Name)

	// drainCheck simulates asking Vector whether its disk buffers are empty.
	// In production this would call Vector's /health or /metrics endpoint and
	// inspect the buffer_events gauge for each sink. We stub it here so the
	// loop structure is realistic without needing a live Vector process.
	attempt := 0
	drainCheck := func() bool {
		attempt++
		// Pretend the buffers clear after 3 polls so the loop actually exits
		// in tests. A real implementation would inspect Vector's internal metrics.
		return attempt >= 3
	}

	// Poll until the buffers report empty, re-checking every 5 seconds.
	// Two things can unblock each iteration:
	//   - ctx.Done() → the operator received SIGTERM mid-deletion; give up so
	//     Kubernetes doesn't wait forever for the finalizer to be removed.
	//   - ticker.C   → time to ask Vector whether it has finished flushing.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Operator is shutting down before the drain completed.
			// Return the context error so controller-runtime re-queues the
			// deletion on the next startup rather than silently swallowing it.
			logger.Info("Context cancelled during drain — deferring finalizer removal", "name", tr.Name)
			return ctrl.Result{}, ctx.Err()

		case <-ticker.C:
			// Ticker fired — check whether Vector has finished flushing.
			if !drainCheck() {
				logger.Info("Buffers still draining — will retry", "name", tr.Name, "attempt", attempt)
				continue // stay in the loop, wait for the next tick
			}

			// Buffers are empty. Safe to remove the finalizer so Kubernetes
			// can proceed with garbage-collecting the CR and its owned resources.
			logger.Info("Buffers drained — removing finalizer", "name", tr.Name)
			tr.Finalizers = removeString(tr.Finalizers, finalizerName)
			return ctrl.Result{}, r.Update(ctx, tr)
		}
	}
}

// isVectorCrashLooping returns true if any pod in the Vector Deployment for
// this TelemetryRouter has a container in the CrashLoopBackOff waiting state.
// We inspect all pods (not just the latest) because during a rolling update
// the old healthy pod and the new crashing pod coexist.
func (r *TelemetryRouterReconciler) isVectorCrashLooping(
	ctx context.Context, tr *telemetryv1.TelemetryRouter,
) (bool, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(tr.Namespace),
		client.MatchingLabels(vectorLabels(tr.Name)),
	); err != nil {
		return false, err
	}
	for _, pod := range podList.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "vector" &&
				cs.State.Waiting != nil &&
				cs.State.Waiting.Reason == "CrashLoopBackOff" {
				return true, nil
			}
		}
	}
	return false, nil
}

// rollbackConfig restores the last-known-good TOML from the ConfigMap
// annotation written by reconcileConfigMap before the previous overwrite.
// It returns the hash of the restored config so the caller can drive the
// Deployment back to the good state.
func (r *TelemetryRouterReconciler) rollbackConfig(
	ctx context.Context, tr *telemetryv1.TelemetryRouter,
) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      vectorConfigMapName(tr.Name),
		Namespace: tr.Namespace,
	}, cm); err != nil {
		return "", err
	}

	lkgCfg := cm.Annotations[annotationLKGConfig]
	lkgHash := cm.Annotations[annotationLKGHash]
	if lkgCfg == "" {
		return "", fmt.Errorf("no last-known-good snapshot in ConfigMap annotations — cannot roll back")
	}

	// Restore the previous config. We intentionally do NOT clear the LKG
	// annotation here so that a second rollback (if the LKG config itself is
	// somehow also broken) doesn't lose the snapshot entirely.
	cm.Data["vector.toml"] = lkgCfg
	cm.Annotations[annotationConfigHash] = lkgHash
	return lkgHash, r.Update(ctx, cm)
}

func (r *TelemetryRouterReconciler) setStatus(
	ctx context.Context, tr *telemetryv1.TelemetryRouter, phase, msg string,
) error {
	tr.Status.Phase   = phase
	tr.Status.Message = msg
	return r.Status().Update(ctx, tr)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func vectorConfigMapName(routerName string) string {
	return fmt.Sprintf("vector-config-%s", routerName)
}

func vectorDeploymentName(routerName string) string {
	return fmt.Sprintf("vector-%s", routerName)
}

func vectorLabels(routerName string) map[string]string {
	return map[string]string{
		"app":    "vector",
		"router": routerName,
		"managed-by": "telemetry-operator",
	}
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}
