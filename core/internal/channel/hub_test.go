package channel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// errSendFailed stands in for a dropped connection: the thing that happens to a channel
// halfway through delivering a long answer.
var errSendFailed = errors.New("boom: the connection dropped")

func testLogger() zerolog.Logger { return zerolog.Nop() }

// fakeChannel is one connected platform. Its Send records before it decides to fail, so a
// test can tell "never attempted" from "attempted and refused" — the difference between the
// hub stopping early and the hub not stopping at all.
type fakeChannel struct {
	mu    sync.Mutex
	kind  domain.ChannelKind
	sends []Reply

	// failOn is the 1-based send that fails. Zero never fails.
	failOn   int
	runErr   error
	closeErr error
	closes   int

	// directs records who DirectTarget was asked about, and directErr fails it. The
	// conversation it hands back is deliberately not the id it was given, because on Discord
	// it is not: a caller that used the user id as a conversation must fail a test.
	directs   []string
	directErr error

	started chan struct{}
}

func newFakeChannel(kind domain.ChannelKind) *fakeChannel {
	return &fakeChannel{kind: kind, started: make(chan struct{})}
}

func (c *fakeChannel) Kind() domain.ChannelKind { return c.kind }

func (c *fakeChannel) Run(ctx context.Context) error {
	close(c.started)
	if c.runErr != nil {
		return c.runErr
	}
	<-ctx.Done()
	return ctx.Err()
}

func (c *fakeChannel) Send(_ context.Context, out Reply) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, out)
	if c.failOn == len(c.sends) {
		return errSendFailed
	}
	return nil
}

func (c *fakeChannel) DirectTarget(_ context.Context, externalUserID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.directs = append(c.directs, externalUserID)
	if c.directErr != nil {
		return "", c.directErr
	}
	return "dm-" + externalUserID, nil
}

func (c *fakeChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return c.closeErr
}

func (c *fakeChannel) sent() []Reply {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Reply(nil), c.sends...)
}

func (c *fakeChannel) closed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func (c *fakeChannel) directed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.directs...)
}

func TestNewHub_withNothingConnectedIsAValidInstance(t *testing.T) {
	// Arrange — an instance reached only over HTTP. Refusing to build would make channels
	// mandatory, which they are not.

	// Act
	hub, err := NewHub(testLogger())

	// Assert
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if len(hub.Kinds()) != 0 {
		t.Errorf("kinds = %v, want none", hub.Kinds())
	}
}

func TestNewHub_ignoresAChannelThatWasNotConfigured(t *testing.T) {
	// Arrange — cmd builds an adapter per token and hands over whatever it has. A nil in
	// the list is "no token for that platform", not a mistake to refuse over.
	telegram := newFakeChannel(domain.ChannelTelegram)

	// Act
	hub, err := NewHub(testLogger(), telegram, nil)

	// Assert
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if got := hub.Kinds(); len(got) != 1 || got[0] != domain.ChannelTelegram {
		t.Errorf("kinds = %v, want just telegram", got)
	}
}

func TestNewHub_refusesAPlatformThisBuildCannotDeliverTo(t *testing.T) {
	// Arrange — an adapter whose kind is not in the closed set would be registered, accept
	// messages, and then have no way to answer any of them.

	// Act
	_, err := NewHub(testLogger(), newFakeChannel("signal"))

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the unknown platform refused")
	}
	if !strings.Contains(err.Error(), "signal") {
		t.Errorf("err = %v, want it to name the platform", err)
	}
}

func TestNewHub_refusesTwoAdaptersForOnePlatform(t *testing.T) {
	// Arrange — two connections consuming the same updates is every message answered
	// twice, and every answer paid for twice.

	// Act
	_, err := NewHub(testLogger(), newFakeChannel(domain.ChannelSlack), newFakeChannel(domain.ChannelSlack))

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the duplicate refused")
	}
	if !strings.Contains(err.Error(), "slack") {
		t.Errorf("err = %v, want it to name the platform", err)
	}
}

