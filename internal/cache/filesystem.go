package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileStore manages local filesystem storage for OCI images using distribution registry layout:
//
//	docker/registry/v2/
//	  blobs/<algo>/<2-char-prefix>/<hex>/data          — content-addressed blob data
//	  repositories/<registry>/<repo>/
//	    _manifests/revisions/<algo>/<hex>/link          — manifest link (contains digest string)
type FileStore struct {
	root   string
	logger *slog.Logger
}

// NewStore creates a new filesystem cache store (alias for NewFileStore).
func NewStore(root string, logger *slog.Logger) *FileStore {
	return NewFileStore(root, logger)
}

// NewFileStore creates a new filesystem cache store.
func NewFileStore(root string, logger *slog.Logger) *FileStore {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &FileStore{
		root:   root,
		logger: logger,
	}
}

// Root returns the cache root directory path.
func (s *FileStore) Root() string { return s.root }

// parseDigest splits "sha256:abc123" into (algo="sha256", hex="abc123").
func parseDigest(digest string) (algo, hex string, ok bool) {
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) == 2 && len(parts[0]) > 0 && len(parts[1]) >= 2 {
		return parts[0], parts[1], true
	}
	return "", "", false
}

// blobPath constructs the distribution blob path from a digest.
func (s *FileStore) blobPath(digest string) string {
	if algo, hex, ok := parseDigest(digest); ok {
		return filepath.Join(s.root, "docker", "registry", "v2", "blobs", algo, hex[:2], hex, "data")
	}
	return filepath.Join(s.root, "docker", "registry", "v2", "blobs", "unknown", digest, "data")
}

// manifestLinkPath returns the distribution _manifests link path.
func (s *FileStore) manifestLinkPath(registry, repo, digest string) string {
	safeRepo := sanitizePath(repo)
	if algo, hex, ok := parseDigest(digest); ok {
		return filepath.Join(s.root, "docker", "registry", "v2", "repositories", registry, safeRepo, "_manifests", "revisions", algo, hex, "link")
	}
	return filepath.Join(s.root, "docker", "registry", "v2", "repositories", registry, safeRepo, "_manifests", "revisions", digest, "link")
}

// GetManifest retrieves a cached manifest from the blob data.
func (s *FileStore) GetManifest(registry, repo, digest string) ([]byte, string, error) {
	path := s.blobPath(digest)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", errf("read manifest: %w", err)
	}
	s.logger.Debug("cache hit", "type", "manifest", "path", path)
	return data, detectMediaType(data), nil
}

// PutManifest stores a manifest as a blob and creates the repository link files.
func (s *FileStore) PutManifest(registry, repo, digest, tag string, data []byte, mediaType string) error {
	blobPath := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		return errf("create blob dir: %w", err)
	}
	if err := os.WriteFile(blobPath, data, 0644); err != nil {
		return errf("write manifest blob: %w", err)
	}

	linkPath := s.manifestLinkPath(registry, repo, digest)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		return errf("create manifest link dir: %w", err)
	}
	if err := os.WriteFile(linkPath, []byte(digest), 0644); err != nil {
		return errf("write manifest link: %w", err)
	}

	// Tag entries (when ref is a tag, not a digest)
	if tag != "" && !strings.HasPrefix(tag, "sha256:") && !strings.HasPrefix(tag, "sha512:") {
		safeRepo := sanitizePath(repo)
		base := filepath.Join(s.root, "docker", "registry", "v2", "repositories", registry, safeRepo, "_manifests", "tags", tag)

		// current/link — points to the digest
		currentPath := filepath.Join(base, "current", "link")
		if err := os.MkdirAll(filepath.Dir(currentPath), 0755); err != nil {
			return errf("create tag current dir: %w", err)
		}
		if err := os.WriteFile(currentPath, []byte(digest), 0644); err != nil {
			return errf("write tag current link: %w", err)
		}

		// index/<algo>/<hex>/link — tag index entry
		if algo, hex, ok := parseDigest(digest); ok {
			indexPath := filepath.Join(base, "index", algo, hex, "link")
			if err := os.MkdirAll(filepath.Dir(indexPath), 0755); err != nil {
				return errf("create tag index dir: %w", err)
			}
			if err := os.WriteFile(indexPath, []byte(digest), 0644); err != nil {
				return errf("write tag index link: %w", err)
			}
		}
	}

	s.logger.Debug("cache store", "type", "manifest", "blob", blobPath, "link", linkPath, "tag", tag, "size", len(data))
	return nil
}

// GetBlob opens a cached blob for streaming.
func (s *FileStore) GetBlob(registry, repo, digest string) (io.ReadCloser, int64, error) {
	path := s.blobPath(digest)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, errf("open blob: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, errf("stat blob: %w", err)
	}

	s.logger.Debug("cache hit", "type", "blob", "path", path, "size", info.Size())
	return f, info.Size(), nil
}

// PutBlob stores a blob in the cache by streaming from reader.
func (s *FileStore) PutBlob(registry, repo, digest string, reader io.Reader, size int64) error {
	path := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return errf("create blob dir: %w", err)
	}

	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return errf("create temp blob: %w", err)
	}

	written, err := io.Copy(f, reader)
	if err != nil {
		f.Close()
		os.Remove(tmpPath)
		return errf("write blob: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, path); err != nil {
		if err2 := copyFile(tmpPath, path); err2 != nil {
			os.Remove(tmpPath)
			return errf("copy blob fallback: %w", err2)
		}
		os.Remove(tmpPath)
	}

	s.logger.Debug("cache store", "type", "blob", "path", path, "size", written)
	return nil
}

// Exists checks if any content exists for the given digest.
func (s *FileStore) Exists(registry, repo, digest string) (bool, error) {
	if _, err := os.Stat(s.blobPath(digest)); err == nil {
		return true, nil
	}
	if _, err := os.Stat(s.manifestLinkPath(registry, repo, digest)); err == nil {
		return true, nil
	}
	return false, nil
}

// Evict removes blobs and manifest links older than maxAge.
// Uses mtime as proxy for last access (atime is unreliable on most filesystems).
func (s *FileStore) Evict(ctx context.Context, maxAge time.Duration, dryRun bool) (*EvictionResult, error) {
	cutoff := time.Now().Add(-maxAge)
	result := &EvictionResult{}

	blobRoot := filepath.Join(s.root, "docker", "registry", "v2", "blobs")
	repoRoot := filepath.Join(s.root, "docker", "registry", "v2", "repositories")

	blobCount := 0
	err := filepath.WalkDir(blobRoot, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
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
		return nil, errf("scan blobs: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	result.RemainingCount = blobCount - result.EvictedCount

	if err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
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
		return nil, errf("scan manifest links: %w", err)
	}

	return result, nil
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

// errf wraps fmt.Errorf to avoid importing fmt in every call site.
func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
