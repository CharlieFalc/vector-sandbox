"""
Price Service — STACKIT Mock Customer Workload
Generates a rich mix of logs, traces, and metrics via OTel SDK.
"""

import os
import random
import time
import logging
import json
from contextlib import asynccontextmanager
from datetime import datetime

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel

# ── OTel Setup ──────────────────────────────────────────────────────────────

from opentelemetry import trace, metrics
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry._logs import set_logger_provider
from opentelemetry.instrumentation.logging import LoggingInstrumentor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
from opentelemetry.sdk.resources import Resource, SERVICE_NAME, SERVICE_VERSION
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor

OTLP_ENDPOINT         = os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

# Per-signal endpoint overrides.
# - Traces  → Tempo directly (Vector's HTTP sink can't emit OTLP protobuf/JSON,
#             so bypassing Vector avoids silent drops / format mismatches).
# - Metrics → Prometheus OTLP receiver directly (Vector 0.42 has no metrics output
#             on the opentelemetry source, so metrics would be silently dropped).
# - Logs    → Vector (enrichment + label injection → Loki) — default fallback.
OTLP_TRACES_ENDPOINT  = os.getenv(
    "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
    f"{OTLP_ENDPOINT}/v1/traces",
)
OTLP_METRICS_ENDPOINT = os.getenv(
    "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
    f"{OTLP_ENDPOINT}/v1/metrics",
)
SERVICE = os.getenv("OTEL_SERVICE_NAME", "price-service")
ENV     = os.getenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=local")

resource = Resource.create({
    SERVICE_NAME: SERVICE,
    SERVICE_VERSION: "1.0.0",
    "deployment.environment": "local",
    "k8s.namespace": "customer-app",
    "k8s.cluster": "docker-desktop",
})

# Traces
tracer_provider = TracerProvider(resource=resource)
tracer_provider.add_span_processor(
    BatchSpanProcessor(OTLPSpanExporter(endpoint=OTLP_TRACES_ENDPOINT))
)
trace.set_tracer_provider(tracer_provider)
tracer = trace.get_tracer(SERVICE)

# Metrics — sent to OTLP_METRICS_ENDPOINT (defaults to Vector, can be overridden
# to Prometheus OTLP receiver so metrics bypass Vector's opentelemetry source,
# which in Vector ≤0.42 does not expose an otlp_in.metrics named output).
metric_reader = PeriodicExportingMetricReader(
    OTLPMetricExporter(endpoint=OTLP_METRICS_ENDPOINT),
    export_interval_millis=15_000,
)
meter_provider = MeterProvider(resource=resource, metric_readers=[metric_reader])
metrics.set_meter_provider(meter_provider)
meter = metrics.get_meter(SERVICE)

# Logs
logger_provider = LoggerProvider(resource=resource)
logger_provider.add_log_record_processor(
    BatchLogRecordProcessor(OTLPLogExporter(endpoint=f"{OTLP_ENDPOINT}/v1/logs"))
)
set_logger_provider(logger_provider)

# Inject otelTraceID / otelSpanID into Python log records
LoggingInstrumentor().instrument(set_logging_format=True)

# Configure Python's logging module
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s [%(name)s] [trace_id=%(otelTraceID)s span_id=%(otelSpanID)s] %(message)s",
)
# Bridge Python logs → OTel logger provider
logging.getLogger().addHandler(LoggingHandler(level=logging.NOTSET, logger_provider=logger_provider))
logger = logging.getLogger("price-service")

# ── Custom Metrics ───────────────────────────────────────────────────────────

order_counter = meter.create_counter(
    "orders_total",
    description="Total number of orders placed",
    unit="1",
)
order_value_histogram = meter.create_histogram(
    "order_value_usd",
    description="Value of placed orders in USD",
    unit="USD",
)
price_lookup_histogram = meter.create_histogram(
    "price_lookup_duration_ms",
    description="Time taken to look up a price",
    unit="ms",
)
active_orders_gauge = meter.create_up_down_counter(
    "active_orders",
    description="Number of orders currently being processed",
    unit="1",
)

# ── Fake Data ────────────────────────────────────────────────────────────────

PRICES = {
    "AAPL":   182.50,
    "GOOGL":  141.80,
    "MSFT":   415.30,
    "NVDA":   875.40,
    "STACKIT": 42.00,   # 🚀
    "AMZN":   186.90,
}