func TestHubKinds_isTheDeclaredOrderRatherThanTheWiringOrder(t *testing.T) {
	// Arrange — a map's iteration order is random, and a reference endpoint that lists
	// channels in a different order on every restart is one no client can diff.
	hub, err := NewHub(testLogger(),
		newFakeChannel(domain.ChannelDiscord),
		newFakeChannel(domain.ChannelTelegram),
		newFakeChannel(domain.ChannelSlack))
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act — twice, because one agreeing run proves nothing about a random order.
	first, second := hub.Kinds(), hub.Kinds()

	// Assert
	want := domain.AllChannelKinds()
	for i, kind := range want {
		if first[i] != kind || second[i] != kind {
			t.Fatalf("kinds = %v then %v, want %v", first, second, want)
		}
	}
}

func TestHubSend_deliversToThePlatformThatWasAskedFor(t *testing.T) {
	// Arrange
	telegram, discord := newFakeChannel(domain.ChannelTelegram), newFakeChannel(domain.ChannelDiscord)
	hub, err := NewHub(testLogger(), telegram, discord)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.Send(context.Background(), domain.ChannelTelegram, "tg-chat-42", "Revenue is up 3%.")

	// Assert
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := telegram.sent()
	if len(sent) != 1 {
		t.Fatalf("telegram sends = %d, want 1", len(sent))
	}
	if sent[0].ConversationID != "tg-chat-42" || sent[0].Text != "Revenue is up 3%." {
		t.Errorf("sent = %+v, want the text in the named conversation", sent[0])
	}
	// And nowhere else: one answer going out on every connected platform would put a
	// private answer in front of everybody linked to the same account.
	if len(discord.sent()) != 0 {
		t.Errorf("discord sends = %d, want none", len(discord.sent()))
	}
}

func TestHubSend_onAPlatformNothingIsConnectedToSaysSoRatherThanSucceeding(t *testing.T) {
	// Arrange — an operator removed a token and the people who linked to that platform are
	// still sending messages into it. Silently dropping their answers would leave the
	// symptom "the bot stopped replying" and no line anywhere saying why.
	hub, err := NewHub(testLogger(), newFakeChannel(domain.ChannelTelegram))
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.Send(context.Background(), domain.ChannelSlack, "slack-chat-1", "Revenue is up.")

	// Assert
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	if !strings.Contains(err.Error(), "slack") {
		t.Errorf("err = %v, want it to name the platform", err)
	}
}

func TestHubSend_withNowhereToSendItRefusesInsteadOfGuessing(t *testing.T) {
	// Arrange — an empty conversation id reaches the platform as a malformed request, and
	// on some of them as a message to the wrong place.
	telegram := newFakeChannel(domain.ChannelTelegram)
	hub, err := NewHub(testLogger(), telegram)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.Send(context.Background(), domain.ChannelTelegram, "  ", "Revenue is up.")

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the send refused")
	}
	if len(telegram.sent()) != 0 {
		t.Errorf("sends = %d, want nothing attempted", len(telegram.sent()))
	}
}

func TestHubSend_withNothingToSaySendsNothingAndDoesNotFail(t *testing.T) {
	// Arrange — a run that ended with no text is ordinary. The chunker already returns
	// nothing for it, and this is the assertion that the loop over nothing is not an error.
	telegram := newFakeChannel(domain.ChannelTelegram)
	hub, err := NewHub(testLogger(), telegram)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.Send(context.Background(), domain.ChannelTelegram, "tg-chat-42", "   \n ")

	// Assert
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(telegram.sent()) != 0 {
		t.Errorf("sends = %d, want none", len(telegram.sent()))
	}
}

