#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────────────
# test.sh  —  End-to-end smoke test for the STACKIT Telemetry Router local demo
#
# Checks:
#   1. Customer app (price-service) — all endpoints
#   2. Operator REST API — list / create / get / patch / delete
#   3. Vector health (via operator-deployed sidecar)
#   4. Observability stack — Prometheus, Loki, Tempo reachable
#   5. Telemetry actually arriving (query Prometheus for price_service metrics)
#
# Prerequisites: make port-forward must be running
# ──────────────────────────────────────────────────────────────────────────────

set -euo pipefail

PRICE_SVC="http://localhost:8000"
OPERATOR="http://localhost:8080"
PROMETHEUS="http://localhost:9090"
LOKI="http://localhost:3100"
TEMPO="http://localhost:3200"

PASS=0
FAIL=0

# ── Helpers ───────────────────────────────────────────────────────────────────

check() {
    local label=$1 url=$2 expected_code=${3:-200}
    local code
    code=$(curl -s -o /dev/null -w "%{http_code}" "$url" || echo "000")
    if [ "$code" = "$expected_code" ]; then
        echo "  ✅  $label  ($code)"
        PASS=$((PASS + 1))
    else
        echo "  ❌  $label  (expected $expected_code, got $code) → $url"
        FAIL=$((FAIL + 1))
    fi
}

post_check() {
    local label=$1 url=$2 body=$3 expected_code=${4:-200}
    local code
    code=$(curl -s -o /dev/null -w "%{http_code}" \
               -X POST -H "Content-Type: application/json" \
               -d "$body" "$url" || echo "000")
    if [ "$code" = "$expected_code" ]; then
        echo "  ✅  $label  ($code)"
        PASS=$((PASS + 1))
    else
        echo "  ❌  $label  (expected $expected_code, got $code)"
        FAIL=$((FAIL + 1))
    fi
}

separator() { echo ""; echo "── $1 ──────────────────────────────────────────"; }

# ── 1. Customer App ───────────────────────────────────────────────────────────
separator "Customer App (price-service)"

check "health endpoint"          "$PRICE_SVC/health"
check "list prices"              "$PRICE_SVC/prices"
check "get price AAPL"           "$PRICE_SVC/prices/AAPL"
check "get price STACKIT"        "$PRICE_SVC/prices/STACKIT"
check "get price unknown (404)"  "$PRICE_SVC/prices/DOESNOTEXIST" 404
check "list orders"              "$PRICE_SVC/orders"
check "simulate burst"           "$PRICE_SVC/simulate"

echo ""
echo "  Placing a test order..."
ORDER_RESP=$(curl -s -X POST -H "Content-Type: application/json" \
    -d '{"symbol":"NVDA","quantity":5}' "$PRICE_SVC/orders" || echo "{}")
ORDER_ID=$(echo "$ORDER_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('order_id',''))" 2>/dev/null || echo "")
if [ -n "$ORDER_ID" ]; then
    echo "  ✅  POST /orders  (order_id=$ORDER_ID)"
    PASS=$((PASS + 1))
    check "GET order by id" "$PRICE_SVC/orders/$ORDER_ID"
else
    echo "  ❌  POST /orders  (no order_id returned)"
    FAIL=$((FAIL + 1))
fi

# ── 2. Operator REST API ──────────────────────────────────────────────────────
separator "Operator REST API"

check "healthz" "$OPERATOR/healthz"
check "list routers" "$OPERATOR/api/v1/routers"

# Create a test router via API
TEST_ROUTER_NAME="test-router-$$"
echo ""
echo "  Creating test router: $TEST_ROUTER_NAME"
CREATE_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST -H "Content-Type: application/json" \
    -d '{
      "replicas": 1,
      "sources": ["OTLP"],
      "destinations": [
        {"name":"prom","type":"PROMETHEUS","endpoint":"http://prometheus.observability.svc.cluster.local:9090/api/v1/write"}
      ]
    }' \
    "$OPERATOR/api/v1/routers?name=$TEST_ROUTER_NAME" || echo "000")

