// signal-forge is the demo application for the k8s-observability-demo.
//
// It emits all three OTel signal types:
//   - Traces  → via OTel SDK BatchSpanProcessor → OTLP gRPC → OTel Collector → SigNoz
//   - Metrics → via OTel SDK PeriodicReader      → OTLP gRPC → OTel Collector → SigNoz
//   - Logs    → via zerolog (JSON to stdout)      → Vector DaemonSet → SigNoz
//
// The SignalPolicy CRD controls sampling/batching/backpressure at runtime.
// The operator watches SignalPolicy CRs and patches the OTel Collector ConfigMap.
//
// Quick start (Docker Desktop Kubernetes):
//
//	kubectl apply -k k8s/
//	kubectl port-forward svc/signal-forge 8080:8080 -n observability-demo
//	open http://localhost:8080/docs
package main

import (
	"context"
	"embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"signal-forge/internal/api"
	"signal-forge/internal/telemetry"
)

//go:embed openapi.yaml
var specFS embed.FS

func main() {
	// ------------------------------------------------------------------
	// Structured JSON logging (zerolog → stdout → Vector → SigNoz)
	// ------------------------------------------------------------------
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if os.Getenv("LOG_LEVEL") == "debug" {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	// Write structured JSON to stdout — Vector collects from /var/log/pods
	logger := zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "signal-forge").
		Logger()
	zlog.Logger = logger

	// ------------------------------------------------------------------
	// OTel SDK — read config from env (injected by SignalPolicy operator)
	// ------------------------------------------------------------------
	samplingRatio := 1.0
	if v := os.Getenv("OTEL_SAMPLING_RATIO"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			samplingRatio = f
		}
	}
	batchTimeout := 5 * time.Second
	if v := os.Getenv("OTEL_BATCH_TIMEOUT_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil {
			batchTimeout = time.Duration(ms) * time.Millisecond
		}
	}
	maxQueueSize := 2048
	if v := os.Getenv("OTEL_MAX_QUEUE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxQueueSize = n
		}
	}

	cfg := telemetry.Config{
		OTLPEndpoint:       envOrDefault("OTEL_EXPORTER_OTLP_ENDPOINT", "otel-collector:4317"),
		ServiceName:        envOrDefault("OTEL_SERVICE_NAME", "signal-forge"),
		ServiceVersion:     envOrDefault("SERVICE_VERSION", "0.1.0"),
		SamplingRatio:      samplingRatio,
		BatchTimeout:       batchTimeout,
		MaxExportBatchSize: 512,
		MaxQueueSize:       maxQueueSize,
	}

	ctx := context.Background()
	sdk, err := telemetry.Init(ctx, cfg)
	if err != nil {
		log.Fatalf("otel init: %v", err)
	}

	zlog.Info().
		Str("otlp_endpoint", cfg.OTLPEndpoint).
		Float64("sampling_ratio", cfg.SamplingRatio).
		Dur("batch_timeout", cfg.BatchTimeout).
		Int("max_queue_size", cfg.MaxQueueSize).
		Msg("OTel SDK initialised")

	// ------------------------------------------------------------------
	// HTTP handlers + OTel middleware
	// ------------------------------------------------------------------
	store := api.NewWidgetStore()
	h := api.New(store, sdk)

	mux := http.NewServeMux()
	api.RegisterRoutes(mux, h)

	// OpenAPI spec
	mux.HandleFunc("GET /api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		data, err := specFS.ReadFile("openapi.yaml")
		if err != nil {
			http.Error(w, "spec not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(data)
	})

	// Swagger UI (CDN, no extra deps)
	mux.HandleFunc("GET /docs", swaggerUIHandler)
	mux.HandleFunc("GET /docs/", swaggerUIHandler)

	// Health / readiness
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})

	// otelLoggingMiddleware is placed INSIDE otelhttp so that the span is
	// already active in the context when we attach the OTel log hook.
	// This enables zerolog → OTel log SDK bridging with proper TraceId/SpanId.
	innerMux := otelLoggingMiddleware(sdk.Logger("signal-forge"))(mux)

	// Wrap the entire mux with otelhttp — this creates the root span for
	// every request automatically, propagates W3C TraceContext headers,
	// and calls the global MeterProvider to record http.server.* metrics.
	handler := otelhttp.NewHandler(innerMux, "signal-forge",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	port := envOrDefault("PORT", "8080")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      loggingMiddleware(handler),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		zlog.Info().
			Str("addr", "http://localhost:"+port).
			Msg("=== signal-forge starting ===")
		zlog.Info().Msgf("  API:    http://localhost:%s/api/v1/widgets", port)
		zlog.Info().Msgf("  Docs:   http://localhost:%s/docs", port)
		zlog.Info().Msgf("  Health: http://localhost:%s/health", port)
		zlog.Info().Msg("")
		zlog.Info().Msg("Simulate signals:")
		zlog.Info().Msgf("  slow trace:  curl http://localhost:%s/api/v1/simulate/slow", port)
		zlog.Info().Msgf("  error trace: curl http://localhost:%s/api/v1/simulate/error", port)

		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	// ------------------------------------------------------------------
	// Background metrics ticker — emits a "heartbeat" gauge every 30s
	// so SigNoz always has a signal even when there's no HTTP traffic.
	// ------------------------------------------------------------------
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sdk.ActiveRequests.Add(ctx, 0) // force a read so the gauge appears
				zlog.Debug().Msg("heartbeat metric tick")
			}
		}
	}()

	// ------------------------------------------------------------------
	// Graceful shutdown
	// ------------------------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	zlog.Info().Msg("shutdown signal received, draining...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		zlog.Error().Err(err).Msg("HTTP server shutdown error")
	}

	// Flush all pending spans / metric points before exiting.
	sdk.Shutdown(shutCtx)
	zlog.Info().Msg("shutdown complete")
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lrw, r)
		zlog.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", lrw.status).
			Int64("latency_ms", time.Since(start).Milliseconds()).
			Msg("http")
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.status = code
	lrw.ResponseWriter.WriteHeader(code)
}

func swaggerUIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>signal-forge API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>body{margin:0;background:#fafafa;}.topbar{background:#1b1b2f!important;}</style>
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  SwaggerUIBundle({
    url: "/api/openapi.yaml",
    dom_id: '#swagger-ui',
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
    layout: "BaseLayout",
    deepLinking: true,
    tryItOutEnabled: true,
  });
</script>
</body>
</html>`)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// otelLogHook bridges zerolog events to the OTel log SDK so that log records
// arrive at SigNoz via OTLP with TraceId/SpanId set at the record level,
// enabling trace-log correlation in the SigNoz UI.
type otelLogHook struct {
	logger  otellog.Logger
	spanCtx trace.SpanContext
}

func (h *otelLogHook) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	if !h.spanCtx.IsValid() {
		return
	}
	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	rec.SetBody(otellog.StringValue(msg))
	rec.SetSeverity(zerologToOTelSeverity(level))
	rec.SetSeverityText(level.String())
	// Pass a context carrying the span so the SDK sets TraceId/SpanId on the OTLP record.
	ctx := trace.ContextWithSpanContext(context.Background(), h.spanCtx)
	h.logger.Emit(ctx, rec)
}

func zerologToOTelSeverity(l zerolog.Level) otellog.Severity {
	switch l {
	case zerolog.TraceLevel:
		return otellog.SeverityTrace
	case zerolog.DebugLevel:
		return otellog.SeverityDebug
	case zerolog.InfoLevel:
		return otellog.SeverityInfo
	case zerolog.WarnLevel:
		return otellog.SeverityWarn
	case zerolog.ErrorLevel:
		return otellog.SeverityError
	case zerolog.FatalLevel, zerolog.PanicLevel:
		return otellog.SeverityFatal
	default:
		return otellog.SeverityUndefined
	}
}

// otelLoggingMiddleware injects an otelLogHook into the zerolog context logger
// for each request. Must run INSIDE otelhttp so the span is already active.
func otelLoggingMiddleware(l otellog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			spanCtx := trace.SpanFromContext(r.Context()).SpanContext()
			hook := &otelLogHook{logger: l, spanCtx: spanCtx}
			ctx := zerolog.Ctx(r.Context()).Hook(hook).WithContext(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
