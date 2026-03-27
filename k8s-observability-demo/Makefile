# =============================================================================
# k8s-observability-demo — Makefile
# =============================================================================
# Prerequisites (Docker Desktop with Kubernetes enabled + helm):
#   make setup      ← install SigNoz, build images, deploy everything
#
# Go build (local testing without Kubernetes):
#   make tidy       ← go mod tidy (required on first run)
#   make build      ← compile both binaries
#   make run        ← run signal-forge locally (no OTel Collector needed)
# =============================================================================

APP_IMG      := signal-forge:latest
OPERATOR_IMG := signal-forge-operator:latest
NAMESPACE    := observability-demo

.DEFAULT_GOAL := help

.PHONY: help
help:
	@echo ""
	@echo "  k8s-observability-demo — available targets"
	@echo ""
	@echo "  Setup (Docker Desktop Kubernetes)"
	@echo "    setup          Install SigNoz + build images + deploy everything"
	@echo "    teardown       Remove demo namespace (keeps SigNoz)"
	@echo "    teardown-all   Remove everything including SigNoz"
	@echo ""
	@echo "  Development"
	@echo "    tidy           go mod tidy (run first)"
	@echo "    build          Compile signal-forge + operator binaries"
	@echo "    build-images   Build Docker images (for Kubernetes)"
	@echo "    run            Run signal-forge locally on :8080"
	@echo ""
	@echo "  Kubernetes"
	@echo "    deploy         Apply all k8s manifests (images must be built first)"
	@echo "    undeploy       Remove all manifests"
	@echo "    logs-app       Tail signal-forge logs"
	@echo "    logs-operator  Tail operator logs"
	@echo "    logs-collector Tail OTel Collector logs"
	@echo "    port-forward   Port-forward signal-forge + SigNoz"
	@echo "    status         Show pod status + SignalPolicy status"
	@echo ""
	@echo "  Signals"
	@echo "    simulate-slow  Generate a slow trace (2s latency)"
	@echo "    simulate-error Generate an error trace"
	@echo "    load-test      Generate 20 requests over 60s"
	@echo ""

# ------------------------------------------------------------------
# Setup / teardown (full stack)
# ------------------------------------------------------------------
.PHONY: setup
setup:
	./scripts/setup.sh

.PHONY: teardown
teardown:
	./scripts/teardown.sh

.PHONY: teardown-all
teardown-all:
	./scripts/teardown.sh --all

# ------------------------------------------------------------------
# Go build
# ------------------------------------------------------------------
.PHONY: tidy
tidy:
	go mod tidy

.PHONY: build
build:
	go build -o bin/signal-forge ./cmd/signal-forge
	go build -o bin/operator ./cmd/operator
	@echo "✓ Binaries: bin/signal-forge, bin/operator"

.PHONY: run
run:
	@echo "Running signal-forge on :8080 (no OTel Collector — OTLP will fail gracefully)"
	OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 \
	go run ./cmd/signal-forge

# ------------------------------------------------------------------
# Docker images
# ------------------------------------------------------------------
.PHONY: build-images
build-images:
	docker build -t $(APP_IMG) -f Dockerfile .
	docker build -t $(OPERATOR_IMG) -f Dockerfile.operator .
	@echo "✓ Images built: $(APP_IMG), $(OPERATOR_IMG)"

# ------------------------------------------------------------------
# Kubernetes
# ------------------------------------------------------------------
.PHONY: deploy
deploy:
	kubectl apply -f k8s/crds/
	kubectl wait --for=condition=Established \
	  crd/signalpolicies.observability.demo.dev --timeout=30s
	kubectl apply -k k8s/

.PHONY: undeploy
undeploy:
	kubectl delete -k k8s/ --ignore-not-found=true
	kubectl delete -f k8s/crds/ --ignore-not-found=true

.PHONY: logs-app
logs-app:
	kubectl logs -f deployment/signal-forge -n $(NAMESPACE)

.PHONY: logs-operator
logs-operator:
	kubectl logs -f deployment/signal-forge-operator -n $(NAMESPACE)

.PHONY: logs-collector
logs-collector:
	kubectl logs -f deployment/otel-collector -n $(NAMESPACE)

.PHONY: port-forward
port-forward:
	@echo "Port-forwarding signal-forge to :8080 and SigNoz to :3301"
	@echo "Open http://localhost:8080/docs and http://localhost:3301"
	kubectl port-forward svc/signal-forge 8080:8080 -n $(NAMESPACE) &
	kubectl port-forward svc/signoz-frontend 3301:3301 -n platform

.PHONY: status
status:
	@echo "=== Pods ==="
	kubectl get pods -n $(NAMESPACE)
	@echo ""
	@echo "=== SignalPolicies ==="
	kubectl get signalpolicies -n $(NAMESPACE)
	@echo ""
	@echo "=== OTel Collector ConfigMap hash ==="
	kubectl get configmap otel-collector-config -n $(NAMESPACE) \
	  -o jsonpath='{.metadata.annotations.observability\.demo\.dev/config-hash}' 2>/dev/null && echo ""

# ------------------------------------------------------------------
# Signal generation (assumes port-forward is running)
# ------------------------------------------------------------------
PORT ?= 8080
BASE := http://localhost:$(PORT)

.PHONY: simulate-slow
simulate-slow:
	@echo "Generating slow trace (2s)..."
	curl -s $(BASE)/api/v1/simulate/slow | jq .

.PHONY: simulate-error
simulate-error:
	@echo "Generating error trace..."
	curl -s $(BASE)/api/v1/simulate/error | jq .

.PHONY: load-test
load-test:
	@echo "Generating traffic (20 requests over 60s)..."
	@for i in $$(seq 1 10); do \
	  curl -s -X POST $(BASE)/api/v1/widgets \
	    -H 'Content-Type: application/json' \
	    -d "{\"name\":\"widget-$$i\",\"color\":\"blue\",\"weight\":$$i}" > /dev/null; \
	  curl -s $(BASE)/api/v1/simulate/slow > /dev/null & \
	  sleep 6; \
	done
	@echo "Done — check SigNoz for traces"
