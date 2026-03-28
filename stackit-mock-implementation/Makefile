# ══════════════════════════════════════════════════════════════════════════════
#  STACKIT Telemetry Router — Local Demo  (Docker Desktop Kubernetes)
# ══════════════════════════════════════════════════════════════════════════════
#
#  Quick start:
#    make deploy-all        # build images + deploy everything
#    make port-forward      # open all localhost tunnels (runs in background)
#    make create-router     # apply the default TelemetryRouter CR
#    make simulate          # generate test traffic
#    make status            # show pod / CR health at a glance
#
#  Teardown:
#    make teardown          # delete all K8s resources (keeps images cached)
#
# ══════════════════════════════════════════════════════════════════════════════

OPERATOR_IMAGE     := stackit-mock/operator:local
CUSTOMER_IMAGE     := stackit-mock/price-service:local
OPERATOR_DIR       := ./operator
CUSTOMER_DIR       := ./customer-app
K8S_DIR            := ./k8s
SCRIPTS_DIR        := ./scripts
OPERATOR_NAMESPACE := telemetry-system
NODE_ARCH          := $(shell kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null)
HOST_ARCH_RAW      := $(shell uname -m)
HOST_ARCH          := $(if $(filter aarch64 arm64,$(HOST_ARCH_RAW)),arm64,$(if $(filter x86_64 amd64,$(HOST_ARCH_RAW)),amd64,$(HOST_ARCH_RAW)))
IMAGE_ARCH         ?= $(if $(NODE_ARCH),$(NODE_ARCH),$(HOST_ARCH))
IMAGE_PLATFORM     ?= linux/$(IMAGE_ARCH)
SIM_BUFFER_DURATION ?= 90
SIM_BUFFER_PARALLEL ?= 40
API_PORT           := 8080
GRAFANA_PORT       := 3000
PROMETHEUS_PORT    := 9090
CUSTOMER_PORT      := 8000

.PHONY: all build-images build-operator build-customer \
        load-images \
        deploy-all deploy-crds deploy-namespaces deploy-observability \
        deploy-operator deploy-customer-app create-router \
        hotfix-operator hotfix-customer hotfix-observability \
        port-forward stop-port-forward \
        simulate simulate-buffer test status logs \
        apply-opensearch teardown-opensearch \
        teardown teardown-router teardown-observability \
        help

# ── Default ───────────────────────────────────────────────────────────────────

all: help

# ── Image builds ─────────────────────────────────────────────────────────────

## build-images: Build both Docker images (operator + customer app)
build-images: build-operator build-customer

## load-images: Import locally-built images into Docker Desktop Kubernetes node runtime
load-images:
	@echo "▶  Loading local images into Kubernetes node runtime"
	@NODE=$$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null); \
	if [ -z "$$NODE" ]; then \
	    echo "   Could not detect Kubernetes node name"; \
	    exit 1; \
	fi; \
	echo "   Node: $$NODE"; \
	docker save $(OPERATOR_IMAGE) | docker exec -i $$NODE ctr -n k8s.io images import - >/dev/null; \
	docker save $(CUSTOMER_IMAGE) | docker exec -i $$NODE ctr -n k8s.io images import - >/dev/null; \
	echo "   Images loaded"

## build-operator: Build the Go operator image (runs go mod tidy inside Docker)
build-operator:
	@echo "▶  Building operator image: $(OPERATOR_IMAGE)"
	docker build --platform $(IMAGE_PLATFORM) -t $(OPERATOR_IMAGE) $(OPERATOR_DIR)

## build-customer: Build the Python price-service image
build-customer:
	@echo "▶  Building customer app image: $(CUSTOMER_IMAGE)"
	docker build --platform $(IMAGE_PLATFORM) -t $(CUSTOMER_IMAGE) $(CUSTOMER_DIR)

# ── Full deploy ───────────────────────────────────────────────────────────────

