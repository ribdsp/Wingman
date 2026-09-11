package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// A response body is a place secrets leak from. These tests read the raw JSON
// rather than a decoded struct, because a field added to a view later would
// deserialise into nothing and pass a struct-based assertion.

func TestMetricResponsesNeverPublishHowAMetricIsCollected(t *testing.T) {
	// A metric definition carries the SQL it runs, the datasource it runs against
	// and the internal URL it fetches. The endpoint exists so a client can discover
	// which keys a goal may reference; anything past that is a map of the inside of
	// the network.
	f := newFixture(t)

	for _, path := range []string{"/v1/metrics", "/v1/metrics/acme.signups", "/v1/metrics/acme.mrr"} {
		t.Run(path, func(t *testing.T) {
			rec := f.asBot(t, http.MethodGet, path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, leaked := range []string{
				"internal.example.invalid", // the URL
				"jsonPath", "json_path",    // how the response is read
				"query", "datasource", "authHeader", "auth_header", "url",
			} {
				if strings.Contains(body, leaked) {
					t.Fatalf("metric response leaked %q: %s", leaked, body)
				}
			}
		})
	}
}

func TestMetricListIsOrderedAndComplete(t *testing.T) {
	// Ordering is not cosmetic here: a client that renders this list for a human
	// picking a metric key should not see it reshuffle between calls, and map
	// iteration would do exactly that.
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodGet, "/v1/metrics", ""), http.StatusOK)
	var views []metricView
	dataInto(t, env, &views)

	if len(views) != 2 {
		t.Fatalf("expected both declared metrics, got %+v", views)
	}
	if views[0].Key != "acme.mrr" || views[1].Key != "acme.signups" {
		t.Fatalf("expected metrics sorted by key, got %q then %q", views[0].Key, views[1].Key)
	}
	if views[0].Source != "push" || views[1].Source != "http" {
		t.Fatalf("expected the declared sources to survive rendering, got %+v", views)
	}
	if views[0].Unit != "IDR" {
		t.Fatalf("expected the unit to be published, got %q", views[0].Unit)
	}
}

func TestAnUndeclaredMetricIsNotFoundWithoutNamingTheOnesThatAre(t *testing.T) {
	f := newFixture(t)

	env := decode(t, f.asBot(t, http.MethodGet, "/v1/metrics/acme.secret", ""), http.StatusNotFound)
	if got := errorCode(t, env); got != utils.ErrCodeUnknownMetric {
		t.Fatalf("expected %s, got %s", utils.ErrCodeUnknownMetric, got)
	}
	if strings.Contains(env.Error.Message, "acme.mrr") {
		t.Fatalf("a 404 enumerated the declared metrics: %q", env.Error.Message)
	}
}

func TestAGoalMayOnlyReferenceADeclaredMetric(t *testing.T) {
	// The registry is the operator's list. A goal pointing at a metric nobody
	// declared would sit in the engine failing every tick, and accepting it would
	// also be the first half of "let the agent name its own datasource".
	f := newFixture(t)

	body := strings.Replace(validGoalBody(), `"acme.mrr"`, `"acme.invented"`, 1)
	env := decode(t, f.asBot(t, http.MethodPost, "/v1/goals", body), http.StatusBadRequest)
	if got := errorCode(t, env); got != utils.ErrCodeValidation {
		t.Fatalf("expected %s, got %s", utils.ErrCodeValidation, got)
	}
}

func TestASourceTextIsRecordedByLengthNotContent(t *testing.T) {
	// The audit entry for a patch is a before/after diff, and sourceText is a
	// paraphrase of something a human said. Recording the old and new text in full
	// would turn the audit log into a transcript store, so the diff carries lengths.
	f := newFixture(t)
	goalID := f.seedGoal(t)

	decode(t, f.asOperator(t, http.MethodPatch, "/v1/goals/"+goalID,
		`{"sourceText":"a completely different phrasing nobody should find in a log"}`), http.StatusOK)

	env := decode(t, f.asOperator(t, http.MethodGet, "/v1/audit?action=goal.updated", ""), http.StatusOK)
	body := env.Data
	if strings.Contains(string(body), "nobody should find in a log") {
		t.Fatalf("the audit log recorded the source text verbatim: %s", body)
	}
	if !strings.Contains(string(body), "sourceText") {
		t.Fatalf("expected the diff to mention the field that changed: %s", body)
	}
}

func TestAuditDetailIsEmittedAsJSONNotAsAQuotedString(t *testing.T) {
	// The detail column already holds JSON. Re-encoding it into a string field would
	// hand every client a blob to parse a second time, which is the kind of thing
	// that gets parsed with a regex.
	f := newFixture(t)
	goalID := f.seedGoal(t)
	decode(t, f.asOperator(t, http.MethodPatch, "/v1/goals/"+goalID, `{"targetValue":123456}`), http.StatusOK)

	env := decode(t, f.asOperator(t, http.MethodGet, "/v1/audit?action=goal.updated", ""), http.StatusOK)
	var views []struct {
		Action string          `json:"action"`
		Detail json.RawMessage `json:"detail"`
	}
	dataInto(t, env, &views)
	if len(views) == 0 {
		t.Fatalf("expected the patch to be in the audit log: %s", env.Data)
	}

	var detail struct {
		Changes map[string]struct {
			From any `json:"from"`
			To   any `json:"to"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(views[0].Detail, &detail); err != nil {
		t.Fatalf("expected detail to be a JSON object, got %s: %v", views[0].Detail, err)
	}
	change, ok := detail.Changes["targetValue"]
	if !ok {
		t.Fatalf("expected the changed field in the diff, got %v", detail)
	}
	if change.To != float64(123456) {
		t.Fatalf("expected the new value in the diff, got %v", change.To)
	}
	if change.From != float64(100000000) {
		t.Fatalf("expected the value it had before, got %v", change.From)
	}
}

func TestJSONRawRendersWhatItWasGiven(t *testing.T) {
	// The invalid case is the one that matters: a row written by hand or by an older
	// schema must not be able to corrupt the whole response body.
	for _, tc := range []struct {
		name  string
		given jsonRaw
		want  string
	}{
		{"an object passes through", `{"a":1}`, `{"a":1}`},
		{"whitespace is trimmed", "  {\"a\":1}\n", `{"a":1}`},
		{"empty becomes null", "", "null"},
		{"whitespace only becomes null", "   ", "null"},
		{"a bare string is quoted", `not json at all`, `"not json at all"`},
		{"a truncated object is quoted", `{"a":`, `"{\"a\":"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.given)
			if err != nil {
				t.Fatalf("marshalling must not fail: %v", err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, encoded)
			}
			// Whatever it produced has to leave the document parseable.
			var into any
			if err := json.Unmarshal(encoded, &into); err != nil {
				t.Fatalf("produced unparseable JSON %s: %v", encoded, err)
			}
		})
	}
}
