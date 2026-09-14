package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// seedEvaluation writes an evaluation straight into storage.
//
// Not through the API, because there is no route that writes one and there must
// never be: an endpoint that could add an evaluation could add a verdict no
// recorded observation supports. Evaluations appear only as a side effect of a
// monitor tick.
func (f *fixture) seedEvaluation(t *testing.T, goalID string, decision domain.Decision, paceRatio float64) {
	t.Helper()
	_, err := f.evals.Insert(context.Background(), domain.Evaluation{
		GoalID:        goalID,
		ObservedValue: 42_000_000,
		TargetValue:   100_000_000,
		BaselineValue: 10_000_000,
		ExpectedValue: 55_000_000,
		ProgressRatio: 0.35,
		ElapsedRatio:  0.5,
		PaceRatio:     paceRatio,
		OnTrack:       paceRatio >= 1,
		TargetMet:     false,
		Decision:      decision,
		Reason:        "behind pace",
		EvaluatedAt:   testNow,
	}, nil)
	if err != nil {
		t.Fatalf("seed evaluation: %v", err)
	}
}

func TestListGoalEvaluations_isReadableByBothRoles(t *testing.T) {
	// The history is a read of what the monitor already decided. Gating it to
	// operators would mean the dashboard a bot key drives could show every goal's
	// target but never whether it is being met, which is the one number that
	// matters.
	f := newFixture(t)
	goalID := f.seedGoal(t)
	f.seedEvaluation(t, goalID, domain.DecisionTrigger, 0.7)
	path := "/v1/goals/" + goalID + "/evaluations"

	operator := decode(t, f.asOperator(t, http.MethodGet, path, ""), http.StatusOK)
	bot := decode(t, f.asBot(t, http.MethodGet, path, ""), http.StatusOK)

	for name, env := range map[string]envelope{"operator": operator, "bot": bot} {
		var views []evaluationView
		dataInto(t, env, &views)
		if len(views) != 1 {
			t.Fatalf("%s: expected one evaluation, got %d", name, len(views))
		}
		if views[0].GoalID != goalID {
			t.Errorf("%s: goalId = %q, want %q", name, views[0].GoalID, goalID)
		}
	}
}

func TestListGoalEvaluations_rendersTheEvaluatorsOwnNumbers(t *testing.T) {
	// Every pace number the evaluator computed is published, because the point of
	// the endpoint is that no client recomputes them. A dashboard dividing progress
	// by elapsed time for itself would eventually disagree with the code that
	// decides whether an agent is woken, and the disagreement would be silent.
	f := newFixture(t)
	goalID := f.seedGoal(t)
	f.seedEvaluation(t, goalID, domain.DecisionTrigger, 0.7)

	rec := f.asOperator(t, http.MethodGet, "/v1/goals/"+goalID+"/evaluations", "")
	env := decode(t, rec, http.StatusOK)

	var views []evaluationView
	dataInto(t, env, &views)
	got := views[0]
	if got.PaceRatio != 0.7 {
		t.Errorf("paceRatio = %v, want 0.7", got.PaceRatio)
	}
	if got.ExpectedValue != 55_000_000 || got.ObservedValue != 42_000_000 {
		t.Errorf("expected/observed = %v/%v, want 55000000/42000000", got.ExpectedValue, got.ObservedValue)
	}
	if got.OnTrack {
		t.Error("onTrack should be false at a pace ratio of 0.7")
	}
	if got.Decision != string(domain.DecisionTrigger) {
		t.Errorf("decision = %q, want %q", got.Decision, domain.DecisionTrigger)
	}

	// The envelope is camelCase, and these field names are what a client reads.
	body := rec.Body.String()
	for _, field := range []string{
		"goalId", "observedValue", "targetValue", "baselineValue", "expectedValue",
		"progressRatio", "elapsedRatio", "paceRatio", "onTrack", "targetMet",
		"decision", "reason", "evaluatedAt",
	} {
		if !strings.Contains(body, `"`+field+`"`) {
			t.Errorf("expected %q in the response body: %s", field, body)
		}
	}
	if strings.Contains(body, "_") {
		t.Errorf("snake_case reached the public API: %s", body)
	}
}

