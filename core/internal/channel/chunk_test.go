package channel

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ribdsp/wingman/core/internal/domain"
)

func TestLimitFor_isEachPlatformsOwnLimit(t *testing.T) {
	// Arrange
	cases := map[domain.ChannelKind]int{
		domain.ChannelTelegram: telegramLimit,
		domain.ChannelSlack:    slackLimit,
		domain.ChannelDiscord:  discordLimit,
	}

	for kind, want := range cases {
		t.Run(string(kind), func(t *testing.T) {
			// Act & Assert
			if got := LimitFor(kind); got != want {
				t.Errorf("limit = %d, want %d", got, want)
			}
		})
	}
}

func TestLimitFor_anUnknownPlatformGetsTheSmallestLimit(t *testing.T) {
	// Arrange — nothing should reach this with an unknown kind. If something does, the
	// failure that sends too little is recoverable and the one that sends too much is a
	// message the platform throws away.
	smallest := discordLimit
	for _, kind := range domain.AllChannelKinds() {
		if limit := LimitFor(kind); limit < smallest {
			smallest = limit
		}
	}

	// Act
	got := LimitFor("signal")

	// Assert
	if got != smallest {
		t.Errorf("limit = %d, want the smallest known limit %d", got, smallest)
	}
}

func TestChunk_returnsNothingForAnEmptyReply(t *testing.T) {
	// Arrange — an adapter sending "no reply" and one sending "a reply that was empty" take
	// the same path, which is to send nothing at all.
	for _, text := range []string{"", "   ", "\n\t\r\n"} {
		// Act
		got := Chunk(text, 100)

		// Assert
		if len(got) != 0 {
			t.Errorf("Chunk(%q) = %v, want nothing", text, got)
		}
	}
}

func TestChunk_leavesSomethingThatAlreadyFitsAlone(t *testing.T) {
	// Arrange
	text := "Yesterday's revenue was 4.2 million, up 3% on the week."

	// Act
	got := Chunk(text, 100)

	// Assert
	if len(got) != 1 || got[0] != text {
		t.Fatalf("Chunk = %v, want the text unchanged in one message", got)
	}
}

func TestChunk_trimsTheWholeReplyBeforeMeasuringIt(t *testing.T) {
	// Arrange — a model's answer routinely arrives with a trailing newline, and a reply that
	// is one character over the limit because of it is a reply the platform rejects.
	text := strings.Repeat("a", 100)

	// Act
	got := Chunk("\n  "+text+"  \n", 100)

	// Assert
	if len(got) != 1 {
		t.Fatalf("chunks = %d, want the padding not to have caused a split", len(got))
	}
	if got[0] != text {
		t.Errorf("chunk = %q, want it trimmed", got[0])
	}
}

func TestChunk_neverExceedsTheLimitInAnyChunk(t *testing.T) {
	// Arrange — the property that matters. Prose, one long unbroken word, and a mixture,
	// each measured in runes because that is what the limits are counted in here.
	cases := map[string]string{
		"prose":                       strings.Repeat("Revenue is up. ", 400),
		"one unbroken word":           strings.Repeat("x", 900),
		"a paragraph then a long url": strings.Repeat("Revenue is up.\n\n", 30) + "https://example.test/" + strings.Repeat("y", 400),
		"multi-byte runes":            strings.Repeat("é", 700),
	}

	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got := Chunk(text, 200)

			// Assert
			if len(got) == 0 {
				t.Fatal("chunks = 0, want the text split rather than dropped")
			}
			for i, chunk := range got {
				if runes := utf8.RuneCountInString(chunk); runes > 200 {
					t.Errorf("chunk %d is %d runes, want at most 200", i, runes)
				}
				if chunk == "" {
					t.Errorf("chunk %d is empty", i)
				}
			}
		})
	}
}

func TestChunk_keepsEveryWordWhenItSplitsProse(t *testing.T) {
	// Arrange — a split that loses text is worse than no split: the person reads an answer
	// with a hole in it and has no way to tell.
	words := []string{}
	for i := 0; i < 120; i++ {
		words = append(words, "word"+string(rune('a'+i%26)))
	}
	text := strings.Join(words, " ")

	// Act
	got := Chunk(text, 200)

	// Assert
	if len(got) < 2 {
		t.Fatalf("chunks = %d, want the text split", len(got))
	}
	rejoined := strings.Join(got, " ")
	for _, word := range words {
		if !strings.Contains(rejoined, word) {
			t.Fatalf("the word %q was lost in the split", word)
		}
	}
}

func TestChunk_breaksAtAParagraphWhenThereIsOneInRange(t *testing.T) {
	// Arrange — a blank line is the best break available: it is a paragraph the author
	// already separated.
	first := strings.Repeat("Revenue is up. ", 10)
	second := strings.Repeat("Costs are flat. ", 10)
	text := strings.TrimSpace(first) + "\n\n" + strings.TrimSpace(second)

	// Act
	got := Chunk(text, 180)

	// Assert
	if len(got) < 2 {
		t.Fatalf("chunks = %d, want the text split", len(got))
	}
	if got[0] != strings.TrimSpace(first) {
		t.Errorf("first chunk = %q, want it to end at the blank line", got[0])
	}
	// And the blank line itself is not carried into the next message as leading whitespace.
	if strings.HasPrefix(got[1], "\n") || strings.HasPrefix(got[1], " ") {
		t.Errorf("second chunk = %q, want the separator not carried over", got[1])
	}
}

