#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────────────
# port-forward.sh  —  Background kubectl port-forwards for the local demo
#
# Usage:
#   scripts/port-forward.sh start    # open all tunnels
#   scripts/port-forward.sh stop     # kill all tunnels
#   scripts/port-forward.sh status   # show what's running
# ──────────────────────────────────────────────────────────────────────────────

set -euo pipefail

ACTION="${1:-start}"

start_forward() {
    local name=$1 namespace=$2 resource=$3 local_port=$4 remote_port=$5
    echo "   ↳ $name  localhost:${local_port} → ${resource}:${remote_port}"
    kubectl port-forward \
        -n "$namespace" "$resource" \
        "${local_port}:${remote_port}" \
        >/tmp/pf-"${name}".log 2>&1 &
}

case "$ACTION" in
start)
    echo "▶  Starting port-forwards..."

    # Grafana
    start_forward "grafana"     "observability"    "svc/grafana"              3000 3000
    # Prometheus
    start_forward "prometheus"  "observability"    "svc/prometheus"           9090 9090
    # Loki (direct query if needed)
    start_forward "loki"        "observability"    "svc/loki"                 3100 3100
    # Tempo
    start_forward "tempo"       "observability"    "svc/tempo"                3200 3200
    # Operator REST API
    start_forward "operator"    "telemetry-system" "svc/telemetry-operator-api" 8080 8080
    # Vector metrics
    start_forward "vector"      "telemetry-system" "svc/vector"               9598 9598
    # Customer price-service
    start_forward "price-svc"   "customer-app"     "svc/price-service"        8000 8000

    echo ""
    echo "✅  Port-forwards started (PIDs in /tmp/pf-*.log)"
    echo ""
    echo "   Service        URL"
    echo "   ─────────────────────────────────────────────"
    echo "   Grafana        http://localhost:3000  (admin / admin)"
    echo "   Prometheus     http://localhost:9090"
    echo "   Loki           http://localhost:3100"
    echo "   Tempo          http://localhost:3200"
    echo "   Operator API   http://localhost:8080/api/v1/routers"
    echo "   Vector metrics http://localhost:9598/metrics"
    echo "   Price Service  http://localhost:8000"
    ;;

stop)
    echo "▶  Stopping port-forwards..."
    pkill -f "kubectl port-forward" 2>/dev/null && echo "   Stopped." || echo "   None running."
    ;;

status)
    echo "▶  Active port-forwards:"
    pgrep -a -f "kubectl port-forward" 2>/dev/null | sed 's/^/   /' || echo "   None running."
    ;;

*)
    echo "Usage: $0 {start|stop|status}" >&2
    exit 1
    ;;
esac
