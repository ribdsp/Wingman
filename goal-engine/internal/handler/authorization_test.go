package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// These tests are about the boundary, not the rules. The rules have their own
// tests in internal/service and internal/domain; what can only be checked here is
// whether a request arrives at those rules carrying the right actor, and whether
// the answer comes back as the right status.

func TestAnUnauthenticatedRequestReachesNothing(t *testing.T) {
	// The kill switch and the approval queue sit behind this middleware. A route
	// that answered without a credential would hand both to anyone who can reach
	// the port.
	f := newFixture(t)

	for _, route := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/v1/goals", ""},
		{http.MethodPost, "/v1/goals", validGoalBody()},
		{http.MethodGet, "/v1/flags/kill-switch", ""},
		{http.MethodPut, "/v1/flags/kill-switch", `{"engaged":true,"reason":"x"}`},
		{http.MethodGet, "/v1/audit", ""},
		{http.MethodPost, "/v1/monitor/tick", ""},
		{http.MethodGet, "/v1/metrics", ""},
		{http.MethodPost, "/v1/metrics/acme.mrr/samples", `{"value":1}`},
		{http.MethodGet, "/v1/metrics/acme.mrr/samples/latest", ""},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := f.unauthenticated(t, route.method, route.path, route.body)
			env := decode(t, rec, http.StatusUnauthorized)
			if got := errorCode(t, env); got != utils.ErrCodeUnauthorized {
				t.Fatalf("expected %s, got %s", utils.ErrCodeUnauthorized, got)
			}
		})
	}

	if len(f.audit.events) != 0 {
		t.Fatalf("an unauthenticated request wrote to the audit log: %v", f.audit.events)
	}
}

func TestTheActorComesFromTheCredentialNotTheRequest(t *testing.T) {
	// This is the property the whole authorization model rests on. If a body or a
	// header could name its own actor, every operator-only rule downstream would be
	// advisory.
	f := newFixture(t)

	f.seedGoal(t)
	operatorEntry := f.audit.events[len(f.audit.events)-1]
	if operatorEntry.ActorType != repository.ActorUser {
		t.Fatalf("expected an operator's create to be recorded as %q, got %q",
			repository.ActorUser, operatorEntry.ActorType)
	}
	if operatorEntry.ActorID != "ops" {
		t.Fatalf("expected the credential name as actor id, got %q", operatorEntry.ActorID)
	}

	// The same call as a bot, with a body that claims to be the operator.
	claiming := strings.Replace(validGoalBody(), `"product": "acme"`,
		`"product": "acme", "actorType": "user", "createdBy": "ops"`, 1)
	decode(t, f.asBot(t, http.MethodPost, "/v1/goals", claiming), http.StatusCreated)

	botEntry := f.audit.events[len(f.audit.events)-1]
	if botEntry.ActorType != repository.ActorBot {
		t.Fatalf("a bot's request claimed the operator role and got it: recorded as %q",
			botEntry.ActorType)
	}
	if botEntry.ActorID != "bot-growth" {
		t.Fatalf("expected the bot's credential name as actor id, got %q", botEntry.ActorID)
	}
}

func TestOperatorOnlyRoutesRefuseABot(t *testing.T) {
	// Both of these are guarded twice — here at the route and again in the service.
	// This test covers the route half; the service half is covered where the rule
	// lives, and both have to hold for the invariant to survive a new route.
	f := newFixture(t)
	goalID := f.seedGoal(t)
	approvalID := f.seedPendingApproval(t)

	for _, call := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"lower the target it is judged by", http.MethodPatch, "/v1/goals/" + goalID, `{"targetValue":1}`},
		{"answer its own spend request", http.MethodPost, "/v1/approvals/" + approvalID + "/resolve", `{"resolution":"approved"}`},
	} {
		t.Run(call.name, func(t *testing.T) {
			env := decode(t, f.asBot(t, call.method, call.path, call.body), http.StatusForbidden)
			if got := errorCode(t, env); got != utils.ErrCodeForbidden {
				t.Fatalf("expected %s, got %s", utils.ErrCodeForbidden, got)
			}
		})
	}

	// Nothing changed: a refused request must not be a partial one.
	if len(f.spend.recorded) != 0 {
		t.Fatalf("a refused resolve still recorded spend: %v", f.spend.recorded)
	}
	env := decode(t, f.asOperator(t, http.MethodGet, "/v1/goals/"+goalID, ""), http.StatusOK)
	var goal goalView
	dataInto(t, env, &goal)
	if goal.TargetValue == 1 {
		t.Fatal("a refused patch still changed the target")
	}
}