func TestHubSend_splitsALongAnswerToThePlatformsLimitInOrder(t *testing.T) {
	// Arrange — Discord's limit is the smallest, so this is the platform where an answer of
	// a few thousand characters has to become several messages.
	discord := newFakeChannel(domain.ChannelDiscord)
	hub, err := NewHub(testLogger(), discord)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	text := strings.TrimSpace(strings.Repeat("Revenue is up. ", 400))

	// Act
	err = hub.Send(context.Background(), domain.ChannelDiscord, "dc-chat-7", text)

	// Assert
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := discord.sent()
	if len(sent) < 2 {
		t.Fatalf("sends = %d, want the answer split", len(sent))
	}
	rebuilt := make([]string, 0, len(sent))
	for i, out := range sent {
		if runes := utf8.RuneCountInString(out.Text); runes > discordLimit {
			t.Errorf("part %d is %d runes, want at most %d", i+1, runes, discordLimit)
		}
		if out.ConversationID != "dc-chat-7" {
			t.Errorf("part %d went to %q, want the one conversation", i+1, out.ConversationID)
		}
		rebuilt = append(rebuilt, out.Text)
	}
	// Order is the whole point of sending them one at a time: an answer delivered
	// backwards is an answer nobody can read.
	if !strings.HasPrefix(text, strings.TrimSpace(rebuilt[0])) {
		t.Error("the first message is not the start of the answer")
	}
	if !strings.HasSuffix(text, strings.TrimSpace(rebuilt[len(rebuilt)-1])) {
		t.Error("the last message is not the end of the answer")
	}
}

func TestHubSend_stopsAtTheFirstFailureRatherThanRetryingADroppedConnection(t *testing.T) {
	// Arrange — a connection that has dropped fails on every part. Pushing all of them
	// turns one outage into one log line per part, and none of them is more informative
	// than the first.
	discord := newFakeChannel(domain.ChannelDiscord)
	discord.failOn = 2
	hub, err := NewHub(testLogger(), discord)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	text := strings.Repeat("Revenue is up. ", 600)
	if want := 3; len(ChunkFor(domain.ChannelDiscord, text)) < want {
		t.Fatalf("the fixture splits into fewer than %d parts, so it cannot prove an early stop", want)
	}

	// Act
	err = hub.Send(context.Background(), domain.ChannelDiscord, "dc-chat-7", text)

	// Assert
	if !errors.Is(err, errSendFailed) {
		t.Fatalf("err = %v, want the platform's failure", err)
	}
	if len(discord.sent()) != 2 {
		t.Errorf("sends = %d, want the second attempt to have been the last", len(discord.sent()))
	}
	// The failure says which part of how many, because a first part failing and a fourth
	// part failing are two different stories: nothing arrived, or half the answer did.
	if !strings.Contains(err.Error(), "part 2") {
		t.Errorf("err = %v, want it to say how far the delivery got", err)
	}
	// And not the conversation id: this line is about a connection, not about a person.
	if strings.Contains(err.Error(), "dc-chat-7") {
		t.Error("the failure names the private conversation")
	}
}

func TestHubSendDirect_deliversToThePersonsOwnConversationRatherThanToTheirId(t *testing.T) {
	// Arrange
	//
	// A link stores a person's id on the platform, which on Discord is not a place a message
	// can go. So the hub asks the adapter where a direct conversation with them is and sends
	// there — and the fake returns something other than the id it was given precisely so that
	// a version skipping the lookup fails here instead of in production.
	telegram, discord := newFakeChannel(domain.ChannelTelegram), newFakeChannel(domain.ChannelDiscord)
	hub, err := NewHub(testLogger(), telegram, discord)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.SendDirect(context.Background(), domain.ChannelDiscord, "4242424242", "A spend is waiting for a decision.")

	// Assert
	if err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	if got := discord.directed(); len(got) != 1 || got[0] != "4242424242" {
		t.Fatalf("direct lookups = %v, want the recipient asked about once", got)
	}
	sent := discord.sent()
	if len(sent) != 1 {
		t.Fatalf("discord sends = %d, want 1", len(sent))
	}
	if sent[0].ConversationID != "dm-4242424242" {
		t.Errorf("conversation = %q, want the one the adapter resolved", sent[0].ConversationID)
	}
	if sent[0].Text != "A spend is waiting for a decision." {
		t.Errorf("text = %q, want it unchanged", sent[0].Text)
	}
	// And not to everybody linked to the same account on other platforms: telling somebody
	// once is the point, and three copies of every notification is how they stop being read.
	if len(telegram.sent()) != 0 {
		t.Errorf("telegram sends = %d, want none", len(telegram.sent()))
	}
}

