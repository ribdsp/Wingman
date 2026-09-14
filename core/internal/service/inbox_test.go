package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/channel"
	"github.com/ribdsp/wingman/core/internal/domain"
)

// inboxFixture wires the inbound path over its four fakes and the kill switch.
//
// The clock is movable rather than fixed, because the throttle is the one decision here
// that is about the gap between two calls: a fixed clock can prove a second message is
// refused and cannot prove the first one stops being refused.
type inboxFixture struct {
	inbox      *Inbox
	identities *fakeIdentities
	codes      *fakeLinkCodes
	work       *fakeChannelWork
	halt       *fakeHalt

	now time.Time
}

func newInboxFixture(t *testing.T, tweaks ...func(*InboxDeps)) *inboxFixture {
	t.Helper()

	f := &inboxFixture{
		identities: newFakeIdentities(),
		codes:      newFakeLinkCodes(),
		work:       &fakeChannelWork{},
		halt:       &fakeHalt{},
		now:        testNow,
	}
	deps := InboxDeps{
		Identities: f.identities,
		Linker:     f.identities,
		Codes:      f.codes,
		Work:       f.work,
		Halt:       f.halt,
		Clock:      func() time.Time { return f.now },
		Logger:     silentLogger(),
	}
	for _, tweak := range tweaks {
		tweak(&deps)
	}

	inbox, err := NewInbox(deps)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	f.inbox = inbox
	return f
}

// withGroups is CHANNEL_ALLOW_GROUPS=true, and withInterval is CHANNEL_MIN_INTERVAL.
func withGroups(deps *InboxDeps) { deps.AllowGroups = true }

func withInterval(gap time.Duration) func(*InboxDeps) {
	return func(deps *InboxDeps) { deps.MinInterval = gap }
}

