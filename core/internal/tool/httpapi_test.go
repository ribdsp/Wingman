package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recordedRequest is what a stub API saw.
type recordedRequest struct {
	method string
	// path is the escaped path, so an escaped value is visible as it went out rather
	// than as the server chose to read it.
	path   string
	query  url.Values
	header http.Header
	body   string
	calls  int
}

// stubAPI answers every request with status and reply, recording what it was sent.
func stubAPI(t *testing.T, status int, reply string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	seen := &recordedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen.calls++
		seen.method = r.Method
		seen.path = r.URL.EscapedPath()
		seen.query = r.URL.Query()
		seen.header = r.Header.Clone()
		seen.body = string(body)

		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(server.Close)
	return server, seen
}

// fakeEnv is a lookupEnv over a map, so a test never reads the real environment.
func fakeEnv(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, present := values[name]
		return value, present
	}
}

// readReportTool is the endpoint most of these tests call: one path parameter, one
// optional query parameter.
func readReportTool() HTTPTool {
	return HTTPTool{
		Name:        "read_report",
		Description: "Fetch one report by id.",
		Method:      http.MethodGet,
		Path:        "/v1/reports/{reportId}",
		Parameters: []HTTPParameter{
			{Name: "reportId", In: ParameterPath, Description: "The report's id.", Required: true},
			{Name: "format", In: ParameterQuery, Description: "csv or json."},
		},
	}
}

// createReportTool declares a JSON body.
func createReportTool() HTTPTool {
	return HTTPTool{
		Name:            "create_report",
		Description:     "Queue a new report.",
		Method:          http.MethodPost,
		Path:            "/v1/reports",
		BodyDescription: `{"period":"2026-09","kind":"revenue"}`,
	}
}

// httpRunner builds a runner for a service at baseURL.
func httpRunner(t *testing.T, baseURL string, tools ...HTTPTool) *HTTPAPI {
	t.Helper()
	return NewHTTPAPI(
		HTTPService{Name: "reports", BaseURL: baseURL, Tools: tools, Enabled: true},
		"test",
		fakeEnv(nil),
	)
}

// call runs one invocation with arguments as raw JSON.
func call(t *testing.T, runner *HTTPAPI, name, arguments string) (Result, error) {
	t.Helper()
	return runner.Call(context.Background(), Invocation{
		Name:      name,
		Input:     []byte(arguments),
		Workspace: t.TempDir(),
	})
}

func TestHTTPAPI_tools_describesEachDeclaredEndpointToTheModel(t *testing.T) {
	// Arrange
	runner := httpRunner(t, "https://reports.internal", readReportTool(), createReportTool())

	// Act
	definitions, err := runner.Tools(context.Background())

	// Assert
	// Listing cannot fail and touches no network: the tool list of an HTTP service is a
	// config file, so unlike an MCP server there is nothing to be unavailable when a
	// run starts.
	if err != nil {
		t.Fatalf("listing declared endpoints failed: %v", err)
	}
	if len(definitions) != 2 {
		t.Fatalf("offered %d tools; want 2", len(definitions))
	}

	read := definitions[0]
	if read.Name != "read_report" || !ValidName(read.Name) {
		t.Errorf("name = %q; want a provider-acceptable read_report", read.Name)
	}
	if read.Source != SourceHTTP || read.Origin != "reports" {
		t.Errorf("source/origin = %q/%q; want %q/reports", read.Source, read.Origin, SourceHTTP)
	}

	properties, ok := read.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the schema has no properties object: %#v", read.InputSchema)
	}
	if _, declared := properties["reportId"]; !declared {
		t.Error("the path parameter is not in the schema, so the model cannot supply it")
	}
	if _, declared := properties["format"]; !declared {
		t.Error("the query parameter is not in the schema")
	}
	// False, so a model that invents a parameter is corrected by the provider before
	// the call is made rather than by us after it.
	if read.InputSchema["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v; want false", read.InputSchema["additionalProperties"])
	}
	required := fmt.Sprint(read.InputSchema["required"])
	if !strings.Contains(required, "reportId") || strings.Contains(required, "format") {
		t.Errorf("required = %v; want the path parameter alone", read.InputSchema["required"])
	}

	create := definitions[1]
	createProperties := create.InputSchema["properties"].(map[string]any)
	body, declared := createProperties[bodyArgument].(map[string]any)
	if !declared {
		t.Fatalf("the declared body is not offered as an argument: %#v", createProperties)
	}
	if body["type"] != "object" {
		t.Errorf("the body is typed %v; want object", body["type"])
	}
	// The operator's description is the whole interface as far as the model is
	// concerned, so it has to reach it verbatim.
	if body["description"] != createReportTool().BodyDescription {
		t.Errorf("body description = %v; want the operator's own", body["description"])
	}
	if got := fmt.Sprint(create.InputSchema["required"]); !strings.Contains(got, bodyArgument) {
		t.Errorf("required = %v; want the body", create.InputSchema["required"])
	}
}

