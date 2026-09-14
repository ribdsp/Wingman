package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeHTTPTools writes body as an http tool config in a temporary directory.
func writeHTTPTools(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "http-tools.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadHTTPServices_readsWhatTheOperatorDeclared(t *testing.T) {
	// Arrange
	path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal/api/
    authEnv: REPORTS_TOKEN
    timeout: 5s
    tools:
      - name: read_report
        description: Fetch one report by id.
        path: /v1/reports/{reportId}
        parameters:
          - name: reportId
            in: path
            description: The report's id.
          - name: format
            description: csv or json.
      - name: create_report
        description: Queue a new report.
        method: post
        path: /v1/reports
        bodyDescription: '{"period":"2026-09","kind":"revenue"}'
  - name: billing
    baseUrl: https://billing.internal
    tools:
      - name: get_invoice
        description: Fetch one invoice.
        path: /v1/invoices/{id}
        parameters:
          - name: id
            in: path
`)

	// Act
	services, err := LoadHTTPServices(path)

	// Assert
	if err != nil {
		t.Fatalf("a valid config was refused: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("loaded %d services; want 2", len(services))
	}
	// Sorted, so a startup log and an error message list them the same way twice.
	if services[0].Name != "billing" || services[1].Name != "reports" {
		t.Fatalf("services are not sorted by name: %s, %s", services[0].Name, services[1].Name)
	}

	billing := services[0]
	if !billing.Enabled {
		t.Error("a service that said nothing about enabled was loaded disabled")
	}
	if billing.Timeout != defaultHTTPTimeout {
		t.Errorf("timeout = %s; want the default %s", billing.Timeout, defaultHTTPTimeout)
	}
	if billing.AuthHeader != "Authorization" {
		t.Errorf("authHeader = %q; want Authorization by default", billing.AuthHeader)
	}
	if billing.Tools[0].Method != "GET" {
		t.Errorf("method = %q; want GET by default", billing.Tools[0].Method)
	}
	if !billing.Tools[0].Parameters[0].Required {
		t.Error("a path parameter was loaded optional")
	}

	reports := services[1]
	// The trailing slash is trimmed, because a path always starts with one and
	// //v1/reports is a different request.
	if reports.BaseURL != "https://reports.internal/api" {
		t.Errorf("baseUrl = %q; want the trailing slash trimmed", reports.BaseURL)
	}
	if reports.Timeout != 5*time.Second {
		t.Errorf("timeout = %s; want 5s", reports.Timeout)
	}
	if reports.AuthEnv != "REPORTS_TOKEN" {
		t.Errorf("authEnv = %q", reports.AuthEnv)
	}
	if len(reports.Tools) != 2 {
		t.Fatalf("reports has %d tools; want 2", len(reports.Tools))
	}
	if reports.Tools[1].Method != "POST" {
		t.Errorf("method = %q; want a lowercase declaration upcased", reports.Tools[1].Method)
	}
	if reports.Tools[1].BodyDescription == "" {
		t.Error("the declared body description was dropped")
	}

	format := reports.Tools[0].Parameters[1]
	if format.In != ParameterQuery {
		t.Errorf("in = %q; want %q by default", format.In, ParameterQuery)
	}
	if format.Required {
		t.Error("a query parameter that said nothing about required was loaded required")
	}
}

func TestLoadHTTPServices_refusesAFileThatIsNotThere(t *testing.T) {
	// Arrange
	missing := filepath.Join(t.TempDir(), "nothing.yaml")

	// Act
	_, err := LoadHTTPServices(missing)

	// Assert
	// Loudly, not as an empty list. An operator who declared an API and mistyped the
	// path should not get a service that starts with the tools silently absent.
	if err == nil {
		t.Fatal("a missing config file was accepted")
	}
}

func TestLoadHTTPServices_acceptsAFileWithNoServicesInIt(t *testing.T) {
	// Arrange
	// The shipped default. No HTTP API is reachable until an operator writes one down,
	// and that has to be a valid state rather than a startup failure.
	cases := map[string]string{
		"an empty list": "services: []\n",
		"comments only": "# nothing declared yet\n",
	}

	// Act, Assert
	for label, body := range cases {
		services, err := LoadHTTPServices(writeHTTPTools(t, body))
		if err != nil {
			t.Errorf("%s was refused: %v", label, err)
			continue
		}
		if len(services) != 0 {
			t.Errorf("%s produced %d services", label, len(services))
		}
	}
}

func TestLoadHTTPServices_refusesAFieldNobodyDeclared(t *testing.T) {
	// Arrange
	// base_url instead of baseUrl. Ignoring it would leave the service with no base
	// URL and the operator reading a file that says otherwise.
	path := writeHTTPTools(t, `
services:
  - name: reports
    base_url: https://reports.internal
    tools:
      - name: read_report
        description: Fetch one report.
        path: /v1/reports
`)

	// Act
	_, err := LoadHTTPServices(path)

	// Assert
	if err == nil {
		t.Fatal("an unknown field was accepted")
	}
	if !strings.Contains(err.Error(), "base_url") {
		t.Errorf("the error does not name the field: %v", err)
	}
}

func TestLoadHTTPServices_reportsEveryProblemAtOnce(t *testing.T) {
	// Arrange
	// Five services, five different faults. An operator fixing a config one startup at
	// a time is an operator who restarts five times.
	path := writeHTTPTools(t, `
services:
  - name: ""
    baseUrl: https://a.internal
  - name: Billing
    baseUrl: https://billing.internal
    tools:
      - name: get_invoice
        description: Fetch one invoice.
        path: /v1/invoices
  - name: reports
    tools:
      - name: read_report
        path: /v1/reports
  - name: reports
    baseUrl: https://reports.internal
    tools:
      - name: read_report_again
        description: Fetch one report.
        path: /v1/reports
  - name: warehouse
    baseUrl: https://warehouse.internal
    tools: []
`)

	// Act
	_, err := LoadHTTPServices(path)

	// Assert
	if err == nil {
		t.Fatal("five broken services were accepted")
	}
	for _, want := range []string{
		"name is required",
		`"Billing"`,
		"baseUrl is required",
		"description is required",
		"declared twice",
		"declares no tools",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}

func TestLoadHTTPServices_refusesABaseURLItCannotDialAndDoesNotEchoIt(t *testing.T) {
	// Arrange
	// A base URL is the one part of this file that can carry a credential — an
	// operator pasting a signed URL from a vendor's console does it by accident. So the
	// value never appears in the message, only the field.
	secret := "s3cr3t-signature"
	cases := map[string]string{
		"a relative URL":   "reports.internal/api",
		"a file URL":       "file:///etc/passwd",
		"no host":          "https://",
		"a query string":   "https://reports.internal/api?key=" + secret,
		"a fragment":       "https://reports.internal/api#" + secret,
		"a signed ftp URL": "ftp://reports.internal/?token=" + secret,
		// An embedded credential is a secret in a config file, which is the one thing
		// this file exists not to hold. Basic auth is expressible as authEnv with
		// authPrefix: Basic.
		"embedded credentials": "https://wingman:" + secret + "@reports.internal",
	}

	// Act, Assert
	for label, baseURL := range cases {
		path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: "`+baseURL+`"
    tools:
      - name: read_report
        description: Fetch one report.
        path: /v1/reports
`)
		_, err := LoadHTTPServices(path)
		if err == nil {
			t.Errorf("%s was accepted as a baseUrl", label)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s put the secret in the error: %v", label, err)
		}
	}
}

