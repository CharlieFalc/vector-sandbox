# =============================================================================
# Transform Registry — Makefile
# =============================================================================
# Prerequisites (installed by `make install-tools`):
#   - go 1.22+
#   - oapi-codegen  (https://github.com/oapi-codegen/oapi-codegen)
#
# Quickstart:
#   make install-tools
#   make run          ← starts server on :8080; docs at http://localhost:8080/docs
# =============================================================================

BINARY      := transform-registry
BIN_DIR     := bin
CMD         := ./cmd/server
SPEC        := api/openapi.yaml
GEN_FILE    := gen/types.gen.go
CODEGEN_CFG := oapi-codegen.yaml
PORT        ?= 8080

# Go toolchain
GOFLAGS     ?= -race
GOTESTFLAGS ?= -v -race -count=1

.DEFAULT_GOAL := help

# -----------------------------------------------------------------------------
# help — list all targets
# -----------------------------------------------------------------------------
.PHONY: help
help:
	@echo ""
	@echo "  Transform Registry — available targets"
	@echo ""
	@echo "  Setup"
	@echo "    install-tools   Install oapi-codegen code-generation CLI"
	@echo "    tidy            go mod tidy (sync go.sum)"
	@echo ""
	@echo "  Code generation"
	@echo "    generate        Re-generate Go types from api/openapi.yaml"
	@echo "    check-spec      Validate OpenAPI spec with swagger-cli (requires npm)"
	@echo ""
	@echo "  Development"
	@echo "    build           Compile binary to bin/$(BINARY)"
	@echo "    run             Build and run the server (PORT=$(PORT))"
	@echo "    watch           Run with hot-reload via air (install separately)"
	@echo ""
	@echo "  Testing"
	@echo "    test            Run all tests"
	@echo "    test-cover      Run tests with HTML coverage report"
	@echo "    test-store      Run only store unit tests"
	@echo "    test-handler    Run only handler integration tests"
	@echo ""
	@echo "  Documentation"
	@echo "    serve-docs      Open the OpenAPI spec in the online Swagger editor"
	@echo ""
	@echo "  Cleanup"
	@echo "    clean           Remove compiled binary"
	@echo "    clean-gen       Remove generated files (re-run make generate after)"
	@echo "    clean-all       Remove binary + generated files"
	@echo ""

# -----------------------------------------------------------------------------
# install-tools — install the oapi-codegen CLI
# -----------------------------------------------------------------------------
.PHONY: install-tools
install-tools:
	@echo "→ Installing oapi-codegen..."
	go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
	@echo "✓ oapi-codegen installed ($(shell oapi-codegen --version 2>/dev/null || echo 'check your PATH'))"

# -----------------------------------------------------------------------------
# tidy — sync go.sum with go.mod
# -----------------------------------------------------------------------------
.PHONY: tidy
tidy:
	go mod tidy

# -----------------------------------------------------------------------------
# generate — regenerate Go types from the OpenAPI spec
#
# The generated file is committed so the project compiles without running this
# target first, but you MUST re-run it whenever api/openapi.yaml changes.
# The //go:generate directive in gen/types.gen.go mirrors this target so
# `go generate ./...` also works.
# -----------------------------------------------------------------------------
.PHONY: generate
generate: $(SPEC)
	@echo "→ Generating Go types from $(SPEC)..."
	@mkdir -p gen
	oapi-codegen --config=$(CODEGEN_CFG) $(SPEC)
	@echo "✓ Generated $(GEN_FILE)"

# Mark the generated file as depending on the spec so Make detects staleness.
$(GEN_FILE): $(SPEC) $(CODEGEN_CFG)
	$(MAKE) generate

