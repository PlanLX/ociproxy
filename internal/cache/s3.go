package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	s3cfg "github.com/planlx/ociproxy/internal/config"
)

const lastAccessedMetaKey = "x-amz-meta-last-accessed"

// S3Store manages OCI cache on S3-compatible storage.
type S3Store struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
	prefix   string
	encrypt  bool
	logger   *slog.Logger

	accessCh chan string
	wg       sync.WaitGroup
}

// NewS3Store creates a new S3 cache store.
func NewS3Store(ctx context.Context, cfg *s3cfg.StorageConfig, logger *slog.Logger) (*S3Store, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awscfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if cfg.RegionEndpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.RegionEndpoint)
		})
	}
	if cfg.ForcePathStyle {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awscfg, clientOpts...)
	uploader := manager.NewUploader(client)

	prefix := strings.Trim(cfg.RootDirectory, "/")
	if prefix != "" {
		prefix += "/"
	}

	s := &S3Store{
		client:   client,
		uploader: uploader,
		bucket:   cfg.Bucket,
		prefix:   prefix + "docker/registry/v2/",
		encrypt:  cfg.Encrypt,
		logger:   logger,
		accessCh: make(chan string, 1000),
	}

	s.wg.Add(1)
	go s.accessUpdateWorker()

	logger.Info("S3 cache initialized", "bucket", cfg.Bucket, "prefix", s.prefix)
	return s, nil
}

// Close flushes pending access updates and stops the worker.
func (s *S3Store) Close() {
	close(s.accessCh)
	s.wg.Wait()
}

func (s *S3Store) blobKey(digest string) string {
	if algo, hex, ok := parseDigest(digest); ok {
		return s.prefix + "blobs/" + algo + "/" + hex[:2] + "/" + hex + "/data"
	}
	return s.prefix + "blobs/unknown/" + digest + "/data"
}

func (s *S3Store) manifestLinkKey(registry, repo, digest string) string {
	safeRepo := sanitizePathS3(repo)
	if algo, hex, ok := parseDigest(digest); ok {
		return s.prefix + "repositories/" + registry + "/" + safeRepo + "/_manifests/revisions/" + algo + "/" + hex + "/link"
	}
	return s.prefix + "repositories/" + registry + "/" + safeRepo + "/_manifests/revisions/" + digest + "/link"
}

func sanitizePathS3(p string) string {
	return strings.Trim(p, "/")
}

func (s *S3Store) GetManifest(registry, repo, digest string) ([]byte, string, error) {
	key := s.blobKey(digest)
	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("S3 GetObject: %w", err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read manifest body: %w", err)
	}

	s.queueAccessUpdate(key)
	s.logger.Debug("cache hit", "type", "manifest", "key", key)
	return data, detectMediaType(data), nil
}

func (s *S3Store) PutManifest(registry, repo, digest, tag string, data []byte, mediaType string) error {
	blobKey := s.blobKey(digest)
	if err := s.putObject(blobKey, data); err != nil {
		return err
	}
	linkKey := s.manifestLinkKey(registry, repo, digest)
	if err := s.putObject(linkKey, []byte(digest)); err != nil {
		return err
	}

	// Tag entries (when ref is a tag, not a digest)
	if tag != "" && !strings.HasPrefix(tag, "sha256:") && !strings.HasPrefix(tag, "sha512:") {
		safeRepo := sanitizePathS3(repo)
		base := s.prefix + "repositories/" + registry + "/" + safeRepo + "/_manifests/tags/" + tag

		currentKey := base + "/current/link"
		if err := s.putObject(currentKey, []byte(digest)); err != nil {
			return err
		}

		if algo, hex, ok := parseDigest(digest); ok {
			indexKey := base + "/index/" + algo + "/" + hex + "/link"
			if err := s.putObject(indexKey, []byte(digest)); err != nil {
				return err
			}
		}
	}

	s.logger.Info("cache store", "type", "manifest", "bucket", s.bucket, "blob", blobKey, "link", linkKey, "tag", tag, "size", len(data))
	return nil
}

func (s *S3Store) GetBlob(registry, repo, digest string) (io.ReadCloser, int64, error) {
	key := s.blobKey(digest)
	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("S3 GetObject: %w", err)
	}

	size := aws.ToInt64(out.ContentLength)
	s.logger.Debug("cache hit", "type", "blob", "key", key, "size", size)
	return &accessTrackingReader{rc: out.Body, onDone: func() { s.queueAccessUpdate(key) }}, size, nil
}

