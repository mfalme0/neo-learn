package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neolearn/neolearn/internal/auth"
	"github.com/neolearn/neolearn/internal/config"
	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/httpapi"
	"github.com/neolearn/neolearn/internal/learning"
	"github.com/neolearn/neolearn/internal/testsupport"
)

// testServer wraps a running handler with a cookie-carrying client.
type testServer struct {
	*httptest.Server
}

// client returns an http.Client sharing one cookie jar, mirroring how a browser
// behaves across a multi-step flow.
func (s *testServer) client() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic("cookie jar: " + err.Error())
	}
	return &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// newTestServer starts a server for the duration of a test.
func newTestServer(t *testing.T, handler http.Handler) *testServer {
	t.Helper()

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &testServer{Server: ts}
}

// harness bundles a running API with the fixture it serves.
type harness struct {
	Server  *testServer
	Env     *testsupport.Config
	Fixture testsupport.Fixture
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	cfg := &config.Config{
		HTTP:     config.HTTPConfig{Addr: ":0"},
		LogLevel: "error",
		Redis:    config.RedisConfig{Addr: env.Redis.Options().Addr},
		Session: config.SessionConfig{
			CookieName: "neo_session_test",
			TTL:        time.Hour,
		},
		Argon2:    config.Argon2Config{Time: 1, Memory: 8 * 1024, Threads: 1},
		WebOrigin: "http://localhost:3000",
	}

	httpapi.SetSessionCookieName(cfg.Session.CookieName)

	queries := db.New(env.Pool)
	sessions := auth.NewSessionStore(env.Redis, "session_test:", cfg.Session.TTL)

	server := httpapi.NewServer(cfg, queries, sessions, learning.NewService(env.Pool, queries), httpapi.Options{})

	return &harness{Server: newTestServer(t, server.Router()), Env: env, Fixture: fx}
}

// client returns an http.Client carrying the session cookie across requests.
func (h *harness) client(t *testing.T) *http.Client {
	t.Helper()

	return h.Server.client()
}

func (h *harness) login(t *testing.T) *http.Client {
	t.Helper()

	client := h.client(t)
	body := map[string]string{
		"institution": "kibera-secondary",
		"email":       "joseph.learner@example.test",
		"password":    testsupport.FixturePassword,
	}

	resp, err := client.Post(h.Server.URL+"/v1/auth/login", "application/json", jsonBody(body))
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body: %s)", resp.StatusCode, readBody(t, resp))
	}

	for _, c := range resp.Cookies() {
		if c.Name == "neo_session_test" && c.Value == "" {
			t.Error("login returned an empty session cookie")
		}
	}
	return client
}

func jsonBody(v any) io.Reader {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return strings.NewReader(string(b))
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

func decodeJSONBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

type errorBody struct {
	Error   string            `json:"error"`
	Code    string            `json:"code"`
	Details map[string]string `json:"details"`
}
