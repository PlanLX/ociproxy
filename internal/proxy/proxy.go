// Package proxy provides stateless OCI registry proxy logic.
// It forwards requests to upstream registries without caching or storage.
package proxy

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/planlx/ociproxy/internal/config"
)

var (
	clientsMu sync.Mutex
	clients   = make(map[string]*http.Client)
)

// upstreamClient returns a dedicated HTTP client for the given mirror,
// keyed by URL to enable connection reuse.
func upstreamClient(url string) *http.Client {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	if c, ok := clients[url]; ok {
		return c
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		Proxy:           http.ProxyFromEnvironment,
	}

	// Wrap with Bearer auth transport for anonymous or cred-based auth
	at := &authTransport{
		base:       tr,
		hostURL:    url,
		tokenCache: make(map[string]*tokenEntry),
	}

	c := &http.Client{Transport: at}

	clients[url] = c
	return c
}

// CloseAll closes all upstream HTTP clients.
func CloseAll() {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	for _, c := range clients {
		c.CloseIdleConnections()
	}
	clients = make(map[string]*http.Client)
}

// Handler is the stateless OCI registry proxy.
type Handler struct {
	cfg    *config.Config
	logger *slog.Logger
}

// New creates a new proxy handler.
func New(cfg *config.Config, logger *slog.Logger) *Handler {
	return &Handler{
		cfg:    cfg,
		logger: logger,
	}
}

