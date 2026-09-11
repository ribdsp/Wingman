package service

import (
	"errors"
	"fmt"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
)

// Page limits for list endpoints. A handler must not be able to pass an
// unbounded limit through to a query, so the clamp lives here rather than in
// whatever validation the transport happens to have.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// clampPage returns a usable limit and offset for a list query.
func clampPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// fieldError builds a validation error that names the offending field, so the API
// can answer with per-field messages while callers still match on ErrValidation.
//
// Both errors are wrapped: callers switch on the ErrValidation sentinel, and the
// handler layer digs the field out with ValidationFields.
func fieldError(field, message string) error {
	return fmt.Errorf("%w: %w", ErrValidation, domain.ValidationError{Field: field, Message: message})
}

// ValidationFields extracts the per-field messages from a validation error, or
// nil if the error carries none. It lets a handler render field-level messages
// without knowing how the service builds its errors.
func ValidationFields(err error) map[string]string {
	var errs domain.ValidationErrors
	if errors.As(err, &errs) {
		return errs.Fields()
	}
	var one domain.ValidationError
	if errors.As(err, &one) {
		return map[string]string{one.Field: one.Message}
	}
	return nil
}
