package fanout

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"

	"telemetry-router/internal/store"
)

const (
	// perSinkBufferSize is the number of records each sink channel can hold
	// before the dispatcher considers it full and drops (with logging).
	// This isolates a slow sink from backpressuring the ingest pipeline.
	perSinkBufferSize = 10_000

	// maxAttempts is the maximum delivery attempts per record per sink.
	maxAttempts = 5

	// baseBackoff is the initial retry wait duration (doubles each attempt).
	baseBackoff = 1 * time.Second

	// maxBackoff caps the retry wait to avoid very long delays.
	maxBackoff = 16 * time.Second
)

// Engine is the central fan-out coordinator. One Engine runs per Telemetry
// Router instance; it owns a SinkWorker goroutine for each configured sink.
type Engine struct {
	instanceID string
	store      *store.Store

	mu      sync.RWMutex
	workers map[string]*SinkWorker // destinationId → worker
}

// NewEngine creates an Engine. Call AddSink to register destinations before
// calling Dispatch.
func NewEngine(instanceID string, s *store.Store) *Engine {
	return &Engine{
		instanceID: instanceID,
		store:      s,
		workers:    make(map[string]*SinkWorker),
	}
}

// AddSink registers a new destination and starts its background goroutine.
// This is called by the operator reconcile loop when a destination is created
// or updated.
func (e *Engine) AddSink(ctx context.Context, cfg SinkConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Shut down existing worker for this destination if already running
	// (e.g., config update).
	if old, ok := e.workers[cfg.DestinationID]; ok {
		old.stop()
	}

	w := newSinkWorker(cfg, e.store)
	e.workers[cfg.DestinationID] = w
	go w.run(ctx)
	log.Printf("[engine] sink registered: dest=%s type=%s", cfg.DestinationID, cfg.Type)
}

// RemoveSink gracefully drains and stops the worker for a destination.
func (e *Engine) RemoveSink(destinationID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if w, ok := e.workers[destinationID]; ok {
		w.stop()
		delete(e.workers, destinationID)
		log.Printf("[engine] sink removed: dest=%s", destinationID)
	}
}

// Dispatch fans out a single LogRecord to all registered sinks concurrently.
// The call is non-blocking: each sink's channel receives the record (or drops
// it if the sink's buffer is full). The caller gets a result channel it can
// optionally read from to know the aggregate outcome.
func (e *Engine) Dispatch(record *LogRecord) <-chan FanoutResult {
	e.mu.RLock()
	workers := make([]*SinkWorker, 0, len(e.workers))
	for _, w := range e.workers {
		workers = append(workers, w)
	}
	e.mu.RUnlock()

	resultCh := make(chan FanoutResult, 1)

	go func() {
		var wg sync.WaitGroup
		results := make([]DeliveryResult, 0, len(workers))
		var mu sync.Mutex

		for _, w := range workers {
			wg.Add(1)
			go func(worker *SinkWorker) {
				defer wg.Done()
				res := worker.enqueue(record)
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
			}(w)
		}

		wg.Wait()
		resultCh <- FanoutResult{EventID: record.EventID, Results: results}
		close(resultCh)
	}()

	return resultCh
}

// -----------------------------------------------------------------------------
// SinkWorker — one per destination
// -----------------------------------------------------------------------------

// SinkWorker owns a bounded channel and delivers records to one sink with
// retry/backoff.  It runs as a long-lived goroutine managed by the Engine.
type SinkWorker struct {
	cfg     SinkConfig
	store   *store.Store
	ch      chan *LogRecord
	stopCh  chan struct{}
	deliver DeliverFunc // injectable for testing
}

// DeliverFunc is the actual network call to send one record to the sink.
// The real implementation would use the OTLP gRPC client or S3 PutObject.
type DeliverFunc func(ctx context.Context, cfg SinkConfig, record *LogRecord) error

func newSinkWorker(cfg SinkConfig, s *store.Store) *SinkWorker {
	return &SinkWorker{
		cfg:     cfg,
		store:   s,
		ch:      make(chan *LogRecord, perSinkBufferSize),
		stopCh:  make(chan struct{}),
		deliver: defaultDeliver,
	}
}

func (w *SinkWorker) stop() {
	close(w.stopCh)
}

