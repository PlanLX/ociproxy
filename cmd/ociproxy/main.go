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

	var localCache *cache.Store
	if *cacheDir != "" {
		localCache = cache.NewStore(*cacheDir, logger)
		logger.Info("local cache enabled", "dir", *cacheDir)
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
	logger.Info("shutdown complete")
}

func runCacheEvict(args []string) {
	fs := flag.NewFlagSet("cache-evict", flag.ExitOnError)
	cacheDir := fs.String("cache-dir", "", "cache root directory (e.g. .data)")
	maxAge := fs.Duration("max-age", 7*24*time.Hour, "evict blobs older than this duration")
	dryRun := fs.Bool("dry-run", false, "show what would be evicted without deleting")
	fs.Parse(args)

	if *cacheDir == "" {
		fmt.Fprintln(os.Stderr, "error: --cache-dir is required")
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	store := cache.NewStore(*cacheDir, logger)

	fmt.Printf("scanning cache: %s (max-age=%s, dry-run=%v)\n", *cacheDir, *maxAge, *dryRun)

	result, err := store.Evict(*maxAge, *dryRun)
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
}
