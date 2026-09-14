package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/repository"
)

const (
	testPassword = "correct horse battery staple"
	nextPassword = "a different long passphrase"
)

// hashedTestPassword is computed once for the whole package. argon2id at 64 MiB is
// deliberately slow, so seeding twenty accounts with twenty real hashes would spend
// seconds proving something auth's own tests already prove.
var hashedTestPassword = sync.OnceValue(func() string {
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		panic("hash the test password: " + err.Error())
	}
	return hash
})

const testSessionTTL = 24 * time.Hour

// accountsFixture is the wiring every test below starts from, with the fakes kept to hand
// so a test can inject a failure or read what was stored.
type accountsFixture struct {
	accounts *Accounts
	users    *fakeUsers
	sessions *fakeSessions
}

func newAccountsFixture(t *testing.T, openRegistration bool) accountsFixture {
	t.Helper()

	users := newFakeUsers()
	sessions := newFakeSessions()
	accounts, err := NewAccounts(AccountsDeps{
		Users:            users,
		Passwords:        users,
		Admin:            users,
		Sessions:         sessions,
		Revoker:          sessions,
		OpenRegistration: openRegistration,
		SessionTTL:       testSessionTTL,
		Clock:            fixedClock(testNow),
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewAccounts: %v", err)
	}
	return accountsFixture{accounts: accounts, users: users, sessions: sessions}
}

// seedAccount puts an account in the store with the shared hash, bypassing Create so a
// test about signing in does not also pay for hashing.
func seedAccount(t *testing.T, users *fakeUsers, email string) repository.User {
	t.Helper()

	user, err := users.Create(context.Background(), email, "Seeded Person", hashedTestPassword())
	if err != nil {
		t.Fatalf("seed %s: %v", email, err)
	}
	return user
}

// weakHash is a real argon2id hash of password under parameters weaker than the current
// ones, in the PHC encoding auth writes.
//
// Built here rather than asked of auth because auth has no way to write one on purpose,
// and correctly so — nothing in production should. It is the only way to reach the
// rehash-on-sign-in path, which by definition needs a credential that predates a
// parameter change.
func weakHash(t *testing.T, password string) string {
	t.Helper()

	const (
		weakMemory  = 8 * 1024
		weakTime    = 1
		weakThreads = 1
		digestLen   = 32
	)
	salt := []byte("a-sixteen-byte-s")
	digest := argon2.IDKey([]byte(password), salt, weakTime, weakMemory, weakThreads, digestLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, weakMemory, weakTime, weakThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	)
}

func operatorActor() Actor { return Actor{Type: ActorOperator, ID: "operator-key"} }
func botActor() Actor      { return Actor{Type: ActorBot, ID: "goal-engine"} }
func userActor(id string) Actor {
	return Actor{Type: ActorUser, ID: id, UserID: id}
}

func TestNewAccounts_namesEveryMissingDependencyAtOnce(t *testing.T) {
	// Arrange — nothing wired, which is what a half-finished cmd looks like.

	// Act
	_, err := NewAccounts(AccountsDeps{})

	// Assert — one error listing all of them, so an operator fixes the wiring in one pass
	// rather than one restart per field.
	if err == nil {
		t.Fatal("err = nil, want a complaint about the wiring")
	}
	for _, name := range []string{"Users", "Passwords", "Admin", "Sessions", "Revoker", "SessionTTL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
}

func TestAccountsCreate_isOperatorOnly(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	in := NewAccount{Email: "person@example.com", DisplayName: "Person", Password: testPassword}

	for _, actor := range []Actor{userActor("user-1"), botActor()} {
		t.Run(string(actor.Type), func(t *testing.T) {
			// Act
			_, err := f.accounts.Create(context.Background(), in, actor)

			// Assert
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestAccountsCreate_worksWithRegistrationClosed(t *testing.T) {
	// Arrange — closed registration is about strangers signing themselves up, not about an
	// operator making an account for somebody.
	f := newAccountsFixture(t, false)

	// Act
	user, err := f.accounts.Create(context.Background(), NewAccount{
		Email:       "  Person@Example.COM ",
		DisplayName: "  Person  ",
		Password:    testPassword,
	}, operatorActor())

	// Assert
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if user.Email != "person@example.com" {
		t.Errorf("email = %q, want it lowercased and trimmed", user.Email)
	}
	if user.DisplayName != "Person" {
		t.Errorf("displayName = %q, want it trimmed", user.DisplayName)
	}
	if !user.IsActive {
		t.Error("a new account is not active, want active")
	}
}

func TestAccountsCreate_reportsEveryFieldProblemAtOnce(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)

	// Act
	_, err := f.accounts.Create(context.Background(), NewAccount{
		Email:       "not-an-address",
		DisplayName: "",
		Password:    "short",
	}, operatorActor())

	// Assert — three problems, three named fields, one round trip.
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	for _, field := range []string{"email", "displayName", "password"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("err = %v, want it to name %s", err, field)
		}
	}
}

func TestAccountsCreate_neverEchoesThePassword(t *testing.T) {
	// Arrange — a password that fails validation, because that is the path that builds a
	// message out of what was sent.
	f := newAccountsFixture(t, false)

	// Act
	_, err := f.accounts.Create(context.Background(), NewAccount{
		Email:       "person@example.com",
		DisplayName: "Person",
		Password:    "hunter2",
	}, operatorActor())

	// Assert
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("err = %v, want it not to quote the password", err)
	}
}

func TestAccountsCreate_secondAccountOnOneAddressIsAConflict(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	seedAccount(t, f.users, "person@example.com")

	// Act
	_, err := f.accounts.Create(context.Background(), NewAccount{
		Email:       "PERSON@example.com",
		DisplayName: "Impostor",
		Password:    testPassword,
	}, operatorActor())

	// Assert
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestAccountsRegister_isRefusedWhenRegistrationIsClosed(t *testing.T) {
	// Arrange — the default, and the default is the point: an open box on the internet is
	// a box running strangers' code in your sandbox on your API key.
	f := newAccountsFixture(t, false)

	// Act
	_, err := f.accounts.Register(context.Background(), NewAccount{
		Email:       "stranger@example.com",
		DisplayName: "Stranger",
		Password:    testPassword,
	})

	// Assert — ErrForbidden rather than a sentinel of its own, so the answer does not tell
	// a stranger whether anybody may sign up.
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if count, _ := f.users.Count(context.Background()); count != 0 {
		t.Errorf("accounts = %d, want none created", count)
	}
}

func TestAccountsRegister_worksWhenAnOperatorOpenedIt(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, true)

	// Act
	user, err := f.accounts.Register(context.Background(), NewAccount{
		Email:       "person@example.com",
		DisplayName: "Person",
		Password:    testPassword,
	})

	// Assert
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if user.ID == "" {
		t.Error("user id is empty, want an account")
	}
}

func TestAccountsSignIn_returnsATokenWhoseStoredFormIsDifferent(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	user := seedAccount(t, f.users, "person@example.com")

	// Act
	signedIn, err := f.accounts.SignIn(context.Background(), SignIn{
		Email:     "person@example.com",
		Password:  testPassword,
		UserAgent: "wingman-test",
		IP:        "203.0.113.7",
	})

	// Assert
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if signedIn.User.ID != user.ID {
		t.Errorf("user = %q, want %q", signedIn.User.ID, user.ID)
	}
	if signedIn.Token == "" {
		t.Fatal("token is empty, want the plaintext handed back exactly once")
	}
	if want := testNow.Add(testSessionTTL); !signedIn.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %s, want %s", signedIn.ExpiresAt, want)
	}

	// A database dump must not be a set of live sessions, so what was stored is a hash of
	// the token and not the token.
	stored := f.sessions.hashes["session-1"]
	if stored == "" {
		t.Fatal("no session was stored")
	}
	if stored == signedIn.Token {
		t.Error("the token was stored as-is, want only its hash")
	}
	if hashed, err := auth.ParseSessionToken(signedIn.Token); err != nil || hashed != stored {
		t.Errorf("stored form does not match the token: %v", err)
	}
}

func TestAccountsSignIn_answersTheSameWayForEveryKindOfFailure(t *testing.T) {
	// The three facts here — no such address, wrong password, account disabled — are
	// exactly what a stranger would like to learn from a sign-in form, so all three get one
	// sentence.
	cases := map[string]struct {
		email    string
		password string
		disable  bool
	}{
		"unknown address": {email: "nobody@example.com", password: testPassword},
		"wrong password":  {email: "person@example.com", password: "not the password"},
		"disabled":        {email: "person@example.com", password: testPassword, disable: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			f := newAccountsFixture(t, false)
			user := seedAccount(t, f.users, "person@example.com")
			if tc.disable {
				if err := f.users.SetActive(context.Background(), user.ID, false); err != nil {
					t.Fatalf("SetActive: %v", err)
				}
			}

			// Act
			_, err := f.accounts.SignIn(context.Background(), SignIn{Email: tc.email, Password: tc.password})

			// Assert
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
			if err.Error() != ErrUnauthenticated.Error() {
				t.Errorf("err = %q, want the bare sentence with nothing added", err)
			}
			if len(f.sessions.rows) != 0 {
				t.Error("a session was created for a refused sign-in")
			}
		})
	}
}

func TestAccountsSignIn_missingFieldsAreARequestProblemNotACredentialOne(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)

	// Act
	_, err := f.accounts.SignIn(context.Background(), SignIn{Email: "  ", Password: ""})

	// Assert — nothing was checked, so there is nothing to be unauthenticated about.
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestAccountsSignIn_revokesTheSessionItCannotHandBack(t *testing.T) {
	// Arrange — the account read fails after the session row exists.
	f := newAccountsFixture(t, false)
	seedAccount(t, f.users, "person@example.com")
	f.users.failGetByID = errBoom

	// Act
	_, err := f.accounts.SignIn(context.Background(), SignIn{Email: "person@example.com", Password: testPassword})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the read failure")
	}
	if len(f.sessions.rows) != 1 {
		t.Fatalf("sessions = %d, want the one that was created", len(f.sessions.rows))
	}
	// A live session the caller never received is a credential nobody can revoke.
	if f.sessions.rows[0].RevokedAt == nil {
		t.Error("the session is still live, want it revoked")
	}
}