func TestHTTPAPI_call_sendsTheRequestTheOperatorDeclared(t *testing.T) {
	// Arrange
	server, seen := stubAPI(t, http.StatusOK, `{"rows":41}`)
	runner := httpRunner(t, server.URL, readReportTool())

	// Act
	result, err := call(t, runner, "read_report", `{"reportId":"r-2026-09","format":"csv"}`)

	// Assert
	if err != nil {
		t.Fatalf("a declared call failed: %v", err)
	}
	if result.IsError {
		t.Errorf("a 200 was reported as an error: %q", result.Content)
	}
	if result.Content != `{"rows":41}` {
		t.Errorf("content = %q; want the vendor's body", result.Content)
	}
	if seen.method != http.MethodGet {
		t.Errorf("method = %s; want GET", seen.method)
	}
	if seen.path != "/v1/reports/r-2026-09" {
		t.Errorf("path = %q; want the placeholder filled", seen.path)
	}
	if seen.query.Get("format") != "csv" {
		t.Errorf("query = %v; want format=csv", seen.query)
	}
	if seen.header.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q", seen.header.Get("Accept"))
	}
	// Vendors rate-limit and support on the user agent, so it says what we are.
	if !strings.HasPrefix(seen.header.Get("User-Agent"), "wingman-core/") {
		t.Errorf("User-Agent = %q; want this service named", seen.header.Get("User-Agent"))
	}
	if seen.header.Get("Authorization") != "" {
		t.Error("a service with no authEnv sent an Authorization header")
	}
}

func TestHTTPAPI_call_omitsAnOptionalParameterTheModelDidNotSupply(t *testing.T) {
	// Arrange
	// Sending format= empty would be a different request: many APIs read a present but
	// blank parameter as a filter matching nothing.
	server, seen := stubAPI(t, http.StatusOK, "ok")
	runner := httpRunner(t, server.URL, readReportTool())

	// Act
	if _, err := call(t, runner, "read_report", `{"reportId":"r-1"}`); err != nil {
		t.Fatalf("a declared call failed: %v", err)
	}

	// Assert
	if _, present := seen.query["format"]; present {
		t.Errorf("query = %v; want no format at all", seen.query)
	}
}

func TestHTTPAPI_call_escapesAValueTheModelChose(t *testing.T) {
	// Arrange
	// The id came from a model reading somebody's data. A bare ../ in it would otherwise
	// be a request to a different path than the operator declared — the traversal check
	// at load time only covers what the operator wrote.
	server, seen := stubAPI(t, http.StatusOK, "ok")
	runner := httpRunner(t, server.URL, readReportTool())

	// Act
	if _, err := call(t, runner, "read_report", `{"reportId":"../../admin"}`); err != nil {
		t.Fatalf("the call failed: %v", err)
	}

	// Assert
	if strings.Contains(seen.path, "..") && !strings.Contains(seen.path, "%2F") {
		t.Errorf("path = %q; want the separators escaped", seen.path)
	}
	if !strings.HasPrefix(seen.path, "/v1/reports/") {
		t.Errorf("path = %q; want it still under the declared prefix", seen.path)
	}
}

