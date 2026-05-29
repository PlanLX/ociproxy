// Package cache provides a pull-through cache for OCI manifests and blobs.
// It supports filesystem and S3 backends via the Store interface.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Store is the interface for cache backends.
type Store interface {
	GetManifest(registry, repo, digest string) ([]byte, string, error)
	PutManifest(registry, repo, digest, tag string, data []byte, mediaType string) error
	GetBlob(registry, repo, digest string) (io.ReadCloser, int64, error)
	PutBlob(registry, repo, digest string, reader io.Reader, size int64) error
	Exists(registry, repo, digest string) (bool, error)
	Evict(ctx context.Context, maxAge time.Duration, dryRun bool) (*EvictionResult, error)
}

// EvictionResult summarizes what was evicted.
type EvictionResult struct {
	EvictedFiles   []string
	EvictedBytes   int64
	EvictedCount   int
	RemainingCount int
}

// ManifestCache is an in-memory TTL cache for manifests.
type ManifestCache struct {
	cache sync.Map
}

// manifestEntry holds a cached manifest with expiry.
type manifestEntry struct {
	data      []byte
	mediaType string
	expiresAt time.Time
}

// Data returns the cached manifest body.
func (e *manifestEntry) Data() []byte     { return e.data }

// MediaType returns the cached manifest's media type.
func (e *manifestEntry) MediaType() string { return e.mediaType }

// Get retrieves a manifest from memory cache.
func (c *ManifestCache) Get(key string) (*manifestEntry, bool) {
	if v, ok := c.cache.Load(key); ok {
		e := v.(*manifestEntry)
		if time.Now().Before(e.expiresAt) {
			return e, true
		}
		c.cache.Delete(key)
	}
	return nil, false
}

// Set stores a manifest in memory cache.
func (c *ManifestCache) Set(key string, data []byte, mediaType string, ttl time.Duration) {
	c.cache.Store(key, &manifestEntry{
		data:      data,
		mediaType: mediaType,
		expiresAt: time.Now().Add(ttl),
	})
}

// GetManifestTTL returns appropriate TTL based on the reference type.
func GetManifestTTL(ref string) time.Duration {
	if strings.HasPrefix(ref, "sha256:") {
		return 24 * time.Hour
	}
	switch ref {
	case "latest", "main", "master", "dev", "develop":
		return 10 * time.Minute
	}
	return 30 * time.Minute
}

// Cleanup periodically removes expired entries.
func (c *ManifestCache) Cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			c.cache.Range(func(key, value any) bool {
				if value.(*manifestEntry).expiresAt.Before(now) {
					c.cache.Delete(key)
				}
				return true
			})
		}
	}()
}

// CacheKey computes a short stable key for in-memory caching.
func CacheKey(method, registry, repo, ref string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%s", method, registry, repo, ref)))
	return hex.EncodeToString(h[:8])
}