// handle runs one message through and fails the test on the error, which is reserved for
// an adapter bug. Every other outcome is a Handled with something to say.
func (f *inboxFixture) handle(t *testing.T, in channel.Inbound) channel.Handled {
	t.Helper()

	handled, err := f.inbox.Handle(context.Background(), in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return handled
}

// The sender every test uses unless it needs two.
const (
	testSenderID       = "tg-9001"
	testConversationID = "tg-chat-42"
)

// directMessage is one message in a private chat: the ordinary case, and the only one
// this instance acts on unless an operator allowed groups.
func directMessage(text string) channel.Inbound {
	return channel.Inbound{
		Kind:           domain.ChannelTelegram,
		SenderID:       testSenderID,
		SenderName:     "Rina",
		ConversationID: testConversationID,
		Text:           text,
		Direct:         true,
	}
}

// seedLinkCode stores a code for an account and returns it as the person was shown it —
// grouped, which is what they paste back. Minted through auth so the test travels the
// same path production does.
func seedLinkCode(t *testing.T, codes *fakeLinkCodes, userID string, expiresAt time.Time) string {
	t.Helper()

	plaintext, stored, err := auth.NewLinkCode()
	if err != nil {
		t.Fatalf("mint a link code: %v", err)
	}
	codes.seedCode(userID, stored, expiresAt)
	return domain.FormatLinkCode(plaintext)
}

// unknownLinkCode is a well-formed code that was never stored.
func unknownLinkCode(t *testing.T) string {
	t.Helper()

	plaintext, _, err := auth.NewLinkCode()
	if err != nil {
		t.Fatalf("mint a link code: %v", err)
	}
	return domain.FormatLinkCode(plaintext)
}

func TestNewInbox_namesEveryMissingDependencyAtOnce(t *testing.T) {
	// Act
	_, err := NewInbox(InboxDeps{})

	// Assert — Halt among them: an instance with no engine to ask is an instance whose
	// brakes cannot be read, and the rule for that is halt, not carry on.
	if err == nil {
		t.Fatal("err = nil, want a complaint about the wiring")
	}
	for _, name := range []string{"Identities", "Linker", "Codes", "Work", "Halt"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
}

func TestInboxHandle_refusesAMessageNoVerdictWouldMeanAnythingAbout(t *testing.T) {
	// Arrange — the only error this method returns, and it is about the adapter rather than
	// about the sender: no author means nothing to attach to an account, no conversation
	// means nowhere to answer, an unknown platform means nothing can deliver.
	cases := map[string]channel.Inbound{
		"no author":                           {Kind: domain.ChannelTelegram, ConversationID: testConversationID, Text: "hello", Direct: true},
		"nowhere to answer":                   {Kind: domain.ChannelTelegram, SenderID: testSenderID, Text: "hello", Direct: true},
		"a platform this build does not know": {Kind: "signal", SenderID: testSenderID, ConversationID: testConversationID, Text: "hello", Direct: true},
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInboxFixture(t)

			// Act
			_, err := f.inbox.Handle(context.Background(), in)

			// Assert
			if !errors.Is(err, channel.ErrIncomplete) {
				t.Fatalf("err = %v, want channel.ErrIncomplete", err)
			}
			if f.identities.resolveCalls != 0 || len(f.work.calls) != 0 {
				t.Error("an incomplete message reached the database")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Rungs 1 and 2 — nothing addressed to a person

func TestInboxHandle_fromAnotherApplicationIsIgnoredWithoutAskingAnybodyAnything(t *testing.T) {
	// Arrange — loop prevention is the first rung because every rung below it replies, and
	// two applications answering each other spends real money per exchange.
	f := newInboxFixture(t)
	in := directMessage("Wingman, run the report")
	in.FromBot = true

	// Act
	handled := f.handle(t, in)

	// Assert
	if handled.Verdict != domain.InboundIgnore {
		t.Fatalf("verdict = %q, want ignore", handled.Verdict)
	}
	if handled.Reply != "" {
		t.Errorf("reply = %q, want silence", handled.Reply)
	}
	if f.identities.resolveCalls != 0 {
		t.Errorf("resolve calls = %d, want the sender not looked up", f.identities.resolveCalls)
	}
	if f.halt.calls != 0 {
		t.Errorf("kill switch reads = %d, want the goal engine left alone", f.halt.calls)
	}
}

func TestInboxHandle_withNothingToActOnIsIgnored(t *testing.T) {
	// Arrange — a sticker, a photo with no caption, somebody joining a room.
	f := newInboxFixture(t)

	// Act
	handled := f.handle(t, directMessage("   \n\t "))

	// Assert
	if handled.Verdict != domain.InboundIgnore || handled.Reply != "" {
		t.Fatalf("handled = %+v, want a silent ignore", handled)
	}
	if !handled.Verdict.Silent() {
		t.Error("ignore is not silent, which contradicts the verdict's own rule")
	}
}

// ---------------------------------------------------------------------------
// Rung 3 — the room

func TestInboxHandle_inASharedRoomIsRefusedWithoutLookingUpTheSender(t *testing.T) {
	// Arrange — whether a shared room may be used at all is a fact about the room, not
	// about who is typing. Telling a stranger in a public channel how to link would invite
	// an account credential into a place other people can read.
	f := newInboxFixture(t)
	in := directMessage("Wingman, check yesterday's revenue")
	in.Direct = false

	// Act
	handled := f.handle(t, in)

	// Assert
	if handled.Verdict != domain.InboundGroupRefused {
		t.Fatalf("verdict = %q, want group_refused", handled.Verdict)
	}
	if handled.Reply != replyGroupRefused {
		t.Errorf("reply = %q, want the group refusal", handled.Reply)
	}
	if f.identities.resolveCalls != 0 {
		t.Errorf("resolve calls = %d, want the sender not looked up", f.identities.resolveCalls)
	}
}

func TestInboxHandle_inASharedRoomTheOperatorAllowedIsTreatedLikeAnyOther(t *testing.T) {
	// Arrange
	f := newInboxFixture(t, withGroups)
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)
	in := directMessage("Wingman, check yesterday's revenue")
	in.Direct = false

	// Act
	handled := f.handle(t, in)

	// Assert
	if handled.Verdict != domain.InboundAccept {
		t.Fatalf("verdict = %q, want accept", handled.Verdict)
	}
}

// ---------------------------------------------------------------------------
// Rung 4 — the sender, and the one thing an unlinked one may reach

func TestInboxHandle_fromAStrangerAsksForACodeAndQueuesNothing(t *testing.T) {
	// Arrange
	f := newInboxFixture(t)

	// Act
	handled := f.handle(t, directMessage("Wingman, check yesterday's revenue"))

	// Assert
	if handled.Verdict != domain.InboundLinkRequired {
		t.Fatalf("verdict = %q, want link_required", handled.Verdict)
	}
	if handled.Reply != replyLinkRequired {
		t.Errorf("reply = %q, want the linking instructions", handled.Reply)
	}
	if len(f.work.calls) != 0 {
		t.Error("a stranger's message was queued as work")
	}
	if f.halt.calls != 0 {
		t.Errorf("kill switch reads = %d, want no request for a message that stops at rung 4", f.halt.calls)
	}
}

func TestInboxHandle_fromARevokedSenderIsAStrangerAgain(t *testing.T) {
	// Arrange — somebody who unlinked withdrew permission, so the row is there and it is
	// not live. That is the whole point of the route that revokes one.
	f := newInboxFixture(t)
	f.identities.seedRevoked("user-1", domain.ChannelTelegram, testSenderID)

	// Act
	handled := f.handle(t, directMessage("Wingman, check yesterday's revenue"))

	// Assert
	if handled.Verdict != domain.InboundLinkRequired {
		t.Fatalf("verdict = %q, want link_required", handled.Verdict)
	}
	if len(f.work.calls) != 0 {
		t.Error("a revoked sender's message was queued as work")
	}
}

func TestInboxHandle_aLiveCodeConnectsTheChatAccountThatSentIt(t *testing.T) {
	// Arrange — pasted in the grouped form a person is shown, which the ladder normalises.
	f := newInboxFixture(t)
	code := seedLinkCode(t, f.codes, "user-1", testNow.Add(15*time.Minute))

	// Act
	handled := f.handle(t, directMessage(code))

	// Assert
	if handled.Verdict != domain.InboundLinkAttempt {
		t.Fatalf("verdict = %q, want link_attempt", handled.Verdict)
	}
	if handled.Reply != replyLinked {
		t.Errorf("reply = %q, want the connected confirmation", handled.Reply)
	}
	if f.codes.consumed != 1 {
		t.Errorf("consumed = %d, want the code spent exactly once", f.codes.consumed)
	}

	identity := f.identities.find(domain.ChannelTelegram, testSenderID)
	if identity == nil {
		t.Fatal("no identity was recorded for the sender")
	}
	// The account comes from the code, and the chat account from the message. Neither is
	// read off anything the sender chose to say.
	if identity.UserID != "user-1" {
		t.Errorf("userId = %q, want the code's owner", identity.UserID)
	}
	if identity.DisplayName != "Rina" {
		t.Errorf("displayName = %q, want the platform's name for the sender", identity.DisplayName)
	}
	if !identity.Live() {
		t.Error("the new identity is not live")
	}
	// Linking does not start a run. Somebody who has just connected has not asked for
	// anything yet.
	if len(f.work.calls) != 0 {
		t.Error("connecting a chat account queued work")
	}
}

func TestInboxHandle_aCodeThatWillNotWorkGetsOneAnswerWhicheverWayItFailed(t *testing.T) {
	// Arrange — unknown, expired and already spent are one answer. A sender who can tell
	// them apart has been handed the difference between "there is no such code" and "there
	// was one, and you are a minute late".
	spent := func(t *testing.T, f *inboxFixture) string {
		code := seedLinkCode(t, f.codes, "user-1", testNow.Add(time.Hour))
		canonical, _ := domain.NormaliseLinkCode(code)
		if _, err := f.codes.Consume(context.Background(), auth.HashToken(canonical), testNow); err != nil {
			t.Fatalf("spend the seeded code: %v", err)
		}
		return code
	}
	cases := map[string]func(*testing.T, *inboxFixture) string{
		"never existed": func(t *testing.T, _ *inboxFixture) string { return unknownLinkCode(t) },
		"expired": func(t *testing.T, f *inboxFixture) string {
			return seedLinkCode(t, f.codes, "user-1", testNow.Add(-time.Second))
		},
		"already used": spent,
	}

	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInboxFixture(t)
			code := arrange(t, f)

			// Act
			handled := f.handle(t, directMessage(code))

			// Assert
			if handled.Reply != replyCodeRefused {
				t.Errorf("reply = %q, want the one refusal every failure gets", handled.Reply)
			}
			if f.identities.find(domain.ChannelTelegram, testSenderID) != nil {
				t.Error("a refused code still connected the chat account")
			}
		})
	}
}

func TestInboxHandle_aCodeWithAnythingElseAroundItIsNotACode(t *testing.T) {
	// Arrange — the whole message has to be the code. Scanning prose for something
	// code-shaped is a rule nobody can predict, and a false positive spends a live
	// credential that may not belong to the sender.
	f := newInboxFixture(t)
	code := seedLinkCode(t, f.codes, "user-1", testNow.Add(time.Hour))

	// Act
	handled := f.handle(t, directMessage("hi! "+code))

	// Assert
	if handled.Verdict != domain.InboundLinkRequired {
		t.Fatalf("verdict = %q, want link_required", handled.Verdict)
	}
	if f.codes.consumed != 0 {
		t.Error("a code inside a sentence was spent")
	}
}

func TestInboxHandle_aCodeFromTheAccountThatReleasedTheChatReconnectsIt(t *testing.T) {
	// Arrange — the same person picking their own phone back up. Link conflicts on the
	// chat account whether or not the row is live, which is what sends this to Relink.
	f := newInboxFixture(t)
	revoked := f.identities.seedRevoked("user-1", domain.ChannelTelegram, testSenderID)
	code := seedLinkCode(t, f.codes, "user-1", testNow.Add(time.Hour))

	// Act
	handled := f.handle(t, directMessage(code))

	// Assert
	if handled.Reply != replyRelinked {
		t.Fatalf("reply = %q, want the reconnected confirmation", handled.Reply)
	}
	stored := f.identities.identities[revoked.ID]
	if !stored.Live() {
		t.Error("the released identity was not picked up again")
	}
	if stored.UserID != "user-1" {
		t.Errorf("userId = %q, want the same account that released it", stored.UserID)
	}
}

func TestInboxHandle_aCodeForAChatAccountAnotherAccountHoldsNamesNobody(t *testing.T) {
	// Arrange — the chat account was connected to somebody else and released. Relink is
	// scoped to the account that just redeemed the code, so it finds nothing.
	f := newInboxFixture(t)
	held := f.identities.seedRevoked("user-2", domain.ChannelTelegram, testSenderID)
	code := seedLinkCode(t, f.codes, "user-1", testNow.Add(time.Hour))

	// Act
	handled := f.handle(t, directMessage(code))

	// Assert
	if handled.Reply != replyLinkHeld {
		t.Fatalf("reply = %q, want the held-elsewhere answer", handled.Reply)
	}
	// Which Wingman account a chat account was once attached to is not this sender's
	// business, and this sender may be the one trying to find out.
	if strings.Contains(handled.Reply, "user-2") {
		t.Error("the reply names the account that holds the chat account")
	}
	stored := f.identities.identities[held.ID]
	if stored.UserID != "user-2" || stored.Live() {
		t.Error("somebody else's released identity was taken over")
	}
	// The code is spent either way, which is what single use means.
	if f.codes.consumed != 1 {
		t.Errorf("consumed = %d, want the code spent even though the link failed", f.codes.consumed)
	}
}

// ---------------------------------------------------------------------------
// Rung 5 — the throttle

func TestInboxHandle_aSecondMessageInsideTheIntervalIsThrottledSilently(t *testing.T) {
	// Arrange — a reply to every throttled message is itself a flood, and one this side
	// pays to send.
	f := newInboxFixture(t, withInterval(time.Minute))
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)
	f.handle(t, directMessage("Check yesterday's revenue"))

	// Act
	f.now = f.now.Add(30 * time.Second)
	handled := f.handle(t, directMessage("And the day before"))

	// Assert
	if handled.Verdict != domain.InboundThrottled {
		t.Fatalf("verdict = %q, want throttled", handled.Verdict)
	}
	if handled.Reply != "" {
		t.Errorf("reply = %q, want silence", handled.Reply)
	}
	if len(f.work.calls) != 1 {
		t.Errorf("queued = %d, want only the first message", len(f.work.calls))
	}
}

