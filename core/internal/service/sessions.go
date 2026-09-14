package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/repository"
)

// SessionsDeps is everything the session service needs. The janitor is optional: an
// instance that never sweeps still refuses expired sessions, it just keeps the rows.
type SessionsDeps struct {
	Sessions SessionStore
	Revoker  SessionRevoker
	Janitor  SessionJanitor

	Clock  Clock
	Logger zerolog.Logger
}

// Sessions is what a person can see and do about their own signed-in devices.
//
// It is separate from Accounts because the two answer different questions — who you are
// against where you are signed in — and because this one is reachable by a person for
// their own rows only. Nothing here takes a user id from the caller: it comes from the
// actor, through Owner().
type Sessions struct {
	sessions SessionStore
	revoker  SessionRevoker
	janitor  SessionJanitor

	clock Clock
	log   zerolog.Logger
}

// NewSessions validates its wiring and returns a ready service.
func NewSessions(deps SessionsDeps) (*Sessions, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Sessions != nil, "Sessions")
	require(deps.Revoker != nil, "Revoker")
	if len(missing) > 0 {
		return nil, fmt.Errorf("sessions: missing dependencies: %v", missing)
	}

	s := &Sessions{
		sessions: deps.Sessions,
		revoker:  deps.Revoker,
		janitor:  deps.Janitor,
		clock:    deps.Clock,
		log:      deps.Logger,
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	return s, nil
}

// List is the caller's own devices, newest first, as the repository orders them.
//
// Revoked and expired sessions are included rather than filtered out, because "this
// laptop signed out last Tuesday" is what somebody checking for a session they do not
// recognise actually needs to see. repository.Session carries no token hash, so the list
// is not a set of credentials.
func (s *Sessions) List(ctx context.Context, limit, offset int, actor Actor) ([]repository.Session, error) {
	actor, err := actor.prepare()
	if err != nil {
		return nil, err
	}
	userID, err := actor.Owner()
	if err != nil {
		return nil, err
	}
	return s.sessions.ListForUser(ctx, userID, limit, offset)
}

// Revoke ends one of the caller's sessions by id.
//
// The user id is part of the query rather than checked against the row afterwards, so a
// session id belonging to somebody else is simply not found — which is the same answer
// as an id that never existed, and says nothing either way.
func (s *Sessions) Revoke(ctx context.Context, sessionID string, actor Actor) error {
	actor, err := actor.prepare()
	if err != nil {
		return err
	}
	userID, err := actor.Owner()
	if err != nil {
		return err
	}
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("%w: a session id is required", ErrValidation)
	}

	if err := s.revoker.RevokeByID(ctx, userID, sessionID, s.clock()); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.log.Info().Str("userId", userID).Str("sessionId", sessionID).Msg("session revoked")
	return nil
}

// RevokeAll ends every session the caller holds, including the one making the request.
//
// Including it deliberately. This is the "sign out everywhere" button, and a version
// that spared the current device would leave the one session somebody at a shared
// machine was trying to close.
func (s *Sessions) RevokeAll(ctx context.Context, actor Actor) (int, error) {
	actor, err := actor.prepare()
	if err != nil {
		return 0, err
	}
	userID, err := actor.Owner()
	if err != nil {
		return 0, err
	}

	revoked, err := s.revoker.RevokeAllForUser(ctx, userID, s.clock())
	if err != nil {
		return 0, err
	}
	s.log.Info().Str("userId", userID).Int("sessionsRevoked", revoked).Msg("all sessions revoked")
	return revoked, nil
}

// Sweep deletes sessions that expired before now, and reports how many.
//
// Hygiene, not enforcement: an expired session is already refused at the door by
// repository.SessionRepository.Resolve, which compares against now. This keeps the table
// from growing forever, which is why a missing janitor is not an error — an instance
// without one is untidy, not unsafe.
//
// It takes no Actor because no request reaches it. cmd runs it on a ticker.
func (s *Sessions) Sweep(ctx context.Context) (int, error) {
	if s.janitor == nil {
		return 0, nil
	}

	deleted, err := s.janitor.DeleteExpired(ctx, s.clock())
	if err != nil {
		return 0, fmt.Errorf("sweep expired sessions: %w", err)
	}
	if deleted > 0 {
		s.log.Info().Int("sessionsDeleted", deleted).Msg("expired sessions swept")
	}
	return deleted, nil
}
