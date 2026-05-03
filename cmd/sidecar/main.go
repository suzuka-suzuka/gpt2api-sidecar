package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"gpt2api-sidecar/internal/config"
	"gpt2api-sidecar/internal/pool"
	"gpt2api-sidecar/internal/runner"
	"gpt2api-sidecar/internal/server"
	"gpt2api-sidecar/pkg/logger"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to sidecar config yaml")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		panic(err)
	}

	if err := logger.Init(cfg.Server.LogLevel, cfg.Server.LogFormat, "stdout"); err != nil {
		panic(err)
	}
	defer logger.Sync()

	minInterval, err := cfg.MinIntervalDuration()
	if err != nil {
		panic(err)
	}
	cooldown429, err := cfg.Cooldown429Duration()
	if err != nil {
		panic(err)
	}
	blobTTL, err := cfg.BlobTTLDuration()
	if err != nil {
		panic(err)
	}
	requestTimeout, err := cfg.RequestTimeoutDuration()
	if err != nil {
		panic(err)
	}

	stateStore := pool.StateStore(pool.NewMemoryStateStore())
	if redisStore, err := pool.NewRedisStateStore(context.Background(), pool.RedisOptions{
		Addr:      cfg.Redis.Addr,
		Password:  cfg.Redis.Password,
		DB:        cfg.Redis.DB,
		KeyPrefix: cfg.Redis.KeyPrefix,
	}); err != nil {
		logger.L().Warn("redis state store unavailable; falling back to in-memory account state",
			zap.String("addr", cfg.Redis.Addr),
			zap.Error(err),
		)
	} else {
		defer redisStore.Close()
		stateStore = redisStore
		logger.L().Info("redis state store connected",
			zap.String("addr", cfg.Redis.Addr),
			zap.Int("db", cfg.Redis.DB),
			zap.String("key_prefix", cfg.Redis.KeyPrefix),
		)
	}

	accountPool := pool.NewWithStore(cfg.Accounts, minInterval, stateStore)
	imageRunner := runner.New(accountPool, cooldown429, cfg.Server.MaxImageBytes)
	sidecarServer := server.New(cfg, *configPath, accountPool, imageRunner, blobTTL)

	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           sidecarServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.L().Info("gpt2api-sidecar listening",
			zap.String("listen", cfg.Server.Listen),
			zap.String("public_base_url", cfg.Server.PublicBaseURL),
			zap.Int("accounts", len(cfg.Accounts)),
			zap.Int("models", len(cfg.Models)),
		)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.L().Fatal("http server crashed", zap.Error(err))
		}
	}()

	stopSignal := make(chan os.Signal, 1)
	signal.Notify(stopSignal, syscall.SIGINT, syscall.SIGTERM)
	<-stopSignal

	shutdownCtx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.L().Error("graceful shutdown failed", zap.Error(err))
	}
}
