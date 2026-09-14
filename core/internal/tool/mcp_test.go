package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeMCP puts an mcp config on disk and returns its path.
func writeMCP(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write mcp config: %v", err)
	}
	return path
}

func TestLoadMCPServers_readsWhatTheOperatorDeclared(t *testing.T) {
	// Arrange
	path := writeMCP(t, `
servers:
  - name: reports
    description: The finance team's report server
    command: npx
    args: ["-y", "@acme/reports-mcp"]
    passEnv: ["REPORTS_TOKEN"]
  - name: billing
    transport: http
    url: https://mcp.billing.internal/mcp
    authEnv: BILLING_MCP_TOKEN
`)

	// Act
	servers, err := LoadMCPServers(path)

	// Assert
	if err != nil {
		t.Fatalf("a valid mcp config was refused: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("loaded %d servers; want 2", len(servers))
	}
	// Sorted, so the qualified tool names a run is offered do not reorder between
	// restarts for no reason.
	if servers[0].Name != "billing" || servers[1].Name != "reports" {
		t.Fatalf("servers are not sorted by name: %s, %s", servers[0].Name, servers[1].Name)
	}

	reports := servers[1]
	// stdio by default: it is the transport every locally installed server uses, and
	// the one that needs no url.
	if reports.Transport != TransportStdio {
		t.Errorf("transport = %q; want stdio by default", reports.Transport)
	}
	if reports.Command != "npx" || strings.Join(reports.Args, " ") != "-y @acme/reports-mcp" {
		t.Errorf("command = %q %v", reports.Command, reports.Args)
	}
	if strings.Join(reports.PassEnv, ",") != "REPORTS_TOKEN" {
		t.Errorf("passEnv = %v", reports.PassEnv)
	}
	// Omitted means connect: a server an operator wrote down is meant to be reachable.
	if !reports.Enabled {
		t.Error("a server with no enabled field was loaded switched off")
	}

	billing := servers[0]
	if billing.Transport != TransportHTTP || billing.URL != "https://mcp.billing.internal/mcp" {
		t.Errorf("http server = %q %q", billing.Transport, billing.URL)
	}
	// The name of the variable, never the token. That is the whole point of authEnv
	// existing instead of a token field.
	if billing.AuthEnv != "BILLING_MCP_TOKEN" {
		t.Errorf("authEnv = %q", billing.AuthEnv)
	}
}

func TestLoadMCPServers_refusesAFileThatIsNotThere(t *testing.T) {
	// Arrange, Act
	_, err := LoadMCPServers(filepath.Join(t.TempDir(), "absent.yaml"))

	// Assert
	// "No servers configured" and "the file moved" are different situations. Reading
	// the second as the first means an agent quietly loses every tool it had.
	if err == nil {
		t.Fatal("a missing mcp config was read as an empty one")
	}
}

func TestLoadMCPServers_acceptsAFileWithNoServersInIt(t *testing.T) {
	// Arrange
	cases := map[string]string{
		"an explicit empty list": "servers: []\n",
		"comments only":          "# add servers here\n",
	}

	// Act, Assert
	for label, body := range cases {
		servers, err := LoadMCPServers(writeMCP(t, body))
		if err != nil {
			t.Errorf("%s was refused: %v", label, err)
			continue
		}
		if len(servers) != 0 {
			t.Errorf("%s loaded %d servers", label, len(servers))
		}
	}
}

func TestLoadMCPServers_refusesAFieldNobodyDeclared(t *testing.T) {
	// Arrange
	// `env` rather than `passEnv`. Ignoring it would start the server without the
	// variable it needs, and the failure would look like the server being broken.
	path := writeMCP(t, "servers:\n  - name: reports\n    command: npx\n    env: [\"REPORTS_TOKEN\"]\n")

	// Act
	_, err := LoadMCPServers(path)

	// Assert
	if err == nil {
		t.Fatal("a misspelled field was accepted")
	}
}

func TestLoadMCPServers_reportsEveryProblemAtOnce(t *testing.T) {
	// Arrange
	path := writeMCP(t, `
servers:
  - name: ""
    command: npx
  - name: Reports
    command: npx
  - name: no-command
  - name: reports
    command: npx
  - name: reports
    command: npx
  - name: mystery
    transport: grpc
    command: npx
`)

	// Act
	_, err := LoadMCPServers(path)

	// Assert
	if err == nil {
		t.Fatal("a config with six problems was accepted")
	}
	message := err.Error()
	for _, want := range []string{
		"name is required",
		`"Reports"`,
		"command is required",
		"declared twice",
		`"grpc"`,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the report does not mention %q:\n%s", want, message)
		}
	}
}

