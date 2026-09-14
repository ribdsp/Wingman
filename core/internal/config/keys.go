package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Bounds on a configured credential.
const (
	// minAPIKeyLength rejects keys short enough to brute-force.
	minAPIKeyLength = 24
	// maxPrincipalNameLength bounds the name a key is recorded under.
	maxPrincipalNameLength = 64
)

// Role separates the three kinds of principal core can be talking to.
//
// A role is a property of *where the credential came from*, fixed before the first
// request arrives, and never something a caller can assert about itself. That is the
// whole mechanism: there is no request field, header or body key that raises a role,
// so there is nothing to forge.
type Role string

const (
	// RoleOperator is whoever runs this instance. Only an operator may cancel
	// another user's run, read the whole audit log, or change a tool grant.
	RoleOperator Role = "operator"
	// RoleBot is a service — in practice the goal engine's trigger bridge. A bot
	// may file tasks and report; it may not read a user's transcript.
	RoleBot Role = "bot"
	// RoleUser is a human on the web, desktop or mobile client. It arrives only
	// from a session token in the database, never from an environment variable, and
	// an environment key can never become one.
	RoleUser Role = "user"
)

// APIKey is one accepted inbound machine credential together with the principal
// name every action taken with it is recorded under.
//
// A name is required from the moment a key is configured, for the same reason the
// goal engine requires one: an audit trail that can only say "some valid key" does
// not answer the question an audit trail exists to answer.
type APIKey struct {
	// Name is what appears in the audit log.
	Name string
	// Secret is the credential itself. Never logged, never rendered into an error.
	Secret string
	// Role is fixed by which environment variable listed the key.
	Role Role
}

// parseAPIKeys reads one comma-separated key list, where each entry is either
// "name:secret" or a bare secret.
//
// Everything before the first colon is the name, everything after it the secret, so
// a secret must not contain a colon. An unnamed key is labelled with a short hash of
// itself, which ties an audit entry to exactly one credential without recording the
// credential.
//
// seenName and seenSecret are shared across every list, so an operator key and a bot
// key can never be the same secret or answer to the same name. That is stricter than
// ordinary uniqueness on purpose: the lists carry different privileges, and a
// credential appearing in both would leave a request's privilege depending on which
// list happened to be searched first.
func parseAPIKeys(envName, raw string, role Role, seenName, seenSecret map[string]string, fail func(string, ...any)) []APIKey {
	entries := splitAndTrim(raw)
	keys := make([]APIKey, 0, len(entries))

	for i, entry := range entries {
		where := fmt.Sprintf("%s[%d]", envName, i)
		name, secret := "", entry
		if idx := strings.Index(entry, ":"); idx >= 0 {
			name = strings.TrimSpace(entry[:idx])
			secret = strings.TrimSpace(entry[idx+1:])
		}

		// The secret is described, never echoed. An error message quoting the key
		// back puts it in whatever collected the boot log.
		if len(secret) < minAPIKeyLength {
			fail("%s has a key shorter than %d characters", where, minAPIKeyLength)
			continue
		}
		if name == "" {
			name = fingerprintName(secret)
		} else if len(name) > maxPrincipalNameLength {
			fail("%s has a name longer than %d characters", where, maxPrincipalNameLength)
			continue
		} else if strings.ContainsAny(name, " \t\n") {
			fail("%s has a name containing whitespace: %q", where, name)
			continue
		}

		// The key is checked before the name: a key listed twice also collides on
		// its fingerprint name, and "you listed the same key twice" is the more
		// useful of the two things to be told.
		if prev, dup := seenSecret[secret]; dup {
			fail("%s repeats the key from %s", where, prev)
			continue
		}
		if prev, dup := seenName[name]; dup {
			fail("%s reuses the name from %s: %q", where, prev, name)
			continue
		}

		seenName[name] = where
		seenSecret[secret] = where
		keys = append(keys, APIKey{Name: name, Secret: secret, Role: role})
	}
	return keys
}

// fingerprintName labels an unnamed key with eight hex characters of its SHA-256.
// That says which key acted without helping anyone reconstruct it.
func fingerprintName(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "key-" + hex.EncodeToString(sum[:4])
}
