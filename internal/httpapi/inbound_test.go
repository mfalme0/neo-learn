package httpapi_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/neolearn/neolearn/internal/auth"
	"github.com/neolearn/neolearn/internal/config"
	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/httpapi"
	"github.com/neolearn/neolearn/internal/inbound"
	"github.com/neolearn/neolearn/internal/learning"
	"github.com/neolearn/neolearn/internal/sms"
	"github.com/neolearn/neolearn/internal/testsupport"
)

// The inbound webhook is the only unauthenticated-by-design write path in the
// system: it cannot hold a session cookie, so it authenticates with a shared
// secret instead. These tests pin that boundary.

const testInboundSecret = "shared-secret-for-tests"

type inboundHarness struct {
	Server           *testServer
	Env              *testsupport.Config
	Fixture          testsupport.Fixture
	Acknowledgements []inbound.Acknowledgement
}

// newInboundHarness builds a server with the webhook mounted.
func newInboundHarness(t *testing.T) *inboundHarness {
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
	templates := sms.NewTemplates()
	delivery := sms.NewDelivery(queries, templates, nil)

	h := &inboundHarness{Env: env, Fixture: fx}

	processor := inbound.NewProcessor(
		queries,
		learning.NewService(env.Pool, queries),
		delivery,
		templates,
		nil,
	)

	server := httpapi.NewServer(cfg, queries, sessions, learning.NewService(env.Pool, queries), httpapi.Options{
		Inbound:     processor,
		InboundAuth: httpapi.NewInboundAuth(testInboundSecret),
		Acknowledge: func(ctx context.Context, ack inbound.Acknowledgement) error {
			h.Acknowledgements = append(h.Acknowledgements, ack)
			return nil
		},
	})

	h.Server = newTestServer(t, server.Router())
	return h
}

type inboundRequestBody struct {
	From       string `json:"from"`
	Body       string `json:"body"`
	MessageID  string `json:"message_id"`
	ReceivedAt string `json:"received_at"`
}

func (h *inboundHarness) post(t *testing.T, body inboundRequestBody, headers map[string]string) *http.Response {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, h.Server.URL+"/v1/sms/inbound", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := h.Server.client().Do(req)
	if err != nil {
		t.Fatalf("post inbound: %v", err)
	}
	return resp
}

// openQuestionConversation makes the learner look like they have a question
// pending, so a reply can be graded.
func (h *inboundHarness) openQuestionConversation(t *testing.T, questionID int64) {
	t.Helper()

	_, err := h.Env.Pool.Exec(context.Background(), `
		INSERT INTO sms_conversations (institution_id, learner_id, state,
		                               pending_assessment_id, pending_question_id, pending_position)
		VALUES ($1, $2, 'awaiting_answer', $3, $4, 0)`,
		h.Fixture.InstitutionID, h.Fixture.LearnerID, h.Fixture.AssessmentID, questionID)
	if err != nil {
		t.Fatalf("open conversation: %v", err)
	}
}

func TestInboundRequiresAuthentication(t *testing.T) {
	h := newInboundHarness(t)

	cases := map[string]map[string]string{
		"no credentials":       {},
		"wrong secret":         {"X-Neo-Auth": "not-the-secret"},
		"empty header":         {"X-Neo-Auth": ""},
		"signature with no ts": {"X-Neo-Signature": "deadbeef"},
	}

	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			resp := h.post(t, inboundRequestBody{
				From:      h.Fixture.LearnerMSISDN,
				Body:      "A",
				MessageID: "gw-" + name,
			}, headers)
			defer func() { _ = resp.Body.Close() }()

			// A non-2xx tells the gateway to retry, which is correct for bad
			// credentials: they may be misconfigured rather than malicious.
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

func TestInboundAcceptsTheSharedSecret(t *testing.T) {
	h := newInboundHarness(t)
	h.openQuestionConversation(t, h.Fixture.QuestionIDs[0])

	resp := h.post(t, inboundRequestBody{
		From:      h.Fixture.LearnerMSISDN,
		Body:      h.Fixture.CorrectLabels[0],
		MessageID: "gw-authenticated-1",
	}, map[string]string{"X-Neo-Auth": testInboundSecret})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, readBody(t, resp))
	}
}

