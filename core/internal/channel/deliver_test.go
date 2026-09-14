package channel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// fakeHandler stands in for service.Inbox: it records what it was asked about and answers
// with whatever the test set up.
type fakeHandler struct {
	handled  Handled
	err      error
	inbounds []Inbound
}

func (h *fakeHandler) Handle(_ context.Context, in Inbound) (Handled, error) {
	h.inbounds = append(h.inbounds, in)
	if h.err != nil {
		return Handled{}, h.err
	}
	return h.handled, nil
}

// inbound is a plausible direct message, for tests that care about what happens to a
// verdict rather than about which platform produced it.
func inbound(kind domain.ChannelKind) Inbound {
	return Inbound{
		Kind:           kind,
		SenderID:       "sender-1",
		SenderName:     "Someone",
		ConversationID: "conv-1",
		Text:           "what happened to revenue",
		Direct:         true,
	}
}

func TestDispatch_sendsTheAnswerBackToTheConversationItCameFrom(t *testing.T) {
	// Arrange — in a group the sender and the conversation are different, and an answer that
	// went to the sender would leave the room with a question and no reply.
	adapter := newFakeChannel(domain.ChannelTelegram)
	handler := &fakeHandler{handled: Handled{Verdict: domain.InboundAccept, Reply: "Working on it."}}
	in := inbound(domain.ChannelTelegram)
	in.Direct = false

	// Act
	dispatch(context.Background(), adapter, handler, testLogger(), in)

	// Assert
	if len(handler.inbounds) != 1 {
		t.Fatalf("handler calls = %d, want 1", len(handler.inbounds))
	}
	sent := adapter.sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if sent[0].ConversationID != "conv-1" || sent[0].Text != "Working on it." {
		t.Errorf("sent = %+v, want the answer in the conversation the message arrived in", sent[0])
	}
}

func TestDispatch_withNothingToSaySendsNothing(t *testing.T) {
	// Arrange — this is the ignore verdict, and it is the common one: a bot's own message
	// echoed back, an empty caption, a message overheard in a room. An adapter that answered
	// those would be talking to itself in public.
	adapter := newFakeChannel(domain.ChannelSlack)
	handler := &fakeHandler{handled: Handled{Verdict: domain.InboundIgnore}}

	// Act
	dispatch(context.Background(), adapter, handler, testLogger(), inbound(domain.ChannelSlack))

	// Assert
	if len(adapter.sent()) != 0 {
		t.Errorf("sends = %d, want none", len(adapter.sent()))
	}
}

func TestDispatch_whenNoVerdictCouldBeReachedSendsNothing(t *testing.T) {
	// Arrange — an error here means the adapter produced something incomplete, so there is
	// no verdict and nothing that could honestly be said to the sender. Guessing a reply
	// would tell a person their message was handled when nothing knows whether it was.
	adapter := newFakeChannel(domain.ChannelDiscord)
	handler := &fakeHandler{err: errors.New("boom: incomplete")}

	// Act
	dispatch(context.Background(), adapter, handler, testLogger(), inbound(domain.ChannelDiscord))

	// Assert
	if len(adapter.sent()) != 0 {
		t.Errorf("sends = %d, want none", len(adapter.sent()))
	}
}

func TestDispatch_splitsALongAnswerToThePlatformsLimit(t *testing.T) {
	// Arrange — an adapter's own reply path has to chunk exactly as the runner's does. Two
	// implementations of that would mean one platform silently truncating a report.
	adapter := newFakeChannel(domain.ChannelDiscord)
	handler := &fakeHandler{handled: Handled{
		Verdict: domain.InboundAccept,
		Reply:   strings.TrimSpace(strings.Repeat("Revenue is up. ", 400)),
	}}

	// Act
	dispatch(context.Background(), adapter, handler, testLogger(), inbound(domain.ChannelDiscord))

	// Assert
	sent := adapter.sent()
	if len(sent) < 2 {
		t.Fatalf("sends = %d, want the answer split", len(sent))
	}
	for i, out := range sent {
		if runes := utf8.RuneCountInString(out.Text); runes > discordLimit {
			t.Errorf("part %d is %d runes, want at most %d", i+1, runes, discordLimit)
		}
	}
}

func TestDispatch_aRefusedDeliveryIsLoggedRatherThanRetriedOrPanicked(t *testing.T) {
	// Arrange — dispatch runs on an adapter's update loop and has nobody to return an error
	// to. A platform refusing one reply must not stop the loop that reads the next message,
	// which is the difference between one lost answer and a channel that has gone deaf.
	adapter := newFakeChannel(domain.ChannelTelegram)
	adapter.failOn = 1
	handler := &fakeHandler{handled: Handled{Verdict: domain.InboundAccept, Reply: "Working on it."}}

	// Act
	dispatch(context.Background(), adapter, handler, testLogger(), inbound(domain.ChannelTelegram))

	// Assert
	if len(adapter.sent()) != 1 {
		t.Errorf("sends = %d, want the one refused attempt and no retry", len(adapter.sent()))
	}
}
