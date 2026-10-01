// Command api is the Neo Learn HTTP server and SMS delivery worker.
//
// Both run in one process. They consume the same learning engine; neither is
// allowed to write learner state directly.
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
	"github.com/neolearn/neolearn/internal/outbox"
	"github.com/neolearn/neolearn/internal/sms"
	"github.com/neolearn/neolearn/internal/store"
	"github.com/neolearn/neolearn/internal/worker"
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

	// Cancelled on SIGINT/SIGTERM so a shutdown propagates through the tree.
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

	// The SMS pipeline is optional. A deployment without messaging still serves
	// the web API in full; only the SMS routes and the drain worker are absent.
	serverOptions := httpapi.Options{TrustedProxies: cfg.TrustedProxies}

	if cfg.SMS.Worker.Enabled || cfg.SMS.InboundSecret != "" {
		gateway, err := buildGateway(cfg)
		if err != nil {
			return err
		}

		pipeline, err := worker.Assemble(worker.Deps{
			Pool:      pool.Pool,
			Queries:   queries,
			Learning:  learningService,
			Gateway:   gateway,
			Templates: sms.Templates{Brand: cfg.SMS.Brand},
			Logger:    slog.Default(),
			Config:    workerConfig(cfg),
		})
		if err != nil {
			return err
		}

		if cfg.SMS.Worker.Enabled {
			slog.Info("sms worker enabled",
				"gateway", gateway.Name(),
				"inbound_capable", sms.CapabilitiesOf(gateway).Inbound,
				"poll_interval", cfg.SMS.Worker.PollInterval)

			// A dedicated goroutine so the drain loop's ticker cannot delay an
			// inbound HTTP response.
			workerDone := make(chan struct{})
			go func() {
				defer close(workerDone)
				pipeline.Start(ctx)
			}()

			// The drain loop exits on its own when ctx is done; the wait below
			// is a no-op safety net that keeps the channel meaningful.
			go func() {
				<-ctx.Done()
				<-workerDone
			}()
		}

		if cfg.SMS.InboundSecret != "" {
			if !sms.CapabilitiesOf(gateway).Inbound {
				// Worth flagging: an inbound secret with a one-way gateway means
				// learners can be messaged but never reply, which changes what
				// the delivery scheduler is allowed to do.
				slog.Warn("inbound webhook enabled but the gateway cannot receive replies",
					"gateway", gateway.Name())
			}

			serverOptions.Inbound = pipeline.Inbound
			serverOptions.InboundAuth = httpapi.NewInboundAuth(cfg.SMS.InboundSecret)
			serverOptions.Acknowledge = pipeline.Acknowledge
			slog.Info("sms inbound webhook enabled", "path", "/v1/sms/inbound")
		} else {
			slog.Info("sms inbound disabled: SMS_INBOUND_SECRET is not set")
		}
	}

	server := httpapi.NewServer(cfg, queries, sessions, learningService, serverOptions)

	httpServer := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           server.Router(),
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       120 * time.Second,
		// Bound the header map; a large one is a cheap memory-exhaustion vector.
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

	// Drain in-flight requests so a learner mid-lesson is not cut off.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		_ = httpServer.Close()
	}

	slog.Info("shutdown complete")
	return nil
}

// buildGateway selects the outbound provider.
//
// Only the logging gateway exists today. Adding a real provider means
// implementing sms.Gateway -- nothing else in the pipeline changes, which is the
// property the transport abstraction is meant to guarantee.
func buildGateway(cfg *config.Config) (sms.Gateway, error) {
	switch cfg.SMS.Gateway {
	case "log", "":
		gateway := sms.NewLogSender(256)
		slog.Info("sms gateway: log",
			"note", "outbound messages are recorded to stdout, not delivered to any handset")
		return gateway, nil
	default:
		return nil, fmt.Errorf("config: unknown SMS_GATEWAY %q (supported: log)", cfg.SMS.Gateway)
	}
}

func workerConfig(cfg *config.Config) outbox.Config {
	return outbox.Config{
		PollInterval: cfg.SMS.Worker.PollInterval,
		BatchSize:    int32(cfg.SMS.Worker.BatchSize),
		MaxAttempts:  cfg.SMS.Worker.MaxAttempts,
		BaseBackoff:  cfg.SMS.Worker.BaseBackoff,
		MaxBackoff:   cfg.SMS.Worker.MaxBackoff,
		StaleClaim:   cfg.SMS.Worker.StaleClaim,
	}
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