# Simulate a small in-memory order book
ORDER_BOOK: dict[str, dict] = {}


# ── App ──────────────────────────────────────────────────────────────────────

@asynccontextmanager
async def lifespan(app: FastAPI):
    logger.info("Price Service starting up", extra={"event": "startup"})
    yield
    logger.info("Price Service shutting down", extra={"event": "shutdown"})
    tracer_provider.shutdown()
    meter_provider.shutdown()
    logger_provider.shutdown()

app = FastAPI(title="Price Service", version="1.0.0", lifespan=lifespan)
FastAPIInstrumentor.instrument_app(app)


@app.get("/health")
async def health():
    return {"status": "ok", "service": SERVICE, "timestamp": datetime.utcnow().isoformat()}


@app.get("/prices")
async def list_prices():
    """Return all stock prices with simulated jitter."""
    with tracer.start_as_current_span("list_prices") as span:
        jitter = random.uniform(0.005, 0.05)
        time.sleep(jitter)

        # Apply random drift to each price
        result = {sym: round(p * (1 + random.uniform(-0.005, 0.005)), 2)
                  for sym, p in PRICES.items()}

        span.set_attribute("prices.count", len(result))
        logger.info("Listed %d prices in %.0fms", len(result), jitter * 1000)
        return {"prices": result, "timestamp": datetime.utcnow().isoformat()}


@app.get("/prices/{symbol}")
async def get_price(symbol: str):
    """Look up a single stock price. Randomly fails 8% of the time to generate errors."""
    with tracer.start_as_current_span("get_price") as span:
        span.set_attribute("symbol", symbol.upper())
        start = time.perf_counter()

        symbol = symbol.upper()

        # Simulate random latency spike
        latency = random.uniform(0.01, 0.15)
        time.sleep(latency)

        # Intentional random error (good for trace error exploration)
        if random.random() < 0.08:
            span.set_status(trace.StatusCode.ERROR, "Price feed timeout")
            span.set_attribute("error.type", "timeout")
            logger.error("Price feed timeout for symbol=%s", symbol)
            price_lookup_histogram.record(
                (time.perf_counter() - start) * 1000,
                {"symbol": symbol, "status": "error"},
            )
            raise HTTPException(status_code=503, detail=f"Price feed temporarily unavailable for {symbol}")

        if symbol not in PRICES:
            span.set_status(trace.StatusCode.ERROR, "Symbol not found")
            logger.warning("Unknown symbol requested: %s", symbol)
            raise HTTPException(status_code=404, detail=f"Symbol '{symbol}' not found. Available: {list(PRICES.keys())}")

        price = round(PRICES[symbol] * (1 + random.uniform(-0.01, 0.01)), 2)
        duration_ms = (time.perf_counter() - start) * 1000

        price_lookup_histogram.record(duration_ms, {"symbol": symbol, "status": "success"})
        span.set_attribute("price.value", price)
        span.set_attribute("lookup.duration_ms", duration_ms)
        logger.info("Price lookup: symbol=%s price=%.2f latency=%.1fms", symbol, price, duration_ms)

        return {"symbol": symbol, "price": price, "timestamp": datetime.utcnow().isoformat()}


class OrderRequest(BaseModel):
    symbol: str
    quantity: int
    side: str = "BUY"


