package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/core/internal/repository"
)

// sessionsFixture wires the service over one fake, kept to hand so a test can read what
// was stored.
type sessionsFixture struct {
	sessions *Sessions
	store    *fakeSessions
}

func newSessionsFixture(t *testing.T, withJanitor bool) sessionsFixture {
	t.Helper()

	store := newFakeSessions()
	deps := SessionsDeps{
		Sessions: store,
		Revoker:  store,
		Clock:    fixedClock(testNow),
		Logger:   silentLogger(),
	}
	if withJanitor {
		deps.Janitor = store
	}
	sessions, err := NewSessions(deps)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	return sessionsFixture{sessions: sessions, store: store}
}

// seedSession puts one session row in the store, expiring at the given time.
func seedSession(t *testing.T, store *fakeSessions, userID string, expiresAt time.Time) repository.Session {
	t.Helper()

	session, err := store.Create(context.Background(), repository.NewSession{
		UserID:    userID,
		TokenHash: "hash-" + userID,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("seed a session for %s: %v", userID, err)
	}
	return session
}

func TestNewSessions_namesEveryMissingDependencyAtOnce(t *testing.T) {
	// Act
	_, err := NewSessions(SessionsDeps{})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want a complaint about the wiring")
	}
	for _, name := range []string{"Sessions", "Revoker"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
}

func TestNewSessions_withoutAJanitorIsAValidInstance(t *testing.T) {
	// Arrange — sweeping is hygiene; an expired session is refused at the door either way.
	store := newFakeSessions()

	// Act
	_, err := NewSessions(SessionsDeps{Sessions: store, Revoker: store})

	// Assert
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
}

func TestSessionsList_showsTheCallersOwnDevicesIncludingEndedOnes(t *testing.T) {
	// Arrange — two of the caller's, one of somebody else's, and one of the caller's
	// already revoked.
	f := newSessionsFixture(t, false)
	seedSession(t, f.store, "user-1", testNow.Add(time.Hour))
	revoked := seedSession(t, f.store, "user-1", testNow.Add(time.Hour))
	seedSession(t, f.store, "user-2", testNow.Add(time.Hour))
	if err := f.store.RevokeByID(context.Background(), "user-1", revoked.ID, testNow); err != nil {
		t.Fatalf("revoke the seeded session: %v", err)
	}

	// Act
	got, err := f.sessions.List(context.Background(), 10, 0, userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// The revoked row is included on purpose: "this laptop signed out last Tuesday" is what
	// somebody checking for a session they do not recognise needs to see.
	if len(got) != 2 {
		t.Fatalf("sessions = %d, want the caller's two", len(got))
	}
	for _, session := range got {
		if session.UserID != "user-1" {
			t.Errorf("userId = %q, want only the caller's rows", session.UserID)
		}
	}
}

func TestSessionsList_isNotSomethingAMachineKeyCanRead(t *testing.T) {
	// Arrange
	f := newSessionsFixture(t, false)

	for _, actor := range []Actor{operatorActor(), botActor()} {
		t.Run(string(actor.Type), func(t *testing.T) {
			// Act — a machine key has no account, so there is no "own devices" to answer with.
			_, err := f.sessions.List(context.Background(), 10, 0, actor)

			// Assert
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestSessionsRevoke_endsTheCallersOwnSessionAtTheClock(t *testing.T) {
	// Arrange
	f := newSessionsFixture(t, false)
	session := seedSession(t, f.store, "user-1", testNow.Add(time.Hour))

	// Act
	err := f.sessions.Revoke(context.Background(), session.ID, userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if f.store.rows[0].RevokedAt == nil {
		t.Fatal("the session is still live, want it revoked")
	}
	if got := *f.store.rows[0].RevokedAt; !got.Equal(testNow) {
		t.Errorf("revokedAt = %s, want the clock's %s", got, testNow)
	}
}

func TestSessionsRevoke_somebodyElsesSessionIsSimplyNotFound(t *testing.T) {
	// Arrange
	f := newSessionsFixture(t, false)
	session := seedSession(t, f.store, "user-2", testNow.Add(time.Hour))

	// Act
	err := f.sessions.Revoke(context.Background(), session.ID, userActor("user-1"))

	// Assert — the same answer as an id that never existed, which is the point: a
	// forbidden here would confirm that the id is real and belongs to somebody.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if f.store.rows[0].RevokedAt != nil {
		t.Error("somebody else's session was revoked")
	}
}

func TestSessionsRevoke_unknownIdAndEmptyIdAreDifferentAnswers(t *testing.T) {
	// Arrange
	f := newSessionsFixture(t, false)

	// Act
	empty := f.sessions.Revoke(context.Background(), "   ", userActor("user-1"))
	unknown := f.sessions.Revoke(context.Background(), "session-404", userActor("user-1"))

	// Assert — nothing was looked up in the first case, so it is a malformed request
	// rather than a missing row.
	if !errors.Is(empty, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", empty)
	}
	if !errors.Is(unknown, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", unknown)
	}
}

func TestSessionsRevoke_reportsAStoreFailureAsItself(t *testing.T) {
	// Arrange — a failure that is not absence must not be flattened into a 404, which
	// would tell the caller their session is gone when it is not.
	f := newSessionsFixture(t, false)
	f.store.failByID = errBoom

	// Act
	err := f.sessions.Revoke(context.Background(), "session-1", userActor("user-1"))

	// Assert
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a store failure was reported as a missing session")
	}
}

func TestSessionsRevokeAll_endsEveryOneOfTheCallersSessionsAndCountsThem(t *testing.T) {
	// Arrange
	f := newSessionsFixture(t, false)
	for range 3 {
		seedSession(t, f.store, "user-1", testNow.Add(time.Hour))
	}
	seedSession(t, f.store, "user-2", testNow.Add(time.Hour))

	// Act
	revoked, err := f.sessions.RevokeAll(context.Background(), userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if revoked != 3 {
		t.Errorf("revoked = %d, want 3", revoked)
	}
	// Including the session making the request: this is the button somebody presses at a
	// shared machine, and sparing the current device would leave the one they meant.
	if live := f.store.live("user-1"); live != 0 {
		t.Errorf("live sessions = %d, want none", live)
	}
	if live := f.store.live("user-2"); live != 1 {
		t.Errorf("somebody else's live sessions = %d, want 1 untouched", live)
	}
}

func TestSessionsRevokeAll_isNotSomethingAMachineKeyCanDo(t *testing.T) {
	// Arrange
	f := newSessionsFixture(t, false)

	// Act
	_, err := f.sessions.RevokeAll(context.Background(), botActor())

	// Assert
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestSessionsSweep_withoutAJanitorDoesNothingAndSaysSo(t *testing.T) {
	// Arrange
	f := newSessionsFixture(t, false)

	// Act
	deleted, err := f.sessions.Sweep(context.Background())

	// Assert — not an error: an instance that keeps its expired rows is untidy, not unsafe.
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
}

func TestSessionsSweep_deletesOnlyWhatHasAlreadyExpired(t *testing.T) {
	// Arrange
	f := newSessionsFixture(t, true)
	seedSession(t, f.store, "user-1", testNow.Add(-time.Minute))
	seedSession(t, f.store, "user-1", testNow.Add(-24*time.Hour))
	seedSession(t, f.store, "user-1", testNow.Add(time.Minute))

	// Act
	deleted, err := f.sessions.Sweep(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want the two that had expired", deleted)
	}
	if len(f.store.rows) != 1 {
		t.Errorf("rows left = %d, want the one still valid", len(f.store.rows))
	}
}

func TestSessionsSweep_wrapsAJanitorFailureWithWhatItWasDoing(t *testing.T) {
	// Arrange
	f := newSessionsFixture(t, true)
	f.store.failDelete = errBoom

	// Act
	deleted, err := f.sessions.Sweep(context.Background())

	// Assert
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the janitor's failure", err)
	}
	if !strings.Contains(err.Error(), "sweep expired sessions") {
		t.Errorf("err = %v, want it to name what failed", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
}