func TestHTTPAPI_call_formatsANumberOrABooleanTheModelSent(t *testing.T) {
	// Arrange
	// Every JSON number decodes as a float64, so an id of 41 arrives as 41.0 and must
	// not be sent that way. Models send an id as a number often enough that refusing
	// would be refusing a correct call.
	tool := readReportTool()
	tool.Parameters = append(tool.Parameters, HTTPParameter{Name: "draft", In: ParameterQuery})
	server, seen := stubAPI(t, http.StatusOK, "ok")
	runner := httpRunner(t, server.URL, tool)

	// Act
	if _, err := call(t, runner, "read_report", `{"reportId":41,"format":0.5,"draft":true}`); err != nil {
		t.Fatalf("the call failed: %v", err)
	}

	// Assert
	if seen.path != "/v1/reports/41" {
		t.Errorf("path = %q; want /v1/reports/41 rather than 41.0 or 4.1e+01", seen.path)
	}
	if seen.query.Get("format") != "0.5" {
		t.Errorf("format = %q; want 0.5", seen.query.Get("format"))
	}
	if seen.query.Get("draft") != "true" {
		t.Errorf("draft = %q; want true", seen.query.Get("draft"))
	}
}

func TestHTTPAPI_call_sendsADeclaredBodyAsJSON(t *testing.T) {
	// Arrange
	server, seen := stubAPI(t, http.StatusAccepted, `{"id":"job-1"}`)
	runner := httpRunner(t, server.URL, createReportTool())

	// Act
	result, err := call(t, runner, "create_report", `{"body":{"period":"2026-09","kind":"revenue"}}`)

	// Assert
	if err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if result.IsError {
		t.Errorf("a 202 was reported as an error: %q", result.Content)
	}
	if seen.method != http.MethodPost {
		t.Errorf("method = %s; want POST", seen.method)
	}
	if seen.header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", seen.header.Get("Content-Type"))
	}
	if !strings.Contains(seen.body, `"period":"2026-09"`) {
		t.Errorf("body = %q; want the model's object", seen.body)
	}
}

func TestHTTPAPI_call_theModelsMistakesComeBackAsResultsItCanCorrect(t *testing.T) {
	// Arrange
	// Each of these is a wrong call rather than a fault. It reaches the model as a
	// result, and the request is never sent — a GET with an empty path segment is a
	// list where a fetch was meant, and an ignored argument is a filter the model
	// believes it applied.
	server, seen := stubAPI(t, http.StatusOK, "ok")
	runner := httpRunner(t, server.URL, readReportTool(), createReportTool())

	cases := map[string]struct {
		tool      string
		arguments string
		mentions  string
	}{
		"a missing required argument":     {"read_report", `{}`, "reportId"},
		"an argument nobody declared":     {"read_report", `{"reportId":"r-1","filter":"all"}`, "filter"},
		"a blank path value":              {"read_report", `{"reportId":"   "}`, "reportId"},
		"an object where a value belongs": {"read_report", `{"reportId":{"id":1}}`, "reportId"},
		"an array where a value belongs":  {"read_report", `{"reportId":["r-1"]}`, "reportId"},
		"a null value":                    {"read_report", `{"reportId":null}`, "reportId"},
		"a missing body":                  {"create_report", `{}`, bodyArgument},
		"a body that is not an object":    {"create_report", `{"body":"period=2026-09"}`, bodyArgument},
	}

	// Act, Assert
	for label, tc := range cases {
		result, err := call(t, runner, tc.tool, tc.arguments)
		if err != nil {
			t.Errorf("%s ended the step instead of reaching the model: %v", label, err)
			continue
		}
		if !result.IsError {
			t.Errorf("%s was accepted: %q", label, result.Content)
		}
		if !strings.Contains(result.Content, tc.mentions) {
			t.Errorf("%s does not name %q: %q", label, tc.mentions, result.Content)
		}
	}
	if seen.calls != 0 {
		t.Errorf("the service was called %d times for calls that were never valid", seen.calls)
	}
}