func TestAccountsSignIn_aSessionThatCannotEvenBeRevokedStillReportsTheOriginalFailure(t *testing.T) {
	// Arrange — both the account read and the tidying-up fail.
	f := newAccountsFixture(t, false)
	seedAccount(t, f.users, "person@example.com")
	f.users.failGetByID = errBoom
	f.sessions.failByToken = errors.New("and the revocation failed too")

	// Act
	_, err := f.accounts.SignIn(context.Background(), SignIn{Email: "person@example.com", Password: testPassword})

	// Assert — the caller is told what actually went wrong. The second failure has nowhere
	// to go but the log, and reporting it instead would send an operator to the sessions
	// table for a problem in the users table.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the account read's failure", err)
	}
}

func TestAccountsSignIn_upgradesAHashWrittenUnderWeakerParameters(t *testing.T) {
	// Arrange — a credential from before the parameters were raised. Signing in is the only
	// moment core holds the plaintext for an existing account, so it is the only moment the
	// stored hash can be rewritten.
	f := newAccountsFixture(t, false)
	user, err := f.users.Create(context.Background(), "person@example.com", "Seeded Person", weakHash(t, testPassword))
	if err != nil {
		t.Fatalf("seed an account with a weak hash: %v", err)
	}

	// Act
	signedIn, err := f.accounts.SignIn(context.Background(), SignIn{Email: "person@example.com", Password: testPassword})

	// Assert — the old parameters still verify, so the sign-in works either way. What this
	// proves is that it does not keep working under them forever.
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if f.users.updatedHashes != 1 {
		t.Errorf("password writes = %d, want the hash upgraded once", f.users.updatedHashes)
	}
	if auth.NeedsRehash(f.users.accounts[user.ID].hash) {
		t.Error("the stored hash still uses the weaker parameters")
	}
	// Not a revocation: nothing about the credential changed, only the cost of verifying
	// it, so the sessions the person already holds are still theirs.
	if f.sessions.revokeAllCalls != 0 {
		t.Errorf("revokeAll calls = %d, want none", f.sessions.revokeAllCalls)
	}
	if signedIn.Token == "" {
		t.Error("no token was handed back")
	}
}

