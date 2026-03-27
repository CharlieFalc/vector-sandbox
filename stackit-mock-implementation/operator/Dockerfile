# ── Stage 1: dependency fetch + build ────────────────────────────────────────
# We run `go mod tidy` here so the image build works without a pre-generated
# go.sum file on the host (handy for Docker Desktop / CI environments).
FROM golang:1.22-alpine AS builder

WORKDIR /workspace

# Copy module manifest first so this layer is cached unless deps change
COPY go.mod ./

# Download deps and generate go.sum in one shot
RUN go mod download || true

# Copy the rest of the source
COPY . .

# Tidy (generates go.sum from scratch if missing) then build a statically-linked binary
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /operator ./main.go

# ── Stage 2: minimal runtime image ───────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /operator /operator

USER 65532:65532

ENTRYPOINT ["/operator"]