func TestHTTPAPI_call_namesWhatTheEndpointDoesTakeWhenTheModelGuessesWrong(t *testing.T) {
	// Arrange
	// A model that sent `id` where the parameter is `reportId` needs to be told the
	// name, not only that it was wrong. Dropping the argument would have returned the
	// unfiltered list, which reads like an answer.
	server, _ := stubAPI(t, http.StatusOK, "ok")
	runner := httpRunner(t, server.URL, readReportTool())

	// Act
	result, err := call(t, runner, "read_report", `{"id":"r-1"}`)

	// Assert
	if err != nil {
		t.Fatalf("the call ended the step: %v", err)
	}
	if !result.IsError {
		t.Fatalf("an undeclared argument was accepted: %q", result.Content)
	}
	for _, want := range []string{"id", "reportId", "format"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("the message does not mention %q: %q", want, result.Content)
		}
	}
}

func TestHTTPAPI_call_saysSoWhenAnEndpointTakesNothingAtAll(t *testing.T) {
	// Arrange
	// A parameterless endpoint that is told "it takes " would leave the model guessing
	// at a name it should stop sending entirely.
	server, seen := stubAPI(t, http.StatusOK, "[]")
	runner := httpRunner(t, server.URL, HTTPTool{
		Name:        "list_reports",
		Description: "List every report.",
		Method:      http.MethodGet,
		Path:        "/v1/reports",
	})

	// Act
	result, err := call(t, runner, "list_reports", `{"page":"2"}`)

	// Assert
	if err != nil {
		t.Fatalf("the call ended the step: %v", err)
	}
	if !result.IsError {
		t.Fatalf("an argument was accepted by an endpoint that declares none: %q", result.Content)
	}
	if !strings.Contains(result.Content, "no arguments") {
		t.Errorf("the message does not say the endpoint takes nothing: %q", result.Content)
	}
	if seen.calls != 0 {
		t.Errorf("the request was sent anyway (%d calls)", seen.calls)
	}
}

func TestHTTPAPI_call_addsTheOperatorsCredentialFromTheEnvironment(t *testing.T) {
	// Arrange
	// The credential is never in the config file — only the name of the variable
	// holding it.
	server, seen := stubAPI(t, http.StatusOK, "ok")
	bare := ""
	basic := "Basic"

	cases := map[string]struct {
		service HTTPService
		header  string
		want    string
	}{
		"a bearer token by default": {
			service: HTTPService{AuthEnv: "REPORTS_TOKEN"},
			header:  "Authorization",
			want:    "Bearer operator-token",
		},
		"an API key in its own header": {
			service: HTTPService{AuthEnv: "REPORTS_TOKEN", AuthHeader: "X-Api-Key", AuthPrefix: &bare},
			header:  "X-Api-Key",
			want:    "operator-token",
		},
		"a scheme the vendor chose": {
			service: HTTPService{AuthEnv: "REPORTS_TOKEN", AuthHeader: "Authorization", AuthPrefix: &basic},
			header:  "Authorization",
			want:    "Basic operator-token",
		},
	}

	// Act, Assert
	for label, tc := range cases {
		service := tc.service
		service.Name = "reports"
		service.BaseURL = server.URL
		service.Tools = []HTTPTool{readReportTool()}
		if service.AuthHeader == "" {
			service.AuthHeader = "Authorization"
		}
		runner := NewHTTPAPI(service, "test", fakeEnv(map[string]string{"REPORTS_TOKEN": "operator-token"}))

		if _, err := call(t, runner, "read_report", `{"reportId":"r-1"}`); err != nil {
			t.Errorf("%s failed: %v", label, err)
			continue
		}
		if got := seen.header.Get(tc.header); got != tc.want {
			t.Errorf("%s sent %s: %q; want %q", label, tc.header, got, tc.want)
		}
	}
}

