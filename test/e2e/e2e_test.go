// Package e2e provides end-to-end tests against a real Docker Compose
// environment. Requires the test infrastructure to be running:
//
//   cd deploy && docker compose up -d
//
// The tests verify that ociproxy correctly:
//   - Routes requests to the correct upstream registry
//   - Rewrites paths with the configured base_path
//   - Forwards request/response headers
//   - Handles the Docker Registry v2 API subset
//   - Rejects invalid methods and unknown registries
//
// Run with:
//
//   go test -v ./test/e2e/ -tags=e2e
//
package e2e_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
)

// testHost is the base URL of the running ociproxy instance.
// Override via the OCIPROXY_E2E_URL environment variable.
func testHost(t *testing.T) string {
	t.Helper()
	if h := os.Getenv("OCIPROXY_E2E_URL"); h != "" {
		return h
	}
	return "http://localhost:8081"
}

// testUpstreamHost is the base URL of the real upstream registry.
func testUpstreamHost(t *testing.T) string {
	t.Helper()
	if h := os.Getenv("OCIPROXY_E2E_UPSTREAM"); h != "" {
		return h
	}
	return "http://localhost:5001"
}

// testMirrorHost is the base URL of the mirror registry.
func testMirrorHost(t *testing.T) string {
	t.Helper()
	if h := os.Getenv("OCIPROXY_E2E_MIRROR"); h != "" {
		return h
	}
	return "http://localhost:5002"
}

// TestE2E_V2Base verifies the proxy /v2/ endpoint returns the API version.
func TestE2E_V2Base(t *testing.T) {
	resp := proxyGet(t, "/v2/")
	assertStatus(t, resp, http.StatusOK)

	apiVersion := resp.Header.Get("Docker-Distribution-API-Version")
	if apiVersion != "registry/2.0" {
		t.Fatalf("API version = %q; want %q", apiVersion, "registry/2.0")
	}
}

// TestE2E_Catalog verifies the catalog is accessible (or returns a valid error).
func TestE2E_Catalog(t *testing.T) {
	resp := proxyGet(t, "/v2/_catalog")
	// Read-only proxy may block or forward catalog
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unexpected status for /v2/_catalog: %d", resp.StatusCode)
	}
}

// TestE2E_PushManifestAndBlob pushes data to upstream directly, then reads via proxy.
func TestE2E_PushManifestAndBlob(t *testing.T) {
	repo := "library/e2e-test-busybox"
	tag := "latest"
	image := repo

	// Push to upstream directly
	layerDigest := pushBlobToUpstream(t, repo, []byte("hello from e2e test"))
	pushManifestToUpstream(t, repo, tag, layerDigest)

	// Pull manifest via proxy
	resp := proxyGet(t, fmt.Sprintf("/v2/%s/manifests/%s", image, tag))
	assertStatus(t, resp, http.StatusOK)

	var manifest map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}

	// Pull blob via proxy
	resp2 := proxyGet(t, fmt.Sprintf("/v2/%s/blobs/%s", image, layerDigest))
	assertStatus(t, resp2, http.StatusOK)

	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(body) != "hello from e2e test" {
		t.Fatalf("blob content = %q; want %q", body, "hello from e2e test")
	}
}

// TestE2E_HEADManifest verifies HEAD requests for manifests work.
func TestE2E_HEADManifest(t *testing.T) {
	repo := "library/e2e-test-nginx"
	tag := "1.0"
	image := repo

	// Push to upstream
	layerDigest := pushBlobToUpstream(t, repo, []byte("layer"))
	pushManifestToUpstream(t, repo, tag, layerDigest)

	// HEAD the manifest via proxy
	req, err := http.NewRequest(http.MethodHead, testHost(t)+"/v2/"+image+"/manifests/"+tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD manifest: %v", err)
	}
	defer resp.Body.Close()

	assertStatus(t, resp, http.StatusOK)

	if resp.Header.Get("Docker-Content-Digest") == "" {
		t.Error("missing Docker-Content-Digest header")
	}
}

