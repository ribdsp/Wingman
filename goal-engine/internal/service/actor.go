package service

import (
	"fmt"
	"strings"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// Actor is who asked for something, carried explicitly rather than hidden in a
// context value.
//
// Every write in this package lands in the business audit log, and the log is
// only worth keeping if it answers "who moved the target". Passing the actor as a
// parameter means a caller cannot forget it silently: the compiler asks for it.
type Actor struct {
	// Type separates an operator from a bot. An agent creating its own goals is
	// exactly the loop Wingman is built around, so it has to be visible in the
	// log rather than indistinguishable from a human.
	Type repository.ActorType
	// ID is the authenticated principal — an operator name or a bot id.
	ID string
	// RequestID ties the entry back to one HTTP request.
	RequestID string
}

// SystemActor is the engine acting on its own schedule, for background work that
// no request initiated.
func SystemActor() Actor {
	return Actor{Type: repository.ActorSystem, ID: "goal-engine"}
}

// normalise trims the actor and fills in a type, defaulting to the least
// privileged reading: an unnamed caller is a bot, not an operator.
func (a Actor) normalise() Actor {
	out := a
	out.ID = strings.TrimSpace(out.ID)
	out.RequestID = strings.TrimSpace(out.RequestID)
	switch out.Type {
	case repository.ActorUser, repository.ActorBot, repository.ActorSystem:
	default:
		out.Type = repository.ActorBot
	}
	return out
}

// validate rejects an unattributed write. An audit entry with no actor is not an
// audit entry.
func (a Actor) validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return fieldError("actor", "is required")
	}
	return nil
}

// IsOperator reports whether this is a human.
//
// A handful of operations in this package are a human's alone: clearing a spend,
// releasing the kill switch, rewriting a goal. Each of those is an agent's way to
// hand itself what the design says it has to be given, so each one asks this
// question rather than trusting the route it arrived on.
func (a Actor) IsOperator() bool { return a.Type == repository.ActorUser }

// requireOperator turns a non-human actor away from a human's decision.
func (a Actor) requireOperator(what string) error {
	if a.IsOperator() {
		return nil
	}
	return fmt.Errorf("%w: %s is reserved for a human operator, not a %s", ErrForbidden, what, a.Type)
}
