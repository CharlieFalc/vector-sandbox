# Jansen Call — Deep Dive Study Guide

> Source: raw notes from the call with Jansen (Datadog Observability Pipelines team).
> Every bullet from the original notes is expanded here with context, examples, and interview framing.

---

## Table of Contents

1. [Vector Architecture — The Single Binary Model](#1-vector-architecture--the-single-binary-model)
2. [VRL — Vector Remap Language](#2-vrl--vector-remap-language)
3. [Vector Limitations You Need to Know Cold](#3-vector-limitations-you-need-to-know-cold)
4. [Disk Buffers & RKYV Serialization](#4-disk-buffers--rkyv-serialization)
5. [Tokio — Vector's Async Runtime](#5-tokio--vectors-async-runtime)
6. [End-to-End Acknowledgements & the Batch Notifier Pattern](#6-end-to-end-acknowledgements--the-batch-notifier-pattern)
7. [Backpressure & Event-Driven Architecture (EDA)](#7-backpressure--event-driven-architecture-eda)
8. [OTel / Datadog Integration — 4 Ingestion Modes](#8-otel--datadog-integration--4-ingestion-modes)
9. [OTel Collector Deployment Modes](#9-otel-collector-deployment-modes)
10. [Observing Vector Itself](#10-observing-vector-itself)
11. [Scaling Vector in Kubernetes — The HPA Problem](#11-scaling-vector-in-kubernetes--the-hpa-problem)
12. [Sensitive Data Scanning](#12-sensitive-data-scanning)
13. [Poison Payloads & Error Isolation](#13-poison-payloads--error-isolation)
14. [Quick Reference — Interview Answer Skeletons](#14-quick-reference--interview-answer-skeletons)

---

## 1. Vector Architecture — The Single Binary Model

### What Jansen said
> "Vector on its own — all components are defined in the same binary."
> "Components are part of the main Vector binary so you need to choose at compile time what components to use."

### What this actually means

Vector is a compiled Rust binary. Unlike something like Logstash (which is JVM-based and loads plugins at runtime from the filesystem), Vector bakes all its sources, transforms, and sinks into one statically-linked executable at compile time.

**The three primitive types in Vector:**

| Primitive | Role | Examples |
|---|---|---|
| **Source** | Ingests events into Vector | `kafka`, `http`, `kubernetes_logs`, `otlp`, `file`, `stdin` |
| **Transform** | Mutates, filters, routes events | `remap` (VRL), `filter`, `route`, `aggregate`, `lua` |
| **Sink** | Delivers events to a downstream system | `datadog_logs`, `http`, `s3`, `kafka`, `otlp`, `prometheus_exporter` |

A **topology** is a directed graph connecting these. In `vector.toml`:

```toml
[sources.my_http]
  type = "http_server"
  address = "0.0.0.0:8080"

[transforms.parse]
  type = "remap"
  inputs = ["my_http"]
  source = '''
    . = parse_json!(string!(.message))
  '''

[sinks.datadog]
  type = "datadog_logs"
  inputs = ["parse"]
  default_api_key = "${DD_API_KEY}"
```

### Why compile-time matters

**Benefit:** No runtime plugin loading = zero cold-start overhead, smaller attack surface, guaranteed ABI compatibility between components.

**Tradeoff:** If a customer needs a source that isn't in the binary they're running, they need a custom build. Datadog ships a curated binary with a specific set of components. If you want a niche source (say, a proprietary internal queue), you'd need to compile your own Vector binary including that custom component — which Jansen's team supports via the Vector component SDK.

**Interview framing:** *"Because all components are compiled in, Vector's binary is self-contained and predictable — you don't have a Logstash situation where a plugin upgrade can break the pipeline at 2am. The tradeoff is you can't hot-add a new source without a binary release, which is why Datadog's release cadence for OP is important."*

### Creating custom components

Jansen mentioned: *"Component in Vector — can create your own sinks/transforms."*

Custom components are written in Rust and implement the `Source`, `Transform`, or `Sink` traits from Vector's component SDK. They get compiled directly into the binary. This is how Datadog adds proprietary components (like the DD Agent integration shims) without upstreaming everything to the public Vector repo.

---

## 2. VRL — Vector Remap Language

### What Jansen said
> "VRL — Vector Remap Language, allows you to do operations on a value, allows you to transform certain values."

### What VRL actually is

VRL is a **domain-specific language** (DSL) purpose-built for transforming observability events. It's:

- **Sandboxed** — no file I/O, no network calls, no arbitrary code execution
- **Typed** — has a type system with fallible operations (the `!` and `?` operators)
- **Compiled to bytecode** — runs fast because VRL programs are compiled once at Vector startup, not interpreted per-event
- **Purely functional on a single event** — VRL operates on `.` (the current event) and returns either a mutated event or an error

### Syntax crash course

```vrl
# Parse JSON from the message field
. = parse_json!(.message)

# Add a new field
.environment = "production"

# Conditionally transform
if .level == "ERROR" {
  .severity = "high"
  .alert = true
}

# Delete a field
del(.raw_bytes)

# Type coercion (fallible — ! aborts on error, ? returns null)
.timestamp = to_timestamp!(.ts, format: "%Y-%m-%dT%H:%M:%SZ")

# Regex match and capture
structured, err = parse_regex(.message, r'^(?P<ip>\d+\.\d+\.\d+\.\d+) (?P<method>\w+)')
if err == null {
  .client_ip = structured.ip
  .http_method = structured.method
}

# PII redaction (mask email addresses)
.message = redact(.message, filters: [
  { type: "pattern", pattern: r'[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}' }
])
```

### The `!` vs `?` vs `??` operators — why they matter

VRL has fallible functions (ones that can fail, like `parse_json` when the input isn't valid JSON). You must explicitly decide what to do on failure:

| Operator | Meaning | Use when |
|---|---|---|
| `parse_json!(str)` | **Abort** the program on failure, drop the event | You're certain the input is always valid |
| `parse_json?(str)` | Return `null` on failure, continue | You want to tolerate bad input gracefully |
| `parse_json(str) ?? {}` | Return the fallback `{}` on failure | You want a default value |

This is a common interview topic because it directly affects pipeline reliability. If you use `!` everywhere and a malformed event hits, VRL aborts processing of that event and it gets routed to the component's error output (or dropped if none is configured).

### Where VRL hits its limits (what Jansen said about Lua)

> "Lua — scripting language" (mentioned as an alternative when VRL can't do something)

VRL's sandbox is intentional but limiting. Things you **cannot** do in VRL:
- Make HTTP calls (can't call an external enrichment API mid-transform)
- Read from the filesystem
- Maintain state across events (VRL is stateless per-event — no "running total" pattern)
- Complex algorithmic logic (no loops over arbitrary-length structures without functional primitives)
- Call external libraries

When customers hit these walls, the options are:
1. **`lua` transform** — full Lua scripting environment, stateful, can maintain data between events, but slower (GC overhead, interpreted) and less safe
2. **Custom Rust component** — compiled in, zero overhead, but requires a binary rebuild
3. **`wasm` transform** (experimental) — WebAssembly modules, sandboxed but more capable than VRL

**Interview framing:** *"VRL is intentionally constrained — its safety properties come from what it can't do. When I hit the wall, the question is whether the complexity belongs in Vector at all, or whether the data should be enriched at a different layer — e.g., the consuming service queries a reference database itself rather than asking Vector to do it."*

---

## 3. Vector Limitations You Need to Know Cold

### 3a. No HPA (Horizontal Pod Autoscaler) Support

> "No HPA within Vector — one of the weak points with Vector."
> "Need to architect this yourself."
> "Customers will run Vector on hundreds of thousands of pods."

**Why HPA doesn't work with Vector:**

HPA (Kubernetes Horizontal Pod Autoscaler) scales a Deployment by adding/removing pod replicas based on CPU, memory, or custom metrics. It works great for stateless services. Vector breaks HPA for two reasons:

**1. Disk buffers are stateful and node-local.**
If Vector is running as a DaemonSet (one pod per node, collecting `/var/log/pods`), you can't HPA a DaemonSet — it's already one-per-node. And even in aggregator mode with a Deployment, if a pod has a disk buffer with undelivered events and HPA decides to scale it down, those events are lost unless you explicitly drain first.

**2. In-flight events.**
Unlike a stateless HTTP server where a dying pod just stops getting routed traffic, a dying Vector pod has events sitting in memory or disk buffers. HPA doesn't know about this — it just kills the pod.

**The real-world pattern Jansen's team uses:**

For DaemonSet (agent mode) — HPA doesn't apply, it's 1:1 with nodes. Scale the nodes.

For aggregator mode — use **manual scaling with drain logic**:
```bash
# Before scaling down, send SIGTERM to trigger graceful shutdown
# Vector will flush in-flight events before exiting
kill -SIGTERM <vector-pid>
```

Or use a **StatefulSet** instead of a Deployment for aggregators:
- Gives stable pod identities (useful for disk buffer paths)
- Allows graceful rolling updates with `podManagementPolicy: OrderedReady`
- Still no auto-scaling, but at least scale-down is predictable

**Throughput-based scaling instead:**
Instead of HPA, size Vector instances based on expected throughput at design time. Run load tests to find the saturation point for your component configuration, then set `resources.requests` appropriately and use static replica counts.

### 3b. Diminishing returns on scaling

> "You get diminishing returns on Vector with how much you scale it."

This is a nuanced point. Vector's internal pipeline is parallelized via Tokio (async multi-threading), so it can use multiple CPU cores efficiently. But there are bottlenecks:

- **Single-threaded components:** Some transforms (particularly `lua`) are single-threaded. Adding more CPU doesn't help if the bottleneck is a Lua transform.
- **Network/disk I/O ceilings:** A single Vector instance is often bottlenecked on the sink's network bandwidth or the disk's IOPS, not CPU. Adding more Vector replicas sharing the same disk or network interface doesn't help.
- **Kafka partition limit:** If your source is Kafka, Vector's parallelism is bounded by the number of partitions. 8 Kafka partitions → at most 8 parallel consumers, regardless of how many Vector replicas you run.

**The right mental model:** Profile which component is the bottleneck first. If it's CPU on a VRL-heavy transform, more replicas help. If it's sink throughput (writing to S3 with a rate limit), more replicas don't help — you need to tune the sink or negotiate a higher rate limit.

### 3c. No native Kubernetes operator

> "No native way to run it in Kubernetes — all needs to be built out by customer."

Vector doesn't ship with a Kubernetes operator or a CRD. There's a Helm chart, but it's a generic chart — it doesn't provide:
- A `VectorPipeline` CRD that you can apply to configure the topology
- Automatic rolling restarts when the config ConfigMap changes
- Status subresources to reflect pipeline health

This is exactly the gap that the STACKIT Telemetry Router fills — and what we built in the `k8s-observability-demo` SignalPolicy operator. In production, teams solve this with:
- **The ConfigMap checksum annotation pattern** (what we implemented): hash the Vector config, annotate the Deployment, Kubernetes triggers rolling restart on change
- **Vector's `--watch-config` flag:** Vector can reload its config file in-place without restarting, but this only works if the config change doesn't require topology reconstruction

---

## 4. Disk Buffers & RKYV Serialization

### What Jansen said
> "Disk pressure — RKYV — serialize directly from a Rust struct, just directly puts it on disk."
> "Flushes the disk every .5s OR if you hit max size."

### What RKYV is

**RKYV** (pronounced "archive") is a **zero-copy deserialization framework for Rust**. "Zero-copy" means that when you read RKYV data back from disk, you don't have to deserialize it into a new heap-allocated struct — you just cast a pointer at the bytes on disk. The bytes *are* the struct.

Traditional serialization (JSON, MessagePack, Protobuf):
```
In-memory struct → serialize → bytes on disk
bytes on disk → deserialize → copy into new in-memory struct
```

RKYV:
```
In-memory struct → write raw memory layout → bytes on disk
bytes on disk → cast pointer → in-memory struct (zero allocation, zero copy)
```

**Why Vector uses RKYV for disk buffers:**

The disk buffer is a high-frequency path — events hit it continuously in high-throughput pipelines. Using JSON or Protobuf would add significant CPU overhead per event (allocation, encoding, decoding). RKYV makes the write path nearly as fast as just `memcpy`-ing the struct to disk.

**The flush behavior:**

> "Flushes the disk every 0.5s OR if you hit max size."

Vector's disk buffer is a memory-mapped write-ahead log. Events accumulate in an in-memory write buffer and are flushed to disk:
- Every **500ms** (configurable, this is the default)
- Immediately when the write buffer reaches `max_size`

This 500ms window means there's a small durability gap: if Vector crashes in the middle of a 500ms window, events written since the last flush are lost. For most use cases this is acceptable — the alternative (fsync on every event) would destroy throughput.

**Configuration:**
```toml
[sinks.my_sink]
  type = "datadog_logs"
  inputs = ["parse"]

  [sinks.my_sink.buffer]
    type = "disk"
    max_size = 268435456  # 256MB
    when_full = "drop_newest"  # or "block"
```

**`when_full` policies — know both cold:**

| Policy | Behavior | Use when |
|---|---|---|
| `drop_newest` | New incoming events are dropped when buffer is full | Low-latency pipelines where fresh data beats completeness |
| `block` | The upstream component blocks (pauses) until space frees | Zero-loss pipelines where you'd rather slow the source than drop data |

`block` propagates backpressure all the way up to the source. If the source is `http_server`, incoming HTTP requests will start queuing/timing out. If the source is `kafka`, Vector stops committing offsets — Kafka stops delivering more messages. This is the correct behavior for a "never drop a log" requirement, at the cost of upstream latency.

**Memory buffer for comparison:**
```toml
  [sinks.my_sink.buffer]
    type = "memory"
    max_events = 10000
    when_full = "drop_newest"
```
Memory buffers are faster (no disk I/O) but not durable. If the pod restarts, everything in the buffer is gone. End-to-end acknowledgements won't help here because the data never made it to disk.

---

## 5. Tokio — Vector's Async Runtime

### What Jansen said
> "Tokio — gold standard for async multi-threading library."

### What Tokio is and why it matters for Vector

**Tokio** is the dominant async runtime for Rust. It implements an async task scheduler backed by a thread pool, allowing thousands of concurrent tasks to run on a small number of OS threads without blocking.

**The core idea:**

In a traditional synchronous server, each connection gets its own OS thread. OS threads are expensive (~8MB stack each) — a server handling 10,000 concurrent connections needs 10,000 threads, which creates scheduling overhead.

Tokio's model: a fixed pool of worker threads (typically one per CPU core) plus a non-blocking I/O poller (epoll on Linux). When a task is waiting on I/O (e.g., waiting for the next Kafka message), it yields the thread back to the pool rather than blocking. When the I/O is ready, the task is woken and a worker thread picks it up.

```
Tokio Runtime
├── Worker thread 0 ──► runs task A (VRL transform) → task A awaits sink write → switches to task B
├── Worker thread 1 ──► runs task C (Kafka consumer) → C awaits next message → switches to task D
├── Worker thread 2 ──► runs task B (disk buffer write) → completes → notifies task A
└── I/O thread ──────► epoll loop, wakes tasks when their I/O is ready
```

**Why this matters for Vector:**

Every Vector source, transform, and sink runs as a Tokio task. A single Vector process can have hundreds of concurrent tasks running efficiently on a handful of threads. This is how Vector handles high fan-out (one source → many sinks) without spawning hundreds of OS threads.

**The caveat — blocking operations:**

Tokio's worker threads must never block. If a task does a blocking operation (like a synchronous `std::fs::File::read`), it blocks the entire worker thread, starving all other tasks on that thread.

This is why:
- VRL transforms are designed to be non-blocking (no I/O)
- Lua transforms run on a separate dedicated thread (Lua's GC and blocking behavior would poison a Tokio worker)
- Disk buffer writes use Tokio's `spawn_blocking` to offload to a separate thread pool

**Interview framing:** *"Tokio lets Vector multiplex thousands of pipeline tasks onto a small fixed thread pool. The performance win vs. a thread-per-connection model is roughly 10-100x in memory usage and scheduling efficiency. The constraint is that all transforms must be non-blocking — which is part of why VRL is sandboxed and Lua runs on a separate thread."*

---

## 6. End-to-End Acknowledgements & the Batch Notifier Pattern

### What Jansen said
> "Only time events are used are for e2e acknowledgements — to guarantee."
> "Batch notifier / end to end."

### What end-to-end acknowledgements (e2e acks) are

By default, Vector uses a "fire and forget" delivery model: a source ingests an event, hands it off to the topology, and immediately acknowledges it to the upstream (commits a Kafka offset, sends HTTP 200, etc.). If Vector crashes after the ack but before the sink delivers, the event is lost.

**End-to-end acknowledgements flip this:** Vector doesn't acknowledge the source until the event has been successfully delivered by the sink (or written to a durable disk buffer that will eventually deliver it).

This is the difference between **at-most-once** and **at-least-once** delivery.

### The Batch Notifier pattern (how it's implemented)

This is the mechanism Jansen was referring to. Here's how it works:

```
Source receives batch of events
         │
         ▼
Source creates a BatchNotifier {
  source_half: stays with the source
  events_half: attached to each event in the batch
}
         │
         ▼ events flow through topology
         │
[Transform 1] → [Transform 2] → [Disk Buffer] → [Sink]
         │
         ▼ Sink delivers to downstream
         │
Sink captures response status (success/failure)
         │
         ▼ updates BatchNotifier.events_half with status
         │
BatchNotifier detects all events in batch are settled
         │
         ▼ notifies source_half
         │
Source propagates acknowledgement upstream:
  - Kafka source: commits offset
  - HTTP source: sends 200 response
  - File source: advances read cursor
```

**The key insight:** The BatchNotifier is dropped when all events in the batch are settled. The source holds the `source_half` and is notified via a channel when this happens. This is why Jansen said "only time events are used are for e2e acknowledgements" — the event object itself carries the `events_half` of the notifier, which is how the settlement signal propagates back to the source.

**Practical implication:**

If you enable e2e acks and use an in-memory buffer, you get a false sense of security — the sink acks the source once events hit the in-memory buffer, which is not durable. For true durability you need e2e acks + disk buffer:

```toml
[sources.kafka]
  type = "kafka"
  acknowledgements.enabled = true  # don't commit offset until sink delivers

[sinks.datadog_logs]
  acknowledgements.enabled = true

  [sinks.datadog_logs.buffer]
    type = "disk"  # events survive pod restart
    max_size = 268435456
```

**Vector upgrade without dropping events (Jansen's Q2):**

With e2e acks + disk buffer, the upgrade flow is:
1. Send SIGTERM to the old Vector pod
2. Vector stops accepting new events from the source
3. Vector drains the disk buffer — delivers remaining events to the sink
4. Once sink acks all events, Vector updates source (commits Kafka offsets)
5. Old pod exits cleanly
6. New pod starts, resumes from the committed offset

Without disk buffer: there's always a gap between SIGTERM and the last committed offset. The new pod will re-process a small window of events (at-least-once, not exactly-once).

---

## 7. Backpressure & Event-Driven Architecture (EDA)

### What Jansen said
> "Split arrays and many events at once, the EDA can make sure everything goes where it needs to."
> "OTel collectors have different ways you can run them. Default modes can promote backpressure, the alternatives can help reduce these instances i.e. gateway mode — forwards to a centralized collector/gateway that will handle disk/memory buffers."

### Backpressure propagation in Vector

Backpressure is the signal that flows **upstream** when a downstream component is overwhelmed. In Vector:

```
Source → Transform → Sink (slow/down)
                 ←── backpressure signal ───
```

When the sink's buffer is full (`when_full = "block"`):
1. The sink stops consuming from the transform's output channel
2. The transform's output channel fills up
3. The transform blocks and stops consuming from the source's output channel
4. The source's output channel fills up
5. The source blocks and stops consuming from the upstream (pauses Kafka consumption, stops accepting HTTP connections)

This is **natural backpressure** — the slow downstream propagates upstream without any explicit signaling. It works because everything is connected via bounded channels.

**The multi-tenant problem (Jansen's Q1):**

In a topology with one source and multiple sinks, a slow Sink B should NOT block Sink A:

```
Source → Transform → Sink A (fast)
                  → Sink B (slow — blocked)
```

**The solution: per-sink buffers with `drop_newest`.**

Each sink has its own independent buffer. When Sink B's buffer fills, it drops newest events for Sink B but doesn't affect Sink A's buffer. Sink A keeps flowing.

```toml
[sinks.sink_a]
  inputs = ["transform"]
  [sinks.sink_a.buffer]
    type = "memory"
    max_events = 50000
    when_full = "drop_newest"  # sink A drops for itself, doesn't block sink B

[sinks.sink_b]
  inputs = ["transform"]
  [sinks.sink_b.buffer]
    type = "disk"
    max_size = 1073741824  # 1GB
    when_full = "drop_newest"
```

This is the core answer to "how do you prevent one slow sink from starving the whole topology" — per-sink buffers with `drop_newest` give you isolation. The tradeoff is that Sink B may lose recent events during an outage, but Sink A is unaffected.

### The EDA / "split arrays" comment

Jansen was referring to how Vector handles **batches of events**. A single source event might produce multiple output events (e.g., parsing a log line that contains a JSON array into individual events). The EDA (the internal Tokio task + channel topology) ensures:

1. Each split sub-event carries its own `BatchNotifier` half
2. All sub-events must be settled before the original source is acked
3. Sub-events can fan out to different sinks independently

This is why the e2e ack model is designed around "events" (the atomic unit that carries the notifier) rather than "batches."

---

## 8. OTel / Datadog Integration — 4 Ingestion Modes

### What Jansen said
> "Ways to get OTel [into Datadog]:
> - **agent** — direct OTLP ingestion
> - **Datadog OTel collector** — embedded in the agent (great for fleet mgmt)
> - **OTLP endpoint** — send it directly to your Datadog account
> - **standalone OTel collector** — can package and send to Datadog"

### The 4 modes explained

**Mode 1: DD Agent with OTLP ingestion**

The Datadog Agent (the traditional one that runs as a DaemonSet) has an OTLP receiver built in as of Agent 6.32+. Your app sends OTLP to `localhost:4317` (gRPC) or `localhost:4318` (HTTP), the Agent receives it and forwards to Datadog.

```
App → OTLP gRPC → DD Agent (localhost:4317) → Datadog intake
```

Best for: teams already running the DD Agent who want to add OTel support with minimal changes.

**Mode 2: Datadog OTel Collector (embedded in Agent)**

This is a more tightly integrated option — it's not the upstream OpenTelemetry Collector binary, it's a Datadog-maintained distribution that includes the DD exporter and is embedded/co-deployed with the Agent. Good for fleet management because Agent management tooling (Fleet Automation) can push config changes to it.

```
App → OTLP → DD-maintained OTel Collector → DD intake
                    ↑
          Managed via DD Fleet Automation
```

**Mode 3: OTLP endpoint (direct to Datadog)**

Send OTLP directly to Datadog's intake endpoint over HTTPS. No local Collector needed. Datadog exposes an OTLP-compatible endpoint at `https://agent.datadoghq.com`.

```
App → OTLP HTTP (with DD API key) → Datadog directly
```

Best for: serverless (Lambda, Cloud Run) where you can't run a DaemonSet. Simplest setup, but no local buffering — if Datadog is down, you lose the data.

**Mode 4: Standalone OTel Collector → Datadog**

Run the upstream OpenTelemetry Collector with the `datadogexporter` component. Full control over the Collector config — you can add processors, route signals to multiple backends, etc.

```
App → OTLP → OTel Collector (datadog exporter) → Datadog
                     ↓
              (also → S3, Grafana, etc.)
```

Best for: multi-backend routing, teams that already run OTel Collector infrastructure, or organizations that want vendor-agnostic pipelines.

**The relevant one for STACKIT:** Mode 4 is most analogous to what STACKIT builds — a standalone OTel Collector (or Vector) in the data path, routing to multiple backends.

---

## 9. OTel Collector Deployment Modes

### What Jansen said
> "OTel collectors have different ways you can run them. Default modes can promote backpressure, the alternatives can help reduce these instances i.e. gateway mode — forwards to a centralized collector/gateway that will handle disk/memory buffers."

### The three deployment topologies

**Agent mode (default/common)**

One Collector per node (DaemonSet). Collects signals from local pods.

```
Node 1: App → OTel Collector (agent) → Datadog/SigNoz
Node 2: App → OTel Collector (agent) → Datadog/SigNoz
```

Backpressure problem: Each agent has a small in-memory buffer. If the downstream (Datadog) is slow, the agent's queue fills, and it starts dropping. With hundreds of nodes, each dropping independently, you get hard-to-detect partial data loss.

**Gateway mode (what Jansen recommended)**

Agents on each node forward to a centralized gateway (Deployment/StatefulSet) that handles the heavy disk buffering and retry logic.

```
Node 1: App → OTel Collector (agent, small buffer) ──►
Node 2: App → OTel Collector (agent, small buffer) ──►  OTel Gateway (large disk buffer) → Datadog
Node 3: App → OTel Collector (agent, small buffer) ──►
```

The agents are lightweight — they just receive OTLP from local pods and forward to the gateway with minimal processing. The gateway does all the heavy lifting: batching, retry, large disk buffer, tail sampling.

**Why gateway mode solves the backpressure problem:**

- The gateway has a large disk buffer (GBs vs MBs on agents)
- Agents can use `when_full = "drop_newest"` for their tiny buffer — they'd rather drop than block apps
- The gateway absorbs sustained Datadog outages without losing data
- Tail sampling only works at the gateway (you need to see the full trace to make a sampling decision — impossible if spans are on different agents)

**Sidecar mode (less common)**

One Collector per pod. Used for per-service routing rules or compliance requirements (e.g., a financial service that must not share a pipeline with other services).

```
App Pod:
  ├── App container → OTLP → localhost:4317
  └── OTel Collector sidecar → Datadog
```

Operationally expensive — N pods = N Collector instances to manage.

**For the STACKIT interview:** *"We'd architect this as agent + gateway. Agents on nodes do OTLP receive and pass-through with minimal processing. The gateway Deployment has the large disk buffer, handles retry, does tail sampling, and is what the STACKIT Telemetry Router CRD controls — customers configure their SignalPolicy CR and the operator reconciles the gateway's Collector config."*

---

## 10. Observing Vector Itself

### What Jansen said
> "Debug logging within Vector (lookup)."

### Key internal metrics to monitor

Vector exposes internal metrics via a `prometheus_exporter` sink or an `internal_metrics` source. The ones that actually matter in production:

**Buffer metrics (most important):**

| Metric | What it tells you |
|---|---|
| `buffer_byte_size` | Current buffer utilization — watch for sustained high values |
| `buffer_events` | Number of events currently buffered |
| `buffer_discarded_events_total` | Events dropped because `when_full = "drop_newest"` hit — this is your data loss counter |

`buffer_discarded_events_total` is the most critical alert to set up. If this is non-zero, you're losing data right now.

**Component metrics:**

| Metric | What it tells you |
|---|---|
| `component_errors_total` | Errors per component (VRL abort, sink connection failure, etc.) |
| `component_received_events_total` | Throughput in to each component |
| `component_sent_events_total` | Throughput out from each component |
| `component_sent_events_total` vs `received` delta | Identifies where events are being dropped |

**Sink-specific:**

| Metric | What it tells you |
|---|---|
| `http_error_response_total` | Downstream returning 4xx/5xx — authentication issues, rate limits |
| `send_errors_total` | Network-level failures to reach the downstream |
| `request_duration_seconds` | Latency to the downstream — rising latency often precedes buffer fill |

**When internal metrics lie:**

Jansen hinted at this. The problem: if Vector itself is overwhelmed, it may not have cycles to scrape and emit its own metrics on schedule. A Prometheus scrape timeout during a backpressure event might show a gap in metrics exactly when you most need them.

Mitigation: run a separate lightweight health-check sidecar that monitors Vector's process (CPU, memory, file descriptors) independently of Vector's self-reported metrics.

**Debug logging:**

```bash
# Increase Vector log verbosity at runtime (sends SIGUSR1)
# or set in config:
[log_schema]
  host_key = "host"

# Run with debug logs
vector --log-level debug

# Or set per-component (useful in production without flooding logs)
VECTOR_LOG="vector[component_id]=debug,vector=info" vector
```

---

## 11. Scaling Vector in Kubernetes — The HPA Problem

### Recap of Jansen's point
> "No HPA within Vector — one of the weak points."
> "Need to architect this yourself."
> "Customers will run vector on hundreds of thousands of pods."
> "You get diminishing returns on Vector with how much you scale it."

### The full picture for interview

**Why "hundreds of thousands of pods" is the context:**

At Datadog scale, a single large customer might have 50,000 pods across their fleet. Each pod's stdout goes to `/var/log/pods` on its node. A DaemonSet Vector collects all of this. But for Vector running as a log aggregator (receiving from agents), the aggregator needs to handle the combined log volume of those 50,000 pods. HPA would help here — scale out the aggregator when log volume spikes — but it doesn't work cleanly.

**The architectural solutions:**

**Option A: Kafka as the elastic buffer (most robust)**

```
Agents → Kafka (elastic, partitioned) → Vector aggregators (fixed replicas)
```

Kafka absorbs the burst. Vector reads from Kafka at a fixed rate it can handle. Scale Vector replicas by adding Kafka partitions. This decouples the ingestion rate from the processing rate.

**Option B: Static sizing with headroom**

Profile peak load, multiply by 1.5x safety factor, set static replica count. Use VPA (Vertical Pod Autoscaler) to right-size CPU/memory within each pod. Simpler, but less elastic.

**Option C: KEDA (Kubernetes Event-Driven Autoscaler)**

KEDA scales Deployments based on external metrics — Kafka consumer lag, Prometheus metrics, etc. You can use `buffer_byte_size` or Kafka lag as the scaling signal:

```yaml
# KEDA ScaledObject example
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: vector-aggregator
spec:
  scaleTargetRef:
    name: vector-aggregator
  minReplicaCount: 2
  maxReplicaCount: 10
  triggers:
    - type: kafka
      metadata:
        topic: raw-logs
        lagThreshold: "1000"  # scale up when consumer lag > 1000
```

But this still has the draining problem — KEDA needs to call a pre-stop hook that drains Vector before the pod is killed.

**Interview framing:** *"HPA is a known gap. In practice you solve it with either Kafka as the elastic buffer (Kafka absorbs spikes, Vector runs at steady capacity) or KEDA with a proper drain pre-stop hook. The key insight is that the scaling problem is really a buffering problem — if you have enough buffer headroom, you don't need to scale in real time."*

---

## 12. Sensitive Data Scanning

### What Jansen said
> "Vector sensitive data scanner built in?"

### What it is

Vector has a built-in **sensitive data redaction** capability via VRL's `redact` function. There's also a higher-level Scanner concept in some distributions (including Datadog Observability Pipelines).

**VRL-level redaction:**

```vrl
# Redact credit card numbers (PAN)
.message = redact(.message, filters: [
  { type: "pattern", pattern: r'\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}\b' }
])

# Redact email addresses
.message = redact(.message, filters: [
  { type: "pattern", pattern: r'[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}' }
])

# Redact IP addresses
.message = redact(.message, filters: [
  { type: "pattern", pattern: r'\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b' }
])

# Multiple filters at once
.message = redact(.message, filters: [
  "us_social_security_number",  # built-in named patterns
  "us_credit_card",
  { type: "pattern", pattern: r'employee-\d+' }
])
```

**Named patterns** (built-in recognizers): `us_social_security_number`, `us_credit_card`, `us_bank_routing_number`, and others. These use pre-compiled regex under the hood.

**Why this matters for STACKIT:** EU data sovereignty rules mean PII must be redacted before logs leave the region — or ideally before they leave the pod. Vector running as a DaemonSet with PII redaction built into the pipeline is the right architecture: redact at the edge (in the agent) so PII never hits the network, not in the aggregator.

---

## 13. Poison Payloads & Error Isolation

### From Jansen's Q5 (what you asked him)
> "What happens when a malformed or oversized event hits a VRL transform that panics — does Vector isolate it per-event, or does it tank the whole component?"

### How Vector actually handles this

**VRL abort (`!` operator):**

When a VRL program aborts (a `!` function fails), Vector handles it at the **per-event** level:
1. The single failing event is routed to the component's `dropped` output
2. All other events in the same batch continue processing normally
3. A `component_errors_total` counter is incremented
4. The component does NOT crash

```toml
[transforms.parse]
  type = "remap"
  inputs = ["source"]
  source = '''
    . = parse_json!(.message)  # aborts this event if message isn't JSON
  '''
  # Events that abort go here instead of crashing the transform:
  drop_on_abort = true  # drop the event (default)
  # OR route to error output:
  reroute_dropped = true  # sends to parse.dropped output
```

**Routing poison payloads to a dead-letter queue:**

```toml
[transforms.parse]
  type = "remap"
  inputs = ["source"]
  reroute_dropped = true
  source = '''
    . = parse_json!(.message)
  '''

# Parse failures go here — archive them for later inspection
[sinks.dead_letter]
  type = "s3"
  inputs = ["parse.dropped"]
  bucket = "my-dead-letter-bucket"
  key_prefix = "vector-failures/%Y/%m/%d/"
```

**Oversized events:**

Vector has `max_uncompressed_bytes` and similar limits on some sources. An oversized event that exceeds these limits is rejected at the source level, before it enters the topology. The source logs an error and drops the event.

**The real poison payload danger — Lua:**

Lua transforms run in a single-threaded interpreter. A Lua script that panics (not just returns an error, but actually panics — accessing nil, stack overflow, etc.) can crash the Lua interpreter, which crashes the transform task. Tokio will try to restart it, but this is a harder failure mode than VRL's graceful abort.

---

## 14. Quick Reference — Interview Answer Skeletons

### "How does Vector handle backpressure?"

*"Each sink has an independent bounded buffer. When the buffer fills, `when_full` determines behavior — `drop_newest` keeps the pipeline flowing at the cost of data loss, `block` preserves data at the cost of propagating pressure upstream all the way to the source. In multi-tenant topologies, per-sink buffers with `drop_newest` give you isolation — a slow Sink B doesn't block Sink A. The backpressure signal flows naturally through Tokio's bounded channels; there's no explicit signaling protocol, it's just channel saturation."*

### "How do you upgrade Vector without dropping events?"

*"Enable e2e acknowledgements plus disk buffers. Send SIGTERM — Vector stops accepting new events, drains the disk buffer to the sink, waits for sink acks, then has the source commit its position (Kafka offset, file cursor). The new version starts from the committed position. Without disk buffers you'll always have a small at-least-once window where the new instance re-processes recent events."*

### "Why can't you HPA Vector?"

*"Two reasons: disk buffers are node-local and stateful — a scaled-down pod loses its undelivered buffer — and the DaemonSet deployment model (one per node) doesn't support HPA at all. The right solution is either Kafka as the elastic buffer between agents and aggregators (Kafka absorbs spikes, Vector aggregators run at steady capacity) or KEDA with a proper drain pre-stop hook. In practice, most teams right-size Vector statically based on peak load profiling."*

### "What's the difference between `block` and `drop_newest`?"

*"`block` propagates backpressure upstream — the source pauses, which is correct for zero-loss requirements but can cause cascading slowdowns. `drop_newest` keeps the pipeline flowing but silently loses the most recent events when the buffer is full. `drop_newest` is almost always the right default for logs (a 1% gap in logs is acceptable; a stalled pipeline is not). For financial or compliance data where every event must be delivered, `block` with a very large disk buffer is appropriate."*

### "What is RKYV and why does Vector use it?"

*"RKYV is a zero-copy serialization framework for Rust. The encoded representation is identical to the in-memory layout of the struct, so reading from disk is just a pointer cast — no allocation, no deserialization work. Vector uses this for disk buffers because the buffer write path is extremely hot in high-throughput pipelines. JSON or Protobuf serialization on every event would add unacceptable CPU overhead."*

### "What VRL can't do?"

*"VRL is sandboxed — no network calls, no filesystem access, no state across events. When customers need external enrichment (calling a threat intel API, looking up a customer record), we either pre-load the enrichment data into an `enrichment_table` (VRL can query a local enrichment table loaded from CSV at startup), or we push that logic out of Vector entirely — into a dedicated microservice that the consuming system calls. If stateful transforms are unavoidable, Lua is the escape hatch, but it comes with GC overhead and runs on a separate thread."*

---

*Sources referenced: [Vector buffering model](https://vector.dev/docs/architecture/buffering-model/) · [Vector e2e acknowledgements](https://vector.dev/docs/architecture/end-to-end-acknowledgements/) · [RKYV zero-copy deserialization](https://rkyv.org/zero-copy-deserialization.html) · [OTel Collector deployment patterns](https://www.controltheory.com/resources/opentelemetry-collector-deployment-patterns-a-guide/) · [Vector high availability](https://vector.dev/docs/setup/going-to-prod/high-availability/)*