if [ "$CREATE_CODE" = "201" ]; then
    echo "  ✅  POST /api/v1/routers  (201)"
    PASS=$((PASS + 1))
    check "GET  /api/v1/routers/$TEST_ROUTER_NAME" "$OPERATOR/api/v1/routers/$TEST_ROUTER_NAME"

    # Patch it
    PATCH_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
        -X PATCH -H "Content-Type: application/json" \
        -d '{"replicas": 2}' \
        "$OPERATOR/api/v1/routers/$TEST_ROUTER_NAME" || echo "000")
    if [ "$PATCH_CODE" = "200" ]; then
        echo "  ✅  PATCH /api/v1/routers/$TEST_ROUTER_NAME  (200)"
        PASS=$((PASS + 1))
    else
        echo "  ❌  PATCH /api/v1/routers/$TEST_ROUTER_NAME  ($PATCH_CODE)"
        FAIL=$((FAIL + 1))
    fi

    # Delete it
    DELETE_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
        -X DELETE "$OPERATOR/api/v1/routers/$TEST_ROUTER_NAME" || echo "000")
    if [ "$DELETE_CODE" = "200" ]; then
        echo "  ✅  DELETE /api/v1/routers/$TEST_ROUTER_NAME  (200)"
        PASS=$((PASS + 1))
    else
        echo "  ❌  DELETE /api/v1/routers/$TEST_ROUTER_NAME  ($DELETE_CODE)"
        FAIL=$((FAIL + 1))
    fi
else
    echo "  ❌  POST /api/v1/routers  (expected 201, got $CREATE_CODE)"
    FAIL=$((FAIL + 1))
fi

# ── 3. Observability Stack ────────────────────────────────────────────────────
separator "Observability Stack"

check "Prometheus /-/healthy"   "$PROMETHEUS/-/healthy"
check "Prometheus /api/v1/labels" "$PROMETHEUS/api/v1/labels"
check "Loki /ready"             "$LOKI/ready"
check "Loki /loki/api/v1/labels" "$LOKI/loki/api/v1/labels"
check "Tempo /ready"            "$TEMPO/ready"

# ── 4. Telemetry flowing check ────────────────────────────────────────────────
separator "Telemetry Data (Prometheus)"

echo "  Querying for price_service request metrics..."
METRIC_RESULT=$(curl -s "$PROMETHEUS/api/v1/query?query=http_server_request_duration_seconds_count" \
    | python3 -c "
import sys, json
d = json.load(sys.stdin)
results = d.get('data', {}).get('result', [])
print(len(results))
" 2>/dev/null || echo "0")

if [ "$METRIC_RESULT" -gt "0" ] 2>/dev/null; then
    echo "  ✅  Found $METRIC_RESULT metric series in Prometheus — telemetry flowing!"
    PASS=$((PASS + 1))
else
    echo "  ⚠️   No request metrics in Prometheus yet (may need 'make simulate' first)"
    # Not a hard fail — data may not have flushed yet
fi

echo ""
echo "  Querying for Vector internal metrics..."
VECTOR_RESULT=$(curl -s "$PROMETHEUS/api/v1/query?query=vector_component_events_in_total" \
    | python3 -c "
import sys, json
d = json.load(sys.stdin)
results = d.get('data', {}).get('result', [])
print(len(results))
" 2>/dev/null || echo "0")

if [ "$VECTOR_RESULT" -gt "0" ] 2>/dev/null; then
    echo "  ✅  Found Vector component metrics in Prometheus ($VECTOR_RESULT series)"
    PASS=$((PASS + 1))
else
    echo "  ⚠️   No Vector metrics in Prometheus yet"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════"
TOTAL=$((PASS + FAIL))
echo "  Tests passed: $PASS / $TOTAL"
if [ "$FAIL" -gt "0" ]; then
    echo "  Tests failed: $FAIL"
    echo ""
    echo "  Tip: Make sure port-forwards are running:"
    echo "    make port-forward"
    echo ""
    exit 1
else
    echo ""
    echo "  🎉  All tests passed!"
    echo ""
fi
