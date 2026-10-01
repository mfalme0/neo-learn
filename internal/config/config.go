// Package config loads runtime configuration from the environment.
//
// Every setting has a development-friendly default so the API can start with
// nothing but a reachable Postgres and Redis. Production deployments are
// expected to override the secrets and the session cookie settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	HTTP     HTTPConfig
	LogLevel string

	DatabaseURL string

	Redis     RedisConfig
	Session   SessionConfig
	Argon2    Argon2Config
	WebOrigin string

	TrustedProxies []string
}

// HTTPConfig controls the API listener.
type HTTPConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// RedisConfig points at the Redis instance backing opaque sessions.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// SessionConfig describes the opaque session cookie and its lifetime.
type SessionConfig struct {
	CookieName   string
	TTL          time.Duration
	CookieSecure bool
	CookieDomain string
}

// Argon2Config holds the password hashing cost parameters. These are stored
// alongside each hash so existing credentials survive a parameter increase.
type Argon2Config struct {
	Time    uint32
	Memory  uint32
	Threads uint8
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	cfg := &Config{
		HTTP: HTTPConfig{
			Addr:            env("HTTP_ADDR", ":8080"),
			ReadTimeout:     envDuration("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			ShutdownTimeout: envDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
		},
		LogLevel: env("LOG_LEVEL", "info"),

		DatabaseURL: env("DATABASE_URL", "postgres://neolearn:neolearn@localhost:5432/neolearn?sslmode=disable"),

		Redis: RedisConfig{
			Addr:     env("REDIS_ADDR", "localhost:6379"),
			Password: env("REDIS_PASSWORD", ""),
			DB:       envInt("REDIS_DB", 0),
		},
		Session: SessionConfig{
			CookieName:   env("SESSION_COOKIE_NAME", "neo_session"),
			TTL:          envDuration("SESSION_TTL", 7*24*time.Hour),
			CookieSecure: envBool("SESSION_COOKIE_SECURE", false),
			CookieDomain: env("SESSION_COOKIE_DOMAIN", ""),
		},
		Argon2: Argon2Config{
			Time:    uint32(envInt("ARGON2_TIME", 3)),
			Memory:  uint32(envInt("ARGON2_MEMORY_KIB", 64*1024)),
			Threads: uint8(envInt("ARGON2_THREADS", 2)),
		},
		WebOrigin:      env("WEB_ORIGIN", "http://localhost:3000"),
		TrustedProxies: envList("TRUSTED_PROXIES"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var problems []error

	if c.DatabaseURL == "" {
		problems = append(problems, errors.New("config: DATABASE_URL is required"))
	}
	if c.Redis.Addr == "" {
		problems = append(problems, errors.New("config: REDIS_ADDR is required"))
	}
	if c.Session.CookieName == "" {
		problems = append(problems, errors.New("config: SESSION_COOKIE_NAME is required"))
	}
	if c.Session.TTL <= 0 {
		problems = append(problems, errors.New("config: SESSION_TTL must be positive"))
	}
	if c.Argon2.Time == 0 {
		problems = append(problems, errors.New("config: ARGON2_TIME must be at least 1"))
	}
	if c.Argon2.Memory < 8*1024 {
		problems = append(problems, errors.New("config: ARGON2_MEMORY_KIB must be at least 8192"))
	}
	if c.Argon2.Threads == 0 {
		problems = append(problems, errors.New("config: ARGON2_THREADS must be at least 1"))
	}

	return errors.Join(problems...)
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := env(key, "")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	raw := env(key, "")
	if raw == "" {
		return fallback
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return b
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := env(key, "")
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}

func envList(key string) []string {
	raw := env(key, "")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// String redacts secrets so Config can be logged at startup.
func (c *Config) String() string {
	return fmt.Sprintf(
		"Config{Addr:%s LogLevel:%s Redis:%s/%d Session:{Name:%s TTL:%s Secure:%t} Argon2:{Time:%d MemoryKiB:%d Threads:%d} WebOrigin:%s}",
		c.HTTP.Addr, c.LogLevel, c.Redis.Addr, c.Redis.DB,
		c.Session.CookieName, c.Session.TTL, c.Session.CookieSecure,
		c.Argon2.Time, c.Argon2.Memory, c.Argon2.Threads, c.WebOrigin,
	)
}
