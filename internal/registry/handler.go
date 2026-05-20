// Package registry provides an embedded OCI registry with S3 pull-through caching.
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
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
	Storage StorageConfig `yaml:"storage"`
}

// NewHandler creates a distribution registry with S3 storage and pull-through cache.
// The proxy handler runs on an internal port as the remote upstream.
func NewHandler(ctx context.Context, cfg Config, proxyHandler http.Handler, logger *slog.Logger) (http.Handler, func(), error) {
	srvListener, err := startInternalServer(proxyHandler, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("create internal listener: %w", err)
	}

	// Build distribution config with proxy as pull-through upstream
	s3Params := configuration.Parameters{
		"region":         cfg.Storage.Region,
		"bucket":         cfg.Storage.Bucket,
		"accesskey":      cfg.Storage.AccessKey,
		"secretkey":      cfg.Storage.SecretKey,
		"regionendpoint": cfg.Storage.RegionEndpoint,
		"secure":         cfg.Storage.Secure,
		"forcepathstyle": cfg.Storage.ForcePathStyle,
		"encrypt":        cfg.Storage.Encrypt,
	}

	if cfg.Storage.RootDirectory != "" {
		s3Params["rootdirectory"] = cfg.Storage.RootDirectory
	}

	distCfg := &configuration.Configuration{
		Version: "0.1",
		Storage: configuration.Storage{
			"s3": s3Params,
		},
		Proxy: configuration.Proxy{
			RemoteURL: srvListener.URL,
		},
	}

	distApp := handlers.NewApp(ctx, distCfg)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v2/") && r.URL.Path != "/v2" {
			distApp.ServeHTTP(w, r)
			return
		}
		distApp.ServeHTTP(w, r)
	})

	logger.Info("embedded registry initialized",
		"storage", cfg.Storage.Type,
		"bucket", cfg.Storage.Bucket,
		"rootdirectory", cfg.Storage.RootDirectory,
		"proxy_url", srvListener.URL,
	)

	return handler, func() { srvListener.Close() }, nil
}

type serverListener struct {
	*http.Server
	URL string
}

func (sl *serverListener) Close() {
	sl.Shutdown(context.Background())
}

func startInternalServer(proxyHandler http.Handler, logger *slog.Logger) (*serverListener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("create listener: %w", err)
	}

	internalPort := ln.Addr().(*net.TCPAddr).Port
	internalURL := fmt.Sprintf("http://127.0.0.1:%d", internalPort)

	proxyServer := &http.Server{
		Handler:      proxyHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		if err := proxyServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("internal proxy server error", "error", err)
		}
	}()

	return &serverListener{Server: proxyServer, URL: internalURL}, nil
}
