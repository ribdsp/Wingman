package service

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
)

// maxSourceTextInBrief bounds how much of the operator's original phrasing is
// quoted, so one pathological goal cannot produce an unbounded prompt.
const maxSourceTextInBrief = 2000

// BuildBrief writes the instruction an agent is woken with.
//
// It is plain text on purpose: this is read by a language model and by whichever
// human is watching the channel, and both need the same numbers. The constraints
// section is not decoration — it tells the agent that spending goes through the
// approval API, which is the boundary this whole service exists to hold.
func BuildBrief(goal domain.Goal, ev domain.Evaluation, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Goal %q for %s is off pace and needs attention.\n\n", goal.Title, goal.Product)

	if source := strings.TrimSpace(goal.SourceText); source != "" {
		b.WriteString("What the operator asked for:\n  ")
		b.WriteString(truncateRunes(source, maxSourceTextInBrief))
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "Where it stands as of %s:\n", ev.EvaluatedAt.In(loc).Format(time.RFC3339))
	fmt.Fprintf(&b, "  metric        %s\n", goal.MetricKey)
	fmt.Fprintf(&b, "  observed      %s\n", formatNumber(ev.ObservedValue))
	fmt.Fprintf(&b, "  expected now  %s\n", formatNumber(ev.ExpectedValue))
	fmt.Fprintf(&b, "  target        %s (%s) by %s\n",
		formatNumber(ev.TargetValue), comparatorPhrase(goal.Comparator), goal.PeriodEnd.In(loc).Format(time.RFC3339))
	fmt.Fprintf(&b, "  elapsed       %.0f%% of the period\n", ev.ElapsedRatio*100)
	fmt.Fprintf(&b, "  pace          %.2f (1.00 is exactly on schedule)\n\n", ev.PaceRatio)

	fmt.Fprintf(&b, "Why you were woken:\n  %s\n\n", ev.Reason)

	b.WriteString("What to do:\n")
	b.WriteString("  1. Work out why the metric is behind.\n")
	b.WriteString("  2. Propose the smallest set of actions that closes the gap.\n")
	b.WriteString("  3. Carry out what you are cleared to carry out, then report back in this channel.\n\n")

	b.WriteString("Constraints:\n")
	b.WriteString("  - Any action that spends money must be submitted to the Wingman approval API first.\n")
	b.WriteString("    Do not spend, commit, or authorise funds on your own judgement.\n")
	b.WriteString("  - Do not edit the goal. If the target or the deadline looks wrong, say so instead.\n")

	return b.String()
}

// comparatorPhrase renders a comparator for a human reader.
func comparatorPhrase(c domain.Comparator) string {
	if c == domain.ComparatorLTE {
		return "stay at or below"
	}
	return "reach at least"
}

// formatNumber renders a metric value for a brief. Money and counts are the
// common cases, so thousands are grouped and whole numbers lose their decimals —
// "41,000,000" is legible where "4.1e+07" is not.
func formatNumber(v float64) string {
	if math.IsNaN(v) {
		return "unavailable"
	}
	if math.IsInf(v, 1) {
		return "+infinity"
	}
	if math.IsInf(v, -1) {
		return "-infinity"
	}

	decimals := 2
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		decimals = 0
	}
	formatted := strconv.FormatFloat(v, 'f', decimals, 64)

	sign := ""
	if strings.HasPrefix(formatted, "-") {
		sign, formatted = "-", formatted[1:]
	}
	intPart, fracPart, hasFrac := strings.Cut(formatted, ".")
	grouped := groupThousands(intPart)
	if hasFrac {
		return sign + grouped + "." + fracPart
	}
	return sign + grouped
}

// groupThousands inserts commas every three digits from the right.
func groupThousands(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// truncateRunes shortens s to at most limit runes, cutting on a rune boundary so
// the result stays valid UTF-8.
//
// The ellipsis is counted against the limit rather than added on top of it: a
// caller that passes a column width or a prompt budget is stating what the result
// has to fit inside, not what it may exceed by one.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit < 1 {
		return ""
	}
	return string(runes[:limit-1]) + "…"
}
