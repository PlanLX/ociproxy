package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/planlx/ociproxy/internal/config"
	"github.com/planlx/ociproxy/internal/proxy"
)

// mockRegistry creates a test server that mimics a Docker registry.
func mockRegistry() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Docker registry API handshake
		if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
			return
		}

		// Return a minimal manifest for any manifest request
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234")
			w.Header().Set("Content-Length", "2")
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
			return
		}

		http.NotFound(w, r)
	}))
}

func testConfig() *config.Config {
	return &config.Config{
		DefaultRegistry: "docker.io",
		Registries: []config.Registry{
			{
				Name: "docker.io",
				Mirrors: []config.Mirror{
					{URL: "https://registry-1.docker.io", Default: true},
					{URL: "https://m.daocloud.io", BasePath: "/docker.io"},
				},
			},
		},
	}
}

func TestProxy_MethodNotAllowed(t *testing.T) {
	h := proxy.New(testConfig(), nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v2/docker.io/library/busybox/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d; want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestProxy_BaseEndpoint(t *testing.T) {
	h := proxy.New(testConfig(), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("got status %d; want %d", rec.Code, http.StatusOK)
	}
	if rec.Header().Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Error("missing Docker-Distribution-API-Version header")
	}
}

func TestProxy_UnknownRegistry(t *testing.T) {
	h := proxy.New(testConfig(), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v2/ghcr.io/library/nginx/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// ghcr.io is not configured → 404
	if rec.Code != http.StatusNotFound {
		t.Errorf("got status %d; want %d", rec.Code, http.StatusNotFound)
	}
}

func TestProxy_HeadRequest(t *testing.T) {
	srv := mockRegistry()
	defer srv.Close()

	cfg := &config.Config{
		DefaultRegistry: "docker.io",
		Registries: []config.Registry{
			{
				Name: "docker.io",
				Mirrors: []config.Mirror{
					{URL: srv.URL, Default: true},
				},
			},
		},
	}
	h := proxy.New(cfg, nil, nil)

	req := httptest.NewRequest(http.MethodHead, "/v2/docker.io/library/busybox/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Should succeed via mock registry
	if rec.Code != http.StatusOK {
		t.Errorf("got status %d; want %d", rec.Code, http.StatusOK)
	}
	if rec.Header().Get("Docker-Content-Digest") == "" {
		t.Error("missing Docker-Content-Digest header")
	}
}

func TestProxy_DefaultRegistryPath(t *testing.T) {
	srv := mockRegistry()
	defer srv.Close()

	cfg := &config.Config{
		DefaultRegistry: "docker.io",
		Registries: []config.Registry{
			{
				Name: "docker.io",
				Mirrors: []config.Mirror{
					{URL: srv.URL, Default: true},
				},
			},
		},
	}
	h := proxy.New(cfg, nil, nil)

	// /v2/library/busybox/manifests/latest — "library" is not a registry,
	// should use default registry "docker.io" and include "library" in the image path
	req := httptest.NewRequest(http.MethodHead, "/v2/library/busybox/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Should succeed via mock registry
	if rec.Code != http.StatusOK {
		t.Errorf("default registry path should succeed, got status %d", rec.Code)
	}
}

func TestProxy_NamespaceOverride(t *testing.T) {
	srv := mockRegistry()
	defer srv.Close()

	cfg := &config.Config{
		DefaultRegistry: "docker.io",
		Registries: []config.Registry{
			{
				Name: "docker.io",
				Mirrors: []config.Mirror{
					{URL: srv.URL, Default: true},
				},
			},
			{
				Name: "quay.io",
				Mirrors: []config.Mirror{
					{URL: srv.URL, Default: true},
				},
			},
		},
	}
	h := proxy.New(cfg, nil, nil)

	// /v2/library/busybox/manifests/latest?ns=docker.io — ns overrides registry
	req := httptest.NewRequest(http.MethodHead, "/v2/library/busybox/manifests/latest?ns=docker.io", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Should succeed via docker.io mirror
	if rec.Code != http.StatusOK {
		t.Errorf("ns=docker.io path should succeed, got status %d", rec.Code)
	}
}

func TestProxy_NamespaceOverrideUnknown(t *testing.T) {
	h := proxy.New(testConfig(), nil, nil)

	// /v2/library/busybox/manifests/latest?ns=unknown.io — unknown registry via ns
	req := httptest.NewRequest(http.MethodHead, "/v2/library/busybox/manifests/latest?ns=unknown.io", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Should return 404 for unknown registry
	if rec.Code != http.StatusNotFound {
		t.Errorf("got status %d; want %d for unknown ns", rec.Code, http.StatusNotFound)
	}
}

func TestProxy_GetManifest(t *testing.T) {
	srv := mockRegistry()
	defer srv.Close()

	cfg := &config.Config{
		DefaultRegistry: "docker.io",
		Registries: []config.Registry{
			{
				Name: "docker.io",
				Mirrors: []config.Mirror{
					{URL: srv.URL, Default: true},
				},
			},
		},
	}
	h := proxy.New(cfg, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v2/docker.io/library/busybox/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("got status %d; want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() == 0 {
		t.Error("empty response body")
	}
}
