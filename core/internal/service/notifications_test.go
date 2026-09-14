package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/repository"
)

// The account CORE_UNATTENDED_OWNER names, in these tests. Every notification goes to
// whoever this is, and to nobody else: there is no recipient in the request.
const notifyOwner = "user-owner"

// notificationsFixture wires the notifier over the identity fake and the sender fake, both
// kept to hand. What was looked up is one assertion and what was delivered is the other.
type notificationsFixture struct {
	notifications *Notifications
	identities    *fakeIdentities
	sender        *fakeDirectSender
}

func newNotificationsFixture(t *testing.T, ownerID string) notificationsFixture {
	t.Helper()

	identities := newFakeIdentities()
	sender := &fakeDirectSender{failOn: map[repository.ChannelKind]error{}}
	notifications, err := NewNotifications(NotificationsDeps{
		Directory: identities,
		Sender:    sender,
		OwnerID:   ownerID,
		Logger:    silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewNotifications: %v", err)
	}
	return notificationsFixture{notifications: notifications, identities: identities, sender: sender}
}

// triggerNotice is the shape the goal engine sends, with the wording it sends: what
// happened and which goal, and no figure of any kind.
func triggerNotice() Notice {
	return Notice{
		Kind:      NoticeTrigger,
		SubjectID: "goal-7",
		Headline:  "A goal fell behind and an agent was dispatched.",
		Link:      "https://wingman.example/goals/goal-7",
	}
}

func TestNewNotifications_namesEveryMissingDependencyAtOnce(t *testing.T) {
	// Act
	_, err := NewNotifications(NotificationsDeps{})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want a complaint about the wiring")
	}
	for _, name := range []string{"Directory", "Sender"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
}

func TestNewNotifications_withoutAnOwnerIsStillWired(t *testing.T) {
	// Arrange — an instance with no CORE_UNATTENDED_OWNER. It is a valid instance: an
	// operator who never set one simply has nobody to tell, and refusing to boot over a
	// notification would take the whole service down for the least important thing in it.

	// Act
	notifications, err := NewNotifications(NotificationsDeps{
		Directory: newFakeIdentities(),
		Sender:    &fakeDirectSender{},
		Logger:    silentLogger(),
	})

	// Assert
	if err != nil {
		t.Fatalf("NewNotifications: %v", err)
	}
	if notifications == nil {
		t.Fatal("notifications = nil, want a service")
	}
}

func TestNotify_reachesEveryPlatformTheOwnerLinked(t *testing.T) {
	// Arrange — the owner on two platforms, and the goal engine's own key as the caller.
	f := newNotificationsFixture(t, notifyOwner)
	f.identities.seed(notifyOwner, repository.ChannelTelegram, "987654321")
	f.identities.seed(notifyOwner, repository.ChannelDiscord, "555000111")
	notice := triggerNotice()

	// Act
	delivered, err := f.notifications.Notify(context.Background(), notice, botActor())

	// Assert
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if delivered.Recipients != 2 {
		t.Errorf("Recipients = %d, want 2", delivered.Recipients)
	}
	telegram := f.sender.sentOn(repository.ChannelTelegram)
	if telegram.externalUserID != "987654321" {
		t.Errorf("telegram recipient = %q, want the linked account's own id", telegram.externalUserID)
	}
	// The headline as written, then the link on a line of its own. Core adds no wording of
	// its own: the engine decided what may leave the box, and rephrasing it here would put
	// that decision in two places.
	want := notice.Headline + "\n" + notice.Link
	if telegram.text != want {
		t.Errorf("text = %q, want %q", telegram.text, want)
	}
	if discord := f.sender.sentOn(repository.ChannelDiscord); discord.externalUserID != "555000111" {
		t.Errorf("discord recipient = %q, want the linked account's own id", discord.externalUserID)
	}
}

func TestNotify_withoutALinkSendsTheHeadlineAlone(t *testing.T) {
	// Arrange — WEB_BASE_URL unset on the engine, so there is nothing to link to.
	f := newNotificationsFixture(t, notifyOwner)
	f.identities.seed(notifyOwner, repository.ChannelTelegram, "987654321")
	notice := triggerNotice()
	notice.Link = ""

	// Act
	delivered, err := f.notifications.Notify(context.Background(), notice, botActor())

	// Assert
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if delivered.Recipients != 1 {
		t.Errorf("Recipients = %d, want 1", delivered.Recipients)
	}
	if got := f.sender.sentOn(repository.ChannelTelegram).text; got != notice.Headline {
		t.Errorf("text = %q, want the headline with no trailing blank line", got)
	}
}

func TestNotify_fromASignedInPersonIsRefused(t *testing.T) {
	// Arrange — somebody's own session. A person must not be able to make core message the
	// operator's Telegram, which is the same rule that keeps them from dispatching a task.
	f := newNotificationsFixture(t, notifyOwner)
	f.identities.seed(notifyOwner, repository.ChannelTelegram, "987654321")

	// Act
	_, err := f.notifications.Notify(context.Background(), triggerNotice(), userActor("user-someone"))

	// Assert
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if len(f.sender.calls) != 0 {
		t.Errorf("sends = %d, want none: refused before anything was delivered", len(f.sender.calls))
	}
}

func TestNotify_skipsAChatAccountSomebodyDisconnected(t *testing.T) {
	// Arrange — one live link and one the owner revoked. A revoked identity is somebody
	// asking not to be messaged there again, and ListForUser returns those rows.
	f := newNotificationsFixture(t, notifyOwner)
	f.identities.seedRevoked(notifyOwner, repository.ChannelSlack, "U-GONE")
	f.identities.seed(notifyOwner, repository.ChannelTelegram, "987654321")

	// Act
	delivered, err := f.notifications.Notify(context.Background(), triggerNotice(), operatorActor())

	// Assert
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if delivered.Recipients != 1 {
		t.Errorf("Recipients = %d, want 1", delivered.Recipients)
	}
	if sent := f.sender.sentOn(repository.ChannelSlack); sent.kind != "" {
		t.Errorf("slack was sent %q, want nothing: the link was revoked", sent.text)
	}
}

func TestNotify_withNothingLinkedSucceedsWithNobodyToTell(t *testing.T) {
	// Arrange — an owner who has connected no chat account. Ordinary: notifications are on
	// and the linking is a separate one-time step somebody has not done yet.
	f := newNotificationsFixture(t, notifyOwner)

	// Act
	delivered, err := f.notifications.Notify(context.Background(), triggerNotice(), botActor())

	// Assert — not an error. A retry would find the same empty list, and the engine drops
	// the answer anyway; core's own log is where an operator sees this.
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if delivered.Recipients != 0 {
		t.Errorf("Recipients = %d, want 0", delivered.Recipients)
	}
	if len(f.sender.calls) != 0 {
		t.Errorf("sends = %d, want none", len(f.sender.calls))
	}
}

func TestNotify_withNoOwnerConfiguredLooksNobodyUp(t *testing.T) {
	// Arrange — CORE_UNATTENDED_OWNER unset. The directory is rigged to fail, so a lookup
	// that happened anyway would show up as an error rather than as a silent extra query.
	f := newNotificationsFixture(t, "")
	f.identities.failList = errBoom

	// Act
	delivered, err := f.notifications.Notify(context.Background(), triggerNotice(), botActor())

	// Assert
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if delivered.Recipients != 0 {
		t.Errorf("Recipients = %d, want 0", delivered.Recipients)
	}
}

func TestNotify_whenOnePlatformFailsTheOthersStillGetIt(t *testing.T) {
	// Arrange — Telegram is down, Discord is not.
	f := newNotificationsFixture(t, notifyOwner)
	f.identities.seed(notifyOwner, repository.ChannelTelegram, "987654321")
	f.identities.seed(notifyOwner, repository.ChannelDiscord, "555000111")
	f.sender.failOn[repository.ChannelTelegram] = errBoom

	// Act
	delivered, err := f.notifications.Notify(context.Background(), triggerNotice(), botActor())

	// Assert — the one that worked is reported, and a partial delivery is not a failure:
	// the operator was told.
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if delivered.Recipients != 1 {
		t.Errorf("Recipients = %d, want 1", delivered.Recipients)
	}
	if len(f.sender.calls) != 2 {
		t.Errorf("sends attempted = %d, want 2: a failure on one platform stops nothing", len(f.sender.calls))
	}
}

func TestNotify_whenEveryDeliveryFailsSaysSo(t *testing.T) {
	// Arrange — both platforms refuse. Nobody was told, and the engine's log line should
	// say that rather than claiming a delivery.
	f := newNotificationsFixture(t, notifyOwner)
	f.identities.seed(notifyOwner, repository.ChannelTelegram, "987654321")
	f.identities.seed(notifyOwner, repository.ChannelDiscord, "555000111")
	f.sender.failOn[repository.ChannelTelegram] = errBoom
	f.sender.failOn[repository.ChannelDiscord] = errBoom

	// Act
	delivered, err := f.notifications.Notify(context.Background(), triggerNotice(), botActor())

	// Assert
	if err == nil {
		t.Fatal("err = nil, want a failure: nothing was delivered")
	}
	// Not a sentinel, so the handler renders it as a 500 — which is what it is: core could
	// not do the thing it was asked to do.
	for _, sentinel := range []error{ErrValidation, ErrNotFound, ErrForbidden} {
		if errors.Is(err, sentinel) {
			t.Errorf("err = %v, want a plain failure rather than %v", err, sentinel)
		}
	}
	if delivered.Recipients != 0 {
		t.Errorf("Recipients = %d, want 0", delivered.Recipients)
	}
}

func TestNotify_whenTheDirectoryCannotBeReadSaysSo(t *testing.T) {
	// Arrange
	f := newNotificationsFixture(t, notifyOwner)
	f.identities.failList = errBoom

	// Act
	_, err := f.notifications.Notify(context.Background(), triggerNotice(), botActor())

	// Assert — a database that cannot be read is not "nobody is linked".
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the directory's failure", err)
	}
	if len(f.sender.calls) != 0 {
		t.Errorf("sends = %d, want none", len(f.sender.calls))
	}
}