func TestHubSendDirect_onAPlatformNothingIsConnectedToSaysSo(t *testing.T) {
	// Arrange — somebody linked Slack and an operator has since removed the token. The
	// notification cannot be delivered, and the caller has to be able to say which platform
	// swallowed it rather than reporting a success nobody received.
	hub, err := NewHub(testLogger(), newFakeChannel(domain.ChannelTelegram))
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.SendDirect(context.Background(), domain.ChannelSlack, "U123ABC", "A goal fell behind pace.")

	// Assert
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	if !strings.Contains(err.Error(), "slack") {
		t.Errorf("err = %v, want it to name the platform", err)
	}
}

func TestHubSendDirect_withNoRecipientRefusesBeforeAskingThePlatform(t *testing.T) {
	// Arrange — an empty external id is a corrupted link. Asking the platform to open a
	// conversation with nobody is a request that can only fail, and on Discord it is a
	// request that leaves the process.
	discord := newFakeChannel(domain.ChannelDiscord)
	hub, err := NewHub(testLogger(), discord)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.SendDirect(context.Background(), domain.ChannelDiscord, "  ", "A spend is waiting.")

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the send refused")
	}
	if len(discord.directed()) != 0 {
		t.Errorf("direct lookups = %d, want nothing asked", len(discord.directed()))
	}
	if len(discord.sent()) != 0 {
		t.Errorf("sends = %d, want nothing attempted", len(discord.sent()))
	}
}

func TestHubSendDirect_withNothingToSayLooksNothingUp(t *testing.T) {
	// Arrange — an empty notification is a bug in whoever built the text, and the chunker
	// already turns it into no messages. What matters is that it costs no request: on Discord
	// the lookup opens a conversation, and opening one to say nothing is a DM channel
	// appearing in somebody's client with no message in it.
	discord := newFakeChannel(domain.ChannelDiscord)
	hub, err := NewHub(testLogger(), discord)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.SendDirect(context.Background(), domain.ChannelDiscord, "4242424242", "   ")

	// Assert
	if err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	if len(discord.directed()) != 0 {
		t.Errorf("direct lookups = %d, want nothing asked", len(discord.directed()))
	}
	if len(discord.sent()) != 0 {
		t.Errorf("sends = %d, want nothing attempted", len(discord.sent()))
	}
}

func TestHubSendDirect_whenTheConversationCannotBeOpenedSendsNothing(t *testing.T) {
	// Arrange — a person who blocked the bot, or a snowflake that is no longer a user. The
	// failure has to reach the caller: a notification reported as delivered when the DM was
	// never opened is worse than one reported as lost.
	discord := newFakeChannel(domain.ChannelDiscord)
	discord.directErr = errors.New("boom: cannot send messages to this user")
	hub, err := NewHub(testLogger(), discord)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.SendDirect(context.Background(), domain.ChannelDiscord, "4242424242", "A spend is waiting.")

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the failure reported")
	}
	if len(discord.sent()) != 0 {
		t.Errorf("sends = %d, want nothing attempted", len(discord.sent()))
	}
}

func TestHubRun_keepsTheOtherPlatformsWhenOneDrops(t *testing.T) {
	// Arrange — a bad Telegram token must not take Slack down with it, and neither may
	// stop the process. This is the property that makes channels optional at runtime as
	// well as at startup.
	telegram := newFakeChannel(domain.ChannelTelegram)
	telegram.runErr = errors.New("boom: unauthorised")
	slack := newFakeChannel(domain.ChannelSlack)
	hub, err := NewHub(testLogger(), telegram, slack)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Act
	returned := make(chan struct{})
	go func() {
		hub.Run(ctx)
		close(returned)
	}()

	// Assert — the one that works is still running after the other has failed.
	<-telegram.started
	<-slack.started
	select {
	case <-returned:
		t.Fatal("Run returned while a working channel was still connected")
	case <-time.After(50 * time.Millisecond):
	}

	// And it returns once it is asked to stop, rather than needing the process killed.
	cancel()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestHubRun_withNothingConnectedReturnsImmediately(t *testing.T) {
	// Arrange — cmd calls this unconditionally, so the no-channels case must not block the
	// shutdown path forever.
	hub, err := NewHub(testLogger())
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	returned := make(chan struct{})
	go func() {
		hub.Run(context.Background())
		close(returned)
	}()

	// Assert
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked with nothing to run")
	}
}

