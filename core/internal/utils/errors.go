package utils

// API error codes. These are the same nine strings the goal engine publishes, and
// they are part of the public contract across both services — a client that talks
// to Wingman should not have to learn a second vocabulary halfway through.
//
// Two of the nine mean something slightly different here than next door, and the
// difference is worth stating rather than discovering:
//
//	UNKNOWN_METRIC   only reaches a caller of core when it asks for a model or a
//	                 tool that the operator did not declare. It is the same shape
//	                 of mistake — naming something the operator's YAML does not
//	                 define — so it reuses the code rather than inventing a tenth.
//	ALREADY_RESOLVED is every 409 core answers: the state the caller is asking to
//	                 create is already recorded. A second attempt to cancel a run
//	                 that has already stopped — whoever stopped it first is who
//	                 decided — and a second account on one email address are the
//	                 same shape of answer, and neither is fixed by retrying.
//
// A halted run is not an error. A run refused because the kill switch is engaged
// comes back as a recorded run with StopHalted as its reason, exactly as a spend
// refused under the switch comes back as a recorded denial.
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
