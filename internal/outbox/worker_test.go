package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/outbox"
	"github.com/neolearn/neolearn/internal/sms"
	"github.com/neolearn/neolearn/internal/testsupport"
)

// These tests exercise the retry and deduplication guarantees of the drain
// worker against a real database: claiming with FOR UPDATE SKIP LOCKED, the
// give-up threshold, and backoff scheduling are all database behaviour.

// recordingSender captures what the worker tried to send and can be made to
// fail a fixed number of times.
type recordingSender struct {
	mu      sync.Mutex
	sent    []smsOutbound
	failFor int
	failErr error
}

type smsOutbound struct {
	To        string
	Body      string
	Reference string
}

func (s *recordingSender) SendText(ctx context.Context, to, body, reference string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failFor > 0 {
		s.failFor--
		return s.failErr
	}
	s.sent = append(s.sent, smsOutbound{To: to, Body: body, Reference: reference})
	return nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *recordingSender) last() smsOutbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		return smsOutbound{}
	}
	return s.sent[len(s.sent)-1]
}

// fixedRenderer returns a constant body and reports whether it was consulted.
type fixedRenderer struct {
	body  string
	err   error
	calls int
}

func (r *fixedRenderer) Render(ctx context.Context, msg outbox.Message) (string, error) {
	r.calls++
	if r.err != nil {
		return "", r.err
	}
	return r.body, nil
}

type staticRecipient struct {
	msisdn string
	err    error
	calls  int
}

func (r *staticRecipient) Resolve(ctx context.Context, institutionID, learnerID int64) (outbox.Recipient, error) {
	r.calls++
	if r.err != nil {
		return outbox.Recipient{}, r.err
	}
	return outbox.Recipient{MSISDN: r.msisdn}, nil
}

// fastConfig keeps the tests quick: the backoff schedule is asserted directly on
// Config.BackoffFor, so integration tests do not need to wait it out.
func fastConfig() outbox.Config {
	return outbox.Config{
		PollInterval: 10 * time.Millisecond,
		BatchSize:    25,
		MaxAttempts:  3,
		BaseBackoff:  time.Millisecond,
		MaxBackoff:   5 * time.Millisecond,
		StaleClaim:   time.Second,
	}
}

// seedOutbox writes one notification row.
func seedOutbox(t *testing.T, env *testsupport.Config, fx testsupport.Fixture, topic string) string {
	t.Helper()

	eventID := uuid.New()
	ctx := context.Background()

	_, err := env.Pool.Exec(ctx, `
		INSERT INTO learning_events (event_id, institution_id, learner_id, course_id, lesson_id,
		                             kind, source, payload, occurred_at)
		VALUES ($1, $2, $3, $4, $5, 'lesson_started', 'web', '{}'::jsonb, now())`,
		eventID, fx.InstitutionID, fx.LearnerID, fx.CourseID, fx.LessonID)
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}

	if _, err := env.Pool.Exec(ctx, `
		INSERT INTO outbox (institution_id, event_id, topic, payload, destination_msisdn)
		VALUES ($1, $2, $3, $4, $5)`,
		fx.InstitutionID, eventID, topic,
		fmt.Sprintf(`{"kind":"%s","source":"sms","learner_id":%d}`, topic, fx.LearnerID),
		fx.LearnerMSISDN); err != nil {
		t.Fatalf("seed outbox row: %v", err)
	}

	return eventID.String()
}

func newWorker(
	t *testing.T,
	env *testsupport.Config,
	sender outbox.Sender,
	renderer outbox.Renderer,
	recipient outbox.RecipientLookup,
) *outbox.Worker {
	t.Helper()

	w, err := outbox.NewWorker(env.Pool, db.New(env.Pool), sender, renderer, recipient, fastConfig(), nil)
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}
	return w
}

