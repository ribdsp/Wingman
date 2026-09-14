package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// Bounds on the two free-text fields an account carries. Neither is a security
// property; both stop a display name or an address from being a paragraph.
const (
	maxEmailLength       = 254
	maxDisplayNameLength = 100
)

// AccountsDeps is everything the account service needs.
type AccountsDeps struct {
	Users     UserStore
	Passwords PasswordStore
	Admin     UserAdmin
	Sessions  SessionStore
	Revoker   SessionRevoker

	// OpenRegistration mirrors CORE_OPEN_REGISTRATION. It is false by default in
	// config, and the default is the point: a self-hosted box found on the internet
	// with open sign-up is a box running strangers' code in your sandbox on your API
	// key.
	OpenRegistration bool
	// SessionTTL is how long a new session lives. Validated and bounded by config.
	SessionTTL time.Duration

	Clock  Clock
	Logger zerolog.Logger
}

// Accounts is sign-up, sign-in, passwords, and the operator's view of who exists.
//
// It is the only service that handles a credential, which is why the two ports that
// can reach one — PasswordStore and SessionStore — are separate interfaces and are
// used nowhere else in the package.
type Accounts struct {
	users     UserStore
	passwords PasswordStore
	admin     UserAdmin
	sessions  SessionStore
	revoker   SessionRevoker

	openRegistration bool
	sessionTTL       time.Duration

	clock Clock
	log   zerolog.Logger
}

// NewAccounts validates its wiring and returns a ready service.
func NewAccounts(deps AccountsDeps) (*Accounts, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Users != nil, "Users")
	require(deps.Passwords != nil, "Passwords")
	require(deps.Admin != nil, "Admin")
	require(deps.Sessions != nil, "Sessions")
	require(deps.Revoker != nil, "Revoker")
	require(deps.SessionTTL > 0, "SessionTTL")
	if len(missing) > 0 {
		return nil, fmt.Errorf("accounts: missing dependencies: %v", missing)
	}

	a := &Accounts{
		users:            deps.Users,
		passwords:        deps.Passwords,
		admin:            deps.Admin,
		sessions:         deps.Sessions,
		revoker:          deps.Revoker,
		openRegistration: deps.OpenRegistration,
		sessionTTL:       deps.SessionTTL,
		clock:            deps.Clock,
		log:              deps.Logger,
	}
	if a.clock == nil {
		a.clock = time.Now
	}
	return a, nil
}

// NewAccount is what creating an account needs.
type NewAccount struct {
	Email       string
	DisplayName string
	// Password is the plaintext, held only as long as it takes to hash. Nothing in this
	// package logs it, returns it, or stores it.
	Password string
}

// Create makes an account on an operator's say-so.
//
// This is what `core createuser` calls, and what an operator route calls. It works
// whether or not registration is open, because an operator making an account for
// somebody is not the case CORE_OPEN_REGISTRATION is about.
func (a *Accounts) Create(ctx context.Context, in NewAccount, actor Actor) (repository.User, error) {
	actor, err := actor.prepare()
	if err != nil {
		return repository.User{}, err
	}
	if err := actor.requireOperator("creating an account"); err != nil {
		return repository.User{}, err
	}
	return a.create(ctx, in)
}

// Register is the open sign-up path, and it takes no Actor because there is no caller
// to describe: the request arrives with no credential at all.
//
// That is exactly why it checks the flag itself. The route is unauthenticated, so the
// middleware has nothing to refuse on, and this refusal is the only one there is.
func (a *Accounts) Register(ctx context.Context, in NewAccount) (repository.User, error) {
	if !a.openRegistration {
		// ErrForbidden rather than a sentinel of its own: a stranger learns that they
		// may not sign up, and not whether anybody may. See the note in errors.go.
		return repository.User{}, fmt.Errorf("%w: registration is closed on this instance", ErrForbidden)
	}
	return a.create(ctx, in)
}

