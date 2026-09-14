package auth

import (
	"crypto/rand"
	"fmt"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// Link code generation. The shape — the prefix, the length and the alphabet — is
// defined once in internal/domain, because the same three facts are needed to read a
// code back off an inbound message.
const (
	// linkCodeAlphabetSize is how many characters a position can hold.
	linkCodeAlphabetSize = len(domain.LinkCodeAlphabet)
	// linkCodeCeiling is the largest multiple of the alphabet size that fits in a
	// byte. A random byte at or above it is thrown away rather than folded in with a
	// modulo: 256 is not a multiple of 30, so `b % 30` on its own would make the
	// first sixteen letters of the alphabet likelier than the last fourteen. The
	// bias is small, and it is also free to avoid — and the whole security argument
	// for a ten-character credential is that all 49 bits are really there.
	linkCodeCeiling = 256 - 256%linkCodeAlphabetSize
)

// NewLinkCode mints a channel link code, returning the value to show the person and
// the value to store.
//
// Two values for the same reason NewSessionToken returns two: the database holds only
// the hash, so a dump of the table is a list of codes nobody can use. SHA-256 is right
// here for the same reason it is right there — the code is uniform random, so the hash
// exists to make a stolen row useless rather than to slow down guessing.
//
// The plaintext is returned canonical (WGM-ABCDEFGHJK), not grouped for display.
// Formatting is domain.FormatLinkCode's job and happens at the edge, so the value that
// gets hashed and the value that gets looked up are produced by the same code path.
func NewLinkCode() (plaintext, stored string, err error) {
	body := make([]byte, 0, domain.LinkCodeBodyLength)
	// One read per pass, sized to what is still missing. With a 240/256 acceptance
	// rate a second pass is unlikely and a third is rare, so this loop is bounded in
	// practice by the random source rather than by an iteration count.
	for len(body) < domain.LinkCodeBodyLength {
		buf := make([]byte, domain.LinkCodeBodyLength-len(body))
		if _, err := rand.Read(buf); err != nil {
			return "", "", fmt.Errorf("read random link code: %w", err)
		}
		for _, b := range buf {
			draw := int(b)
			if draw >= linkCodeCeiling {
				continue
			}
			body = append(body, domain.LinkCodeAlphabet[draw%linkCodeAlphabetSize])
		}
	}

	plaintext = domain.LinkCodePrefix + string(body)
	return plaintext, HashToken(plaintext), nil
}
