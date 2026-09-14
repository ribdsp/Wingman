package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/provider"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// The prompt. Tested because three of the things it says are safety properties said in
// words — what the bounds are, whether anybody is watching, and which capabilities are
// missing — and because two of them are about what it must *not* say.

func TestSystemPrompt_statesTheBoundsTheRunIsActuallyHeldTo(t *testing.T) {
	// Arrange
	// The numbers, not "you are limited". A model told it has ten turns spends them
	// differently from one told to be brief, and the bounds hold whether or not it knows
	// them — so leaving them out only removes the chance of planning.
	in := testInput()
	in.Tools = &fakeTools{}
	in.Run.Limits.MaxIterations = 7
	in.Run.Limits.MaxToolCalls = 21
	in.Run.Limits.MaxTokensPerRun = 123_456

	// Act
	got := systemPrompt(in)

	// Assert
	for _, want := range []string{"7", "21", "123456"} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt does not state the bound %q:\n%s", want, got)
		}
	}
}

func TestSystemPrompt_isTheSameForEveryTurnOfOneRun(t *testing.T) {
	// Arrange
	// The prompt is the cached prefix at both vendors. A counter in it — "you have used
	// 4 of 10 turns" — changes the prefix on every turn and bills the whole thing again
	// each time. The counters are the ladder's business, and it stops the run without
	// needing the model's cooperation.
	first := testInput()
	first.Tools = &fakeTools{}

	later := first
	later.Run.State = domain.RunState{Iterations: 6, ToolCalls: 14, TokensUsed: 90_000}

	// Act, Assert
	if systemPrompt(first) != systemPrompt(later) {
		t.Error("the prompt changed between iterations of one run; the cached prefix is lost and something in it is a counter")
	}
}

func TestSystemPrompt_saysWhetherAnybodyIsWatching(t *testing.T) {
	tests := []struct {
		name       string
		source     domain.TaskSource
		wantSaid   string
		wantUnsaid string
	}{
		{
			name:   "a person asked",
			source: domain.TaskSourceUser,
			// It may ask a question, because somebody is there to answer it.
			wantSaid:   "waiting",
			wantUnsaid: "Nobody is watching",
		},
		{
			name:   "a metric moved",
			source: domain.TaskSourceGoalEngine,
			// And it is told the consequence, not just the fact: a model that keeps
			// retrying a refused write spends the whole run learning the same answer.
			wantSaid:   "Do not retry a refused call",
			wantUnsaid: "they can answer a question",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			in := testInput()
			in.Tools = &fakeTools{}
			in.Task.Source = test.source

			// Act
			got := systemPrompt(in)

			// Assert
			if !strings.Contains(got, test.wantSaid) {
				t.Errorf("the prompt does not say %q:\n%s", test.wantSaid, got)
			}
			if strings.Contains(got, test.wantUnsaid) {
				t.Errorf("the prompt says %q, which belongs to the other kind of run", test.wantUnsaid)
			}
		})
	}
}

func TestSystemPrompt_doesNotNameTheWorkspaceDirectory(t *testing.T) {
	// Arrange
	// The model does not need the host path to use a relative one, and a prompt is the
	// part of a run most likely to be pasted into a bug report. The path carries a user
	// id, which is why it is described rather than printed.
	in := testInput()
	in.Tools = &fakeTools{}
	in.Workspace = "/srv/wingman/workspaces/user_1"

	// Act
	got := systemPrompt(in)

	// Assert
	if strings.Contains(got, in.Workspace) {
		t.Errorf("the prompt names the workspace path:\n%s", got)
	}
	if !strings.Contains(got, "working directory") {
		t.Errorf("the prompt does not mention a working directory at all:\n%s", got)
	}
}

