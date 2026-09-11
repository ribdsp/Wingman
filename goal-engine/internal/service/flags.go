package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// maxFlagReasonLength bounds the note stored with a flag change.
const maxFlagReasonLength = 500

// FlagsDeps is everything the flag service needs.
type FlagsDeps struct {
	Flags FlagAdmin
	Audit AuditSink

	Clock  Clock
	Logger zerolog.Logger
}

// Flags is the operator's stop button.
//
// The kill switch is the one control that has to work when everything else is
// going wrong, so this service is deliberately thin: read a row, write a row,
// record who did it. Nothing here can fail in a way that leaves the switch in an
// unknown state, because the readers treat an unreadable switch as engaged.
type Flags struct {
	flags FlagAdmin
	audit AuditSink
	clock Clock
	log   zerolog.Logger
}

// NewFlags validates its wiring and returns a ready flag service.
func NewFlags(deps FlagsDeps) (*Flags, error) {
	missing := []string{}
	if deps.Flags == nil {
		missing = append(missing, "Flags")
	}
	if deps.Audit == nil {
		missing = append(missing, "Audit")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("flags: missing dependencies: %v", missing)
	}

	f := &Flags{
		flags: deps.Flags,
		audit: deps.Audit,
		clock: deps.Clock,
		log:   deps.Logger,
	}
	if f.clock == nil {
		f.clock = time.Now
	}
	return f, nil
}

// KillSwitch reads the current state of the switch.
func (f *Flags) KillSwitch(ctx context.Context) (repository.Flag, error) {
	flag, err := f.flags.Get(ctx, repository.KillSwitchKey)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// A switch that was never thrown has no row. That is "not engaged", and
			// it is safe to report as such here because this is an operator reading
			// the state, not the engine deciding whether it may act — the engine's
			// own read treats any doubt as engaged.
			return repository.Flag{Key: repository.KillSwitchKey}, nil
		}
		return repository.Flag{}, fmt.Errorf("flags: read kill switch: %w", err)
	}
	return flag, nil
}

// SetKillSwitch engages or releases the switch.
//
// Engaging it stops the monitor waking agents and makes the approval gate deny
// everything. A reason is required: this is the entry somebody will read while
// working out why the engine went quiet.
//
// The two directions are not equally guarded, on purpose. Anyone holding a
// credential may engage it — an agent that notices it is doing damage should be
// able to stop the fleet, and the worst case is an outage an operator can undo.
// Releasing it is a human's alone: a compromised or confused agent that could
// turn its own brakes off would make the switch meaningless exactly when it
// matters.
func (f *Flags) SetKillSwitch(ctx context.Context, engaged bool, reason string, actor Actor) (repository.Flag, error) {
	actor = actor.normalise()
	if err := actor.validate(); err != nil {
		return repository.Flag{}, err
	}
	if !engaged {
		if err := actor.requireOperator("releasing the kill switch"); err != nil {
			return repository.Flag{}, err
		}
	}
	reason = truncateRunes(strings.TrimSpace(reason), maxFlagReasonLength)
	if reason == "" {
		return repository.Flag{}, fieldError("reason", "is required")
	}

	flag, err := f.flags.Set(ctx, repository.KillSwitchKey, engaged, reason, actor.ID)
	if err != nil {
		return repository.Flag{}, fmt.Errorf("flags: set kill switch: %w", err)
	}

	outcome := "released"
	if engaged {
		outcome = "engaged"
	}
	if _, auditErr := f.audit.Append(ctx, repository.AuditEvent{
		ActorType:   actor.Type,
		ActorID:     actor.ID,
		Action:      ActionKillSwitchSet,
		SubjectType: SubjectFlag,
		SubjectID:   repository.KillSwitchKey,
		Outcome:     outcome,
		RequestID:   actor.RequestID,
		Detail:      detailJSON(map[string]any{"engaged": engaged, "reason": reason}),
	}); auditErr != nil {
		f.log.Error().Err(auditErr).Bool("engaged", engaged).Msg("failed to audit kill switch change")
	}

	// Logged at Warn either way: both directions of this switch are events somebody
	// operating the system wants to find in the logs without filtering for them.
	f.log.Warn().
		Bool("engaged", engaged).
		Str("actor", actor.ID).
		Str("reason", reason).
		Msg("kill switch changed")

	return flag, nil
}

// List reads every flag.
func (f *Flags) List(ctx context.Context) ([]repository.Flag, error) {
	flags, err := f.flags.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("flags: list: %w", err)
	}
	return flags, nil
}
