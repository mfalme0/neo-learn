package sms

import (
	"fmt"
	"strings"
	"unicode"
)

// SMS length limits.
//
// A single GSM-7 message holds 160 characters, or 153 once segmented at 153
// per part. UCS-2 (any character outside the GSM-7 alphabet, which includes
// all emoji and many non-Latin scripts) holds 70 per segment instead.
const (
	gsm7SingleLimit   = 160
	gsm7SegmentLength = 153
	ucs2SingleLimit   = 70
	ucs2SegmentLength = 67
	// The header the README shows on every message. Per-message fixed overhead
	// is what makes the payload smaller than the raw limit.
	maxHeaderLines = 4
)

// Chunk splits a message body into segment-sized pieces.
//
// Splitting on GSM-7 vs UCS-2 boundaries is not pedantry: a Swahili or Amharic
// lesson body drops from 160 to 70 characters per segment, so a message that
// "fits" by the GSM limit silently bills as two segments. Overrunning a
// provider's per-message segment allowance causes the send to be rejected, so
// the conservative limit is the one to respect.
func Chunk(body string) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}

	limit := gsm7SingleLimit
	segment := gsm7SegmentLength
	if !isGSM7(body) {
		limit = ucs2SingleLimit
		segment = ucs2SegmentLength
	}

	if len([]rune(body)) <= limit {
		return []string{body}
	}

	return splitOnWords(body, segment)
}

// isGSM7 reports whether every rune is in the GSM 7-bit alphabet.
//
// This checks the printable set only. It does not attempt to validate the full
// extension-table escapes for currency and accented characters, which is a
// deliberate simplification: misdetecting one of those as GSM-7 produces a
// slightly long segment, whereas the reverse produces a truncated one.
func isGSM7(s string) bool {
	for _, r := range s {
		if r < 32 || r > 126 {
			// Tab and newline are legitimate in the GSM alphabet.
			if r != '\n' && r != '\r' && r != '\t' {
				return false
			}
		}
	}
	return true
}

// splitOnWords breaks text at whitespace near the segment limit.
//
// Splitting mid-word would mangle a formula or a word; leaving a short trailing
// segment is harmless. If a single word is itself longer than the limit, it is
// hard-split rather than sent oversized, because the alternative is a rejected
// send.
func splitOnWords(body string, segment int) []string {
	runes := []rune(body)
	var chunks []string

	for start := 0; start < len(runes); {
		end := min(start+segment, len(runes))
		if end < len(runes) {
			// Walk back to the last whitespace inside the window so words stay
			// intact. If there is none, fall through and hard-split.
			if boundary := lastWhitespaceBefore(runes, start, end); boundary > start {
				end = boundary
			}
		}

		chunk := strings.TrimRight(string(runes[start:end]), " \t\n")
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		start = end
	}

	return chunks
}

