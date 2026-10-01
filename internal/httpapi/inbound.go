package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/neolearn/neolearn/internal/inbound"
	"github.com/neolearn/neolearn/internal/sms"
)

// Inbound auth header names.
//
// Gateways disagree on these; the names are configurable so a provider
// integration does not require a code change.
const (
	defaultInboundAuthHeader   = "X-Neo-Auth"
	defaultInboundSignatureHdr = "X-Neo-Signature"
	defaultInboundFromHeader   = "X-Neo-From"
	defaultInboundSenderIDHdr  = "X-Neo-Sender-Id"
	defaultInboundMessageIDHdr = "X-Neo-Message-Id"
	defaultInboundTimestampHdr = "X-Neo-Timestamp"
)

// inboundRequest is the webhook body.
//
// Only the body text is required. The sender number, message id, and timestamp
// come from headers where the provider supplies them, because those are the
// values that must be trusted: a body field can be tampered with in transit,
// whereas a header covered by the signature cannot.
type inboundRequest struct {
	From              string `json:"from"`
	Body              string `json:"body"`
	ProviderMessageID string `json:"message_id"`
	ReceivedAt        string `json:"received_at"`
}

// InboundAuth validates an inbound webhook.
//
// Two modes, because gateways differ:
//
//   - Shared secret in a header. Simple, and adequate when the transport is
//     TLS-terminated and the header is not logged.
//   - HMAC over the raw body plus a timestamp. Stronger, and resistant to
//     replay, which matters because a replayed webhook would otherwise re-answer
//     a question -- though the derived event id already makes that harmless.
type InboundAuth struct {
	Secret          string
	Header          string
	SignatureHeader string
	FromHeader      string
	SenderIDHeader  string
	MessageIDHeader string
	TimestampHeader string
	// MaxClockSkew bounds how old a signed request may be. Rejecting an old
	// timestamp is what makes the signature scheme replay-resistant.
	MaxClockSkew time.Duration
}

// NewInboundAuth builds an authenticator from the header names.
func NewInboundAuth(secret string) *InboundAuth {
	return &InboundAuth{
		Secret:          secret,
		Header:          defaultInboundAuthHeader,
		SignatureHeader: defaultInboundSignatureHdr,
		FromHeader:      defaultInboundFromHeader,
		SenderIDHeader:  defaultInboundSenderIDHdr,
		MessageIDHeader: defaultInboundMessageIDHdr,
		TimestampHeader: defaultInboundTimestampHdr,
		MaxClockSkew:    5 * time.Minute,
	}
}

// enabled reports whether inbound delivery is configured.
//
// With no secret the route is not mounted at all, so an unconfigured deployment
// exposes no unauthenticated write path.
func (a *InboundAuth) enabled() bool { return a.Secret != "" }

// authenticate checks a request's credentials.
func (a *InboundAuth) authenticate(r *http.Request, body []byte) bool {
	if !a.enabled() {
		return false
	}

	// Signed form, preferred when a signature is present.
	if signature := r.Header.Get(a.SignatureHeader); signature != "" {
		return a.verifySignature(r, body, signature)
	}

	provided := r.Header.Get(a.Header)
	if provided == "" {
		return false
	}

	return hmac.Equal([]byte(provided), []byte(a.Secret))
}

func (a *InboundAuth) verifySignature(r *http.Request, body []byte, signature string) bool {
	timestamp := r.Header.Get(a.TimestampHeader)
	if timestamp == "" {
		return false
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}

	sent := time.Unix(seconds, 0)
	// A stale signature is a replay, and a replay is the one thing this scheme
	// exists to prevent.
	if drift := time.Since(sent); drift > a.MaxClockSkew || drift < -a.MaxClockSkew {
		return false
	}

	mac := hmac.New(sha256.New, []byte(a.Secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)

	expected := hex.EncodeToString(mac.Sum(nil))
	// Constant-time compare: a timing side channel here would let an attacker
	// recover the signature byte by byte.
	return subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) == 1
}

