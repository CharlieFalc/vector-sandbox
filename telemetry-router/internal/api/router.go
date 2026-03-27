package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"telemetry-router/internal/store"
)

// NewRouter wires up all routes using only the standard library.
// In production, use chi or gorilla/mux for cleaner path param extraction.
func NewRouter(s *store.Store) http.Handler {
	h := NewHandler(s)
	mux := http.NewServeMux()

	// Collection routes
	mux.Handle("/v2/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route(w, r, h)
	}))

	return mux
}

// route dispatches to the correct handler by matching URL patterns manually.
// This keeps the example dependency-free while demonstrating the routing logic.
func route(w http.ResponseWriter, r *http.Request, h *Handler) {
	// Pattern:
	//   /v2/projects/{projectId}/regions/{region}/instances/{instanceId}/destinations
	//   /v2/projects/{projectId}/regions/{region}/instances/{instanceId}/destinations/{destinationId}
	//   /v2/projects/{projectId}/regions/{region}/instances/{instanceId}/destinations/{destinationId}/health

	destCollectionRE := regexp.MustCompile(
		`^/v2/projects/([^/]+)/regions/([^/]+)/instances/([^/]+)/destinations$`)
	destItemRE := regexp.MustCompile(
		`^/v2/projects/([^/]+)/regions/([^/]+)/instances/([^/]+)/destinations/([^/]+)$`)
	destHealthRE := regexp.MustCompile(
		`^/v2/projects/([^/]+)/regions/([^/]+)/instances/([^/]+)/destinations/([^/]+)/health$`)

	path := r.URL.Path
	path = strings.TrimRight(path, "/")

	// Inject path vars into context so handlers can read them uniformly.
	withVars := func(req *http.Request, vars map[string]string) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), pathVarsKey{}, vars))
	}

	if m := destHealthRE.FindStringSubmatch(path); m != nil && r.Method == http.MethodGet {
		r = withVars(r, map[string]string{
			"projectId": m[1], "region": m[2], "instanceId": m[3], "destinationId": m[4],
		})
		h.GetDestinationHealth(w, r)
		return
	}

	if m := destItemRE.FindStringSubmatch(path); m != nil {
		r = withVars(r, map[string]string{
			"projectId": m[1], "region": m[2], "instanceId": m[3], "destinationId": m[4],
		})
		switch r.Method {
		case http.MethodGet:
			h.GetDestination(w, r)
		case http.MethodPut:
			h.UpdateDestination(w, r)
		case http.MethodDelete:
			h.DeleteDestination(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	if m := destCollectionRE.FindStringSubmatch(path); m != nil {
		r = withVars(r, map[string]string{
			"projectId": m[1], "region": m[2], "instanceId": m[3],
		})
		switch r.Method {
		case http.MethodGet:
			h.ListDestinations(w, r)
		case http.MethodPost:
			h.CreateDestination(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	http.NotFound(w, r)
}
