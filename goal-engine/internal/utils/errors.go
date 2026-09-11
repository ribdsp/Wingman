package utils

// API error codes. These are part of the public contract, so they are stable
// strings rather than generated values.
//
// Every code here is one a caller can actually receive. A published code no path
// emits is a branch a client writes and never exercises — a halted engine, for
// one, is not an error: a spend requested under the kill switch comes back as a
// recorded denial with the switch named as the reason.
const (
	ErrCodeValidation      = "VALIDATION_ERROR"
	ErrCodeUnauthorized    = "UNAUTHORIZED"
	ErrCodeForbidden       = "FORBIDDEN"
	ErrCodeNotFound        = "NOT_FOUND"
	ErrCodeRateLimited     = "RATE_LIMITED"
	ErrCodeInternal        = "INTERNAL_ERROR"
	ErrCodeUnavailable     = "SERVICE_UNAVAILABLE"
	ErrCodeUnknownMetric   = "UNKNOWN_METRIC"
	ErrCodeAlreadyResolved = "ALREADY_RESOLVED"
)