func TestAccountsSignIn_anUpgradeThatFailsDoesNotRefuseTheSignIn(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	if _, err := f.users.Create(context.Background(), "person@example.com", "Seeded Person", weakHash(t, testPassword)); err != nil {
		t.Fatalf("seed an account with a weak hash: %v", err)
	}
	f.users.failUpdate = errBoom

	// Act
	signedIn, err := f.accounts.SignIn(context.Background(), SignIn{Email: "person@example.com", Password: testPassword})

	// Assert — the password was correct. Refusing over bookkeeping would lock somebody out
	// of their own instance because a hash could not be rewritten.
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if signedIn.Token == "" {
		t.Error("no token was handed back")
	}
}

func TestAccountsSignOut_endsThePresentedSession(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	seedAccount(t, f.users, "person@example.com")
	signedIn, err := f.accounts.SignIn(context.Background(), SignIn{Email: "person@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	// Act
	err = f.accounts.SignOut(context.Background(), signedIn.Token)

	// Assert
	if err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	if f.sessions.rows[0].RevokedAt == nil {
		t.Error("the session is still live, want it revoked")
	}
	if got := *f.sessions.rows[0].RevokedAt; !got.Equal(testNow) {
		t.Errorf("revokedAt = %s, want the clock's %s", got, testNow)
	}
}

func TestAccountsSignOut_succeedsForSomethingThatIsNotASession(t *testing.T) {
	// A sign-out that failed would leave a client unable to clear its own state, and there
	// is nothing to protect: whatever was presented is not a session either way.
	cases := map[string]string{
		"not a token":  "hello",
		"empty":        "",
		"never issued": "wm_sess_" + strings.Repeat("a", 43),
	}

	for name, presented := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			f := newAccountsFixture(t, false)

			// Act
			err := f.accounts.SignOut(context.Background(), presented)

			// Assert
			if err != nil {
				t.Fatalf("SignOut: %v", err)
			}
		})
	}
}

