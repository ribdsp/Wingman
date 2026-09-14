package goalengine

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/agent"
)

// The kill switch. Every test here is about one rule: this call answers "not engaged"
// only when the goal engine said so in as many words. Everything else is an error, and
// the agent loop halts on an error.

// The signature is Halt's, and this is the assertion that keeps it that way. If it
// stops compiling, cmd needs an adapter — which is exactly the three lines of mapping
// that a shared shape exists to avoid.
var _ agent.Halt = (*Client)(nil)

// And the standalone instance's switch satisfies the same port, so the wiring in cmd
// chooses between two implementations rather than between an implementation and a nil.
var _ agent.Halt = Absent{}

func TestEngaged_theEngineSaysTheSwitchIsOff_answersNotEngaged(t *testing.T) {
	// Arrange
	fake, client := newEngine(t, http.StatusOK, killSwitchReleasedBody)

	// Act
	engaged, err := client.Engaged(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Engaged() = %v; want an answer", err)
	}
	if engaged {
		t.Error("engaged = true; want false, which is what the engine said")
	}
	got := fake.only(t)
	if got.method != http.MethodGet || got.path != KillSwitchPath {
		t.Errorf("request = %s %s; want GET %s", got.method, got.path, KillSwitchPath)
	}
}

func TestEngaged_theEngineSaysTheSwitchIsOn_answersEngaged(t *testing.T) {
	// Arrange
	// enabled: true on a flag named kill_switch means everything stops. The reason and
	// the operator who engaged it are in the body; the loop needs neither, and reading
	// only the boolean is what keeps this call cheap enough to make before every
	// iteration of every run.
	_, client := newEngine(t, http.StatusOK, killSwitchEngagedBody)

	// Act
	engaged, err := client.Engaged(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Engaged() = %v; want an answer", err)
	}
	if !engaged {
		t.Error("engaged = false; want true — the switch was on and runs would have carried on")
	}
}

func TestEngaged_anAnswerItCannotTrust_isAnErrorAndNeverAFalse(t *testing.T) {
	// The whole point of the port returning (bool, error). Each of these would read as
	// "nothing is halted" if the bool were taken on its own, and the one thing an
	// unreadable switch must not do is release the fleet.
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "the engine failed",
			status: http.StatusInternalServerError,
			body:   `{"success":false,"error":{"code":"internal_error","message":"database is unavailable"}}`,
			want:   "500",
		},
		{
			name: "core is not authenticated",
			// The likeliest misconfiguration, and the reason config refuses to boot
			// with a base URL and no key: it would show up here, halting everything.
			status: http.StatusUnauthorized,
			body:   `{"success":false,"error":{"code":"unauthorized"}}`,
			want:   "401",
		},
		{
			name:   "something else is on that address",
			status: http.StatusOK,
			body:   "<html><body>please sign in</body></html>",
			want:   "decode response",
		},
		{
			name:   "a 200 with no flag in it",
			status: http.StatusOK,
			body:   `{"success":true,"code":200,"message":"ok","data":{}}`,
			want:   "refusing to read that as released",
		},
		{
			name:   "a 200 with no body at all",
			status: http.StatusOK,
			body:   `{}`,
			want:   "refusing to read that as released",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			_, client := newEngine(t, test.status, test.body)

			// Act
			engaged, err := client.Engaged(context.Background())

			// Assert
			if err == nil {
				t.Fatalf("Engaged() = %v, nil; want an error", engaged)
			}
			if engaged {
				t.Error("engaged = true alongside an error; the caller reads the error, and a true here would be a second opinion")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v; want it to mention %q", err, test.want)
			}
			if !strings.Contains(err.Error(), "kill switch") {
				t.Errorf("error = %v; want it to say which call failed", err)
			}
		})
	}
}

func TestEngaged_nothingAnswers_isAnError(t *testing.T) {
	// Arrange
	// An unreachable goal engine is the case this fail-closed rule exists for: core
	// keeps working, and every run halts until the engine answers again.
	client := newUnreachableEngine(t)

	// Act
	engaged, err := client.Engaged(context.Background())

	// Assert
	if err == nil {
		t.Fatalf("Engaged() = %v, nil; want an error", engaged)
	}
	if engaged {
		t.Error("engaged = true; want false with the error, so the caller has one thing to read")
	}
	// Retryable, because the engine may be restarting. The loop does not retry it —
	// it halts — but a report worker checking whether to try again does.
	if !IsRetryable(err) {
		t.Errorf("error = %v; want it marked retryable", err)
	}
}

func TestAbsent_answersThatNothingIsHalted(t *testing.T) {
	// Arrange
	// An instance with no goal engine configured. This is the one permissive answer in
	// the package, and it is permissive because the question was never asked: there is
	// no switch, rather than a switch that cannot be read.
	var switchboard Absent

	// Act
	engaged, err := switchboard.Engaged(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Engaged() = %v; want no error — there is nothing to fail", err)
	}
	if engaged {
		t.Error("engaged = true; a standalone instance would never run anything")
	}
}