func TestNotify_refusesANoticeThatSaysNothingUseful(t *testing.T) {
	// Arrange — every way the engine could send something unusable.
	cases := map[string]Notice{
		"no kind":        {SubjectID: "goal-7", Headline: "A goal fell behind."},
		"unknown kind":   {Kind: "escalation", SubjectID: "goal-7", Headline: "A goal fell behind."},
		"no subject":     {Kind: NoticeTrigger, Headline: "A goal fell behind."},
		"no headline":    {Kind: NoticeTrigger, SubjectID: "goal-7"},
		"blank headline": {Kind: NoticeTrigger, SubjectID: "goal-7", Headline: "   "},
		"long headline": {Kind: NoticeTrigger, SubjectID: "goal-7",
			Headline: strings.Repeat("a", maxNoticeHeadlineLength+1)},
		"long subject": {Kind: NoticeTrigger, Headline: "A goal fell behind.",
			SubjectID: strings.Repeat("g", maxNoticeSubjectLength+1)},
	}

	for name, notice := range cases {
		t.Run(name, func(t *testing.T) {
			f := newNotificationsFixture(t, notifyOwner)
			f.identities.seed(notifyOwner, repository.ChannelTelegram, "987654321")

			// Act
			_, err := f.notifications.Notify(context.Background(), notice, botActor())

			// Assert
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if len(f.sender.calls) != 0 {
				t.Errorf("sends = %d, want none", len(f.sender.calls))
			}
		})
	}
}

