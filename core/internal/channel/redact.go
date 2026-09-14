package channel

import "strings"

// redact wraps err so that its text cannot carry a credential into a log line.
//
// This is not defensive tidiness. Telegram puts the bot token in the URL *path*, so any
// transport failure against it — a DNS error, a timeout, a 401 with the request echoed —
// produces an error string containing the token. By the time zerolog sees that string it
// is already too late, and an operator pasting a log into an issue has published a
// credential that can read every message the bot receives.
//
// The wrapper keeps Unwrap, so errors.Is still works on what it holds: a Run that returns
// a redacted context.Canceled is still recognised as a cancellation by the hub.
func redact(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	keep := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		// A short or empty value would replace everything or nothing. Empty happens
		// whenever an optional token is unset, and there is nothing to hide in it.
		if len(secret) >= minRedactableSecret {
			keep = append(keep, secret)
		}
	}
	if len(keep) == 0 {
		return err
	}
	return redactedError{err: err, secrets: keep}
}

// minRedactableSecret is the shortest string worth substituting out of an error.
//
// Below it the value is not a credential any of these platforms issues, and replacing a
// two-character string would corrupt the message it appears in.
const minRedactableSecret = 8

type redactedError struct {
	err     error
	secrets []string
}

func (e redactedError) Error() string {
	return scrub(e.err.Error(), e.secrets...)
}

func (e redactedError) Unwrap() error { return e.err }

// scrub substitutes secrets out of a string that is about to be logged.
//
// Separate from redact because an SDK's own logger hands over a formatted line rather than
// an error, and that line is the other way a token reaches a log file.
func scrub(text string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) < minRedactableSecret {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return text
}