func TestLoadMCPServers_refusesANameThatLeavesNoRoomForItsToolNames(t *testing.T) {
	// Arrange
	// A server name is spent twice: once in the config and once inside every qualified
	// tool name it produces. Both providers cut off at 64 characters and reject the
	// whole request, so a long server name breaks every call the run makes.
	cases := map[string]string{
		"upper case":     "Reports",
		"a dot":          "reports.finance",
		"a space":        "reports server",
		"a leading dash": "-reports",
		"too long":       strings.Repeat("r", maxServerNameLength+1),
	}

	// Act, Assert
	for label, name := range cases {
		body := "servers:\n  - name: \"" + name + "\"\n    command: npx\n"
		if _, err := LoadMCPServers(writeMCP(t, body)); err == nil {
			t.Errorf("%s was accepted as a server name", label)
		}
	}
}

func TestQualifiedName_fitsInsideWhatTheProvidersAllow(t *testing.T) {
	// Arrange
	// The arithmetic behind maxServerNameLength, written down so a later change to
	// either constant fails here rather than on the wire.
	server := strings.Repeat("a", maxServerNameLength)
	tool := strings.Repeat("b", 64-len(qualifiedPrefix)-maxServerNameLength-len(qualifiedSeparator))

	// Act
	qualified := QualifiedName(server, tool)

	// Assert
	if len(qualified) != 64 {
		t.Errorf("a server at the limit produces a %d-character tool name", len(qualified))
	}
	if !ValidName(qualified) {
		t.Errorf("the providers would reject %q", qualified)
	}
}

func TestLoadMCPServers_refusesFieldsThatBelongToTheOtherTransport(t *testing.T) {
	// Arrange
	// Ignored fields are the dangerous kind. A stdio entry with a url is an operator
	// who believes they configured a remote server; running the local command anyway
	// starts a process they have stopped thinking about.
	cases := map[string]string{
		"a stdio server with a url": "servers:\n  - name: reports\n    command: npx\n    url: https://example.test/mcp\n",
		"a stdio server with authEnv": "servers:\n  - name: reports\n    command: npx\n" +
			"    authEnv: REPORTS_TOKEN\n",
		"an http server with a command": "servers:\n  - name: reports\n    transport: http\n" +
			"    url: https://example.test/mcp\n    command: npx\n",
		"an http server with args": "servers:\n  - name: reports\n    transport: http\n" +
			"    url: https://example.test/mcp\n    args: [\"-y\"]\n",
		"an http server with passEnv": "servers:\n  - name: reports\n    transport: http\n" +
			"    url: https://example.test/mcp\n    passEnv: [\"REPORTS_TOKEN\"]\n",
		"an http server with no url": "servers:\n  - name: reports\n    transport: http\n",
	}

	// Act, Assert
	for label, body := range cases {
		if _, err := LoadMCPServers(writeMCP(t, body)); err == nil {
			t.Errorf("%s was accepted", label)
		}
	}
}

func TestLoadMCPServers_refusesAURLItCannotDialAndDoesNotEchoIt(t *testing.T) {
	// Arrange
	// An operator who put a token in the query string has already made one mistake.
	// Naming the url in a startup log would put it in the one place it must not be.
	secret := "s3cr3t-token-value"
	cases := map[string]string{
		"relative":       "/mcp?token=" + secret,
		"the wrong sche": "ftp://mcp.example.test/mcp?token=" + secret,
		"no host":        "https:///mcp?token=" + secret,
	}

	// Act, Assert
	for label, raw := range cases {
		_, err := LoadMCPServers(writeMCP(t,
			"servers:\n  - name: reports\n    transport: http\n    url: \""+raw+"\"\n"))
		if err == nil {
			t.Errorf("%s url was accepted", label)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s url leaked into the error: %v", label, err)
		}
	}
}

