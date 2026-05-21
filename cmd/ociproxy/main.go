package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/planlx/ociproxy/internal/config"
	"github.com/planlx/ociproxy/internal/proxy"
	"github.com/planlx/ociproxy/internal/registry"
	"github.com/planlx/ociproxy/internal/router"
)

var version = "dev"

func main() {
	var (
		configPath string
		addr       string
		logLevel   string
	)
	flag.StringVar(&configPath, "config", "", "path to config YAML file")
	flag.StringVar(&addr, "addr", ":8080", "listen address")
	flag.StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	flag.Parse()

	if configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required")
		os.Exit(1)
	}

	// Set up logger
	var level slog.Level
	switch logLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		fmt.Fprintf(os.Stderr, "error: unknown log level %q\n", logLevel)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	logger.Info("starting ociproxy", "version", version, "addr", addr, "config", configPath)

	// Load config
	cfg, err := config.LoadFile(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	var handler http.Handler

	if cfg.Storage != nil {
		// Embedded registry with pull-through cache
		px := proxy.New(cfg, logger)
		regHandler, cleanup, err := registry.NewHandler(ctx, registry.Config{
			Storage: registry.StorageConfig{
				Type:           cfg.Storage.Type,
				Region:         cfg.Storage.Region,
				Bucket:         cfg.Storage.Bucket,
				AccessKey:      cfg.Storage.AccessKey,
				SecretKey:      cfg.Storage.SecretKey,
				RootDirectory:  cfg.Storage.RootDirectory,
				Secure:         cfg.Storage.Secure,
				ForcePathStyle: cfg.Storage.ForcePathStyle,
				RegionEndpoint: cfg.Storage.RegionEndpoint,
				Encrypt:        cfg.Storage.Encrypt,
			},
			DefaultRegistry: cfg.DefaultRegistry,
		}, px, logger)
		if err != nil {
			logger.Error("failed to create embedded registry", "error", err)
			os.Exit(1)
		}
		defer cleanup()
		handler = regHandler

		logger.Info("embedded registry enabled", "storage", cfg.Storage.Type, "bucket", cfg.Storage.Bucket)
	} else {
		// Stateless proxy mode
		h, err := router.New(cfg, logger)
		if err != nil {
			logger.Error("failed to create router", "error", err)
			os.Exit(1)
		}
		handler = h
	}

	// Start server
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		logger.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down...")
	ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx2)
	proxy.CloseAll()
	logger.Info("shutdown complete")
}
