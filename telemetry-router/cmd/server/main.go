// main is the entrypoint for the Telemetry Router control-plane server.
// It wires together three independent subsystems:
//
//  1. REST API server  — handles destination CRUD from customers / UI
//  2. Fan-out engine   — dispatches OTLP records to configured sinks
//  3. Operator         — reconciles TelemetryRouter CRDs with k8s state
//
// In production each subsystem would run as a separate binary / container, but
// this single-binary design makes the demo easy to run and inspect.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"telemetry-router/internal/api"
	"telemetry-router/internal/fanout"
	"telemetry-router/internal/store"
	"telemetry-router/operator"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Println("=== STACKIT Telemetry Router — starting up ===")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// -------------------------------------------------------------------------
	// Shared in-memory store (would be PostgreSQL in production)
	// -------------------------------------------------------------------------
	s := store.New()

	// -------------------------------------------------------------------------
	// Fan-out engine for instance "demo-instance-001"
	// -------------------------------------------------------------------------
	const instanceID = "demo-instance-001"
	engine := fanout.NewEngine(instanceID, s)

	// Register two demo sinks so we can show fan-out working at startup.
	engine.AddSink(ctx, fanout.SinkConfig{
		DestinationID: "dest-otlp-siem",
		Type:          "OTLP",
		Endpoint:      "https://ingest.mysiem.example.com:4317",
	})
	engine.AddSink(ctx, fanout.SinkConfig{
		DestinationID: "dest-s3-archive",
		Type:          "S3",
		BucketName:    "stackit-telemetry-archive",
		Region:        "eu-central-1",
	})

	// -------------------------------------------------------------------------
	// Kubernetes operator (using stub client for demo)
	// -------------------------------------------------------------------------
	k8sClient := &operator.StubK8sClient{
		Secrets: map[string]map[string]string{
			"default/siem-credentials": {
				"api_key": "demo-bearer-token",
			},
			"default/s3-credentials": {
				"access_key_id":     "AKIADEMO",
				"secret_access_key": "demo-secret",
			},
		},
	}
	reconciler := operator.NewReconciler(k8sClient)

	// Trigger an initial reconcile for the demo TelemetryRouter CR.
	demoRouter := &operator.TelemetryRouter{
		Name:      "demo-router",
		Namespace: "default",
		Spec: operator.TelemetryRouterSpec{
			InstanceID: instanceID,
			ProjectID:  "proj-demo-12345",
			Region:     "eu-central-1",
			Replicas:   2,
			Destinations: []operator.DestinationSpec{
				{
					DestinationID:  "dest-otlp-siem",
					Name:           "My SIEM",
					Type:           "OTLP",
					OTLPEndpoint:   "https://ingest.mysiem.example.com:4317",
					SecretRef:      "siem-credentials",
				},
				{
					DestinationID:    "dest-s3-archive",
					Name:             "S3 Archival",
					Type:             "S3",
					S3Bucket:         "stackit-telemetry-archive",
					S3BucketRegion:   "eu-central-1",
					S3Endpoint:       "https://object.storage.eu-central-1.stackit.cloud",
					SecretRef:        "s3-credentials",
				},
			},
		},
	}

	go func() {
		if err := reconciler.Reconcile(ctx, demoRouter); err != nil {
			log.Printf("initial reconcile error: %v", err)
		}
	}()

	// -------------------------------------------------------------------------
	// Demonstrate the fan-out engine by dispatching a test record
	// -------------------------------------------------------------------------
	go func() {
		time.Sleep(200 * time.Millisecond) // let sinks start up
		record := &fanout.LogRecord{
			EventID:    uuid.NewString(),
			InstanceID: instanceID,
			Timestamp:  time.Now().UTC(),
			Body:       "user@example.com logged in from 10.0.0.42",
			Attributes: map[string]string{"k8s.pod.name": "my-app-7d9f8b-xkqz"},
			Resource:   map[string]string{"service.name": "my-app"},
		}
		log.Printf("[demo] dispatching test record: eventId=%s", record.EventID)
		resultCh := engine.Dispatch(record)
		result := <-resultCh
		successes, failures := 0, 0
		for _, r := range result.Results {
			if r.Success {
				successes++
			} else {
				failures++
			}
		}
		log.Printf("[demo] fan-out complete: %d/%d sinks accepted the record",
			successes, successes+failures)
	}()

	// -------------------------------------------------------------------------
	// HTTP server (REST API)
	// -------------------------------------------------------------------------
	addr := envOrDefault("LISTEN_ADDR", ":8080")
	router := api.NewRouter(s)

	srv := &http.Server{
		Addr:         addr,
		Handler:      loggingMiddleware(router),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("[api] listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("api server error: %v", err)
		}
	}()

	// Print example API calls for the demo.
	printExampleRequests(addr, instanceID)

	// -------------------------------------------------------------------------
	// Graceful shutdown
	// -------------------------------------------------------------------------
	<-ctx.Done()
	log.Println("[main] shutdown signal received, draining...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] HTTP server shutdown error: %v", err)
	}
	log.Println("[main] shutdown complete")
}

// loggingMiddleware logs every incoming request.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("[http] %s %s — %dms", r.Method, r.URL.Path,
			time.Since(start).Milliseconds())
	})
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func printExampleRequests(addr, instanceID string) {
	base := fmt.Sprintf("http://localhost%s", addr)
	urlBase := fmt.Sprintf("%s/v2/projects/proj-demo-12345/regions/eu-central-1/instances/%s",
		base, instanceID)

	log.Println()
	log.Println("=== Example API requests ===")
	log.Printf("List destinations:    curl -s '%s/destinations' | jq", urlBase)
	log.Printf("Create OTLP dest:     curl -s -X POST '%s/destinations' \\", urlBase)
	log.Printf("                        -H 'Content-Type: application/json' \\")
	log.Printf(`                        -d '{"name":"My SIEM","type":"OTLP","otlp":{"endpoint":"https://siem.example.com:4317","secretRef":"siem-creds"}}' | jq`)
	log.Printf("Create S3 dest:       curl -s -X POST '%s/destinations' \\", urlBase)
	log.Printf("                        -H 'Content-Type: application/json' \\")
	log.Printf(`                        -d '{"name":"Archive","type":"S3","s3":{"bucket":"my-bucket","region":"eu-central-1","secretRef":"s3-creds"}}' | jq`)
	log.Println()
}