func TestListGoalEvaluations_unknownGoal_isNotFoundRatherThanAnEmptyPage(t *testing.T) {
	// An empty page for a mistyped id reads as a goal the monitor has never once
	// managed to evaluate. That is a real state, and an alarming one; it must not
	// be what a typo looks like.
	f := newFixture(t)

	rec := f.asOperator(t, http.MethodGet, "/v1/goals/goal-does-not-exist/evaluations", "")
	env := decode(t, rec, http.StatusNotFound)
	if code := errorCode(t, env); code != utils.ErrCodeNotFound {
		t.Errorf("error code = %q, want %s", code, utils.ErrCodeNotFound)
	}
}

func TestLatestEvaluations_returnsOneRowPerGoal(t *testing.T) {
	// This is the query the dashboard opens with. Two rows for one goal would mean
	// the same goal rendered twice with different pace, and an operator has no way
	// to tell which of the two is current.
	f := newFixture(t)
	first := f.seedGoal(t)
	second := f.seedGoal(t)
	f.seedEvaluation(t, first, domain.DecisionNoop, 1.2)
	f.seedEvaluation(t, first, domain.DecisionTrigger, 0.6)
	f.seedEvaluation(t, second, domain.DecisionNoop, 1.05)

	env := decode(t, f.asBot(t, http.MethodGet, "/v1/evaluations/latest", ""), http.StatusOK)
	var views []evaluationView
	dataInto(t, env, &views)

	if len(views) != 2 {
		t.Fatalf("expected one row per evaluated goal, got %d", len(views))
	}
	byGoal := map[string]evaluationView{}
	for _, view := range views {
		if _, seen := byGoal[view.GoalID]; seen {
			t.Fatalf("goal %s appears twice", view.GoalID)
		}
		byGoal[view.GoalID] = view
	}
	if got := byGoal[first].PaceRatio; got != 0.6 {
		t.Errorf("goal %s paceRatio = %v, want the newest evaluation's 0.6", first, got)
	}
	if env.Meta.Pagination == nil || env.Meta.Pagination.TotalItems != 2 {
		t.Errorf("expected totalItems 2, got %+v", env.Meta.Pagination)
	}
}

func TestLatestEvaluations_goalWithNoHistory_isAbsentRatherThanZeroed(t *testing.T) {
	// A goal the monitor has not reached yet has no pace. Rendering it as zero
	// would put it on the dashboard as catastrophically behind; rendering it as one
	// would put it there as on track. Neither is true, so it is not there at all.
	f := newFixture(t)
	evaluated := f.seedGoal(t)
	f.seedGoal(t) // never evaluated
	f.seedEvaluation(t, evaluated, domain.DecisionNoop, 1.1)

	env := decode(t, f.asOperator(t, http.MethodGet, "/v1/evaluations/latest", ""), http.StatusOK)
	var views []evaluationView
	dataInto(t, env, &views)
	if len(views) != 1 {
		t.Fatalf("expected only the evaluated goal, got %d rows", len(views))
	}
	if views[0].GoalID != evaluated {
		t.Errorf("goalId = %q, want %q", views[0].GoalID, evaluated)
	}
}

func TestEvaluations_storageFailure_isA500WithoutTheDriverMessage(t *testing.T) {
	f := newFixture(t)
	goalID := f.seedGoal(t)
	f.evals.listErr = errStorage

	for _, path := range []string{"/v1/goals/" + goalID + "/evaluations", "/v1/evaluations/latest"} {
		rec := f.asOperator(t, http.MethodGet, path, "")
		env := decode(t, rec, http.StatusInternalServerError)
		if code := errorCode(t, env); code != utils.ErrCodeInternal {
			t.Errorf("%s: error code = %q, want %s", path, code, utils.ErrCodeInternal)
		}
		if strings.Contains(rec.Body.String(), errStorage.Error()) {
			t.Errorf("%s: the driver message reached the response: %s", path, rec.Body.String())
		}
	}
}