# -----------------------------------------------------------------------------
# check-spec — validate the OpenAPI spec (requires @apidevtools/swagger-cli)
# -----------------------------------------------------------------------------
.PHONY: check-spec
check-spec:
	@command -v swagger-cli >/dev/null 2>&1 || \
		{ echo "swagger-cli not found — run: npm install -g @apidevtools/swagger-cli"; exit 1; }
	swagger-cli validate $(SPEC)
	@echo "✓ Spec is valid"

# -----------------------------------------------------------------------------
# build — compile the server binary
# -----------------------------------------------------------------------------
.PHONY: build
build: $(GEN_FILE)
	@echo "→ Copying spec for go:embed..."
	@cp $(SPEC) cmd/server/openapi.yaml
	@echo "→ Building $(BIN_DIR)/$(BINARY)..."
	@mkdir -p $(BIN_DIR)
	go build $(GOFLAGS) -o $(BIN_DIR)/$(BINARY) $(CMD)
	@echo "✓ Binary: $(BIN_DIR)/$(BINARY)"

# -----------------------------------------------------------------------------
# run — build and start the server
# -----------------------------------------------------------------------------
.PHONY: run
run: build
	@echo "→ Starting server on :$(PORT)"
	@echo "  API:  http://localhost:$(PORT)/v1/transforms"
	@echo "  Docs: http://localhost:$(PORT)/docs"
	@echo ""
	PORT=$(PORT) ./$(BIN_DIR)/$(BINARY)

# -----------------------------------------------------------------------------
# watch — hot-reload via air (go install github.com/air-verse/air@latest)
# -----------------------------------------------------------------------------
.PHONY: watch
watch:
	@command -v air >/dev/null 2>&1 || \
		{ echo "air not found — run: go install github.com/air-verse/air@latest"; exit 1; }
	air

# -----------------------------------------------------------------------------
# test targets
# -----------------------------------------------------------------------------
.PHONY: test
test: $(GEN_FILE)
	go test $(GOTESTFLAGS) ./...

.PHONY: test-cover
test-cover: $(GEN_FILE)
	@mkdir -p $(BIN_DIR)
	go test $(GOTESTFLAGS) -coverprofile=$(BIN_DIR)/coverage.out ./...
	go tool cover -html=$(BIN_DIR)/coverage.out -o $(BIN_DIR)/coverage.html
	@echo "✓ Coverage report: $(BIN_DIR)/coverage.html"
	@open $(BIN_DIR)/coverage.html 2>/dev/null || xdg-open $(BIN_DIR)/coverage.html 2>/dev/null || true

.PHONY: test-store
test-store: $(GEN_FILE)
	go test $(GOTESTFLAGS) ./internal/store/...

.PHONY: test-handler
test-handler: $(GEN_FILE)
	go test $(GOTESTFLAGS) ./internal/handler/...

# -----------------------------------------------------------------------------
# serve-docs — open the spec in the online Swagger editor
# This avoids needing Docker or a local swagger-ui install.
# For a fully offline experience, run the server (make run) and visit /docs.
# -----------------------------------------------------------------------------
.PHONY: serve-docs
serve-docs:
	@echo "→ Opening spec in Swagger Editor..."
	@SPEC_URL="https://editor.swagger.io/?url=http://localhost:$(PORT)/api/openapi.yaml"; \
	 echo "  Make sure the server is running (make run) first."; \
	 echo "  Opening: $$SPEC_URL"; \
	 open "$$SPEC_URL" 2>/dev/null || xdg-open "$$SPEC_URL" 2>/dev/null || \
	     echo "  Could not auto-open browser — visit: $$SPEC_URL"

# -----------------------------------------------------------------------------
# clean targets
# -----------------------------------------------------------------------------
.PHONY: clean
clean:
	rm -rf $(BIN_DIR)
	rm -f cmd/server/openapi.yaml

.PHONY: clean-gen
clean-gen:
	rm -f $(GEN_FILE)
	@echo "✓ Removed $(GEN_FILE) — run 'make generate' to regenerate"

.PHONY: clean-all
clean-all: clean clean-gen
