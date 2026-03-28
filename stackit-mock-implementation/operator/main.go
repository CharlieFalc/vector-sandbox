package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"

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

	// ── Shared state ──────────────────────────────────────────────────────────
	// vectorHealth is written by the health monitor and read by the REST API.
	// Thread-safety is handled inside VectorHealthCache via sync.RWMutex.
	vectorHealth := &api.VectorHealthCache{}

	// ── REST API server (goroutine) ────────────────────────────────────────────
	// Runs alongside the controller manager so `kubectl` and curl-based tools
	// can create/inspect/delete TelemetryRouter CRs via a simple HTTP API.
	apiServer := api.NewServer(mgr.GetClient(), operatorNamespace, vectorHealth)
	ctx := ctrl.SetupSignalHandler()

	// wg tracks the background goroutines so main() can wait for them to finish
	// their cleanup before exiting. Without this, os.Exit (called on manager
	// error) could kill the process while a goroutine is mid-shutdown.
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done() // signal completion when this goroutine returns
		setupLog.Info(fmt.Sprintf("Starting REST API server on %s", apiAddr))
		if err := api.StartAPIServer(ctx, apiAddr, apiServer); err != nil {
			setupLog.Error(err, "REST API server exited")
		}
	}()

	// ── Vector health monitor (goroutine) ─────────────────────────────────────
	// Polls the Vector /health endpoint on a ticker and writes results into
	// vectorHealth so the REST API can surface them without an extra HTTP hop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runVectorHealthMonitor(ctx, setupLog, vectorHealth)
	}()

	// ── Start controller manager (blocks until ctx is cancelled) ─────────────
	setupLog.Info("Starting controller manager",
		"namespace", operatorNamespace,
		"metricsAddr", metricsAddr,
		"apiAddr", apiAddr,
	)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Manager exited")
		// Wait for goroutines before exiting so they can finish any in-flight
		// work (e.g. the API server's graceful HTTP shutdown).
		wg.Wait()
		os.Exit(1)
	}

	// Normal shutdown path: wait for both background goroutines to return
	// before letting the process exit.
	wg.Wait()
	setupLog.Info("All goroutines stopped — operator exiting cleanly")
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runVectorHealthMonitor polls Vector's /health endpoint every 15 seconds,
// writes the result into cache, and logs status changes. It owns its own
// ticker and http.Client so it has no shared mutable state with other goroutines.
//
// The function blocks until ctx is cancelled. Two things can unblock the select:
//   - ticker.C fires   → perform a health check, update the cache, and loop
//   - ctx.Done() fires → operator is shutting down, stop the ticker and return
func runVectorHealthMonitor(ctx context.Context, log logr.Logger, cache *api.VectorHealthCache) {
	// Vector's built-in API server listens on port 8686 by default.
	// In a real deployment this would be the ClusterIP Service DNS name;
	// here we use localhost so the monitor works in a local dev run too.
	const vectorHealthURL = "http://localhost:8686/health"

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop() // always release the ticker when the goroutine exits

	// Timeout covers only the round-trip, not the select wait, so the goroutine
	// is never blocked longer than 3s per iteration even if Vector is slow.
	httpClient := &http.Client{Timeout: 3 * time.Second}
	log.Info("Vector health monitor started", "url", vectorHealthURL)

	for {
		select {
		case <-ctx.Done():
			// The root context was cancelled — operator is shutting down.
			log.Info("Vector health monitor stopping", "reason", ctx.Err())
			return

		case <-ticker.C:
			// Build the request with the current context so that if ctx is
			// cancelled while the HTTP call is in-flight, the request is
			// aborted immediately rather than waiting out the 3s timeout.
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, vectorHealthURL, nil)
			if err != nil {
				log.Error(err, "Failed to build Vector health request")
				cache.Set(false, "failed to build request: "+err.Error())
				continue
			}

			resp, err := httpClient.Do(req)
			if err != nil {
				log.Error(err, "Vector health check failed — is Vector running?")
				cache.Set(false, "request error: "+err.Error())
				continue
			}
			resp.Body.Close()

			healthy := resp.StatusCode == http.StatusOK
			msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
			cache.Set(healthy, msg)

			if healthy {
				log.Info("Vector health check passed", "status", resp.StatusCode)
			} else {
				log.Info("Vector health check returned non-OK status", "status", resp.StatusCode)
			}
		}
	}
}
