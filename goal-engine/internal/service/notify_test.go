package service

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/core"
)

func testNotices(t *testing.T, consoleURL string) (*Notices, *fakeNotifier) {
	t.Helper()
	notifier := &fakeNotifier{}
	return NewNotices(NoticesDeps{
		Notifier:       notifier,
		ConsoleBaseURL: consoleURL,
		Logger:         zerolog.Nop(),
	}), notifier
}

func TestNotices_triggerDispatched_namesTheGoalAndLinksToIt(t *testing.T) {
	// Arrange
	notices, notifier := testNotices(t, "https://console.wingman.test")

	// Act
	notices.TriggerDispatched(context.Background(), "goal-1")

	// Assert
	if len(notifier.sent) != 1 {
		t.Fatalf("expected one notification, got %d", len(notifier.sent))
	}
	sent := notifier.sent[0]
	if sent.Kind != core.NotifyTrigger {
		t.Fatalf("unexpected kind %q", sent.Kind)
	}
	if sent.SubjectID != "goal-1" {
		t.Fatalf("unexpected subject %q", sent.SubjectID)
	}
	if sent.Link != "https://console.wingman.test/goals/goal-1" {
		t.Fatalf("unexpected link %q", sent.Link)
	}
	if strings.TrimSpace(sent.Headline) == "" {
		t.Fatal("expected a headline")
	}
}

func TestNotices_approvalPending_carriesTheActionTypeAndTheDeckLink(t *testing.T) {
	notices, notifier := testNotices(t, "https://console.wingman.test")

	notices.ApprovalPending(context.Background(), "req-7", "ads_spend")

	if len(notifier.sent) != 1 {
		t.Fatalf("expected one notification, got %d", len(notifier.sent))
	}
	sent := notifier.sent[0]
	if sent.Kind != core.NotifyApprovalPending || sent.SubjectID != "req-7" {
		t.Fatalf("unexpected notification %+v", sent)
	}
	if !strings.Contains(sent.Headline, "ads_spend") {
		t.Fatalf("expected the action type in the headline, got %q", sent.Headline)
	}
	// The deck, not the approval: its first panel is the queue, which is the screen
	// somebody woken by this message actually needs.
	if sent.Link != "https://console.wingman.test" {
		t.Fatalf("unexpected link %q", sent.Link)
	}
}

func TestNotices_approvalPending_withNoActionType_stillReads(t *testing.T) {
	notices, notifier := testNotices(t, "")

	notices.ApprovalPending(context.Background(), "req-7", "   ")

	if len(notifier.sent) != 1 {
		t.Fatalf("expected one notification, got %d", len(notifier.sent))
	}
	if strings.Contains(notifier.sent[0].Headline, "()") {
		t.Fatalf("expected no empty parenthesis, got %q", notifier.sent[0].Headline)
	}
}

func TestNotices_carryNoAmountCurrencyOrPace(t *testing.T) {
	// The invariant this whole shape exists for. A message sits on a third party's
	// servers, and one complete enough to act on invites approving a spend from the
	// notification instead of from the console.
	notices, notifier := testNotices(t, "https://console.wingman.test")

	notices.ApprovalPending(context.Background(), "req-7", "ads_spend")
	notices.TriggerDispatched(context.Background(), "goal-1")

	for _, sent := range notifier.sent {
		text := sent.Headline
		for _, forbidden := range []string{"IDR", "USD", "%", "amount", "pace ratio"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("expected %q to stay out of a notification, got %q", forbidden, text)
			}
		}
		if strings.ContainsAny(text, "0123456789") {
			t.Fatalf("expected no figure in a notification headline, got %q", text)
		}
	}
}

func TestNotices_withNoConsoleURL_sendsNoLink(t *testing.T) {
	// WEB_BASE_URL is optional. A relative path would render as text in a chat
	// message, which is worse than saying nothing.
	notices, notifier := testNotices(t, "")

	notices.TriggerDispatched(context.Background(), "goal-1")

	if notifier.sent[0].Link != "" {
		t.Fatalf("expected no link, got %q", notifier.sent[0].Link)
	}
}

func TestNotices_nilIsTheDisabledCase(t *testing.T) {
	// NOTIFY_ENABLED=false means wiring passes nothing, and neither the monitor nor
	// the approval gate has a branch about messaging in it.
	var notices *Notices

	notices.TriggerDispatched(context.Background(), "goal-1")
	notices.ApprovalPending(context.Background(), "req-7", "ads_spend")
}

func TestNewNotices_withoutANotifier_isTheDisabledCase(t *testing.T) {
	if notices := NewNotices(NoticesDeps{Logger: zerolog.Nop()}); notices != nil {
		t.Fatal("expected no notices without something to send with")
	}
}

func TestNotices_aFailedSendIsSwallowed(t *testing.T) {
	// A notification is not a decision. By the time it is sent the decision is
	// already durable, and the caller must not be given anything to retry.
	notifier := &fakeNotifier{err: errBoom}
	notices := NewNotices(NoticesDeps{Notifier: notifier, Logger: zerolog.Nop()})

	notices.TriggerDispatched(context.Background(), "goal-1")

	if len(notifier.sent) != 1 {
		t.Fatalf("expected the send to have been attempted, got %d", len(notifier.sent))
	}
}