func TestLoadMCPServers_refusesAValueWhereAVariableNameBelongs(t *testing.T) {
	// Arrange
	// `TOKEN=abc123` is the mistake worth catching: it reads like it works, and it puts
	// the secret in the config file this design exists to keep secrets out of.
	secret := "abc123"
	cases := map[string]string{
		"a value in passEnv":   "servers:\n  - name: reports\n    command: npx\n    passEnv: [\"TOKEN=" + secret + "\"]\n",
		"a value in authEnv":   "servers:\n  - name: reports\n    transport: http\n    url: https://a.test/mcp\n    authEnv: \"TOKEN=" + secret + "\"\n",
		"an empty entry":       "servers:\n  - name: reports\n    command: npx\n    passEnv: [\"\"]\n",
		"not a variable name":  "servers:\n  - name: reports\n    command: npx\n    passEnv: [\"reports token\"]\n",
		"a leading digit":      "servers:\n  - name: reports\n    command: npx\n    passEnv: [\"1TOKEN\"]\n",
		"a shell substitution": "servers:\n  - name: reports\n    command: npx\n    passEnv: [\"$TOKEN\"]\n",
	}

	// Act, Assert
	for label, body := range cases {
		err := func() error {
			_, err := LoadMCPServers(writeMCP(t, body))
			return err
		}()
		if err == nil {
			t.Errorf("%s was accepted", label)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s put the secret in the error: %v", label, err)
		}
	}
}

func TestLoadMCPServers_keepsADisabledServerSoItCanBeToldApartFromATypo(t *testing.T) {
	// Arrange
	path := writeMCP(t, `
servers:
  - name: reports
    command: npx
  - name: billing
    command: billing-mcp
    enabled: false
`)

	// Act
	servers, err := LoadMCPServers(path)

	// Assert
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("loaded %d servers; a disabled one was dropped", len(servers))
	}

	// The filter is where the decision is made, not the loader, so the config still
	// answers "what has been declared" as well as "what will be connected".
	enabled := EnabledMCPServers(servers)
	if len(enabled) != 1 || enabled[0].Name != "reports" {
		t.Errorf("connecting to %v; want reports alone", enabled)
	}
}

func TestMCP_localName_onlyAnswersForItsOwnTools(t *testing.T) {
	// Arrange
	runner := NewMCP(MCPServer{Name: "reports", Transport: TransportStdio, Command: "npx"}, "test")

	cases := []struct {
		qualified string
		want      string
		mine      bool
	}{
		{qualified: QualifiedName("reports", "read_report"), want: "read_report", mine: true},
		// A tool whose own name contains the separator survives the round trip: only
		// the prefix is removed, so the rest is whatever the server called it.
		{qualified: "mcp__reports__read__report", want: "read__report", mine: true},
		// Another server's tool. Answering for it would send one server's arguments to
		// a different server's tool of the same name.
		{qualified: QualifiedName("billing", "read_report")},
		{qualified: "read_report"},
		{qualified: "mcp__reports__"},
		{qualified: ""},
	}

	// Act, Assert
	for _, c := range cases {
		got, mine := runner.localName(c.qualified)
		if mine != c.mine {
			t.Errorf("localName(%q) claimed = %v; want %v", c.qualified, mine, c.mine)
		}
		if got != c.want {
			t.Errorf("localName(%q) = %q; want %q", c.qualified, got, c.want)
		}
	}
}

func TestMCP_childEnv_handsAServerNothingItWasNotGiven(t *testing.T) {
	// Arrange
	// This process holds the model keys, the database DSN and every session secret in
	// its environment. A stdio MCP server is a child process installed from a registry;
	// os.Environ() would hand it all of them.
	environment := map[string]string{
		"PATH":                 "/usr/bin",
		"HOME":                 "/home/wingman",
		"TZ":                   "Asia/Jakarta",
		"REPORTS_TOKEN":        "granted-on-purpose",
		"ANTHROPIC_API_KEY":    "sk-ant-leaked",
		"OPENAI_API_KEY":       "sk-oai-leaked",
		"CORE_DATABASE_URL":    "postgres://wingman:hunter2@db/core",
		"CORE_SESSION_SECRET":  "session-signing-key",
		"WINGMAN_CORE_API_KEY": "bot-key",
	}
	runner := NewMCP(MCPServer{
		Name:      "reports",
		Transport: TransportStdio,
		Command:   "npx",
		PassEnv:   []string{"REPORTS_TOKEN"},
	}, "test")
	runner.lookupEnv = func(name string) (string, bool) {
		value, present := environment[name]
		return value, present
	}

	// Act
	env := runner.childEnv()

	// Assert
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/wingman", "TZ=Asia/Jakarta", "REPORTS_TOKEN=granted-on-purpose"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the child was not given %q, so it may not start at all:\n%s", want, joined)
		}
	}
	// The values, not the names: a variable renamed on the way through would still be
	// the same secret in the child's environment.
	for _, secret := range []string{"sk-ant-leaked", "sk-oai-leaked", "hunter2", "session-signing-key", "bot-key"} {
		if strings.Contains(joined, secret) {
			t.Errorf("a secret reached the child process: %q", secret)
		}
	}
	if len(env) != 4 {
		t.Errorf("the child was given %d variables; want the four that were allowed:\n%s", len(env), joined)
	}
}