func TestInboxHandle_onceTheIntervalHasPassedTheSameSenderIsAcceptedAgain(t *testing.T) {
	// Arrange
	f := newInboxFixture(t, withInterval(time.Minute))
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)
	f.handle(t, directMessage("Check yesterday's revenue"))

	// Act
	f.now = f.now.Add(time.Minute + time.Second)
	handled := f.handle(t, directMessage("And the day before"))

	// Assert
	if handled.Verdict != domain.InboundAccept {
		t.Fatalf("verdict = %q, want accept", handled.Verdict)
	}
	if len(f.work.calls) != 2 {
		t.Errorf("queued = %d, want both messages", len(f.work.calls))
	}
}

func TestInboxHandle_throttlesASenderAcrossConversations(t *testing.T) {
	// Arrange — the same person talking in two rooms is one sender spending one budget, so
	// the throttle is keyed by the platform's id for them and not by the conversation.
	f := newInboxFixture(t, withInterval(time.Minute))
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)
	f.handle(t, directMessage("Check yesterday's revenue"))

	elsewhere := directMessage("Check it again")
	elsewhere.ConversationID = "tg-chat-77"

	// Act
	handled := f.handle(t, elsewhere)

	// Assert
	if handled.Verdict != domain.InboundThrottled {
		t.Fatalf("verdict = %q, want throttled", handled.Verdict)
	}
}

