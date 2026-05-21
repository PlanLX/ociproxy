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
	Storage       StorageConfig `yaml:"storage"`
	DefaultRegistry string      `yaml:"default_registry"`
}

// NewHandler creates a distribution registry with S3 storage and pull-through cache.
// Proxy handler runs on an internal port as the remote upstream.
func NewHandler(ctx context.Context, cfg Config, proxyHandler http.Handler, logger *slog.Logger) (http.Handler, func(), error) {
	srvLn, err := startInternalServer(proxyHandler, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("create internal listener: %w", err)
	}

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
			RemoteURL: srvLn.URL,
		},
	}

	distApp := handlers.NewApp(ctx, distCfg)

	// Rewrite path to always include registry namespace, so S3 storage paths
	// are namespaced correctly (e.g., docker.io/library/busybox not just library/busybox).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v2/") && r.URL.Path != "/v2" {
			distApp.ServeHTTP(w, r)
			return
		}

		ns := r.URL.Query().Get("ns")
		rest := strings.TrimPrefix(r.URL.Path, "/v2/")
		var newPath string

		if ns != "" {
			// Client specified ns — prepend it as registry namespace
			newPath = "/v2/" + ns + "/" + rest
		} else if !strings.Contains(rest, ".") {
			// No ns, and first segment doesn't look like a hostname (e.g., "library")
			// This is a default registry path — prepend the default registry
			defaultReg := cfg.DefaultRegistry
			if defaultReg == "" {
				defaultReg = "docker.io"
			}
			newPath = "/v2/" + defaultReg + "/" + rest
		}

		if newPath != "" {
			r2 := r.Clone(r.Context())
			r2.URL.Path = newPath
			r2.URL.RawQuery = ""
			distApp.ServeHTTP(w, r2)
			return
		}

		distApp.ServeHTTP(w, r)
	})

	logger.Info("embedded registry initialized",
		"storage", cfg.Storage.Type,
		"bucket", cfg.Storage.Bucket,
		"rootdirectory", cfg.Storage.RootDirectory,
		"proxy_url", srvLn.URL,
	)

	return handler, func() { srvLn.Close() }, nil
}

type srvListener struct {
	*http.Server
	URL string
}

func (sl *srvListener) Close() {
	sl.Shutdown(context.Background())
}

func startInternalServer(proxyHandler http.Handler, logger *slog.Logger) (*srvListener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("create listener: %w", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	internalURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	srv := &http.Server{
		Handler:      proxyHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("internal proxy server error", "error", err)
		}
	}()

	return &srvListener{Server: srv, URL: internalURL}, nil
}
