// Package proxy provides stateless OCI registry proxy logic.
// It uses go-containerregistry to communicate with upstream registries.
package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/planlx/ociproxy/internal/cache"
	"github.com/planlx/ociproxy/internal/config"
)

// remoteOptsCache caches []remote.Option per mirror URL to avoid rebuilding.
var (
	optsMu     sync.RWMutex
	optsCache  = make(map[string][]remote.Option)
)

// buildRemoteOpts builds remote.Option slice for a mirror.
func buildRemoteOpts(mirror *config.Mirror) []remote.Option {
	optsMu.RLock()
	opts, ok := optsCache[mirror.URL]
	optsMu.RUnlock()
	if ok {
		return opts
	}

	optsMu.Lock()
	defer optsMu.Unlock()

	// Double-check after acquiring write lock
	if opts, ok := optsCache[mirror.URL]; ok {
		return opts
	}

	var optsList []remote.Option

	// Auth
	var auth authn.Authenticator
	if mirror.Credentials != nil && mirror.Credentials.Username != "" {
		auth = &authn.Basic{Username: mirror.Credentials.Username, Password: mirror.Credentials.Password}
	} else {
		auth = authn.Anonymous
	}
	optsList = append(optsList, remote.WithAuth(auth))

	// User agent
	optsList = append(optsList, remote.WithUserAgent("ociproxy/1.0"))

	// Transport with proxy support
	tr := defaultTransport()
	optsList = append(optsList, remote.WithTransport(tr))

	// Timeout context wrapper — each call will use a 120s detached context
	// go-containerregistry doesn't have a WithTimeout, so we use the default HTTP client
	// with our transport

	optsCache[mirror.URL] = optsList
	return optsList
}

// defaultTransport returns the default HTTP transport with TLS and proxy support.
func defaultTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: nil, // use system defaults
		Proxy:           http.ProxyFromEnvironment,
	}
}

// Handler is the stateless OCI registry proxy.
type Handler struct {
	cfg    *config.Config
	logger *slog.Logger

	// Optional cache backend (filesystem or S3).
	cache       cache.Store
	cacheCloser io.Closer
	manifestMem *cache.ManifestCache

	// negCache caches 404 responses to prevent hammering upstream.
	// Key = "method:path", value = notFoundEntry.
	negCache sync.Map
}

// notFoundEntry records when a 404 was first seen.
type notFoundEntry struct {
	seenAt time.Time
}

// negativeCacheTTL is how long to cache 404 responses.
const negativeCacheTTL = 60 * time.Second

// New creates a new proxy handler.
func New(cfg *config.Config, localCache cache.Store, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h := &Handler{
		cfg:         cfg,
		logger:      logger,
		cache:       localCache,
		manifestMem: &cache.ManifestCache{},
	}
	if closer, ok := localCache.(io.Closer); ok {
		h.cacheCloser = closer
	}
	go h.negCacheCleanup()
	h.manifestMem.Cleanup()
	return h
}

// Close releases resources held by the cache backend.
func (h *Handler) Close() {
	if h.cacheCloser != nil {
		h.cacheCloser.Close()
	}
}

// negCacheCleanup periodically removes expired entries.
func (h *Handler) negCacheCleanup() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		h.negCache.Range(func(key, value any) bool {
			if time.Since(value.(*notFoundEntry).seenAt) > negativeCacheTTL {
				h.negCache.Delete(key)
			}
			return true
		})
	}
}

// CloseAll clears the remote options cache.
func CloseAll() {
	optsMu.Lock()
	defer optsMu.Unlock()
	optsCache = make(map[string][]remote.Option)
}