func TestInboxHandle_aSecondSenderIsNotThrottledByTheFirst(t *testing.T) {
	// Arrange
	f := newInboxFixture(t, withInterval(time.Minute))
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)
	f.identities.seed("user-2", domain.ChannelTelegram, "tg-9002")
	f.handle(t, directMessage("Check yesterday's revenue"))

	other := directMessage("Check mine too")
	other.SenderID = "tg-9002"
	other.ConversationID = "tg-chat-43"

	// Act
	handled := f.handle(t, other)

	// Assert
	if handled.Verdict != domain.InboundAccept {
		t.Fatalf("verdict = %q, want accept", handled.Verdict)
	}
}

func TestInboxRecordAccepted_forgetsEverybodyRatherThanGrowingWithoutBound(t *testing.T) {
	// Arrange — the map is keyed by a value strangers control, so it needs a ceiling:
	// anybody who can find the bot can add a key by sending it one message.
	f := newInboxFixture(t)
	for i := 0; i < maxTrackedSenders; i++ {
		f.inbox.recordAccepted("telegram:"+strconv.Itoa(i), testNow)
	}

	// Act
	f.inbox.recordAccepted("telegram:one-too-many", testNow)

	// Assert — the cost of forgetting is one extra accepted message per sender; the cost of
	// not forgetting is a process somebody has to restart.
	if got := len(f.inbox.accepted); got != 1 {
		t.Fatalf("tracked senders = %d, want the map emptied and the new one kept", got)
	}
	if f.inbox.lastAccepted("telegram:one-too-many").IsZero() {
		t.Error("the sender that triggered the reset was not remembered")
	}
}

