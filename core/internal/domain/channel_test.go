package domain

import (
	"strings"
	"testing"
	"time"
)

var inboundNow = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

// linkedInbound is a message that would be accepted: a linked sender, in a direct
// conversation, saying something short, with nothing in the way. Each test spoils
// exactly one of those facts, so the test name says which branch it is proving.
func linkedInbound() InboundFacts {
	return InboundFacts{
		Kind:         ChannelTelegram,
		Text:         "check yesterday's numbers",
		Direct:       true,
		Linked:       true,
		LastAccepted: inboundNow.Add(-time.Hour),
		MinInterval:  3 * time.Second,
	}
}

func TestClassifyInbound_linkedSenderWithNothingInTheWay_isAccepted(t *testing.T) {
	// Arrange
	in := linkedInbound()

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundAccept {
		t.Fatalf("verdict = %s, want accept", got.Verdict)
	}
	if got.LinkCode != "" {
		t.Fatalf("linkCode = %q, want empty on an accepted message", got.LinkCode)
	}
}

func TestClassifyInbound_botAuthored_isIgnored(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.FromBot = true

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundIgnore {
		t.Fatalf("verdict = %s, want ignore", got.Verdict)
	}
}

// The first rung has to be first: two bots answering each other costs real money
// per exchange, and every rung below this one replies to something.
func TestClassifyInbound_botAuthored_outranksEveryOtherRefusal(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.FromBot = true
	in.Linked = false
	in.Direct = false
	in.KillSwitchEngaged = true
	in.Text = strings.Repeat("x", MaxBriefLength+1)

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundIgnore {
		t.Fatalf("verdict = %s, want ignore", got.Verdict)
	}
}

func TestClassifyInbound_emptyText_isIgnored(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Text = "   \n\t "

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundIgnore {
		t.Fatalf("verdict = %s, want ignore", got.Verdict)
	}
}

func TestClassifyInbound_groupWhenGroupsDisallowed_isRefused(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Direct = false

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundGroupRefused {
		t.Fatalf("verdict = %s, want group_refused", got.Verdict)
	}
}

func TestClassifyInbound_groupWhenGroupsAllowed_isAccepted(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Direct = false
	in.AllowGroups = true

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundAccept {
		t.Fatalf("verdict = %s, want accept", got.Verdict)
	}
}

// Whether a room may be used at all is a fact about the room, not about who is
// typing in it. Answering an unlinked stranger there with "here is how to link"
// invites an account credential into a place other people can read.
func TestClassifyInbound_groupRefusal_outranksTheLinkCheck(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Direct = false
	in.Linked = false

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundGroupRefused {
		t.Fatalf("verdict = %s, want group_refused", got.Verdict)
	}
}

func TestClassifyInbound_unlinkedSender_needsALink(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Linked = false

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundLinkRequired {
		t.Fatalf("verdict = %s, want link_required", got.Verdict)
	}
}

func TestClassifyInbound_unlinkedSenderPastingACode_isALinkAttempt(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Linked = false
	in.Text = " wgm-abcde-fghjk "

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundLinkAttempt {
		t.Fatalf("verdict = %s, want link_attempt", got.Verdict)
	}
	if got.LinkCode != "WGM-ABCDEFGHJK" {
		t.Fatalf("linkCode = %q, want the normalised code", got.LinkCode)
	}
}

// A linked sender pasting a code is talking, not linking. Consuming it would burn
// somebody else's outstanding code — the codes are not scoped to a channel, so the
// one in the message may be a colleague's.
func TestClassifyInbound_linkedSenderPastingACode_isJustAMessage(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Text = "WGM-ABCDE-FGHJK"

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundAccept {
		t.Fatalf("verdict = %s, want accept", got.Verdict)
	}
}

