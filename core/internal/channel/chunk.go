package channel

import (
	"strings"
	"unicode/utf8"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// What one message may hold, per platform.
//
// These are the platforms' own limits, and they are counted in different things: Telegram
// documents 4096 UTF-16 code units, Slack about 3000 characters before a message is
// truncated in some clients, Discord 2000 characters. Splitting is counted in runes
// against the smallest reading of each, because a reply that comes back one character over
// is a reply the platform rejects and a person never sees.
const (
	telegramLimit = 4096
	slackLimit    = 3000
	discordLimit  = 2000

	// maxChunks bounds how many messages one reply may become.
	//
	// A run that produced a hundred pages of output is a run whose answer nobody is going
	// to read in a chat window, and sending it as fifty messages is indistinguishable from
	// flooding the room. What is over the ceiling is dropped with a line saying so, rather
	// than silently: the transcript is in Wingman either way, and the person needs to know
	// there is more of it.
	maxChunks = 8

	// truncationNotice replaces what did not fit. It says where the rest is, because "…"
	// on its own tells somebody their answer was cut and nothing about how to get it.
	truncationNotice = "…\n\n(That is as much as fits here. The full transcript is in Wingman.)"
)

// LimitFor is how much text one message on this platform may carry.
//
// An unknown kind gets the smallest limit rather than an error. Nothing should reach this
// with one — Inbound.Validate refuses it at the door — but if something does, the failure
// that sends too little is recoverable and the one that sends too much is a message the
// platform throws away.
func LimitFor(kind domain.ChannelKind) int {
	switch kind {
	case domain.ChannelTelegram:
		return telegramLimit
	case domain.ChannelSlack:
		return slackLimit
	case domain.ChannelDiscord:
		return discordLimit
	default:
		return discordLimit
	}
}

// Chunk splits text into messages that each fit within limit.
//
// Pure, and the only reason it is a function rather than three lines in each adapter: the
// rule about where a split may fall is worth testing once. It prefers to break at a blank
// line, then at a line ending, then at a space, and only cuts a word when a single word is
// longer than the limit — which happens with a URL or a base64 blob and has to be handled
// rather than looped on.
//
// An empty or whitespace-only text returns nothing at all, so an adapter sending "no
// reply" and an adapter sending "a reply that was empty" take the same path.
func Chunk(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if limit <= 0 {
		limit = discordLimit
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	chunks := []string{}
	rest := text
	for rest != "" {
		if utf8.RuneCountInString(rest) <= limit {
			chunks = append(chunks, rest)
			break
		}
		if len(chunks) == maxChunks-1 {
			// The last chunk this reply is allowed. What is left goes, and the notice
			// replaces it — sized so the chunk with the notice appended still fits.
			chunks = append(chunks, fit(rest, limit))
			break
		}

		head, tail := split(rest, limit)
		chunks = append(chunks, head)
		rest = strings.TrimLeft(tail, " \t\r\n")
	}
	return chunks
}

// ChunkFor is Chunk against a platform's own limit, which is what every adapter wants.
func ChunkFor(kind domain.ChannelKind, text string) []string {
	return Chunk(text, LimitFor(kind))
}

// split cuts rest into a piece that fits and the remainder, at the latest acceptable
// boundary. rest is known to be longer than limit.
func split(rest string, limit int) (head, tail string) {
	// The byte offset just past the limit-th rune, so the boundary search below works in
	// bytes on a prefix that is known to be rune-aligned.
	cut := len(rest)
	seen := 0
	for offset := range rest {
		if seen == limit {
			cut = offset
			break
		}
		seen++
	}
	window := rest[:cut]

	// A blank line is the best break: it is a paragraph the author already separated.
	// Then a line ending, then a space. Nothing shorter than a third of the limit is
	// accepted as a boundary, because splitting a 4000-character answer after 40
	// characters to land on a space is worse than splitting it mid-sentence.
	floor := limit / 3
	for _, separator := range []string{"\n\n", "\n", " "} {
		at := strings.LastIndex(window, separator)
		if at > 0 && utf8.RuneCountInString(window[:at]) >= floor {
			return strings.TrimRight(window[:at], " \t\r\n"), rest[at:]
		}
	}
	// One word longer than the limit. Cut it.
	return window, rest[cut:]
}

// fit shortens text so it plus the truncation notice fits within limit.
func fit(text string, limit int) string {
	room := limit - utf8.RuneCountInString(truncationNotice)
	if room <= 0 {
		// A limit smaller than the notice itself. Nothing useful can be said in it, so say
		// the shortest true thing.
		return "…"
	}
	if utf8.RuneCountInString(text) <= room {
		return text + truncationNotice
	}

	head, _ := split(text, room)
	return strings.TrimRight(head, " \t\r\n") + truncationNotice
}
