package cache_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/planlx/ociproxy/internal/cache"
)

func newStore(t *testing.T) (*cache.Store, func()) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	return cache.NewStore(dir, nil), func() { os.RemoveAll(dir) }
}

func TestStore_ManifestRoundtrip(t *testing.T) {
	s, _ := newStore(t)
	registry := "docker.io"
	repo := "library/busybox"
	digest := "sha256:abc123"
	data := []byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)

	// Cache miss
	got, mt, err := s.GetManifest(registry, repo, digest)
	if err != nil {
		t.Fatalf("GetManifest error: %v", err)
	}
	if got != nil {
		t.Error("expected cache miss, got data")
	}

	// Store
	if err := s.PutManifest(registry, repo, digest, data, "application/json"); err != nil {
		t.Fatalf("PutManifest error: %v", err)
	}

	// Cache hit
	got, mt, err = s.GetManifest(registry, repo, digest)
	if err != nil {
		t.Fatalf("GetManifest after put error: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("got manifest %q, want %q", string(got), string(data))
	}
	if mt != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("got mediaType %q, want %q", mt, "application/vnd.oci.image.manifest.v1+json")
	}

	// Verify distribution layout: blob data + link file
	blobPath := filepath.Join(s.Root(), "docker", "registry", "v2", "blobs", "sha256", "ab", "abc123", "data")
	linkPath := filepath.Join(s.Root(), "docker", "registry", "v2", "repositories", "docker.io", "library", "busybox", "_manifests", "revisions", "sha256", "abc123", "link")
	if _, err := os.Stat(blobPath); err != nil {
		t.Errorf("blob data file missing: %s (%v)", blobPath, err)
	}
	if _, err := os.Stat(linkPath); err != nil {
		t.Errorf("manifest link file missing: %s (%v)", linkPath, err)
	} else {
		linkContent, _ := os.ReadFile(linkPath)
		if string(linkContent) != digest {
			t.Errorf("link content = %q, want %q", string(linkContent), digest)
		}
	}
}

func TestStore_BlobRoundtrip(t *testing.T) {
	s, _ := newStore(t)
	registry := "gcr.io"
	repo := "kaniko-project/executor"
	digest := "sha256:def456"
	data := []byte("binary blob content here")

	// Cache miss
	rc, size, err := s.GetBlob(registry, repo, digest)
	if err != nil {
		t.Fatalf("GetBlob error: %v", err)
	}
	if rc != nil {
		t.Error("expected cache miss, got reader")
	}

	// Store
	if err := s.PutBlob(registry, repo, digest, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutBlob error: %v", err)
	}

	// Cache hit
	rc, size, err = s.GetBlob(registry, repo, digest)
	if err != nil {
		t.Fatalf("GetBlob after put error: %v", err)
	}
	if rc == nil {
		t.Fatal("expected cache hit, got nil reader")
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Errorf("got blob %q, want %q", string(got), string(data))
	}
	if size != int64(len(data)) {
		t.Errorf("got size %d, want %d", size, len(data))
	}
}

func TestStore_ManifestCacheTTL(t *testing.T) {
	mc := &cache.ManifestCache{}

	key := "test-key"
	data := []byte(`{"test":true}`)

	// Set with 100ms TTL
	mc.Set(key, data, "application/json", 100*time.Millisecond)

	// Just verify no panic on Get after set and after expiry
	mc.Get(key)

	// Wait for expiry
	time.Sleep(150 * time.Millisecond)
	mc.Get(key)
}

func TestStore_GetManifestTTL(t *testing.T) {
	tests := []struct {
		ref  string
		want time.Duration
	}{
		{"sha256:abc123", 24 * time.Hour},
		{"latest", 10 * time.Minute},
		{"main", 10 * time.Minute},
		{"v1.2.3", 30 * time.Minute},
		{"debug", 30 * time.Minute},
	}
	for _, tt := range tests {
		got := cache.GetManifestTTL(tt.ref)
		if got != tt.want {
			t.Errorf("GetManifestTTL(%q) = %v, want %v", tt.ref, got, tt.want)
		}
	}
}

func TestStore_CacheKey(t *testing.T) {
	// Same inputs should produce same key
	k1 := cache.CacheKey("GET", "docker.io", "library/busybox", "latest")
	k2 := cache.CacheKey("GET", "docker.io", "library/busybox", "latest")
	if k1 != k2 {
		t.Error("same inputs should produce same key")
	}

	// Different inputs should produce different keys
	k3 := cache.CacheKey("GET", "docker.io", "library/busybox", "v1.0")
	if k1 == k3 {
		t.Error("different inputs should produce different keys")
	}
}