func TestLoadHTTPServices_refusesACredentialWrittenWhereAVariableNameBelongs(t *testing.T) {
	// Arrange
	secret := "sk-live-4f19"
	cases := map[string]string{
		"a value":          "REPORTS_TOKEN=" + secret,
		"a bare secret":    secret,
		"a shell variable": "$REPORTS_TOKEN",
	}

	// Act, Assert
	for label, authEnv := range cases {
		path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    authEnv: "`+authEnv+`"
    tools:
      - name: read_report
        description: Fetch one report.
        path: /v1/reports
`)
		_, err := LoadHTTPServices(path)
		if err == nil {
			t.Errorf("%s was accepted as an authEnv", label)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s put the secret in the error: %v", label, err)
		}
	}
}

func TestLoadHTTPServices_refusesAMethodItWillNotSend(t *testing.T) {
	// Arrange
	// DELETE is absent from the allowed set on purpose: a declared endpoint that
	// destroys a record is the most expensive thing this file can enable, and an
	// operator who wants one declares the vendor's POST-shaped equivalent.
	cases := []string{"delete", "options", "trace", "head", "connect", "sql"}

	// Act, Assert
	for _, method := range cases {
		path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    tools:
      - name: read_report
        description: Fetch one report.
        method: `+method+`
        path: /v1/reports
`)
		if _, err := LoadHTTPServices(path); err == nil {
			t.Errorf("method %q was accepted", method)
		}
	}
}