// ServeHTTP proxieses the request to the appropriate upstream mirror.
//
// URL formats:
//   /v2/<registry>/<image>/manifests/<tag>    — e.g. /v2/docker.io/library/busybox/manifests/latest
//   /v2/<registry>/<image>/blobs/<digest>     — e.g. /v2/docker.io/library/busybox/blobs/sha256:abc123
//   /v2/<registry>/<image>/tags/list          — list tags
//
// Query parameter ns= overrides the registry namespace:
//   /v2/<image>/manifests/<tag>?ns=docker.io  — e.g. /v2/library/busybox/manifests/latest?ns=docker.io
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		h.serveBase(w, r)
		return
	}

	info, err := h.parsePath(r.URL.Path, r.URL.Query().Get("ns"))
	if err != nil {
		if errors.Is(err, errUnknownRegistry) {
			h.logger.Warn("unknown registry", "path", r.URL.Path)
			http.Error(w, "registry not found", http.StatusNotFound)
			return
		}
		h.logger.Warn("bad request path", "path", r.URL.Path)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if info == nil {
		h.logger.Warn("bad request path", "path", r.URL.Path)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	mirrors := info.resolveMirrors(h.cfg)
	if len(mirrors) == 0 {
		h.logger.Warn("unknown registry", "registry", info.Registry)
		http.Error(w, "registry not found", http.StatusNotFound)
		return
	}

	// Check negative cache
	cacheKey := r.Method + ":" + r.URL.Path
	if entry, ok := h.negCache.Load(cacheKey); ok {
		if time.Since(entry.(*notFoundEntry).seenAt) < negativeCacheTTL {
			h.logger.Debug("serving cached 404", "path", r.URL.Path)
			http.Error(w, "manifest unknown", http.StatusNotFound)
			return
		}
		h.negCache.Delete(cacheKey)
	}

	// Check memory manifest cache
	if isManifestPath(r.URL.Path) {
		if hit := h.serveFromMemCache(w, r, info); hit {
			return
		}
	}

	// Route by path type
	if isManifestPath(r.URL.Path) {
		h.handleManifest(w, r, info, mirrors)
	} else if isBlobPath(r.URL.Path) {
		h.handleBlob(w, r, info, mirrors)
	} else if isTagsPath(r.URL.Path) {
		h.handleTags(w, r, info, mirrors)
	} else {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// parseManifestPath extracts repo and ref from suffix like "/library/busybox/manifests/latest".
func parseManifestPath(suffix string) (repo, ref string, ok bool) {
	suffix = strings.TrimPrefix(suffix, "/")
	idx := strings.Index(suffix, "/manifests/")
	if idx <= 0 {
		return "", "", false
	}
	repo = suffix[:idx]
	ref = suffix[idx+len("/manifests/"):]
	if repo == "" || ref == "" {
		return "", "", false
	}
	return repo, ref, true
}

// parseBlobPath extracts repo and digest from suffix like "/library/busybox/blobs/sha256:abc".
func parseBlobPath(suffix string) (repo, digest string, ok bool) {
	suffix = strings.TrimPrefix(suffix, "/")
	idx := strings.Index(suffix, "/blobs/")
	if idx <= 0 {
		return "", "", false
	}
	repo = suffix[:idx]
	digest = suffix[idx+len("/blobs/"):]
	if repo == "" || digest == "" {
		return "", "", false
	}
	return repo, digest, true
}

// parseTagsPath extracts repo from suffix like "/library/nginx/tags/list".
func parseTagsPath(suffix string) (repo string, ok bool) {
	suffix = strings.TrimPrefix(suffix, "/")
	idx := strings.Index(suffix, "/tags/list")
	if idx <= 0 {
		return "", false
	}
	repo = suffix[:idx]
	if repo == "" {
		return "", false
	}
	return repo, true
}

func (h *Handler) serveFromMemCache(w http.ResponseWriter, r *http.Request, info *pathInfo) bool {
	repo, ref, ok := parseManifestPath(info.Suffix)
	if !ok {
		return false
	}

	memKey := cache.CacheKey("GET", info.Registry, repo, ref)
	entry, ok := h.manifestMem.Get(memKey)
	if !ok {
		return false
	}

	h.logger.Debug("memory cache hit", "path", r.URL.Path, "repo", repo, "ref", ref)
	body := entry.Data()
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	w.Header().Set("Content-Type", entry.MediaType())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}

func (h *Handler) handleManifest(w http.ResponseWriter, r *http.Request, info *pathInfo, mirrors []mirrorTarget) {
	repo, ref, ok := parseManifestPath(info.Suffix)
	if !ok {
		h.logger.Warn("bad manifest path", "suffix", info.Suffix)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodHead {
		h.handleManifestHead(w, r, info, mirrors, repo, ref)
		return
	}

	// GET manifest — try mirrors
	var lastErr error
	for _, mt := range mirrors {
		imageRef := buildImageRef(mt.mirror, info.Registry, repo, ref)
		h.logger.Debug("trying mirror", "mirror", mt.mirror.URL, "image", imageRef, "repo", repo, "ref", ref)

		parsedRef, err := name.ParseReference(imageRef, name.StrictValidation)
		if err != nil {
			h.logger.Error("parse reference", "error", err, "image", imageRef)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		opts := append(buildRemoteOpts(mt.mirror), remote.WithContext(ctx))
		desc, err := remote.Get(parsedRef, opts...)
		cancel()
		if err != nil {
			h.logger.Warn("upstream GET failed, trying next mirror",
				"error", err, "mirror", mt.mirror.URL, "image", imageRef)
			mt.mirror.MarkFailed()
			lastErr = err
			continue
		}

		mt.mirror.MarkSuccess()

		// Cache in memory
		memKey := cache.CacheKey("GET", info.Registry, repo, ref)
		h.manifestMem.Set(memKey, desc.Manifest, string(desc.MediaType), cache.GetManifestTTL(ref))

		// Write filesystem cache asynchronously
		contentType := string(desc.MediaType)
		if h.cache != nil {
			body := desc.Manifest
			digest := desc.Digest.String()
			go func() {
				if err := h.cache.PutManifest(info.Registry, repo, digest, ref, body, contentType); err != nil {
					h.logger.Error("failed to cache manifest", "error", err, "registry", info.Registry, "repo", repo, "digest", digest)
				}
			}()
		}
		h.logger.Info("upstream manifest fetched", "mirror", mt.mirror.URL, "registry", info.Registry, "repo", repo, "ref", ref, "digest", desc.Digest.String(), "size", len(desc.Manifest))

		// Respond
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(desc.Manifest)))
		w.Header().Set("Docker-Content-Digest", desc.Digest.String())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(desc.Manifest)
		return
	}

	if lastErr != nil {
		h.logger.Warn("all mirrors failed", "registry", info.Registry, "error", lastErr)
	}
	h.negCache.Store(cacheKey(r), &notFoundEntry{seenAt: time.Now()})
	http.Error(w, "manifest unknown", http.StatusNotFound)
}

func (h *Handler) handleManifestHead(w http.ResponseWriter, r *http.Request, info *pathInfo, mirrors []mirrorTarget, repo, ref string) {
	var lastErr error
	for _, mt := range mirrors {
		imageRef := buildImageRef(mt.mirror, info.Registry, repo, ref)
		h.logger.Debug("trying mirror (HEAD)", "mirror", mt.mirror.URL, "image", imageRef)

		parsedRef, err := name.ParseReference(imageRef, name.StrictValidation)
		if err != nil {
			h.logger.Error("parse reference (HEAD)", "error", err, "image", imageRef)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		opts := append(buildRemoteOpts(mt.mirror), remote.WithContext(ctx))
		desc, err := remote.Head(parsedRef, opts...)
		cancel()
		if err != nil {
			h.logger.Warn("upstream HEAD failed, trying next mirror",
				"error", err, "mirror", mt.mirror.URL, "image", imageRef)
			mt.mirror.MarkFailed()
			lastErr = err
			continue
		}

		mt.mirror.MarkSuccess()

		h.logger.Info("upstream HEAD fetched", "mirror", mt.mirror.URL, "registry", info.Registry, "repo", repo, "ref", ref, "digest", desc.Digest.String())

		w.Header().Set("Content-Type", string(desc.MediaType))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", desc.Size))
		w.Header().Set("Docker-Content-Digest", desc.Digest.String())
		w.WriteHeader(http.StatusOK)
		return
	}

	if lastErr != nil {
		h.logger.Warn("all mirrors HEAD failed", "registry", info.Registry, "error", lastErr)
	}
	h.negCache.Store(cacheKey(r), &notFoundEntry{seenAt: time.Now()})
	http.Error(w, "manifest unknown", http.StatusNotFound)
}

func (h *Handler) handleBlob(w http.ResponseWriter, r *http.Request, info *pathInfo, mirrors []mirrorTarget) {
	repo, digest, ok := parseBlobPath(info.Suffix)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Check local filesystem cache first (pull-through, same as S3 distribution mode)
	if h.cache != nil {
		reader, size, err := h.cache.GetBlob(info.Registry, repo, digest)
		if err != nil {
			h.logger.Warn("read blob cache failed, falling back to upstream", "error", err)
		} else if reader != nil {
			defer reader.Close()
			h.logger.Info("blob cache hit", "registry", info.Registry, "repo", repo, "digest", digest, "size", size)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
			w.Header().Set("Docker-Content-Digest", digest)
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.Copy(w, reader)
			return
		}
	}

	var lastErr error
	for _, mt := range mirrors {
		imageRef := buildImageRef(mt.mirror, info.Registry, repo, digest)
		parsedRef, err := name.NewDigest(imageRef, name.StrictValidation)
		if err != nil {
			h.logger.Error("parse digest reference", "error", err, "image", imageRef)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		opts := append(buildRemoteOpts(mt.mirror), remote.WithContext(ctx))
		layer, err := remote.Layer(parsedRef, opts...)
		if err != nil {
			cancel()
			h.logger.Warn("upstream layer failed, trying next mirror",
				"error", err, "mirror", mt.mirror.URL)
			mt.mirror.MarkFailed()
			lastErr = err
			continue
		}

		mt.mirror.MarkSuccess()

		size, err := layer.Size()
		if err != nil {
			cancel()
			h.logger.Warn("get layer size failed", "error", err)
			mt.mirror.MarkFailed()
			continue
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		w.Header().Set("Docker-Content-Digest", digest)

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Stream blob to client
		reader, err := layer.Compressed()
		if err != nil {
			h.logger.Warn("get compressed layer failed", "error", err)
			mt.mirror.MarkFailed()
			continue
		}

		h.logger.Info("upstream blob fetched", "mirror", mt.mirror.URL, "registry", info.Registry, "repo", repo, "digest", digest, "size", size)

		w.WriteHeader(http.StatusOK)

		// Stream blob to client while writing to local cache (pull-through pattern)
		if h.cache != nil {
			tee := io.TeeReader(reader, w)
			err = h.cache.PutBlob(info.Registry, repo, digest, tee, size)
		} else {
			_, err = io.Copy(w, reader)
		}
		reader.Close()
		if err != nil {
			h.logger.Warn("blob stream failed", "error", err)
		}
		return
	}

	if lastErr != nil {
		h.logger.Warn("all mirrors blob failed", "registry", info.Registry, "error", lastErr)
	}
	http.Error(w, "blob unknown", http.StatusNotFound)
}

func (h *Handler) handleTags(w http.ResponseWriter, r *http.Request, info *pathInfo, mirrors []mirrorTarget) {
	repo, ok := parseTagsPath(info.Suffix)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var lastErr error
	for _, mt := range mirrors {
		// Build repo ref without tag for listing
		imageRef := buildImageRefBase(mt.mirror, info.Registry, repo)
		repoRef, err := name.NewRepository(imageRef, name.StrictValidation)
		if err != nil {
			h.logger.Error("parse repository reference", "error", err, "image", imageRef)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		opts := append(buildRemoteOpts(mt.mirror), remote.WithContext(ctx))
		tags, err := remote.List(repoRef, opts...)
		cancel()
		if err != nil {
			h.logger.Warn("upstream list failed, trying next mirror",
				"error", err, "mirror", mt.mirror.URL)
			mt.mirror.MarkFailed()
			lastErr = err
			continue
		}

		mt.mirror.MarkSuccess()

		// Build JSON response
		tagsJSON := buildTagsJSON(repo, tags)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tagsJSON)
		return
	}

	if lastErr != nil {
		h.logger.Warn("all mirrors tags failed", "registry", info.Registry, "error", lastErr)
	}
	http.Error(w, "tags unknown", http.StatusNotFound)
}

func buildTagsJSON(repo string, tags []string) []byte {
	// Simple JSON construction without importing encoding/json
	// Result: {"name":"<repo>","tags":["t1","t2",...]}
	var sb strings.Builder
	sb.WriteString(`{"name":"`)
	sb.WriteString(repo)
	sb.WriteString(`","tags":[`)
	for i, t := range tags {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('"')
		sb.WriteString(t)
		sb.WriteByte('"')
	}
	sb.WriteString("]}")
	return []byte(sb.String())
}

// buildImageRef constructs the full image reference for a mirror.
// For m.daocloud.io with base_path=/docker.io, registry=docker.io, repo=library/busybox, ref=latest:
//   -> m.daocloud.io/docker.io/library/busybox:latest
// For mirrors without base_path (Docker Hub clones like docker.1ms.run):
//   -> docker.1ms.run/library/busybox:latest
func buildImageRef(mirror *config.Mirror, registry, repo, ref string) string {
	host := extractHost(mirror.URL)
	sep := ":"
	if strings.HasPrefix(ref, "sha256:") || strings.HasPrefix(ref, "sha512:") {
		sep = "@"
	}
	if mirror.BasePath != "" {
		return fmt.Sprintf("%s%s/%s%s%s", host, mirror.BasePath, repo, sep, ref)
	}
	return fmt.Sprintf("%s/%s%s%s", host, repo, sep, ref)
}

// buildImageRefBase constructs the repository reference (no tag) for a mirror.
// Used for remote.List and similar operations.
func buildImageRefBase(mirror *config.Mirror, registry, repo string) string {
	host := extractHost(mirror.URL)
	if mirror.BasePath != "" {
		return fmt.Sprintf("%s%s/%s", host, mirror.BasePath, repo)
	}
	return fmt.Sprintf("%s/%s", host, repo)
}

// extractHost extracts the hostname from a URL like "https://m.daocloud.io".
func extractHost(url string) string {
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	url = trimTrailingSlash(url)
	return url
}

func cacheKey(r *http.Request) string {
	return r.Method + ":" + r.URL.Path
}

func (h *Handler) serveBase(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

var errUnknownRegistry = errors.New("unknown registry")

// pathInfo holds parsed path data.
type pathInfo struct {
	Registry   string
	Suffix     string
	IsHostname bool
}

// parsePath extracts the registry namespace and image path from the URL.
func (h *Handler) parsePath(rawPath string, ns string) (*pathInfo, error) {
	rest := rawPath
	if len(rest) >= 3 && rest[:3] == "/v2" {
		rest = rest[3:]
	}
	if len(rest) > 0 && rest[0] == '/' {
		rest = rest[1:]
	}

	if ns != "" {
		if reg := h.cfg.RegistryByName(ns); reg != nil {
			return &pathInfo{
				Registry: ns,
				Suffix:   "/" + rest,
			}, nil
		}
		return nil, errUnknownRegistry
	}

	slash := indexByte(rest, '/')
	if slash <= 0 {
		return nil, nil
	}

	first := rest[:slash]
	suffix := rest[slash:]

	var registry string

	if isHostname(first) {
		if reg := h.cfg.RegistryByName(first); reg != nil {
			registry = first
		} else {
			return nil, errUnknownRegistry
		}
	} else {
		if reg := h.cfg.RegistryByName(first); reg != nil {
			registry = first
		} else {
			def := h.cfg.DefaultRegistryConfig()
			if def == nil {
				return nil, nil
			}
			registry = def.Name
			suffix = "/" + first + suffix
		}
	}

	return &pathInfo{
		Registry:   registry,
		Suffix:     suffix,
		IsHostname: isHostname(first),
	}, nil
}

// resolveMirrors returns all mirrors for the path's registry, ordered by
// weighted selection with the best candidate first.
func (p *pathInfo) resolveMirrors(cfg *config.Config) []mirrorTarget {
	reg := cfg.RegistryByName(p.Registry)
	if reg == nil {
		return nil
	}

	best := reg.SelectMirror()
	if best == nil {
		return nil
	}

	var targets []mirrorTarget
	targets = append(targets, mirrorTarget{mirror: best})

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

func isBlobPath(path string) bool {
	return strings.Contains(path, "/blobs/")
}

func isTagsPath(path string) bool {
	return strings.Contains(path, "/tags/list")
}
