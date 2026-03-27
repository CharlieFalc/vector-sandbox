// main is the entrypoint for the Transform Registry server.
//
// In addition to the API routes it also serves:
//   - GET /docs               → embedded Swagger UI (CDN-loaded, no extra deps)
//   - GET /api/openapi.yaml   → the raw OpenAPI spec (consumed by Swagger UI)
//   - GET /health             → liveness check
package main

import (
	"context"
	"embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"transform-registry/internal/handler"
	"transform-registry/internal/store"
)

// specFS embeds the OpenAPI spec so the binary is fully self-contained.
// The Makefile copies api/openapi.yaml to cmd/server/openapi.yaml before
// building (make build), so the embed path stays within the package directory.
//
//go:embed openapi.yaml
var specFS embed.FS

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	port := envOrDefault("PORT", "8080")
	addr := ":" + port

	// ------------------------------------------------------------------
	// Wire up dependencies
	// ------------------------------------------------------------------
	s := store.NewMemoryStore()
	h := handler.New(s, nil) // nil → uses default VRL compilation simulator

	// ------------------------------------------------------------------
	// Routes
	// ------------------------------------------------------------------
	mux := http.NewServeMux()

	// Business API
	handler.RegisterRoutes(mux, h)

	// OpenAPI spec — served at the path Swagger UI expects.
	mux.HandleFunc("GET /api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		data, err := specFS.ReadFile("openapi.yaml")
		if err != nil {
			http.Error(w, "spec not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Access-Control-Allow-Origin", "*") // allow Swagger UI to fetch it
		_, _ = w.Write(data)
	})

	// Swagger UI — served inline using CDN assets.
	// No extra npm/docker/tooling required.
	mux.HandleFunc("GET /docs", swaggerUIHandler)
	mux.HandleFunc("GET /docs/", swaggerUIHandler) // trailing-slash redirect

	// Health check
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// ------------------------------------------------------------------
	// Server
	// ------------------------------------------------------------------
	srv := &http.Server{
		Addr:         addr,
		Handler:      loggingMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("=== Transform Registry ===")
		log.Printf("API:    http://localhost%s/v1/transforms", addr)
		log.Printf("Docs:   http://localhost%s/docs", addr)
		log.Printf("Spec:   http://localhost%s/api/openapi.yaml", addr)
		log.Printf("Health: http://localhost%s/health", addr)
		log.Printf("")
		log.Printf("Example requests:")
		log.Printf("  curl -s -X POST http://localhost%s/v1/transforms \\", addr)
		log.Printf(`    -H 'Content-Type: application/json' \`)
		log.Printf(`    -d '{"name":"redact-emails","vrl":".message = replace(.message, r\"\\\\b[\\\\w.]+@[\\\\w.]+\\\\b\", \"[REDACTED]\")"}' | jq`)
		log.Printf("")

		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// ------------------------------------------------------------------
	// Graceful shutdown
	// ------------------------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutdown signal received, draining...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	log.Println("shutdown complete")
}

// swaggerUIHandler serves an HTML page that loads Swagger UI from the CDN
// and points it at the local /api/openapi.yaml endpoint.
func swaggerUIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Transform Registry API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>
    body { margin: 0; background: #fafafa; }
    .topbar { background: #1b1b2f !important; }
    .topbar-wrapper img { content: url('data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg"/>'); }
  </style>
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  SwaggerUIBundle({
    url: "/api/openapi.yaml",
    dom_id: '#swagger-ui',
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
    layout: "BaseLayout",
    deepLinking: true,
    tryItOutEnabled: true,
    requestInterceptor: (req) => { req.headers['Accept'] = 'application/json'; return req; }
  });
</script>
</body>
</html>
`))
}

// loggingMiddleware logs method, path, and latency for every request.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("[http] %s %s  %dms", r.Method, r.URL.Path, time.Since(start).Milliseconds())
	})
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