func TestNotify_refusesALinkThatIsNotAWebAddress(t *testing.T) {
	// Arrange — the link is the one part of a notification a person is invited to act on.
	// A message from Wingman carrying a javascript: or file: link is a phishing message
	// that arrives with this instance's credibility attached to it.
	for _, link := range []string{
		"javascript:alert(1)",
		"file:///etc/passwd",
		"wingman.example/goals/goal-7",
		"//wingman.example/goals/goal-7",
	} {
		t.Run(link, func(t *testing.T) {
			f := newNotificationsFixture(t, notifyOwner)
			f.identities.seed(notifyOwner, repository.ChannelTelegram, "987654321")
			notice := triggerNotice()
			notice.Link = link

			// Act
			_, err := f.notifications.Notify(context.Background(), notice, botActor())

			// Assert
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if len(f.sender.calls) != 0 {
				t.Errorf("sends = %d, want none", len(f.sender.calls))
			}
		})
	}
}

func TestNotify_sendsSomebodyElsesLinksNowhere(t *testing.T) {
	// Arrange — another account has a chat account linked; the owner has none. The lookup
	// is scoped to the owner, so this is the test that a notification cannot land in a
	// stranger's DMs because they happened to link first.
	f := newNotificationsFixture(t, notifyOwner)
	f.identities.seed("user-somebody-else", repository.ChannelTelegram, "111222333")

	// Act
	delivered, err := f.notifications.Notify(context.Background(), triggerNotice(), botActor())

	// Assert
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if delivered.Recipients != 0 {
		t.Errorf("Recipients = %d, want 0", delivered.Recipients)
	}
	if len(f.sender.calls) != 0 {
		t.Errorf("sends = %d, want none", len(f.sender.calls))
	}
}

func TestNoticeKind_isAClosedSet(t *testing.T) {
	// Arrange
	cases := map[NoticeKind]bool{
		NoticeTrigger:         true,
		NoticeApprovalPending: true,
		"":                    false,
		"trigger ":            false,
		"approved":            false,
	}

	for kind, want := range cases {
		// Act
		got := kind.Valid()

		// Assert
		if got != want {
			t.Errorf("NoticeKind(%q).Valid() = %v, want %v", kind, got, want)
		}
	}
}