// ---------------------------------------------------------------------------
// Rung 6 — longer than a brief may be

func TestInboxHandle_aMessageLongerThanABriefIsRefusedBeforeTheBrakesAreRead(t *testing.T) {
	// Arrange — refused rather than truncated: a brief cut in half is an instruction whose
	// second half the agent never sees.
	f := newInboxFixture(t)
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)

	// Act
	handled := f.handle(t, directMessage(strings.Repeat("é", domain.MaxBriefLength+1)))

	// Assert
	if handled.Verdict != domain.InboundTooLong {
		t.Fatalf("verdict = %q, want too_long", handled.Verdict)
	}
	// The limit is in the reply, because "shorter" without a number is not something
	// somebody can act on.
	if !strings.Contains(handled.Reply, fmt.Sprint(domain.MaxBriefLength)) {
		t.Errorf("reply = %q, want it to say the limit", handled.Reply)
	}
	if f.halt.calls != 0 {
		t.Errorf("kill switch reads = %d, want no request for a message that stops at rung 6", f.halt.calls)
	}
	if len(f.work.calls) != 0 {
		t.Error("an oversized message was queued")
	}
}

func TestInboxHandle_aMessageExactlyAtTheLimitIsAccepted(t *testing.T) {
	// Arrange — the boundary belongs to the accepted side, which is what MaxBriefLength
	// means everywhere else it is checked.
	f := newInboxFixture(t)
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)

	// Act
	handled := f.handle(t, directMessage(strings.Repeat("a", domain.MaxBriefLength)))

	// Assert
	if handled.Verdict != domain.InboundAccept {
		t.Fatalf("verdict = %q, want accept", handled.Verdict)
	}
}

// ---------------------------------------------------------------------------
// Rung 7 — the brakes