## deploy-all: Build images and deploy every component to Docker Desktop K8s
deploy-all: build-images load-images deploy-namespaces deploy-crds deploy-observability deploy-operator deploy-customer-app
	@echo ""
	@echo "✅  All components deployed!"
	@echo ""
	@echo "   Next steps:"
	@echo "     make create-router    # create the default TelemetryRouter CR"
	@echo "     make port-forward     # open localhost tunnels"
	@echo "     make simulate         # generate test traffic"
	@echo "     make status           # check pod health"
	@echo ""

## deploy-namespaces: Ensure all three namespaces exist and are Active
deploy-namespaces:
	@echo "▶  Ensuring namespaces exist..."
	@for ns in customer-app telemetry-system observability; do \
	    phase=$$(kubectl get namespace $$ns -o jsonpath='{.status.phase}' 2>/dev/null || echo "Missing"); \
	    if [ "$$phase" = "Terminating" ]; then \
	        echo "   $$ns is Terminating — waiting up to 90s for deletion to finish..."; \
	        kubectl wait --for=delete namespace/$$ns --timeout=90s 2>/dev/null || true; \
	        phase="Missing"; \
	    fi; \
	    if [ "$$phase" = "Missing" ]; then \
	        echo "   Creating namespace: $$ns"; \
	        kubectl create namespace $$ns; \
	    else \
	        echo "   Namespace $$ns already Active — skipping"; \
	    fi; \
	done

## deploy-crds: Install TelemetryRouter CRD
deploy-crds:
	@echo "▶  Installing CRDs"
	kubectl apply -f $(K8S_DIR)/01-crds.yaml
	@echo "   Waiting for CRD to be established..."
	kubectl wait --for=condition=established --timeout=30s \
	    crd/telemetryrouters.telemetry.stackit.local

## deploy-observability: Deploy Prometheus, Loki, Tempo, Grafana
deploy-observability: deploy-namespaces
	@echo "▶  Deploying observability stack (Prometheus / Loki / Tempo / Grafana)"
	kubectl apply -f $(K8S_DIR)/observability/prometheus.yaml
	kubectl apply -f $(K8S_DIR)/observability/loki.yaml
	kubectl apply -f $(K8S_DIR)/observability/tempo.yaml
	kubectl apply -f $(K8S_DIR)/observability/grafana.yaml
	@echo "   Waiting for observability pods to be ready (up to 120s)..."
	kubectl rollout status deployment/prometheus -n observability --timeout=120s || true
	kubectl rollout status deployment/loki        -n observability --timeout=120s || true
	kubectl rollout status deployment/tempo       -n observability --timeout=240s || true
	kubectl rollout status deployment/grafana     -n observability --timeout=120s || true

## deploy-operator: Deploy RBAC + operator (expects image already built)
deploy-operator: deploy-namespaces deploy-crds
	@echo "▶  Deploying telemetry operator"
	kubectl apply -f $(K8S_DIR)/operator/rbac.yaml
	kubectl apply -f $(K8S_DIR)/operator/deployment.yaml
	@echo "   Waiting for operator to be ready..."
	kubectl rollout status deployment/telemetry-operator \
	    -n $(OPERATOR_NAMESPACE) --timeout=90s

## deploy-customer-app: Deploy the price-service into customer-app namespace
deploy-customer-app: deploy-namespaces
	@echo "▶  Deploying price-service customer app"
	kubectl apply -f $(K8S_DIR)/customer-app/deployment.yaml
	@echo "   Waiting for price-service to be ready..."
	kubectl rollout status deployment/price-service \
	    -n customer-app --timeout=90s || true
	@kubectl get pods -n customer-app -l app=price-service

# ── Hotfix rebuilds (no full teardown required) ───────────────────────────────

## hotfix-operator: Rebuild operator image + rolling-restart (keeps CR and Vector alive)
hotfix-operator: build-operator load-images
	@echo "▶  Rolling restart of telemetry-operator to pick up new image"
	kubectl rollout restart deployment/telemetry-operator -n $(OPERATOR_NAMESPACE)
	kubectl rollout status  deployment/telemetry-operator -n $(OPERATOR_NAMESPACE) --timeout=90s