func TestLoadHTTPServices_refusesAPathThatCouldLeaveTheService(t *testing.T) {
	// Arrange
	// Each of these produces a request to somewhere other than the endpoint the
	// operator believes they declared, and it would go out carrying the service's
	// credential.
	cases := map[string]string{
		"no leading slash": "v1/reports",
		"another host":     "//evil.test/v1/reports",
		"a traversal":      "/v1/../../admin",
		"a query string":   "/v1/reports?admin=1",
		"a fragment":       "/v1/reports#top",
		"whitespace":       "/v1/re ports",
		"an absolute URL":  "https://evil.test/v1/reports",
		"nothing at all":   "",
	}

	// Act, Assert
	for label, declared := range cases {
		path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    tools:
      - name: read_report
        description: Fetch one report.
        path: "`+declared+`"
`)
		if _, err := LoadHTTPServices(path); err == nil {
			t.Errorf("%s (%q) was accepted as a path", label, declared)
		}
	}
}

func TestLoadHTTPServices_refusesAPlaceholderAndAParameterThatDisagree(t *testing.T) {
	// Arrange
	// Both directions, because neither mismatch fails loudly at call time: an
	// undeclared placeholder is sent literally as {id}, and a declared path parameter
	// with nowhere to go is dropped from a request that then means something else.
	cases := map[string]string{
		"an undeclared placeholder": `
        path: /v1/reports/{reportId}
        parameters:
          - name: format
`,
		"a parameter with nowhere to go": `
        path: /v1/reports
        parameters:
          - name: reportId
            in: path
`,
		"a placeholder spelled differently": `
        path: /v1/reports/{report_id}
        parameters:
          - name: reportId
            in: path
`,
	}

	// Act, Assert
	for label, tail := range cases {
		path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    tools:
      - name: read_report
        description: Fetch one report.`+tail)
		if _, err := LoadHTTPServices(path); err == nil {
			t.Errorf("%s was accepted", label)
		}
	}
}

