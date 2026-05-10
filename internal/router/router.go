// Package router provides the HTTP router for ociproxy.
package router

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/planlx/ociproxy/internal/config"
	"github.com/planlx/ociproxy/internal/proxy"
)

// New creates the main HTTP router.
func New(cfg *config.Config, logger *slog.Logger) (http.Handler, error) {
	px := proxy.New(cfg, logger)

	mux := http.NewServeMux()

	// All /v2/ requests go through the proxy handler.
	// The handler internally dispatches based on method and path.
	mux.Handle("/", &v2Handler{proxy: px, mux: mux})

	return mux, nil
}

// v2Handler routes /v2/ requests to the proxy and handles everything else.
type v2Handler struct {
	proxy *proxy.Handler
	mux   *http.ServeMux
}

func (h *v2Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v2/") || r.URL.Path == "/v2" {
		h.proxy.ServeHTTP(w, r)
		return
	}

	// Health endpoint
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}

	// Root redirect
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		http.Redirect(w, r, "https://github.com/planlx/ociproxy", http.StatusFound)
		return
	}

	http.NotFound(w, r)
}