## hotfix-operator-and-validate: Rebuild operator image + restart + dump diagnostics for vector pod
hotfix-operator-and-validate: hotfix-operator
	@echo "▶  Wait for vector pods to settle"
	kubectl rollout status deployment -n $(OPERATOR_NAMESPACE) -l app=vector --timeout=90s || true
	@echo "▶  Pod summary"
	kubectl get pods -n $(OPERATOR_NAMESPACE) -l app=vector
	@POD=$$(kubectl get pod -n $(OPERATOR_NAMESPACE) -l app=vector -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true); \
	if [ -n "$$POD" ]; then \
	  echo "▶  Logs for $$POD"; \
	  kubectl logs -n $(OPERATOR_NAMESPACE) $$POD --tail=80 || true; \
	  echo "▶  Pod yaml for $$POD (first 120 lines)"; \
	  kubectl get pod $$POD -n $(OPERATOR_NAMESPACE) -o yaml | sed -n '1,120p' || true; \
	  echo "▶  Vector config from $$POD"; \
	  kubectl exec -n $(OPERATOR_NAMESPACE) -c vector $$POD -- cat /etc/vector/vector.toml || true; \
	else \
	  echo "No vector pod found to inspect"; \
	fi

## hotfix-customer: Rebuild customer-app image + rolling-restart
hotfix-customer: build-customer load-images
	@echo "▶  Applying updated customer-app deployment manifest"
	kubectl apply -f $(K8S_DIR)/customer-app/deployment.yaml
	kubectl rollout restart deployment/price-service -n customer-app
	kubectl rollout status  deployment/price-service -n customer-app --timeout=90s

## hotfix-observability: Re-apply observability manifests (Prometheus config change etc.)
hotfix-observability:
	@echo "▶  Re-applying observability stack manifests"
	kubectl apply -f $(K8S_DIR)/observability/prometheus.yaml
	kubectl apply -f $(K8S_DIR)/observability/loki.yaml
	kubectl apply -f $(K8S_DIR)/observability/tempo.yaml
	kubectl apply -f $(K8S_DIR)/observability/grafana.yaml
	kubectl rollout restart deployment/prometheus -n observability
	kubectl rollout status  deployment/prometheus -n observability --timeout=60s

# ── TelemetryRouter CR ────────────────────────────────────────────────────────

## create-router: Apply the default TelemetryRouter CR (main-router)
create-router:
	@echo "▶  Creating TelemetryRouter CR: main-router"
	kubectl apply -f $(K8S_DIR)/telemetry-router-cr.yaml
	@echo "   Waiting for router to reach Ready phase..."
	@for i in $$(seq 1 20); do \
	    phase=$$(kubectl get telemetryrouter main-router \
	                -n $(OPERATOR_NAMESPACE) \
	                -o jsonpath='{.status.phase}' 2>/dev/null || echo ""); \
	    echo "   phase=$$phase"; \
	    if [ "$$phase" = "Ready" ]; then break; fi; \
	    sleep 3; \
	done
	@echo ""
	kubectl get telemetryrouter -n $(OPERATOR_NAMESPACE)

## delete-router: Delete the default TelemetryRouter CR
delete-router:
	kubectl delete -f $(K8S_DIR)/telemetry-router-cr.yaml --ignore-not-found

# ── OpenSearch optional sink ──────────────────────────────────────────────────