func TestHTTPAPI_call_refusesToCallAnAuthenticatedServiceAnonymously(t *testing.T) {
	// Arrange
	// Named and missing is a misconfiguration, not an anonymous API. Sending the
	// request would hand the model a 401 to reason about and leave the operator to work
	// backwards from it.
	server, seen := stubAPI(t, http.StatusOK, "ok")
	service := HTTPService{
		Name: "reports", BaseURL: server.URL, AuthEnv: "REPORTS_TOKEN",
		AuthHeader: "Authorization", Tools: []HTTPTool{readReportTool()},
	}

	cases := map[string]map[string]string{
		"unset": nil,
		"blank": {"REPORTS_TOKEN": "   "},
	}

	// Act, Assert
	for label, env := range cases {
		runner := NewHTTPAPI(service, "test", fakeEnv(env))
		_, err := call(t, runner, "read_report", `{"reportId":"r-1"}`)
		if err == nil {
			t.Errorf("a %s credential was accepted", label)
			continue
		}
		if !strings.Contains(err.Error(), "REPORTS_TOKEN") {
			t.Errorf("the %s error does not name the variable to set: %v", label, err)
		}
	}
	if seen.calls != 0 {
		t.Errorf("the service was called %d times without its credential", seen.calls)
	}
}

func TestHTTPAPI_call_reportsAVendorFailureAsOutputTheModelCanSee(t *testing.T) {
	// Arrange
	// A 4xx or 5xx is the model's to act on: it asked for an invoice that does not
	// exist, or the vendor is down. The body is included because the vendor's own
	// message is the only thing that makes the failure correctable.
	cases := map[string]struct {
		status  int
		reply   string
		isError bool
		wants   []string
	}{
		"a not-found with a message": {http.StatusNotFound, `{"error":"report r-1 does not exist"}`, true,
			[]string{"404", "does not exist"}},
		"a validation error":     {http.StatusUnprocessableEntity, `field "period" must be a month`, true, []string{"422", "period"}},
		"a failure with no body": {http.StatusInternalServerError, "", true, []string{"500"}},
		"a success with no body": {http.StatusNoContent, "", false, []string{"204"}},
	}

	// Act, Assert
	for label, tc := range cases {
		server, _ := stubAPI(t, tc.status, tc.reply)
		runner := httpRunner(t, server.URL, readReportTool())

		result, err := call(t, runner, "read_report", `{"reportId":"r-1"}`)
		if err != nil {
			t.Errorf("%s ended the step instead of reaching the model: %v", label, err)
			continue
		}
		if result.IsError != tc.isError {
			t.Errorf("%s: isError = %v; want %v (%q)", label, result.IsError, tc.isError, result.Content)
		}
		for _, want := range tc.wants {
			if !strings.Contains(result.Content, want) {
				t.Errorf("%s does not mention %q: %q", label, want, result.Content)
			}
		}
	}
}

func TestHTTPAPI_call_boundsWhatItReadsFromAService(t *testing.T) {
	// Arrange
	// The registry truncates again before this reaches the model. This bound exists so
	// a vendor streaming without end cannot exhaust memory before truncation gets the
	// chance.
	server, _ := stubAPI(t, http.StatusOK, strings.Repeat("x", maxHTTPResponseBytes+4_096))
	runner := httpRunner(t, server.URL, readReportTool())

	// Act
	result, err := call(t, runner, "read_report", `{"reportId":"r-1"}`)

	// Assert
	if err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if len(result.Content) > maxHTTPResponseBytes+64 {
		t.Errorf("kept %d bytes; the bound is %d", len(result.Content), maxHTTPResponseBytes)
	}
	if !strings.Contains(result.Content, "truncated") {
		t.Error("nothing in the output said it had been cut")
	}
}