func TestAccountsChangePassword_replacesTheHashAndEndsEverySession(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	user := seedAccount(t, f.users, "person@example.com")
	for range 3 {
		if _, err := f.sessions.Create(context.Background(), repository.NewSession{
			UserID: user.ID, TokenHash: "hash-" + user.ID, ExpiresAt: testNow.Add(time.Hour),
		}); err != nil {
			t.Fatalf("seed a session: %v", err)
		}
	}

	// Act
	err := f.accounts.ChangePassword(context.Background(), testPassword, nextPassword, userActor(user.ID))

	// Assert
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	creds, err := f.users.GetCredentialsByEmail(context.Background(), user.Email)
	if err != nil {
		t.Fatalf("read the credentials back: %v", err)
	}
	if ok, err := auth.VerifyPassword(creds.PasswordHash, nextPassword); err != nil || !ok {
		t.Errorf("the new password does not verify against the stored hash: %v", err)
	}
	// Every session, including the one making the request: a password change is what
	// somebody does when they think another person has their session.
	if live := f.sessions.live(user.ID); live != 0 {
		t.Errorf("live sessions = %d, want none", live)
	}
}

func TestAccountsChangePassword_refusesAWrongCurrentPassword(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	user := seedAccount(t, f.users, "person@example.com")

	// Act
	err := f.accounts.ChangePassword(context.Background(), "not the password", nextPassword, userActor(user.ID))

	// Assert — a credential failing to check out, not a malformed request.
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	creds, _ := f.users.GetCredentialsByEmail(context.Background(), user.Email)
	if creds.PasswordHash != hashedTestPassword() {
		t.Error("the stored hash changed, want it untouched")
	}
}

