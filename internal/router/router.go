// Package router provides the HTTP router for ociproxy.
package router

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/planlx/ociproxy/internal/config"
	"github.com/planlx/ociproxy/internal/proxy"
)

// New creates the main HTTP router.
func New(cfg *config.Config, px *proxy.Handler, logger *slog.Logger) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle("/", &v2Handler{proxy: px, mux: mux, logger: logger})
	return &loggingMiddleware{next: mux, logger: logger}, nil
}

// loggingMiddleware logs method, path, status, and duration for each request.
type loggingMiddleware struct {
	next   http.Handler
	logger *slog.Logger
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (m *loggingMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	originalPath := r.URL.Path
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	m.next.ServeHTTP(rec, r)
	elapsed := time.Since(start)

	m.logger.Info("request",
		"method", r.Method,
		"path", originalPath,
		"status", rec.status,
		"duration_ms", elapsed.Milliseconds(),
	)
}

// v2Handler routes /v2/ requests to the proxy and handles everything else.
type v2Handler struct {
	proxy  *proxy.Handler
	mux    *http.ServeMux
	logger *slog.Logger
}

func (h *v2Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Docker daemon registry-mirror prefix: /docker/<registry>/v2/...
	// E.g. Docker sends: /docker/docker.io/v2/library/nginx/manifests/latest
	if dockerPath, ok := stripDockerPrefix(r.URL.Path); ok {
		r.URL.Path = dockerPath
		h.logger.Debug("docker mirror prefix", "original", r.URL.Path, "rewritten", dockerPath)
		h.proxy.ServeHTTP(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v2/") || r.URL.Path == "/v2" {
		h.proxy.ServeHTTP(w, r)
		return
	}

	// Health endpoint
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}

	// Root redirect
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		http.Redirect(w, r, "https://github.com/planlx/ociproxy", http.StatusFound)
		return
	}

	http.NotFound(w, r)
}

// stripDockerPrefix detects and strips Docker daemon's registry-mirror prefix.
// Format: /docker/<registry>/v2/... -> /v2/...
// Returns the rewritten path and true if matched, otherwise ("", false).
func stripDockerPrefix(path string) (string, bool) {
	if !strings.HasPrefix(path, "/docker/") {
		return "", false
	}
	rest := strings.TrimPrefix(path, "/docker/")
	idx := strings.Index(rest, "/v2")
	if idx <= 0 {
		return "", false
	}
	// Validate that after /v2 it's either end or /
	after := rest[idx+3:]
	if after != "" && after[0] != '/' {
		return "", false
	}
	return "/v2" + after, true
}