func TestMCP_transport_refusesToConnectAnonymouslyToAServerThatNeedsAToken(t *testing.T) {
	// Arrange
	// Named and missing is a misconfiguration, not an anonymous server. Connecting
	// without the header would send the operator's requests unauthenticated and leave
	// them reading a 401 from somebody else's logs.
	runner := NewMCP(MCPServer{
		Name:      "billing",
		Transport: TransportHTTP,
		URL:       "https://mcp.billing.internal/mcp",
		AuthEnv:   "BILLING_MCP_TOKEN",
	}, "test")
	runner.lookupEnv = func(string) (string, bool) { return "", false }

	// Act
	_, err := runner.transport()

	// Assert
	if err == nil {
		t.Fatal("an http server with no token was connected to anyway")
	}
	if !strings.Contains(err.Error(), "BILLING_MCP_TOKEN") {
		t.Errorf("the error does not name the variable to set: %v", err)
	}

	// Present but blank is the same mistake with a different shape — an exported
	// variable somebody emptied.
	runner.lookupEnv = func(string) (string, bool) { return "   ", true }
	if _, err := runner.transport(); err == nil {
		t.Error("a blank token was accepted")
	}
}

func TestMCP_transport_buildsWhatEachTransportNeeds(t *testing.T) {
	// Arrange
	stdio := NewMCP(MCPServer{Name: "reports", Transport: TransportStdio, Command: "npx", Args: []string{"-y", "reports-mcp"}}, "test")
	stdio.lookupEnv = func(name string) (string, bool) {
		if name == "PATH" {
			return "/usr/bin", true
		}
		return "", false
	}

	// Act
	built, err := stdio.transport()

	// Assert
	if err != nil {
		t.Fatalf("a stdio transport was refused: %v", err)
	}
	command, ok := built.(*mcp.CommandTransport)
	if !ok {
		t.Fatalf("stdio produced a %T", built)
	}
	// Set rather than left nil: a nil Env means the child inherits this process's own,
	// which is the leak childEnv exists to prevent.
	if command.Command.Env == nil {
		t.Fatal("the child would inherit this process's environment")
	}
	if strings.Join(command.Command.Env, ",") != "PATH=/usr/bin" {
		t.Errorf("child env = %v", command.Command.Env)
	}

	// An http server with no authEnv is a legitimate anonymous one — a server on the
	// operator's own network that authenticates by being unreachable from outside it.
	anonymous := NewMCP(MCPServer{Name: "reports", Transport: TransportHTTP, URL: "http://127.0.0.1:9000/mcp"}, "test")
	built, err = anonymous.transport()
	if err != nil {
		t.Fatalf("an anonymous http transport was refused: %v", err)
	}
	streamable, ok := built.(*mcp.StreamableClientTransport)
	if !ok {
		t.Fatalf("http produced a %T", built)
	}
	if streamable.HTTPClient != nil {
		t.Error("a client with no token was given a bearer round tripper")
	}
	// Nothing in this client acts on a server-initiated message — the tool list is
	// snapshotted per run precisely so it cannot change underneath one — so holding a
	// stream open per server is a socket and a goroutine for nothing.
	if !streamable.DisableStandaloneSSE {
		t.Error("a standalone notification stream is held open for messages nothing reads")
	}

	// An operator's token turns into a header on every request rather than into part of
	// the url, so it stays out of access logs on the way there.
	authenticated := NewMCP(MCPServer{
		Name:      "billing",
		Transport: TransportHTTP,
		URL:       "https://mcp.billing.internal/mcp",
		AuthEnv:   "BILLING_MCP_TOKEN",
	}, "test")
	authenticated.lookupEnv = func(string) (string, bool) { return "operator-token", true }
	built, err = authenticated.transport()
	if err != nil {
		t.Fatalf("an authenticated http transport was refused: %v", err)
	}
	streamable, ok = built.(*mcp.StreamableClientTransport)
	if !ok {
		t.Fatalf("http produced a %T", built)
	}
	if streamable.HTTPClient == nil {
		t.Fatal("a server with a token would be called without one")
	}
	if _, ok := streamable.HTTPClient.Transport.(bearerTransport); !ok {
		t.Errorf("the token is carried by a %T", streamable.HTTPClient.Transport)
	}

	// An unknown transport is refused rather than defaulted, because defaulting to
	// stdio would run a command for an entry that named none.
	unknown := NewMCP(MCPServer{Name: "reports", Transport: "grpc"}, "test")
	if _, err := unknown.transport(); err == nil {
		t.Error("an unknown transport was accepted")
	}
}

