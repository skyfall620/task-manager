package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/skyfall620/task-manager/internal/auth"
	"github.com/skyfall620/task-manager/internal/config"
	"github.com/skyfall620/task-manager/internal/httpapi"
	"github.com/skyfall620/task-manager/internal/mysql"
	"github.com/skyfall620/task-manager/internal/redis"
	"github.com/skyfall620/task-manager/internal/stats"
	"github.com/skyfall620/task-manager/internal/tasks"
	"github.com/skyfall620/task-manager/internal/teams"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("application failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := mysql.Open(ctx, cfg.MySQL)
	if err != nil {
		return fmt.Errorf("initialize MySQL: %w", err)
	}

	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("close MySQL", "error", err)
		}
	}()
	logger.Info("MySQL connection established")

	redisClient := redis.NewClient(cfg.Redis)
	defer func() {
		if err := redisClient.Close(); err != nil {
			logger.Error("close Redis", "error", err)
		}
	}()
	pingCtx, cancelPing := context.WithTimeout(ctx, cfg.Redis.PingTimeout)
	redisErr := redisClient.Ping(pingCtx).Err()
	cancelPing()
	if redisErr != nil {
		logger.Warn("Redis unavailable; continuing startup", "error", redisErr)
	} else {
		logger.Info("Redis connection established")
	}

	if ctx.Err() != nil {
		return nil
	}

	tokens, err := auth.NewTokenManager(cfg.Auth.JWTSecret, cfg.Auth.JWTIssuer, cfg.Auth.JWTAudience, cfg.Auth.JWTTTL)
	if err != nil {
		return fmt.Errorf("initialize JWT: %w", err)
	}

	authService, err := auth.NewService(auth.NewRepository(db), tokens, cfg.Auth.BcryptCost)
	if err != nil {
		return fmt.Errorf("initialize authentication: %w", err)
	}

	teamService := teams.NewService(teams.NewRepository(db))
	taskCache := tasks.NewRedisListCache(redisClient, "task-manager:tasks:v1")
	taskService := tasks.NewServiceWithCache(tasks.NewRepository(db), taskCache)
	statsService := stats.NewService(stats.NewRepository(db))
	server := httpapi.NewServer(cfg, auth.NewHandler(authService), teams.NewHandler(teamService), tasks.NewHandler(taskService), stats.NewHandler(statsService), tokens)
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()
	logger.Info("HTTP server starting", "address", cfg.HTTPAddr)

	select {
	case err := <-serverErrors:
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		stop()
		logger.Info("HTTP server shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	return shutdownHTTPServer(shutdownCtx, server, serverErrors, logger)
}

func shutdownHTTPServer(ctx context.Context, server *http.Server, serverErrors <-chan error, logger *slog.Logger) error {
	if err := server.Shutdown(ctx); err != nil {
		if closeErr := server.Close(); closeErr != nil {
			logger.Error("force close HTTP server", "error", closeErr)
		}

		return fmt.Errorf("shutdown HTTP server: %w", err)
	}

	if err := <-serverErrors; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", err)
	}

	logger.Info("HTTP server stopped")
	return nil
}