func (s *S3Store) PutBlob(registry, repo, digest string, reader io.Reader, size int64) error {
	key := s.blobKey(digest)
	_, err := s.uploader.Upload(context.Background(), &s3.PutObjectInput{
		Bucket:             aws.String(s.bucket),
		Key:                aws.String(key),
		Body:               reader,
		ServerSideEncryption: s.serverSideEncryption(),
		Metadata: map[string]string{
			lastAccessedMetaKey: time.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return fmt.Errorf("S3 Upload: %w", err)
	}
	return nil
}

func (s *S3Store) Exists(registry, repo, digest string) (bool, error) {
	_, err := s.client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.blobKey(digest)),
	})
	if err == nil {
		return true, nil
	}
	if isS3NotFound(err) {
		_, err2 := s.client.HeadObject(context.Background(), &s3.HeadObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.manifestLinkKey(registry, repo, digest)),
		})
		return err2 == nil, nil
	}
	return false, fmt.Errorf("S3 HeadObject: %w", err)
}

func (s *S3Store) Evict(ctx context.Context, maxAge time.Duration, dryRun bool) (*EvictionResult, error) {
	cutoff := time.Now().Add(-maxAge)
	result := &EvictionResult{}

	blobPrefix := s.prefix + "blobs/"
	if err := s.evictPrefix(ctx, blobPrefix, "data", cutoff, dryRun, result); err != nil {
		return nil, err
	}

	linkPrefix := s.prefix + "repositories/"
	if err := s.evictPrefix(ctx, linkPrefix, "link", cutoff, dryRun, result); err != nil {
		return nil, err
	}

	return result, nil
}

func (s *S3Store) evictPrefix(ctx context.Context, prefix, suffix string, cutoff time.Time, dryRun bool, result *EvictionResult) error {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	var keys []string
	for paginator.HasMorePages() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("S3 ListObjectsV2: %w", err)
		}

		for _, obj := range page.Contents {
			if !strings.HasSuffix(*obj.Key, "/"+suffix) {
				continue
			}
			age := aws.ToTime(obj.LastModified)
			if age.Before(cutoff) {
				keys = append(keys, *obj.Key)
				result.EvictedBytes += aws.ToInt64(obj.Size)
				if dryRun {
					result.EvictedFiles = append(result.EvictedFiles, *obj.Key)
				}
				result.EvictedCount++
			}
		}
	}

	if dryRun || len(keys) == 0 {
		return nil
	}

	for i := 0; i < len(keys); i += 1000 {
		end := i + 1000
		if end > len(keys) {
			end = len(keys)
		}
		var objs []types.ObjectIdentifier
		for _, k := range keys[i:end] {
			objs = append(objs, types.ObjectIdentifier{Key: aws.String(k)})
		}
		_, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(true)},
		})
		if err != nil {
			s.logger.Warn("S3 batch delete failed", "error", err)
		}
	}

	return nil
}

type accessTrackingReader struct {
	rc     io.ReadCloser
	onDone func()
	done   sync.Once
}

func (r *accessTrackingReader) Read(p []byte) (int, error) {
	return r.rc.Read(p)
}

func (r *accessTrackingReader) Close() error {
	r.done.Do(r.onDone)
	return r.rc.Close()
}

func (s *S3Store) queueAccessUpdate(key string) {
	select {
	case s.accessCh <- key:
	default:
	}
}

func (s *S3Store) accessUpdateWorker() {
	defer s.wg.Done()

	pending := make(map[string]bool)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case key, ok := <-s.accessCh:
			if !ok {
				for k := range pending {
					s.updateLastAccessed(context.Background(), k)
				}
				return
			}
			pending[key] = true
		case <-ticker.C:
			for k := range pending {
				s.updateLastAccessed(context.Background(), k)
			}
			pending = make(map[string]bool)
		}
	}
}

func (s *S3Store) updateLastAccessed(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(s.bucket),
		CopySource:        aws.String(s.bucket + "/" + key),
		Key:               aws.String(key),
		MetadataDirective: types.MetadataDirectiveReplace,
		Metadata: map[string]string{
			lastAccessedMetaKey: now,
		},
		ServerSideEncryption: s.serverSideEncryption(),
	})
	return err
}

func (s *S3Store) putObject(key string, data []byte) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(string(data)),
		Metadata: map[string]string{
			lastAccessedMetaKey: time.Now().UTC().Format(time.RFC3339),
		},
	}
	if s.encrypt {
		input.ServerSideEncryption = types.ServerSideEncryptionAes256
	}
	_, err := s.client.PutObject(context.Background(), input)
	return err
}

func (s *S3Store) serverSideEncryption() types.ServerSideEncryption {
	if s.encrypt {
		return types.ServerSideEncryptionAes256
	}
	return ""
}

func isS3NotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "404") || strings.Contains(msg, "NoSuchKey")
}