func TestBearerTransport_addsTheTokenWithoutEditingTheCallersRequest(t *testing.T) {
	// Arrange
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	// Act
	response, err := bearerTransport{token: "operator-token"}.RoundTrip(request)

	// Assert
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if seen != "Bearer operator-token" {
		t.Errorf("the server saw Authorization %q", seen)
	}
	// The caller's request is untouched. A RoundTripper that edits the request it was
	// handed is a data race as soon as anything retries.
	if got := request.Header.Get("Authorization"); got != "" {
		t.Errorf("the caller's own request was modified: %q", got)
	}
}

func TestMCPSchema_givesTheProvidersAnObjectWhateverTheServerSent(t *testing.T) {
	// Arrange, Act, Assert
	// A tool that takes no arguments. Both providers require a schema, so nil has to
	// become the honest spelling of "none" rather than being passed through.
	empty, err := mcpSchema(nil)
	if err != nil {
		t.Fatalf("a tool with no arguments was refused: %v", err)
	}
	if empty["type"] != "object" {
		t.Errorf("nil became %v", empty)
	}
	if properties, ok := empty["properties"].(map[string]any); !ok || len(properties) != 0 {
		t.Errorf("nil produced properties %v", empty["properties"])
	}

	// What the SDK actually hands back today: already the map the providers want.
	declared := map[string]any{"type": "object", "properties": map[string]any{"period": map[string]any{"type": "string"}}}
	passed, err := mcpSchema(declared)
	if err != nil {
		t.Fatalf("a declared schema was refused: %v", err)
	}
	if passed["properties"] == nil {
		t.Error("a declared schema lost its properties")
	}

	// Anything else is re-marshalled rather than guessed at, so a future SDK version
	// that returns a typed schema still works.
	raw, err := mcpSchema(json.RawMessage(`{"type":"object","required":["period"]}`))
	if err != nil {
		t.Fatalf("a raw schema was refused: %v", err)
	}
	if raw["type"] != "object" {
		t.Errorf("a raw schema became %v", raw)
	}

	// A schema that is not an object at all. Reported rather than replaced with an
	// empty one: a tool whose arguments we cannot describe is a tool the model will
	// call wrongly, and the caller reports the whole server unavailable.
	for label, schema := range map[string]any{
		"an array":         json.RawMessage(`[1,2]`),
		"unencodable":      make(chan int),
		"invalid raw json": json.RawMessage(`{`),
	} {
		if _, err := mcpSchema(schema); err == nil {
			t.Errorf("%s was accepted as an input schema", label)
		}
	}
}

func TestMCPResult_flattensWhatAServerAnsweredIntoTextAModelCanRead(t *testing.T) {
	// Arrange, Act, Assert
	// Several text blocks, which is how servers return a list.
	joined := mcpResult(&mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "revenue: 41.2m"},
		&mcp.TextContent{Text: "orders: 1204"},
	}})
	if joined.Content != "revenue: 41.2m\norders: 1204" {
		t.Errorf("content = %q", joined.Content)
	}
	if joined.IsError {
		t.Error("a successful call was marked as an error")
	}

	// A tool that failed. Carried through rather than turned into a Go error: the model
	// is what corrects a bad tool call, and it cannot correct one it never sees.
	failed := mcpResult(&mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "no report for that period"}},
		IsError: true,
	})
	if !failed.IsError || failed.Content == "" {
		t.Errorf("a failed call became %+v", failed)
	}

	// Content we do not forward. Named rather than dropped, because silence reads to a
	// model as a tool that returned nothing, and it will call it again.
	image := mcpResult(&mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png"}}})
	if !strings.Contains(image.Content, "omitted") {
		t.Errorf("non-text content vanished silently: %q", image.Content)
	}

	// Structured output with no content blocks. It is already JSON on the wire, so it
	// goes on as JSON.
	structured := mcpResult(&mcp.CallToolResult{StructuredContent: map[string]any{"revenue": 41200000}})
	if !strings.Contains(structured.Content, "41200000") {
		t.Errorf("structured output was dropped: %q", structured.Content)
	}

	// With content present, the content is the answer: sending both would show the
	// model the same numbers twice and bill it for them.
	both := mcpResult(&mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "revenue: 41.2m"}},
		StructuredContent: map[string]any{"revenue": 41200000},
	})
	if both.Content != "revenue: 41.2m" {
		t.Errorf("content = %q; want the text alone", both.Content)
	}

	// A server that answered with nothing at all is not an error, just an empty result.
	if empty := mcpResult(&mcp.CallToolResult{}); empty.Content != "" || empty.IsError {
		t.Errorf("an empty result became %+v", empty)
	}
}

