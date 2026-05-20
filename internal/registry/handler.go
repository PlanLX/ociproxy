// Package registry provides an embedded OCI registry with pull-through caching.
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/s3-aws"
)

// StorageConfig holds S3 storage configuration.
type StorageConfig struct {
	Type           string `yaml:"type"`
	Region         string `yaml:"region"`
	Bucket         string `yaml:"bucket"`
	AccessKey      string `yaml:"accesskey"`
	SecretKey      string `yaml:"secretkey"`
	RootDirectory  string `yaml:"rootdirectory"`
	Secure         bool   `yaml:"secure"`
	ForcePathStyle bool   `yaml:"forcepathstyle"`
	RegionEndpoint string `yaml:"regionendpoint"`
	Encrypt        bool   `yaml:"encrypt"`
}

// Config holds the embedded registry configuration.
type Config struct {
	Storage     StorageConfig `yaml:"storage"`
	ProxyRemoteURL string     `yaml:"proxy_remote_url"` // internal proxy URL for pull-through
}

// NewHandler creates a distribution registry handler with pull-through caching.
// It starts an internal HTTP server on a loopback port for the proxy to serve
// cache misses, then returns the registry as an http.Handler.
func NewHandler(ctx context.Context, cfg Config, proxyHandler http.Handler, logger *slog.Logger) (http.Handler, func(), error) {
	// Start internal proxy server on a random localhost port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("create internal listener: %w", err)
	}

	internalPort := listener.Addr().(*net.TCPAddr).Port
	internalURL := fmt.Sprintf("http://127.0.0.1:%d", internalPort)

	proxyServer := &http.Server{
		Handler:      proxyHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		if err := proxyServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("internal proxy server error", "error", err)
		}
	}()

	cleanup := func() {
		proxyServer.Shutdown(context.Background())
	}

	// Build distribution config
	s3Params := configuration.Parameters{
		"bucket":          cfg.Storage.Bucket,
		"accesskey":       cfg.Storage.AccessKey,
		"secretkey":       cfg.Storage.SecretKey,
		"secure":          cfg.Storage.Secure,
		"forcepathstyle":  cfg.Storage.ForcePathStyle,
		"encrypt":         cfg.Storage.Encrypt,
		"v4auth":          true,
		"chunksize":       10485760, // 10MB
	}

	// Region is required by the S3 driver. For MinIO any value works.
	region := cfg.Storage.Region
	if region == "" {
		region = "us-east-1"
	}
	s3Params["region"] = region

	if cfg.Storage.RootDirectory != "" {
		s3Params["rootdirectory"] = cfg.Storage.RootDirectory
	}
	if cfg.Storage.RegionEndpoint != "" {
		s3Params["regionendpoint"] = cfg.Storage.RegionEndpoint
	}

	distCfg := &configuration.Configuration{
		Version: "0.1",
		Storage: configuration.Storage{
			"s3": s3Params,
		},
		Proxy: configuration.Proxy{
			RemoteURL: internalURL,
		},
	}

	// Create the distribution app (pull-through cache)
	app := handlers.NewApp(ctx, distCfg)

	logger.Info("embedded registry initialized",
		"storage", cfg.Storage.Type,
		"bucket", cfg.Storage.Bucket,
		"proxy_url", internalURL,
	)

	return app, cleanup, nil
}
