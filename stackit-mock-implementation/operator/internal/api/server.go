package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	telemetryv1 "github.com/stackit-mock/operator/api/v1alpha1"
)

// Server exposes a simple REST API to manage TelemetryRouter CRs.
// This mirrors what the STACKIT control-plane API would do for customers.
type Server struct {
	client    client.Client
	namespace string
}

func NewServer(c client.Client, namespace string) *Server {
	return &Server{client: c, namespace: namespace}
}

// RegisterRoutes wires up all HTTP handlers on the given mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/v1/routers", s.handleRouters)
	mux.HandleFunc("/api/v1/routers/", s.handleRouterByName)
}

// ── Health ────────────────────────────────────────────────────────────────────

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── Collection: GET /api/v1/routers  POST /api/v1/routers ───────────────────

func (s *Server) handleRouters(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listRouters(w, r)
	case http.MethodPost:
		s.createRouter(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listRouters(w http.ResponseWriter, r *http.Request) {
	var list telemetryv1.TelemetryRouterList
	if err := s.client.List(r.Context(), &list, client.InNamespace(s.namespace)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list.Items)
}

func (s *Server) createRouter(w http.ResponseWriter, r *http.Request) {
	var spec telemetryv1.TelemetryRouterSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("query param 'name' is required"))
		return
	}

	tr := &telemetryv1.TelemetryRouter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
		},
		Spec: spec,
	}

	if err := s.client.Create(r.Context(), tr); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, fmt.Errorf("router %q already exists", name))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, tr)
}

// ── Item: GET PUT PATCH DELETE /api/v1/routers/{name} ────────────────────────

func (s *Server) handleRouterByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/routers/")
	if name == "" {
		http.Error(w, "router name required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRouter(w, r, name)
	case http.MethodPut:
		s.updateRouter(w, r, name)
	case http.MethodPatch:
		s.patchRouter(w, r, name)
	case http.MethodDelete:
		s.deleteRouter(w, r, name)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getRouter(w http.ResponseWriter, r *http.Request, name string) {
	var tr telemetryv1.TelemetryRouter
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.namespace}, &tr); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, fmt.Errorf("router %q not found", name))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

// updateRouter does a full spec replacement (PUT semantics).
func (s *Server) updateRouter(w http.ResponseWriter, r *http.Request, name string) {
	var spec telemetryv1.TelemetryRouterSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}

	var tr telemetryv1.TelemetryRouter
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.namespace}, &tr); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, fmt.Errorf("router %q not found", name))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	tr.Spec = spec
	if err := s.client.Update(r.Context(), &tr); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

// patchRouter supports partial updates to the spec (PATCH semantics).
// Body is a JSON object with the subset of TelemetryRouterSpec fields to update.
func (s *Server) patchRouter(w http.ResponseWriter, r *http.Request, name string) {
	var patch map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}

	patchBytes, err := json.Marshal(map[string]interface{}{"spec": patch})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var tr telemetryv1.TelemetryRouter
	tr.Name = name
	tr.Namespace = s.namespace

	if err := s.client.Patch(
		r.Context(), &tr,
		client.RawPatch(types.MergePatchType, patchBytes),
	); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, fmt.Errorf("router %q not found", name))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Re-fetch to return the updated object
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.namespace}, &tr); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

func (s *Server) deleteRouter(w http.ResponseWriter, r *http.Request, name string) {
	var tr telemetryv1.TelemetryRouter
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.namespace}, &tr); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, fmt.Errorf("router %q not found", name))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if err := s.client.Delete(r.Context(), &tr); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message": fmt.Sprintf("router %q deletion initiated (finalizer will drain)", name),
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// StartAPIServer starts the HTTP server on the given address.
// It blocks until ctx is cancelled.
func StartAPIServer(ctx context.Context, addr string, s *Server) error {
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("api server: %w", err)
	}
	return nil
}