func TestInboxHandle_whenTheKillSwitchIsEngagedNothingIsQueued(t *testing.T) {
	// Arrange
	f := newInboxFixture(t)
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)
	f.halt.engaged = true

	// Act
	handled := f.handle(t, directMessage("Check yesterday's revenue"))

	// Assert
	if handled.Verdict != domain.InboundHalted {
		t.Fatalf("verdict = %q, want halted", handled.Verdict)
	}
	if handled.Reply != replyHalted {
		t.Errorf("reply = %q, want the halted answer", handled.Reply)
	}
	if len(f.work.calls) != 0 {
		t.Error("work was queued while the instance was halted")
	}
}

func TestInboxHandle_whenTheKillSwitchCannotBeReadNothingIsQueued(t *testing.T) {
	// Arrange — an error is not a false. Unreadable means engaged, the same rule the run
	// ladder and the goal engine both apply.
	f := newInboxFixture(t)
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)
	f.halt.fail = errBoom

	// Act
	handled := f.handle(t, directMessage("Check yesterday's revenue"))

	// Assert
	if handled.Verdict != domain.InboundHalted {
		t.Fatalf("verdict = %q, want halted", handled.Verdict)
	}
	if len(f.work.calls) != 0 {
		t.Error("work was queued against an unreadable kill switch")
	}
}

func TestInboxHandle_servesSomebodyMidConnectionEvenWhileHalted(t *testing.T) {
	// Arrange — a deliberate consequence of the rung order: linking spends nothing and
	// starts no run, and answering somebody mid-connection with silence gives them nothing
	// to act on.
	f := newInboxFixture(t)
	f.halt.engaged = true
	code := seedLinkCode(t, f.codes, "user-1", testNow.Add(time.Hour))

	// Act
	handled := f.handle(t, directMessage(code))

	// Assert
	if handled.Reply != replyLinked {
		t.Fatalf("reply = %q, want the connection to go through", handled.Reply)
	}
	if f.halt.calls != 0 {
		t.Errorf("kill switch reads = %d, want no request for a link attempt", f.halt.calls)
	}
}

// ---------------------------------------------------------------------------
// Accept

func TestInboxHandle_anAcceptedMessageIsFiledAgainstTheResolvedOwnerAndSaysNothing(t *testing.T) {
	// Arrange
	f := newInboxFixture(t)
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)

	// Act
	handled := f.handle(t, directMessage("Check yesterday's revenue"))

	// Assert
	if handled.Verdict != domain.InboundAccept {
		t.Fatalf("verdict = %q, want accept", handled.Verdict)
	}
	// Nothing is said back: the answer is the run's, and an acknowledgement here would
	// double the traffic on every message to say what the next message already says.
	if handled.Reply != "" {
		t.Errorf("reply = %q, want the run to do the talking", handled.Reply)
	}

	if len(f.work.calls) != 1 {
		t.Fatalf("queued = %d, want one", len(f.work.calls))
	}
	queued := f.work.calls[0]
	// The owner is the identity's, never anything in the message. That is what keeps this
	// entry point from being a way to name somebody else's account and spend their budget.
	if queued.UserID != "user-1" {
		t.Errorf("userId = %q, want the resolved owner", queued.UserID)
	}
	if queued.Kind != domain.ChannelTelegram || queued.ConversationID != testConversationID {
		t.Errorf("destination = %s/%s, want where the message came from", queued.Kind, queued.ConversationID)
	}
	if queued.Text != "Check yesterday's revenue" {
		t.Errorf("text = %q, want the message as it arrived", queued.Text)
	}
}

func TestInboxHandle_recordsTheThrottleEvenWhenQueueingTheWorkFails(t *testing.T) {
	// Arrange — the throttle bounds how often a stranger can make this side do expensive
	// things, and an attempt that failed halfway through cost that anyway.
	f := newInboxFixture(t, withInterval(time.Minute))
	f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)
	f.work.fail = errBoom

	// Act
	first := f.handle(t, directMessage("Check yesterday's revenue"))
	second := f.handle(t, directMessage("Check it again"))

	// Assert
	if first.Reply != replyUnavailable {
		t.Errorf("reply = %q, want an apology", first.Reply)
	}
	if second.Verdict != domain.InboundThrottled {
		t.Errorf("verdict = %q, want the failed attempt to have counted", second.Verdict)
	}
}

// ---------------------------------------------------------------------------
// When this side is what is broken