func TestOperatorOnlyRoutesAdvertiseThemselves(t *testing.T) {
	// A client that has to discover which calls need a human by being refused will
	// discover it in production. The list is declared next to the guards so the two
	// cannot drift.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodGet, "/v1/reference", ""), http.StatusOK)
	var reference struct {
		OperatorOnlyRoutes []string `json:"operatorOnlyRoutes"`
		Decisions          []string `json:"decisions"`
		GoalStatuses       []string `json:"goalStatuses"`
	}
	dataInto(t, env, &reference)

	for _, want := range []string{"PATCH /v1/goals/:id", "POST /v1/approvals/:id/resolve"} {
		found := false
		for _, got := range reference.OperatorOnlyRoutes {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected %q in operatorOnlyRoutes, got %v", want, reference.OperatorOnlyRoutes)
		}
	}
	if len(reference.Decisions) == 0 || len(reference.GoalStatuses) == 0 {
		t.Fatalf("expected the enumerations to be populated, got %+v", reference)
	}
}

func TestAnOperatorCanDoWhatTheBotWasRefused(t *testing.T) {
	// The mirror of the test above. A guard that refuses everyone is a guard that
	// nobody notices is wrong.
	f := newFixture(t)
	goalID := f.seedGoal(t)
	approvalID := f.seedPendingApproval(t)

	env := decode(t, f.asOperator(t, http.MethodPatch, "/v1/goals/"+goalID, `{"targetValue":90000000}`), http.StatusOK)
	var goal goalView
	dataInto(t, env, &goal)
	if goal.TargetValue != 90000000 {
		t.Fatalf("expected the target to change, got %v", goal.TargetValue)
	}

	env = decode(t, f.asOperator(t, http.MethodPost, "/v1/approvals/"+approvalID+"/resolve",
		`{"resolution":"approved","note":"checked the campaign"}`), http.StatusOK)
	var approval approvalView
	dataInto(t, env, &approval)
	if approval.Resolution == nil || *approval.Resolution != string(repository.ResolutionApproved) {
		t.Fatalf("expected an approved resolution, got %+v", approval.Resolution)
	}
	if approval.ResolvedBy == nil || *approval.ResolvedBy != "ops" {
		t.Fatalf("expected the operator's credential name as resolver, got %+v", approval.ResolvedBy)
	}
}

func TestABotMayEngageTheKillSwitchButNotReleaseIt(t *testing.T) {
	// The asymmetry, over HTTP. The route is deliberately not operator-gated so an
	// agent that has worked out it is doing damage can reach the brakes; the
	// direction is separated inside the service, and this checks both halves land
	// on the right status.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodPut, "/v1/flags/kill-switch",
		`{"engaged":true,"reason":"I am opening duplicate campaigns"}`), http.StatusOK)
	var flag flagView
	dataInto(t, env, &flag)
	if !flag.Enabled {
		t.Fatal("expected a bot to be able to engage the kill switch")
	}
	if flag.UpdatedBy != "bot-growth" {
		t.Fatalf("expected the bot's credential name on the flag, got %q", flag.UpdatedBy)
	}

	env = decode(t, f.asBot(t, http.MethodPut, "/v1/flags/kill-switch",
		`{"engaged":false,"reason":"I feel fine now"}`), http.StatusForbidden)
	if got := errorCode(t, env); got != utils.ErrCodeForbidden {
		t.Fatalf("expected %s, got %s", utils.ErrCodeForbidden, got)
	}

	// Still engaged. A refused release that left the switch off would be worse than
	// no switch, because the response said no.
	env = decode(t, f.asBot(t, http.MethodGet, "/v1/flags/kill-switch", ""), http.StatusOK)
	dataInto(t, env, &flag)
	if !flag.Enabled {
		t.Fatal("the kill switch came off despite the release being refused")
	}

	// And the operator can undo it.
	env = decode(t, f.asOperator(t, http.MethodPut, "/v1/flags/kill-switch",
		`{"engaged":false,"reason":"reviewed the campaigns, all clear"}`), http.StatusOK)
	dataInto(t, env, &flag)
	if flag.Enabled {
		t.Fatal("expected the operator to be able to release the switch")
	}
}

