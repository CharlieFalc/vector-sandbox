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
	vectorImage   = "timberio/vector:0.42.0-distroless-libc"
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
	tr.Status.Phase            = "Ready"
	tr.Status.VectorConfigHash = cfgHash
	tr.Status.Message          = "Vector deployment running"
	tr.Status.LastReconcileAt  = time.Now().UTC().Format(time.RFC3339)
	if err := r.Status().Update(ctx, &tr); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Reconcile complete", "name", tr.Name, "hash", cfgHash)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileConfigMap ensures the Vector TOML ConfigMap exists and is up-to-date.
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
					"telemetry.stackit.local/config-hash": hash,
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

	// Update if hash changed
	if cm.Annotations["telemetry.stackit.local/config-hash"] != hash {
		cm.Data["vector.toml"] = cfg
		cm.Annotations["telemetry.stackit.local/config-hash"] = hash
		return r.Update(ctx, cm)
	}
	return nil
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

	// Update replicas and config hash annotation to trigger rolling update
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
	logger.Info("Handling deletion — draining finalizer", "name", tr.Name)

	// TODO: in production, wait for in-flight events to flush before removing
	tr.Finalizers = removeString(tr.Finalizers, finalizerName)
	return ctrl.Result{}, r.Update(ctx, tr)
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
