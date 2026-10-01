package sms

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Segment limits
// ---------------------------------------------------------------------------

func TestChunkReturnsShortBodyAsOneSegment(t *testing.T) {
	t.Parallel()

	chunks := Chunk("NEO LEARN\n\nQuestion 1/1\n\n2+2=?")

	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	if chunks[0] == "" {
		t.Error("chunk is empty")
	}
}

func TestChunkReturnsNothingForEmptyBody(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"", "   ", "\n\n", "\t"} {
		if got := Chunk(body); got != nil {
			t.Errorf("Chunk(%q) = %v, want nil", body, got)
		}
	}
}

func TestChunkSplitsLongBodyAndRespectsSegmentLimit(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("word ", 200)
	chunks := Chunk(body)

	if len(chunks) < 2 {
		t.Fatalf("got %d chunks for a %d-char body, want several", len(chunks), len(body))
	}

	for i, chunk := range chunks {
		if got := len([]rune(chunk)); got > gsm7SegmentLength {
			t.Errorf("chunk %d is %d runes, over the %d limit", i, got, gsm7SegmentLength)
		}
		if strings.TrimSpace(chunk) == "" {
			t.Errorf("chunk %d is whitespace only", i)
		}
	}
}

// Splitting mid-word mangles a formula, so segments break at whitespace where
// possible.
func TestChunkPrefersWordBoundaries(t *testing.T) {
	t.Parallel()

	// Deliberately sized so the limit falls mid-word.
	body := strings.Repeat("alpha beta ", 40)
	chunks := Chunk(body)

	for i, chunk := range chunks {
		if i < len(chunks)-1 && strings.Contains(chunk, "-") {
			t.Errorf("chunk %d looks split mid-word: %q", i, chunk)
		}
	}
}

// A single token longer than a segment must still be split: the alternative is a
// send the provider rejects outright.
func TestChunkHardSplitsAnOversizedToken(t *testing.T) {
	t.Parallel()

	// A base64 blob with no whitespace at all.
	body := strings.Repeat("x", 500)
	chunks := Chunk(body)

	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want the token split", len(chunks))
	}
	for i, chunk := range chunks {
		if got := len([]rune(chunk)); got > gsm7SegmentLength {
			t.Errorf("chunk %d is %d runes, over the limit", i, got)
		}
	}
}

// The UCS-2 limit is half the GSM-7 one, so getting this wrong silently doubles
// a learner's per-message cost.
func TestChunkUsesShorterLimitForNonGSM7Text(t *testing.T) {
	t.Parallel()

	// Amharic is well outside the GSM 7-bit alphabet.
	body := strings.Repeat("ፈራስት ", 40)
	chunks := Chunk(body)

	if len(chunks) == 0 {
		t.Fatal("no chunks returned")
	}
	for i, chunk := range chunks {
		if got := len([]rune(chunk)); got > ucs2SegmentLength {
			t.Errorf("chunk %d is %d runes, over the UCS-2 limit of %d", i, got, ucs2SegmentLength)
		}
	}

	// An emoji forces UCS-2 for the whole message.
	emoji := strings.Repeat("🎓 ", 40)
	for i, chunk := range Chunk(emoji) {
		if got := len([]rune(chunk)); got > ucs2SegmentLength {
			t.Errorf("emoji chunk %d is %d runes, over the UCS-2 limit", i, got)
		}
	}
}

