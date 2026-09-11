package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

const (
	// DefaultApprovalTTL is how long a pending request waits for a human before it
	// expires. Silence is not consent: an unanswered request must lapse rather than
	// sit forever waiting to be approved by somebody who has forgotten it.
	DefaultApprovalTTL = 24 * time.Hour

	// maxNoteLength bounds an operator's resolution note.
	maxNoteLength = 2000
)

// SpendRequest is a bot asking permission to spend.
type SpendRequest struct {
	// ActionType selects the policy. An action type with no policy is not free —
	// it goes to a human.
	ActionType string
	Amount     float64
	Currency   string
	// RequestedBy is the bot asking. Recorded for the audit trail, never used to
	// widen a limit.
	RequestedBy string
	// GoalID links the spend to the goal it serves, when there is one.
	GoalID *string
	// IdempotencyKey lets a bot retry a request without opening a second one.
	IdempotencyKey string
	// Payload is the action's own detail, stored verbatim as JSON for the human
	// who has to decide.
	Payload string
}

// ApprovalsDeps is everything the approval gate needs.
type ApprovalsDeps struct {
	Approvals ApprovalStore
	Spend     SpendStore
	Policies  PolicyLookup
	Flags     FlagStore
	Audit     AuditSink

	// TTL overrides DefaultApprovalTTL.
	TTL time.Duration
	// Clock defaults to time.Now.
	Clock  Clock
	Logger zerolog.Logger
}

// Approvals is the gate between an agent deciding to spend and money moving.
//
// Every decision is made here and recorded here. The agent is told the outcome;
// it is never asked for it.
type Approvals struct {
	approvals ApprovalStore
	spend     SpendStore
	policies  PolicyLookup
	flags     FlagStore
	audit     AuditSink
	ttl       time.Duration
	clock     Clock
	log       zerolog.Logger
}

// NewApprovals validates its wiring and returns a ready gate.
func NewApprovals(deps ApprovalsDeps) (*Approvals, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Approvals != nil, "Approvals")
	require(deps.Spend != nil, "Spend")
	require(deps.Policies != nil, "Policies")
	require(deps.Flags != nil, "Flags")
	require(deps.Audit != nil, "Audit")
	if len(missing) > 0 {
		return nil, fmt.Errorf("approvals: missing dependencies: %v", missing)
	}

	a := &Approvals{
		approvals: deps.Approvals,
		spend:     deps.Spend,
		policies:  deps.Policies,
		flags:     deps.Flags,
		audit:     deps.Audit,
		ttl:       deps.TTL,
		clock:     deps.Clock,
		log:       deps.Logger,
	}
	if a.ttl <= 0 {
		a.ttl = DefaultApprovalTTL
	}
	if a.clock == nil {
		a.clock = time.Now
	}
	return a, nil
}