// Linking is not work: it spends no tokens and starts no run, so a halted instance
// still lets a person finish connecting rather than answering them with silence they
// have no way to interpret.
func TestClassifyInbound_linkAttemptWhileHalted_stillLinks(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Linked = false
	in.Text = "WGM-ABCDE-FGHJK"
	in.KillSwitchEngaged = true

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundLinkAttempt {
		t.Fatalf("verdict = %s, want link_attempt", got.Verdict)
	}
}

func TestClassifyInbound_insideTheSenderInterval_isThrottled(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.MinInterval = 10 * time.Second
	in.LastAccepted = inboundNow.Add(-4 * time.Second)

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundThrottled {
		t.Fatalf("verdict = %s, want throttled", got.Verdict)
	}
	if got.RetryAfter != 6*time.Second {
		t.Fatalf("retryAfter = %s, want 6s", got.RetryAfter)
	}
}

func TestClassifyInbound_neverAccepted_isNotThrottled(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.MinInterval = time.Hour
	in.LastAccepted = time.Time{}

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundAccept {
		t.Fatalf("verdict = %s, want accept", got.Verdict)
	}
}

// A zero interval is the one omission that would read as "no throttle", on the one
// path a stranger can drive. The floor applies whatever the caller passed.
func TestClassifyInbound_zeroInterval_stillThrottles(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.MinInterval = 0
	in.LastAccepted = inboundNow.Add(-MinInboundInterval / 2)

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundThrottled {
		t.Fatalf("verdict = %s, want throttled", got.Verdict)
	}
}

func TestClassifyInbound_briefTooLong_isRefused(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Text = strings.Repeat("x", MaxBriefLength+1)

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundTooLong {
		t.Fatalf("verdict = %s, want too_long", got.Verdict)
	}
}

func TestClassifyInbound_briefAtTheLimit_isAccepted(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Text = strings.Repeat("x", MaxBriefLength)

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundAccept {
		t.Fatalf("verdict = %s, want accept", got.Verdict)
	}
}

func TestClassifyInbound_killSwitchEngaged_isHalted(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.KillSwitchEngaged = true

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundHalted {
		t.Fatalf("verdict = %s, want halted", got.Verdict)
	}
}

// The kill switch is the only fact here that costs a network call to establish.
// Everything cheap is decided first, so a flood of messages cannot become a flood
// of requests to the goal engine.
func TestClassifyInbound_throttle_outranksTheKillSwitch(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.LastAccepted = inboundNow
	in.KillSwitchEngaged = true

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundThrottled {
		t.Fatalf("verdict = %s, want throttled", got.Verdict)
	}
}

func TestClassifyInbound_tooLong_outranksTheKillSwitch(t *testing.T) {
	// Arrange
	in := linkedInbound()
	in.Text = strings.Repeat("x", MaxBriefLength+1)
	in.KillSwitchEngaged = true

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if got.Verdict != InboundTooLong {
		t.Fatalf("verdict = %s, want too_long", got.Verdict)
	}
}

func TestClassifyInbound_decidedAt_isThePassedTime(t *testing.T) {
	// Arrange
	in := linkedInbound()

	// Act
	got := ClassifyInbound(in, inboundNow)

	// Assert
	if !got.DecidedAt.Equal(inboundNow) {
		t.Fatalf("decidedAt = %s, want %s", got.DecidedAt, inboundNow)
	}
}

func TestInboundVerdict_silentOnesSayNothingBack(t *testing.T) {
	// Arrange
	cases := map[InboundVerdict]bool{
		InboundIgnore:       true,
		InboundThrottled:    true,
		InboundGroupRefused: false,
		InboundLinkAttempt:  false,
		InboundLinkRequired: false,
		InboundTooLong:      false,
		InboundHalted:       false,
		InboundAccept:       false,
	}

	for verdict, wantSilent := range cases {
		// Act
		got := verdict.Silent()

		// Assert
		if got != wantSilent {
			t.Errorf("%s.Silent() = %t, want %t", verdict, got, wantSilent)
		}
	}
}

