# Multi-stage build for signal-forge app.
# Stage 1: compile
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Download dependencies first (cached layer)
COPY go.mod go.sum* ./
RUN go mod download

# Copy source
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o signal-forge ./cmd/signal-forge

# Stage 2: minimal runtime image
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /build/signal-forge /signal-forge

EXPOSE 8080

ENTRYPOINT ["/signal-forge"]
