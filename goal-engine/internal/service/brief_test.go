package service

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
)

// briefEvaluation is the evaluation a behind-pace goal produces, with the numbers
// the brief has to carry.
func briefEvaluation() domain.Evaluation {
	return domain.Evaluation{
		GoalID:        "goal-1",
		EvaluatedAt:   testNow,
		ObservedValue: 12_500_000,
		ExpectedValue: 41_500_000,
		TargetValue:   100_000_000,
		BaselineValue: 0,
		ElapsedRatio:  0.35,
		PaceRatio:     0.301,
		Decision:      domain.DecisionTrigger,
		Reason:        "observed 12,500,000 is behind the expected 41,500,000 at 35% of the period",
	}
}

func TestBuildBriefCarriesEveryNumberTheAgentNeeds(t *testing.T) {
	// The brief is the whole interface to the agent. A number that is missing here
	// is a number the agent will invent.
	goal := testGoal().Goal
	goal.TargetValue = 100_000_000

	brief := BuildBrief(goal, briefEvaluation(), time.UTC)

	for _, want := range []string{
		"Reach 100M MRR",         // which goal
		"acme",                   // which product
		testMetric,               // which metric
		"12,500,000",             // where it is
		"41,500,000",             // where it should be
		"100,000,000",            // where it is going
		"reach at least",         // in which direction
		"35% of the period",      // how much time is left
		"0.30",                   // how far behind
		"2026-10-01T00:00:00Z",   // by when
		"is behind the expected", // why it was woken
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("expected %q in the brief:\n%s", want, brief)
		}
	}
}

func TestBuildBriefForbidsAutonomousSpending(t *testing.T) {
	// This paragraph is the boundary the whole service exists to hold. If it stops
	// being emitted, an agent is being woken with no instruction to ask first.
	brief := BuildBrief(testGoal().Goal, briefEvaluation(), time.UTC)

	for _, want := range []string{
		"approval API",
		"Do not spend, commit, or authorise funds on your own judgement",
		"Do not edit the goal",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("expected %q in the brief:\n%s", want, brief)
		}
	}
}

func TestBuildBriefQuotesTheOperatorsOwnWording(t *testing.T) {
	// The agent acts on intent, and the operator's phrasing carries intent the
	// metric alone does not.
	goal := testGoal().Goal
	goal.SourceText = "Push MRR to 100 million before the end of September, but do not discount."

	brief := BuildBrief(goal, briefEvaluation(), time.UTC)
	if !strings.Contains(brief, "do not discount") {
		t.Fatalf("expected the operator's wording to survive:\n%s", brief)
	}
}

func TestBuildBriefOmitsTheQuoteSectionWhenThereIsNothingToQuote(t *testing.T) {
	goal := testGoal().Goal
	goal.SourceText = "   "

	brief := BuildBrief(goal, briefEvaluation(), time.UTC)
	if strings.Contains(brief, "What the operator asked for") {
		t.Fatalf("expected no empty quote section:\n%s", brief)
	}
}

func TestBuildBriefBoundsAPathologicalGoalText(t *testing.T) {
	// One goal with a pasted document in it must not produce an unbounded prompt.
	goal := testGoal().Goal
	goal.SourceText = strings.Repeat("a", maxSourceTextInBrief*3)

	brief := BuildBrief(goal, briefEvaluation(), time.UTC)
	if len([]rune(brief)) > maxSourceTextInBrief+2000 {
		t.Fatalf("expected the brief to stay bounded, got %d runes", len([]rune(brief)))
	}
	if !strings.Contains(brief, "…") {
		t.Fatal("expected the truncation to be visible to the reader")
	}
}

func TestBuildBriefRendersTimesInTheOperatorsTimezone(t *testing.T) {
	// An operator reading "17:00" for something that happened at 12:00 UTC cannot
	// line the brief up against their own day.
	jakarta := time.FixedZone("WIB", 7*60*60)

	brief := BuildBrief(testGoal().Goal, briefEvaluation(), jakarta)
	if !strings.Contains(brief, "2026-09-11T19:00:00+07:00") {
		t.Fatalf("expected the local timestamp:\n%s", brief)
	}
}

