package channel

import (
	"strings"
	"testing"
)

func TestStripMention_removesTheMentionAndReportsThatItWasThere(t *testing.T) {
	// Arrange — the ordinary case on every platform: somebody names the bot and then says
	// what they want. What reaches the model has to be the instruction, not the address.
	cases := map[string]struct {
		text  string
		forms []string
		want  string
	}{
		"at the start":            {"@wingman what happened to revenue", []string{"@wingman"}, "what happened to revenue"},
		"at the end":              {"what happened to revenue @wingman", []string{"@wingman"}, "what happened to revenue"},
		"in the middle":           {"so @wingman what happened", []string{"@wingman"}, "so what happened"},
		"discord's plain form":    {"<@12345> what happened", []string{"<@12345>", "<@!12345>"}, "what happened"},
		"discord's nickname form": {"<@!12345> what happened", []string{"<@12345>", "<@!12345>"}, "what happened"},
		"twice in one message":    {"@wingman please @wingman answer", []string{"@wingman"}, "please answer"},
		"the whole message":       {"@wingman", []string{"@wingman"}, ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got, found := stripMention(tc.text, tc.forms...)

			// Assert
			if !found {
				t.Fatal("found = false, want the mention recognised")
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStripMention_matchesATelegramUsernameWithoutRegardToCase(t *testing.T) {
	// Arrange — Telegram treats @Wingman and @wingman as the same handle, and a client will
	// insert whichever case the person typed. Matching exactly would mean a message that
	// Telegram considers addressed to the bot being read as overheard.

	// Act
	got, found := stripMention("@WingMan what happened to revenue", "@wingman")

	// Assert
	if !found {
		t.Fatal("found = false, want the mention recognised whatever its case")
	}
	if got != "what happened to revenue" {
		t.Errorf("got %q, want the instruction alone", got)
	}
}

func TestStripMention_withNoMentionLeavesTheMessageExactlyAsItWas(t *testing.T) {
	// Arrange — this is the answer that decides a group message is dropped, so the text it
	// hands back must not be a rewritten version of one nobody is going to read anyway.
	text := "  what happened to revenue  "

	// Act
	got, found := stripMention(text, "@wingman")

	// Assert
	if found {
		t.Error("found = true, want an unaddressed message reported as such")
	}
	if got != text {
		t.Errorf("got %q, want the original untouched", got)
	}
}

func TestStripMention_keepsTheShapeOfAPastedBrief(t *testing.T) {
	// Arrange — a brief pasted into a room is routinely a list, a table or a block of SQL.
	// Collapsing its whitespace would hand the model something that no longer means what
	// the person wrote, and there is no way to get the shape back afterwards.
	text := "@wingman reconcile these:\n  - invoice 1\n  - invoice 2\n\nSELECT *\n  FROM ledger"

	// Act
	got, found := stripMention(text, "@wingman")

	// Assert
	if !found {
		t.Fatal("found = false, want the mention recognised")
	}
	want := "reconcile these:\n  - invoice 1\n  - invoice 2\n\nSELECT *\n  FROM ledger"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripMention_leavesNoDoubleSpaceWhereTheMentionWas(t *testing.T) {
	// Arrange — a mention taken out of the middle of a sentence without one of its spaces
	// leaves a gap the model reads as a typo in the instruction it was given.

	// Act
	got, _ := stripMention("check @wingman the ledger", "@wingman")

	// Assert
	if strings.Contains(got, "  ") {
		t.Errorf("got %q, want no double space", got)
	}
	if got != "check the ledger" {
		t.Errorf("got %q, want %q", got, "check the ledger")
	}
}

func TestStripMention_doesNotTakeAFormThatWouldMatchEverything(t *testing.T) {
	// Arrange — a bot with no username gives an adapter "@" to look for, and a bare "@"
	// appears in every email address anybody pastes. Treating that as being addressed would
	// forward a room's whole conversation the moment somebody mentioned a mailbox.
	cases := map[string]string{
		"an empty form": "",
		"a bare at":     "@",
	}

	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got, found := stripMention("email finance@example.com about it", form)

			// Assert
			if found {
				t.Error("found = true, want a form that matches anything ignored")
			}
			if got != "email finance@example.com about it" {
				t.Errorf("got %q, want the original untouched", got)
			}
		})
	}
}

func TestStripMention_withNoFormsAtAllIsNotAddressed(t *testing.T) {
	// Arrange — the Slack path passes one form that may be empty, so "no usable forms" has
	// to be a defined answer rather than a loop over nothing that happens to work.

	// Act
	got, found := stripMention("what happened to revenue")

	// Assert
	if found {
		t.Error("found = true, want nothing to have been found")
	}
	if got != "what happened to revenue" {
		t.Errorf("got %q, want the original untouched", got)
	}
}
