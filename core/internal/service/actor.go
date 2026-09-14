package service

import (
	"fmt"
	"strings"
)

// ActorType is the kind of caller a service is acting for. It mirrors middleware.Role,
// declared here so this package does not import the HTTP layer and so the three kinds
// are readable next to the rules written against them.
type ActorType string

const (
	// ActorOperator is whoever runs this instance, holding a key from CORE_API_KEYS, or
	// the CLI running on the box itself.
	ActorOperator ActorType = "operator"
	// ActorBot is a service holding a key from CORE_BOT_KEYS — in practice the goal
	// engine's trigger bridge.
	ActorBot ActorType = "bot"
	// ActorUser is a signed-in human. No environment key can produce one.
	ActorUser ActorType = "user"
)

// Actor is who a service is acting for. It is an explicit parameter on every operation
// that authorises, and it never travels in a context.Context: a permission that can be
// smuggled through a context is a permission nobody can find by reading a signature.
type Actor struct {
	Type ActorType
	// ID is who, in the terms the credential was configured with: a machine key's
	// configured name, or a person's account id.
	ID string
	// UserID is the account whose rows this actor may read and write. It is set for a
	// person and empty for a machine, which is why every operation that touches a user's
	// content asks Owner() for it rather than reading ID.
	UserID string
	// RequestID ties log lines from one request together. Empty for background work.
	RequestID string
}

// SystemActor is core acting on its own behalf: the CLI subcommands, and the background
// sweeps that requeue abandoned work.
//
// It is an operator because the things it does are operator-scoped, and because
// reaching it at all requires a shell on the machine — which is more privilege than any
// key in the environment grants.
func SystemActor() Actor {
	return Actor{Type: ActorOperator, ID: "system"}
}

// normalise trims the fields that arrive from configuration or a header.
//
// Unlike goal-engine's, it supplies no default type. There are three kinds here and
// they are not ordered — a bot may dispatch unattended work that a person may not, and
// a person may read chats a bot may not — so there is no "least privileged" one to fall
// back to. An actor with no type is a wiring bug, and validate refuses it.
func (a Actor) normalise() Actor {
	a.Type = ActorType(strings.TrimSpace(string(a.Type)))
	a.ID = strings.TrimSpace(a.ID)
	a.UserID = strings.TrimSpace(a.UserID)
	a.RequestID = strings.TrimSpace(a.RequestID)
	return a
}

// validate refuses an actor no rule in this package can be applied to.
func (a Actor) validate() error {
	switch a.Type {
	case ActorOperator, ActorBot:
		// A machine key must not name an account. The account an unattended task
		// belongs to comes from configuration — CORE_UNATTENDED_OWNER, read at boot —
		// and is passed as configuration. A machine that could name its own owner in
		// the request could file work against anybody's budget.
		if a.UserID != "" {
			return fmt.Errorf("%w: a %s actor must not carry a user id", ErrValidation, a.Type)
		}
	case ActorUser:
		// Without it every query that scopes on user_id would scope on nothing.
		// middleware.Authenticate refuses to build a user caller without one; this is
		// the same refusal, one layer in, for the paths that do not come from a request.
		if a.UserID == "" {
			return fmt.Errorf("%w: a user actor must carry the account it is acting as", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: %q is not a kind of caller", ErrValidation, a.Type)
	}
	if a.ID == "" {
		return fmt.Errorf("%w: an actor must be named", ErrValidation)
	}
	return nil
}

// requireOperator is the second half of an operator-only check.
//
// The first half is middleware.RequireOperator on the route. This is not redundancy to
// tidy away: a route added later without the middleware would otherwise be a privilege
// escalation rather than an oversight, and the operation names in these errors are how
// an operator reading a 403 learns which check refused them.
func (a Actor) requireOperator(operation string) error {
	if a.Type != ActorOperator {
		return fmt.Errorf("%w: %s is operator-only, and this request came from %s", ErrForbidden, operation, a)
	}
	return nil
}

// requireDispatcher permits the two callers that may create work nobody is watching:
// an operator, and the goal engine's bridge.
//
// A person may not, and that is the point. An unattended task runs against a budget
// with no one at the keyboard to notice it going wrong, so the decision to start one
// belongs to whoever owns the instance.
func (a Actor) requireDispatcher(operation string) error {
	if a.Type != ActorOperator && a.Type != ActorBot {
		return fmt.Errorf("%w: %s is for operators and the goal engine, and this request came from %s", ErrForbidden, operation, a)
	}
	return nil
}

// Owner is the account whose rows this actor may touch, or an error saying it has none.
//
// A machine key deliberately has no owner. It reaches a person's data only through an
// operation that was given the account id from somewhere trustworthy — configuration,
// or a channel identity a person linked themselves — never by reading it off the
// caller.
func (a Actor) Owner() (string, error) {
	if a.Type != ActorUser || a.UserID == "" {
		return "", fmt.Errorf("%w: %s is not acting for an account", ErrForbidden, a)
	}
	return a.UserID, nil
}

// String is the form that goes in a log line: kind and name, never a secret. It is what
// makes a refusal readable without the reader having to correlate a request id against
// the access log.
func (a Actor) String() string {
	if a.ID == "" {
		return string(a.Type)
	}
	return string(a.Type) + ":" + a.ID
}

// prepare is the first statement of every authorising operation: normalise, then
// validate, then hand back the actor the rest of the method uses.
//
// One call rather than two so a method cannot validate the untrimmed actor and then act
// on it — the bug that a "  operator  " in a configured key name would otherwise cause.
func (a Actor) prepare() (Actor, error) {
	actor := a.normalise()
	if err := actor.validate(); err != nil {
		return Actor{}, err
	}
	return actor, nil
}