// create is the half both paths share: validate, hash, insert.
func (a *Accounts) create(ctx context.Context, in NewAccount) (repository.User, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	name := strings.TrimSpace(in.DisplayName)

	var errs domain.ValidationErrors
	if problem := checkEmail(email); problem != "" {
		errs = append(errs, domain.ValidationError{Field: "email", Message: problem})
	}
	if name == "" {
		errs = append(errs, domain.ValidationError{Field: "displayName", Message: "is required"})
	} else if len([]rune(name)) > maxDisplayNameLength {
		errs = append(errs, domain.ValidationError{
			Field:   "displayName",
			Message: fmt.Sprintf("must be at most %d characters", maxDisplayNameLength),
		})
	}
	// auth.ValidatePassword owns the strength rules, so there is one place that decides
	// what a password has to be and this one reports it.
	if err := auth.ValidatePassword(in.Password); err != nil {
		errs = append(errs, domain.ValidationError{Field: "password", Message: err.Error()})
	}
	if len(errs) > 0 {
		return repository.User{}, fmt.Errorf("%w: %w", ErrValidation, errs)
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		// Not wrapped with the password, obviously, and not with the address either:
		// hashing failing is a machine problem, and the account it was for adds nothing
		// to diagnosing it.
		return repository.User{}, fmt.Errorf("hash password for a new account: %w", err)
	}

	user, err := a.users.Create(ctx, email, name, hash)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return repository.User{}, fmt.Errorf("%w: that email address already has an account", ErrConflict)
		}
		if repository.IsConstraintViolation(err) {
			return repository.User{}, fmt.Errorf("%w: %w", ErrValidation, err)
		}
		return repository.User{}, err
	}

	a.log.Info().Str("userId", user.ID).Msg("account created")
	return user, nil
}

// SignIn is a credential check and a new session.
type SignIn struct {
	Email    string
	Password string
	// UserAgent and IP are recorded on the session so a person can recognise their own
	// devices in the list and revoke one they do not. Neither is trusted for anything.
	UserAgent string
	IP        string
}

// SignedIn is what a successful sign-in hands back.
type SignedIn struct {
	// Token is the plaintext session token. It is returned exactly once, here, and only
	// its hash is stored — so a database dump is not a set of live sessions.
	Token     string
	ExpiresAt time.Time
	User      repository.User
}

// SignIn verifies a password and mints a session.
//
// Every failure answers ErrUnauthenticated with the same sentence. An unknown address,
// a wrong password and a deactivated account are three different facts, and serving
// three different answers turns a sign-in form into a way to find out who has an
// account here and whether they still work here.
func (a *Accounts) SignIn(ctx context.Context, in SignIn) (SignedIn, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" || in.Password == "" {
		return SignedIn{}, fmt.Errorf("%w: an email address and a password are required", ErrValidation)
	}

	creds, err := a.passwords.GetCredentialsByEmail(ctx, email)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return SignedIn{}, err
	}

	// The verification runs even when there is no account, against a hash nothing
	// matches. Skipping it would answer an unknown address in a microsecond and a known
	// one in the hundred milliseconds argon2id costs, which is an account-enumeration
	// oracle measurable over the internet.
	stored := creds.PasswordHash
	if stored == "" {
		stored = auth.UnmatchableHash()
	}
	ok, verifyErr := auth.VerifyPassword(stored, in.Password)
	switch {
	case verifyErr != nil && !errors.Is(verifyErr, auth.ErrInvalidHash):
		// A machine problem, not a wrong password.
		return SignedIn{}, fmt.Errorf("verify a password: %w", verifyErr)
	case !ok, err != nil, !creds.IsActive:
		// One sentence for all three. The log line below is where an operator can tell
		// them apart, because that is a place a stranger cannot read.
		a.log.Info().
			Bool("accountExists", err == nil).
			Bool("passwordMatched", ok).
			Bool("accountActive", creds.IsActive).
			Msg("sign-in refused")
		return SignedIn{}, ErrUnauthenticated
	}

	plaintext, storedToken, err := auth.NewSessionToken()
	if err != nil {
		return SignedIn{}, fmt.Errorf("mint a session token: %w", err)
	}

	// This is the only moment core holds a plaintext password for an existing account,
	// so it is the only moment a hash written under weaker parameters can be upgraded.
	// A failure is logged and dropped: the sign-in is valid either way, and refusing it
	// over bookkeeping would lock somebody out of their own instance.
	if auth.NeedsRehash(stored) {
		if err := a.rehash(ctx, creds.UserID, in.Password); err != nil {
			a.log.Warn().Err(err).Str("userId", creds.UserID).
				Msg("could not upgrade a password hash to the current parameters")
		}
	}

	now := a.clock()
	session, err := a.sessions.Create(ctx, repository.NewSession{
		UserID:    creds.UserID,
		TokenHash: storedToken,
		UserAgent: in.UserAgent,
		CreatedIP: in.IP,
		ExpiresAt: now.Add(a.sessionTTL),
	})
	if err != nil {
		return SignedIn{}, err
	}

	user, err := a.users.GetByID(ctx, creds.UserID)
	if err != nil {
		// The session exists at this point. Returning an error would leave a live
		// session the caller never received, so it is revoked before answering.
		a.revokeQuietly(ctx, storedToken, now)
		return SignedIn{}, err
	}

	a.log.Info().Str("userId", user.ID).Str("sessionId", session.ID).Msg("signed in")
	return SignedIn{Token: plaintext, ExpiresAt: session.ExpiresAt, User: user}, nil
}

