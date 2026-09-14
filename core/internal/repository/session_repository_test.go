package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// testTokenHash is a stored session token: the SHA-256 hex digest, never the token.
const testTokenHash = "3d2f0c4a1b8e7d6c5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e"

func sessionRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "user_id", "user_agent", "created_ip",
		"created_at", "expires_at", "revoked_at", "last_seen_at",
	})
}

func TestSessionRepository_create_storesAParsedAddressAndNothingElse(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	expiry := fixedNow.Add(24 * time.Hour)
	mock.ExpectQuery(`INSERT INTO sessions`).
		WithArgs("usr_01", testTokenHash, "curl/8", "203.0.113.7", expiry).
		WillReturnRows(sessionRows().
			AddRow("ses_01", "usr_01", "curl/8", "203.0.113.7", fixedNow, expiry, nil, nil))

	// Act
	session, err := repo.Create(context.Background(), NewSession{
		UserID: "usr_01", TokenHash: testTokenHash,
		UserAgent: "curl/8", CreatedIP: "203.0.113.7", ExpiresAt: expiry,
	})

	// Assert
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session.RevokedAt != nil {
		t.Error("a new session came back revoked")
	}
	if !session.Live(fixedNow) {
		t.Error("a new session was not live at creation")
	}
}

func TestSessionRepository_create_unparseableAddressBecomesNull(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	expiry := fixedNow.Add(time.Hour)
	mock.ExpectQuery(`INSERT INTO sessions`).
		WithArgs("usr_01", testTokenHash, "", nil, expiry).
		WillReturnRows(sessionRows().
			AddRow("ses_01", "usr_01", "", nil, fixedNow, expiry, nil, nil))

	// Act
	session, err := repo.Create(context.Background(), NewSession{
		UserID: "usr_01", TokenHash: testTokenHash,
		CreatedIP: "not-an-address, 10.0.0.1", ExpiresAt: expiry,
	})

	// Assert
	// The address is recorded for a person to recognise, so a missing one costs
	// nothing. A failed insert would cost them their sign-in.
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session.CreatedIP != "" {
		t.Errorf("createdIp = %q; want empty", session.CreatedIP)
	}
}

func TestSessionRepository_create_refusesASessionWithNoExpiry(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewSessionRepository(db)

	// Act
	_, noExpiry := repo.Create(context.Background(), NewSession{
		UserID: "usr_01", TokenHash: testTokenHash,
	})
	_, noToken := repo.Create(context.Background(), NewSession{
		UserID: "usr_01", ExpiresAt: fixedNow.Add(time.Hour),
	})

	// Assert
	// A session with no expiry is a permanent credential handed out by a form. Neither
	// call is expected to query, and the mock fails the test if one did.
	if noExpiry == nil {
		t.Error("a session with no expiry was accepted")
	}
	if noToken == nil {
		t.Error("a session with no token hash was accepted")
	}
}

// The scoping this test asserts is the whole authentication rule: a live session is one
// that is unrevoked, unexpired, and belongs to an account that is still active.
func TestSessionRepository_resolve_requiresLiveSessionAndActiveAccount(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	expiry := fixedNow.Add(time.Hour)
	mock.ExpectQuery(`WHERE s.token_hash = \$1\s+AND s.revoked_at IS NULL\s+AND s.expires_at > \$2\s+AND u.is_active`).
		WithArgs(testTokenHash, fixedNow).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "display_name", "email", "expires_at"}).
			AddRow("ses_01", "usr_01", "Owner", "owner@example.com", expiry))

	// Act
	live, err := repo.Resolve(context.Background(), testTokenHash, fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	if live.UserID != "usr_01" || live.Email != "owner@example.com" {
		t.Errorf("resolved to %+v; want the session's owner", live)
	}
}

func TestSessionRepository_resolve_emptyTokenIsNotFoundWithoutQuerying(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewSessionRepository(db)

	// Act
	_, err := repo.Resolve(context.Background(), "", fixedNow)

	// Assert
	// An unauthenticated request must not cost a round trip, or an unauthenticated
	// flood costs one connection per request.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve empty token = %v; want ErrNotFound", err)
	}
}

func TestSessionRepository_resolve_unknownTokenIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	mock.ExpectQuery(`FROM sessions s`).
		WithArgs(testTokenHash, fixedNow).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "display_name", "email", "expires_at"}))

	// Act
	_, err := repo.Resolve(context.Background(), testTokenHash, fixedNow)

	// Assert
	// Unknown, expired, revoked and deactivated all answer the same. Telling somebody
	// their token was "expired" rather than "unknown" confirms it was once real.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve unknown token = %v; want ErrNotFound", err)
	}
}

func TestSessionRepository_resolve_databaseFailureIsNotAWrongToken(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	mock.ExpectQuery(`FROM sessions s`).
		WithArgs(testTokenHash, fixedNow).
		WillReturnError(errors.New("connection reset"))

	// Act
	_, err := repo.Resolve(context.Background(), testTokenHash, fixedNow)

	// Assert
	if err == nil {
		t.Fatal("a database failure resolved successfully")
	}
	// The caller answers 401 for one and 503 for the other. Folding an outage into
	// ErrNotFound would sign every user out during a blip.
	if errors.Is(err, ErrNotFound) {
		t.Errorf("an outage was reported as an unknown token: %v", err)
	}
}

