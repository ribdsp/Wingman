package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Session token shape.
const (
	// tokenBytes is 32 — 256 bits from the system random source. That is enough
	// that guessing is not a threat model, which is what lets the stored form be a
	// plain SHA-256 (see HashToken).
	tokenBytes = 32
	// tokenPrefix marks a Wingman session token in a log or a bug report, so a
	// leaked string is recognisable as a live credential and can be revoked.
	tokenPrefix = "wgm_"
)

// ErrMalformedToken means the presented string is not shaped like a session token.
//
// It exists so a handler can reject an obviously wrong token without a database
// round trip — but the handler must still answer the caller with the same message it
// gives a valid-but-unknown token, or the difference tells an attacker which of the
// two they produced.
var ErrMalformedToken = errors.New("not a session token")

// NewSessionToken mints a session token, returning the value to give the client and
// the value to store.
//
// The two are different, and that is the whole point: the database holds only the
// hash, so a dump of the sessions table is a list of expired opportunities rather
// than a set of live logins. The plaintext exists for exactly one HTTP response and
// is never written down.
func NewSessionToken() (plaintext, stored string, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("read random token: %w", err)
	}
	// URL-safe and unpadded so the token survives a header, a query string and a
	// mobile deep link without re-encoding.
	plaintext = tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the stored form of a session token.
//
// SHA-256, not argon2id, and the difference is deliberate. A password is short,
// human-chosen and therefore guessable, so hashing it has to be expensive. A session
// token is 256 uniform random bits, so there is nothing to guess — the hash exists
// only to make a stolen database useless, and it is computed on every authenticated
// request, where 64 MiB of argon2 per call would be a denial of service with a
// security rationale attached.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ParseSessionToken checks the shape of a presented token and returns its stored
// form for lookup.
//
// It validates only what is free to validate. Whether the session exists, belongs to
// anybody, or has expired is the repository's answer, not this function's.
func ParseSessionToken(presented string) (stored string, err error) {
	trimmed := strings.TrimSpace(presented)
	if !strings.HasPrefix(trimmed, tokenPrefix) {
		return "", ErrMalformedToken
	}

	body := strings.TrimPrefix(trimmed, tokenPrefix)
	decoded, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(decoded) != tokenBytes {
		return "", ErrMalformedToken
	}
	return HashToken(trimmed), nil
}