// SignOut ends the session the caller presented.
//
// It takes the presented token rather than a session id because that is what a client
// has, and because ending your own session should not require knowing its id. An
// already-dead token succeeds: a sign-out that failed because the session was gone
// would leave a client unable to clear its own state.
func (a *Accounts) SignOut(ctx context.Context, presented string) error {
	stored, err := auth.ParseSessionToken(presented)
	if err != nil {
		// Not an error the caller can act on, and not worth distinguishing: whatever
		// they presented is not a session, so there is nothing to end.
		return nil
	}
	if err := a.revoker.RevokeByToken(ctx, stored, a.clock()); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// ChangePassword replaces a person's password and ends every session they have.
//
// Every session, including the one making the request. A password change is what
// somebody does when they think another person has their session, and leaving that
// session alive is leaving the thing they were trying to close.
func (a *Accounts) ChangePassword(ctx context.Context, current, next string, actor Actor) error {
	actor, err := actor.prepare()
	if err != nil {
		return err
	}
	userID, err := actor.Owner()
	if err != nil {
		return err
	}

	if err := auth.ValidatePassword(next); err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, domain.ValidationErrors{
			{Field: "newPassword", Message: err.Error()},
		})
	}

	user, err := a.users.GetByID(ctx, userID)
	if err != nil {
		return a.mapNotFound(err)
	}
	creds, err := a.passwords.GetCredentialsByEmail(ctx, user.Email)
	if err != nil {
		return a.mapNotFound(err)
	}

	ok, err := auth.VerifyPassword(creds.PasswordHash, current)
	if err != nil && !errors.Is(err, auth.ErrInvalidHash) {
		return fmt.Errorf("verify a password: %w", err)
	}
	if !ok {
		// ErrUnauthenticated rather than ErrValidation: the current password is a
		// credential, and this is it failing to check out.
		return ErrUnauthenticated
	}

	hash, err := auth.HashPassword(next)
	if err != nil {
		return fmt.Errorf("hash a new password: %w", err)
	}
	if err := a.passwords.UpdatePassword(ctx, userID, hash); err != nil {
		return a.mapNotFound(err)
	}

	revoked, err := a.revoker.RevokeAllForUser(ctx, userID, a.clock())
	if err != nil {
		// The password is already changed. Reporting a failure here would say the change
		// did not happen, which is worse than saying it did and that the old sessions
		// may still be live — so it is logged loudly and the caller is told to sign in
		// again anyway.
		a.log.Error().Err(err).Str("userId", userID).
			Msg("password changed but sessions could not be revoked; they remain live until they expire")
		return nil
	}
	a.log.Info().Str("userId", userID).Int("sessionsRevoked", revoked).Msg("password changed")
	return nil
}

// Me is the account behind the caller's session.
func (a *Accounts) Me(ctx context.Context, actor Actor) (repository.User, error) {
	actor, err := actor.prepare()
	if err != nil {
		return repository.User{}, err
	}
	userID, err := actor.Owner()
	if err != nil {
		return repository.User{}, err
	}

	user, err := a.users.GetByID(ctx, userID)
	if err != nil {
		return repository.User{}, a.mapNotFound(err)
	}
	return user, nil
}

// List is the operator's view of who exists.
func (a *Accounts) List(ctx context.Context, limit, offset int, actor Actor) ([]repository.User, int, error) {
	actor, err := actor.prepare()
	if err != nil {
		return nil, 0, err
	}
	if err := actor.requireOperator("listing accounts"); err != nil {
		return nil, 0, err
	}
	return a.admin.List(ctx, limit, offset)
}