func TestHubClose_closesEveryPlatformAndReportsAllTheFailures(t *testing.T) {
	// Arrange — stopping at the first failure would leave the remaining connections open,
	// which on a restart is a second consumer of the same updates.
	telegram := newFakeChannel(domain.ChannelTelegram)
	telegram.closeErr = errors.New("boom: telegram")
	slack := newFakeChannel(domain.ChannelSlack)
	slack.closeErr = errors.New("boom: slack")
	discord := newFakeChannel(domain.ChannelDiscord)
	hub, err := NewHub(testLogger(), telegram, slack, discord)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act
	err = hub.Close()

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the failures reported")
	}
	for _, kind := range []string{"telegram", "slack"} {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("err = %v, want it to name %s", err, kind)
		}
	}
	for _, c := range []*fakeChannel{telegram, slack, discord} {
		if c.closed() != 1 {
			t.Errorf("%s closed %d times, want once", c.kind, c.closed())
		}
	}
}

func TestHubClose_withEverythingClosingCleanlyReportsNothing(t *testing.T) {
	// Arrange
	hub, err := NewHub(testLogger(), newFakeChannel(domain.ChannelTelegram))
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// Act & Assert
	if err := hub.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestTargetDeliverable_needsBothAPlatformAndAConversation(t *testing.T) {
	// Arrange — half a target is a row that would be sent to the wrong place or to
	// nowhere. The database pairs the two columns with a check constraint; this is the
	// same rule on the way out.
	cases := map[string]struct {
		target Target
		want   bool
	}{
		"a channel chat":      {Target{Kind: domain.ChannelTelegram, ConversationID: "tg-chat-42"}, true},
		"a web chat":          {Target{}, false},
		"a kind with no room": {Target{Kind: domain.ChannelTelegram}, false},
		"a room with no kind": {Target{ConversationID: "tg-chat-42"}, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act & Assert
			if got := tc.target.Deliverable(); got != tc.want {
				t.Errorf("Deliverable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeTargets answers where a chat lives. A chat it does not know is a web chat, not an
// error — which is what most chats are.
type fakeTargets struct {
	targets map[string]Target
	err     error
	lookups int
}

func newFakeTargets() *fakeTargets {
	return &fakeTargets{targets: map[string]Target{}}
}

func (f *fakeTargets) seed(userID, chatID string, target Target) {
	f.targets[userID+"/"+chatID] = target
}

func (f *fakeTargets) TargetFor(_ context.Context, userID, chatID string) (Target, error) {
	f.lookups++
	if f.err != nil {
		return Target{}, f.err
	}
	return f.targets[userID+"/"+chatID], nil
}

// replier builds a replier over one connected platform, and hands back both fakes.
func replier(t *testing.T, kind domain.ChannelKind) (*Replier, *fakeTargets, *fakeChannel) {
	t.Helper()

	adapter := newFakeChannel(kind)
	hub, err := NewHub(testLogger(), adapter)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	targets := newFakeTargets()
	r, err := NewReplier(targets, hub, testLogger())
	if err != nil {
		t.Fatalf("NewReplier: %v", err)
	}
	return r, targets, adapter
}

func TestNewReplier_refusesToBuildWithoutSomewhereToLookOrSomethingToSendOn(t *testing.T) {
	// Arrange
	hub, err := NewHub(testLogger())
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	cases := map[string]struct {
		targets Targets
		hub     *Hub
	}{
		"no target lookup": {nil, hub},
		"no hub":           {newFakeTargets(), nil},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, err := NewReplier(tc.targets, tc.hub, testLogger())

			// Assert
			if err == nil {
				t.Fatal("err = nil, want the wiring refused")
			}
		})
	}
}

func TestReplierReply_deliversAnAnswerToTheChannelTheChatBelongsTo(t *testing.T) {
	// Arrange
	r, targets, telegram := replier(t, domain.ChannelTelegram)
	targets.seed("user-1", "chat-1", Target{Kind: domain.ChannelTelegram, ConversationID: "tg-chat-42"})

	// Act
	err := r.Reply(context.Background(), "user-1", "chat-1", "Revenue is up 3%.")

	// Assert
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	sent := telegram.sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if sent[0].ConversationID != "tg-chat-42" || sent[0].Text != "Revenue is up 3%." {
		t.Errorf("sent = %+v, want the answer in the chat's conversation", sent[0])
	}
}

func TestReplierReply_toAChatThatIsNotOnAChannelSendsNothingAndIsNotAFailure(t *testing.T) {
	// Arrange — the runner answers in every chat it works in, and most of them are
	// somebody using the web client. Treating those as errors would fill the log with one
	// line per web run.
	r, targets, telegram := replier(t, domain.ChannelTelegram)
	targets.seed("user-1", "chat-1", Target{})

	// Act
	err := r.Reply(context.Background(), "user-1", "chat-1", "Revenue is up 3%.")

	// Assert
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(telegram.sent()) != 0 {
		t.Errorf("sends = %d, want none", len(telegram.sent()))
	}
}

func TestReplierReply_withNothingToSayLooksNothingUp(t *testing.T) {
	// Arrange — a run that produced no text. Cheaper to notice here than a query per run
	// that had nothing to report.
	r, targets, telegram := replier(t, domain.ChannelTelegram)
	targets.seed("user-1", "chat-1", Target{Kind: domain.ChannelTelegram, ConversationID: "tg-chat-42"})

	// Act
	err := r.Reply(context.Background(), "user-1", "chat-1", "  \n\t ")

	// Assert
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if targets.lookups != 0 {
		t.Errorf("lookups = %d, want none", targets.lookups)
	}
	if len(telegram.sent()) != 0 {
		t.Errorf("sends = %d, want none", len(telegram.sent()))
	}
}

func TestReplierReply_whenItCannotTellWhereToAnswerItSaysSoRatherThanSendingNothing(t *testing.T) {
	// Arrange — an unreadable lookup and a web chat both end with no delivery, and they
	// must not be reported the same way: one is normal and the other is a database nobody
	// has noticed is down.
	r, targets, telegram := replier(t, domain.ChannelTelegram)
	targets.err = errors.New("boom: the database is unreachable")

	// Act
	err := r.Reply(context.Background(), "user-1", "chat-1", "Revenue is up 3%.")

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the lookup failure reported")
	}
	if len(telegram.sent()) != 0 {
		t.Errorf("sends = %d, want none", len(telegram.sent()))
	}
}

func TestReplierReply_toAPlatformThisInstanceNoLongerHasReportsIt(t *testing.T) {
	// Arrange — the chat says Slack, and Slack is not connected. The person is linked and
	// waiting for an answer that has nowhere to go.
	r, targets, _ := replier(t, domain.ChannelTelegram)
	targets.seed("user-1", "chat-1", Target{Kind: domain.ChannelSlack, ConversationID: "slack-chat-1"})

	// Act
	err := r.Reply(context.Background(), "user-1", "chat-1", "Revenue is up 3%.")

	// Assert
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
}

func TestReplierReply_splitsALongAnswerToThePlatformsLimit(t *testing.T) {
	// Arrange — the replier does not chunk; the hub does. This is the assertion that it
	// goes through the hub rather than calling an adapter directly.
	r, targets, discord := replier(t, domain.ChannelDiscord)
	targets.seed("user-1", "chat-1", Target{Kind: domain.ChannelDiscord, ConversationID: "dc-chat-7"})

	// Act
	err := r.Reply(context.Background(), "user-1", "chat-1", strings.Repeat("Revenue is up. ", 400))

	// Assert
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	sent := discord.sent()
	if len(sent) < 2 {
		t.Fatalf("sends = %d, want the answer split", len(sent))
	}
	for i, out := range sent {
		if runes := utf8.RuneCountInString(out.Text); runes > discordLimit {
			t.Errorf("part %d is %d runes, want at most %d", i+1, runes, discordLimit)
		}
	}
}