## apply-opensearch: Deploy OpenSearch, patch TelemetryRouter to add it as a sink, validate
## apply-opensearch: Demonstrates the core operator use-case: a customer calls the API to
## apply-opensearch: add a new log sink and the operator reconciles without any restarts.
apply-opensearch: deploy-namespaces
	@echo ""
	@echo "════════════════════════════════════════════════════════"
	@echo "  Step 0 — Update CRD (add OPENSEARCH to allowed types)"
	@echo "════════════════════════════════════════════════════════"
	kubectl apply -f $(K8S_DIR)/01-crds.yaml
	kubectl wait --for=condition=established --timeout=30s \
	    crd/telemetryrouters.telemetry.stackit.local

	@echo ""
	@echo "════════════════════════════════════════════════════════"
	@echo "  Step 1 — Deploy OpenSearch"
	@echo "════════════════════════════════════════════════════════"
	kubectl apply -f $(K8S_DIR)/observability/opensearch.yaml
	@echo "   Waiting for OpenSearch to be ready (up to 3 minutes)..."
	kubectl rollout status deployment/opensearch -n observability --timeout=180s

	@echo ""
	@echo "════════════════════════════════════════════════════════"
	@echo "  Step 2 — Update Grafana provisioning"
	@echo "════════════════════════════════════════════════════════"
	@echo "   Re-applying grafana.yaml to pick up OpenSearch datasource + dashboard..."
	kubectl apply -f $(K8S_DIR)/observability/grafana.yaml
	kubectl rollout restart deployment/grafana -n observability
	kubectl rollout status  deployment/grafana -n observability --timeout=60s

	@echo ""
	@echo "════════════════════════════════════════════════════════"
	@echo "  Step 3 — Add OpenSearch sink to main-router"
	@echo "════════════════════════════════════════════════════════"
	@echo "   Appending opensearch destination to TelemetryRouter spec..."
	@echo "   The operator detects the spec change, re-renders the Vector config,"
	@echo "   and rolls a new Vector pod — no manual restart needed."
	kubectl patch telemetryrouter main-router -n $(OPERATOR_NAMESPACE) \
	    --type=json \
	    -p='[{"op":"add","path":"/spec/destinations/-","value":{"name":"opensearch","type":"OPENSEARCH","endpoint":"http://opensearch.observability.svc.cluster.local:9200"}}]'
	@echo "   Patch applied — new spec:"
	kubectl get tr main-router -n $(OPERATOR_NAMESPACE) \
	    -o jsonpath='{range .spec.destinations[*]}  {.type}: {.name}{"\n"}{end}'

	@echo ""
	@echo "════════════════════════════════════════════════════════"
	@echo "  Step 4 — Waiting for operator to reconcile"
	@echo "════════════════════════════════════════════════════════"
	@echo "   Giving the operator 30s to render new Vector config and"
	@echo "   roll a new Vector pod with the opensearch sink..."
	@sleep 25
	kubectl rollout status deployment -n $(OPERATOR_NAMESPACE) -l app=vector --timeout=60s || true

	@echo ""
	@echo "════════════════════════════════════════════════════════"
	@echo "  Step 5 — Generate traffic & validate"
	@echo "════════════════════════════════════════════════════════"
	@echo "   Sending a burst of test events..."
	@curl -s http://localhost:$(CUSTOMER_PORT)/simulate >/dev/null 2>&1 \
	    || echo "   (price-service unreachable — run 'make port-forward' first, then re-run validation below)"
	@echo "   Waiting 10s for events to reach OpenSearch..."
	@sleep 10
	@echo "   Querying OpenSearch document count..."
	@COUNT=$$(kubectl exec -n observability deploy/opensearch -- \
	    curl -s http://localhost:9200/telemetry-logs-*/_count 2>/dev/null | \
	    python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('count', 0))" 2>/dev/null || echo "0"); \
	if [ "$$COUNT" -gt "0" ]; then \
	    echo ""; \
	    echo "✅  OpenSearch index has $$COUNT documents — logs are flowing!"; \
	else \
	    echo ""; \
	    echo "⚠️  No documents yet. OpenSearch may still be warming up."; \
	    echo "   Retry manually:"; \
	    echo "     kubectl exec -n observability deploy/opensearch -- \\"; \
	    echo "       curl -s http://localhost:9200/telemetry-logs-*/_count"; \
	fi
	@echo ""
	@echo "════════════════════════════════════════════════════════"
	@echo "  Done — open Grafana to see the data"
	@echo "════════════════════════════════════════════════════════"
	@echo "   Grafana → http://localhost:$(GRAFANA_PORT)"
	@echo "   Folder  → STACKIT Mock → OpenSearch Logs"
	@echo ""
	@echo "   Or query OpenSearch directly:"
	@echo "     kubectl exec -n observability deploy/opensearch -- \\"
	@echo "       curl -s 'http://localhost:9200/telemetry-logs-*/_search?size=3&pretty'"
	@echo ""

