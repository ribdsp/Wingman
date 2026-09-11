package service

import (
	"context"
	"fmt"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// maxAuditWindow bounds how much history one query may sweep. Without it a
// caller can ask the engine to scan the whole log, which is the table that grows
// forever.
const maxAuditWindow = 366 * 24 * time.Hour

// AuditLog is the read side of the business audit log.
//
// There is no write method on purpose. Entries are appended by the workflows that
// caused them, as part of doing the thing they record; an endpoint that could add
// an entry could add a false one.
type AuditLog struct {
	reader AuditReader
	clock  Clock
}

// AuditLogDeps is everything the audit reader needs.
type AuditLogDeps struct {
	Reader AuditReader
	Clock  Clock
}

// NewAuditLog validates its wiring and returns a ready reader.
func NewAuditLog(deps AuditLogDeps) (*AuditLog, error) {
	if deps.Reader == nil {
		return nil, fmt.Errorf("audit: missing dependencies: [Reader]")
	}
	a := &AuditLog{reader: deps.Reader, clock: deps.Clock}
	if a.clock == nil {
		a.clock = time.Now
	}
	return a, nil
}

// List reads a page of audit entries.
func (a *AuditLog) List(ctx context.Context, filter repository.AuditFilter) ([]repository.AuditEvent, int, error) {
	if filter.ActorType != "" {
		switch filter.ActorType {
		case repository.ActorSystem, repository.ActorUser, repository.ActorBot:
		default:
			return nil, 0, fieldError("actorType", fmt.Sprintf("%q is not an actor type", filter.ActorType))
		}
	}
	if !filter.Since.IsZero() && !filter.Until.IsZero() && filter.Until.Before(filter.Since) {
		return nil, 0, fieldError("until", "must be at or after since")
	}
	if filter.Since.IsZero() {
		filter.Since = a.clock().Add(-maxAuditWindow)
	}
	filter.Limit, filter.Offset = clampPage(filter.Limit, filter.Offset)

	events, total, err := a.reader.List(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: list: %w", err)
	}
	return events, total, nil
}
