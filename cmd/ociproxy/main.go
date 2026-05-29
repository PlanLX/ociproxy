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

	"github.com/planlx/ociproxy/internal/cache"
	"github.com/planlx/ociproxy/internal/config"
	"github.com/planlx/ociproxy/internal/proxy"
	"github.com/planlx/ociproxy/internal/router"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "cache-evict" {
		runCacheEvict(os.Args[2:])
		return
	}

	var (
		configPath string
		addr       string
		logLevel   string
	)
	flag.StringVar(&configPath, "config", "", "path to config YAML file")
	flag.StringVar(&addr, "addr", ":8080", "listen address")
	flag.StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	cacheDir := flag.String("cache-dir", "", "local filesystem cache directory (empty = no cache)")
	flag.Parse()

	if configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required")
		os.Exit(1)
	}

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

	cfg, err := config.LoadFile(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	var localCache cache.Store
	if *cacheDir != "" {
		localCache = cache.NewStore(*cacheDir, logger)
		logger.Info("local cache enabled", "dir", *cacheDir)
	} else {
		storage := cfg.Storage
		source := "config"
		if storage == nil {
			storage = config.StorageFromEnv()
			source = "env"
		}
		if storage != nil && storage.Type == "s3" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			s3store, err := cache.NewS3Store(ctx, storage, logger)
			cancel()
			if err != nil {
				logger.Error("failed to initialize S3 cache", "error", err)
				os.Exit(1)
			}
			localCache = s3store
			logger.Info("S3 cache enabled", "bucket", storage.Bucket, "prefix", storage.RootDirectory, "source", source)
		} else {
			logger.Info("no cache configured (neither --cache-dir, config storage, nor S3 env vars)", "config_storage", cfg.Storage != nil)
		}
	}

	px := proxy.New(cfg, localCache, logger)
	handler, err := router.New(cfg, px, logger)
	if err != nil {
		logger.Error("failed to create router", "error", err)
		os.Exit(1)
	}

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

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	proxy.CloseAll()
	px.Close()
	logger.Info("shutdown complete")
}

func runCacheEvict(args []string) {
	fs := flag.NewFlagSet("cache-evict", flag.ExitOnError)
	cacheDir := fs.String("cache-dir", "", "cache root directory (e.g. .data)")
	maxAge := fs.Duration("max-age", 7*24*time.Hour, "evict blobs older than this duration")
	dryRun := fs.Bool("dry-run", false, "show what would be evicted without deleting")

	// S3 flags
	s3Bucket := fs.String("s3-bucket", "", "S3 bucket name")
	s3Region := fs.String("s3-region", "us-east-1", "S3 region")
	s3AccessKey := fs.String("s3-access-key", "", "S3 access key")
	s3SecretKey := fs.String("s3-secret-key", "", "S3 secret key")
	s3Endpoint := fs.String("s3-endpoint", "", "S3 endpoint (for MinIO, etc.)")
	s3Prefix := fs.String("s3-prefix", "ociproxy", "S3 key prefix (e.g. ociproxy)")
	s3ForcePathStyle := fs.Bool("s3-force-path-style", false, "force path-style S3 URLs")
	s3Encrypt := fs.Bool("s3-encrypt", false, "enable server-side encryption")

	fs.Parse(args)

	var store cache.Store
	var closer interface{ Close() }

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *cacheDir != "" {
		store = cache.NewStore(*cacheDir, logger)
		fmt.Printf("scanning filesystem cache: %s (max-age=%s, dry-run=%v)\n", *cacheDir, *maxAge, *dryRun)
	} else if *s3Bucket != "" {
		s3cfg := &config.StorageConfig{
			Type:           "s3",
			Region:         *s3Region,
			Bucket:         *s3Bucket,
			AccessKey:      *s3AccessKey,
			SecretKey:      *s3SecretKey,
			RegionEndpoint: *s3Endpoint,
			RootDirectory:  *s3Prefix,
			ForcePathStyle: *s3ForcePathStyle,
			Encrypt:        *s3Encrypt,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		s3store, err := cache.NewS3Store(ctx, s3cfg, logger)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		store = s3store
		closer = s3store
		fmt.Printf("scanning S3 cache: bucket=%s prefix=%s (max-age=%s, dry-run=%v)\n", *s3Bucket, *s3Prefix, *maxAge, *dryRun)
	} else if envCfg := config.StorageFromEnv(); envCfg != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		s3store, err := cache.NewS3Store(ctx, envCfg, logger)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		store = s3store
		closer = s3store
		fmt.Printf("scanning S3 cache: bucket=%s prefix=%s (max-age=%s, dry-run=%v)\n", envCfg.Bucket, envCfg.RootDirectory, *maxAge, *dryRun)
	} else {
		fmt.Fprintln(os.Stderr, "error: --cache-dir or --s3-bucket is required")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := store.Evict(ctx, *maxAge, *dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	for _, f := range result.EvictedFiles {
		fmt.Println(f)
	}

	fmt.Printf("\nevicted: %d files, %.1f MB\n", result.EvictedCount, float64(result.EvictedBytes)/1024/1024)
	fmt.Printf("remaining: %d blobs\n", result.RemainingCount)

	if *dryRun {
		fmt.Println("(dry run — nothing was deleted)")
	}

	if closer != nil {
		closer.Close()
	}
}