func TestHTTPAPI_call_refusesToFollowARedirectToAnotherHost(t *testing.T) {
	// Arrange
	// net/http drops the Authorization header across hosts, but following the redirect
	// at all would still send the path, the query and whatever the model put in them to
	// a host the operator never named.
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		elsewhere.Add(1)
	}))
	t.Cleanup(other.Close)

	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/reports/r-1", http.StatusFound)
	}))
	t.Cleanup(moved.Close)

	runner := httpRunner(t, moved.URL, readReportTool())

	// Act
	_, err := call(t, runner, "read_report", `{"reportId":"r-1"}`)

	// Assert
	if err == nil {
		t.Fatal("a redirect to another host was followed")
	}
	if hits := elsewhere.Load(); hits != 0 {
		t.Errorf("the other host was called %d times", hits)
	}
}

func TestHTTPAPI_call_followsAVendorsOwnRedirect(t *testing.T) {
	// Arrange
	// Staying on the declared host is the rule; refusing every redirect would break an
	// API that moved a path or normalises a trailing slash.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			http.Redirect(w, r, "/v2/reports/r-1", http.StatusMovedPermanently)
			return
		}
		_, _ = w.Write([]byte(`{"rows":7}`))
	}))
	t.Cleanup(server.Close)
	runner := httpRunner(t, server.URL, readReportTool())

	// Act
	result, err := call(t, runner, "read_report", `{"reportId":"r-1"}`)

	// Assert
	if err != nil {
		t.Fatalf("a same-host redirect was refused: %v", err)
	}
	if result.Content != `{"rows":7}` {
		t.Errorf("content = %q; want the body from the new path", result.Content)
	}
}

func TestHTTPAPI_call_stopsAServiceThatKeepsRedirecting(t *testing.T) {
	// Arrange
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("/v1/reports/r-%d", hits.Add(1)), http.StatusFound)
	}))
	t.Cleanup(server.Close)
	runner := httpRunner(t, server.URL, readReportTool())

	// Act
	_, err := call(t, runner, "read_report", `{"reportId":"r-1"}`)

	// Assert
	if err == nil {
		t.Fatal("an endless redirect chain was followed to the end")
	}
	if hits.Load() > maxHTTPRedirects+1 {
		t.Errorf("followed %d redirects; the bound is %d", hits.Load(), maxHTTPRedirects)
	}
}

func TestHTTPAPI_call_stopsWhenTheStepIsOutOfTime(t *testing.T) {
	// Arrange
	// The step's deadline wins over the service's: WithTimeout takes whichever is
	// sooner, so a slow vendor cannot outlast the run that called it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	runner := httpRunner(t, server.URL, readReportTool())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Act
	_, err := runner.Call(ctx, Invocation{Name: "read_report", Input: []byte(`{"reportId":"r-1"}`)})

	// Assert
	if err == nil {
		t.Fatal("a call that ran past the step's deadline returned successfully")
	}
}

func TestHTTPAPI_call_endsTheStepWhenAServiceHangsUpMidResponse(t *testing.T) {
	// Arrange
	// A body cut short is not a short body. Handing the model half a JSON document as
	// though it were the answer is worse than failing, because it would be summarised as
	// fact and the truncation would never be visible again.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// A Content-Length longer than what is written, then the connection closed: the
		// client reads a header promising 64 bytes and gets four.
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 64\r\n\r\nhalf"))
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)
	runner := httpRunner(t, server.URL, readReportTool())

	// Act
	result, err := call(t, runner, "read_report", `{"reportId":"r-1"}`)

	// Assert
	if err == nil {
		t.Fatalf("a truncated response was returned as a result: %q", result.Content)
	}
	if strings.Contains(err.Error(), "half") {
		t.Errorf("the partial body was put in the error: %v", err)
	}
}

func TestHTTPAPI_call_doesNotPutTheBaseURLInAnError(t *testing.T) {
	// Arrange
	// The loader refuses credentials in a baseUrl, so this is defence in depth: net/url
	// and net/http put the whole URL in their errors, and a run's transcript is stored
	// and rendered.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := dead.URL
	dead.Close()

	secret := "hunter2"
	baseURL := strings.Replace(address, "http://", "http://wingman:"+secret+"@", 1)
	runner := httpRunner(t, baseURL, readReportTool())

	// Act
	_, err := call(t, runner, "read_report", `{"reportId":"r-1"}`)

	// Assert
	if err == nil {
		t.Fatal("a call to a closed port succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error carries the credential from the base URL: %v", err)
	}
}