func TestSessionRepository_touchLastSeen_aRevokedSessionIsNotAnError(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	mock.ExpectExec(`UPDATE sessions SET last_seen_at = \$2 WHERE token_hash = \$1 AND revoked_at IS NULL`).
		WithArgs(testTokenHash, fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.TouchLastSeen(context.Background(), testTokenHash, fixedNow)

	// Assert
	// A session revoked between authenticating and this update is a race with a correct
	// outcome, not a failure to report on a request that already succeeded.
	if err != nil {
		t.Fatalf("touch last seen: %v", err)
	}
}

// RevokeByID names the user id in the WHERE clause rather than comparing it in Go
// afterwards. A guessed session id must not be revocable, and a query that cannot touch
// another account's row is a stronger guarantee than a comparison a refactor can drop.
func TestSessionRepository_revokeByID_scopesTheUpdateToTheOwner(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	mock.ExpectExec(`UPDATE sessions SET revoked_at = \$3\s+WHERE id = \$2 AND user_id = \$1 AND revoked_at IS NULL`).
		WithArgs("usr_01", "ses_01", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Act
	err := repo.RevokeByID(context.Background(), "usr_01", "ses_01", fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("revoke session: %v", err)
	}
}

func TestSessionRepository_revokeByID_anotherAccountsSessionIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	mock.ExpectExec(`UPDATE sessions SET revoked_at`).
		WithArgs("usr_02", "ses_01", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.RevokeByID(context.Background(), "usr_02", "ses_01", fixedNow)

	// Assert
	// Not yours and not there answer the same, so a caller learns nothing about an id
	// they do not own.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke another account's session = %v; want ErrNotFound", err)
	}
}

func TestSessionRepository_revokeByToken_alreadyRevokedIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	mock.ExpectExec(`UPDATE sessions SET revoked_at = \$2`).
		WithArgs(testTokenHash, fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.RevokeByToken(context.Background(), testTokenHash, fixedNow)

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke revoked session = %v; want ErrNotFound", err)
	}
}

func TestSessionRepository_revokeAllForUser_reportsHowManyEnded(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	mock.ExpectExec(`UPDATE sessions SET revoked_at = \$2\s+WHERE user_id = \$1 AND revoked_at IS NULL`).
		WithArgs("usr_01", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 3))

	// Act
	ended, err := repo.RevokeAllForUser(context.Background(), "usr_01", fixedNow)

	// Assert
	// This is what a password change runs. The count is what tells somebody how many
	// devices were signed out, which is how they notice one they did not recognise.
	if err != nil {
		t.Fatalf("revoke all sessions: %v", err)
	}
	if ended != 3 {
		t.Errorf("ended %d sessions; want 3", ended)
	}
}

func TestSessionRepository_listForUser_includesRevokedAndExpiredRows(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	revoked := fixedNow.Add(-time.Hour)
	mock.ExpectQuery(`FROM sessions\s+WHERE user_id = \$1`).
		WithArgs("usr_01", defaultPageLimit, 0).
		WillReturnRows(sessionRows().
			AddRow("ses_02", "usr_01", "Firefox", nil, fixedNow, fixedNow.Add(time.Hour), nil, fixedNow).
			AddRow("ses_01", "usr_01", "curl/8", "203.0.113.7", fixedNow.Add(-48*time.Hour), fixedNow.Add(-24*time.Hour), revoked, nil))

	// Act
	sessions, err := repo.ListForUser(context.Background(), "usr_01", 0, 0)

	// Assert
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions; want 2", len(sessions))
	}
	// A person reviewing their devices has to be able to see a session that ended, not
	// just the ones still open.
	if sessions[1].RevokedAt == nil || sessions[1].Live(fixedNow) {
		t.Errorf("the revoked session came back live: %+v", sessions[1])
	}
	if sessions[0].LastSeenAt == nil {
		t.Error("last seen was dropped, so a person cannot tell which device is theirs")
	}
}

func TestSessionRepository_deleteExpired_keepsRevokedRows(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewSessionRepository(db)
	mock.ExpectExec(`DELETE FROM sessions WHERE expires_at < \$1 AND revoked_at IS NULL`).
		WithArgs(fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 7))

	// Act
	deleted, err := repo.DeleteExpired(context.Background(), fixedNow)

	// Assert
	// The `revoked_at IS NULL` in the query is the point: a revocation is a fact
	// somebody may need to explain later, and an expiry is bookkeeping.
	if err != nil {
		t.Fatalf("delete expired sessions: %v", err)
	}
	if deleted != 7 {
		t.Errorf("deleted %d sessions; want 7", deleted)
	}
}

func TestInetOrNull_refusesAnythingThatIsNotAnAddress(t *testing.T) {
	// Act & Assert
	if got := inetOrNull("203.0.113.7"); got != "203.0.113.7" {
		t.Errorf("inetOrNull(ipv4) = %v; want the address", got)
	}
	if got := inetOrNull("2001:db8::1"); got != "2001:db8::1" {
		t.Errorf("inetOrNull(ipv6) = %v; want the address", got)
	}
	for _, bad := range []string{"", "localhost", "203.0.113.7, 10.0.0.1", "203.0.113.7:443"} {
		if got := inetOrNull(bad); got != nil {
			t.Errorf("inetOrNull(%q) = %v; want nil", bad, got)
		}
	}
}
