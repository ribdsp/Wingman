package service

import "errors"

// Sentinel errors the handler layer maps to HTTP statuses. They exist so a
// handler never has to inspect an error string, and so the repository's own
// errors stop at this boundary rather than leaking into the API.
var (
	// ErrValidation means the request could not be acted on as given.
	ErrValidation = errors.New("service: invalid request")

	// ErrNotFound means the named record does not exist.
	ErrNotFound = errors.New("service: not found")

	// ErrAlreadyResolved means a human already decided this approval. The caller's
	// correct response is to read the existing decision, not to retry.
	ErrAlreadyResolved = errors.New("service: approval already resolved")

	// ErrForbidden means the caller is authenticated but is not allowed to do
	// this. It exists separately from ErrValidation because the request is
	// well-formed: retrying it with better input will not help.
	ErrForbidden = errors.New("service: not permitted for this actor")
)

// There is deliberately no kill-switch error here. A halted engine is not a
// failed request: a spend asked for under the switch is recorded as denied with
// the switch named as the reason, and a monitor tick reports itself halted. Both
// leave a trace of the attempt, which an HTTP error would not, and neither invites
// the retry loop a 503 would.