func TestChannelKind_valid(t *testing.T) {
	// Arrange
	for _, kind := range AllChannelKinds() {
		// Act & Assert
		if !kind.Valid() {
			t.Errorf("%s is in AllChannelKinds but not valid", kind)
		}
	}

	for _, kind := range []ChannelKind{"", "whatsapp", "TELEGRAM", "email"} {
		if kind.Valid() {
			t.Errorf("%q reported valid", kind)
		}
	}
}

func TestAllChannelKinds_isTheClosedSet(t *testing.T) {
	// Arrange
	want := []ChannelKind{ChannelTelegram, ChannelSlack, ChannelDiscord}

	// Act
	got := AllChannelKinds()

	// Assert
	if len(got) != len(want) {
		t.Fatalf("got %d kinds, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("kind %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestNormaliseLinkCode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"the canonical form", "WGM-ABCDEFGHJK", "WGM-ABCDEFGHJK", true},
		{"as displayed, grouped", "WGM-ABCDE-FGHJK", "WGM-ABCDEFGHJK", true},
		{"lowercase, as a phone types it", "wgm-abcde-fghjk", "WGM-ABCDEFGHJK", true},
		{"pasted with spaces", " WGM ABCDE FGHJK ", "WGM-ABCDEFGHJK", true},
		{"pasted with underscores", "WGM_ABCDE_FGHJK", "WGM-ABCDEFGHJK", true},
		{"no prefix", "ABCDE-FGHJK", "", false},
		{"a word before it", "code WGM-ABCDE-FGHJK", "", false},
		{"a word after it", "WGM-ABCDE-FGHJK thanks", "", false},
		{"one character short", "WGM-ABCDE-FGHJ", "", false},
		{"one character long", "WGM-ABCDE-FGHJKM", "", false},
		{"an excluded character", "WGM-ABCDE-FGHJ0", "", false},
		{"an ambiguous letter", "WGM-ABCDE-FGHJI", "", false},
		{"empty", "", "", false},
		{"the prefix alone", "WGM-", "", false},
		{"prose that happens to start with the marker", "wgmail is down again", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got, ok := NormaliseLinkCode(tc.in)

			// Assert
			if ok != tc.ok {
				t.Fatalf("ok = %t, want %t", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("code = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatLinkCode_groupsTheBodyForReading(t *testing.T) {
	// Arrange
	code := "WGM-ABCDEFGHJK"

	// Act
	got := FormatLinkCode(code)

	// Assert
	if got != "WGM-ABCDE-FGHJK" {
		t.Fatalf("formatted = %q, want WGM-ABCDE-FGHJK", got)
	}
}

// A code that is not in canonical form is returned untouched rather than
// rearranged: this function is for display, and inventing a shape for a value it
// does not recognise would put a code on screen that cannot be typed back in.
func TestFormatLinkCode_leavesAnythingElseAlone(t *testing.T) {
	// Arrange
	for _, in := range []string{"", "WGM-", "nonsense"} {
		// Act
		got := FormatLinkCode(in)

		// Assert
		if got != in {
			t.Errorf("FormatLinkCode(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestFormatLinkCode_roundTripsThroughNormalise(t *testing.T) {
	// Arrange
	canonical := "WGM-ABCDEFGHJK"

	// Act
	got, ok := NormaliseLinkCode(FormatLinkCode(canonical))

	// Assert
	if !ok || got != canonical {
		t.Fatalf("round trip gave (%q, %t), want (%q, true)", got, ok, canonical)
	}
}

func TestLinkCodeAlphabet_holdsNoAmbiguousCharacters(t *testing.T) {
	// Arrange — the pairs a person mistypes when reading a code off a screen.
	for _, banned := range []rune{'0', 'O', '1', 'I', 'L', 'U'} {
		// Act & Assert
		if strings.ContainsRune(LinkCodeAlphabet, banned) {
			t.Errorf("alphabet contains %q", banned)
		}
	}
	if len(LinkCodeAlphabet) != 30 {
		t.Fatalf("alphabet is %d characters, want 30", len(LinkCodeAlphabet))
	}
}
