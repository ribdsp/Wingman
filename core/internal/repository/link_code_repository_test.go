package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// testCodeHash is a stored link code: the SHA-256 hex digest, never the code.
const testCodeHash = "9f2c1b0a7e6d5c4b3a2918f7e6d5c4b3a2918f7e6d5c4b3a2918f7e6d5c4b3a2"

func linkCodeRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "user_id", "created_at", "expires_at", "consumed_at"})
}

func TestLinkCodeRepository_mint_supersedesOutstandingCodesInTheSameStatement(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewLinkCodeRepository(db)
	expiry := fixedNow.Add(15 * time.Minute)
	mock.ExpectQuery(`INSERT INTO channel_link_codes`).
		WithArgs("usr_01", testCodeHash, expiry).
		WillReturnRows(linkCodeRows().AddRow("lnk_01", "usr_01", fixedNow, expiry, nil))

	// Act
	code, err := repo.Mint(context.Background(), NewLinkCode{
		UserID: "usr_01", CodeHash: testCodeHash, ExpiresAt: expiry,
	})

	// Assert
	if err != nil {
		t.Fatalf("mint link code: %v", err)
	}
	if code.ConsumedAt != nil {
		t.Error("a freshly minted code came back consumed")
	}
	if !code.Live(fixedNow) {
		t.Error("a freshly minted code was not live at creation")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestLinkCodeRepository_mint_withoutAHash_isRefusedWithoutAQuery(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewLinkCodeRepository(db)

	// Act
	_, err := repo.Mint(context.Background(), NewLinkCode{
		UserID: "usr_01", ExpiresAt: fixedNow.Add(time.Minute),
	})

	// Assert
	// The plaintext code must never reach a query, so a caller that forgot to hash
	// it has to fail here rather than store an empty credential.
	if err == nil {
		t.Fatal("minting without a hash succeeded")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestLinkCodeRepository_mint_withoutAnExpiry_isRefused(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewLinkCodeRepository(db)

	// Act
	_, err := repo.Mint(context.Background(), NewLinkCode{
		UserID: "usr_01", CodeHash: testCodeHash,
	})

	// Assert
	// A code with no expiry is a permanent account credential sitting in somebody's
	// chat history.
	if err == nil {
		t.Fatal("minting without an expiry succeeded")
	}
}

func TestLinkCodeRepository_mint_databaseFailure_isWrapped(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewLinkCodeRepository(db)
	expiry := fixedNow.Add(time.Minute)
	mock.ExpectQuery(`INSERT INTO channel_link_codes`).
		WillReturnError(errors.New("connection refused"))

	// Act
	_, err := repo.Mint(context.Background(), NewLinkCode{
		UserID: "usr_01", CodeHash: testCodeHash, ExpiresAt: expiry,
	})

	// Assert
	if err == nil {
		t.Fatal("a failed insert reported success")
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
		t.Fatalf("an outage was classified as a caller's problem: %v", err)
	}
}

func TestLinkCodeRepository_consume_returnsTheAccountItBelongsTo(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewLinkCodeRepository(db)
	mock.ExpectQuery(`UPDATE channel_link_codes`).
		WithArgs(testCodeHash, fixedNow).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("usr_01"))

	// Act
	userID, err := repo.Consume(context.Background(), testCodeHash, fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("consume link code: %v", err)
	}
	if userID != "usr_01" {
		t.Fatalf("userId = %q, want usr_01", userID)
	}
}

func TestLinkCodeRepository_consume_unknownExpiredOrAlreadyUsed_isNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewLinkCodeRepository(db)
	mock.ExpectQuery(`UPDATE channel_link_codes`).WillReturnError(sql.ErrNoRows)

	// Act
	_, err := repo.Consume(context.Background(), testCodeHash, fixedNow)

	// Assert
	// All three answer the same. A sender who can tell "expired" from "unknown" can
	// tell that a code was once real, which is half of guessing one.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestLinkCodeRepository_consume_emptyHash_isNotFoundWithoutAQuery(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewLinkCodeRepository(db)

	// Act
	_, err := repo.Consume(context.Background(), "", fixedNow)

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestLinkCodeRepository_consume_databaseFailure_isNotFoldedIntoNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewLinkCodeRepository(db)
	mock.ExpectQuery(`UPDATE channel_link_codes`).WillReturnError(errors.New("connection refused"))

	// Act
	_, err := repo.Consume(context.Background(), testCodeHash, fixedNow)

	// Assert
	// A database that is down must not read as "that code is wrong": the first sends
	// the person to try again in a minute, the second tells them to mint another one.
	if errors.Is(err, ErrNotFound) {
		t.Fatal("an outage was reported as a wrong code")
	}
	if err == nil {
		t.Fatal("a failed update reported success")
	}
}

func TestLinkCodeRepository_consume_withoutATime_isRefused(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewLinkCodeRepository(db)

	// Act
	_, err := repo.Consume(context.Background(), testCodeHash, time.Time{})

	// Assert
	// The zero time would compare as expired against every row, turning a real code
	// into a wrong one.
	if err == nil {
		t.Fatal("consuming without a time succeeded")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("a missing time was reported as a wrong code")
	}
}

func TestLinkCodeRepository_deleteSpent_countsWhatItRemoved(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewLinkCodeRepository(db)
	mock.ExpectExec(`DELETE FROM channel_link_codes`).
		WithArgs(fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 4))

	// Act
	removed, err := repo.DeleteSpent(context.Background(), fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("delete spent codes: %v", err)
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want 4", removed)
	}
}

func TestLinkCode_live_isFalseOnceConsumed(t *testing.T) {
	// Arrange
	consumed := fixedNow.Add(-time.Minute)
	code := LinkCode{
		CreatedAt:  fixedNow.Add(-2 * time.Minute),
		ExpiresAt:  fixedNow.Add(10 * time.Minute),
		ConsumedAt: &consumed,
	}

	// Act & Assert
	if code.Live(fixedNow) {
		t.Error("a consumed code reported itself live")
	}
}

func TestLinkCode_live_isFalseOnceExpired(t *testing.T) {
	// Arrange
	code := LinkCode{
		CreatedAt: fixedNow.Add(-time.Hour),
		ExpiresAt: fixedNow.Add(-time.Minute),
	}

	// Act & Assert
	if code.Live(fixedNow) {
		t.Error("an expired code reported itself live")
	}
}
