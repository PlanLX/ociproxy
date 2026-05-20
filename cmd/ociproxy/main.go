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

	// Create router
	h, err := router.New(cfg, logger)
	if err != nil {
		logger.Error("failed to create router", "error", err)
		os.Exit(1)
	}

	// Start server
	srv := &http.Server{
		Addr:         addr,
		Handler:      h,
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	proxy.CloseAll()
	logger.Info("shutdown complete")
}
