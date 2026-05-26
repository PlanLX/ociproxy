// Package cache provides local filesystem pull-through cache for OCI manifests and blobs.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Store manages local filesystem storage for OCI images using distribution registry layout:
//
//	docker/registry/v2/
//	  blobs/<algo>/<2-char-prefix>/<hex>/data          — content-addressed blob data
//	  repositories/<registry>/<repo>/
//	    _manifests/revisions/<algo>/<hex>/link          — manifest link (contains digest string)
type Store struct {
	root   string
	logger *slog.Logger
}

// NewStore creates a new local cache store.
func NewStore(root string, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Store{
		root:   root,
		logger: logger,
	}
}

// Root returns the cache root directory path.
func (s *Store) Root() string { return s.root }

// parseDigest splits "sha256:abc123" into (algo="sha256", hex="abc123").
func parseDigest(digest string) (algo, hex string, ok bool) {
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) == 2 && len(parts[0]) > 0 && len(parts[1]) >= 2 {
		return parts[0], parts[1], true
	}
	return "", "", false
}

// blobPath constructs the distribution blob path from a digest.
func (s *Store) blobPath(digest string) string {
	// blobs/<algo>/<2-char-prefix>/<hex>/data
	if algo, hex, ok := parseDigest(digest); ok {
		return filepath.Join(s.root, "docker", "registry", "v2", "blobs", algo, hex[:2], hex, "data")
	}
	return filepath.Join(s.root, "docker", "registry", "v2", "blobs", "unknown", digest, "data")
}

// manifestLinkPath returns the distribution _manifests link path.
func (s *Store) manifestLinkPath(registry, repo, digest string) string {
	safeRepo := sanitizePath(repo)
	// repositories/<registry>/<repo>/_manifests/revisions/<algo>/<hex>/link
	if algo, hex, ok := parseDigest(digest); ok {
		return filepath.Join(s.root, "docker", "registry", "v2", "repositories", registry, safeRepo, "_manifests", "revisions", algo, hex, "link")
	}
	return filepath.Join(s.root, "docker", "registry", "v2", "repositories", registry, safeRepo, "_manifests", "revisions", digest, "link")
}

// GetManifest retrieves a cached manifest from the blob data.
func (s *Store) GetManifest(registry, repo, digest string) ([]byte, string, error) {
	path := s.blobPath(digest)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("read manifest: %w", err)
	}

	mediaType := detectMediaType(data)

	s.logger.Debug("cache hit", "type", "manifest", "path", path)
	return data, mediaType, nil
}

// PutManifest stores a manifest as a blob and creates the repository link file.
// This mirrors how distribution's pull-through cache works:
//  1. The manifest body lives under blobs/<digest>/data
//  2. A link file under repositories/<registry>/<repo>/_manifests/revisions/<algo>/<hex>/link
//     contains the digest string (e.g., "sha256:abc123")
func (s *Store) PutManifest(registry, repo, digest string, data []byte, mediaType string) error {
	// 1. Store manifest body as a blob
	blobPath := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}
	if err := os.WriteFile(blobPath, data, 0644); err != nil {
		return fmt.Errorf("write manifest blob: %w", err)
	}

	// 2. Create repository link file
	linkPath := s.manifestLinkPath(registry, repo, digest)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		return fmt.Errorf("create manifest link dir: %w", err)
	}
	if err := os.WriteFile(linkPath, []byte(digest), 0644); err != nil {
		return fmt.Errorf("write manifest link: %w", err)
	}

	s.logger.Debug("cache store", "type", "manifest", "blob", blobPath, "link", linkPath, "size", len(data))
	return nil
}

// GetBlob opens a cached blob for streaming.
func (s *Store) GetBlob(registry, repo, digest string) (io.ReadCloser, int64, error) {
	_, _ = registry, repo // unused — blobs are content-addressed globally
	path := s.blobPath(digest)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("open blob: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("stat blob: %w", err)
	}

	s.logger.Debug("cache hit", "type", "blob", "path", path, "size", info.Size())
	return f, info.Size(), nil
}

