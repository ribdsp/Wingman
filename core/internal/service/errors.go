package service

import "errors"

// The sentinels a handler maps to status codes. There are five, and they line up with
// five of the nine published error codes in docs/api.md — core needs no tenth code, and
// adding one would be an API change rather than a detail.
//
// Everything else a service returns is a real failure: it becomes a 500, and its
// message stays in the log rather than the response body.
var (
	// ErrValidation is a request the caller can fix. 400.
	ErrValidation = errors.New("request is not valid")

	// ErrNotFound is a row that does not exist, or one that exists and belongs to
	// somebody else. The two are the same answer on purpose: telling a caller that a
	// chat id is real but not theirs confirms the id, and an id that can be confirmed
	// can be enumerated. 404.
	ErrNotFound = errors.New("not found")

	// ErrConflict is a request that collided with the state already recorded — a second
	// account on one email address, an idempotency key that named a different task.
	// 409.
	//
	// A dispatch retry is not this. The same key arriving twice is the healthy outcome
	// of a retry and answers with the original task, not with a conflict.
	ErrConflict = errors.New("conflicts with what is already recorded")

	// ErrUnauthenticated is a credential that did not check out. 401.
	//
	// It never says which half was wrong. "No account with that address" and "wrong
	// password" are two different sentences, and serving both turns a sign-in form into
	// a way to test whether somebody has an account here.
	ErrUnauthenticated = errors.New("those credentials are not valid")

	// ErrForbidden is a caller who is authenticated and still may not do this. 403.
	//
	// It is what the second half of an operator-only check returns. The route middleware
	// refuses these before the service is reached; the service refuses them again,
	// because a route added later without the middleware would otherwise be an
	// escalation rather than a mistake.
	ErrForbidden = errors.New("that operation is not permitted for this caller")
)

// There is deliberately no error here for registration being closed, and no error for
// a run that stopped.
//
// Closed registration is ErrForbidden: it is a caller who may not do this, and giving
// it its own sentinel would let a handler render a message that tells a stranger the
// difference between "this box does not take sign-ups" and "this box does, but not from
// you". A run that stopped is not an error at all — it is an outcome, with a reason
// from domain.StopReason, and the caller gets a 200 describing it.
