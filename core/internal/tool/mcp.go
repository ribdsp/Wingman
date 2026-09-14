package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// qualifiedPrefix and qualifiedSeparator build the name a granted MCP tool is
	// known by: mcp__<server>__<tool>. Two servers offering "search" would otherwise
	// be the same grant, and the operator would have no way to permit one and not
	// the other. The shape is underscores rather than dots because both providers
	// reject a dot in a tool name.
	qualifiedPrefix    = "mcp__"
	qualifiedSeparator = "__"

	// mcpConnectTimeout bounds the handshake with a server that accepts a
	// connection and then says nothing.
	mcpConnectTimeout = 20 * time.Second
	// maxToolsPerServer bounds a paginating tool list. A server is entitled to
	// offer thousands of tools; a run is not entitled to spend its allowance
	// listing them.
	maxToolsPerServer = 250
)

// QualifiedName is the name an MCP tool is granted and called under.
func QualifiedName(server, tool string) string {
	return qualifiedPrefix + server + qualifiedSeparator + tool
}

// MCP is a Runner backed by one MCP server.
//
// An MCP server is not sandboxed. It is a process or an endpoint the operator chose
// to trust, running with its own credentials, and it can do whatever it was built to
// do. That is the reason its tools need a grant with a class: the sandbox bounds what
// the model's own commands can reach, and a grant is the only thing bounding what a
// server the operator connected can be asked to do on their behalf.
type MCP struct {
	server MCPServer
	// impl is what this client calls itself during the handshake. Servers log it.
	impl *mcp.Implementation
	// lookupEnv is os.LookupEnv outside tests.
	lookupEnv func(string) (string, bool)
	// dial is overridden in tests, where there is no server to start.
	dial func(context.Context) (*mcp.ClientSession, error)

	// mu guards session. Runs execute concurrently and share one connection, so the
	// first run to need the server is the one that opens it.
	mu      sync.Mutex
	session *mcp.ClientSession
}

// NewMCP builds a runner for one server. It does not connect.
//
// Connecting lazily is deliberate: an operator's MCP server being down must not stop
// the service from starting. A service that refuses to boot because one tool source
// is unreachable is a service that cannot answer a chat message, cannot serve its
// health check and cannot be redeployed — over a capability most runs do not use.
func NewMCP(server MCPServer, clientVersion string) *MCP {
	m := &MCP{
		server:    server,
		impl:      &mcp.Implementation{Name: "wingman-core", Version: clientVersion},
		lookupEnv: os.LookupEnv,
	}
	m.dial = m.connect
	return m
}

// Label identifies this runner.
func (m *MCP) Label() string { return "mcp:" + m.server.Name }

// Tools lists what the server offers, qualified by the server's name.
func (m *MCP) Tools(ctx context.Context) ([]Definition, error) {
	session, err := m.dial(ctx)
	if err != nil {
		return nil, err
	}

	var definitions []Definition
	for listed, err := range session.Tools(ctx, nil) {
		if err != nil {
			m.drop(session)
			return nil, fmt.Errorf("mcp server %s: list tools: %w", m.server.Name, err)
		}

		schema, err := mcpSchema(listed.InputSchema)
		if err != nil {
			// The whole server is reported as unavailable rather than this one tool
			// being skipped. A schema we cannot read is a tool we cannot call
			// correctly, and a server sending one is a server whose other schemas are
			// worth doubting too.
			m.drop(session)
			return nil, fmt.Errorf("mcp server %s: tool %q: %w", m.server.Name, listed.Name, err)
		}

		definitions = append(definitions, Definition{
			Name:        QualifiedName(m.server.Name, listed.Name),
			Description: listed.Description,
			InputSchema: schema,
			Source:      SourceMCP,
			Origin:      m.server.Name,
		})
		if len(definitions) >= maxToolsPerServer {
			break
		}
	}
	return definitions, nil
}

// Call asks the server to run one of its tools.
func (m *MCP) Call(ctx context.Context, invocation Invocation) (Result, error) {
	name, ok := m.localName(invocation.Name)
	if !ok {
		return Result{}, fmt.Errorf("mcp server %s was asked for tool %q", m.server.Name, invocation.Name)
	}

	arguments, err := invocation.Arguments()
	if err != nil {
		return Result{}, err
	}

	session, err := m.dial(ctx)
	if err != nil {
		return Result{}, err
	}

	// The run's workspace is deliberately not passed. An MCP server has its own
	// filesystem and its own credentials; handing it a path inside our sandbox would
	// be a path it cannot see, and pretending otherwise invites a tool that writes
	// "into the workspace" somewhere else entirely.
	called, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		m.drop(session)
		return Result{}, fmt.Errorf("mcp server %s: call %q: %w", m.server.Name, name, err)
	}
	return mcpResult(called), nil
}

// Close ends the session, if one was opened.
func (m *MCP) Close() error {
	m.mu.Lock()
	session := m.session
	m.session = nil
	m.mu.Unlock()

	if session == nil {
		return nil
	}
	return session.Close()
}