// serverTool is one tool an in-memory test server offers.
type serverTool struct {
	tool    *mcp.Tool
	handler mcp.ToolHandler
}

// textTool answers every call with reply, and records the arguments it was sent.
func textTool(name, reply string, recorded *json.RawMessage) serverTool {
	return serverTool{
		tool: &mcp.Tool{
			Name:        name,
			Description: name + " on the test server",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"period": map[string]any{"type": "string"}},
			},
		},
		handler: func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if recorded != nil {
				*recorded = request.Params.Arguments
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reply}}}, nil
		},
	}
}

// connectedMCP is a runner wired to a real MCP server over an in-memory transport.
//
// The transport is the only thing faked: the handshake, the tool listing, the
// pagination and the wire encoding are the SDK's own. A hand-written fake session
// would be a test of what this package believes the protocol does.
func connectedMCP(t *testing.T, name string, tools ...serverTool) *MCP {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	for _, offered := range tools {
		server.AddTool(offered.tool, offered.handler)
	}

	serverSide, clientSide := mcp.NewInMemoryTransports()
	// The server is connected first. A client handshaking against a transport nobody
	// is serving waits for a reply that never comes.
	serving, err := server.Connect(context.Background(), serverSide, nil)
	if err != nil {
		t.Fatalf("connect the test server: %v", err)
	}
	t.Cleanup(func() { _ = serving.Close() })

	runner := NewMCP(MCPServer{Name: name, Transport: TransportStdio, Command: "unused", Enabled: true}, "test")
	session, err := mcp.NewClient(runner.impl, nil).Connect(context.Background(), clientSide, nil)
	if err != nil {
		t.Fatalf("connect the client: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	// dial is the injection point: everything above it is the real client, and only
	// the process-starting is skipped.
	runner.session = session
	runner.dial = func(context.Context) (*mcp.ClientSession, error) { return session, nil }
	return runner
}

func TestMCP_tools_qualifiesEveryToolWithTheServerThatOffersIt(t *testing.T) {
	// Arrange
	// Two servers offering "search" would otherwise be one grant, and an operator
	// would have no way to permit the read-only one and not the other.
	runner := connectedMCP(t, "reports",
		textTool("read_report", "revenue: 41.2m", nil),
		textTool("list_reports", "daily, weekly", nil),
	)

	// Act
	definitions, err := runner.Tools(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("listing tools failed: %v", err)
	}
	if len(definitions) != 2 {
		t.Fatalf("listed %d tools; want 2", len(definitions))
	}

	for _, definition := range definitions {
		if !strings.HasPrefix(definition.Name, "mcp__reports__") {
			t.Errorf("tool %q is not qualified by its server", definition.Name)
		}
		// Checked here rather than discovered on the wire: a name outside the vendors'
		// pattern makes them reject the whole request, not just this tool.
		if !ValidName(definition.Name) {
			t.Errorf("the providers would reject %q", definition.Name)
		}
		if definition.Source != SourceMCP || definition.Origin != "reports" {
			t.Errorf("tool %q came from %q/%q", definition.Name, definition.Source, definition.Origin)
		}
		// The description is what the model decides with. A tool listed without one is
		// a tool it will either ignore or misuse.
		if definition.Description == "" {
			t.Errorf("tool %q was listed with no description", definition.Name)
		}
		if definition.InputSchema["type"] != "object" {
			t.Errorf("tool %q has schema %v", definition.Name, definition.InputSchema)
		}
	}
}

func TestMCP_call_sendsTheModelsArgumentsToTheServersOwnToolName(t *testing.T) {
	// Arrange
	var sent json.RawMessage
	runner := connectedMCP(t, "reports", textTool("read_report", "revenue: 41.2m", &sent))

	// Act
	result, err := runner.Call(context.Background(), Invocation{
		Name:      QualifiedName("reports", "read_report"),
		Input:     []byte(`{"period":"today"}`),
		Workspace: "/workspaces/u1",
	})

	// Assert
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if result.Content != "revenue: 41.2m" {
		t.Errorf("content = %q", result.Content)
	}
	if !strings.Contains(string(sent), `"period":"today"`) {
		t.Errorf("the server was sent %q", string(sent))
	}
	// The workspace is deliberately not sent. An MCP server has its own filesystem;
	// a path inside our sandbox is one it cannot see, and passing it invites a tool
	// that writes "into the workspace" somewhere else entirely.
	if strings.Contains(string(sent), "workspaces") {
		t.Errorf("the run's workspace path was sent to a server that cannot see it: %q", string(sent))
	}
}

func TestMCP_call_acceptsAToolThatTakesNoArguments(t *testing.T) {
	// Arrange
	// Some models send `{}`, some send nothing at all. A server asked for a tool with
	// no arguments must work either way.
	var sent json.RawMessage
	runner := connectedMCP(t, "reports", textTool("list_reports", "daily, weekly", &sent))

	// Act
	result, err := runner.Call(context.Background(), Invocation{
		Name: QualifiedName("reports", "list_reports"),
	})

	// Assert
	if err != nil {
		t.Fatalf("a call with no arguments failed: %v", err)
	}
	if result.Content != "daily, weekly" {
		t.Errorf("content = %q", result.Content)
	}
}

func TestMCP_call_refusesAToolBelongingToAnotherServer(t *testing.T) {
	// Arrange
	runner := connectedMCP(t, "reports", textTool("read_report", "revenue: 41.2m", nil))

	// Act
	_, err := runner.Call(context.Background(), Invocation{
		Name: QualifiedName("billing", "read_report"),
	})

	// Assert
	// A routing mistake, not a model one. Stripping the prefix blindly would send the
	// billing server's arguments to the reports server's tool of the same name.
	if err == nil {
		t.Fatal("a call for another server was forwarded")
	}
	if !strings.Contains(err.Error(), "reports") {
		t.Errorf("the error does not say which server refused: %v", err)
	}
}

func TestMCP_call_reportsAToolFailureAsOutputTheModelCanSee(t *testing.T) {
	// Arrange
	// A tool that ran and failed, which is a result and not a protocol error.
	failing := serverTool{
		tool: &mcp.Tool{
			Name:        "read_report",
			Description: "reads a report",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "no report for that period"}},
				IsError: true,
			}, nil
		},
	}
	runner := connectedMCP(t, "reports", failing)

	// Act
	result, err := runner.Call(context.Background(), Invocation{Name: QualifiedName("reports", "read_report")})

	// Assert
	if err != nil {
		t.Fatalf("a failed tool stopped the step: %v", err)
	}
	if !result.IsError {
		t.Error("a failed tool was reported as a success")
	}
	if result.Content != "no report for that period" {
		t.Errorf("content = %q; the model needs to read why it failed", result.Content)
	}
}

