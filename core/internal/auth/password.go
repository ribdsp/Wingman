// Package auth holds Wingman core's credential primitives: password hashing and
// session tokens.
//
// It is deliberately small and free of storage. Hashing and token generation are
// pure functions over their inputs plus the system's random source, so they can be
// tested exhaustively, and the decision of *what* to store lives with the repository
// that stores it.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters.
//
// These are the current OWASP-recommended figures for argon2id: 64 MiB of memory,
// three passes, four lanes. The memory cost is the point — it is what makes a GPU
// farm a poor investment against this hash, in a way that iteration count alone
// never achieves.
//
// They are constants rather than configuration on purpose. An operator tuning
// password hashing downward to save memory on a small VPS is making a security
// decision while thinking about a performance one, and the encoded hash records the
// parameters anyway, so raising them later is a migration rather than a rewrite.
const (
	argonMemory  uint32 = 64 * 1024
	argonTime    uint32 = 3
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
	// argonVersion is recorded in the encoded string so a hash produced by a future
	// version is rejected loudly rather than silently mis-verified.
	argonVersion = argon2.Version
)

// Password bounds. Both matter, for opposite reasons.
const (
	// MinPasswordLength is twelve characters. This is a self-hosted service whose
	// accounts can start sandboxed processes and spend an API budget, so the
	// account is worth more than a forum login.
	MinPasswordLength = 12
	// MaxPasswordLength bounds what will be hashed. argon2id has no silent
	// truncation to worry about, but hashing an unbounded string is 64 MiB of work
	// per request that an unauthenticated caller gets to request.
	MaxPasswordLength = 256
)

// ErrInvalidHash means the stored string is not a hash this package produced.
//
// It is returned rather than treated as a failed password check because the two need
// different responses: a wrong password is the user's problem, and an unreadable
// hash is the operator's.
var ErrInvalidHash = errors.New("stored credential is not a valid argon2id hash")

// ValidatePassword reports whether a password is acceptable to store.
//
// Length is the only rule. Composition rules — a digit, a symbol, mixed case — push
// people toward predictable substitutions and a sticky note, and they are not what
// stands between an attacker and a 64 MiB hash.
func ValidatePassword(password string) error {
	if n := utf8.RuneCountInString(password); n < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	} else if n > MaxPasswordLength {
		return fmt.Errorf("password must be at most %d characters", MaxPasswordLength)
	}
	return nil
}

// HashPassword returns an encoded argon2id hash of password.
//
// The encoding is the standard PHC string, carrying the version, the parameters and
// the salt alongside the digest:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<digest>
//
// Storing the parameters with the hash is what makes them changeable: raising the
// memory cost next year still verifies every password hashed this year.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		// A failing random source is not something to work around. Every credential
		// minted after this point would share whatever entropy is left.
		return "", fmt.Errorf("read random salt: %w", err)
	}

	digest := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

// VerifyPassword reports whether password matches the encoded hash.
//
// It recomputes with the parameters recorded in the hash rather than the current
// constants, so a hash written under older settings still verifies.
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, digest, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	candidate := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(digest)))
	// Constant time: a byte-by-byte comparison that returns early leaks how much of
	// the digest matched, and a digest is guessable one byte at a time if you can
	// measure that.
	return subtle.ConstantTimeCompare(candidate, digest) == 1, nil
}

// NeedsRehash reports whether a stored hash was produced with weaker parameters
// than the current ones.
//
// A caller that checks this after a successful sign-in can upgrade the stored hash
// while it still has the plaintext — the only moment it ever does.
func NeedsRehash(encoded string) bool {
	params, _, _, err := decodeHash(encoded)
	if err != nil {
		// An unreadable hash cannot be verified either, so the caller has a bigger
		// problem than rehashing; say no and let VerifyPassword report it.
		return false
	}
	return params.memory < argonMemory || params.time < argonTime || params.threads < argonThreads
}

// UnmatchableHash returns a real argon2id hash that no password matches.
//
// It exists so sign-in costs the same whether the address has an account or not.
// Verifying against an empty string returns ErrInvalidHash in a microsecond, and the
// gap between that and the hundred milliseconds argon2id costs is an
// account-enumeration oracle measurable over the internet — ask about a thousand
// addresses, and the slow answers are the real ones.
//
// The secret is generated at first use rather than compiled in, because a hash sitting
// in public source is a hash an attacker can verify against to recognise this code path
// for what it is. Computed once and reused: the point is the cost of the comparison, not
// the cost of producing the thing compared against.
//
// If the random source fails it returns "", which VerifyPassword rejects as an invalid
// hash — quickly, and still as a refusal. A broken random source is a worse problem than
// a timing oracle, and it is one HashPassword is already unable to work around.
var UnmatchableHash = sync.OnceValue(func() string {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return ""
	}
	hash, err := HashPassword(hex.EncodeToString(secret))
	if err != nil {
		return ""
	}
	return hash
})

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

// decodeHash parses a PHC-encoded argon2id string.
//
// Every field is checked. A hash string is attacker-influenced in exactly one
// scenario — a database an attacker can already write to — but parsing it loosely
// would turn that into "supply parameters weak enough to forge a match", so the
// strictness costs nothing and closes that.
func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// A leading "$" makes the first field empty: "", "argon2id", "v=19", params,
	// salt, digest.
	if len(parts) != 6 || parts[0] != "" {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		// argon2i and argon2d are different algorithms, not variants to accept.
		return argonParams{}, nil, nil, fmt.Errorf("%w: algorithm is %q, not argon2id", ErrInvalidHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if version != argonVersion {
		return argonParams{}, nil, nil, fmt.Errorf("%w: version %d is not supported", ErrInvalidHash, version)
	}

	var params argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.time, &params.threads); err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if params.memory == 0 || params.time == 0 || params.threads == 0 {
		// argon2.IDKey panics on a zero parameter, and a panic in a sign-in handler
		// is a denial of service reachable from a login form.
		return argonParams{}, nil, nil, fmt.Errorf("%w: parameters are out of range", ErrInvalidHash)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(digest) == 0 {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	return params, salt, digest, nil
}
