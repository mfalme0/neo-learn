// Package store owns the Postgres connection pool.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps the Postgres connection pool and the configuration it was built
// from, so callers can reach either without threading two values around.
type Pool struct {
	*pgxpool.Pool
	maxConns int32
}

// Open connects to Postgres and verifies the connection before returning.
// Retries briefly, because during local development the database container is
// frequently still starting when the API comes up.
func Open(ctx context.Context, databaseURL string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}

	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: create pool: %w", err)
	}

	if err := pingWithRetry(ctx, pool, 5, time.Second); err != nil {
		pool.Close()
		return nil, err
	}

	return &Pool{Pool: pool, maxConns: cfg.MaxConns}, nil
}

func pingWithRetry(ctx context.Context, pool *pgxpool.Pool, attempts int, wait time.Duration) error {
	var lastErr error
	for i := range attempts {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("store: ping aborted: %w", ctx.Err())
			case <-time.After(wait):
			}
		}
	}
	return fmt.Errorf("store: ping after %d attempts: %w", attempts, lastErr)
}

// MaxConns reports the configured pool ceiling, for /healthz diagnostics.
func (p *Pool) MaxConns() int32 { return p.maxConns }
