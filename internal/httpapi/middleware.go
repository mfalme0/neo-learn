package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/neolearn/neolearn/internal/auth"
)

type contextKey string

const (
	// sessionContextKey holds the *auth.Session for an authenticated request.
	sessionContextKey contextKey = "neo.session"
)

// sessionIdentity is the narrowed identity view used by handlers. Keeping
// UserID on the session itself risks a handler reading the wrong field for
// the wrong tenant; this makes "who is acting" a single explicit value.
type sessionIdentity struct {
	UserID        int64
	InstitutionID int64
	Role          string
	DisplayName   string
}

// Identity returns the acting learner's id and institution.
//
// LearnerID is deliberately named for its use in learning queries rather
// than exposing UserID, which callers might apply to teacher-scoped queries.
func (i sessionIdentity) LearnerID() int64 { return i.UserID }

// sessionIdentityFromContext extracts the authenticated identity.
func sessionIdentityFromContext(ctx context.Context) (sessionIdentity, bool) {
	sess, ok := ctx.Value(sessionContextKey).(*auth.Session)
	if !ok {
		return sessionIdentity{}, false
	}
	return sessionIdentity{
		UserID:        sess.UserID,
		InstitutionID: sess.InstitutionID,
		Role:          sess.Role,
		DisplayName:   sess.DisplayName,
	}, true
}

// requireIdentity returns the identity for an authenticated route, writing 401
// itself if the session is missing. Handlers use this so an unauthenticated
// request cannot fall through to a nil-pointer dereference.
func requireIdentity(w http.ResponseWriter, r *http.Request) (sessionIdentity, bool) {
	identity, ok := sessionIdentityFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
		return sessionIdentity{}, false
	}
	return identity, true
}

// sessionFromContext returns the session attached by RequireSession.
// It reports false on unauthenticated requests; callers should have run the
// middleware first.
func sessionFromContext(ctx context.Context) (*auth.Session, bool) {
	sess, ok := ctx.Value(sessionContextKey).(*auth.Session)
	return sess, ok
}

// RequireSession authenticates a request via the session cookie.
//
// It deliberately does not accept a token in a header or query string: those
// get written to access logs and proxy logs. A transport that cannot hold a
// cookie (the SMS gateway, STK) is a separate authenticated path added in
// milestone 2, not a fallback branch here.
func RequireSession(sessions *auth.SessionStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(sessionCookieName)
			if err != nil || cookie.Value == "" {
				writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
				return
			}

			sess, err := sessions.Get(r.Context(), cookie.Value)
			if err != nil {
				if errors.Is(err, auth.ErrSessionNotFound) {
					// Clear the stale cookie so the browser stops sending it.
					expireSessionCookie(w)
					writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
					return
				}
				internalError(w, r, "session.get", err)
				return
			}

			ctx := context.WithValue(r.Context(), sessionContextKey, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sessionCookieName is the cookie RequireSession reads. It is a package
// variable rather than a constant so tests can point at an isolated name.
var sessionCookieName = "neo_session"

// SetSessionCookieName configures the cookie RequireSession reads.
func SetSessionCookieName(name string) {
	if name != "" {
		sessionCookieName = name
	}
}

func expireSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
}

// RequestLogger emits one structured line per request.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		slog.InfoContext(r.Context(), "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"bytes", wrapped.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.wroteHeader = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// AllowWebOrigin is a minimal CORS handler for the browser client.
//
// Credentials are allowed, so the allowed origin must be an explicit list
// rather than "*". Only the single configured web origin is permitted.
func AllowWebOrigin(origin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if origin != "" && r.Header.Get("Origin") == origin {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// securityHeaders sets conservative defaults for an authenticated API.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// RealIP extracts the client address, honouring X-Forwarded-For only when the
// immediate peer is a configured trusted proxy. Trusting the header
// unconditionally would let any caller spoof their address.
func RealIP(trustedProxies []string) func(*http.Request) string {
	trusted := make(map[string]bool, len(trustedProxies))
	for _, p := range trustedProxies {
		trusted[p] = true
	}

	return func(r *http.Request) string {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if !trusted[host] {
			return host
		}

		forwarded := r.Header.Get("X-Forwarded-For")
		if forwarded == "" {
			return host
		}
		parts := strings.Split(forwarded, ",")
		// The leftmost entry is the original client.
		if client := strings.TrimSpace(parts[0]); client != "" {
			return client
		}
		return host
	}
}

// chiParamInt64 reads a positive integer path parameter.
func chiParamInt64(r *http.Request, name string) (int64, error) {
	raw := chi.URLParam(r, name)
	value, err := parseInt64(raw)
	if err != nil || value <= 0 {
		return 0, &RequestError{
			Code:    "invalid_path_param",
			Message: "path parameter " + name + " must be a positive integer",
			Details: map[string]string{name: raw},
		}
	}
	return value, nil
}

func parseInt64(s string) (int64, error) {
	var v int64
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		v = v*10 + int64(c-'0')
		if v > 1<<62 {
			return 0, errors.New("overflow")
		}
	}
	return v, nil
}