func TestMCP_call_dropsTheSessionWhenTheServerBreaksTheProtocol(t *testing.T) {
	// Arrange
	// A handler returning an error is an MCP protocol error, not a tool result. The
	// session is dropped so the next run reconnects rather than reusing a connection
	// this one has reason to doubt.
	broken := serverTool{
		tool: &mcp.Tool{
			Name:        "read_report",
			Description: "reads a report",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, context.Canceled
		},
	}
	runner := connectedMCP(t, "reports", broken)

	// Act
	_, err := runner.Call(context.Background(), Invocation{Name: QualifiedName("reports", "read_report")})

	// Assert
	if err == nil {
		t.Fatal("a protocol error was reported as tool output")
	}
	for _, want := range []string{"reports", "read_report"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is missing %q: %v", want, err)
		}
	}

	runner.mu.Lock()
	held := runner.session
	runner.mu.Unlock()
	if held != nil {
		t.Error("a session that failed a protocol call is still held for the next run")
	}
}

func TestMCP_call_refusesArgumentsThatAreNotAnObject(t *testing.T) {
	// Arrange
	runner := connectedMCP(t, "reports", textTool("read_report", "revenue: 41.2m", nil))

	// Act
	_, err := runner.Call(context.Background(), Invocation{
		Name:  QualifiedName("reports", "read_report"),
		Input: []byte(`["today"]`),
	})

	// Assert
	// Refused before the server is reached. MCP arguments are an object by definition,
	// so an array here means something built the call wrongly on our side.
	if err == nil {
		t.Fatal("a JSON array was forwarded as tool arguments")
	}
}

func TestMCP_tools_reportsTheWholeServerWhenListingFails(t *testing.T) {
	// Arrange
	// A dial that fails is what an unreachable server looks like from here. The
	// registry turns this into one unavailable source rather than a failed run.
	runner := NewMCP(MCPServer{Name: "reports", Transport: TransportStdio, Command: "npx"}, "test")
	runner.dial = func(context.Context) (*mcp.ClientSession, error) {
		return nil, context.DeadlineExceeded
	}

	// Act
	definitions, err := runner.Tools(context.Background())

	// Assert
	if err == nil {
		t.Fatal("an unreachable server listed tools successfully")
	}
	if definitions != nil {
		t.Errorf("an unreachable server offered %d tools", len(definitions))
	}

	// Call fails the same way rather than panicking on a nil session.
	if _, err := runner.Call(context.Background(), Invocation{Name: QualifiedName("reports", "read_report")}); err == nil {
		t.Error("a call to an unreachable server succeeded")
	}
}