// Request decides whether an agent may spend, and records the decision.
//
// The whole sequence — read today's spend, apply the policy, record the outcome —
// runs inside one advisory lock per action type. Reading the ledger outside the
// lock would make the daily cap advisory: two requests arriving together would
// both see room under it and both be allowed through.
func (a *Approvals) Request(ctx context.Context, req SpendRequest) (repository.ApprovalRecord, error) {
	if err := validateSpendRequest(req); err != nil {
		return repository.ApprovalRecord{}, err
	}
	actionType := strings.TrimSpace(req.ActionType)
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))

	// A retry of a request already decided returns that decision rather than
	// opening a second one. Two open requests for one action are how an operator
	// ends up approving the same spend twice.
	if req.IdempotencyKey != "" {
		existing, err := a.approvals.GetByIdempotencyKey(ctx, req.IdempotencyKey)
		switch {
		case err == nil:
			return existing, nil
		case !errors.Is(err, repository.ErrNotFound):
			return repository.ApprovalRecord{}, fmt.Errorf("approvals: look up idempotency key: %w", err)
		}
	}

	// An unreadable kill switch is not "off". The decision core reads an engaged
	// switch as a denial, so a failed read must surface as an error rather than a
	// silent false.
	engaged, err := a.flags.KillSwitchEngaged(ctx)
	if err != nil {
		return repository.ApprovalRecord{}, fmt.Errorf("approvals: read kill switch: %w", err)
	}

	policy := a.policies.Lookup(actionType)
	now := a.clock()
	dayStart := startOfDay(now)

	var record repository.ApprovalRecord
	var decision domain.ApprovalDecision

	err = a.spend.WithinActionLock(ctx, actionType, func(locked repository.SpendLedger) error {
		spentToday, err := locked.SpentSince(ctx, actionType, currency, dayStart)
		if err != nil {
			return fmt.Errorf("read spend since %s: %w", dayStart.Format(time.RFC3339), err)
		}

		decision = domain.DecideApproval(domain.ApprovalRequest{
			ActionType:        actionType,
			Amount:            req.Amount,
			Currency:          currency,
			SpentToday:        spentToday,
			Policy:            policy,
			KillSwitchEngaged: engaged,
		})

		input := repository.ApprovalInput{
			ActionType:     actionType,
			Amount:         req.Amount,
			Currency:       currency,
			RequestedBy:    req.RequestedBy,
			GoalID:         req.GoalID,
			IdempotencyKey: req.IdempotencyKey,
			Outcome:        decision.Outcome,
			PolicyReason:   decision.Reason,
			Payload:        req.Payload,
		}
		if decision.Outcome == domain.ApprovalPending {
			expires := now.Add(a.ttl)
			input.ExpiresAt = &expires
		}

		created, err := a.approvals.Create(ctx, input)
		if err != nil {
			return fmt.Errorf("record approval: %w", err)
		}
		record = created

		// An auto-approved request is spend that will happen with nobody watching,
		// so it lands on the ledger inside the same lock that cleared it. Recording
		// it afterwards would let a burst of small amounts each read a stale total
		// and slip past the daily cap together.
		if decision.Outcome == domain.ApprovalAutoApproved {
			id := created.ID
			if _, err := locked.Record(ctx, repository.SpendInput{
				ActionType: actionType,
				Amount:     req.Amount,
				Currency:   currency,
				ApprovalID: &id,
				BotID:      req.RequestedBy,
				OccurredAt: now,
				Note:       "auto-approved under policy threshold",
			}); err != nil {
				return fmt.Errorf("record auto-approved spend: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return repository.ApprovalRecord{}, fmt.Errorf("approvals: decide %s: %w", actionType, err)
	}

	a.appendAudit(ctx, repository.AuditEvent{
		ActorType:   repository.ActorBot,
		ActorID:     req.RequestedBy,
		Action:      ActionApprovalDecided,
		SubjectType: SubjectApproval,
		SubjectID:   record.ID,
		Outcome:     string(decision.Outcome),
		Detail: detailJSON(map[string]any{
			"actionType": actionType,
			"amount":     req.Amount,
			"currency":   currency,
			"reason":     decision.Reason,
			"goalId":     req.GoalID,
			"policySet":  policy != nil,
		}),
	})
	a.log.Info().
		Str("approvalId", record.ID).
		Str("actionType", actionType).
		Float64("amount", req.Amount).
		Str("currency", currency).
		Str("outcome", string(decision.Outcome)).
		Msg("approval decided")

	return record, nil
}

// Resolve records a human's answer to a pending request.
//
// Approving is what puts the amount on the ledger: until a human says yes, an
// approval request is a question, not a commitment. Only an operator may answer
// it — an agent resolving its own request is the failure this whole gate exists
// to prevent.
func (a *Approvals) Resolve(ctx context.Context, id string, resolution repository.ApprovalResolution, note string, actor Actor) (repository.ApprovalRecord, error) {
	actor = actor.normalise()
	if err := actor.validate(); err != nil {
		return repository.ApprovalRecord{}, err
	}
	// The entire point of a pending approval is that a human sees it. An agent that
	// could resolve its own request would have found a way to spend without asking,
	// and the gate would be decoration. The route is guarded too; this is the rule.
	if err := actor.requireOperator("resolving an approval"); err != nil {
		return repository.ApprovalRecord{}, err
	}
	if strings.TrimSpace(id) == "" {
		return repository.ApprovalRecord{}, fmt.Errorf("%w: approval id is required", ErrValidation)
	}
	if resolution != repository.ResolutionApproved && resolution != repository.ResolutionRejected {
		return repository.ApprovalRecord{}, fmt.Errorf("%w: resolution must be approved or rejected", ErrValidation)
	}
	resolvedBy := actor.ID

	existing, err := a.approvals.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return repository.ApprovalRecord{}, fmt.Errorf("%w: approval %s", ErrNotFound, id)
		}
		return repository.ApprovalRecord{}, fmt.Errorf("approvals: read %s: %w", id, err)
	}
	if !existing.IsOpen() {
		return repository.ApprovalRecord{}, fmt.Errorf("%w: %s is %s", ErrAlreadyResolved, id, existing.Outcome)
	}

	resolved, err := a.approvals.Resolve(ctx, id, resolution, resolvedBy, truncateRunes(strings.TrimSpace(note), maxNoteLength))
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			// Somebody resolved it between the read and the write. Their answer
			// stands; a second one would overwrite a decision already acted on.
			return repository.ApprovalRecord{}, fmt.Errorf("%w: %s", ErrAlreadyResolved, id)
		}
		return repository.ApprovalRecord{}, fmt.Errorf("approvals: resolve %s: %w", id, err)
	}

	if resolution == repository.ResolutionApproved {
		if err := a.recordApprovedSpend(ctx, resolved); err != nil {
			// The approval stands — a human said yes and that is recorded. But the
			// ledger is now behind, which means the daily cap is under-counting, so
			// the caller has to hear about it.
			return resolved, err
		}
	}

	a.appendAudit(ctx, repository.AuditEvent{
		ActorType:   actor.Type,
		ActorID:     resolvedBy,
		Action:      ActionApprovalResolved,
		SubjectType: SubjectApproval,
		SubjectID:   resolved.ID,
		Outcome:     string(resolution),
		RequestID:   actor.RequestID,
		Detail: detailJSON(map[string]any{
			"actionType": resolved.ActionType,
			"amount":     resolved.Amount,
			"currency":   resolved.Currency,
			"note":       note,
		}),
	})
	a.log.Info().
		Str("approvalId", resolved.ID).
		Str("resolution", string(resolution)).
		Str("resolvedBy", resolvedBy).
		Msg("approval resolved by human")

	return resolved, nil
}