func TestInboxHandle_aDependencyFailureApologisesAndReturnsNoError(t *testing.T) {
	// Arrange — an adapter has nothing to do with an error except log it, and there is a
	// person waiting. What goes back names nothing: a person on Telegram cannot act on a
	// constraint name, and the failure may be about somebody else's row.
	cases := map[string]struct {
		text    func(*testing.T, *inboxFixture) string
		arrange func(*inboxFixture)
	}{
		"resolving the sender": {
			text:    func(t *testing.T, _ *inboxFixture) string { return unknownLinkCode(t) },
			arrange: func(f *inboxFixture) { f.identities.failResolve = errBoom },
		},
		"redeeming the code": {
			text: func(t *testing.T, f *inboxFixture) string {
				return seedLinkCode(t, f.codes, "user-1", testNow.Add(time.Hour))
			},
			arrange: func(f *inboxFixture) { f.codes.failConsume = errBoom },
		},
		"linking the sender": {
			text: func(t *testing.T, f *inboxFixture) string {
				return seedLinkCode(t, f.codes, "user-1", testNow.Add(time.Hour))
			},
			arrange: func(f *inboxFixture) { f.identities.failLink = errBoom },
		},
		"reconnecting the sender": {
			text: func(t *testing.T, f *inboxFixture) string {
				f.identities.seedRevoked("user-1", domain.ChannelTelegram, testSenderID)
				return seedLinkCode(t, f.codes, "user-1", testNow.Add(time.Hour))
			},
			arrange: func(f *inboxFixture) { f.identities.failRelink = errBoom },
		},
		"queueing the work": {
			text: func(_ *testing.T, f *inboxFixture) string {
				f.identities.seed("user-1", domain.ChannelTelegram, testSenderID)
				return "Check yesterday's revenue"
			},
			arrange: func(f *inboxFixture) { f.work.fail = errBoom },
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInboxFixture(t)
			text := tc.text(t, f)
			tc.arrange(f)

			// Act
			handled, err := f.inbox.Handle(context.Background(), directMessage(text))

			// Assert
			if err != nil {
				t.Fatalf("Handle: %v, want the failure kept on this side", err)
			}
			if handled.Reply != replyUnavailable {
				t.Errorf("reply = %q, want the apology", handled.Reply)
			}
			if strings.Contains(handled.Reply, "boom") {
				t.Error("the injected failure was quoted at the sender")
			}
		})
	}
}

func TestInboxHandle_whenTheSenderCannotBeResolvedItApologisesInsteadOfAskingForACode(t *testing.T) {
	// Arrange — running the ladder with Linked false would tell somebody who *is* linked to
	// go and get a code, and send them to mint one from a database that is equally
	// unreachable.
	f := newInboxFixture(t)
	f.identities.failResolve = errBoom
	code := seedLinkCode(t, f.codes, "user-1", testNow.Add(time.Hour))

	// Act
	handled := f.handle(t, directMessage(code))

	// Assert
	if handled.Reply == replyLinkRequired {
		t.Fatal("an unreadable identity was answered as an unlinked sender")
	}
	if handled.Reply != replyUnavailable {
		t.Errorf("reply = %q, want the apology", handled.Reply)
	}
	// And the code the sender pasted is still good: nothing was spent during the outage.
	if f.codes.consumed != 0 {
		t.Errorf("consumed = %d, want the code left alone", f.codes.consumed)
	}
	if len(f.work.calls) != 0 {
		t.Error("work was queued for a sender nobody could identify")
	}
}

func TestInboxAct_withAVerdictItDoesNotKnowQueuesNothing(t *testing.T) {
	// Arrange — a verdict the switch does not know is a rung somebody added without
	// deciding what it does. Saying nothing useful is the safe half of that mistake;
	// queueing work would be the other one.
	f := newInboxFixture(t)

	// Act
	handled := f.inbox.act(context.Background(), directMessage("Check yesterday's revenue"),
		f.identities.seed("user-1", domain.ChannelTelegram, testSenderID),
		domain.InboundDecision{Verdict: "a_rung_nobody_wired_up", DecidedAt: testNow})

	// Assert
	if handled.Reply != replyUnavailable {
		t.Errorf("reply = %q, want the apology", handled.Reply)
	}
	if len(f.work.calls) != 0 {
		t.Error("an unknown verdict queued work")
	}
}