func TestIsGSM7(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"Hello":       true,
		"Line\nbreak": true,
		"Tab\there":   true,
		"Café":        false, // accented characters need the extension table
		"ሰላም":         false,
		"emoji 🎓":     false,
	}
	for input, want := range cases {
		if got := isGSM7(input); got != want {
			t.Errorf("isGSM7(%q) = %t, want %t", input, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Control-word parsing
// ---------------------------------------------------------------------------

func TestNormalizedCommand(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"STOP":        CommandStop,
		"stop":        CommandStop,
		"  Stop  ":    CommandStop,
		"STOP ":       CommandStop, // some phones append a space
		"UNSUBSCRIBE": CommandStop,
		"quit":        CommandStop,
		"START":       CommandStart,
		"start":       CommandStart,
		"RESUME":      CommandStart,
		"HELP":        CommandHelp,
		"help":        CommandHelp,
		"?":           CommandHelp,
		"NEXT":        CommandNext,
		"continue":    CommandNext,
		"A":           "", // an answer, not a command
		"maybe":       "",
		"":            "",
	}

	for input, want := range cases {
		if got := NormalizedCommand(input); got != want {
			t.Errorf("NormalizedCommand(%q) = %q, want %q", input, got, want)
		}
	}
}

// A learner who texts STOP and keeps receiving messages concludes the system is
// broken, so casing and padding must not matter.
func TestStopIsRecognisedDespiteCasingAndPadding(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"stop", "STOP", " Stop ", "STOP ", "sToP"} {
		if got := NormalizedCommand(input); got != CommandStop {
			t.Errorf("NormalizedCommand(%q) = %q, want STOP", input, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Reply parsing
// ---------------------------------------------------------------------------

func TestParseReplyRecognisesValidChoice(t *testing.T) {
	t.Parallel()

	valid := []string{"A", "B", "C", "D"}

	cases := map[string]string{
		"B":   "B",
		"b":   "B", // lowercase is the common case from a feature phone
		" B ": "B",
		"B.":  "B",
		"b!":  "B",
	}

	for input, want := range cases {
		got := ParseReply(input, valid)
		if got.Kind != ReplyAnswer {
			t.Errorf("ParseReply(%q).Kind = %q, want answer", input, got.Kind)
			continue
		}
		if got.ChoiceLabel != want {
			t.Errorf("ParseReply(%q).ChoiceLabel = %q, want %q", input, got.ChoiceLabel, want)
		}
	}
}

// A command must not be read as a choice, even if a question offers one.
func TestParseReplyPrefersCommandOverChoice(t *testing.T) {
	t.Parallel()

	// A contrived case: a question with choices labelled A-D cannot contain
	// NEXT, but the ordering is still the guarantee.
	got := ParseReply("STOP", []string{"A", "B", "C", "D"})
	if got.Kind != ReplyCommand {
		t.Errorf("ParseReply(STOP).Kind = %q, want command", got.Kind)
	}
	if got.Command != CommandStop {
		t.Errorf("ParseReply(STOP).Command = %q, want STOP", got.Command)
	}
}

func TestParseReplyRejectsChoiceNotOnOffer(t *testing.T) {
	t.Parallel()

	valid := []string{"A", "B", "C", "D"}

	for _, input := range []string{"Z", "9", "E"} {
		got := ParseReply(input, valid)
		if got.Kind != ReplyUnrecognized {
			t.Errorf("ParseReply(%q).Kind = %q, want unrecognized", input, got.Kind)
		}
	}
}

// With nothing on offer, a bare letter must not be treated as an answer.
func TestParseReplyWithNoPendingQuestion(t *testing.T) {
	t.Parallel()

	got := ParseReply("A", nil)
	if got.Kind != ReplyUnrecognized {
		t.Errorf("ParseReply(A, nil).Kind = %q, want unrecognized", got.Kind)
	}
}

func TestParseReplyRetainsRawForEcho(t *testing.T) {
	t.Parallel()

	// The learner needs to see what was misread, so the original is kept.
	got := ParseReply("  bbb  ", []string{"A", "B"})
	if got.Raw != "  bbb  " {
		t.Errorf("Raw = %q, want the original %q", got.Raw, "  bbb  ")
	}
}

func TestParseReplyEmptyBody(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "   ", "\n"} {
		got := ParseReply(input, []string{"A"})
		if got.Kind != ReplyUnrecognized {
			t.Errorf("ParseReply(%q).Kind = %q, want unrecognized", input, got.Kind)
		}
	}
}

// ---------------------------------------------------------------------------
// Inbound idempotency
// ---------------------------------------------------------------------------

// The whole inbound retry story depends on this: a gateway retry must derive the
// same event id, so the learning engine discards it.
func TestInboundEventIDIsStableForTheSameProviderMessage(t *testing.T) {
	t.Parallel()

	first := InboundEventID("gw-msg-12345", 1)
	second := InboundEventID("gw-msg-12345", 1)

	if first != second {
		t.Errorf("the same provider message derived different ids: %s vs %s", first, second)
	}
}

func TestInboundEventIDDiffersPerMessage(t *testing.T) {
	t.Parallel()

	a := InboundEventID("gw-msg-1", 1)
	b := InboundEventID("gw-msg-2", 1)

	if a == b {
		t.Error("different provider messages derived the same event id")
	}
}

// The constraint is global, so two institutions must not collide.
func TestInboundEventIDDiffersPerInstitution(t *testing.T) {
	t.Parallel()

	a := InboundEventID("gw-msg-1", 1)
	b := InboundEventID("gw-msg-1", 2)

	if a == b {
		t.Error("the same provider message at two institutions derived the same id")
	}
}

func TestInboundEventIDIsAValidUUID(t *testing.T) {
	t.Parallel()

	id := InboundEventID("gw-msg-1", 1)
	if _, err := uuid.Parse(id.String()); err != nil {
		t.Errorf("derived id %q is not a parseable UUID: %v", id, err)
	}
}

func TestInboundEventIDToleratesSurroundingWhitespace(t *testing.T) {
	t.Parallel()

	// A provider that pads an id inconsistently between retries would
	// otherwise defeat deduplication.
	a := InboundEventID("  gw-msg-1  ", 1)
	b := InboundEventID("gw-msg-1", 1)

	if a != b {
		t.Errorf("whitespace changed the derived id: %s vs %s", a, b)
	}
}

// ---------------------------------------------------------------------------
// Event construction
// ---------------------------------------------------------------------------

func TestBuildAnswerEventSetsSMSSource(t *testing.T) {
	t.Parallel()

	occurred := timeAt("2026-01-01T09:10:00Z")
	ev := BuildAnswerEvent(AnswerEventParams{
		EventID:       uuid.New(),
		InstitutionID: 7,
		LearnerID:     42,
		QuestionID:    99,
		AssessmentID:  11,
		ChoiceLabel:   "B",
		OccurredAt:    occurred,
	})

	// The source is what makes an SMS answer distinguishable in the log from
	// the same answer submitted in a browser.
	if string(ev.Source) != "sms" {
		t.Errorf("Source = %q, want sms", ev.Source)
	}
	if string(ev.Kind) != "question_answered" {
		t.Errorf("Kind = %q, want question_answered", ev.Kind)
	}
	if ev.LearnerID != 42 || ev.InstitutionID != 7 {
		t.Errorf("identity = %d/%d, want 42/7", ev.LearnerID, ev.InstitutionID)
	}
	if !ev.OccurredAt.Equal(occurred) {
		t.Errorf("OccurredAt = %v, want the gateway's %v", ev.OccurredAt, occurred)
	}
}

func TestBuildLessonEvent(t *testing.T) {
	t.Parallel()

	ev := BuildLessonEvent(LessonEventParams{
		EventID:       uuid.New(),
		InstitutionID: 1,
		LearnerID:     2,
		LessonID:      3,
		OccurredAt:    timeAt("2026-01-01T09:00:00Z"),
	})

	if string(ev.Source) != "sms" {
		t.Errorf("Source = %q, want sms", ev.Source)
	}
	if ev.LessonID == nil || *ev.LessonID != 3 {
		t.Errorf("LessonID = %v, want 3", ev.LessonID)
	}
	// Payload must not be nil: the column is NOT NULL.
	if len(ev.Payload) == 0 {
		t.Error("Payload is empty; the column is NOT NULL")
	}
}

// ---------------------------------------------------------------------------
// Phone numbers
// ---------------------------------------------------------------------------

func TestValidateE164(t *testing.T) {
	t.Parallel()

	valid := []string{
		"+254700000001",
		"+12065550123", // a short NANP number is the shortest plausible case
		"+447700900123",
	}
	for _, number := range valid {
		if !ValidateE164(number) {
			t.Errorf("ValidateE164(%q) = false, want true", number)
		}
	}

	invalid := []string{
		"",
		"0700000001",       // local format, not E.164
		"+1",               // too short
		"+25470000 0001",   // embedded space
		"+254-700-000-001", // punctuation
		"not a number",
		"+2547000000011234567890", // too long
	}
	for _, number := range invalid {
		if ValidateE164(number) {
			t.Errorf("ValidateE164(%q) = true, want false", number)
		}
	}
}

// A silently wrong number sends a learner's answers to a stranger.
func TestNormalizeE164StripsPunctuation(t *testing.T) {
	t.Parallel()

	got, err := NormalizeE164("+254 700-000.001")
	if err != nil {
		t.Fatalf("NormalizeE164 error = %v", err)
	}
	if got != "+254700000001" {
		t.Errorf("got %q, want +254700000001", got)
	}
}

func TestNormalizeE164RejectsUnusableInput(t *testing.T) {
	t.Parallel()

	for _, number := range []string{"0700000001", "", "abc", "+123"} {
		if _, err := NormalizeE164(number); err == nil {
			t.Errorf("NormalizeE164(%q) error = nil, want an error", number)
		}
	}
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func TestQuestionMessageListsEveryAcceptableAnswer(t *testing.T) {
	t.Parallel()

	templates := NewTemplates()
	body := templates.QuestionMessage(QuestionParams{
		AssessmentTitle: "Algebra 01",
		Prompt:          "2x + 4 = 10",
		Position:        3,
		Total:           10,
		Choices: []Choice{
			{Label: "A", Body: "2"},
			{Label: "B", Body: "3"},
			{Label: "C", Body: "4"},
			{Label: "D", Body: "5"},
		},
	})

	for _, want := range []string{
		"NEO LEARN",
		"Algebra 01",
		"Question 3/10",
		"2x + 4 = 10",
		"A. 2",
		"B. 3",
		"C. 4",
		"D. 5",
		"Reply A, B, C or D",
		"STOP",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("message is missing %q:\n%s", want, body)
		}
	}
}

// The most common failure is replying "3" instead of "C"; the prompt has to
// make the acceptable answers explicit.
func TestQuestionMessageNamesOnlyTheLabels(t *testing.T) {
	t.Parallel()

	templates := NewTemplates()
	body := templates.QuestionMessage(QuestionParams{
		Prompt:   "pick one",
		Position: 1,
		Total:    1,
		Choices: []Choice{
			{Label: "A", Body: "x"},
			{Label: "B", Body: "y"},
		},
	})

	if !strings.Contains(body, "Reply A or B") {
		t.Errorf("expected 'Reply A or B' for two choices:\n%s", body)
	}
}

func TestStopMessageExplainsHowToResume(t *testing.T) {
	t.Parallel()

	templates := NewTemplates()
	body := templates.StopMessage()

	// A learner who cannot get back in would be locked out permanently.
	if !strings.Contains(body, "START") {
		t.Errorf("stop message does not mention how to resume:\n%s", body)
	}
	if !strings.Contains(body, "stopped") {
		t.Errorf("stop message does not confirm the opt-out:\n%s", body)
	}
}

func TestNotUnderstoodMessageEchoesTheInput(t *testing.T) {
	t.Parallel()

	templates := NewTemplates()
	body := templates.NotUnderstoodMessage("xyz", []string{"A", "B"})

	// Echoing lets a learner who typed "b" against a prompt expecting "B" see
	// the difference.
	if !strings.Contains(body, "xyz") {
		t.Errorf("did not echo the received text:\n%s", body)
	}
	if !strings.Contains(body, "A, B") {
		t.Errorf("did not list the valid answers:\n%s", body)
	}
}

func TestSmsSafeBodyStripsMarkdownNoise(t *testing.T) {
	t.Parallel()

	body := smsSafeBody("## Fractions\n\nA fraction has a **numerator**.\n\n- first\n- second")

	for _, unwanted := range []string{"##", "**", "- first"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("markdown artefact %q survived:\n%s", unwanted, body)
		}
	}
	for _, wanted := range []string{"Fractions", "numerator", "first", "second"} {
		if !strings.Contains(body, wanted) {
			t.Errorf("content %q was lost:\n%s", wanted, body)
		}
	}
}

func TestSmsSafeBodyCollapsesBlankLineRuns(t *testing.T) {
	t.Parallel()

	body := smsSafeBody("one\n\n\n\n\ntwo")
	if strings.Contains(body, "\n\n\n") {
		t.Errorf("runs of blank lines survived:\n%q", body)
	}
}

func TestLessonMessageInvitesCompletion(t *testing.T) {
	t.Parallel()

	templates := NewTemplates()
	body := templates.LessonMessage("Fractions", "A fraction represents part of a whole.")

	if !strings.Contains(body, "Fractions") {
		t.Errorf("missing the title:\n%s", body)
	}
	if !strings.Contains(body, "DONE") {
		t.Errorf("does not tell the learner how to finish:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// LogSender
// ---------------------------------------------------------------------------

func TestLogSenderRecordsMessages(t *testing.T) {
	t.Parallel()

	gateway := NewLogSender(4)
	receipt, err := gateway.Send(context.Background(), Message{To: "+254700000001", Body: "hi"})

	if err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if receipt.Status != StatusDelivered {
		t.Errorf("Status = %q, want delivered", receipt.Status)
	}

	select {
	case msg := <-gateway.Sent:
		if msg.To != "+254700000001" {
			t.Errorf("recorded To = %q", msg.To)
		}
	default:
		t.Error("nothing was recorded on the Sent channel")
	}
}

func TestLogSenderReportsNoInboundSupport(t *testing.T) {
	t.Parallel()

	gateway := NewLogSender(1)
	caps := CapabilitiesOf(gateway)

	// Being explicit about the limitation is what stops the scheduler relying
	// on a reply that can never arrive.
	if caps.Inbound {
		t.Error("LogSender claims inbound support it does not have")
	}
}

func TestLogSenderFailureIsSimulatable(t *testing.T) {
	t.Parallel()

	gateway := NewLogSender(1)
	gateway.FailNext = 1

	if _, err := gateway.Send(context.Background(), Message{To: "+254700000001"}); !errors.Is(err, ErrGatewayUnavailable) {
		t.Errorf("first Send error = %v, want ErrGatewayUnavailable", err)
	}

	// The failure is one-shot, so the retry path is exercisable.
	if _, err := gateway.Send(context.Background(), Message{To: "+254700000001"}); err != nil {
		t.Errorf("second Send error = %v, want nil", err)
	}
}

func TestLogSenderDoesNotBlockWhenNobodyIsListening(t *testing.T) {
	t.Parallel()

	// A full buffer must not stall delivery: nobody watching a dev log is not a
	// reason to fail a send.
	gateway := NewLogSender(1)
	for range 10 {
		if _, err := gateway.Send(context.Background(), Message{To: "+254700000001"}); err != nil {
			t.Fatalf("Send error = %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Terminal vs retryable
// ---------------------------------------------------------------------------

func TestDeliveryStatusTerminal(t *testing.T) {
	t.Parallel()

	// Retrying a rejected number burns money and, for an opt-out, keeps texting
	// somebody who asked to stop.
	terminal := []DeliveryStatus{StatusDelivered, StatusSent, StatusRejected}
	for _, status := range terminal {
		if !status.Terminal() {
			t.Errorf("%q.Terminal() = false, want true", status)
		}
	}

	retryable := []DeliveryStatus{StatusQueued, StatusFailed}
	for _, status := range retryable {
		if status.Terminal() {
			t.Errorf("%q.Terminal() = true, want false", status)
		}
	}
}

func TestStripToASCII(t *testing.T) {
	t.Parallel()

	got := StripToASCII("Hello 🎓 world\nnewline")
	if strings.ContainsRune(got, '🎓') {
		t.Errorf("StripToASCII left a non-ASCII rune: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "world") {
		t.Errorf("StripToASCII lost content: %q", got)
	}
}

// timeAt parses a fixed timestamp for tests, failing loudly on a typo rather
// than comparing against the zero time.
func timeAt(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic("sms_test: bad timestamp " + value + ": " + err.Error())
	}
	return parsed
}