// recordApprovedSpend puts a human-approved amount on the ledger, under the same
// per-action lock the automatic path uses so both feed one consistent total.
func (a *Approvals) recordApprovedSpend(ctx context.Context, record repository.ApprovalRecord) error {
	id := record.ID
	err := a.spend.WithinActionLock(ctx, record.ActionType, func(locked repository.SpendLedger) error {
		_, err := locked.Record(ctx, repository.SpendInput{
			ActionType: record.ActionType,
			Amount:     record.Amount,
			Currency:   record.Currency,
			ApprovalID: &id,
			BotID:      record.RequestedBy,
			OccurredAt: a.clock(),
			Note:       "approved by human",
		})
		return err
	})
	if err != nil {
		a.log.Error().Err(err).Str("approvalId", id).
			Msg("approved spend could not be recorded: the daily cap is now under-counting")
		return fmt.Errorf("approvals: record approved spend for %s: %w", id, err)
	}
	a.appendAudit(ctx, repository.AuditEvent{
		ActorType:   repository.ActorSystem,
		ActorID:     "approvals",
		Action:      ActionSpendRecorded,
		SubjectType: SubjectApproval,
		SubjectID:   id,
		Outcome:     "recorded",
		Detail: detailJSON(map[string]any{
			"actionType": record.ActionType,
			"amount":     record.Amount,
			"currency":   record.Currency,
		}),
	})
	return nil
}

// Get returns one approval request.
func (a *Approvals) Get(ctx context.Context, id string) (repository.ApprovalRecord, error) {
	record, err := a.approvals.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return repository.ApprovalRecord{}, fmt.Errorf("%w: approval %s", ErrNotFound, id)
		}
		return repository.ApprovalRecord{}, fmt.Errorf("approvals: read %s: %w", id, err)
	}
	return record, nil
}

// List returns approval requests matching the filter, with the total count.
func (a *Approvals) List(ctx context.Context, filter repository.ApprovalFilter) ([]repository.ApprovalRecord, int, error) {
	records, total, err := a.approvals.List(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("approvals: list: %w", err)
	}
	return records, total, nil
}

// ExpireOverdue lapses pending requests nobody answered in time.
//
// This is what keeps "waiting for a human" from becoming a permanent state that a
// later operator mistakes for a live question.
func (a *Approvals) ExpireOverdue(ctx context.Context) (int, error) {
	now := a.clock()
	count, err := a.approvals.ExpireOverdue(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("approvals: expire overdue: %w", err)
	}
	if count > 0 {
		a.appendAudit(ctx, repository.AuditEvent{
			At:          now,
			ActorType:   repository.ActorSystem,
			ActorID:     "approvals",
			Action:      ActionApprovalResolved,
			SubjectType: SubjectApproval,
			SubjectID:   "batch",
			Outcome:     string(repository.ResolutionExpired),
			Detail:      detailJSON(map[string]any{"count": count}),
		})
		a.log.Warn().Int("count", count).Msg("approval requests expired unanswered")
	}
	return count, nil
}

// validateSpendRequest rejects a request that cannot be decided.
//
// The amount itself is left to the decision core, which refuses anything
// non-positive or non-finite: keeping that rule in one place means the boundary
// cannot disagree with the gate.
func validateSpendRequest(req SpendRequest) error {
	if strings.TrimSpace(req.ActionType) == "" {
		return fmt.Errorf("%w: actionType is required", ErrValidation)
	}
	if strings.TrimSpace(req.Currency) == "" {
		return fmt.Errorf("%w: currency is required", ErrValidation)
	}
	if strings.TrimSpace(req.RequestedBy) == "" {
		return fmt.Errorf("%w: requestedBy is required", ErrValidation)
	}
	return nil
}

// startOfDay truncates to midnight in t's own location, which is the operator's
// timezone when the clock is configured with one. A cap called "daily" has to
// reset when the operator's day does, not when UTC's does.
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// appendAudit writes one audit entry, logging loudly if it cannot. A failed audit
// write never fails the caller: the decision it describes has already been made
// and recorded.
func (a *Approvals) appendAudit(ctx context.Context, event repository.AuditEvent) {
	if event.At.IsZero() {
		event.At = a.clock()
	}
	if _, err := a.audit.Append(ctx, event); err != nil {
		a.log.Error().Err(err).
			Str("action", event.Action).
			Str("subjectId", event.SubjectID).
			Msg("could not append audit event")
	}
}
