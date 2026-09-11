package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

func newAuditFixture(t *testing.T) (*AuditLog, *fakeAuditReader) {
	t.Helper()
	reader := &fakeAuditReader{}
	log, err := NewAuditLog(AuditLogDeps{Reader: reader, Clock: fixedClock(testNow)})
	if err != nil {
		t.Fatalf("expected an audit reader, got %v", err)
	}
	return log, reader
}

func TestNewAuditLogRequiresAReaderAndDefaultsTheClock(t *testing.T) {
	if _, err := NewAuditLog(AuditLogDeps{}); err == nil ||
		!strings.Contains(err.Error(), "Reader") {
		t.Fatalf("expected the missing dependency named, got %v", err)
	}

	log, err := NewAuditLog(AuditLogDeps{Reader: &fakeAuditReader{}})
	if err != nil {
		t.Fatalf("expected an audit reader, got %v", err)
	}
	if log.clock == nil {
		t.Fatal("expected a default clock")
	}
}

func TestAuditListBoundsTheWindowSoNobodyScansTheWholeLog(t *testing.T) {
	// The audit table grows forever. A query with no lower bound is a table scan of
	// everything the engine has ever done.
	log, reader := newAuditFixture(t)

	if _, _, err := log.List(context.Background(), repository.AuditFilter{}); err != nil {
		t.Fatalf("expected a page, got %v", err)
	}
	want := testNow.Add(-maxAuditWindow)
	if got := reader.last().Since; !got.Equal(want) {
		t.Fatalf("expected the window to start at %s, got %s", want, got)
	}
}

func TestAuditListKeepsAnExplicitWindow(t *testing.T) {
	log, reader := newAuditFixture(t)
	since := testNow.Add(-2 * time.Hour)
	until := testNow.Add(-time.Hour)

	if _, _, err := log.List(context.Background(), repository.AuditFilter{Since: since, Until: until}); err != nil {
		t.Fatalf("expected a page, got %v", err)
	}
	got := reader.last()
	if !got.Since.Equal(since) || !got.Until.Equal(until) {
		t.Fatalf("expected the caller's window, got %s..%s", got.Since, got.Until)
	}
}

func TestAuditListRejectsAWindowThatEndsBeforeItStarts(t *testing.T) {
	log, reader := newAuditFixture(t)

	_, _, err := log.List(context.Background(), repository.AuditFilter{
		Since: testNow,
		Until: testNow.Add(-time.Hour),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if len(reader.filters) != 0 {
		t.Fatal("expected no query for an impossible window")
	}
}

func TestAuditListAcceptsAWindowOfZeroLength(t *testing.T) {
	// A point query is a reasonable thing to ask for.
	log, _ := newAuditFixture(t)

	if _, _, err := log.List(context.Background(), repository.AuditFilter{
		Since: testNow,
		Until: testNow,
	}); err != nil {
		t.Fatalf("expected a page, got %v", err)
	}
}

func TestAuditListRejectsAnActorTypeThatIsNotOne(t *testing.T) {
	log, reader := newAuditFixture(t)

	_, _, err := log.List(context.Background(), repository.AuditFilter{ActorType: "administrator"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if field := ValidationFields(err)["actorType"]; field == "" {
		t.Fatalf("expected the error to name actorType, got %v", ValidationFields(err))
	}
	if len(reader.filters) != 0 {
		t.Fatal("expected no query for an impossible filter")
	}
}

func TestAuditListAcceptsEveryRealActorType(t *testing.T) {
	for _, actorType := range []repository.ActorType{
		repository.ActorSystem, repository.ActorUser, repository.ActorBot,
	} {
		log, _ := newAuditFixture(t)
		if _, _, err := log.List(context.Background(), repository.AuditFilter{ActorType: actorType}); err != nil {
			t.Fatalf("%s: %v", actorType, err)
		}
	}
}

func TestAuditListClampsThePageSize(t *testing.T) {
	log, reader := newAuditFixture(t)

	if _, _, err := log.List(context.Background(), repository.AuditFilter{
		Limit:  maxPageLimit + 1000,
		Offset: -1,
	}); err != nil {
		t.Fatalf("expected a page, got %v", err)
	}
	got := reader.last()
	if got.Limit != maxPageLimit || got.Offset != 0 {
		t.Fatalf("expected the page clamped, got limit %d offset %d", got.Limit, got.Offset)
	}
}

func TestAuditListReturnsTheEntriesAndTheTotal(t *testing.T) {
	log, reader := newAuditFixture(t)
	reader.events = []repository.AuditEvent{{
		ID:          7,
		At:          testNow,
		ActorType:   repository.ActorUser,
		ActorID:     "ops",
		Action:      ActionGoalUpdated,
		SubjectType: SubjectGoal,
		SubjectID:   "goal-1",
	}}
	reader.total = 300

	events, total, err := log.List(context.Background(), repository.AuditFilter{
		Action:    ActionGoalUpdated,
		SubjectID: "goal-1",
	})
	if err != nil {
		t.Fatalf("expected a page, got %v", err)
	}
	if len(events) != 1 || total != 300 {
		t.Fatalf("expected 1 entry of 300, got %d of %d", len(events), total)
	}
	if got := reader.last(); got.Action != ActionGoalUpdated || got.SubjectID != "goal-1" {
		t.Fatalf("filter did not survive: %+v", got)
	}
}

func TestAuditListReportsAStorageFailure(t *testing.T) {
	log, reader := newAuditFixture(t)
	reader.err = errors.New("statement timeout")

	if _, _, err := log.List(context.Background(), repository.AuditFilter{}); err == nil ||
		!strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("expected the storage error, got %v", err)
	}
}
