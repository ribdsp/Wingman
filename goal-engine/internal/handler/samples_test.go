package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/service"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// A reported value is the one number in this service that arrives over the wire
// and is then used to decide whether an agent should act. These tests are about
// the boundary that number crosses: which metrics accept one, what a malformed
// request does instead of guessing, and whose credential the record carries.
//
// The fixture's registry declares acme.mrr as push and acme.signups as http, so
// both halves of the rule have a real metric to be checked against.

func TestAReportedValueIsAcceptedAndReadableBack(t *testing.T) {
	// The read-back route is not a convenience: a feed that cannot confirm its value
	// landed has no way to tell a rejected push from a silent one.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodPost, "/v1/metrics/acme.mrr/samples",
		`{"value":82500000,"observedAt":"2026-09-11T11:00:00Z","note":"closed the books"}`),
		http.StatusCreated)

	var recorded sampleView
	dataInto(t, env, &recorded)
	if recorded.MetricKey != "acme.mrr" || recorded.Value != 82500000 {
		t.Fatalf("expected the reported value back, got %+v", recorded)
	}
	if recorded.Source != string(metrics.SourcePush) {
		t.Fatalf("expected the observation to be marked %q, got %q", metrics.SourcePush, recorded.Source)
	}
	if !recorded.ObservedAt.Equal(time.Date(2026, 9, 11, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected the reported timestamp to be kept, got %s", recorded.ObservedAt)
	}
	if recorded.ID == 0 {
		t.Fatal("expected the stored observation to have an id")
	}

	env = decode(t, f.asBot(t, http.MethodGet, "/v1/metrics/acme.mrr/samples/latest", ""), http.StatusOK)
	var latest sampleView
	dataInto(t, env, &latest)
	if latest != recorded {
		t.Fatalf("expected to read back exactly what was recorded: got %+v, want %+v", latest, recorded)
	}
}

func TestAValueForAMetricTheEngineReadsItselfIsRefused(t *testing.T) {
	// acme.signups is fetched over HTTP. Accepting a posted value for it would let
	// a caller overwrite a number the operator said comes from somewhere else, and
	// leave the engine with two sources that can disagree.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodPost, "/v1/metrics/acme.signups/samples",
		`{"value":41}`), http.StatusBadRequest)
	if got := errorCode(t, env); got != utils.ErrCodeValidation {
		t.Fatalf("expected %s, got %s", utils.ErrCodeValidation, got)
	}
	if len(f.samples.records) != 0 {
		t.Fatalf("a refused value was stored anyway: %+v", f.samples.records)
	}
}

func TestAnOperatorCannotPushToAPulledMetricEither(t *testing.T) {
	// The rule is about where a number comes from, not about who is asking. An
	// operator key is for deciding, not for inventing readings.
	f := newFixture(t)

	decode(t, f.asOperator(t, http.MethodPost, "/v1/metrics/acme.signups/samples",
		`{"value":41}`), http.StatusBadRequest)
	if len(f.samples.records) != 0 {
		t.Fatalf("a refused value was stored anyway: %+v", f.samples.records)
	}
}

func TestAValueForAnUndeclaredMetricIsNotFound(t *testing.T) {
	// Metrics exist only in the operator's YAML. A push cannot conjure a key, or the
	// registry would stop being the list of things a goal may be measured on.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodPost, "/v1/metrics/acme.invented/samples",
		`{"value":1}`), http.StatusNotFound)
	if got := errorCode(t, env); got != utils.ErrCodeNotFound {
		t.Fatalf("expected %s, got %s", utils.ErrCodeNotFound, got)
	}
}

func TestZeroIsARealReadingButAnAbsentValueIsNot(t *testing.T) {
	// "Nothing happened today" is a number a goal needs to see. An empty body is not
	// that number, and binding one to zero would turn a broken feed into a report of
	// total collapse.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodPost, "/v1/metrics/acme.mrr/samples",
		`{"value":0}`), http.StatusCreated)
	var recorded sampleView
	dataInto(t, env, &recorded)
	if recorded.Value != 0 {
		t.Fatalf("expected zero to be stored as a reading, got %v", recorded.Value)
	}

	env = decode(t, f.asBot(t, http.MethodPost, "/v1/metrics/acme.mrr/samples",
		`{"note":"forgot the number"}`), http.StatusBadRequest)
	if got := errorCode(t, env); got != utils.ErrCodeValidation {
		t.Fatalf("expected %s, got %s", utils.ErrCodeValidation, got)
	}
	if len(f.samples.records) != 1 {
		t.Fatalf("expected only the zero reading to be stored, got %+v", f.samples.records)
	}
}