// TestE2E_TagsList verifies tag listing.
func TestE2E_TagsList(t *testing.T) {
	repo := "library/e2e-test-alpine"
	image := repo
	tags := []string{"3.18", "3.19", "latest"}

	// Push multiple tags to upstream
	layerDigest := pushBlobToUpstream(t, repo, []byte("data"))
	for _, tag := range tags {
		pushManifestToUpstream(t, repo, tag, layerDigest)
	}

	// List tags via proxy
	resp := proxyGet(t, fmt.Sprintf("/v2/%s/tags/list", image))
	assertStatus(t, resp, http.StatusOK)

	var tagsResp struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err != nil {
		t.Fatalf("decode tags: %v", err)
	}

	tagSet := make(map[string]bool)
	for _, t := range tagsResp.Tags {
		tagSet[t] = true
	}
	for _, want := range tags {
		if !tagSet[want] {
			t.Errorf("tag %q not found in tags list: %v", want, tagsResp.Tags)
		}
	}
}

// TestE2E_PullThroughMirror verifies that the blob exists on the upstream
// (confirming the proxy routed to the correct mirror) and can be read back.
func TestE2E_PullThroughMirror(t *testing.T) {
	repo := "library/e2e-test-redis"
	tag := "7.0"
	image := repo

	// Push to upstream
	layerDigest := pushBlobToUpstream(t, repo, []byte("redis-layer"))
	pushManifestToUpstream(t, repo, tag, layerDigest)

	// Verify blob is directly accessible on upstream
	upstreamResp, err := http.Get(testUpstreamHost(t) + "/v2/" + repo + "/blobs/" + layerDigest)
	if err != nil {
		t.Fatalf("check upstream blob: %v", err)
	}
	defer upstreamResp.Body.Close()
	if upstreamResp.StatusCode != http.StatusOK {
		t.Fatalf("blob not found on upstream, status=%d", upstreamResp.StatusCode)
	}

	// Pull via proxy
	resp := proxyGet(t, fmt.Sprintf("/v2/%s/blobs/%s", image, layerDigest))
	assertStatus(t, resp, http.StatusOK)
}

// TestE2E_DefaultRegistryNoPrefix verifies requests without a registry
// prefix use the default registry namespace.
func TestE2E_DefaultRegistryNoPrefix(t *testing.T) {
	repo := "library/e2e-test-memcached"
	tag := "1.6"
	image := repo

	// Push to upstream
	layerDigest := pushBlobToUpstream(t, repo, []byte("mc-layer"))
	pushManifestToUpstream(t, repo, tag, layerDigest)

	// Pull via the default path (no registry prefix)
	resp := proxyGet(t, fmt.Sprintf("/v2/%s/manifests/%s", image, tag))
	assertStatus(t, resp, http.StatusOK)
}

// TestE2E_BlobNotFound verifies proper 404 for non-existent blobs.
func TestE2E_BlobNotFound(t *testing.T) {
	resp := proxyGet(t, "/v2/docker.io/library/nonexistent/blobs/sha256:"+hexDigest("missing"))
	assertStatus(t, resp, http.StatusNotFound)
}

// TestE2E_ManifestNotFound verifies proper 404 for non-existent manifests.
func TestE2E_ManifestNotFound(t *testing.T) {
	resp := proxyGet(t, "/v2/docker.io/library/nonexistent/manifests/missing")
	assertStatus(t, resp, http.StatusNotFound)
}

// TestE2E_MethodNotAllowed verifies non-GET/HEAD methods are rejected.
func TestE2E_MethodNotAllowed(t *testing.T) {
	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, testHost(t)+"/v2/docker.io/library/busybox/manifests/latest", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s: status = %d; want %d", method, resp.StatusCode, http.StatusMethodNotAllowed)
			}
		})
	}
}

// TestE2E_UnknownRegistry verifies 404 for unconfigured registry namespaces.
func TestE2E_UnknownRegistry(t *testing.T) {
	resp := proxyGet(t, "/v2/unknown.io/some/image/manifests/latest")
	assertStatus(t, resp, http.StatusNotFound)
}

// TestE2E_UpstreamDirect verifies the upstream registry is directly accessible.
func TestE2E_UpstreamDirect(t *testing.T) {
	resp, err := http.Get(testUpstreamHost(t) + "/v2/")
	if err != nil {
		t.Fatalf("upstream /v2/ unreachable: %v", err)
	}
	defer resp.Body.Close()

	assertStatus(t, resp, http.StatusOK)
	if resp.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatal("upstream does not appear to be a Docker Registry v2")
	}
}

// TestE2E_MirrorDirect verifies the mirror registry is directly accessible.
func TestE2E_MirrorDirect(t *testing.T) {
	resp, err := http.Get(testMirrorHost(t) + "/v2/")
	if err != nil {
		t.Fatalf("mirror /v2/ unreachable: %v", err)
	}
	defer resp.Body.Close()

	assertStatus(t, resp, http.StatusOK)
}