// ServeHTTP proxies the request to the appropriate upstream mirror.
//
// URL formats:
//   /v2/<registry>/<image>/manifests/<tag>    — e.g. /v2/docker.io/library/busybox/manifests/latest
//   /v2/<registry>/<image>/blobs/<digest>     — e.g. /v2/docker.io/library/busybox/blobs/sha256:abc123
//   /v2/<registry>/<image>/tags/list          — list tags
//
// When no registry prefix matches a configured namespace:
//   - If the first segment contains a dot (hostname-like), it's treated as the registry name,
//     and the request is routed to the default registry's mirror.
//   - If the first segment doesn't contain a dot, it's part of the image name,
//     and the default registry is used.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		h.serveBase(w, r)
		return
	}

	// Parse the path to determine the target registry and upstream mirrors
	info := h.parsePath(r.URL.Path)
	if info == nil {
		h.logger.Warn("bad request path", "path", r.URL.Path)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Get all mirrors for this registry (best first, fallbacks after)
	mirrors := info.resolveMirrors(h.cfg)
	if len(mirrors) == 0 {
		h.logger.Warn("unknown registry", "registry", info.Registry)
		http.Error(w, "registry not found", http.StatusNotFound)
		return
	}

	// Try each mirror in order, failing over on connection errors
	var lastErr error
	for _, mt := range mirrors {
		mirrorURL := trimTrailingSlash(mt.mirror.URL)
		tPath := targetPath(mt.mirror, info.Suffix)

		h.logger.Debug("forwarding",
			"registry", info.Registry,
			"path", r.URL.Path,
			"mirror", mirrorURL,
			"target", tPath,
		)

		// Build the upstream request
		targetURL := mirrorURL + tPath
		req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, nil)
		if err != nil {
			h.logger.Error("build request", "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		req.URL.RawQuery = r.URL.RawQuery
		h.copyHeaders(req, r)

		// Override Accept for manifest requests to support both Docker and OCI manifests.
		// The registry:2 pull-through cache sends its own Accept which may not include OCI types.
		if isManifestPath(r.URL.Path) {
			req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json,application/json")
		}

		// Forward to upstream
		resp, err := upstreamClient(mirrorURL).Do(req)
		if err != nil {
			h.logger.Warn("upstream request failed, trying next mirror",
				"error", err, "mirror", mirrorURL)
			mt.mirror.MarkFailed()
			lastErr = err
			continue
		}

		// Success — mark mirror healthy
		mt.mirror.MarkSuccess()

		// Copy response back to client
		h.copyResponseHeaders(w, resp)
		w.WriteHeader(resp.StatusCode)

		if r.Method == http.MethodGet && resp.Body != nil {
			_, _ = io.Copy(w, resp.Body)
		}
		return
	}

	// All mirrors failed
	h.logger.Error("all mirrors failed", "registry", info.Registry, "error", lastErr)
	http.Error(w, "bad gateway", http.StatusBadGateway)
}

func (h *Handler) serveBase(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

func (h *Handler) copyHeaders(req *http.Request, src *http.Request) {
	for k, vv := range src.Header {
		if forwardRequestHeaders[k] {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
	}
	req.Header.Set("User-Agent", "ociproxy/1.0")
}

func (h *Handler) copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, vv := range resp.Header {
		if forwardResponseHeaders[k] {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
	}
}

var forwardRequestHeaders = map[string]bool{
	"Accept":            true,
	"Accept-Encoding":   true,
	"Authorization":     true,
	"Cache-Control":     true,
	"If-None-Match":     true,
	"If-Modified-Since": true,
	"OCI-Subject":       true,
}

var forwardResponseHeaders = map[string]bool{
	"Content-Type":                true,
	"Content-Length":              true,
	"Content-Encoding":            true,
	"Content-Range":               true,
	"Accept-Ranges":               true,
	"Docker-Content-Digest":       true,
	"Docker-Distribution-API-Version": true,
	"ETag":                        true,
	"Cache-Control":               true,
	"Last-Modified":               true,
	"Location":                    true,
	"Www-Authenticate":            true,
	"Link":                        true,
	"X-RateLimit-Limit":           true,
	"X-RateLimit-Remaining":       true,
	"Date":                        true,
}

// pathInfo holds parsed path data.
type pathInfo struct {
	// Registry is the registry namespace from the path (e.g., "docker.io").
	Registry string
	// Suffix is everything after the registry prefix (e.g., "/library/busybox/manifests/latest").
	Suffix string
	// IsHostname indicates the first path segment looks like a hostname (contains a dot).
	IsHostname bool
}

// parsePath extracts the registry namespace and image path from the URL.
//
// Cases:
//
//	/v2/docker.io/library/busybox/manifests/latest
//	  → Registry="docker.io", Suffix="/library/busybox/manifests/latest"
//
//	/v2/library/busybox/manifests/latest  (default registry implied, no dot in first segment)
//	  → Registry="<default_registry>", Suffix="/library/busybox/manifests/latest"
//
//	/v2/registry-1.docker.io/library/busybox/manifests/latest
//	  → Registry="docker.io" (resolved via domain_alias or default), Suffix="/library/busybox/manifests/latest"
func (h *Handler) parsePath(rawPath string) *pathInfo {
	// Strip /v2/ prefix
	rest := rawPath
	if len(rest) >= 3 && rest[:3] == "/v2" {
		rest = rest[3:]
	}
	if len(rest) > 0 && rest[0] == '/' {
		rest = rest[1:]
	}

	slash := indexByte(rest, '/')
	if slash <= 0 {
		return nil
	}

	first := rest[:slash]
	suffix := rest[slash:]

	// Determine the registry namespace
	var registry string

	if isHostname(first) {
		// First segment is a hostname like "docker.io" or "registry-1.docker.io"
		// Try to match a configured registry by name
		if reg := h.cfg.RegistryByName(first); reg != nil {
			registry = first
		} else {
			// Check if it maps to a known registry via domain alias, or fall back to default
			def := h.cfg.DefaultRegistryConfig()
			if def != nil {
				registry = def.Name
			} else {
				return nil
			}
		}
	} else {
		// First segment doesn't look like a hostname (e.g., "library")
		// Check if it could be a registry name
		if reg := h.cfg.RegistryByName(first); reg != nil {
			registry = first
		} else {
			// Use the default registry namespace
			def := h.cfg.DefaultRegistryConfig()
			if def == nil {
				return nil
			}
			registry = def.Name
			// The first segment is part of the image name, include it in suffix
			suffix = "/" + first + suffix
		}
	}

	return &pathInfo{
		Registry:   registry,
		Suffix:     suffix,
		IsHostname: isHostname(first),
	}
}

// resolveMirrors returns all mirrors for the path's registry, ordered by
// weighted selection with the best candidate first. Returns empty slice if
// the registry is unknown.
func (p *pathInfo) resolveMirrors(cfg *config.Config) []mirrorTarget {
	reg := cfg.RegistryByName(p.Registry)
	if reg == nil {
		return nil
	}

	// Select the best mirror, then build a fallback list from remaining mirrors
	best := reg.SelectMirror()
	if best == nil {
		return nil
	}

	var targets []mirrorTarget
	targets = append(targets, mirrorTarget{mirror: best})

	// Add remaining mirrors as fallbacks
	for i := range reg.Mirrors {
		m := &reg.Mirrors[i]
		if m.URL == best.URL {
			continue
		}
		targets = append(targets, mirrorTarget{mirror: m})
	}

	return targets
}

type mirrorTarget struct {
	mirror *config.Mirror
}

// targetPath builds the upstream request path for a mirror.
func targetPath(mirror *config.Mirror, suffix string) string {
	if mirror.BasePath != "" {
		return "/v2" + mirror.BasePath + suffix
	}
	return "/v2" + suffix
}

func trimTrailingSlash(s string) string {
	if len(s) > 1 && s[len(s)-1] == '/' {
		return s[:len(s)-1]
	}
	return s
}

func isHostname(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return true
		}
	}
	return false
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func isManifestPath(path string) bool {
	return strings.Contains(path, "/manifests/")
}
