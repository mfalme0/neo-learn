package httpapi_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/neolearn/neolearn/internal/testsupport"
)

// These tests cover the authentication boundary. The rule they encode: a
// request without a valid session must not learn anything about learner state,
// not even whether a course exists.

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	h := newHarness(t)
	anonymous := h.client(t)

	protected := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/me"},
		{http.MethodGet, "/v1/courses"},
		{http.MethodGet, "/v1/progress"},
		{http.MethodPost, "/v1/events"},
		{http.MethodPost, "/v1/auth/logout"},
		{http.MethodGet, "/v1/courses/1"},
		{http.MethodGet, "/v1/lessons/1"},
		{http.MethodGet, "/v1/assessments/1"},
	}

	for _, tc := range protected {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, h.Server.URL+tc.path, bytes.NewReader([]byte("{}")))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := anonymous.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// A garbage cookie must not be treated as a session.
func TestForgedSessionTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	client := h.client(t)

	req, err := http.NewRequest(http.MethodGet, h.Server.URL+"/v1/courses", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "neo_session_test", Value: "not-a-real-session-token"})

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Logging in then out must invalidate the session server-side. Clearing the
// cookie alone would leave a stolen copy of the token usable.
func TestLogoutInvalidatesTheSession(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	// The session works.
	resp, err := client.Get(h.Server.URL + "/v1/me")
	if err != nil {
		t.Fatalf("GET /v1/me: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me status = %d, want 200", resp.StatusCode)
	}

	resp, err = client.Post(h.Server.URL+"/v1/auth/logout", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}

	// The old token is dead. Replay it explicitly, in case the client kept a
	// copy despite the Set-Cookie clearing it.
	resp, err = client.Get(h.Server.URL + "/v1/me")
	if err != nil {
		t.Fatalf("GET /v1/me after logout: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status after logout = %d, want 401", resp.StatusCode)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	h := newHarness(t)
	client := h.client(t)

	resp, err := client.Post(h.Server.URL+"/v1/auth/login", "application/json", jsonBody(map[string]string{
		"institution": "kibera-secondary",
		"email":       "joseph.learner@example.test",
		"password":    "not-the-password",
	}))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// An unknown institution and a wrong password must be indistinguishable, or
// the endpoint becomes a way to enumerate which schools use the platform.
func TestLoginDoesNotDistinguishUnknownInstitutionFromWrongPassword(t *testing.T) {
	h := newHarness(t)

	unknown := loginError(t, h, "no-such-school", "joseph.learner@example.test", testsupportPassword())
	wrongPassword := loginError(t, h, "kibera-secondary", "joseph.learner@example.test", "wrong")
	unknownUser := loginError(t, h, "kibera-secondary", "nobody@example.test", testsupportPassword())

	if unknown.Code != wrongPassword.Code || unknown.Code != unknownUser.Code {
		t.Errorf("error codes differ: unknown institution=%q wrong password=%q unknown user=%q",
			unknown.Code, wrongPassword.Code, unknownUser.Code)
	}
	if unknown.Error != wrongPassword.Error || unknown.Error != unknownUser.Error {
		t.Errorf("error messages differ and leak which part was wrong:\n  %q\n  %q\n  %q",
			unknown.Error, wrongPassword.Error, unknownUser.Error)
	}
}

func TestLoginRejectsMissingFields(t *testing.T) {
	h := newHarness(t)
	client := h.client(t)

	resp, err := client.Post(h.Server.URL+"/v1/auth/login", "application/json", jsonBody(map[string]string{
		"institution": "",
		"email":       "",
	}))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// The session cookie must not be readable by JavaScript, or an XSS bug
// anywhere in the app becomes a full account takeover.
func TestSessionCookieIsHttpOnlyAndSameSite(t *testing.T) {
	h := newHarness(t)

	resp, err := h.client(t).Post(h.Server.URL+"/v1/auth/login", "application/json", jsonBody(map[string]string{
		"institution": "kibera-secondary",
		"email":       "joseph.learner@example.test",
		"password":    testsupportPassword(),
	}))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "neo_session_test" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("login did not set a session cookie")
	}
	if !session.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", session.SameSite)
	}
	if session.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", session.Path)
	}
}

func loginError(t *testing.T, h *harness, institution, email, password string) errorBody {
	t.Helper()

	resp, err := h.client(t).Post(h.Server.URL+"/v1/auth/login", "application/json", jsonBody(map[string]string{
		"institution": institution,
		"email":       email,
		"password":    password,
	}))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body errorBody
	decodeJSONBody(t, resp, &body)
	return body
}

func testsupportPassword() string { return testsupport.FixturePassword }