func TestAnUnparseableObservedAtIsRejectedRatherThanTreatedAsNow(t *testing.T) {
	// Silently stamping the value with now would make a backfill look like a fresh
	// reading, which is the one thing the staleness check exists to notice.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodPost, "/v1/metrics/acme.mrr/samples",
		`{"value":10,"observedAt":"last tuesday"}`), http.StatusBadRequest)
	if !strings.Contains(env.Error.Message, "RFC 3339") {
		t.Fatalf("expected the message to name the format, got %q", env.Error.Message)
	}
	if len(f.samples.records) != 0 {
		t.Fatalf("a rejected timestamp still stored a value: %+v", f.samples.records)
	}
}

func TestAnOmittedObservedAtMeansNow(t *testing.T) {
	// A feed that reports as it reads should not have to send a timestamp to say so.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodPost, "/v1/metrics/acme.mrr/samples",
		`{"value":7}`), http.StatusCreated)
	var recorded sampleView
	dataInto(t, env, &recorded)
	if !recorded.ObservedAt.Equal(testNow) {
		t.Fatalf("expected the observation to be stamped now (%s), got %s", testNow, recorded.ObservedAt)
	}
}

func TestAReportedValueIsAuditedUnderTheCredentialThatSentIt(t *testing.T) {
	// A push metric is only as trustworthy as the credential feeding it, so the
	// credential is what the record has to name. Without it a wrong number cannot be
	// traced back to whatever produced it.
	f := newFixture(t)

	f.asBot(t, http.MethodPost, "/v1/metrics/acme.mrr/samples", `{"value":15}`)
	botEntry := f.audit.events[len(f.audit.events)-1]
	if botEntry.Action != service.ActionSampleRecorded || botEntry.SubjectType != service.SubjectMetric {
		t.Fatalf("expected a recorded-sample event, got %+v", botEntry)
	}
	if botEntry.ActorType != repository.ActorBot || botEntry.ActorID != "bot-growth" {
		t.Fatalf("expected the bot's own credential on the record, got %q/%q",
			botEntry.ActorType, botEntry.ActorID)
	}
	if botEntry.SubjectID != "acme.mrr" {
		t.Fatalf("expected the metric as the subject, got %q", botEntry.SubjectID)
	}
	if botEntry.RequestID == "" {
		t.Fatal("expected the request id on the record")
	}

	f.asOperator(t, http.MethodPost, "/v1/metrics/acme.mrr/samples", `{"value":16}`)
	operatorEntry := f.audit.events[len(f.audit.events)-1]
	if operatorEntry.ActorType != repository.ActorUser || operatorEntry.ActorID != "ops" {
		t.Fatalf("expected the operator's credential on the record, got %q/%q",
			operatorEntry.ActorType, operatorEntry.ActorID)
	}
}

func TestTheLatestValueOfAMetricNobodyHasReportedIsNotFound(t *testing.T) {
	// An absent observation is not zero. Rendering one would hand a dashboard a
	// reading that nothing ever measured.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodGet, "/v1/metrics/acme.mrr/samples/latest", ""),
		http.StatusNotFound)
	if got := errorCode(t, env); got != utils.ErrCodeNotFound {
		t.Fatalf("expected %s, got %s", utils.ErrCodeNotFound, got)
	}
}

func TestTheLatestValueOfAnUndeclaredMetricIsNotFound(t *testing.T) {
	f := newFixture(t)

	decode(t, f.asBot(t, http.MethodGet, "/v1/metrics/acme.invented/samples/latest", ""),
		http.StatusNotFound)
}

func TestAStorageFailureReadingAValueIsA500NotAnAbsentValue(t *testing.T) {
	// A dead database must not read as "no value has been reported", because that is
	// the answer a caller would retry against forever.
	f := newFixture(t)
	f.samples.err = errStorage

	env := decode(t, f.asBot(t, http.MethodGet, "/v1/metrics/acme.mrr/samples/latest", ""),
		http.StatusInternalServerError)
	if got := errorCode(t, env); got != utils.ErrCodeInternal {
		t.Fatalf("expected %s, got %s", utils.ErrCodeInternal, got)
	}
	if strings.Contains(env.Error.Message, "connection refused") {
		t.Fatalf("the storage error reached the response: %q", env.Error.Message)
	}
}

func TestASampleResponseSaysNothingAboutHowTheMetricIsCollected(t *testing.T) {
	// The observation view is read by whatever pushes values, which is the least
	// privileged thing in the deployment. It gets the number back and nothing else.
	f := newFixture(t)

	rec := f.asBot(t, http.MethodPost, "/v1/metrics/acme.mrr/samples",
		`{"value":5,"note":"from the billing export at 10.0.0.4"}`)
	body := rec.Body.String()
	for _, leaked := range []string{
		"internal.example.invalid", "jsonPath", "query", "datasource", "authHeader",
		// The note is stored for an auditor, not echoed to the sender: it can carry
		// whatever a feed felt like writing, including a host or a path.
		"10.0.0.4", "note",
	} {
		if strings.Contains(body, leaked) {
			t.Fatalf("sample response leaked %q: %s", leaked, body)
		}
	}
}