func TestInboundAcceptsAnHmacSignature(t *testing.T) {
	h := newInboundHarness(t)
	h.openQuestionConversation(t, h.Fixture.QuestionIDs[0])

	body := inboundRequestBody{
		From:      h.Fixture.LearnerMSISDN,
		Body:      h.Fixture.CorrectLabels[0],
		MessageID: "gw-signed-1",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testInboundSecret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(raw)

	resp := h.post(t, body, map[string]string{
		"X-Neo-Signature": hex.EncodeToString(mac.Sum(nil)),
		"X-Neo-Timestamp": timestamp,
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", resp.StatusCode, readBody(t, resp))
	}
}

// A signature is only replay-resistant if an old one is refused.
func TestReplayOfASignedRequestIsRefused(t *testing.T) {
	h := newInboundHarness(t)

	body := inboundRequestBody{
		From:      h.Fixture.LearnerMSISDN,
		Body:      "STOP",
		MessageID: "gw-replay-1",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// A timestamp well outside the accepted skew window.
	timestamp := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testInboundSecret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(raw)

	resp := h.post(t, body, map[string]string{
		"X-Neo-Signature": hex.EncodeToString(mac.Sum(nil)),
		"X-Neo-Timestamp": timestamp,
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d for a stale signature, want 401", resp.StatusCode)
	}
}

// A tampered body must invalidate the signature.
func TestTamperedBodyInvalidatesTheSignature(t *testing.T) {
	h := newInboundHarness(t)

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testInboundSecret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write([]byte(`{"from":"+254700000001","body":"A","message_id":"gw-1"}`))

	// A different body than the one that was signed.
	resp := h.post(t, inboundRequestBody{
		From:      h.Fixture.LearnerMSISDN,
		Body:      "STOP",
		MessageID: "gw-tampered-1",
	}, map[string]string{
		"X-Neo-Signature": hex.EncodeToString(mac.Sum(nil)),
		"X-Neo-Timestamp": timestamp,
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d for a tampered body, want 401", resp.StatusCode)
	}
}

// Without a message id there is no way to deduplicate a retry, and silently
// accepting one would let a duplicated delivery score a learner twice.
func TestInboundRequiresAProviderMessageID(t *testing.T) {
	h := newInboundHarness(t)
	h.openQuestionConversation(t, h.Fixture.QuestionIDs[0])

	resp := h.post(t, inboundRequestBody{
		From: h.Fixture.LearnerMSISDN,
		Body: h.Fixture.CorrectLabels[0],
	}, map[string]string{"X-Neo-Auth": testInboundSecret})
	defer func() { _ = resp.Body.Close() }()

	// Acknowledged rather than retried: the same message will always lack an id.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (acknowledged, not retried)", resp.StatusCode)
	}

	var events int
	if err := h.Env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM learning_events WHERE question_id = $1`,
		h.Fixture.QuestionIDs[0]).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("learning_events has %d rows without a message id, want 0", events)
	}
}

// The webhook must not accept a session cookie in place of the secret.
func TestInboundDoesNotAcceptASessionCookie(t *testing.T) {
	h := newInboundHarness(t)

	client := h.Server.client()

	raw, err := json.Marshal(inboundRequestBody{
		From:      h.Fixture.LearnerMSISDN,
		Body:      "STOP",
		MessageID: "gw-cookie-1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, h.Server.URL+"/v1/sms/inbound", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// A valid-looking cookie, but no shared secret.
	req.AddCookie(&http.Cookie{Name: "neo_session_test", Value: testInboundSecret})

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post inbound: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAcknowledgementIsQueuedForDelivery(t *testing.T) {
	h := newInboundHarness(t)
	h.openQuestionConversation(t, h.Fixture.QuestionIDs[0])

	resp := h.post(t, inboundRequestBody{
		From:      h.Fixture.LearnerMSISDN,
		Body:      h.Fixture.CorrectLabels[0],
		MessageID: "gw-ack-1",
	}, map[string]string{"X-Neo-Auth": testInboundSecret})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The reply goes through the queue so a gateway outage while handling the
	// message cannot cost the learner their acknowledgement.
	if len(h.Acknowledgements) != 1 {
		t.Fatalf("got %d acknowledgements, want 1", len(h.Acknowledgements))
	}
	if h.Acknowledgements[0].To != h.Fixture.LearnerMSISDN {
		t.Errorf("acknowledgement To = %q, want %q",
			h.Acknowledgements[0].To, h.Fixture.LearnerMSISDN)
	}
	if h.Acknowledgements[0].Body == "" {
		t.Error("acknowledgement body is empty")
	}
}

// A retried webhook must not produce a second acknowledgement, which would
// cost the learner money.
func TestDuplicateWebhookQueuesNoSecondAcknowledgement(t *testing.T) {
	h := newInboundHarness(t)
	h.openQuestionConversation(t, h.Fixture.QuestionIDs[0])

	body := inboundRequestBody{
		From:      h.Fixture.LearnerMSISDN,
		Body:      h.Fixture.CorrectLabels[0],
		MessageID: "gw-ack-dup",
	}
	headers := map[string]string{"X-Neo-Auth": testInboundSecret}

	first := h.post(t, body, headers)
	_ = first.Body.Close()

	second := h.post(t, body, headers)
	_ = second.Body.Close()

	if len(h.Acknowledgements) != 1 {
		t.Errorf("got %d acknowledgements for a duplicated webhook, want 1", len(h.Acknowledgements))
	}
}

// An unknown sender is acknowledged rather than retried: the same message will
// always be unknown.
func TestUnknownSenderIsAcknowledged(t *testing.T) {
	h := newInboundHarness(t)

	resp := h.post(t, inboundRequestBody{
		From:      "+254700000999",
		Body:      "A",
		MessageID: "gw-stranger-1",
	}, map[string]string{"X-Neo-Auth": testInboundSecret})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (acknowledged, not retried)", resp.StatusCode)
	}
}

// With no secret configured the route must not exist at all, rather than exist
// and reject every request.
func TestWebhookIsAbsentWithoutASecret(t *testing.T) {
	env := testsupport.Get(t)
	testsupport.NewFixture(context.Background(), t, env.Pool)

	cfg := &config.Config{
		HTTP:     config.HTTPConfig{Addr: ":0"},
		LogLevel: "error",
		Redis:    config.RedisConfig{Addr: env.Redis.Options().Addr},
		Session:  config.SessionConfig{CookieName: "neo_session_test", TTL: time.Hour},
		Argon2:   config.Argon2Config{Time: 1, Memory: 8 * 1024, Threads: 1},
	}

	queries := db.New(env.Pool)
	sessions := auth.NewSessionStore(env.Redis, "session_test:", cfg.Session.TTL)
	server := httpapi.NewServer(cfg, queries, sessions, learning.NewService(env.Pool, queries), httpapi.Options{})
	ts := newTestServer(t, server.Router())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/sms/inbound", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := ts.client().Do(req)
	if err != nil {
		t.Fatalf("post inbound: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d with no secret configured, want 404 (route absent)", resp.StatusCode)
	}
}
