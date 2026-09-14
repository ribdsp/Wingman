package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

const testArgon2Hash = "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHQ$aGFzaA"

func userRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "email", "display_name", "is_active", "created_at"})
}

func TestUserRepository_create_lowerCasesTheEmail(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`INSERT INTO users`).
		WithArgs("owner@example.com", "Owner", testArgon2Hash).
		WillReturnRows(userRows().AddRow("usr_01", "owner@example.com", "Owner", true, fixedNow))

	// Act
	user, err := repo.Create(context.Background(), "  Owner@Example.COM ", " Owner ", testArgon2Hash)

	// Assert
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Two accounts differing only in capitalisation would be two token budgets for one
	// person, and they could not tell which one they had signed in to.
	if user.Email != "owner@example.com" {
		t.Errorf("email = %q; want it lower-cased", user.Email)
	}
	if !user.IsActive {
		t.Error("a new account came back inactive")
	}
}

func TestUserRepository_create_refusesAPasswordThatIsNotAnArgon2idHash(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewUserRepository(db)

	// Act
	_, err := repo.Create(context.Background(), "owner@example.com", "Owner", "hunter2")

	// Assert
	// No query is expected: a plaintext password must not reach the database at all,
	// and the mock's expectation check fails the test if one were made.
	if err == nil {
		t.Fatal("a plaintext password was accepted as a hash")
	}
}

func TestUserRepository_create_refusesAnEmptyEmailWithoutQuerying(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewUserRepository(db)

	// Act
	_, err := repo.Create(context.Background(), "   ", "Owner", testArgon2Hash)

	// Assert
	if err == nil {
		t.Fatal("an account with no email address was accepted")
	}
}

func TestUserRepository_create_conflictDoesNotEchoTheEmail(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`INSERT INTO users`).
		WithArgs("taken@example.com", "", testArgon2Hash).
		WillReturnError(pgError(pgUniqueViolation))

	// Act
	_, err := repo.Create(context.Background(), "taken@example.com", "", testArgon2Hash)

	// Assert
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("create user = %v; want ErrConflict", err)
	}
	// This error travels to a sign-up response. Repeating the address back confirms
	// which addresses have accounts on this instance.
	if strings.Contains(err.Error(), "taken@example.com") {
		t.Errorf("the conflict echoed the email address: %v", err)
	}
}

func TestUserRepository_getByID_missingRowIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`SELECT .* FROM users WHERE id = \$1`).
		WithArgs("usr_missing").
		WillReturnRows(userRows())

	// Act
	_, err := repo.GetByID(context.Background(), "usr_missing")

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get user = %v; want ErrNotFound", err)
	}
}

func TestUserRepository_getCredentials_returnsDeactivatedAccountsToo(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`SELECT id, password_hash, is_active FROM users WHERE email = \$1`).
		WithArgs("owner@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "password_hash", "is_active"}).
			AddRow("usr_01", testArgon2Hash, false))

	// Act
	creds, err := repo.GetCredentialsByEmail(context.Background(), " Owner@Example.com ")

	// Assert
	if err != nil {
		t.Fatalf("get credentials: %v", err)
	}
	// A deactivated account still returns its hash so the caller can verify the
	// password before answering. Skipping the verification is what turns a login form
	// into a way to enumerate accounts by response time.
	if creds.IsActive {
		t.Error("a deactivated account came back active")
	}
	if creds.PasswordHash != testArgon2Hash {
		t.Error("the stored hash was not returned, so a caller could not verify the password")
	}
}

func TestUserRepository_getCredentials_unknownEmailIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`FROM users WHERE email = \$1`).
		WithArgs("nobody@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "password_hash", "is_active"}))

	// Act
	_, err := repo.GetCredentialsByEmail(context.Background(), "nobody@example.com")

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get credentials = %v; want ErrNotFound", err)
	}
}

func TestUserRepository_updatePassword_refusesANonArgon2idHash(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewUserRepository(db)

	// Act
	err := repo.UpdatePassword(context.Background(), "usr_01", "$2a$10$notargon")

	// Assert
	// bcrypt is not wrong so much as not what this schema stores; accepting it would
	// leave an account whose password can never be verified again.
	if err == nil {
		t.Fatal("a hash from another algorithm was accepted")
	}
}