func TestSystemPrompt_namesTheToolsTheRunHasNot(t *testing.T) {
	// Arrange
	// A task written against a capability that is missing should be reported as blocked,
	// not attempted three times. Sorted, so one run's prompt equals another's.
	in := testInput()
	in.Tools = &fakeTools{
		unavailable: []tool.RunnerFailure{
			{Label: "zendesk-mcp", Err: errors.New("dial https://user:hunter2@mcp.internal/sse: connection refused")},
			{Label: "accounting-api", Err: errors.New("rejected the configured credential")},
		},
		conflicting: []tool.Conflict{
			{Name: "send_email", Labels: []string{"gmail-mcp", "smtp-api"}},
		},
	}

	// Act
	got := systemPrompt(in)

	// Assert
	if !strings.Contains(got, "accounting-api") || !strings.Contains(got, "zendesk-mcp") {
		t.Errorf("the prompt does not name the sources that failed:\n%s", got)
	}
	if i, j := strings.Index(got, "accounting-api"), strings.Index(got, "zendesk-mcp"); i > j {
		t.Error("the failed sources are not sorted; the prompt differs run to run for no reason")
	}
	if !strings.Contains(got, "send_email") {
		t.Errorf("the prompt does not name the withdrawn tool:\n%s", got)
	}

	// The labels, never the errors. A source's failure text quotes the URL it was
	// configured with, and that URL can carry a credential — this is the one assertion
	// in this file that is a secrets test.
	for _, leak := range []string{"hunter2", "mcp.internal", "connection refused", "rejected the configured credential"} {
		if strings.Contains(got, leak) {
			t.Errorf("the prompt quotes a source's error text (%q); a prompt is not the place for it:\n%s", leak, got)
		}
	}
}

func TestSystemPrompt_saysNothingAboutAbsentToolsWhenNoneAreAbsent(t *testing.T) {
	// Arrange
	// The common case, and it should read as though the feature does not exist.
	in := testInput()
	in.Tools = &fakeTools{}

	// Act
	got := systemPrompt(in)

	// Assert
	if strings.Contains(got, "could not be reached") || strings.Contains(got, "withdrawn") {
		t.Errorf("the prompt talks about missing tools when none are missing:\n%s", got)
	}
}

func TestOpeningMessages_carryTheBriefAsAUserTurn(t *testing.T) {
	// Arrange
	// The brief arrives from a chat, a channel or a goal engine trigger. It is data, and
	// data does not belong in the part of the prompt that grants permissions.
	task := domain.Task{Brief: "  find out why last week's invoices are late  "}

	// Act
	got := openingMessages(task)

	// Assert
	if len(got) != 1 {
		t.Fatalf("openingMessages() returned %d messages; want one", len(got))
	}
	if got[0].Role != provider.RoleUser {
		t.Errorf("role = %q; want %q", got[0].Role, provider.RoleUser)
	}
	if got[0].Text != "find out why last week's invoices are late" {
		t.Errorf("text = %q; want the brief, trimmed", got[0].Text)
	}
}

func TestOpeningMessages_carryTheDispatchersContextAsFacts(t *testing.T) {
	// Arrange
	// The goal engine sends this to say which goal caused the run — exactly the context a
	// brief like "revenue is behind pace, do something" is missing.
	task := domain.Task{
		Brief: "revenue is behind pace",
		Metadata: map[string]string{
			"metric": "revenue.daily",
			"goalId": "goal_7",
			// Skipped: an empty value is a key nobody filled in, and a bare label in a
			// prompt reads as a fact with no value rather than as an absence.
			"note":   "   ",
			"target": "50000000",
		},
	}

	// Act
	got := openingMessages(task)

	// Assert
	if len(got) != 1 {
		t.Fatalf("openingMessages() returned %d messages; want one", len(got))
	}
	text := got[0].Text

	// Labelled, because metadata is a map somebody else filled in and a value in it that
	// reads like an order is still a value.
	if !strings.Contains(text, "not as instructions") {
		t.Errorf("the context is not labelled as facts:\n%s", text)
	}
	for _, want := range []string{"goalId: goal_7", "metric: revenue.daily", "target: 50000000"} {
		if !strings.Contains(text, want) {
			t.Errorf("the context does not carry %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "note") {
		t.Errorf("the context carries a key with no value:\n%s", text)
	}
	// Sorted: the same task has to produce the same prompt twice.
	if i, j := strings.Index(text, "goalId"), strings.Index(text, "metric"); i > j {
		t.Errorf("the context is not sorted:\n%s", text)
	}
	// The brief comes first. It is what was asked; the rest is why.
	if !strings.HasPrefix(text, task.Brief) {
		t.Errorf("the message does not open with the brief:\n%s", text)
	}
}

func TestOpeningMessages_metadataThatSaysNothing_addsNothing(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
	}{
		{name: "no metadata", metadata: nil},
		{name: "empty metadata", metadata: map[string]string{}},
		{name: "only blank values", metadata: map[string]string{"note": "", "other": "  "}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			task := domain.Task{Brief: "check the invoices", Metadata: test.metadata}

			// Act
			got := openingMessages(task)

			// Assert
			if len(got) != 1 || got[0].Text != "check the invoices" {
				t.Errorf("messages = %+v; want the brief and nothing appended", got)
			}
		})
	}
}
