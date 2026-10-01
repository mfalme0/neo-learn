// Command api is the Neo Learn HTTP server.
//
// It is one process containing the REST API and, in later milestones, the
// outbox worker that drains notifications to SMS. Both consume the same
// learning engine; neither is allowed to write learner state directly.
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
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/neolearn/neolearn/internal/auth"
	"github.com/neolearn/neolearn/internal/config"
	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/httpapi"
	"github.com/neolearn/neolearn/internal/learning"
	"github.com/neolearn/neolearn/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	slog.Info("starting neo-learn api", "config", cfg.String())

	// A background context for the whole process lifetime, cancelled on
	// SIGINT/SIGTERM. Long-lived components use it so a shutdown signal
	// propagates through the tree.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	slog.Info("connected to postgres", "max_conns", pool.MaxConns())

	redisClient := newRedisClient(cfg.Redis)
	defer func() { _ = redisClient.Close() }()
	if err := pingRedis(ctx, redisClient); err != nil {
		return err
	}
	slog.Info("connected to redis", "addr", cfg.Redis.Addr)

	queries := db.New(pool.Pool)

	sessions := auth.NewSessionStore(redisClient, "session:", cfg.Session.TTL)
	learningService := learning.NewService(pool.Pool, queries)

	server := httpapi.NewServer(cfg, queries, sessions, learningService)

	httpServer := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           server.Router(),
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       120 * time.Second,
		// Bound the header map; a large one is a cheap memory-exhaustion
		// vector.
		MaxHeaderBytes: 1 << 20,
		ErrorLog:       slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", cfg.HTTP.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("listen and serve: %w", err)
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received", "timeout", cfg.HTTP.ShutdownTimeout.String())
	}

	// Drain in-flight requests before exiting so a learner mid-lesson is not
	// cut off. Requests past the deadline are abandoned.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		_ = httpServer.Close()
	}

	slog.Info("shutdown complete")
	return nil
}

func newRedisClient(cfg config.RedisConfig) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
}

func pingRedis(ctx context.Context, client *redis.Client) error {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := client.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("connect to redis at %s: %w", client.Options().Addr, err)
	}
	return nil
}
