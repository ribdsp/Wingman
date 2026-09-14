package auth

import (
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
)

func TestNewLinkCode_readsBackAsTheCodeItIs(t *testing.T) {
	// Arrange & Act
	plaintext, _, err := NewLinkCode()

	// Assert
	if err != nil {
		t.Fatalf("mint link code: %v", err)
	}
	// The generator and the reader are the two halves of one credential. If a minted
	// code does not normalise back to itself, every link attempt fails and the only
	// symptom is people reporting that the code "does not work".
	normalised, ok := domain.NormaliseLinkCode(plaintext)
	if !ok {
		t.Fatalf("minted %q, which the inbound path does not recognise as a code", plaintext)
	}
	if normalised != plaintext {
		t.Fatalf("normalised = %q, want the minted %q", normalised, plaintext)
	}
}

func TestNewLinkCode_survivesBeingGroupedForDisplayAndTypedBack(t *testing.T) {
	// Arrange
	plaintext, _, err := NewLinkCode()
	if err != nil {
		t.Fatalf("mint link code: %v", err)
	}

	// Act
	shown := domain.FormatLinkCode(plaintext)
	normalised, ok := domain.NormaliseLinkCode(shown)

	// Assert
	// What a person is shown is not what is stored, and the round trip through the
	// grouped form is the path every real code actually takes.
	if !ok {
		t.Fatalf("the displayed form %q does not read back as a code", shown)
	}
	if normalised != plaintext {
		t.Fatalf("normalised = %q, want %q", normalised, plaintext)
	}
}

func TestNewLinkCode_storesTheHashAndNotTheCode(t *testing.T) {
	// Arrange & Act
	plaintext, stored, err := NewLinkCode()
	if err != nil {
		t.Fatalf("mint link code: %v", err)
	}

	// Assert
	if stored == plaintext {
		t.Fatal("the stored form is the code itself")
	}
	if strings.Contains(stored, strings.TrimPrefix(plaintext, domain.LinkCodePrefix)) {
		t.Fatal("the stored form contains the code")
	}
	if stored != HashToken(plaintext) {
		t.Fatalf("stored form is not HashToken's output; a lookup would never match")
	}
}

func TestNewLinkCode_usesOnlyTheUnambiguousAlphabet(t *testing.T) {
	// Arrange & Act
	for i := 0; i < 200; i++ {
		plaintext, _, err := NewLinkCode()
		if err != nil {
			t.Fatalf("mint link code: %v", err)
		}

		// Assert
		body := strings.TrimPrefix(plaintext, domain.LinkCodePrefix)
		if len(body) != domain.LinkCodeBodyLength {
			t.Fatalf("body %q is %d characters, want %d", body, len(body), domain.LinkCodeBodyLength)
		}
		for _, r := range body {
			if !strings.ContainsRune(domain.LinkCodeAlphabet, r) {
				t.Fatalf("minted %q, which contains %q — outside the alphabet", plaintext, r)
			}
		}
	}
}

func TestNewLinkCode_reachesEveryCharacterOfTheAlphabet(t *testing.T) {
	// Arrange
	seen := map[rune]bool{}

	// Act
	// 2000 codes is 20000 draws over a 30-character alphabet. A position that could
	// never produce a given letter would show up as a gap here.
	for i := 0; i < 2000; i++ {
		plaintext, _, err := NewLinkCode()
		if err != nil {
			t.Fatalf("mint link code: %v", err)
		}
		for _, r := range strings.TrimPrefix(plaintext, domain.LinkCodePrefix) {
			seen[r] = true
		}
	}

	// Assert
	// This is what catches a rejection bound written as the wrong constant: clamping
	// or truncating the byte range silently shrinks the keyspace, and the codes still
	// look fine.
	for _, r := range domain.LinkCodeAlphabet {
		if !seen[r] {
			t.Errorf("%q never appeared in 2000 codes; the keyspace is smaller than it looks", r)
		}
	}
}

func TestNewLinkCode_doesNotRepeatItself(t *testing.T) {
	// Arrange
	const mints = 1000
	seen := make(map[string]bool, mints)

	// Act & Assert
	for i := 0; i < mints; i++ {
		plaintext, _, err := NewLinkCode()
		if err != nil {
			t.Fatalf("mint link code: %v", err)
		}
		if seen[plaintext] {
			t.Fatalf("minted %q twice in %d draws", plaintext, mints)
		}
		seen[plaintext] = true
	}
}

func TestLinkCodeCeiling_isAWholeNumberOfAlphabets(t *testing.T) {
	// Arrange & Act & Assert
	// The rejection bound has to be an exact multiple of the alphabet size, or the
	// modulo below it is biased — which is the one defect in a random credential that
	// no amount of testing the output shape would reveal.
	if linkCodeCeiling%linkCodeAlphabetSize != 0 {
		t.Fatalf("ceiling %d is not a multiple of the %d-character alphabet",
			linkCodeCeiling, linkCodeAlphabetSize)
	}
	if linkCodeCeiling > 256 || linkCodeCeiling <= 256-linkCodeAlphabetSize {
		t.Fatalf("ceiling %d discards more bytes than it has to", linkCodeCeiling)
	}
}
