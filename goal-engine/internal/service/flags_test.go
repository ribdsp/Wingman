package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

type flagsFixture struct {
	flags *Flags
	admin *fakeFlagAdmin
	audit *fakeAudit
}

func newFlagsFixture(t *testing.T) *flagsFixture {
	t.Helper()
	f := &flagsFixture{
		admin: newFakeFlagAdmin(),
		audit: &fakeAudit{},
	}
	flags, err := NewFlags(FlagsDeps{
		Flags: f.admin,
		Audit: f.audit,
		Clock: fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("expected a flag service, got %v", err)
	}
	f.flags = flags
	return f
}

func TestNewFlagsNamesEveryMissingDependencyAndDefaultsTheClock(t *testing.T) {
	_, err := NewFlags(FlagsDeps{})
	if err == nil {
		t.Fatal("expected construction to fail")
	}
	for _, name := range []string{"Flags", "Audit"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("expected %q in %q", name, err)
		}
	}

	flags, err := NewFlags(FlagsDeps{Flags: newFakeFlagAdmin(), Audit: &fakeAudit{}})
	if err != nil {
		t.Fatalf("expected a flag service, got %v", err)
	}
	if flags.clock == nil {
		t.Fatal("expected a default clock")
	}
}

func TestKillSwitchReadsAsNotEngagedOnASystemWhereItWasNeverThrown(t *testing.T) {
	// No row means nobody ever pulled it. This is an operator reading the state, not
	// the engine deciding whether it may act — the engine's own read treats any
	// doubt as engaged.
	f := newFlagsFixture(t)

	flag, err := f.flags.KillSwitch(context.Background())
	if err != nil {
		t.Fatalf("expected a readable switch, got %v", err)
	}
	if flag.Enabled {
		t.Fatal("expected the switch to read as not engaged")
	}
	if flag.Key != repository.KillSwitchKey {
		t.Fatalf("expected the switch key, got %q", flag.Key)
	}
}

func TestKillSwitchReportsAnUnreadableSwitchRatherThanGuessing(t *testing.T) {
	f := newFlagsFixture(t)
	f.admin.getErr = errors.New("connection refused")

	if _, err := f.flags.KillSwitch(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected the storage error, got %v", err)
	}
}

func TestSetKillSwitchRecordsWhoStoppedTheEngineAndWhy(t *testing.T) {
	// This is the entry somebody will read while working out why the engine went
	// quiet.
	f := newFlagsFixture(t)

	flag, err := f.flags.SetKillSwitch(context.Background(), true, "agent opened three duplicate campaigns", testActor())
	if err != nil {
		t.Fatalf("expected the switch to be thrown, got %v", err)
	}
	if !flag.Enabled || flag.UpdatedBy != "ops" {
		t.Fatalf("unexpected flag: %+v", flag)
	}
	if flag.Reason != "agent opened three duplicate campaigns" {
		t.Fatalf("expected the reason stored, got %q", flag.Reason)
	}

	if len(f.audit.events) != 1 {
		t.Fatalf("expected one audit entry, got %d", len(f.audit.events))
	}
	event := f.audit.events[0]
	if event.Action != ActionKillSwitchSet || event.SubjectType != SubjectFlag {
		t.Fatalf("unexpected audit entry: %+v", event)
	}
	if event.SubjectID != repository.KillSwitchKey || event.Outcome != "engaged" {
		t.Fatalf("unexpected audit subject or outcome: %+v", event)
	}
	if event.ActorID != "ops" || event.RequestID != "req-1" {
		t.Fatalf("expected the actor on the entry, got %+v", event)
	}
	if !strings.Contains(event.Detail, "duplicate campaigns") {
		t.Fatalf("expected the reason in the detail, got %s", event.Detail)
	}
}

func TestSetKillSwitchRecordsAReleaseAsDistinctFromAnEngage(t *testing.T) {
	// Restarting the engine is as much an event as stopping it.
	f := newFlagsFixture(t)

	flag, err := f.flags.SetKillSwitch(context.Background(), false, "duplicates cleaned up, resuming", testActor())
	if err != nil {
		t.Fatalf("expected the switch to be released, got %v", err)
	}
	if flag.Enabled {
		t.Fatal("expected the switch released")
	}
	if got := f.audit.events[0].Outcome; got != "released" {
		t.Fatalf("expected a released outcome, got %q", got)
	}
}

func TestSetKillSwitchRequiresAReasonAndAnActor(t *testing.T) {
	// An unexplained stop is the entry that wastes somebody's morning.
	f := newFlagsFixture(t)

	if _, err := f.flags.SetKillSwitch(context.Background(), true, "   ", testActor()); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a reason to be required, got %v", err)
	}
	if _, err := f.flags.SetKillSwitch(context.Background(), true, "because", Actor{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected an actor to be required, got %v", err)
	}
	if len(f.admin.sets) != 0 {
		t.Fatal("expected nothing written")
	}
	if len(f.audit.events) != 0 {
		t.Fatal("expected no audit entry")
	}
}