func TestHTTPAPI_call_refusesAToolThatIsNotItsOwn(t *testing.T) {
	// Arrange
	// Reaching here means the registry routed a name to the wrong runner, which is a
	// fault in the loop rather than a mistake the model can fix.
	runner := httpRunner(t, "https://reports.internal", readReportTool())

	// Act
	_, err := call(t, runner, "get_invoice", `{"id":"inv-1"}`)

	// Assert
	if err == nil {
		t.Fatal("a runner answered for a tool it never declared")
	}
	if !strings.Contains(err.Error(), "get_invoice") {
		t.Errorf("the error does not name the tool: %v", err)
	}
}

func TestHTTPAPI_call_refusesArgumentsThatAreNotJSON(t *testing.T) {
	// Arrange
	// Truncated JSON is a broken reply from the provider, not a wrong call: there is no
	// argument to name back to the model.
	runner := httpRunner(t, "https://reports.internal", readReportTool())

	// Act
	_, err := call(t, runner, "read_report", `{"reportId":`)

	// Assert
	if err == nil {
		t.Fatal("arguments that are not JSON were accepted")
	}
}

func TestHTTPAPI_aServiceBuiltWithoutATimeoutGetsTheDefault(t *testing.T) {
	// Arrange
	// LoadHTTPServices always sets one. A service built in code might not, and zero
	// would mean an already-expired deadline rather than an unlimited one — every call
	// would fail before it was sent.
	runner := httpRunner(t, "https://reports.internal", readReportTool())

	// Act, Assert
	if runner.service.Timeout != defaultHTTPTimeout {
		t.Errorf("timeout = %s; want the default %s", runner.service.Timeout, defaultHTTPTimeout)
	}
}

func TestHTTPAPI_label_namesTheServiceAnOperatorConfigured(t *testing.T) {
	// Arrange
	runner := httpRunner(t, "https://reports.internal", readReportTool())

	// Act, Assert
	// Specific, not "http": two declared APIs failing look identical in an audit trail
	// otherwise.
	if got := runner.Label(); got != "http:reports" {
		t.Errorf("Label() = %q; want http:reports", got)
	}
}

func TestHTTPBody_refusesABodyItCannotEncode(t *testing.T) {
	// Arrange
	// Not reachable through Call, where the body is always JSON the provider sent. It is
	// here so that a later caller building arguments in Go gets a result the model can
	// read rather than a request with an empty body.
	declared := createReportTool()
	arguments := map[string]any{bodyArgument: map[string]any{"queue": make(chan int)}}

	// Act
	body, err := httpBody(declared, arguments)

	// Assert
	if err == nil {
		t.Fatalf("a body that cannot be encoded produced %q", body)
	}
	var wrong argumentError
	if !errors.As(err, &wrong) {
		t.Errorf("the failure ends the step instead of going back to the model: %v", err)
	}
}

func TestRedactURL_leavesAnErrorAloneWhenThereIsNothingToRedact(t *testing.T) {
	// Arrange
	failure := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")

	// Act, Assert
	if got := redactURL(nil, "https://reports.internal"); got != nil {
		t.Errorf("redactURL(nil, ...) = %v; want nil", got)
	}
	if got := redactURL(failure, ""); got != failure {
		t.Errorf("a service with no base URL had its error rewritten: %v", got)
	}
	// Returned as it stands rather than reconstructed, so a caller can still unwrap it
	// with errors.Is — the substitution is only made when there is a URL in the text.
	if got := redactURL(failure, "https://reports.internal"); got != failure {
		t.Errorf("an error that does not mention the base URL was rewritten: %v", got)
	}
}