func TestBuildBriefFallsBackToUTCWhenNoTimezoneIsConfigured(t *testing.T) {
	// A nil location must not panic in the one code path that wakes an agent.
	brief := BuildBrief(testGoal().Goal, briefEvaluation(), nil)
	if !strings.Contains(brief, "2026-09-11T12:00:00Z") {
		t.Fatalf("expected a UTC timestamp:\n%s", brief)
	}
}

func TestBuildBriefDescribesAStayUnderGoalAsStayingUnder(t *testing.T) {
	// Telling an agent to "reach at least" a cost ceiling would invert the goal.
	goal := testGoal().Goal
	goal.Comparator = domain.ComparatorLTE
	goal.Title = "Keep refund rate under 2%"

	brief := BuildBrief(goal, briefEvaluation(), time.UTC)
	if !strings.Contains(brief, "stay at or below") {
		t.Fatalf("expected the ceiling phrasing:\n%s", brief)
	}
	if strings.Contains(brief, "reach at least") {
		t.Fatalf("expected no growth phrasing on a ceiling goal:\n%s", brief)
	}
}

func TestFormatNumberGroupsThousandsAndDropsPointlessDecimals(t *testing.T) {
	cases := map[float64]string{
		0:             "0",
		7:             "7",
		999:           "999",
		1000:          "1,000",
		12345:         "12,345",
		100_000_000:   "100,000,000",
		1_234_567_890: "1,234,567,890",
		-45_000:       "-45,000",
		2.5:           "2.50",
		-0.126:        "-0.13",
		-1234.5:       "-1,234.50",
	}
	for input, want := range cases {
		if got := formatNumber(input); got != want {
			t.Fatalf("formatNumber(%v) = %q, want %q", input, got, want)
		}
	}
}

func TestFormatNumberNamesABrokenValueInsteadOfPrintingOne(t *testing.T) {
	// "NaN" in a brief reads as a metric value. It is not one, and an agent must not
	// be left to interpret it.
	cases := map[string]struct {
		in   float64
		want string
	}{
		"nan":       {math.NaN(), "unavailable"},
		"+infinity": {math.Inf(1), "+infinity"},
		"-infinity": {math.Inf(-1), "-infinity"},
	}
	for name, c := range cases {
		if got := formatNumber(c.in); got != c.want {
			t.Fatalf("%s: formatNumber = %q, want %q", name, got, c.want)
		}
	}
}

func TestFormatNumberKeepsVeryLargeValuesOutOfExponentNotation(t *testing.T) {
	// A brief that says "1e+16" has told the reader nothing.
	got := formatNumber(1e16)
	if strings.ContainsAny(got, "eE") {
		t.Fatalf("expected plain digits, got %q", got)
	}
}

func TestTruncateRunesKeepsTheResultInsideTheLimit(t *testing.T) {
	// A caller passing a budget is stating what the result has to fit inside.
	cases := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"under the limit", "hello", 10, "hello"},
		{"exactly at the limit", "hello", 5, "hello"},
		{"over the limit", "hello", 4, "hel…"},
		{"limit of one", "hello", 1, "…"},
		{"limit of zero", "hello", 0, ""},
		{"negative limit", "hello", -3, ""},
		{"empty input", "", 5, ""},
	}
	for _, c := range cases {
		got := truncateRunes(c.in, c.limit)
		if got != c.want {
			t.Fatalf("%s: truncateRunes(%q, %d) = %q, want %q", c.name, c.in, c.limit, got, c.want)
		}
		if c.limit > 0 && len([]rune(got)) > c.limit {
			t.Fatalf("%s: result exceeds the limit: %d > %d", c.name, len([]rune(got)), c.limit)
		}
	}
}

func TestTruncateRunesCutsOnARuneBoundary(t *testing.T) {
	// Cutting bytes rather than runes would leave a mangled character in a prompt:
	// "héllo wörld" is 11 runes but 13 bytes.
	got := truncateRunes("héllo wörld", 6)
	if got != "héllo…" {
		t.Fatalf("expected a clean cut, got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid UTF-8, got %q", got)
	}
}