// enqueue attempts a non-blocking send to the sink's channel.
// If the buffer is full, the record is dropped and a FAILED attempt is logged
// (this is a deliberate trade-off: we prefer dropping over backpressuring the
// ingest pipeline or blocking other sinks).
func (w *SinkWorker) enqueue(record *LogRecord) DeliveryResult {
	select {
	case w.ch <- record:
		// Delivered to buffer successfully; actual network call happens in run().
		return DeliveryResult{
			DestinationID: w.cfg.DestinationID,
			EventID:       record.EventID,
			Success:       true, // optimistic: buffer accepted
		}
	default:
		// Buffer full — drop and record.
		log.Printf("[sink:%s] buffer full, dropping event %s", w.cfg.DestinationID, record.EventID)
		attempt := &store.DeliveryAttemptRecord{
			AttemptID:     uuid.NewString(),
			EventID:       record.EventID,
			DestinationID: w.cfg.DestinationID,
			AttemptNumber: 0,
			Status:        "FAILED",
			ErrorMessage:  "sink buffer full: record dropped",
			AttemptedAt:   time.Now().UTC(),
		}
		_ = w.store.RecordAttempt(attempt)
		return DeliveryResult{
			DestinationID: w.cfg.DestinationID,
			EventID:       record.EventID,
			Success:       false,
			Error:         fmt.Errorf("sink buffer full"),
		}
	}
}

// run is the sink's event loop. It reads from its channel and calls
// deliverWithRetry for each record.
func (w *SinkWorker) run(ctx context.Context) {
	log.Printf("[sink:%s] worker started (type=%s)", w.cfg.DestinationID, w.cfg.Type)
	for {
		select {
		case <-w.stopCh:
			log.Printf("[sink:%s] worker stopped", w.cfg.DestinationID)
			return
		case <-ctx.Done():
			log.Printf("[sink:%s] context cancelled, draining channel", w.cfg.DestinationID)
			// Drain remaining records before exiting.
			for len(w.ch) > 0 {
				rec := <-w.ch
				w.deliverWithRetry(ctx, rec)
			}
			return
		case record := <-w.ch:
			w.deliverWithRetry(ctx, record)
		}
	}
}

// deliverWithRetry attempts delivery up to maxAttempts times with exponential
// backoff. Each attempt is recorded in the delivery_attempts table so the
// audit trail is always complete.
func (w *SinkWorker) deliverWithRetry(ctx context.Context, record *LogRecord) {
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		start := time.Now()
		err := w.deliver(ctx, w.cfg, record)
		elapsed := time.Since(start).Milliseconds()

		status := "SUCCESS"
		errMsg := ""
		if err != nil {
			status = "FAILED"
			errMsg = err.Error()
		}

		_ = w.store.RecordAttempt(&store.DeliveryAttemptRecord{
			AttemptID:     uuid.NewString(),
			EventID:       record.EventID,
			DestinationID: w.cfg.DestinationID,
			AttemptNumber: attempt,
			Status:        status,
			ErrorMessage:  errMsg,
			AttemptedAt:   time.Now().UTC(),
			DurationMs:    elapsed,
		})

		if err == nil {
			log.Printf("[sink:%s] delivered event %s (attempt %d, %dms)",
				w.cfg.DestinationID, record.EventID, attempt, elapsed)
			return
		}

		log.Printf("[sink:%s] delivery failed event %s attempt %d/%d: %v",
			w.cfg.DestinationID, record.EventID, attempt, maxAttempts, err)

		if attempt == maxAttempts {
			log.Printf("[sink:%s] giving up on event %s after %d attempts",
				w.cfg.DestinationID, record.EventID, maxAttempts)
			return
		}

		// Exponential backoff: 1s, 2s, 4s, 8s, 16s (capped).
		backoff := time.Duration(math.Min(
			float64(baseBackoff)*math.Pow(2, float64(attempt-1)),
			float64(maxBackoff),
		))

		select {
		case <-time.After(backoff):
			// retry
		case <-ctx.Done():
			log.Printf("[sink:%s] context cancelled during backoff, abandoning event %s",
				w.cfg.DestinationID, record.EventID)
			return
		case <-w.stopCh:
			return
		}
	}
}

// defaultDeliver is the real network call stub.  In production, this would use
// the OTLP gRPC exporter (go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc)
// for OTLP sinks, or aws-sdk-go-v2 PutObject for S3 sinks.
func defaultDeliver(ctx context.Context, cfg SinkConfig, record *LogRecord) error {
	// Simulate network latency.
	time.Sleep(20 * time.Millisecond)

	// Simulate a slow SIEM (30-second response time) — this only blocks this
	// sink's worker goroutine; all other sinks continue unaffected.
	if cfg.Type == "OTLP" && cfg.DestinationID == "slow-siem" {
		time.Sleep(30 * time.Second)
	}

	// Simulate occasional transient failures.
	// In production, treat HTTP 429 / 503 as retryable; 400 / 401 as terminal.
	if record.EventID == "force-fail" {
		return fmt.Errorf("simulated delivery error")
	}

	log.Printf("[deliver] sent event %s to %s (%s)", record.EventID, cfg.DestinationID, cfg.Type)
	return nil
}
