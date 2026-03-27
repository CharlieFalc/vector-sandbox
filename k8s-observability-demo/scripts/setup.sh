#!/usr/bin/env bash
# setup.sh — one-shot deployment of the k8s-observability-demo on Docker Desktop Kubernetes.
#
# Prerequisites:
#   - Docker Desktop with Kubernetes enabled
#   - kubectl configured (docker-desktop context)
#   - helm (brew install helm)
#
# Usage:
#   ./scripts/setup.sh
#
# What this does:
#   1. Install SigNoz via Helm in the 'platform' namespace
#   2. Build Docker images locally (no registry needed — imagePullPolicy: Never)
#   3. Apply all k8s manifests via kustomize
#   4. Print access instructions

set -euo pipefail
cd "$(dirname "$0")/.."

BLUE='\033[0;34m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${BLUE}▶ $*${NC}"; }
ok()   { echo -e "${GREEN}✓ $*${NC}"; }
warn() { echo -e "${YELLOW}⚠ $*${NC}"; }

# ------------------------------------------------------------------
# Preflight checks
# ------------------------------------------------------------------
log "Checking prerequisites..."

for cmd in kubectl helm docker; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: '$cmd' not found. Please install it first."
    exit 1
  fi
done

# Verify Docker Desktop Kubernetes is the active context
KUBE_CTX=$(kubectl config current-context 2>/dev/null || echo "none")
if [[ "$KUBE_CTX" != "docker-desktop" ]]; then
  warn "Current context is '$KUBE_CTX', not 'docker-desktop'."
  warn "Switch with: kubectl config use-context docker-desktop"
  read -rp "Continue anyway? (y/N): " ans
  [[ "$ans" =~ ^[Yy]$ ]] || exit 1
fi
ok "kubectl context: $KUBE_CTX"

# ------------------------------------------------------------------
# 1. Install SigNoz via Helm
# ------------------------------------------------------------------
log "Installing SigNoz (open-source observability platform)..."

helm repo add signoz https://charts.signoz.io 2>/dev/null || true
helm repo update

if helm status signoz -n platform &>/dev/null; then
  warn "SigNoz already installed — skipping."
else
  helm install signoz signoz/signoz \
    --namespace platform \
    --create-namespace \
    --set frontend.service.type=ClusterIP \
    --wait \
    --timeout 10m
  ok "SigNoz installed"
fi

# ------------------------------------------------------------------
# 2. Build Docker images locally
# ------------------------------------------------------------------
log "Building signal-forge Docker image..."
docker build -t signal-forge:latest -f Dockerfile .
ok "signal-forge:latest built"

log "Building operator Docker image..."
docker build -t signal-forge-operator:latest -f Dockerfile.operator .
ok "signal-forge-operator:latest built"

# ------------------------------------------------------------------
# 3. Apply Kubernetes manifests
# ------------------------------------------------------------------
log "Applying Kubernetes manifests..."

# CRDs first — other resources depend on them
kubectl apply -f k8s/crds/

# Brief wait for CRD to be established before creating CRs
kubectl wait --for=condition=Established \
  crd/signalpolicies.observability.demo.dev \
  --timeout=30s

# Everything else via kustomize
kubectl apply -k k8s/

ok "All manifests applied"

# ------------------------------------------------------------------
# 4. Wait for pods to be ready
# ------------------------------------------------------------------
log "Waiting for pods to become ready..."

kubectl rollout status deployment/otel-collector \
  -n observability-demo --timeout=120s
kubectl rollout status deployment/signal-forge \
  -n observability-demo --timeout=120s
kubectl rollout status deployment/signal-forge-operator \
  -n observability-demo --timeout=120s

ok "All pods ready"

# ------------------------------------------------------------------
# 5. Print access instructions
# ------------------------------------------------------------------
echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  k8s-observability-demo is running!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo "Port-forward to access services:"
echo ""
echo "  # SigNoz UI (open in browser)"
echo "  kubectl port-forward svc/signoz-frontend 3301:3301 -n platform"
echo "  open http://localhost:3301"
echo ""
echo "  # signal-forge API + Swagger UI"
echo "  kubectl port-forward svc/signal-forge 8080:8080 -n observability-demo"
echo "  open http://localhost:8080/docs"
echo ""
echo "Generate signals:"
echo "  curl http://localhost:8080/api/v1/simulate/slow   # slow trace"
echo "  curl http://localhost:8080/api/v1/simulate/error  # error trace"
echo "  curl -X POST http://localhost:8080/api/v1/widgets \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"name\":\"test\",\"color\":\"blue\",\"weight\":1.5}'"
echo ""
echo "Inspect SignalPolicy:"
echo "  kubectl get signalpolicies -n observability-demo"
echo "  kubectl describe signalpolicy default -n observability-demo"
echo ""
echo "Apply a different policy (triggers operator reconcile):"
echo "  kubectl apply -f k8s/signalpolicy/high-sampling.yaml"
echo "  kubectl get signalpolicies -w -n observability-demo"
echo ""
echo "Watch operator logs:"
echo "  kubectl logs -f deployment/signal-forge-operator -n observability-demo"
echo ""