// PutBlob stores a blob in the cache by streaming from reader.
func (s *Store) PutBlob(registry, repo, digest string, reader io.Reader, size int64) error {
	_, _ = registry, repo // unused — blobs are content-addressed globally
	path := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}

	// Write to temp file first, then rename atomically
	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp blob: %w", err)
	}

	written, err := io.Copy(f, reader)
	if err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write blob: %w", err)
	}
	f.Close()

	// Use os.Rename for atomicity; fallback to copy+delete on Windows cross-volume
	if err := os.Rename(tmpPath, path); err != nil {
		if err2 := copyFile(tmpPath, path); err2 != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("copy blob fallback: %w", err2)
		}
		os.Remove(tmpPath)
	}

	s.logger.Debug("cache store", "type", "blob", "path", path, "size", written)
	return nil
}

// Exists checks if any content exists for the given digest.
func (s *Store) Exists(registry, repo, digest string) (bool, error) {
	// Check blob (manifests are also stored as blobs)
	if _, err := os.Stat(s.blobPath(digest)); err == nil {
		return true, nil
	}

	// Check manifest link
	if _, err := os.Stat(s.manifestLinkPath(registry, repo, digest)); err == nil {
		return true, nil
	}

	return false, nil
}

// sanitizePath makes a repo path safe for filesystem use.
func sanitizePath(p string) string {
	return strings.ReplaceAll(p, "/", string(filepath.Separator))
}

// copyFile copies a file from src to dst (fallback for Windows cross-volume rename).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// detectMediaType attempts to detect the media type from manifest JSON.
func detectMediaType(data []byte) string {
	// Simple detection: look for known media type strings
	s := string(data)
	if strings.Contains(s, "application/vnd.oci.image.index.v1+json") {
		return "application/vnd.oci.image.index.v1+json"
	}
	if strings.Contains(s, "application/vnd.oci.image.manifest.v1+json") {
		return "application/vnd.oci.image.manifest.v1+json"
	}
	if strings.Contains(s, "application/vnd.docker.distribution.manifest.list.v2+json") {
		return "application/vnd.docker.distribution.manifest.list.v2+json"
	}
	if strings.Contains(s, "application/vnd.docker.distribution.manifest.v2+json") {
		return "application/vnd.docker.distribution.manifest.v2+json"
	}
	if strings.Contains(s, "application/vnd.docker.container.image.v1+json") {
		return "application/vnd.docker.container.image.v1+json"
	}
	if strings.HasPrefix(strings.TrimSpace(s), "{") {
		return "application/json"
	}
	return "application/octet-stream"
}

// cacheKey computes a short stable key for in-memory caching.
func CacheKey(method, registry, repo, ref string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%s", method, registry, repo, ref)))
	return hex.EncodeToString(h[:8])
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

// EvictionResult summarizes what was evicted.
type EvictionResult struct {
	EvictedFiles []string
	EvictedBytes int64
	EvictedCount int
	RemainingCount int
}

// Evict removes blobs and manifest links older than maxAge.
// Uses mtime as proxy for last access (atime is unreliable on most filesystems).
func (s *Store) Evict(maxAge time.Duration, dryRun bool) (*EvictionResult, error) {
	cutoff := time.Now().Add(-maxAge)
	result := &EvictionResult{}

	blobRoot := filepath.Join(s.root, "docker", "registry", "v2", "blobs")
	repoRoot := filepath.Join(s.root, "docker", "registry", "v2", "repositories")

	// Evict blobs
	blobCount := 0
	err := filepath.WalkDir(blobRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "data" || d.IsDir() {
			return nil
		}
		blobCount++
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if dryRun {
				result.EvictedFiles = append(result.EvictedFiles, path)
				result.EvictedBytes += info.Size()
			} else {
				os.Remove(path)
				cleanEmptyDirs(filepath.Dir(path), blobRoot)
				result.EvictedBytes += info.Size()
			}
			result.EvictedCount++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("scan blobs: %w", err)
	}
	result.RemainingCount = blobCount - result.EvictedCount

	// Evict manifest links
	if err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "link" || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if dryRun {
				result.EvictedFiles = append(result.EvictedFiles, path)
			} else {
				os.Remove(path)
				cleanEmptyDirs(filepath.Dir(path), repoRoot)
			}
			result.EvictedCount++
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("scan manifest links: %w", err)
	}

	return result, nil
}

// cleanEmptyDirs removes empty directories up to the root boundary.
func cleanEmptyDirs(dir, root string) {
	for {
		if dir == root || dir == filepath.Dir(root) {
			break
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		os.Remove(dir)
		dir = filepath.Dir(dir)
	}
}
