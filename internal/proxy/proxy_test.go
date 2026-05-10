package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/planlx/ociproxy/internal/config"
	"github.com/planlx/ociproxy/internal/proxy"
)

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
	h := proxy.New(testConfig(), nil)

	req := httptest.NewRequest(http.MethodPost, "/v2/docker.io/library/busybox/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d; want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestProxy_BaseEndpoint(t *testing.T) {
	h := proxy.New(testConfig(), nil)

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
	h := proxy.New(testConfig(), nil)

	req := httptest.NewRequest(http.MethodGet, "/v2/ghcr.io/library/nginx/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// ghcr.io is not configured → 404
	if rec.Code != http.StatusNotFound {
		t.Errorf("got status %d; want %d", rec.Code, http.StatusNotFound)
	}
}

func TestProxy_HeadRequest(t *testing.T) {
	h := proxy.New(testConfig(), nil)

	req := httptest.NewRequest(http.MethodHead, "/v2/docker.io/library/busybox/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Should attempt to forward (will get 502 since no real upstream), not 405
	if rec.Code == http.StatusMethodNotAllowed {
		t.Error("HEAD method should not be rejected")
	}
}

func TestProxy_DefaultRegistryPath(t *testing.T) {
	h := proxy.New(testConfig(), nil)

	// /v2/library/busybox/manifests/latest — "library" is not a registry,
	// should use default registry "docker.io" and include "library" in the image path
	req := httptest.NewRequest(http.MethodHead, "/v2/library/busybox/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Should attempt to forward to the docker.io mirror, not reject
	if rec.Code == http.StatusBadRequest || rec.Code == http.StatusNotFound {
		t.Errorf("default registry path should not be rejected, got status %d", rec.Code)
	}
}