func TestAccountsChangePassword_refusesANewPasswordItWouldNotStore(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	user := seedAccount(t, f.users, "person@example.com")

	// Act
	err := f.accounts.ChangePassword(context.Background(), testPassword, "short", userActor(user.ID))

	// Assert — checked before the current password, because a request that could never
	// succeed should not cost an argon2id verification.
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "newPassword") {
		t.Errorf("err = %v, want it to name newPassword", err)
	}
}

func TestAccountsChangePassword_reportsSuccessWhenOnlyTheRevocationFailed(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	user := seedAccount(t, f.users, "person@example.com")
	f.sessions.failAll = errBoom

	// Act
	err := f.accounts.ChangePassword(context.Background(), testPassword, nextPassword, userActor(user.ID))

	// Assert — the password is already changed. Reporting a failure would say it was not,
	// which is worse than saying it was and that the old sessions may outlive it.
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if f.users.updatedHashes != 1 {
		t.Errorf("password writes = %d, want 1", f.users.updatedHashes)
	}
}

func TestAccountsChangePassword_isNotSomethingAMachineKeyCanDo(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)

	// Act
	err := f.accounts.ChangePassword(context.Background(), testPassword, nextPassword, botActor())

	// Assert
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestAccountsMe_readsTheCallersOwnAccount(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	user := seedAccount(t, f.users, "person@example.com")

	// Act
	got, err := f.accounts.Me(context.Background(), userActor(user.ID))

	// Assert
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("id = %q, want %q", got.ID, user.ID)
	}
}

func TestAccountsMe_accountThatIsGoneIsNotFound(t *testing.T) {
	// Arrange — a live session whose account was deleted underneath it.
	f := newAccountsFixture(t, false)

	// Act
	_, err := f.accounts.Me(context.Background(), userActor("user-404"))

	// Assert — the repository's sentinel must not reach the HTTP layer.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if errors.Is(err, repository.ErrNotFound) {
		t.Error("the repository's sentinel leaked out of the service")
	}
}

func TestAccountsListAndCount_areOperatorOnly(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	seedAccount(t, f.users, "person@example.com")

	for _, actor := range []Actor{userActor("user-1"), botActor()} {
		t.Run(string(actor.Type), func(t *testing.T) {
			// Act
			_, _, listErr := f.accounts.List(context.Background(), 10, 0, actor)
			_, countErr := f.accounts.Count(context.Background(), actor)

			// Assert
			if !errors.Is(listErr, ErrForbidden) {
				t.Errorf("List err = %v, want ErrForbidden", listErr)
			}
			if !errors.Is(countErr, ErrForbidden) {
				t.Errorf("Count err = %v, want ErrForbidden", countErr)
			}
		})
	}
}

func TestAccountsSetActive_disablingEndsEverySessionAndEnablingEndsNone(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	user := seedAccount(t, f.users, "person@example.com")
	if _, err := f.sessions.Create(context.Background(), repository.NewSession{
		UserID: user.ID, TokenHash: "hash", ExpiresAt: testNow.Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed a session: %v", err)
	}

	// Act — disable.
	if err := f.accounts.SetActive(context.Background(), user.ID, false, operatorActor()); err != nil {
		t.Fatalf("SetActive(false): %v", err)
	}

	// Assert — an account that cannot be signed in to but whose session still works is not
	// disabled.
	if live := f.sessions.live(user.ID); live != 0 {
		t.Errorf("live sessions = %d, want none", live)
	}

	// Act — enable again.
	before := f.sessions.revokeAllCalls
	if err := f.accounts.SetActive(context.Background(), user.ID, true, operatorActor()); err != nil {
		t.Fatalf("SetActive(true): %v", err)
	}

	// Assert — there is nothing to revoke, and reactivating somebody should not punish them.
	if f.sessions.revokeAllCalls != before {
		t.Errorf("revokeAll calls = %d, want it unchanged at %d", f.sessions.revokeAllCalls, before)
	}
}

func TestAccountsSetActive_requiresAnOperatorAndAnId(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)

	// Act
	forbidden := f.accounts.SetActive(context.Background(), "user-1", false, userActor("user-1"))
	invalid := f.accounts.SetActive(context.Background(), "   ", false, operatorActor())
	missing := f.accounts.SetActive(context.Background(), "user-404", false, operatorActor())

	// Assert
	if !errors.Is(forbidden, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", forbidden)
	}
	if !errors.Is(invalid, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", invalid)
	}
	if !errors.Is(missing, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", missing)
	}
}