// SetActive enables or disables an account.
//
// Disabling revokes every session the person holds, because an account that cannot be
// signed in to but whose existing session still works is not disabled. Enabling revokes
// nothing: there is nothing to revoke, and reactivating somebody should not punish them.
func (a *Accounts) SetActive(ctx context.Context, userID string, active bool, actor Actor) error {
	actor, err := actor.prepare()
	if err != nil {
		return err
	}
	if err := actor.requireOperator("enabling or disabling an account"); err != nil {
		return err
	}
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("%w: an account id is required", ErrValidation)
	}

	if err := a.admin.SetActive(ctx, userID, active); err != nil {
		return a.mapNotFound(err)
	}
	if active {
		a.log.Info().Str("userId", userID).Str("by", actor.String()).Msg("account enabled")
		return nil
	}

	revoked, err := a.revoker.RevokeAllForUser(ctx, userID, a.clock())
	if err != nil {
		// Same reasoning as ChangePassword, and worse in consequence: an operator who
		// disabled somebody has to know their sessions outlived it.
		a.log.Error().Err(err).Str("userId", userID).
			Msg("account disabled but sessions could not be revoked; they remain live until they expire")
		return nil
	}
	a.log.Info().Str("userId", userID).Str("by", actor.String()).Int("sessionsRevoked", revoked).
		Msg("account disabled")
	return nil
}

// Count is how many accounts exist. It is what `core createuser` reads to tell an
// operator that they have just made the first one.
func (a *Accounts) Count(ctx context.Context, actor Actor) (int, error) {
	actor, err := actor.prepare()
	if err != nil {
		return 0, err
	}
	if err := actor.requireOperator("counting accounts"); err != nil {
		return 0, err
	}
	return a.users.Count(ctx)
}

// ResolveOwner turns a configured email address into the account id unattended work is
// filed against.
//
// It is called once, at boot, by cmd — never from a request. An operator who set
// CORE_UNATTENDED_OWNER to an address with no account learns it while starting the
// process, rather than when the goal engine dispatches at three in the morning.
func (a *Accounts) ResolveOwner(ctx context.Context, email string) (repository.User, error) {
	trimmed := strings.ToLower(strings.TrimSpace(email))
	if trimmed == "" {
		return repository.User{}, fmt.Errorf("%w: an owner address is required", ErrValidation)
	}

	user, err := a.users.GetByEmail(ctx, trimmed)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The address is quoted here and only here: this error goes to an operator's
			// own terminal while they configure the box, and telling them which address
			// has no account is the whole value of the message.
			return repository.User{}, fmt.Errorf("%w: no account for %q; create it with `core createuser` first", ErrNotFound, trimmed)
		}
		return repository.User{}, err
	}
	if !user.IsActive {
		return repository.User{}, fmt.Errorf("%w: the account for %q is disabled, so unattended work would have nowhere to go", ErrValidation, trimmed)
	}
	return user, nil
}

// rehash stores password again under the current argon2id parameters.
//
// Deliberately not a revocation: nothing about the credential changed, only the cost of
// verifying it, so the sessions the person already holds are still theirs.
func (a *Accounts) rehash(ctx context.Context, userID, password string) error {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	return a.passwords.UpdatePassword(ctx, userID, hash)
}

// revokeQuietly ends a session on a path that is already returning an error, where a
// second failure has nowhere to go but the log.
func (a *Accounts) revokeQuietly(ctx context.Context, storedToken string, now time.Time) {
	if err := a.revoker.RevokeByToken(ctx, storedToken, now); err != nil && !errors.Is(err, repository.ErrNotFound) {
		a.log.Error().Err(err).Msg("could not revoke a session that was never handed out")
	}
}

// mapNotFound turns the repository's absence into this package's, and leaves everything
// else alone. Called at this boundary rather than in a handler so a repository sentinel
// never reaches the HTTP layer.
func (a *Accounts) mapNotFound(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// checkEmail reports what is wrong with an address, or "" if nothing is.
//
// It is a shape check, not a validity check: the only way to know an address exists is
// to send mail to it, and core sends none. What it rules out is a value that could not
// be an address at all, and one long enough to be a payload.
func checkEmail(email string) string {
	switch {
	case email == "":
		return "is required"
	case len(email) > maxEmailLength:
		return fmt.Sprintf("must be at most %d characters", maxEmailLength)
	}

	at := strings.IndexByte(email, '@')
	switch {
	case at < 0:
		return "must contain @"
	case strings.ContainsAny(email, " \t\r\n"):
		return "must not contain spaces"
	}

	local, host := email[:at], email[at+1:]
	switch {
	case local == "" || host == "":
		return "must have something either side of the @"
	case strings.Contains(host, "@"):
		return "must contain exactly one @"
	case !strings.Contains(host, "."):
		return "must have a domain with a dot in it"
	}
	return ""
}
