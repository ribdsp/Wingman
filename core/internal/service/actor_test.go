package service

import (
	"errors"
	"strings"
	"testing"
)

func TestActorPrepare_trimsBeforeValidating(t *testing.T) {
	// Arrange — a configured key name with stray whitespace, which is what an operator's
	// env var actually looks like when they have pasted it.
	actor := Actor{Type: "  operator  ", ID: "  goal-engine  ", RequestID: " req-1 "}

	// Act
	prepared, err := actor.prepare()

	// Assert
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared.Type != ActorOperator {
		t.Errorf("type = %q, want %q", prepared.Type, ActorOperator)
	}
	if prepared.ID != "goal-engine" {
		t.Errorf("id = %q, want %q", prepared.ID, "goal-engine")
	}
	if prepared.RequestID != "req-1" {
		t.Errorf("requestId = %q, want %q", prepared.RequestID, "req-1")
	}
}

func TestActorPrepare_refusesAMachineActorCarryingAUserId(t *testing.T) {
	// A machine that could name its own owner could file work against anybody's budget,
	// so this is a wiring bug rather than a request to interpret.
	for _, kind := range []ActorType{ActorOperator, ActorBot} {
		t.Run(string(kind), func(t *testing.T) {
			// Arrange
			actor := Actor{Type: kind, ID: "bridge", UserID: "user-1"}

			// Act
			_, err := actor.prepare()

			// Assert
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), "must not carry a user id") {
				t.Errorf("err = %v, want it to say why", err)
			}
		})
	}
}

func TestActorPrepare_refusesAUserActorWithoutAnAccount(t *testing.T) {
	// Arrange
	actor := Actor{Type: ActorUser, ID: "user-1"}

	// Act
	_, err := actor.prepare()

	// Assert — without a user id every query that scopes on one would scope on nothing.
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestActorPrepare_refusesAKindNoRuleIsWrittenFor(t *testing.T) {
	// There is deliberately no default type: the three kinds are not ordered, so there is
	// no least-privileged one to fall back to.
	for name, actor := range map[string]Actor{
		"empty":   {ID: "who"},
		"unknown": {Type: "admin", ID: "who"},
		"unnamed": {Type: ActorOperator},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			_, err := actor.prepare()

			// Assert
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestActorRequireOperator_permitsOnlyAnOperator(t *testing.T) {
	// Arrange
	cases := map[ActorType]bool{ActorOperator: true, ActorBot: false, ActorUser: false}

	for kind, permitted := range cases {
		t.Run(string(kind), func(t *testing.T) {
			// Act
			err := Actor{Type: kind, ID: "who"}.requireOperator("editing a goal")

			// Assert
			switch {
			case permitted && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case !permitted && !errors.Is(err, ErrForbidden):
				t.Fatalf("err = %v, want ErrForbidden", err)
			case !permitted && !strings.Contains(err.Error(), "editing a goal"):
				// The operation name is how an operator reading a 403 learns which check
				// refused them.
				t.Errorf("err = %v, want it to name the operation", err)
			}
		})
	}
}

func TestActorRequireDispatcher_permitsAnOperatorAndTheGoalEngineButNotAPerson(t *testing.T) {
	// Unattended work runs with nobody at the keyboard, so starting one belongs to
	// whoever owns the instance.
	cases := map[ActorType]bool{ActorOperator: true, ActorBot: true, ActorUser: false}

	for kind, permitted := range cases {
		t.Run(string(kind), func(t *testing.T) {
			// Act
			err := Actor{Type: kind, ID: "who"}.requireDispatcher("dispatching unattended work")

			// Assert
			switch {
			case permitted && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case !permitted && !errors.Is(err, ErrForbidden):
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestActorOwner_isAnErrorForAMachineKey(t *testing.T) {
	// A machine key has no account. It reaches a person's data only through an operation
	// that was handed the id from configuration.
	for _, kind := range []ActorType{ActorOperator, ActorBot} {
		t.Run(string(kind), func(t *testing.T) {
			// Act
			owner, err := Actor{Type: kind, ID: "bridge"}.Owner()

			// Assert
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
			if owner != "" {
				t.Errorf("owner = %q, want empty", owner)
			}
		})
	}
}

func TestActorOwner_isTheAccountForAPerson(t *testing.T) {
	// Act
	owner, err := Actor{Type: ActorUser, ID: "user-1", UserID: "user-1"}.Owner()

	// Assert
	if err != nil {
		t.Fatalf("Owner: %v", err)
	}
	if owner != "user-1" {
		t.Errorf("owner = %q, want %q", owner, "user-1")
	}
}

func TestActorString_namesTheKindAndWhoWithoutASecret(t *testing.T) {
	// Arrange — the id is a configured key *name*, never the key itself, and this is the
	// form that goes in a log line.
	cases := map[string]Actor{
		"bot:goal-engine": {Type: ActorBot, ID: "goal-engine"},
		"user:user-1":     {Type: ActorUser, ID: "user-1", UserID: "user-1"},
		"operator":        {Type: ActorOperator},
	}

	for want, actor := range cases {
		// Act
		got := actor.String()

		// Assert
		if got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func TestSystemActor_isAnOperatorThatValidates(t *testing.T) {
	// Act
	prepared, err := SystemActor().prepare()

	// Assert — the CLI and the background sweeps go through it, so it has to satisfy the
	// same rules a configured key does.
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared.Type != ActorOperator {
		t.Errorf("type = %q, want %q", prepared.Type, ActorOperator)
	}
	if err := prepared.requireOperator("sweeping"); err != nil {
		t.Errorf("requireOperator: %v", err)
	}
	if _, err := prepared.Owner(); !errors.Is(err, ErrForbidden) {
		// It runs on the box, so it is an operator — but it is still not a person, and it
		// must not resolve to somebody's account.
		t.Errorf("Owner err = %v, want ErrForbidden", err)
	}
}