## teardown-opensearch: Remove OpenSearch from the cluster (leaves TelemetryRouter intact)
teardown-opensearch:
	@echo "▶  Removing OpenSearch from observability namespace..."
	kubectl delete -f $(K8S_DIR)/observability/opensearch.yaml --ignore-not-found
	@echo "   Note: TelemetryRouter still has opensearch in destinations."
	@echo "   Vector will log sink errors until you remove it. To revert:"
	@echo "     make create-router   # re-applies the base CR without opensearch"

# ── Port forwarding ───────────────────────────────────────────────────────────

## port-forward: Open localhost tunnels for all services (background processes)
port-forward: stop-port-forward
	@echo "▶  Starting port-forwards (background)"
	@bash $(SCRIPTS_DIR)/port-forward.sh start
	@echo ""
	@echo "   Grafana     → http://localhost:$(GRAFANA_PORT)    (user: admin / pass: admin)"
	@echo "   Prometheus  → http://localhost:$(PROMETHEUS_PORT)"
	@echo "   Operator API→ http://localhost:$(API_PORT)"
	@echo "   Price Svc   → http://localhost:$(CUSTOMER_PORT)"
	@echo ""
	@echo "   Run 'make stop-port-forward' to kill all tunnels."

## stop-port-forward: Kill all port-forward background processes
stop-port-forward:
	@pkill -f "kubectl port-forward" 2>/dev/null && echo "   Stopped existing port-forwards" || true

# ── Traffic generation ────────────────────────────────────────────────────────

## simulate: Hit the /simulate endpoint to generate a burst of test traffic
simulate:
	@echo "▶  Generating test traffic via /simulate endpoint..."
	curl -s http://localhost:$(CUSTOMER_PORT)/simulate | python3 -m json.tool || \
	    echo "   (price-service not reachable — is 'make port-forward' running?)"