func TestLoadHTTPServices_refusesAParameterItCannotSendSafely(t *testing.T) {
	// Arrange
	cases := map[string]string{
		"an optional path parameter": `
          - name: reportId
            in: path
            required: false
`,
		"a parameter in a place that does not exist": `
          - name: reportId
            in: path
          - name: token
            in: header
`,
		"a nameless parameter": `
          - name: reportId
            in: path
          - name: ""
`,
		"a name that is not an identifier": `
          - name: reportId
            in: path
          - name: "1st-page"
`,
		"the same parameter twice": `
          - name: reportId
            in: path
          - name: format
          - name: format
`,
	}

	// Act, Assert
	for label, parameters := range cases {
		path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    tools:
      - name: read_report
        description: Fetch one report.
        path: /v1/reports/{reportId}
        parameters:`+parameters)
		if _, err := LoadHTTPServices(path); err == nil {
			t.Errorf("%s was accepted", label)
		}
	}
}

func TestLoadHTTPServices_refusesABodyOnAGet(t *testing.T) {
	// Arrange
	// Dropping it silently would leave the operator believing the filter they
	// described is being sent, and the vendor logging a request without it.
	path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    tools:
      - name: read_report
        description: Fetch one report.
        path: /v1/reports
        bodyDescription: '{"period":"2026-09"}'
`)

	// Act
	_, err := LoadHTTPServices(path)

	// Assert
	if err == nil {
		t.Fatal("a GET with a body was accepted")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

func TestLoadHTTPServices_refusesTwoServicesSharingAToolName(t *testing.T) {
	// Arrange
	// A tool name is a grant. Two services offering get_invoice would share one line
	// in tools.yaml, so an operator could not permit the read-only one without
	// permitting the other.
	path := writeHTTPTools(t, `
services:
  - name: billing
    baseUrl: https://billing.internal
    tools:
      - name: get_invoice
        description: Fetch one invoice.
        path: /v1/invoices
  - name: legacy-billing
    baseUrl: https://legacy.internal
    tools:
      - name: get_invoice
        description: Fetch one invoice from the old system.
        path: /invoices
`)

	// Act
	_, err := LoadHTTPServices(path)

	// Assert
	if err == nil {
		t.Fatal("two services claiming one tool name were accepted")
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("the error does not name the service that already has it: %v", err)
	}
}

func TestLoadHTTPServices_refusesAToolNameAProviderWouldReject(t *testing.T) {
	// Arrange
	// Both vendors reject the whole request over one bad tool name, so this would break
	// every call the run makes rather than only the one that uses it.
	cases := []string{"read report", "reports.read", "read/report", strings.Repeat("x", 65)}

	// Act, Assert
	for _, name := range cases {
		path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    tools:
      - name: "`+name+`"
        description: Fetch one report.
        path: /v1/reports
`)
		if _, err := LoadHTTPServices(path); err == nil {
			t.Errorf("tool name %q was accepted", name)
		}
	}
}

func TestLoadHTTPServices_refusesAToolWithNoName(t *testing.T) {
	// Arrange
	// There is nothing to grant and nothing to offer, and a nameless entry is usually a
	// half-finished endpoint rather than one the operator meant to leave out.
	path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    tools:
      - description: Fetch one report.
        path: /v1/reports
`)

	// Act
	_, err := LoadHTTPServices(path)

	// Assert
	if err == nil {
		t.Fatal("a nameless tool was accepted")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

func TestLoadHTTPServices_refusesATimeoutOutsideItsBounds(t *testing.T) {
	// Arrange
	// Zero is the dangerous one: net/http reads it as no limit, so an unresponsive
	// vendor would hold a run's worker open until the process was restarted.
	cases := []string{"0s", "500ms", "10m", "soon"}

	// Act, Assert
	for _, timeout := range cases {
		path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    timeout: `+timeout+`
    tools:
      - name: read_report
        description: Fetch one report.
        path: /v1/reports
`)
		if _, err := LoadHTTPServices(path); err == nil {
			t.Errorf("timeout %q was accepted", timeout)
		}
	}
}

func TestLoadHTTPServices_refusesAnAuthHeaderThatIsNotOne(t *testing.T) {
	// Arrange
	// A header name with a colon or a newline in it is header injection, and net/http
	// would refuse the request at a point where the error names nothing useful.
	cases := []string{"X-Api-Key: leaked", "X Api Key"}

	// Act, Assert
	for _, header := range cases {
		path := writeHTTPTools(t, `
services:
  - name: reports
    baseUrl: https://reports.internal
    authEnv: REPORTS_TOKEN
    authHeader: "`+header+`"
    tools:
      - name: read_report
        description: Fetch one report.
        path: /v1/reports
`)
		if _, err := LoadHTTPServices(path); err == nil {
			t.Errorf("authHeader %q was accepted", header)
		}
	}
}

func TestLoadHTTPServices_keepsADisabledServiceSoItCanBeToldApartFromATypo(t *testing.T) {
	// Arrange
	// enabled: false is a decision. Dropping it at load time would make a switched-off
	// service indistinguishable from one whose name is misspelled in tools.yaml.
	path := writeHTTPTools(t, `
services:
  - name: billing
    baseUrl: https://billing.internal
    enabled: false
    tools:
      - name: get_invoice
        description: Fetch one invoice.
        path: /v1/invoices
  - name: reports
    baseUrl: https://reports.internal
    tools:
      - name: read_report
        description: Fetch one report.
        path: /v1/reports
`)

	// Act
	services, err := LoadHTTPServices(path)

	// Assert
	if err != nil {
		t.Fatalf("a valid config was refused: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("loaded %d services; want both", len(services))
	}
	if services[0].Enabled {
		t.Error("the disabled service was loaded enabled")
	}

	enabled := EnabledHTTPServices(services)
	if len(enabled) != 1 || enabled[0].Name != "reports" {
		t.Errorf("enabled services = %+v; want reports alone", enabled)
	}
}