// lastWhitespaceBefore returns the index of the last whitespace rune in
// runes[start:end], or -1 when the window contains none.
func lastWhitespaceBefore(runes []rune, start, end int) int {
	for i := end - 1; i >= start; i-- {
		if unicode.IsSpace(runes[i]) {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Message templates
// ---------------------------------------------------------------------------

// Templates renders the fixed parts of every message, so the brand and the
// reply instructions are identical no matter which code path builds a message.
type Templates struct {
	Brand string
}

// NewTemplates builds templates with a default brand.
func NewTemplates() Templates {
	return Templates{Brand: "NEO LEARN"}
}

// header is the first lines of every outbound message.
//
// A learner receiving several texts in a row needs to know which are course
// material and which are, say, a fee reminder. Naming the sender in the first
// two lines is what makes that possible on a notification that shows only the
// first few characters.
func (t Templates) header() string {
	brand := t.Brand
	if brand == "" {
		brand = "NEO LEARN"
	}
	return brand
}

// QuestionMessage renders a multiple-choice question.
//
// The shape is fixed by the constraint that an answer must fit in a reply: the
// learner is told the exact set of acceptable answers, and a learner with no
// other information can still respond.
func (t Templates) QuestionMessage(params QuestionParams) string {
	var b strings.Builder

	b.WriteString(t.header())
	b.WriteString("\n\n")

	if params.AssessmentTitle != "" {
		b.WriteString(params.AssessmentTitle)
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("Question %d/%d\n\n", params.Position, params.Total))
	b.WriteString(params.Prompt)
	b.WriteString("\n\n")

	for _, choice := range params.Choices {
		b.WriteString(choice.Label)
		b.WriteString(". ")
		b.WriteString(choice.Body)
		b.WriteString("\n")
	}

	// Listing the valid answers removes the single most common failure: a
	// learner replying "3" instead of "C".
	labels := make([]string, 0, len(params.Choices))
	for _, choice := range params.Choices {
		labels = append(labels, choice.Label)
	}

	b.WriteString("\nReply ")
	if len(labels) == 1 {
		b.WriteString(labels[0])
	} else {
		b.WriteString(strings.Join(labels[:len(labels)-1], ", "))
		b.WriteString(" or ")
		b.WriteString(labels[len(labels)-1])
	}
	b.WriteString(".")
	b.WriteString("\nReply STOP to stop messages.")

	return b.String()
}

// QuestionParams are the inputs for a rendered question.
type QuestionParams struct {
	AssessmentTitle string
	Prompt          string
	Position        int
	Total           int
	Choices         []Choice
}

// Choice is one selectable answer.
type Choice struct {
	Label string
	Body  string
}

// CorrectAnswerMessage acknowledges a right answer and, when more questions
// remain, says so. Silence after an answer is indistinguishable from a dropped
// message, and the learner cannot tell whether to keep going.
func (t Templates) CorrectAnswerMessage(params ResultParams) string {
	return t.resultMessage(params, "Correct.", params.ScoreLine)
}

// IncorrectAnswerMessage acknowledges a wrong answer.
//
// It states the correct choice rather than only marking the attempt wrong:
// the platform is a teaching tool, and a learner who is told the right answer
// learns something even when they got it wrong.
func (t Templates) IncorrectAnswerMessage(params ResultParams) string {
	body := "Not correct. The answer is " + params.CorrectChoice + "."
	if params.CorrectBody != "" {
		body += " " + params.CorrectBody
	}
	return t.resultMessage(params, body, params.ScoreLine)
}

func (t Templates) resultMessage(params ResultParams, verdict, scoreLine string) string {
	var b strings.Builder
	b.WriteString(t.header())
	b.WriteString("\n\n")
	b.WriteString(verdict)

	if scoreLine != "" {
		b.WriteString("\n")
		b.WriteString(scoreLine)
	}

	switch {
	case params.Completed:
		b.WriteString("\n\nAssessment complete. Thank you.")
	case params.NextAvailable:
		b.WriteString("\n\nReply NEXT for the next question.")
	}
	b.WriteString("\nReply HELP for help, STOP to stop messages.")

	return b.String()
}

// ResultParams are the inputs for an answer acknowledgement.
type ResultParams struct {
	ScoreLine     string
	CorrectChoice string
	CorrectBody   string
	Completed     bool
	NextAvailable bool
}

// LessonMessage renders lesson content for SMS.
//
// Plain paragraphs, blank-line separated, matching what the web client shows,
// so a learner switching between the two sees the same thing.
func (t Templates) LessonMessage(title, body string) string {
	var b strings.Builder
	b.WriteString(t.header())
	b.WriteString("\n\n")
	b.WriteString(title)
	b.WriteString("\n\n")
	// Collapse the paragraph structure markdown introduces into the blank-line
	// separation that reads correctly on a phone.
	b.WriteString(smsSafeBody(body))
	b.WriteString("\n\nReply DONE when you have read this.")
	return b.String()
}

// LessonCompleteMessage acknowledges finishing a lesson.
func (t Templates) LessonCompleteMessage(lessonTitle string) string {
	return t.header() + "\n\nMarked complete: " + lessonTitle + ".\nReply NEXT for the next lesson."
}

// StopMessage confirms an opt-out.
//
// Confirming matters: a learner who texts STOP and then keeps receiving
// messages concludes the system is broken. It also states how to resume, so
// the opt-out is not a dead end.
func (t Templates) StopMessage() string {
	return t.header() +
		"\n\nYou have stopped receiving messages. Reply START to resume at any time."
}

// StartMessage confirms resuming.
func (t Templates) StartMessage() string {
	return t.header() + "\n\nWelcome back. Reply NEXT to continue where you left off."
}

// HelpMessage lists the commands.
//
// Sent whenever the learner asks, but it is also the fallback for an
// unparseable reply, because a learner who typed something the parser did not
// understand needs to learn what would work.
func (t Templates) HelpMessage(hasPendingQuestion bool) string {
	var b strings.Builder
	b.WriteString(t.header())
	b.WriteString("\n\nCommands:\n")
	if hasPendingQuestion {
		b.WriteString("A, B, C, D  answer the current question\n")
	}
	b.WriteString("NEXT        continue\n")
	b.WriteString("HELP        show this message\n")
	b.WriteString("STOP        stop receiving messages\n")
	return b.String()
}

// NotUnderstoodMessage is the fallback for an unparseable reply.
//
// It echoes what was received: a learner typing "b" against a prompt that
// expected "B" needs to see the difference, and echo-and-explain is more useful
// than a bare "invalid".
func (t Templates) NotUnderstoodMessage(received string, expected []string) string {
	var b strings.Builder
	b.WriteString(t.header())
	b.WriteString("\n\n")
	if received != "" {
		b.WriteString(fmt.Sprintf("Did not understand %q.\n\n", received))
	}
	if len(expected) > 0 {
		b.WriteString("Reply ")
		b.WriteString(strings.Join(expected, ", "))
		b.WriteString(".\n")
	}
	b.WriteString("Reply HELP for all commands.")
	return b.String()
}

// smsSafeBody prepares lesson markdown for SMS.
//
// Headings, emphasis markers, list bullets, and links are stripped or
// rewritten rather than passed through: "## Fractions" or "[text](url)" is
// noise on a 160-character screen, and a raw URL eats the whole segment.
func smsSafeBody(body string) string {
	var out []string

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			out = append(out, "")
			continue
		}

		// Heading markers.
		trimmed = strings.TrimLeft(trimmed, "#")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == "" {
			continue
		}

		// Emphasis markers: bold/italic are unreadable on a feature phone.
		trimmed = strings.NewReplacer("**", "", "__", "", "*", "").Replace(trimmed)

		// List bullets become a plain separator.
		trimmed = strings.TrimSpace(strings.TrimLeft(trimmed, "-*+ "))

		out = append(out, trimmed)
	}

	// Collapse runs of blank lines to a single break.
	joined := strings.Join(out, "\n")
	for strings.Contains(joined, "\n\n\n") {
		joined = strings.ReplaceAll(joined, "\n\n\n", "\n\n")
	}

	return strings.TrimSpace(joined)
}
