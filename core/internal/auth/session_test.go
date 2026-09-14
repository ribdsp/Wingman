package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestNewSessionToken_returnsAPlaintextAndADifferentStoredForm(t *testing.T) {
	// Act
	plaintext, stored, err := NewSessionToken()

	// Assert
	if err != nil {
		t.Fatalf("minting failed: %v", err)
	}
	if plaintext == "" || stored == "" {
		t.Fatal("one of the two halves came back empty")
	}
	// The point of the pair: what the database holds must not be usable as a token.
	if plaintext == stored {
		t.Error("the stored form equals the plaintext; a database dump would be a set of live sessions")
	}
	if strings.Contains(stored, plaintext) {
		t.Error("the stored form contains the plaintext")
	}
}

func TestNewSessionToken_isRecognisableAsOne(t *testing.T) {
	// Act
	plaintext, _, err := NewSessionToken()
	if err != nil {
		t.Fatalf("minting failed: %v", err)
	}

	// Assert
	// The prefix is what makes a leaked string in a log or a bug report identifiable
	// as a live credential, so it can be revoked rather than wondered about.
	if !strings.HasPrefix(plaintext, tokenPrefix) {
		t.Errorf("token %q does not start with %q", plaintext, tokenPrefix)
	}

	body := strings.TrimPrefix(plaintext, tokenPrefix)
	decoded, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("token body is not URL-safe unpadded base64: %v", err)
	}
	if len(decoded) != tokenBytes {
		t.Errorf("token carries %d bytes of randomness; want %d", len(decoded), tokenBytes)
	}
	// URL-safe and unpadded, so the token survives a header, a query string and a
	// mobile deep link without re-encoding.
	if strings.ContainsAny(body, "+/=") {
		t.Errorf("token body %q contains characters that need escaping in a URL", body)
	}
}

func TestNewSessionToken_neverRepeats(t *testing.T) {
	// Arrange
	const mints = 200
	seen := make(map[string]struct{}, mints)

	// Act & Assert
	for i := 0; i < mints; i++ {
		plaintext, stored, err := NewSessionToken()
		if err != nil {
			t.Fatalf("minting failed on attempt %d: %v", i, err)
		}
		if _, dup := seen[plaintext]; dup {
			t.Fatalf("token repeated after %d mints", i)
		}
		if _, dup := seen[stored]; dup {
			t.Fatalf("stored form repeated after %d mints", i)
		}
		seen[plaintext] = struct{}{}
		seen[stored] = struct{}{}
	}
}

func TestHashToken_isStableAndDistinguishing(t *testing.T) {
	// Arrange
	first := "wgm_" + strings.Repeat("a", 43)
	second := "wgm_" + strings.Repeat("b", 43)

	// Act & Assert
	// Stable, because the hash is recomputed on every authenticated request and has
	// to land on the row written at sign-in.
	if HashToken(first) != HashToken(first) {
		t.Error("hashing the same token twice gave two answers")
	}
	if HashToken(first) == HashToken(second) {
		t.Error("two different tokens hashed to the same value")
	}
	// Hex SHA-256: 64 characters, and nothing of the input left in it.
	if got := len(HashToken(first)); got != 64 {
		t.Errorf("stored form is %d characters; want 64 hex characters of SHA-256", got)
	}
	if strings.Contains(HashToken(first), "wgm_") {
		t.Error("the stored form still carries the token prefix")
	}
}

func TestParseSessionToken_acceptsAMintedTokenAndAgreesWithTheStoredForm(t *testing.T) {
	// Arrange
	plaintext, stored, err := NewSessionToken()
	if err != nil {
		t.Fatalf("minting failed: %v", err)
	}

	// Act
	lookup, err := ParseSessionToken(plaintext)

	// Assert
	if err != nil {
		t.Fatalf("a freshly minted token did not parse: %v", err)
	}
	// If these two ever disagree, every session lookup misses and nobody can sign in.
	if lookup != stored {
		t.Errorf("parse gave %q; NewSessionToken stored %q", lookup, stored)
	}
}

func TestParseSessionToken_toleratesSurroundingWhitespace(t *testing.T) {
	// Arrange
	plaintext, stored, err := NewSessionToken()
	if err != nil {
		t.Fatalf("minting failed: %v", err)
	}

	// Act
	// A token pasted into a client, or carried in a header a proxy padded, arrives
	// with whitespace around it. That is not a forged credential.
	lookup, err := ParseSessionToken("  " + plaintext + "\n")

	// Assert
	if err != nil {
		t.Fatalf("a padded token was rejected: %v", err)
	}
	if lookup != stored {
		t.Errorf("parse gave %q; want %q", lookup, stored)
	}
}

func TestParseSessionToken_rejectsAnythingNotShapedLikeOne(t *testing.T) {
	// Arrange
	body := strings.Repeat("a", 43) // 43 base64 characters decode to 32 bytes
	cases := map[string]string{
		"empty":            "",
		"prefix only":      tokenPrefix,
		"no prefix":        body,
		"wrong prefix":     "sess_" + body,
		"bearer header":    "Bearer " + tokenPrefix + body,
		"not base64":       tokenPrefix + strings.Repeat("!", 43),
		"padded base64":    tokenPrefix + base64.StdEncoding.EncodeToString(make([]byte, tokenBytes)),
		"too short":        tokenPrefix + strings.Repeat("a", 22),
		"too long":         tokenPrefix + strings.Repeat("a", 86),
		"an api key":       "wingman-operator-key-0123456789",
		"an argon2 hash":   "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$ZGlnZXN0",
		"prefix in middle": body + tokenPrefix + body,
	}

	// Act & Assert
	for name, presented := range cases {
		t.Run(name, func(t *testing.T) {
			stored, err := ParseSessionToken(presented)
			if stored != "" {
				t.Errorf("a malformed token produced a lookup key %q", stored)
			}
			if !errors.Is(err, ErrMalformedToken) {
				t.Errorf("error = %v; want ErrMalformedToken", err)
			}
		})
	}
}

// The shape check saves a database round trip, and that is all it is allowed to
// decide. Whether a well-formed token belongs to anybody is the repository's answer.
func TestParseSessionToken_wellFormedButUnknownTokenStillParses(t *testing.T) {
	// Arrange
	invented := tokenPrefix + base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes))

	// Act
	stored, err := ParseSessionToken(invented)

	// Assert
	if err != nil {
		t.Fatalf("a well-formed token was rejected on shape: %v", err)
	}
	if stored != HashToken(invented) {
		t.Error("parse did not return the stored form of the presented token")
	}
}
