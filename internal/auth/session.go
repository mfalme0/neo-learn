package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrSessionNotFound is returned when a token does not resolve to a live
// session. Callers map this to 401 without distinguishing expired, revoked,
// and never-existed: the distinction would leak which tokens are real.
var ErrSessionNotFound = errors.New("auth: session not found")

// Session is the server-side state behind an opaque session token.
type Session struct {
	UserID        int64
	InstitutionID int64
	Role          string
	Email         string
	DisplayName   string
	// TokenHash is the SHA-256 of the bearer token. The raw token is never
	// persisted.
	TokenHash string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// SessionStore persists sessions in Redis.
//
// The raw token is generated once, returned to the client in a cookie, and
// never stored: Redis holds only its SHA-256. A dump of Redis therefore does
// not yield usable session tokens.
type SessionStore struct {
	rdb    redis.UniversalClient
	prefix string
	ttl    time.Duration
}

// NewSessionStore builds a store over the given Redis client. ttl is the
// maximum session lifetime; individual sessions may expire sooner.
func NewSessionStore(rdb redis.UniversalClient, prefix string, ttl time.Duration) *SessionStore {
	if prefix == "" {
		prefix = "session:"
	}
	return &SessionStore{rdb: rdb, prefix: prefix, ttl: ttl}
}

// ttlFor derives the Redis key TTL. A non-positive result means the session
// has already expired and must not be stored.
func (s *SessionStore) ttlFor(sess Session) time.Duration {
	remaining := time.Until(sess.ExpiresAt)
	if remaining <= 0 {
		return -1
	}
	// Never let the Redis key outlive the recorded expiry, even if the two
	// were derived from different clocks.
	if remaining < s.ttl {
		return remaining
	}
	return s.ttl
}

func (s *SessionStore) key(tokenHash string) string {
	return s.prefix + tokenHash
}

// GenerateToken returns a fresh 256-bit token, URL-safe encoded, along with
// the SHA-256 hex digest used as its storage key.
func GenerateToken() (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: generate session token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashToken(raw), nil
}

// HashToken derives the storage key for a raw token.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Create persists a session and returns the raw bearer token. The caller
// stores that token in the cookie; it is unrecoverable afterwards.
func (s *SessionStore) Create(ctx context.Context, sess Session) (string, error) {
	ttl := s.ttlFor(sess)
	if ttl <= 0 {
		return "", fmt.Errorf("auth: refusing to create already-expired session")
	}

	raw, hash, err := GenerateToken()
	if err != nil {
		return "", err
	}
	sess.TokenHash = hash

	encoded, err := encodeSession(sess)
	if err != nil {
		return "", err
	}

	if err := s.rdb.Set(ctx, s.key(hash), encoded, ttl).Err(); err != nil {
		return "", fmt.Errorf("auth: store session: %w", err)
	}
	return raw, nil
}

// Get resolves a raw token to its session, refreshing the TTL so an active
// user's session does not expire mid-lesson.
func (s *SessionStore) Get(ctx context.Context, raw string) (Session, error) {
	if raw == "" {
		return Session{}, ErrSessionNotFound
	}

	hash := HashToken(raw)
	encoded, err := s.rdb.Get(ctx, s.key(hash)).Result()
	if errors.Is(err, redis.Nil) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("auth: read session: %w", err)
	}

	sess, err := decodeSession(encoded)
	if err != nil {
		return Session{}, err
	}

	// Guard against a Redis key that outlived its payload expiry, which can
	// happen if the clock moved backwards on restart.
	if time.Now().After(sess.ExpiresAt) {
		_ = s.Delete(ctx, raw)
		return Session{}, ErrSessionNotFound
	}

	// Sliding expiry: only extend within the configured maximum.
	if remaining := time.Until(sess.ExpiresAt); remaining > 0 && remaining < s.ttl {
		if err := s.rdb.Expire(ctx, s.key(hash), remaining).Err(); err != nil {
			// A failure to extend is not fatal; the session remains valid
			// until its recorded expiry.
			return sess, nil
		}
	}

	return sess, nil
}

// Delete revokes a single session.
func (s *SessionStore) Delete(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	if err := s.rdb.Del(ctx, s.key(HashToken(raw))).Err(); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}