@app.post("/orders", status_code=201)
async def place_order(order: OrderRequest):
    """
    Place a stock order. Creates a multi-span trace:
      place_order
        ├── validate_order
        ├── fetch_price
        └── execute_order
    """
    with tracer.start_as_current_span("place_order") as root_span:
        root_span.set_attribute("order.symbol",   order.symbol.upper())
        root_span.set_attribute("order.quantity", order.quantity)
        root_span.set_attribute("order.side",     order.side)

        active_orders_gauge.add(1, {"symbol": order.symbol})

        try:
            # ── Step 1: Validate ──────────────────────────────────────────
            with tracer.start_as_current_span("validate_order") as vs:
                time.sleep(random.uniform(0.005, 0.02))
                if order.quantity <= 0:
                    vs.set_status(trace.StatusCode.ERROR, "Invalid quantity")
                    raise HTTPException(status_code=400, detail="Quantity must be positive")
                if order.side not in ("BUY", "SELL"):
                    vs.set_status(trace.StatusCode.ERROR, "Invalid side")
                    raise HTTPException(status_code=400, detail="Side must be BUY or SELL")
                vs.set_attribute("validation.passed", True)

            # ── Step 2: Fetch current price ───────────────────────────────
            with tracer.start_as_current_span("fetch_price") as fps:
                time.sleep(random.uniform(0.01, 0.05))
                sym = order.symbol.upper()
                if sym not in PRICES:
                    fps.set_status(trace.StatusCode.ERROR, "Unknown symbol")
                    raise HTTPException(status_code=404, detail=f"Symbol {sym} not found")
                price = round(PRICES[sym] * (1 + random.uniform(-0.005, 0.005)), 2)
                fps.set_attribute("price.fetched", price)

            # ── Step 3: Execute ───────────────────────────────────────────
            with tracer.start_as_current_span("execute_order") as es:
                time.sleep(random.uniform(0.02, 0.1))
                # 5% execution failure rate
                if random.random() < 0.05:
                    es.set_status(trace.StatusCode.ERROR, "Execution failed")
                    root_span.set_status(trace.StatusCode.ERROR, "Order execution failed")
                    logger.error("Order execution failed: symbol=%s quantity=%d", sym, order.quantity)
                    raise HTTPException(status_code=503, detail="Order execution failed — please retry")

                order_id = f"ORD-{random.randint(10000, 99999)}"
                total_value = round(price * order.quantity, 2)
                ORDER_BOOK[order_id] = {
                    "order_id": order_id,
                    "symbol": sym,
                    "quantity": order.quantity,
                    "side": order.side,
                    "price": price,
                    "total_value": total_value,
                    "status": "FILLED",
                    "created_at": datetime.utcnow().isoformat(),
                }
                es.set_attribute("order.id",          order_id)
                es.set_attribute("order.total_value", total_value)
                es.set_attribute("order.status",      "FILLED")

            # Record metrics
            order_counter.add(1, {"symbol": sym, "side": order.side, "status": "filled"})
            order_value_histogram.record(total_value, {"symbol": sym, "side": order.side})
            root_span.set_attribute("order.id", order_id)

            logger.info(
                "Order filled: id=%s symbol=%s qty=%d side=%s price=%.2f total=%.2f",
                order_id, sym, order.quantity, order.side, price, total_value,
            )
            return ORDER_BOOK[order_id]

        finally:
            active_orders_gauge.add(-1, {"symbol": order.symbol})


@app.get("/orders")
async def list_orders():
    return {"orders": list(ORDER_BOOK.values()), "count": len(ORDER_BOOK)}


@app.get("/orders/{order_id}")
async def get_order(order_id: str):
    if order_id not in ORDER_BOOK:
        raise HTTPException(status_code=404, detail=f"Order {order_id} not found")
    return ORDER_BOOK[order_id]


@app.get("/simulate")
async def simulate_traffic(requests: int = 20):
    """
    Trigger a burst of mixed traffic to generate telemetry.
    Great for warming up Grafana dashboards after deploy.
    """
    import asyncio, httpx

    results = {"prices_fetched": 0, "orders_placed": 0, "errors": 0}
    symbols = list(PRICES.keys())

    async with httpx.AsyncClient(base_url="http://localhost:8000", timeout=10) as client:
        for _ in range(requests):
            try:
                sym = random.choice(symbols)
                # Mix of price lookups and orders
                if random.random() < 0.7:
                    r = await client.get(f"/prices/{sym}")
                    if r.status_code == 200:
                        results["prices_fetched"] += 1
                    else:
                        results["errors"] += 1
                else:
                    r = await client.post("/orders", json={
                        "symbol": sym,
                        "quantity": random.randint(1, 100),
                        "side": random.choice(["BUY", "SELL"]),
                    })
                    if r.status_code == 201:
                        results["orders_placed"] += 1
                    else:
                        results["errors"] += 1
                await asyncio.sleep(random.uniform(0.01, 0.05))
            except Exception as e:
                results["errors"] += 1
                logger.warning("Simulate request failed: %s", str(e))

    logger.info("Simulation complete: %s", json.dumps(results))
    return results


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=int(os.getenv("PORT", "8000")))