func TestDrainOnceDeliversAndMarksDelivered(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	sender := &recordingSender{}
	renderer := &fixedRenderer{body: "NEO LEARN\n\nToday's lesson: Fractions"}
	recipient := &staticRecipient{msisdn: fx.LearnerMSISDN}

	w := newWorker(t, env, sender, renderer, recipient)

	delivered, err := w.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce() error = %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}
	if sender.count() != 1 {
		t.Errorf("sender called %d times, want 1", sender.count())
	}
	if sender.last().To != fx.LearnerMSISDN {
		t.Errorf("sent to %q, want %q", sender.last().To, fx.LearnerMSISDN)
	}

	var deliveredAt *time.Time
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT delivered_at FROM outbox WHERE topic = $1`, "lesson_started").Scan(&deliveredAt); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if deliveredAt == nil {
		t.Error("delivered_at is NULL after a successful send")
	}
}

// A delivered row must never be picked up again, or every learner is billed for
// the same message on every pass.
func TestDeliveredRowIsNotClaimedTwice(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	sender := &recordingSender{}
	renderer := &fixedRenderer{body: "hello"}
	w := newWorker(t, env, sender, renderer, &staticRecipient{msisdn: fx.LearnerMSISDN})

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("first DrainOnce() error = %v", err)
	}
	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("second DrainOnce() error = %v", err)
	}

	if sender.count() != 1 {
		t.Errorf("sender called %d times across two drains, want 1", sender.count())
	}
}

// A transient failure must be retried, not abandoned on the first error.
func TestFailedSendIsRetriedLater(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	sender := &recordingSender{
		failFor: 1,
		failErr: sms.ErrGatewayUnavailable,
	}
	renderer := &fixedRenderer{body: "hello"}

	// A backoff long enough to distinguish "scheduled in the future" from
	// "immediately due"; fastConfig uses 1ms, which is inside the noise of the
	// round trip.
	cfg := fastConfig()
	cfg.BaseBackoff = 200 * time.Millisecond
	cfg.MaxBackoff = time.Second

	w, err := outbox.NewWorker(env.Pool, db.New(env.Pool), sender, renderer,
		&staticRecipient{msisdn: fx.LearnerMSISDN}, cfg, nil)
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}

	drainStart := time.Now()
	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("first DrainOnce() error = %v", err)
	}

	// The row is recorded as failed but not yet due.
	var lastError *string
	var nextAttempt time.Time
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT last_error, next_attempt_at FROM outbox WHERE topic = $1`,
		"lesson_started").Scan(&lastError, &nextAttempt); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if lastError == nil {
		t.Error("last_error is NULL after a failed send")
	}
	if nextAttempt.Before(drainStart.Add(100 * time.Millisecond)) {
		t.Errorf("next_attempt_at = %v, want at least the backoff after %v", nextAttempt, drainStart)
	}

	// The message is not retried while it is still within its backoff window.
	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("early DrainOnce() error = %v", err)
	}
	if sender.count() != 0 {
		t.Errorf("sender called %d times inside the backoff window, want 0", sender.count())
	}

	// Wait out the backoff and the retry succeeds.
	time.Sleep(250 * time.Millisecond)
	delivered, err := w.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("second DrainOnce() error = %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d on the retry, want 1", delivered)
	}
}

// A message that keeps failing must stop costing money and become visible.
func TestMessageIsAbandonedAfterMaxAttempts(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	sender := &recordingSender{
		failFor: 10,
		failErr: sms.ErrGatewayUnavailable,
	}
	renderer := &fixedRenderer{body: "hello"}
	w := newWorker(t, env, sender, renderer, &staticRecipient{msisdn: fx.LearnerMSISDN})

	ctx := context.Background()

	// MaxAttempts is 3 in fastConfig.
	for range 5 {
		if _, err := w.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce() error = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var deadAt *time.Time
	var attempts int
	if err := env.Pool.QueryRow(ctx,
		`SELECT dead_at, attempts FROM outbox WHERE topic = $1`,
		"lesson_started").Scan(&deadAt, &attempts); err != nil {
		t.Fatalf("read outbox: %v", err)
	}

	if deadAt == nil {
		t.Errorf("dead_at is NULL after %d failed attempts; the message would be retried forever", attempts)
	}
	if attempts > 4 {
		t.Errorf("attempts = %d, want it capped near MaxAttempts", attempts)
	}
}

// A provider rejection is terminal: retrying a refused number burns money and, for
// an opted-out learner, keeps texting somebody who asked to stop.
func TestRejectedMessageIsNotRetried(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	sender := &recordingSender{
		failFor: 10,
		failErr: &sms.RejectedError{To: fx.LearnerMSISDN, Status: sms.StatusRejected, Reason: "invalid number"},
	}
	renderer := &fixedRenderer{body: "hello"}
	w := newWorker(t, env, sender, renderer, &staticRecipient{msisdn: fx.LearnerMSISDN})

	ctx := context.Background()
	if _, err := w.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce() error = %v", err)
	}

	// A single attempt is enough; the row is dead immediately.
	var deadAt *time.Time
	var attempts int
	if err := env.Pool.QueryRow(ctx,
		`SELECT dead_at, attempts FROM outbox WHERE topic = $1`,
		"lesson_started").Scan(&deadAt, &attempts); err != nil {
		t.Fatalf("read outbox: %v", err)
	}

	if deadAt == nil {
		t.Error("a rejected message was not marked dead; it would be retried against a refused number")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 for a rejection", attempts)
	}
}