// localName strips the qualification, and reports false for a name belonging to
// somebody else.
func (m *MCP) localName(qualified string) (string, bool) {
	prefix := qualifiedPrefix + m.server.Name + qualifiedSeparator
	if !strings.HasPrefix(qualified, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(qualified, prefix)
	if name == "" {
		return "", false
	}
	return name, true
}

// connect opens the session, at most once.
func (m *MCP) connect(ctx context.Context) (*mcp.ClientSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != nil {
		return m.session, nil
	}

	transport, err := m.transport()
	if err != nil {
		return nil, err
	}

	// WithoutCancel, because the session outlives the run that happened to open it.
	// Tying a shared connection to one run's context means the second run finds a
	// dead session as soon as the first one ends.
	dialCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpConnectTimeout)
	defer cancel()

	// No CreateMessageHandler and no ElicitationHandler: this client does not offer
	// sampling or elicitation. A server that could ask us to run an inference would
	// be spending the operator's model budget on its own prompt, and one that could
	// elicit input would be putting questions to a user who never chose to talk to
	// it.
	session, err := mcp.NewClient(m.impl, nil).Connect(dialCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp server %s: connect: %w", m.server.Name, err)
	}

	m.session = session
	return session, nil
}

// transport builds the way in to this server.
func (m *MCP) transport() (mcp.Transport, error) {
	switch m.server.Transport {
	case TransportStdio:
		command := exec.Command(m.server.Command, m.server.Args...)
		command.Env = m.childEnv()
		return &mcp.CommandTransport{Command: command}, nil

	case TransportHTTP:
		transport := &mcp.StreamableClientTransport{
			Endpoint: m.server.URL,
			// This client never acts on a server-initiated message: the tool list a
			// run may use is snapshotted when the run starts, precisely so it cannot
			// change underneath it. Holding a stream open per server to receive
			// notifications nothing reads is a socket and a goroutine for nothing.
			DisableStandaloneSSE: true,
		}
		if m.server.AuthEnv != "" {
			token, present := m.lookupEnv(m.server.AuthEnv)
			if !present || strings.TrimSpace(token) == "" {
				// Named and missing, which is a misconfiguration rather than an
				// anonymous server: connecting without the header would send the
				// operator's requests unauthenticated and log a 401 they have to
				// work backwards from.
				return nil, fmt.Errorf("mcp server %s: %s is not set in the environment", m.server.Name, m.server.AuthEnv)
			}
			transport.HTTPClient = &http.Client{Transport: bearerTransport{token: token}}
		}
		return transport, nil

	default:
		return nil, fmt.Errorf("mcp server %s: unknown transport %q", m.server.Name, m.server.Transport)
	}
}

// childEnv is the environment a stdio server is started with.
//
// An allowlist, not os.Environ(). This process holds the model API keys, the
// database DSN, the goal engine's key and every session secret in its environment; a
// tool server started as a child of it would inherit all of them, and an MCP server
// is exactly the kind of component an operator installs from a registry without
// reading. What it gets is the handful of variables a program needs to run at all,
// plus the ones the operator named.
func (m *MCP) childEnv() []string {
	// Neutral variables: the interpreter lookup path, a home directory, a temporary
	// directory, locale and timezone, and the Windows equivalents of those. Nothing
	// here is a credential.
	base := []string{
		"PATH", "HOME", "LANG", "LC_ALL", "TZ", "TMPDIR",
		"SystemRoot", "ComSpec", "PATHEXT", "TEMP", "TMP",
		"USERPROFILE", "APPDATA", "LOCALAPPDATA",
	}

	env := make([]string, 0, len(base)+len(m.server.PassEnv))
	for _, name := range append(base, m.server.PassEnv...) {
		if value, present := m.lookupEnv(name); present {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// bearerTransport adds the operator's token to every request to an http server.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// The request is cloned rather than edited: a RoundTripper mutating the caller's
	// request is a data race waiting for a retry.
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)

	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// drop forgets a session that failed, so the next run reconnects.
//
// The comparison matters: two runs can fail on the same broken session at the same
// moment, and without it the second one would close the connection the first had
// already replaced.
func (m *MCP) drop(session *mcp.ClientSession) {
	m.mu.Lock()
	stale := m.session == session
	if stale {
		m.session = nil
	}
	m.mu.Unlock()

	if stale {
		_ = session.Close()
	}
}

// mcpSchema normalises a server's input schema into the map the providers want.
func mcpSchema(schema any) (map[string]any, error) {
	switch typed := schema.(type) {
	case nil:
		// A tool that takes no arguments. Both providers require a schema, and an
		// object with no properties is the honest way to say "none".
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	case map[string]any:
		return typed, nil
	}

	// Anything else — json.RawMessage, a typed struct from a future SDK version — is
	// re-marshalled rather than guessed at.
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("input schema cannot be encoded: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, fmt.Errorf("input schema is not a JSON object: %w", err)
	}
	return out, nil
}

// mcpResult flattens a tool result into the text a model reads.
func mcpResult(called *mcp.CallToolResult) Result {
	var parts []string
	for _, content := range called.Content {
		switch typed := content.(type) {
		case *mcp.TextContent:
			parts = append(parts, typed.Text)
		default:
			// Images, audio and embedded resources are named rather than dropped: the
			// model asked for something and got it, and silence would read as a tool
			// that returned nothing. Passing them through would mean deciding how to
			// re-encode one vendor's attachment format into another's, which is not a
			// decision to make on the way past.
			parts = append(parts, fmt.Sprintf("[%T omitted: this tool returned content that is not text]", typed))
		}
	}

	if len(parts) == 0 && called.StructuredContent != nil {
		// A server that answered only with structured output. It is already JSON on
		// the wire, so it goes to the model as JSON.
		if encoded, err := json.Marshal(called.StructuredContent); err == nil {
			parts = append(parts, string(encoded))
		}
	}

	return Result{Content: strings.Join(parts, "\n"), IsError: called.IsError}
}
