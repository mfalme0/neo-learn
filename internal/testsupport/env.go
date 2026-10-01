// Package testsupport provides the shared harness for integration tests.
//
// These tests exercise the real Postgres and Redis rather than fakes. The
// behaviour that matters -- the ON CONFLICT idempotency guarantee, the
// transaction boundary spanning the log, the projection, and the outbox -- is
// behaviour of the database. A mock would only test the mock.
package testsupport

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	dbfs "github.com/neolearn/neolearn/db"
)

// Config holds the connections a test needs.
type Config struct {
	Pool  *pgxpool.Pool
	Redis *redis.Client
	// DBName is the per-run scratch database. Tests truncate between cases.
	DBName string
}

// DefaultURL is used when DATABASE_URL is unset, matching the compose file.
const DefaultURL = "postgres://neolearn:neolearn@localhost:5433/neolearn?sslmode=disable"

var (
	sharedOnce sync.Once
	shared     *Config
	sharedErr  error
)

// Get returns the shared environment, or skips the test.
//
// Skipping rather than failing keeps `go test ./...` usable without Docker
// running, while still executing the full suite in CI where Postgres and Redis
// are present. Run `make up` first to exercise it locally.
func Get(t *testing.T) *Config {
	t.Helper()

	sharedOnce.Do(func() {
		shared, sharedErr = setup()
	})

	if sharedErr != nil {
		t.Skipf("integration dependencies unavailable: %v", sharedErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := shared.Pool.Ping(ctx); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	return shared
}

func setup() (*Config, error) {
	redisAddr := envOr("REDIS_ADDR", "localhost:6379")
	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := redisClient.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis at %s: %w", redisAddr, err)
	}

	adminURL, err := maintenanceURL(envOr("DATABASE_URL", DefaultURL))
	if err != nil {
		return nil, err
	}

	adminConn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}

	// A scratch database per run isolates fixtures from development data and
	// lets packages run in parallel.
	dbName := fmt.Sprintf("neolearn_test_%d", os.Getpid())
	if _, err := adminConn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		_ = adminConn.Close(ctx)
		return nil, fmt.Errorf("drop stale test database: %w", err)
	}
	if _, err := adminConn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		_ = adminConn.Close(ctx)
		return nil, fmt.Errorf("create test database: %w", err)
	}
	if err := adminConn.Close(ctx); err != nil {
		return nil, fmt.Errorf("close admin connection: %w", err)
	}

	pool, err := pgxpool.New(ctx, replaceDatabase(envOr("DATABASE_URL", DefaultURL), dbName))
	if err != nil {
		return nil, fmt.Errorf("connect to test database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping test database: %w", err)
	}
	if err := ApplySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &Config{Pool: pool, Redis: redisClient, DBName: dbName}, nil
}

// ApplySchema runs the embedded migrations in filename order.
func ApplySchema(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := fs.Glob(dbfs.Migrations, "migrations/*.up.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	for _, name := range entries {
		raw, err := dbfs.Migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(raw)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

// Truncate clears learner state between test cases.
//
// RESTART IDENTITY resets the sequences so ids are predictable across cases;
// otherwise the second test in a package would see course ids starting well
// above 1. CASCADE clears dependents, and TRUNCATE takes an ACCESS EXCLUSIVE
// lock, so this must not run while other cases hold a transaction.
func Truncate(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	_, err := pool.Exec(ctx, `
		TRUNCATE outbox, progress, learning_events, course_enrollments,
		         choices, questions, assessments, lessons, modules, courses,
		         sessions, users, institutions
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
}

// maintenanceURL rewrites a database URL to point at the default maintenance
// database, so a test database can be created.
func maintenanceURL(url string) (string, error) {
	base, query, hasQuery := strings.Cut(url, "?")
	idx := lastSlash(base)
	if idx < 0 {
		return "", fmt.Errorf("DATABASE_URL has no database path: %q", url)
	}

	out := base[:idx+1] + "postgres"
	if hasQuery {
		out += "?" + query
	}
	return out, nil
}

func replaceDatabase(url, name string) string {
	base, query, hasQuery := strings.Cut(url, "?")
	idx := lastSlash(base)
	if idx < 0 {
		return url
	}

	out := base[:idx+1] + name
	if hasQuery {
		out += "?" + query
	}
	return out
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
