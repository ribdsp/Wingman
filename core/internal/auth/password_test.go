package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

const goodPassword = "correct horse battery staple"

// hashWith builds an encoded hash under chosen parameters, so a test can produce
// the "hashed under older, weaker settings" case that a real deployment eventually
// has in its database.
func hashWith(password string, memory, time uint32, threads uint8) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest := argon2.IDKey([]byte(password), salt, time, memory, threads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, memory, time, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

func TestHashPassword_thenVerify_acceptsTheSamePassword(t *testing.T) {
	// Arrange
	encoded, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}

	// Act
	ok, err := VerifyPassword(encoded, goodPassword)

	// Assert
	if err != nil {
		t.Fatalf("verifying failed: %v", err)
	}
	if !ok {
		t.Error("the password that produced the hash did not verify against it")
	}
}

func TestVerifyPassword_rejectsAWrongPassword(t *testing.T) {
	// Arrange
	encoded, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}

	// Act
	ok, err := VerifyPassword(encoded, "correct horse battery stapl")

	// Assert
	if err != nil {
		t.Fatalf("verifying a wrong password returned an error rather than a false: %v", err)
	}
	if ok {
		t.Error("a wrong password verified")
	}
}

func TestHashPassword_saltsEachHashSeparately(t *testing.T) {
	// Arrange & Act
	first, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}
	second, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}

	// Assert
	// Two accounts sharing a password must not share a stored hash, or a dump tells
	// an attacker which accounts to attack once and then reuse.
	if first == second {
		t.Error("the same password hashed to the same string twice; the salt is not random")
	}
}

func TestHashPassword_recordsItsParameters(t *testing.T) {
	// Act
	encoded, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}

	// Assert
	// The parameters travel with the hash, which is what makes raising them later a
	// migration rather than a rewrite.
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("encoded hash = %q; want the current parameters recorded in it", encoded)
	}
	if parts := strings.Split(encoded, "$"); len(parts) != 6 {
		t.Errorf("encoded hash has %d fields; want 6", len(parts))
	}
}

func TestValidatePassword_boundsLengthAtBothEnds(t *testing.T) {
	// Arrange
	cases := map[string]struct {
		password string
		wantErr  bool
	}{
		"too short":      {strings.Repeat("a", MinPasswordLength-1), true},
		"at the minimum": {strings.Repeat("a", MinPasswordLength), false},
		"at the maximum": {strings.Repeat("a", MaxPasswordLength), false},
		"too long":       {strings.Repeat("a", MaxPasswordLength+1), true},
		"empty":          {"", true},
	}

	// Act & Assert
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidatePassword(tc.password)
			if tc.wantErr && err == nil {
				t.Errorf("a %d-character password was accepted", len(tc.password))
			}
			if !tc.wantErr && err != nil {
				t.Errorf("a %d-character password was rejected: %v", len(tc.password), err)
			}
		})
	}
}

// The bound counts characters, not bytes, so a passphrase in a language needing
// multi-byte runes is not held to a shorter length than an English one.
func TestValidatePassword_countsCharactersNotBytes(t *testing.T) {
	// Arrange
	password := strings.Repeat("あ", MinPasswordLength)

	// Act
	err := ValidatePassword(password)

	// Assert
	if err != nil {
		t.Errorf("a %d-character passphrase was rejected: %v", MinPasswordLength, err)
	}
}

func TestHashPassword_refusesAPasswordItWouldNotAccept(t *testing.T) {
	// Act
	_, err := HashPassword("short")

	// Assert
	// Hashing enforces the rule too. A caller that forgets to validate first must
	// not be the reason a five-character password ends up stored.
	if err == nil {
		t.Error("a too-short password was hashed")
	}
}

func TestVerifyPassword_malformedHashIsAnErrorNotAFailedCheck(t *testing.T) {
	// Arrange
	// A wrong password is the user's problem; an unreadable hash is the operator's,
	// and answering "wrong password" to the second sends them looking in the wrong
	// place entirely.
	cases := map[string]string{
		"empty":              "",
		"not a hash":         "hunter2",
		"too few fields":     "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA",
		"wrong algorithm":    "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$ZGlnZXN0",
		"bcrypt":             "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
		"unknown version":    "$argon2id$v=99$m=65536,t=3,p=4$c2FsdA$ZGlnZXN0",
		"unparseable params": "$argon2id$v=19$m=lots,t=3,p=4$c2FsdA$ZGlnZXN0",
		"bad base64 salt":    "$argon2id$v=19$m=65536,t=3,p=4$!!!!$ZGlnZXN0",
		"bad base64 digest":  "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$!!!!",
		"empty salt":         "$argon2id$v=19$m=65536,t=3,p=4$$ZGlnZXN0",
		"empty digest":       "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$",
		"no leading dollar":  "argon2id$v=19$m=65536,t=3,p=4$c2FsdA$ZGlnZXN0",
	}

	// Act & Assert
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyPassword(encoded, goodPassword)
			if ok {
				t.Fatal("a malformed hash verified")
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Errorf("error = %v; want it to wrap ErrInvalidHash so a caller can tell the two cases apart", err)
			}
		})
	}
}

// argon2.IDKey panics on a zero parameter, and a panic in a sign-in handler is a
// denial of service reachable from a login form.
func TestVerifyPassword_zeroParametersAreRejectedNotPassedToArgon(t *testing.T) {
	// Arrange
	cases := []string{
		"$argon2id$v=19$m=0,t=3,p=4$c2FsdA$ZGlnZXN0",
		"$argon2id$v=19$m=65536,t=0,p=4$c2FsdA$ZGlnZXN0",
		"$argon2id$v=19$m=65536,t=3,p=0$c2FsdA$ZGlnZXN0",
	}

	// Act & Assert
	for _, encoded := range cases {
		ok, err := VerifyPassword(encoded, goodPassword)
		if ok {
			t.Errorf("%q verified", encoded)
		}
		if !errors.Is(err, ErrInvalidHash) {
			t.Errorf("%q gave error %v; want ErrInvalidHash", encoded, err)
		}
	}
}

func TestNeedsRehash_saysYesOnlyForWeakerParameters(t *testing.T) {
	// Arrange
	current, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}
	weaker := "$argon2id$v=19$m=4096,t=1,p=1$c2FsdA$ZGlnZXN0"

	// Act & Assert
	if NeedsRehash(current) {
		t.Error("a hash written with the current parameters was flagged for rehashing")
	}
	if !NeedsRehash(weaker) {
		t.Error("a hash written with weaker parameters was not flagged")
	}
	// An unreadable hash cannot be verified either, so the caller has a bigger
	// problem than rehashing; VerifyPassword is where they find that out.
	if NeedsRehash("not a hash") {
		t.Error("an unreadable hash was flagged for rehashing rather than left to VerifyPassword")
	}
}

// A hash produced under older, weaker parameters must still verify — otherwise
// raising the cost locks every existing user out on the next deploy.
func TestVerifyPassword_usesTheParametersInTheHashNotTheCurrentOnes(t *testing.T) {
	// Arrange
	weak, err := hashWith(goodPassword, 8192, 1, 1)
	if err != nil {
		t.Fatalf("could not build the fixture: %v", err)
	}

	// Act
	ok, err := VerifyPassword(weak, goodPassword)

	// Assert
	if err != nil {
		t.Fatalf("verifying a weakly-hashed password failed: %v", err)
	}
	if !ok {
		t.Error("a password hashed under older parameters did not verify")
	}
	if !NeedsRehash(weak) {
		t.Error("the older hash was not flagged for rehashing")
	}
}