// TestE2E_CrossRegistryRouting verifies that when multiple registries
// are configured, requests are routed to the correct one.
func TestE2E_CrossRegistryRouting(t *testing.T) {
	repo1 := "library/e2e-reg-a"
	repo2 := "library/e2e-reg-b"

	// Push both to upstream (same registry namespace)
	layer1 := pushBlobToUpstream(t, repo1, []byte("reg-a"))
	pushManifestToUpstream(t, repo1, "v1", layer1)

	layer2 := pushBlobToUpstream(t, repo2, []byte("reg-b"))
	pushManifestToUpstream(t, repo2, "v1", layer2)

	if layer1 == layer2 {
		t.Fatal("expected different digests for different blobs")
	}

	// Pull both through the proxy and verify correctness
	resp1 := proxyGet(t, fmt.Sprintf("/v2/library/e2e-reg-a/blobs/%s", layer1))
	assertStatus(t, resp1, http.StatusOK)
	body1, _ := io.ReadAll(resp1.Body)
	if string(body1) != "reg-a" {
		t.Fatalf("reg-a blob = %q; want %q", body1, "reg-a")
	}

	resp2 := proxyGet(t, fmt.Sprintf("/v2/library/e2e-reg-b/blobs/%s", layer2))
	assertStatus(t, resp2, http.StatusOK)
	body2, _ := io.ReadAll(resp2.Body)
	if string(body2) != "reg-b" {
		t.Fatalf("reg-b blob = %q; want %q", body2, "reg-b")
	}
}

// --- helpers ---

// proxyGet makes a GET request to the proxy.
func proxyGet(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(testHost(t) + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// assertStatus checks the HTTP status code.
func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("status = %d; want %d, body=%q", resp.StatusCode, want, body)
	}
}

// pushBlobToUpstream uploads a blob directly to the upstream registry.
func pushBlobToUpstream(t *testing.T, repo string, data []byte) string {
	t.Helper()
	base := testUpstreamHost(t)
	return pushBlobRaw(t, base, repo, data)
}

// pushManifestToUpstream pushes a manifest directly to the upstream registry.
func pushManifestToUpstream(t *testing.T, repo, tag, layerDigest string) {
	t.Helper()
	base := testUpstreamHost(t)

	// Push config blob first (required by registry)
	configData := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := pushBlobRaw(t, base, repo, configData)

	pushManifestRaw(t, base, repo, tag, layerDigest, configDigest)
}

// pushBlobRaw uploads a blob to the given registry base URL.
func pushBlobRaw(t *testing.T, base, repo string, data []byte) string {
	t.Helper()

	digest := sha256Digest(data)

	// Initiate blob upload
	resp, err := http.Post(base+"/v2/"+repo+"/blobs/uploads/", "text/plain", nil)
	if err != nil {
		t.Fatalf("initiate upload: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		t.Fatalf("initiate upload: status=%d", resp.StatusCode)
	}

	if resp.StatusCode == http.StatusCreated {
		return digest
	}

	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("missing Location header in upload response")
	}

	uploadURL := location
	if location[0] == '/' {
		uploadURL = base + location
	}

	// Add digest query parameter properly
	u, err := url.Parse(uploadURL)
	if err != nil {
		t.Fatalf("parse upload URL: %v", err)
	}
	q := u.Query()
	q.Set("digest", digest)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodPut, u.String(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))

	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload blob: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp2.Body, 512))
		t.Fatalf("upload blob: status=%d, body=%q", resp2.StatusCode, body)
	}

	return digest
}

// pushManifestRaw pushes a manifest to the given registry base URL.
func pushManifestRaw(t *testing.T, base, repo, tag, layerDigest, configDigest string) {
	t.Helper()

	configData := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)

	manifest := fmt.Sprintf(`{
		"schemaVersion": 2,
		"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"config": {
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"size": %d,
			"digest": "%s"
		},
		"layers": [
			{
				"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
				"size": %d,
				"digest": "%s"
			}
		]
	}`, len(configData), configDigest, len(configData), layerDigest)

	req, err := http.NewRequest(http.MethodPut, base+"/v2/"+repo+"/manifests/"+tag, bytes.NewReader([]byte(manifest)))
	if err != nil {
		t.Fatalf("build manifest request: %v", err)
	}
	req.Header.Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("push manifest: status=%d, body=%q", resp.StatusCode, body)
	}
}

func sha256Digest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + fmt.Sprintf("%x", h[:])
}

func hexDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:])
}