// inboundWebhook handles messages delivered by the SMS provider.
//
// It always answers 2xx for anything it has understood, including messages it
// chose to ignore. A non-2xx tells the gateway to retry, and retrying a message
// that was never going to succeed just burns quota.
func (s *Server) handleInboundSMS(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "could not read the request body")
		return
	}

	if !s.inboundAuth.authenticate(r, raw) {
		// Not 2xx: an unauthenticated request should be retried only if the
		// credentials were wrong, which is worth surfacing.
		slog.Warn("inbound sms rejected: authentication failed",
			"remote", s.realIP(r), "path", r.URL.Path)
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid webhook credentials")
		return
	}

	// The body was already read, so it is decoded directly rather than through
	// decodeJSON, which would try to read r.Body again.
	var req inboundRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
			return
		}
	}

	msg, err := s.buildInboundMessage(r, req)
	if err != nil {
		// A message we cannot identify is acknowledged rather than retried.
		slog.Warn("inbound sms is not usable", "error", err, "remote", s.realIP(r))
		writeJSON(w, http.StatusOK, inboundResponse{Outcome: "unusable", Handled: false})
		return
	}

	result, err := s.inbound.Handle(r.Context(), msg)
	if err != nil {
		// A genuine internal failure is a 5xx so the gateway retries: unlike an
		// unusable message, this one may well succeed later.
		internalError(w, r, "inbound.handle", err)
		return
	}

	if result.Reply != "" {
		// Fire-and-forget: the acknowledgement is written through the outbox so
		// it retries if the gateway is unavailable. The webhook returns before
		// delivery is attempted, which keeps the gateway's timeout satisfied.
		if err := s.enqueueAcknowledgement(r, msg, result.Reply); err != nil {
			slog.Error("could not enqueue inbound acknowledgement",
				"error", err, "from", msg.From)
		}
	}

	slog.Info("inbound sms handled",
		"outcome", result.Outcome,
		"duplicate", result.Duplicate,
		"from", sms.StripToASCII(msg.From))

	writeJSON(w, http.StatusOK, inboundResponse{
		Outcome: string(result.Outcome),
		Handled: result.Handled,
	})
}

type inboundResponse struct {
	Outcome string `json:"outcome"`
	Handled bool   `json:"handled"`
}

// buildInboundMessage assembles the inbound message from headers and body.
//
// A missing provider message id is a hard error: without it there is no way to
// deduplicate a gateway retry, and silently accepting one would let a duplicated
// delivery score a learner twice.
func (s *Server) buildInboundMessage(r *http.Request, req inboundRequest) (sms.InboundMessage, error) {
	from := firstNonEmpty(
		r.Header.Get(s.inboundAuth.FromHeader),
		req.From,
	)
	if from == "" {
		return sms.InboundMessage{}, errors.New("inbound: no sender number in header or body")
	}

	normalized, err := sms.NormalizeE164(from)
	if err != nil {
		return sms.InboundMessage{}, err
	}

	messageID := firstNonEmpty(
		r.Header.Get(s.inboundAuth.MessageIDHeader),
		req.ProviderMessageID,
	)
	if messageID == "" {
		return sms.InboundMessage{}, errors.New("inbound: no provider message id; retries could not be deduplicated")
	}

	receivedAt := time.Now().UTC()
	if raw := req.ReceivedAt; raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			receivedAt = parsed.UTC()
		}
	}

	return sms.InboundMessage{
		From:              normalized,
		Body:              req.Body,
		ProviderMessageID: messageID,
		ReceivedAt:        receivedAt,
	}, nil
}

// enqueueAcknowledgement writes the reply to the outbox.
//
// The reply goes through the queue rather than being sent inline so a gateway
// outage while handling a reply cannot cost the learner their acknowledgement.
func (s *Server) enqueueAcknowledgement(r *http.Request, msg sms.InboundMessage, reply string) error {
	if s.acknowledge == nil {
		return nil
	}
	return s.acknowledge(r.Context(), inbound.Acknowledgement{
		To:      msg.From,
		Body:    reply,
		ReplyTo: msg.ProviderMessageID,
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
