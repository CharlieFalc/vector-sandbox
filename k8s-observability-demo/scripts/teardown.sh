#!/usr/bin/env bash
# teardown.sh — remove everything created by setup.sh.
#
# Usage:
#   ./scripts/teardown.sh           # removes demo namespace; keeps SigNoz
#   ./scripts/teardown.sh --all     # removes everything including SigNoz

set -euo pipefail
cd "$(dirname "$0")/.."

BLUE='\033[0;34m'
GREEN='\033[0;32m'
NC='\033[0m'

log() { echo -e "${BLUE}▶ $*${NC}"; }
ok()  { echo -e "${GREEN}✓ $*${NC}"; }

REMOVE_SIGNOZ=false
if [[ "${1:-}" == "--all" ]]; then
  REMOVE_SIGNOZ=true
fi

# ------------------------------------------------------------------
# Remove demo namespace (includes all workloads, configmaps, services)
# ------------------------------------------------------------------
log "Removing observability-demo namespace..."
kubectl delete namespace observability-demo --ignore-not-found=true
ok "Namespace removed"

# ------------------------------------------------------------------
# Remove CRDs (this also removes all SignalPolicy CRs cluster-wide)
# ------------------------------------------------------------------
log "Removing CRDs..."
kubectl delete -f k8s/crds/ --ignore-not-found=true
ok "CRDs removed"

# ------------------------------------------------------------------
# Remove cluster-scoped RBAC for Vector
# ------------------------------------------------------------------
log "Removing ClusterRole/ClusterRoleBinding for Vector..."
kubectl delete clusterrole vector-observability-demo --ignore-not-found=true
kubectl delete clusterrolebinding vector-observability-demo --ignore-not-found=true
ok "Vector RBAC removed"

# ------------------------------------------------------------------
# Optionally remove SigNoz
# ------------------------------------------------------------------
if [[ "$REMOVE_SIGNOZ" == "true" ]]; then
  log "Removing SigNoz Helm release..."
  helm uninstall signoz -n platform 2>/dev/null || true
  kubectl delete namespace platform --ignore-not-found=true
  ok "SigNoz removed"
else
  echo ""
  echo "SigNoz kept (run with --all to remove it too)."
fi

echo ""
ok "Teardown complete."
