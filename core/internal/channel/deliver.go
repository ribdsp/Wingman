package channel

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"
)

// deliver sends text on one channel, split into as many messages as the platform needs.
//
// Sequential, and it stops at the first failure: a connection that has dropped will fail
// on every chunk, and eight attempts against it produce eight log lines about one outage.
// A partial delivery is the honest outcome of losing a connection halfway through, and the
// whole answer is in the chat either way.
//
// Shared by the hub and by each adapter's own inbound reply path, so that "how a long
// answer becomes several messages" has one implementation and not four.
func deliver(ctx context.Context, c Channel, conversationID, text string) error {
	kind := c.Kind()
	chunks := ChunkFor(kind, text)
	for i, chunk := range chunks {
		if err := c.Send(ctx, Reply{ConversationID: conversationID, Text: chunk}); err != nil {
			// The conversation id is not in the message. It identifies a private chat, and
			// this line is about a connection rather than about a person.
			return fmt.Errorf("send part %d of %d on %s: %w", i+1, len(chunks), kind, err)
		}
	}
	return nil
}

// dispatch is the whole inbound path for one message, on any platform: hand it to the
// handler, say back whatever the handler decided, and let neither step stop the connection.
//
// Every adapter's update loop ends here, which is why the loops themselves contain no
// policy at all — they map their platform's update and call this.
func dispatch(ctx context.Context, c Channel, h Handler, log zerolog.Logger, in Inbound) {
	kind := string(c.Kind())

	handled, err := h.Handle(ctx, in)
	if err != nil {
		// The only error the handler returns is about an Inbound it could not reach any
		// verdict on, which is a bug in the mapper above. Logged as one, and not a reason
		// to stop reading updates: the next message may well be fine.
		log.Error().Err(err).Str("channel", kind).
			Msg("an inbound message was incomplete and no verdict was reached")
		return
	}
	if handled.Reply == "" {
		return
	}

	if err := deliver(ctx, c, in.ConversationID, handled.Reply); err != nil {
		log.Error().Err(err).Str("channel", kind).Str("verdict", string(handled.Verdict)).
			Msg("a reply could not be delivered")
	}
}
