package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// ActorType mirrors the audit_actor_type enum.
type ActorType string

const (
	// ActorSystem is the goal engine acting on its own timer.
	ActorSystem ActorType = "system"
	// ActorUser is a human operator.
	ActorUser ActorType = "user"
	// ActorBot is an agent in Wingman core.
	ActorBot ActorType = "bot"
)

// AuditEvent is one entry in the business audit log.
//
// This log is deliberately separate from Wingman core's internal logs: when an
// autonomous system spends money or changes a campaign, the record of why has to
// survive independently of the tool that did it.
type AuditEvent struct {
	ID          int64
	At          time.Time
	ActorType   ActorType
	ActorID     string
	Action      string
	SubjectType string
	SubjectID   string
	Outcome     string
	// Detail is JSON. It should carry the reasoning, not a secret.
	Detail    string
	RequestID string
}

// AuditFilter narrows an audit listing.
type AuditFilter struct {
	ActorType   ActorType
	Action      string
	SubjectType string
	SubjectID   string
	Since       time.Time
	Until       time.Time
	Limit       int
	Offset      int
}

// AuditRepository appends to and reads the business audit log.
type AuditRepository struct {
	db *sqlx.DB
}

// NewAuditRepository builds a repository over the given pool.
func NewAuditRepository(db *sqlx.DB) *AuditRepository {
	return &AuditRepository{db: db}
}

const auditColumns = `id, at, actor_type, actor_id, action, subject_type,
	subject_id, outcome, detail::text AS detail, request_id`

type auditRow struct {
	ID          int64     `db:"id"`
	At          time.Time `db:"at"`
	ActorType   string    `db:"actor_type"`
	ActorID     string    `db:"actor_id"`
	Action      string    `db:"action"`
	SubjectType string    `db:"subject_type"`
	SubjectID   string    `db:"subject_id"`
	Outcome     string    `db:"outcome"`
	Detail      string    `db:"detail"`
	RequestID   string    `db:"request_id"`
}

func (r auditRow) toEvent() AuditEvent {
	return AuditEvent{
		ID:          r.ID,
		At:          r.At,
		ActorType:   ActorType(r.ActorType),
		ActorID:     r.ActorID,
		Action:      r.Action,
		SubjectType: r.SubjectType,
		SubjectID:   r.SubjectID,
		Outcome:     r.Outcome,
		Detail:      r.Detail,
		RequestID:   r.RequestID,
	}
}

// Append writes one audit event. The log is append-only: there is no update or
// delete, because an audit trail an operator can edit is not an audit trail.
func (r *AuditRepository) Append(ctx context.Context, event AuditEvent) (AuditEvent, error) {
	const query = `
		INSERT INTO audit_events (
			at, actor_type, actor_id, action, subject_type, subject_id, outcome,
			detail, request_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)
		RETURNING ` + auditColumns

	at := event.At
	if at.IsZero() {
		at = time.Now()
	}
	actorType := event.ActorType
	if actorType == "" {
		actorType = ActorSystem
	}

	var row auditRow
	err := r.db.QueryRowxContext(ctx, query,
		at, string(actorType), event.ActorID, event.Action, event.SubjectType,
		event.SubjectID, event.Outcome, jsonOrEmpty(event.Detail), event.RequestID,
	).StructScan(&row)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("append audit event %s: %w", event.Action, classify(err))
	}
	return row.toEvent(), nil
}

// List returns a page of audit events plus the total number of matches.
func (r *AuditRepository) List(ctx context.Context, filter AuditFilter) ([]AuditEvent, int, error) {
	where := []string{"1 = 1"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if filter.ActorType != "" {
		add("actor_type = $%d", string(filter.ActorType))
	}
	if filter.Action != "" {
		add("action = $%d", filter.Action)
	}
	if filter.SubjectType != "" {
		add("subject_type = $%d", filter.SubjectType)
	}
	if filter.SubjectID != "" {
		add("subject_id = $%d", filter.SubjectID)
	}
	if !filter.Since.IsZero() {
		add("at >= $%d", filter.Since)
	}
	if !filter.Until.IsZero() {
		add("at <= $%d", filter.Until)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit events: %w", classify(err))
	}

	limit, offset := normalisePage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	query := fmt.Sprintf(
		`SELECT %s FROM audit_events WHERE %s ORDER BY at DESC, id DESC LIMIT $%d OFFSET $%d`,
		auditColumns, clause, len(args)-1, len(args),
	)

	rows := []auditRow{}
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, 0, fmt.Errorf("list audit events: %w", classify(err))
	}

	events := make([]AuditEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, row.toEvent())
	}
	return events, total, nil
}
