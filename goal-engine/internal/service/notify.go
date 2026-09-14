package service

import (
	"context"
	"strings"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/core"
)

// Notices turns a decision that has already been recorded into one short message
// for a human.
//
// The wording lives here rather than in internal/domain for the same reason
// BuildBrief does: it is text for a person, not a rule, and nothing decides
// anything from it.
//
// What it deliberately does not carry is the number. No amount, no currency, no
// observed value, no pace ratio — the tests assert that a headline contains no
// digit at all. Two reasons worth the constraint. A chat message is retained on
// somebody else's servers, whereas the whole posture of this project is that the
// business figures stay on the box. And a message complete enough to decide from
// invites deciding from it: approving a spend at a glance, without the policy, the
// day's total or the payload in front of you, is the failure this project is most
// exposed to. The link is what makes somebody open the console, where all of that
// is.
type Notices struct {
	notifier   Notifier
	consoleURL string
	log        zerolog.Logger
}

// NoticesDeps is what a Notices needs. All of it is optional in the sense that
// wiring may pass nothing, which is how notifications are turned off.
type NoticesDeps struct {
	// Notifier is core's notification endpoint. Absent means disabled.
	Notifier Notifier
	// ConsoleBaseURL is the console's origin, with no trailing slash. Empty means
	// notifications go out without a link rather than with a broken one.
	ConsoleBaseURL string
	Logger         zerolog.Logger
}

// NewNotices returns a Notices, or nil when there is nothing to send with.
//
// A nil *Notices is the disabled case and every method below tolerates it. That
// is the point: NOTIFY_ENABLED=false becomes one branch in wiring, and neither the
// monitor nor the approval gate carries a question about messaging.
func NewNotices(deps NoticesDeps) *Notices {
	if deps.Notifier == nil {
		return nil
	}
	return &Notices{
		notifier:   deps.Notifier,
		consoleURL: strings.TrimRight(strings.TrimSpace(deps.ConsoleBaseURL), "/"),
		log:        deps.Logger,
	}
}

// TriggerDispatched says an agent has been woken for a goal that fell behind.
//
// Call it after the dispatch row is marked sent and the audit entry is written:
// this is a description of something that already happened.
func (n *Notices) TriggerDispatched(ctx context.Context, goalID string) {
	if n == nil {
		return
	}
	n.send(ctx, core.NotifyRequest{
		Kind:      core.NotifyTrigger,
		SubjectID: goalID,
		Headline:  "A goal fell behind pace and an agent has been woken.",
		Link:      n.link("/goals/" + goalID),
	})
}

// ApprovalPending says a spend is waiting for a human.
//
// The action type is included because it is the policy name an operator chose in
// policies.yaml, and knowing whether this is the familiar gate or something
// unexpected is the difference between reading the message now and later. It is a
// name, not a figure.
func (n *Notices) ApprovalPending(ctx context.Context, approvalID, actionType string) {
	if n == nil {
		return
	}
	headline := "A spend is waiting for a decision."
	if trimmed := strings.TrimSpace(actionType); trimmed != "" {
		headline = "A spend is waiting for a decision (" + trimmed + ")."
	}
	n.send(ctx, core.NotifyRequest{
		Kind:      core.NotifyApprovalPending,
		SubjectID: approvalID,
		Headline:  headline,
		// The deck rather than the request: its first panel is the approval queue,
		// which is the screen somebody woken by this actually needs, and it is one
		// URL that keeps working as the console's routes move.
		Link: n.link(""),
	})
}

// link builds an absolute console URL, or returns "" when no console is
// configured. A bare path would render as text in a chat message.
func (n *Notices) link(path string) string {
	if n.consoleURL == "" {
		return ""
	}
	return n.consoleURL + path
}

// send fires one notification and swallows the failure, logging it.
//
// Same trade-off as appendAudit, for the same reason: by the time this runs the
// decision it describes is durable, so returning an error would only make a caller
// retry something that must not be repeated. The log line carries the subject and
// whether a retry could have helped — never the message, which is the operator's
// business and not the log's.
func (n *Notices) send(ctx context.Context, req core.NotifyRequest) {
	if err := n.notifier.Notify(ctx, req); err != nil {
		n.log.Error().Err(err).
			Str("kind", string(req.Kind)).
			Str("subjectId", req.SubjectID).
			Bool("retryable", core.IsRetryable(err)).
			Msg("could not notify")
		return
	}
	n.log.Info().
		Str("kind", string(req.Kind)).
		Str("subjectId", req.SubjectID).
		Msg("notification sent")
}