func TestABotCannotFileASpendUnderAnotherName(t *testing.T) {
	// A daily cap that can be split across identities is not a cap. The requestedBy
	// field exists for an operator filing on an agent's behalf, and an agent that
	// could use it would have found a way around the ledger.
	f := newFixture(t)

	body := `{"actionType":"ads.spend","amount":50000,"currency":"IDR","requestedBy":"someone-else"}`
	env := decode(t, f.asBot(t, http.MethodPost, "/v1/approvals", body), http.StatusCreated)
	var approval approvalView
	dataInto(t, env, &approval)
	if approval.RequestedBy != "bot-growth" {
		t.Fatalf("a bot named its own requester: recorded as %q", approval.RequestedBy)
	}
	if approval.Outcome != string(domain.ApprovalAutoApproved) {
		t.Fatalf("expected a small spend to auto-approve, got %q (%s)", approval.Outcome, approval.PolicyReason)
	}

	// The operator may, because they are already trusted with the decision they are
	// recording.
	env = decode(t, f.asOperator(t, http.MethodPost, "/v1/approvals",
		`{"actionType":"ads.spend","amount":50000,"currency":"IDR","requestedBy":"bot-growth"}`), http.StatusCreated)
	dataInto(t, env, &approval)
	if approval.RequestedBy != "bot-growth" {
		t.Fatalf("expected the operator's named requester to be honoured, got %q", approval.RequestedBy)
	}
}

func TestADeniedSpendIsStillARecordedRequest(t *testing.T) {
	// Denial is an answer, not a failed call. Returning an HTTP error would tell the
	// caller its request was malformed, and a bot retrying a well-formed request
	// that policy refuses is a loop nobody wants.
	f := newFixture(t)

	body := `{"actionType":"ads.spend","amount":9000000,"currency":"IDR"}`
	env := decode(t, f.asBot(t, http.MethodPost, "/v1/approvals", body), http.StatusCreated)
	var approval approvalView
	dataInto(t, env, &approval)
	if approval.Outcome != string(domain.ApprovalDenied) {
		t.Fatalf("expected a spend over the hard cap to be denied, got %q", approval.Outcome)
	}
	if approval.PolicyReason == "" {
		t.Fatal("expected a denial to say why")
	}
	if !strings.Contains(env.Message, "Denied") {
		t.Fatalf("expected the envelope message to carry the decision, got %q", env.Message)
	}
	if len(f.spend.recorded) != 0 {
		t.Fatalf("a denied spend was written to the ledger: %v", f.spend.recorded)
	}
}

func TestAnUnknownActionTypeWaitsForAHumanRatherThanProceeding(t *testing.T) {
	// Deny by default, expressed as "ask". A spend the operator never wrote a policy
	// for is exactly the case where nobody has decided yet, so nobody has approved.
	f := newFixture(t)

	body := `{"actionType":"ads.something.new","amount":1,"currency":"IDR"}`
	env := decode(t, f.asBot(t, http.MethodPost, "/v1/approvals", body), http.StatusCreated)
	var approval approvalView
	dataInto(t, env, &approval)
	if approval.Outcome != string(domain.ApprovalPending) {
		t.Fatalf("expected an unknown action type to need a human, got %q", approval.Outcome)
	}
}