## simulate-buffer: Scale Loki down, run in-cluster load, validate Vector backpressure signals, restore Loki
simulate-buffer:
	@DUR=$(SIM_BUFFER_DURATION); PAR=$(SIM_BUFFER_PARALLEL); \
	set -e; \
	cleanup() { \
	    echo "▶  Restoring Loki and port-forwards..."; \
	    kubectl scale deploy/loki -n observability --replicas=1 >/dev/null 2>&1 || true; \
	    kubectl rollout status deployment/loki -n observability --timeout=120s >/dev/null 2>&1 || true; \
	    $(MAKE) port-forward >/dev/null 2>&1 || true; \
	    kubectl delete pod loadgen-buffer -n customer-app --ignore-not-found >/dev/null 2>&1 || true; \
	}; \
	trap cleanup EXIT; \
	echo "▶  Simulating sink outage: scaling Loki to 0 replicas"; \
	kubectl scale deploy/loki -n observability --replicas=0; \
	kubectl get endpoints -n observability loki; \
	echo "▶  Starting in-cluster loadgen pod (duration=$$DUR s, parallel=$$PAR)"; \
	kubectl delete pod loadgen-buffer -n customer-app --ignore-not-found >/dev/null 2>&1 || true; \
	kubectl run -n customer-app loadgen-buffer --restart=Never --image=curlimages/curl -- \
	    sh -c "for t in \$$(seq 1 $$DUR); do for i in \$$(seq 1 $$PAR); do curl -s http://price-service:8000/simulate >/dev/null & done; wait; done"; \
	kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/loadgen-buffer -n customer-app --timeout=$$((DUR + 60))s || true; \
	echo "▶  Validating backpressure-related signals"; \
	BUFFER_LINES=$$(curl -s http://localhost:9598/metrics | rg 'vector_buffer_(events|byte)_' | wc -l | tr -d ' '); \
	RETRY_LINES=$$(kubectl logs -n $(OPERATOR_NAMESPACE) -l app=vector --tail=400 | rg -i 'sink_loki|Retrying after error|Connection refused|Events dropped' | wc -l | tr -d ' '); \
	echo "   Vector buffer metric lines found: $$BUFFER_LINES"; \
	echo "   Vector retry/error log lines found: $$RETRY_LINES"; \
	if [ "$$RETRY_LINES" -gt 0 ]; then \
	    echo "✅  Backpressure signal detected (sink retries/errors present)."; \
	else \
	    echo "⚠️  No sink retry/error lines found in recent logs; try higher load."; \
	fi

## test: Run the full test script (checks all endpoints, then validates telemetry)
test:
	@bash $(SCRIPTS_DIR)/test.sh

# ── Status ────────────────────────────────────────────────────────────────────

## status: Show a quick health summary of all pods and the TelemetryRouter CR
status:
	@echo "══════════════════════════════════════════════════════"
	@echo "  Pods"
	@echo "══════════════════════════════════════════════════════"
	@kubectl get pods -A -l 'app in (price-service,telemetry-operator,vector,prometheus,loki,tempo,grafana)' \
	    --no-headers 2>/dev/null | awk '{printf "  %-20s %-30s %-10s\n", $$1, $$2, $$4}'
	@echo ""
	@echo "══════════════════════════════════════════════════════"
	@echo "  TelemetryRouter CRs"
	@echo "══════════════════════════════════════════════════════"
	@kubectl get telemetryrouter -n $(OPERATOR_NAMESPACE) 2>/dev/null || echo "  (none)"
	@echo ""
	@echo "══════════════════════════════════════════════════════"
	@echo "  Vector Deployments"
	@echo "══════════════════════════════════════════════════════"
	@kubectl get deployment -n $(OPERATOR_NAMESPACE) -l app=vector 2>/dev/null || echo "  (none)"

# ── Logs ──────────────────────────────────────────────────────────────────────

## logs-operator: Stream operator logs
logs-operator:
	kubectl logs -n $(OPERATOR_NAMESPACE) -l app=telemetry-operator -f --tail=50

## logs-vector: Stream Vector logs (managed by operator)
logs-vector:
	kubectl logs -n $(OPERATOR_NAMESPACE) -l app=vector -f --tail=50

## logs-customer: Stream price-service logs
logs-customer:
	kubectl logs -n customer-app -l app=price-service -f --tail=50

# ── Teardown ──────────────────────────────────────────────────────────────────

## teardown: Delete all K8s resources (namespaces + everything inside them)
teardown: stop-port-forward
	@echo "▶  Tearing down STACKIT mock deployment..."
	kubectl delete namespace customer-app    --ignore-not-found --timeout=60s &
	kubectl delete namespace telemetry-system --ignore-not-found --timeout=60s &
	kubectl delete namespace observability   --ignore-not-found --timeout=60s &
	wait
	@echo "   Removing CRDs..."
	kubectl delete crd telemetryrouters.telemetry.stackit.local --ignore-not-found
	@echo ""
	@echo "✅  Teardown complete."

# ── Help ──────────────────────────────────────────────────────────────────────

## help: List all available make targets with descriptions
help:
	@echo ""
	@echo "STACKIT Telemetry Router — Local Demo"
	@echo "════════════════════════════════════════════════════════"
	@grep -E '^## [a-zA-Z_-]+:' $(MAKEFILE_LIST) | \
	    sed 's/## //' | \
	    awk -F: '{printf "  %-25s %s\n", $$1, $$2}'
	@echo ""
