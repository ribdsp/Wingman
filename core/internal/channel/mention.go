package channel

import "strings"

// stripMention removes the way a platform writes "this bot" out of a message, and reports
// whether it was there.
//
// Two jobs in one: in a room, finding a mention is how an adapter knows the bot was
// addressed rather than overhearing, and removing it is what stops every group message
// reaching the model as "@wingman what happened to revenue". Several forms because the
// platforms differ — Discord writes a mention two ways depending on the client that sent
// it, and Telegram usernames are matched without case because Telegram treats them that
// way.
//
// What is left is otherwise untouched, byte for byte: a brief pasted into a room keeps its
// line breaks and its indentation, because it may be a list or a block of SQL and this is
// not the place to reformat one.
//
// Pure, which is the point. Whether a message addressed the bot is the only decision an
// adapter makes, so it is the one thing in an adapter that has to be testable without a
// connection.
func stripMention(text string, forms ...string) (string, bool) {
	found := false
	for _, form := range forms {
		if form == "" || form == "@" {
			continue
		}
		for {
			at := indexFold(text, form)
			if at < 0 {
				break
			}
			end := at + len(form)
			// One adjacent blank goes with it, so a mention taken out of the middle of a
			// sentence does not leave a double space where it was.
			switch {
			case end < len(text) && isBlank(text[end]):
				end++
			case at > 0 && isBlank(text[at-1]):
				at--
			}
			text = text[:at] + text[end:]
			found = true
		}
	}
	if !found {
		return text, false
	}
	return strings.TrimSpace(text), true
}

func isBlank(b byte) bool { return b == ' ' || b == '\t' }

// indexFold is strings.Index without case sensitivity, which the standard library has no
// direct equivalent of. Both arguments are short — a message and a username — so the
// straightforward scan is the right one.
func indexFold(text, substring string) int {
	if substring == "" {
		return -1
	}
	limit := len(text) - len(substring)
	for at := 0; at <= limit; at++ {
		if strings.EqualFold(text[at:at+len(substring)], substring) {
			return at
		}
	}
	return -1
}
