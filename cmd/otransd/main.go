package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/otrans-app/otrans-fn/internal/config"
	"github.com/otrans-app/otrans-fn/internal/server"
	"github.com/otrans-app/otrans-fn/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(config.DataDir, 0o750); err != nil {
		logger.Error("cannot create data directory", "error", err)
		os.Exit(1)
	}

	dataStore, err := store.Open(config.DatabasePath)
	if err != nil {
		logger.Error("cannot open database", "error", err)
		os.Exit(1)
	}
	defer dataStore.Close()

	api, err := server.New(server.Config{
		AdminToken: config.AdminToken, BlobDir: config.BlobDir, MaxUploadBytes: config.MaxUploadBytes,
	}, dataStore, logger)
	if err != nil {
		logger.Error("cannot initialize server", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              config.ListenAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	shutdownSignals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownSignals.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("OTrans server started", "address", config.ListenAddr, "dataDir", config.DataDir)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}
