package main

import (
	"flag"
	"fmt"
	"os"

	// Kubernetes / controller-runtime
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	// Our CRD types and controllers
	telemetryv1 "github.com/stackit-mock/operator/api/v1alpha1"
	"github.com/stackit-mock/operator/internal/api"
	"github.com/stackit-mock/operator/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(telemetryv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		apiAddr              string
		enableLeaderElection bool
		operatorNamespace    string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082", "Address for the metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes")
	flag.StringVar(&apiAddr, "api-bind-address", ":8080", "Address for the REST API server")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager")
	flag.StringVar(&operatorNamespace, "namespace", envOrDefault("OPERATOR_NAMESPACE", "telemetry-system"),
		"Namespace the operator manages TelemetryRouter CRs in")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// ── Controller Manager ────────────────────────────────────────────────────
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "telemetry-operator-leader",
	})
	if err != nil {
		setupLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	// ── Reconciler ────────────────────────────────────────────────────────────
	if err = (&controller.TelemetryRouterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "TelemetryRouter")
		os.Exit(1)
	}

	// ── Health / readiness probes ─────────────────────────────────────────────
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	// ── REST API server (goroutine) ────────────────────────────────────────────
	// Runs alongside the controller manager so `kubectl` and curl-based tools
	// can create/inspect/delete TelemetryRouter CRs via a simple HTTP API.
	apiServer := api.NewServer(mgr.GetClient(), operatorNamespace)
	ctx := ctrl.SetupSignalHandler()

	go func() {
		setupLog.Info(fmt.Sprintf("Starting REST API server on %s", apiAddr))
		if err := api.StartAPIServer(ctx, apiAddr, apiServer); err != nil {
			setupLog.Error(err, "REST API server exited")
		}
	}()

	// ── Start controller manager (blocks) ────────────────────────────────────
	setupLog.Info("Starting controller manager",
		"namespace", operatorNamespace,
		"metricsAddr", metricsAddr,
		"apiAddr", apiAddr,
	)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Manager exited")
		os.Exit(1)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
