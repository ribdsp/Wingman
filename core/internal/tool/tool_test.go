package tool

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidName_acceptsWhatBothProvidersAcceptAndNothingElse(t *testing.T) {
	// Arrange
	// Both vendors enforce ^[a-zA-Z0-9_-]{1,64}$ and reject the whole request when a
	// tool name breaks it — so one badly named tool would break every call the run
	// makes, not only the one that uses it.
	cases := map[string]bool{
		"shell":                          true,
		"read_report":                    true,
		"mcp__reports__read_report":      true,
		"Get-Invoice":                    true,
		"a":                              true,
		strings.Repeat("x", 64):          true,
		strings.Repeat("x", 65):          false,
		"":                               false,
		"read report":                    false,
		"reports.read":                   false,
		"read/report":                    false,
		"emoji_\U0001F600":               false,
		"mcp__reports__read:report":      false,
		strings.Repeat("mcp__srv__x", 6): false,
	}

	// Act, Assert
	for name, want := range cases {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v; want %v", name, got, want)
		}
	}
}

func TestInvocation_arguments_treatsNothingAsAnEmptyObject(t *testing.T) {
	// Arrange
	// Absent, empty and literal null all mean the model called a tool that takes no
	// arguments. Some models send `{}`, some send nothing at all, and a runner that
	// only handled one of those would fail on the other for no reason.
	cases := map[string]Invocation{
		"absent": {Name: "shell"},
		"empty":  {Name: "shell", Input: []byte("")},
		"blank":  {Name: "shell", Input: []byte("   ")},
		"null":   {Name: "shell", Input: []byte("null")},
	}

	// Act, Assert
	for label, invocation := range cases {
		arguments, err := invocation.Arguments()
		if err != nil {
			t.Errorf("%s arguments were refused: %v", label, err)
			continue
		}
		if arguments == nil {
			t.Errorf("%s produced a nil map, which a runner would index into", label)
		}
		if len(arguments) != 0 {
			t.Errorf("%s produced %d arguments; want none", label, len(arguments))
		}
	}
}

func TestInvocation_arguments_readsWhatTheModelSent(t *testing.T) {
	// Arrange
	invocation := Invocation{Name: "shell", Input: []byte(`{"script":"ls -la","retries":2}`)}

	// Act
	arguments, err := invocation.Arguments()

	// Assert
	if err != nil {
		t.Fatalf("well-formed arguments were refused: %v", err)
	}
	if arguments["script"] != "ls -la" {
		t.Errorf("script = %v; want ls -la", arguments["script"])
	}
	if arguments["retries"] != float64(2) {
		t.Errorf("retries = %v (%T); want 2 as a float64", arguments["retries"], arguments["retries"])
	}
}

func TestInvocation_arguments_refusesJSONThatIsNotAnObject(t *testing.T) {
	// Arrange
	// Valid JSON, wrong shape. A runner reading arguments["script"] from this would
	// panic on a reply that was never malformed, only unexpected.
	cases := map[string]string{
		"a string":  `"ls -la"`,
		"an array":  `["ls -la"]`,
		"a number":  `7`,
		"truncated": `{"script":`,
	}

	// Act, Assert
	for label, input := range cases {
		invocation := Invocation{Name: "shell", Input: []byte(input)}
		if _, err := invocation.Arguments(); err == nil {
			t.Errorf("%s was accepted as arguments", label)
		}
	}
}

func TestStringArgument_readsARequiredStringAndNamesWhatIsWrong(t *testing.T) {
	// Arrange
	arguments := map[string]any{
		"script": "ls -la",
		"count":  float64(3),
		"blank":  "   ",
	}

	// Act
	script, err := StringArgument(arguments, "script")

	// Assert
	if err != nil || script != "ls -la" {
		t.Fatalf("script = %q, err = %v; want ls -la and no error", script, err)
	}

	// The three ways it can be wrong. Each message reaches the model, which is the
	// only thing that can correct the call, so each has to say which argument.
	for _, key := range []string{"missing", "count", "blank"} {
		_, err := StringArgument(arguments, key)
		if err == nil {
			t.Errorf("argument %q was accepted", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the error for %q does not name it: %v", key, err)
		}
	}
}

func TestTruncate_leavesOutputThatFitsAlone(t *testing.T) {
	// Arrange
	text := "total 0\ndrwxr-xr-x 2 wingman wingman 4096 Sep 12 09:14 ."

	// Act
	got, truncated := truncate(text, MaxOutputBytes)

	// Assert
	if truncated {
		t.Error("short output was reported as truncated")
	}
	if got != text {
		t.Errorf("short output was rewritten: %q", got)
	}
}

func TestTruncate_keepsBothEndsAndSaysHowMuchWentMissing(t *testing.T) {
	// Arrange
	// The head says what the command was doing; the tail holds the error it died of.
	// Keeping only one end loses the half an operator — or a model — needs.
	text := "START" + strings.Repeat("x", 4_000) + "END"

	// Act
	got, truncated := truncate(text, 100)

	// Assert
	if !truncated {
		t.Fatal("long output was not reported as truncated")
	}
	if !strings.HasPrefix(got, "START") {
		t.Errorf("the beginning was dropped: %q", got[:20])
	}
	if !strings.HasSuffix(got, "END") {
		t.Errorf("the end was dropped: %q", got[len(got)-20:])
	}
	if !strings.Contains(got, "bytes dropped") {
		t.Errorf("nothing said the output was cut: %q", got)
	}
	// Loose, because the marker itself is added on top of the kept bytes. What must
	// not happen is 4 KB arriving when 100 bytes were asked for.
	if len(got) > 200 {
		t.Errorf("kept %d bytes for a 100-byte limit", len(got))
	}
}

func TestTruncate_doesNotCutARuneInHalf(t *testing.T) {
	// Arrange
	// Tool output goes to the model as JSON. Half a rune arrives there as U+FFFD, and
	// a model reading a diff full of replacement characters cannot act on it. The
	// limits walk across a two-byte boundary so both the head cut and the tail cut
	// land mid-rune at least once.
	text := strings.Repeat("é", 500)

	// Act, Assert
	for _, limit := range []int{101, 102, 103, 104} {
		got, truncated := truncate(text, limit)
		if !truncated {
			t.Fatalf("limit %d did not truncate 1000 bytes", limit)
		}
		if !utf8.ValidString(got) {
			t.Errorf("limit %d produced output that is not valid UTF-8", limit)
		}
	}
}

func TestTruncate_anAbsentLimitDoesNotSilentlyEmptyTheOutput(t *testing.T) {
	// Arrange
	text := strings.Repeat("x", 1_000)

	// Act, Assert
	// A zero limit is a caller that forgot to set one. Reading it as "keep nothing"
	// would throw away every tool result on the instance, and it would look like
	// tools that return nothing rather than like a misconfiguration.
	for _, limit := range []int{0, -1} {
		got, truncated := truncate(text, limit)
		if truncated || got != text {
			t.Errorf("limit %d truncated the output", limit)
		}
	}
}