func TestChunk_doesNotBreakSoEarlyThatTheChunkIsMostlyEmpty(t *testing.T) {
	// Arrange — one space near the start and nothing else. Honouring it would send a
	// 200-character limit as a 12-character message, which is worse than a mid-word cut.
	text := "Revenue is " + strings.Repeat("x", 400)

	// Act
	got := Chunk(text, 200)

	// Assert
	if runes := utf8.RuneCountInString(got[0]); runes < 200/3 {
		t.Errorf("first chunk is %d runes, want the early boundary rejected", runes)
	}
}

func TestChunk_cutsAWordThatIsLongerThanTheLimit(t *testing.T) {
	// Arrange — a URL or a base64 blob. There is no boundary to find, and looping looking
	// for one is how this function would fail to terminate.
	text := strings.Repeat("z", 500)

	// Act
	got := Chunk(text, 100)

	// Assert
	if len(got) < 5 {
		t.Fatalf("chunks = %d, want the word cut into pieces", len(got))
	}
	if joined := strings.Join(got, ""); !strings.HasPrefix(joined, strings.Repeat("z", 100)) {
		t.Error("the cut lost or reordered text")
	}
}

func TestChunk_stopsAtTheCeilingAndSaysWhereTheRestIs(t *testing.T) {
	// Arrange — a run that produced a hundred pages is a run whose answer nobody reads in a
	// chat window, and sending it as fifty messages is indistinguishable from flooding.
	text := strings.Repeat("Revenue is up. ", 5000)

	// Act
	got := Chunk(text, 200)

	// Assert
	if len(got) != maxChunks {
		t.Fatalf("chunks = %d, want the ceiling of %d", len(got), maxChunks)
	}
	last := got[len(got)-1]
	// Said rather than silent: the transcript is in Wingman either way, and the person needs
	// to know there is more of it.
	if !strings.Contains(last, "full transcript") {
		t.Errorf("last chunk = %q, want it to say where the rest is", last)
	}
	if runes := utf8.RuneCountInString(last); runes > 200 {
		t.Errorf("last chunk is %d runes, want the notice to have been budgeted for", runes)
	}
}

func TestChunk_withAnUnusableLimitFallsBackRatherThanLooping(t *testing.T) {
	// Arrange — a zero limit is a caller that did not fill something in, and a limit smaller
	// than the truncation notice is one that cannot be honoured. Neither may hang.
	cases := map[string]int{"zero": 0, "negative": -10, "smaller than the notice": 4}

	for name, limit := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got := Chunk(strings.Repeat("Revenue is up. ", 200), limit)

			// Assert
			if len(got) == 0 {
				t.Fatal("chunks = 0, want something sent")
			}
			if len(got) > maxChunks {
				t.Errorf("chunks = %d, want at most the ceiling %d", len(got), maxChunks)
			}
		})
	}
}

func TestChunkFor_usesThePlatformsLimit(t *testing.T) {
	// Arrange — 2500 runes fits Telegram and Slack in one message and does not fit Discord.
	text := strings.Repeat("a", 2500)

	// Act & Assert
	if got := ChunkFor(domain.ChannelTelegram, text); len(got) != 1 {
		t.Errorf("telegram chunks = %d, want 1", len(got))
	}
	if got := ChunkFor(domain.ChannelSlack, text); len(got) != 1 {
		t.Errorf("slack chunks = %d, want 1", len(got))
	}
	if got := ChunkFor(domain.ChannelDiscord, text); len(got) < 2 {
		t.Errorf("discord chunks = %d, want it split", len(got))
	}
}

func TestInboundValidate_refusesWhatNoVerdictWouldMeanAnythingAbout(t *testing.T) {
	// Arrange — an adapter bug, not something a sender did, which is why it is the one error
	// the inbound path returns.
	complete := Inbound{
		Kind:           domain.ChannelTelegram,
		SenderID:       "tg-1",
		ConversationID: "tg-chat-1",
		Text:           "hello",
	}
	cases := map[string]struct {
		in   Inbound
		want string
	}{
		"an unknown platform": {func() Inbound { in := complete; in.Kind = "signal"; return in }(), "kind"},
		"no author":           {func() Inbound { in := complete; in.SenderID = "  "; return in }(), "senderId"},
		"nowhere to answer":   {func() Inbound { in := complete; in.ConversationID = ""; return in }(), "conversationId"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			err := tc.in.Validate()

			// Assert
			if err == nil {
				t.Fatal("err = nil, want the message refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

func TestInboundValidate_acceptsAMessageWithNoText(t *testing.T) {
	// Arrange — a sticker or a photo with no caption is a real message that arrives, and it
	// is the ladder's business to ignore it, not this one's. Refusing it here would turn an
	// ordinary event into a logged adapter bug on every sticker anybody sends.
	in := Inbound{Kind: domain.ChannelSlack, SenderID: "slack-1", ConversationID: "slack-chat-1"}

	// Act & Assert
	if err := in.Validate(); err != nil {
		t.Errorf("Validate: %v, want a message with no text accepted", err)
	}
}