func TestMCP_tools_stopsListingAServerThatOffersTooMany(t *testing.T) {
	// Arrange
	// A server is entitled to offer thousands of tools. A run is not entitled to spend
	// its token allowance being told about them, and both providers charge for the
	// whole list on every request.
	tools := make([]serverTool, 0, maxToolsPerServer+5)
	for i := 0; i < maxToolsPerServer+5; i++ {
		tools = append(tools, textTool(fmt.Sprintf("tool_%03d", i), "ok", nil))
	}
	runner := connectedMCP(t, "reports", tools...)

	// Act
	definitions, err := runner.Tools(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if len(definitions) != maxToolsPerServer {
		t.Errorf("listed %d tools; want the cap of %d", len(definitions), maxToolsPerServer)
	}
}

func TestMCP_connect_reportsAServerThatIsNotInstalled(t *testing.T) {
	// Arrange
	// The real dialer, not an injected one. A stdio server is a command an operator
	// wrote in a config file months ago; the failure an unreachable one produces has to
	// name the server, because "exec: not found" alone does not say which of six.
	runner := NewMCP(MCPServer{
		Name:      "reports",
		Transport: TransportStdio,
		Command:   "wingman-no-such-mcp-server-exists",
	}, "test")

	// Act
	session, err := runner.connect(context.Background())

	// Assert
	if err == nil {
		t.Fatal("a server that is not installed connected successfully")
	}
	if session != nil {
		t.Error("a failed connection returned a session")
	}
	if !strings.Contains(err.Error(), "reports") {
		t.Errorf("the error does not name the server: %v", err)
	}
	// Nothing is cached: the operator can install the server and the next run picks it
	// up without a restart.
	runner.mu.Lock()
	held := runner.session
	runner.mu.Unlock()
	if held != nil {
		t.Error("a failed connection was cached")
	}
}

func TestMCP_connect_opensOneSessionForEveryRunOnTheInstance(t *testing.T) {
	// Arrange
	// Runs execute concurrently and share one connection. A session per run would mean
	// a child process per run, and an `npx` server takes seconds to start.
	runner := connectedMCP(t, "reports", textTool("read_report", "revenue: 41.2m", nil))
	first := runner.session

	// Act
	second, err := runner.connect(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("reusing an open session failed: %v", err)
	}
	if second != first {
		t.Error("a second caller was given a different session")
	}
}

func TestMCP_tools_dropsASessionThatDiedUnderIt(t *testing.T) {
	// Arrange
	// An MCP server can be restarted, upgraded or killed by the operator's own
	// deployment at any moment. Holding the dead session would make every later run
	// fail the same way rather than reconnecting once.
	runner := connectedMCP(t, "reports", textTool("read_report", "revenue: 41.2m", nil))
	if err := runner.session.Close(); err != nil {
		t.Fatalf("closing the session for the test failed: %v", err)
	}

	// Act
	_, err := runner.Tools(context.Background())

	// Assert
	if err == nil {
		t.Fatal("listing tools over a dead session succeeded")
	}
	if !strings.Contains(err.Error(), "reports") {
		t.Errorf("the error does not name the server: %v", err)
	}

	runner.mu.Lock()
	held := runner.session
	runner.mu.Unlock()
	if held != nil {
		t.Error("a dead session is still held for the next run")
	}
}

func TestMCP_close_isSafeOnAServerThatWasNeverReached(t *testing.T) {
	// Arrange
	// Connecting is lazy, so shutdown runs against runners that never opened anything.
	// A close that failed here would turn an orderly shutdown into a logged error per
	// configured server.
	runner := NewMCP(MCPServer{Name: "reports", Transport: TransportStdio, Command: "npx"}, "test")

	// Act, Assert
	if err := runner.Close(); err != nil {
		t.Errorf("closing an unconnected server failed: %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Errorf("closing twice failed: %v", err)
	}
}

func TestMCP_label_namesTheServerAnOperatorConfigured(t *testing.T) {
	// Arrange, Act
	label := NewMCP(MCPServer{Name: "reports"}, "test").Label()

	// Assert
	// It is what an operator reads when a source is unavailable or two of them claim
	// one tool name, so it has to match the name in their config file.
	if label != "mcp:reports" {
		t.Errorf("label = %q; want mcp:reports", label)
	}
}
