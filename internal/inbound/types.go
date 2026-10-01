package inbound

// Acknowledgement is a reply to a learner, on its way out.
//
// The inbound path computes the text; delivery is the outbox worker's job. That
// split is deliberate: a reply computed here must survive a gateway outage while
// the webhook is being handled, and sending it inline would lose it.
type Acknowledgement struct {
	// To is the learner's number in E.164 form.
	To string
	// Body is the rendered message.
	Body string
	// ReplyTo is the provider's id of the message being answered. Carried so a
	// delivery report for the acknowledgement can be attributed, and so
	// threading is possible where the provider supports it.
	ReplyTo string
}