func TestSetKillSwitchTrimsAndBoundsTheReason(t *testing.T) {
	f := newFlagsFixture(t)

	flag, err := f.flags.SetKillSwitch(context.Background(), true, "  "+strings.Repeat("a", maxFlagReasonLength+200)+"  ", testActor())
	if err != nil {
		t.Fatalf("expected the switch to be thrown, got %v", err)
	}
	if got := len([]rune(flag.Reason)); got != maxFlagReasonLength {
		t.Fatalf("expected the reason capped at %d runes, got %d", maxFlagReasonLength, got)
	}
	if !strings.HasSuffix(flag.Reason, "…") {
		t.Fatal("expected the truncation to be visible")
	}
}

func TestSetKillSwitchReportsAFailedWrite(t *testing.T) {
	// If the switch could not be stored, the caller has to know: they are standing
	// there believing the engine has stopped.
	f := newFlagsFixture(t)
	f.admin.setErr = errors.New("read-only transaction")

	if _, err := f.flags.SetKillSwitch(context.Background(), true, "stop", testActor()); err == nil ||
		!strings.Contains(err.Error(), "read-only transaction") {
		t.Fatalf("expected the storage error, got %v", err)
	}
}

func TestSetKillSwitchStandsEvenWhenTheAuditWriteFails(t *testing.T) {
	// The switch is already thrown. Reporting failure would make an operator think
	// the engine is still running.
	f := newFlagsFixture(t)
	f.audit.err = errors.New("audit table is full")

	flag, err := f.flags.SetKillSwitch(context.Background(), true, "stop everything", testActor())
	if err != nil {
		t.Fatalf("expected the flip to stand, got %v", err)
	}
	if !flag.Enabled {
		t.Fatal("expected the switch engaged")
	}
	if len(f.admin.sets) != 1 {
		t.Fatal("expected the write to have happened")
	}
}

func TestFlagsListReturnsEveryFlagAndReportsAFailure(t *testing.T) {
	f := newFlagsFixture(t)
	if _, err := f.flags.SetKillSwitch(context.Background(), true, "stop", testActor()); err != nil {
		t.Fatalf("expected the switch to be thrown, got %v", err)
	}

	flags, err := f.flags.List(context.Background())
	if err != nil {
		t.Fatalf("expected the flags, got %v", err)
	}
	if len(flags) != 1 || flags[0].Key != repository.KillSwitchKey {
		t.Fatalf("unexpected flags: %+v", flags)
	}

	f.admin.listErr = errors.New("statement timeout")
	if _, err := f.flags.List(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("expected the storage error, got %v", err)
	}
}

func TestSetKillSwitchLetsAnAgentStopTheFleet(t *testing.T) {
	// The brakes have to be reachable by whoever notices the problem first. An agent
	// that has worked out it is doing damage should be able to stop everything; the
	// worst case is an outage a human can undo in one call.
	f := newFlagsFixture(t)

	flag, err := f.flags.SetKillSwitch(context.Background(), true, "I am opening duplicate campaigns", testBot())
	if err != nil {
		t.Fatalf("expected an agent to be able to engage the switch, got %v", err)
	}
	if !flag.Enabled {
		t.Fatal("expected the switch engaged")
	}
	if got := f.audit.events[0].ActorType; got != repository.ActorBot {
		t.Fatalf("expected the entry to name the agent, got %q", got)
	}
}

func TestSetKillSwitchRefusesToLetAnAgentReleaseItsOwnBrakes(t *testing.T) {
	// The asymmetry is the point. A switch an agent can turn off is not a switch:
	// the case it exists for is precisely the one where the agent's judgement is
	// what went wrong.
	f := newFlagsFixture(t)
	if _, err := f.flags.SetKillSwitch(context.Background(), true, "stop", testActor()); err != nil {
		t.Fatalf("expected the switch to be thrown, got %v", err)
	}

	_, err := f.flags.SetKillSwitch(context.Background(), false, "I feel fine now", testBot())
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}

	flag, err := f.flags.KillSwitch(context.Background())
	if err != nil {
		t.Fatalf("expected a readable switch, got %v", err)
	}
	if !flag.Enabled {
		t.Fatal("expected the switch still engaged after a refused release")
	}
}

func TestKillSwitchReadsBackWhatSetKillSwitchWrote(t *testing.T) {
	f := newFlagsFixture(t)
	if _, err := f.flags.SetKillSwitch(context.Background(), true, "stop", testActor()); err != nil {
		t.Fatalf("expected the switch to be thrown, got %v", err)
	}

	flag, err := f.flags.KillSwitch(context.Background())
	if err != nil {
		t.Fatalf("expected a readable switch, got %v", err)
	}
	if !flag.Enabled || flag.Reason != "stop" || flag.UpdatedBy != "ops" {
		t.Fatalf("unexpected flag: %+v", flag)
	}
}
