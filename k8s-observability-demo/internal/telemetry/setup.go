// Package telemetry initialises the OpenTelemetry SDK for the signal-forge app.
//
// Architecture:
//
//	App (OTel SDK)  ──OTLP gRPC──►  OTel Collector  ──OTLP──►  SigNoz
//	                                      │
//	                              tail sampling, batching,
//	                              retry + sending_queue backpressure
//
// The SignalPolicy CRD controls sampling rate and batch size at runtime.
// The operator watches SignalPolicy CRs and patches the OTel Collector ConfigMap,
// which triggers a rolling restart via the checksum annotation strategy.
package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Config holds all tunable knobs that the SignalPolicy CRD can influence.
// The operator reads the active SignalPolicy and injects these as env vars.
type Config struct {
	// OTLPEndpoint is the OTel Collector gRPC address (host:port).
	// Default: "otel-collector:4317"
	OTLPEndpoint string

	// ServiceName is the OTel service.name resource attribute.
	ServiceName string

	// ServiceVersion is the OTel service.version resource attribute.
	ServiceVersion string

	// SamplingRatio is the head-based TraceIDRatioBased sampler rate [0.0, 1.0].
	// The OTel Collector tail_sampling processor applies a second, smarter pass.
	// Default: 1.0 (sample everything — let Collector decide)
	SamplingRatio float64

	// BatchTimeout is how long the SDK batches spans before flushing.
	// Lower = lower latency but more network overhead.
	// Default: 5s
	BatchTimeout time.Duration

	// MaxExportBatchSize is the maximum number of spans per export call.
	// Maps to the SDK BatchSpanProcessor MaxExportBatchSize option.
	// Default: 512
	MaxExportBatchSize int

	// MaxQueueSize is the bounded in-memory span queue.
	// When full, NEW spans are dropped (head-drop) — prevents OOM.
	// Default: 2048
	MaxQueueSize int
}

// DefaultConfig returns sensible defaults suitable for development / low-traffic.
func DefaultConfig() Config {
	return Config{
		OTLPEndpoint:       "otel-collector:4317",
		ServiceName:        "signal-forge",
		ServiceVersion:     "0.1.0",
		SamplingRatio:      1.0,
		BatchTimeout:       5 * time.Second,
		MaxExportBatchSize: 512,
		MaxQueueSize:       2048,
	}
}

// SDK holds the initialised providers and exposes named instruments.
type SDK struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	LogProvider    *sdklog.LoggerProvider

	// Logger returns an OTel logger for the given scope.
	// Use this to emit log records with trace context via the OTel SDK.
	Logger func(scope string) otellog.Logger

	// Pre-created instruments used by the HTTP handlers.
	RequestCounter  metric.Int64Counter
	RequestDuration metric.Float64Histogram
	ActiveRequests  metric.Int64UpDownCounter

	tracer trace.Tracer
}

// Tracer returns the named tracer for this service.
func (s *SDK) Tracer() trace.Tracer { return s.tracer }

// Init bootstraps the OTel SDK and registers global providers.
// The returned SDK must be shut down via Shutdown() to flush pending telemetry.
func Init(ctx context.Context, cfg Config) (*SDK, error) {
	// ------------------------------------------------------------------
	// gRPC connection to OTel Collector
	// ------------------------------------------------------------------
	conn, err := grpc.NewClient(
		cfg.OTLPEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", cfg.OTLPEndpoint, err)
	}

	// ------------------------------------------------------------------
	// Resource — identifies THIS service in every signal
	// ------------------------------------------------------------------
	// Use "" schema URL to avoid conflicts with resource.Default() which uses a
	// different semconv version than our v1.24.0 import. The attribute names
	// (service.name, service.version, etc.) are stable across semconv versions.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			"",
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			// Custom attribute — useful for multi-tenant Collector routing.
			semconv.DeploymentEnvironment("kubernetes"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	// ------------------------------------------------------------------
	// Traces
	// ------------------------------------------------------------------
	traceExp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}

	// Head-based sampler — fast, no context needed.
	// The OTel Collector tail_sampling processor makes smarter decisions
	// AFTER seeing the full trace (e.g. keep all error spans, drop 90% of
	// success spans).
	sampler := sdktrace.TraceIDRatioBased(cfg.SamplingRatio)

	// BatchSpanProcessor is the backpressure valve on the SDK side:
	//   - MaxQueueSize  → drops new spans when queue is full (vs blocking caller)
	//   - BatchTimeout  → flush cadence
	//   - MaxExportBatchSize → caps single export RPC payload
	bsp := sdktrace.NewBatchSpanProcessor(
		traceExp,
		sdktrace.WithMaxQueueSize(cfg.MaxQueueSize),
		sdktrace.WithBatchTimeout(cfg.BatchTimeout),
		sdktrace.WithMaxExportBatchSize(cfg.MaxExportBatchSize),
		// ExportTimeout: if Collector is slow, don't block forever.
		sdktrace.WithExportTimeout(10*time.Second),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sampler),
		sdktrace.WithSpanProcessor(bsp),
		sdktrace.WithResource(res),
	)

	// ------------------------------------------------------------------
	// Metrics
	// ------------------------------------------------------------------
	metricExp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(metricExp,
				sdkmetric.WithInterval(15*time.Second),
			),
		),
		sdkmetric.WithResource(res),
	)

	// ------------------------------------------------------------------
	// Register globals (used by otelhttp middleware automatically)
	// ------------------------------------------------------------------
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// ------------------------------------------------------------------
	// Pre-create named instruments
	// ------------------------------------------------------------------
	meter := mp.Meter(cfg.ServiceName)

	reqCounter, err := meter.Int64Counter(
		"http.server.request.total",
		metric.WithDescription("Total HTTP requests handled"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("request counter: %w", err)
	}

	reqDuration, err := meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithDescription("HTTP request latency"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("request duration histogram: %w", err)
	}

	activeReqs, err := meter.Int64UpDownCounter(
		"http.server.active_requests",
		metric.WithDescription("Currently in-flight HTTP requests"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("active requests gauge: %w", err)
	}

	// ------------------------------------------------------------------
	// Logs
	// ------------------------------------------------------------------
	logExp, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("log exporter: %w", err)
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	global.SetLoggerProvider(lp)

	return &SDK{
		TracerProvider:  tp,
		MeterProvider:   mp,
		LogProvider:     lp,
		RequestCounter:  reqCounter,
		RequestDuration: reqDuration,
		ActiveRequests:  activeReqs,
		tracer:          tp.Tracer(cfg.ServiceName),
		Logger:          func(scope string) otellog.Logger { return lp.Logger(scope) },
	}, nil
}

// Shutdown flushes all pending telemetry and releases connections.
// Call this in your graceful-shutdown path with a timeout context.
func (s *SDK) Shutdown(ctx context.Context) {
	if err := s.TracerProvider.Shutdown(ctx); err != nil {
		fmt.Printf("trace provider shutdown: %v\n", err)
	}
	if err := s.MeterProvider.Shutdown(ctx); err != nil {
		fmt.Printf("metric provider shutdown: %v\n", err)
	}
	if s.LogProvider != nil {
		if err := s.LogProvider.Shutdown(ctx); err != nil {
			fmt.Printf("log provider shutdown: %v\n", err)
		}
	}
}