// A message that cannot be rendered will never become renderable, so retrying
// it wastes money forever.
func TestUnrenderableMessageIsAbandoned(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	sender := &recordingSender{}
	renderer := &fixedRenderer{err: errors.New("question has no choices")}
	w := newWorker(t, env, sender, renderer, &staticRecipient{msisdn: fx.LearnerMSISDN})

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce() error = %v", err)
	}

	if sender.count() != 0 {
		t.Errorf("sender called %d times for an unrenderable message, want 0", sender.count())
	}

	var deadAt *time.Time
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT dead_at FROM outbox WHERE topic = $1`,
		"lesson_started").Scan(&deadAt); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if deadAt == nil {
		t.Error("an unrenderable message was not marked dead")
	}
}

// A message for a learner with no number cannot be delivered and must not block
// the queue behind it.
func TestRowWithoutDestinationIsSkipped(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	// Insert with no destination, as happens when a learner's number has not
	// been collected yet.
	eventID := uuid.New()
	ctx := context.Background()
	if _, err := env.Pool.Exec(ctx, `
		INSERT INTO learning_events (event_id, institution_id, learner_id, course_id, lesson_id,
		                             kind, source, payload, occurred_at)
		VALUES ($1, $2, $3, $4, $5, 'lesson_started', 'web', '{}'::jsonb, now())`,
		eventID, fx.InstitutionID, fx.LearnerID, fx.CourseID, fx.LessonID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := env.Pool.Exec(ctx, `
		INSERT INTO outbox (institution_id, event_id, topic, payload, destination_msisdn)
		VALUES ($1, $2, 'lesson_started', $3, NULL)`,
		fx.InstitutionID, eventID, `{"kind":"lesson_started","learner_id":1}`); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	sender := &recordingSender{}
	renderer := &fixedRenderer{body: "hello"}
	w := newWorker(t, env, sender, renderer, &staticRecipient{msisdn: fx.LearnerMSISDN})

	if _, err := w.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce() error = %v", err)
	}
	if sender.count() != 0 {
		t.Errorf("sender called %d times for a row with no destination, want 0", sender.count())
	}
}

// The recipient lookup is only consulted when the stored destination is empty.
func TestRecipientLookupIsUsedOnlyAsFallback(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	sender := &recordingSender{}
	recipient := &staticRecipient{msisdn: "+254700000099"}
	w := newWorker(t, env, sender, &fixedRenderer{body: "hello"}, recipient)

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce() error = %v", err)
	}

	if recipient.calls != 0 {
		t.Errorf("recipient lookup called %d times despite a stored destination, want 0", recipient.calls)
	}
}

func TestStatsReportsPendingAndDead(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	w := newWorker(t, env, &recordingSender{}, &fixedRenderer{body: "x"}, &staticRecipient{})

	stats, err := w.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Pending != 1 {
		t.Errorf("Pending = %d, want 1", stats.Pending)
	}
	if stats.Dead != 0 {
		t.Errorf("Dead = %d, want 0", stats.Dead)
	}
}

// ---------------------------------------------------------------------------
// Backoff policy
// ---------------------------------------------------------------------------

func TestBackoffGrowsExponentiallyAndIsCapped(t *testing.T) {
	t.Parallel()

	cfg := outbox.Config{BaseBackoff: 30 * time.Second, MaxBackoff: 30 * time.Minute}

	cases := []struct {
		attempts int32
		want     time.Duration
	}{
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, 16 * time.Minute},
		// Capped rather than continuing to 32 minutes: a message that failed
		// five times because the provider is down must come back when it
		// recovers, not tomorrow.
		{7, 30 * time.Minute},
		{50, 30 * time.Minute},
	}

	for _, tc := range cases {
		if got := cfg.BackoffFor(tc.attempts); got != tc.want {
			t.Errorf("BackoffFor(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

// A huge attempt count must not overflow the duration computation.
func TestBackoffDoesNotOverflow(t *testing.T) {
	t.Parallel()

	cfg := outbox.Config{BaseBackoff: time.Second, MaxBackoff: time.Hour}

	for _, attempts := range []int32{0, 1, 62, 63, 64, 1000} {
		got := cfg.BackoffFor(attempts)
		if got < 0 {
			t.Errorf("BackoffFor(%d) = %v, want a non-negative duration", attempts, got)
		}
		if got > cfg.MaxBackoff {
			t.Errorf("BackoffFor(%d) = %v, over the cap %v", attempts, got, cfg.MaxBackoff)
		}
	}
}

func TestIsDeadAtThreshold(t *testing.T) {
	t.Parallel()

	cfg := outbox.Config{MaxAttempts: 3}

	if cfg.IsDead(1) || cfg.IsDead(2) {
		t.Error("a message below the threshold is reported dead")
	}
	if !cfg.IsDead(3) {
		t.Error("a message at the threshold is not reported dead")
	}
	if !cfg.IsDead(4) {
		t.Error("a message past the threshold is not reported dead")
	}
}

// ---------------------------------------------------------------------------
// Configuration validation
// ---------------------------------------------------------------------------

func TestNewWorkerRejectsUnusableConfig(t *testing.T) {
	t.Parallel()

	base := outbox.DefaultConfig()

	cases := map[string]outbox.Config{
		"zero poll interval": {PollInterval: 0, BatchSize: 1, MaxAttempts: 1, BaseBackoff: time.Second, MaxBackoff: time.Minute, StaleClaim: time.Second},
		"zero batch":         {PollInterval: time.Second, BatchSize: 0, MaxAttempts: 1, BaseBackoff: time.Second, MaxBackoff: time.Minute, StaleClaim: time.Second},
		"zero attempts":      {PollInterval: time.Second, BatchSize: 1, MaxAttempts: 0, BaseBackoff: time.Second, MaxBackoff: time.Minute, StaleClaim: time.Second},
		"zero backoff":       {PollInterval: time.Second, BatchSize: 1, MaxAttempts: 1, BaseBackoff: 0, MaxBackoff: time.Minute, StaleClaim: time.Second},
		"cap below floor":    {PollInterval: time.Second, BatchSize: 1, MaxAttempts: 1, BaseBackoff: time.Hour, MaxBackoff: time.Minute, StaleClaim: time.Second},
		"zero stale claim":   {PollInterval: time.Second, BatchSize: 1, MaxAttempts: 1, BaseBackoff: time.Second, MaxBackoff: time.Minute, StaleClaim: 0},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := outbox.NewWorker(nil, nil, &recordingSender{}, &fixedRenderer{}, &staticRecipient{}, cfg, nil); err == nil {
				t.Error("NewWorker() error = nil for an unusable config, want an error")
			}
		})
	}

	// The default config must be usable, or the app cannot start.
	if _, err := outbox.NewWorker(nil, nil, &recordingSender{}, &fixedRenderer{}, &staticRecipient{}, base, nil); err != nil {
		t.Errorf("DefaultConfig() rejected: %v", err)
	}
}

func TestNewWorkerRequiresCollaborators(t *testing.T) {
	t.Parallel()

	cfg := outbox.DefaultConfig()

	if _, err := outbox.NewWorker(nil, nil, nil, &fixedRenderer{}, &staticRecipient{}, cfg, nil); err == nil {
		t.Error("NewWorker() accepted a nil sender")
	}
	if _, err := outbox.NewWorker(nil, nil, &recordingSender{}, nil, &staticRecipient{}, cfg, nil); err == nil {
		t.Error("NewWorker() accepted a nil renderer")
	}
	if _, err := outbox.NewWorker(nil, nil, &recordingSender{}, &fixedRenderer{}, nil, cfg, nil); err == nil {
		t.Error("NewWorker() accepted a nil recipient lookup")
	}
}

// A claim stranded by a worker killed mid-delivery must be reclaimable, or the
// message is never delivered.
func TestStaleClaimIsReclaimable(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	ctx := context.Background()
	// Simulate a worker that claimed the row and died.
	if _, err := env.Pool.Exec(ctx, `
		UPDATE outbox SET claimed_at = now() - interval '1 hour', attempts = 1
		WHERE topic = 'lesson_started'`); err != nil {
		t.Fatalf("age the claim: %v", err)
	}

	sender := &recordingSender{}
	w := newWorker(t, env, sender, &fixedRenderer{body: "hello"}, &staticRecipient{msisdn: fx.LearnerMSISDN})

	delivered, err := w.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce() error = %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1; a stale claim should be reclaimable", delivered)
	}
}

// A fresh claim from another worker must not be stolen.
func TestFreshClaimIsNotStolen(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	ctx := context.Background()
	if _, err := env.Pool.Exec(ctx,
		`UPDATE outbox SET claimed_at = now() WHERE topic = 'lesson_started'`); err != nil {
		t.Fatalf("set claim: %v", err)
	}

	sender := &recordingSender{}
	w := newWorker(t, env, sender, &fixedRenderer{body: "hello"}, &staticRecipient{msisdn: fx.LearnerMSISDN})

	if _, err := w.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce() error = %v", err)
	}

	// Two workers running concurrently must not both send: SMS costs money per
	// message, so a duplicate send is a real defect, not a cosmetic one.
	if sender.count() != 0 {
		t.Errorf("sender called %d times for a row claimed moments ago, want 0", sender.count())
	}
}

// SKIP LOCKED is what makes two concurrent workers safe.
func TestConcurrentDrainsDoNotDoubleSend(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	seedOutbox(t, env, fx, "lesson_started")

	sender := &recordingSender{}
	w := newWorker(t, env, sender, &fixedRenderer{body: "hello"}, &staticRecipient{msisdn: fx.LearnerMSISDN})

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.DrainOnce(context.Background())
		}()
	}
	wg.Wait()

	// Four workers raced for one row; exactly one send is correct.
	if sender.count() != 1 {
		t.Errorf("sender called %d times across four concurrent drains, want 1", sender.count())
	}
}

func TestPayloadIsNotSentForMalformedRows(t *testing.T) {
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(context.Background(), t, env.Pool)

	// A corrupt payload must not become a blank message.
	eventID := uuid.New()
	ctx := context.Background()
	if _, err := env.Pool.Exec(ctx, `
		INSERT INTO learning_events (event_id, institution_id, learner_id, course_id, lesson_id,
		                             kind, source, payload, occurred_at)
		VALUES ($1, $2, $3, $4, $5, 'lesson_started', 'web', '{}'::jsonb, now())`,
		eventID, fx.InstitutionID, fx.LearnerID, fx.CourseID, fx.LessonID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	// Valid JSONB, wrong shape: a JSON array where an object is expected. The
	// column itself rejects literal garbage, so this is the realistic form of
	// the corruption -- a payload written by a version that shaped it
	// differently.
	if _, err := env.Pool.Exec(ctx, `
		INSERT INTO outbox (institution_id, event_id, topic, payload, destination_msisdn)
		VALUES ($1, $2, 'lesson_started', '[]'::jsonb, $3)`,
		fx.InstitutionID, eventID, fx.LearnerMSISDN); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	sender := &recordingSender{}
	w := newWorker(t, env, sender, &fixedRenderer{body: "hello"}, &staticRecipient{msisdn: fx.LearnerMSISDN})

	if _, err := w.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce() error = %v", err)
	}
	if sender.count() != 0 {
		t.Errorf("sender called %d times for a malformed payload, want 0", sender.count())
	}
}

// ---------------------------------------------------------------------------
// Outbound segmentation
// ---------------------------------------------------------------------------

func TestSendTextSplitsLongMessages(t *testing.T) {
	t.Parallel()

	gateway := sms.NewLogSender(16)
	out := sms.NewOutbound(gateway, sms.NewTemplates(), nil)

	long := strings.Repeat("A very long lesson sentence. ", 40)
	err := out.SendText(context.Background(), "+254700000001", long, "ref-1")
	if err != nil {
		t.Fatalf("SendText() error = %v", err)
	}

	// Drain what the gateway recorded.
	received := drainSent(gateway)
	if len(received) < 2 {
		t.Errorf("got %d messages for a %d-character body, want several", len(received), len(long))
	}
	for i, msg := range received {
		if len([]rune(msg.Body)) > 153 {
			t.Errorf("segment %d is %d runes, over the GSM-7 segment limit", i, len([]rune(msg.Body)))
		}
	}
}

func drainSent(gateway *sms.LogSender) []sms.Message {
	var out []sms.Message
	for {
		select {
		case msg := <-gateway.Sent:
			out = append(out, msg)
		default:
			return out
		}
	}
}