func TestAccountsResolveOwner_findsTheAccountUnattendedWorkIsFiledAgainst(t *testing.T) {
	// Arrange
	f := newAccountsFixture(t, false)
	user := seedAccount(t, f.users, "unattended@example.com")

	// Act
	got, err := f.accounts.ResolveOwner(context.Background(), "  Unattended@Example.com  ")

	// Assert
	if err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("id = %q, want %q", got.ID, user.ID)
	}
}

func TestAccountsResolveOwner_refusesAnAddressThatWouldLeaveWorkNowhereToGo(t *testing.T) {
	// Every case here is an operator misconfiguring the box, and every one is caught at
	// boot rather than at three in the morning when the goal engine dispatches.
	cases := map[string]struct {
		email   string
		seed    bool
		disable bool
		want    error
	}{
		"empty":    {email: "   ", want: ErrValidation},
		"no such":  {email: "nobody@example.com", want: ErrNotFound},
		"disabled": {email: "unattended@example.com", seed: true, disable: true, want: ErrValidation},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			f := newAccountsFixture(t, false)
			if tc.seed {
				user := seedAccount(t, f.users, tc.email)
				if tc.disable {
					if err := f.users.SetActive(context.Background(), user.ID, false); err != nil {
						t.Fatalf("SetActive: %v", err)
					}
				}
			}

			// Act
			_, err := f.accounts.ResolveOwner(context.Background(), tc.email)

			// Assert
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the address shape rules

func TestCheckEmail_refusesWhatCouldNotBeAnAddressAndSaysWhich(t *testing.T) {
	// Tested directly rather than through Create, because Create reports every field
	// problem in one message and that message cannot say which rule fired. What is
	// asserted is the rule, not existence — nothing here proves an address receives mail,
	// and core sends none.
	//
	// The length rule has the one boundary worth pinning: at the limit is an address, one
	// character over it is a payload.
	const host = "@example.com"
	atTheLimit := strings.Repeat("a", maxEmailLength-len(host)) + host
	overTheLimit := strings.Repeat("a", maxEmailLength-len(host)+1) + host

	cases := map[string]struct {
		email string
		want  string // a substring of the complaint, or "" for an address that is accepted
	}{
		"an address":       {"person@example.com", ""},
		"a subdomain":      {"person@mail.example.co.uk", ""},
		"at the limit":     {atTheLimit, ""},
		"empty":            {"", "is required"},
		"over the limit":   {overTheLimit, "must be at most"},
		"no at sign":       {"person.example.com", "must contain @"},
		"a space":          {"person name@example.com", "must not contain spaces"},
		"a newline":        {"person@example.com\n", "must not contain spaces"},
		"no local part":    {"@example.com", "either side"},
		"no host":          {"person@", "either side"},
		"two at signs":     {"person@example@com", "exactly one @"},
		"a host with none": {"person@localhost", "a dot in it"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got := checkEmail(tc.email)

			// Assert
			if tc.want == "" {
				if got != "" {
					t.Fatalf("complaint = %q, want the address accepted", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("complaint = %q, want one containing %q", got, tc.want)
			}
		})
	}
}