func TestUserRepository_updatePassword_missingAccountIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectExec(`UPDATE users SET password_hash = \$2 WHERE id = \$1`).
		WithArgs("usr_missing", testArgon2Hash).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.UpdatePassword(context.Background(), "usr_missing", testArgon2Hash)

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("update password = %v; want ErrNotFound", err)
	}
}

func TestUserRepository_setActive_deactivatesWithoutDeleting(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectExec(`UPDATE users SET is_active = \$2 WHERE id = \$1`).
		WithArgs("usr_01", false).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Act
	err := repo.SetActive(context.Background(), "usr_01", false)

	// Assert
	// There is no delete on this repository at all: a run, its transcript and its spend
	// are the record of money that was actually spent, and they reference this row.
	if err != nil {
		t.Fatalf("set active: %v", err)
	}
}

func TestUserRepository_count_answersWhetherThisIsAFreshInstance(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`SELECT count\(\*\) FROM users`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// Act
	total, err := repo.Count(context.Background())

	// Assert
	// `core createuser` reads this to refuse a second account instead of quietly making
	// one, which on a box reachable from the internet is the difference between a setup
	// step and a back door.
	if err != nil {
		t.Fatalf("count users: %v", err)
	}
	if total != 0 {
		t.Errorf("count = %d; want 0", total)
	}
}

func TestUserRepository_list_boundsThePageAndReportsTheTotal(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`SELECT count\(\*\) FROM users`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`FROM users\s+ORDER BY created_at ASC`).
		WithArgs(maxPageLimit, 0).
		WillReturnRows(userRows().
			AddRow("usr_01", "a@example.com", "A", true, fixedNow).
			AddRow("usr_02", "b@example.com", "B", false, fixedNow))

	// Act
	users, total, err := repo.List(context.Background(), 5_000, -1)

	// Assert
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if total != 2 || len(users) != 2 {
		t.Fatalf("got %d of %d users; want 2 of 2", len(users), total)
	}
	if users[0].ID != "usr_01" || users[1].ID != "usr_02" {
		t.Errorf("users came back in the wrong order: %+v", users)
	}
}

func TestUserRepository_list_countFailureStopsTheRead(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`SELECT count\(\*\) FROM users`).
		WillReturnError(errors.New("connection reset"))

	// Act
	users, total, err := repo.List(context.Background(), 10, 0)

	// Assert
	// A failed count must not be reported as zero accounts alongside a successful list:
	// the pair would say "no users" while listing some.
	if err == nil {
		t.Fatal("a failed count was reported as a successful list")
	}
	if users != nil || total != 0 {
		t.Errorf("got %v / %d alongside the error; want nothing", users, total)
	}
}

func TestUserRepository_getByID_readsTheAccountBackWithoutItsPasswordHash(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`FROM users WHERE id = \$1`).
		WithArgs("usr_01").
		WillReturnRows(userRows().AddRow("usr_01", "owner@example.com", "Owner", true, fixedNow))

	// Act
	user, err := repo.GetByID(context.Background(), "usr_01")

	// Assert
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.Email != "owner@example.com" || !user.IsActive {
		t.Errorf("user = %+v; want the active account", user)
	}
	// The hash is not in userColumns and not on User, so it cannot be rendered into a
	// response by accident. Reading it needs GetCredentialsByEmail, which exists for
	// sign-in and nothing else.
	if strings.Contains(fmt.Sprintf("%+v", user), "argon2") {
		t.Errorf("the password hash reached a general-purpose read: %+v", user)
	}
}

func TestUserRepository_getByID_anAccountThatDoesNotExistIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewUserRepository(db)
	mock.ExpectQuery(`FROM users WHERE id = \$1`).
		WithArgs("usr_missing").
		WillReturnRows(userRows())

	// Act
	_, err := repo.GetByID(context.Background(), "usr_missing")

	// Assert
	// A zero User returned instead would be an account with an empty id that IsActive
	// reads as false — which is the same shape as a deactivated account, so a caller
	// checking IsActive would look correct while working on nobody.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get a missing account = %v; want ErrNotFound", err)
	}
}
