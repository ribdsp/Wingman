package tool

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Transports an MCP server can be reached over.
const (
	// TransportStdio runs the server as a child process and speaks to it over its
	// standard input and output.
	TransportStdio = "stdio"
	// TransportHTTP speaks to a server over streamable HTTP.
	TransportHTTP = "http"
)

// maxServerNameLength keeps the qualified tool name inside the 64 characters both
// providers allow. "mcp__" plus the server name plus "__" is the overhead, so a
// 20-character server name still leaves 37 for the tool's own name — and a server
// named at the limit that offers a long tool name gets a clear error from
// LoadGrants at startup rather than a rejected request mid-run.
const maxServerNameLength = 20

var (
	serverNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[_-][a-z0-9]+)*$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// MCPServer is one validated MCP server declaration.
type MCPServer struct {
	Name      string
	Transport string
	// Command and Args start a stdio server.
	Command string
	Args    []string
	// PassEnv names environment variables to hand to the child process. Only names
	// appear here — see childEnv for why, and for what the child gets besides these.
	PassEnv []string
	// URL is the endpoint of an http server.
	URL string
	// AuthEnv names the environment variable holding the bearer token for an http
	// server. The token itself is never in the config file.
	AuthEnv string
	Enabled bool
}

// serverEntry is the on-disk shape of one server.
type serverEntry struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Transport   string   `yaml:"transport"`
	Command     string   `yaml:"command"`
	Args        []string `yaml:"args"`
	PassEnv     []string `yaml:"passEnv"`
	URL         string   `yaml:"url"`
	AuthEnv     string   `yaml:"authEnv"`
	Enabled     *bool    `yaml:"enabled"`
}

type mcpFile struct {
	Servers []serverEntry `yaml:"servers"`
}

// LoadMCPServers reads and validates the MCP server config at path.
//
// As with the grants, every problem is reported at once and a file with no document
// in it is a valid configuration meaning no server. Disabled servers are returned
// too: the caller decides, and a list that quietly omitted them would make
// "misspelled the name" and "switched it off" look the same.
func LoadMCPServers(path string) ([]MCPServer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mcp config %s: %w", path, err)
	}

	var parsed mcpFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse mcp config %s: %w", path, err)
	}

	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	seen := map[string]bool{}
	servers := make([]MCPServer, 0, len(parsed.Servers))
	for i, entry := range parsed.Servers {
		label := fmt.Sprintf("servers[%d]", i)
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			fail("%s: name is required", label)
			continue
		}
		label = fmt.Sprintf("server %q", name)
		if !serverNamePattern.MatchString(name) {
			fail("%s: name must be lowercase alphanumeric segments separated by _ or -", label)
		}
		if len(name) > maxServerNameLength {
			fail("%s: name is longer than %d characters, which leaves no room for the tool names it qualifies",
				label, maxServerNameLength)
		}
		if seen[name] {
			fail("%s: declared twice", label)
			continue
		}
		seen[name] = true

		transport := strings.ToLower(strings.TrimSpace(entry.Transport))
		if transport == "" {
			transport = TransportStdio
		}

		enabled := true
		if entry.Enabled != nil {
			enabled = *entry.Enabled
		}
		server := MCPServer{
			Name:      name,
			Transport: transport,
			Command:   strings.TrimSpace(entry.Command),
			Args:      entry.Args,
			PassEnv:   entry.PassEnv,
			URL:       strings.TrimSpace(entry.URL),
			AuthEnv:   strings.TrimSpace(entry.AuthEnv),
			Enabled:   enabled,
		}

		validateServer(server, label, fail)
		servers = append(servers, server)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("invalid mcp config %s:\n  - %s", path, strings.Join(problems, "\n  - "))
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return servers, nil
}

// validateServer checks one declaration is complete for the transport it chose.
//
// Fields belonging to the other transport are rejected rather than ignored. A stdio
// server with a url is an operator who thinks they configured a remote server, and
// ignoring the field would run a local command they have stopped thinking about.
func validateServer(server MCPServer, label string, fail func(string, ...any)) {
	switch server.Transport {
	case TransportStdio:
		if server.Command == "" {
			fail("%s: command is required for the %s transport", label, TransportStdio)
		}
		if server.URL != "" {
			fail("%s: url belongs to the %s transport", label, TransportHTTP)
		}
		if server.AuthEnv != "" {
			fail("%s: authEnv belongs to the %s transport", label, TransportHTTP)
		}

	case TransportHTTP:
		if server.URL == "" {
			fail("%s: url is required for the %s transport", label, TransportHTTP)
		} else if parsed, err := url.Parse(server.URL); err != nil || parsed.Host == "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			// The url is not echoed. An operator who put a token in the query string
			// would otherwise have it in a startup log, which is the one place it was
			// not supposed to be.
			fail("%s: url must be an absolute http or https URL", label)
		}
		if server.Command != "" {
			fail("%s: command belongs to the %s transport", label, TransportStdio)
		}
		if len(server.Args) > 0 {
			fail("%s: args belong to the %s transport", label, TransportStdio)
		}
		if len(server.PassEnv) > 0 {
			fail("%s: passEnv belongs to the %s transport", label, TransportStdio)
		}

	default:
		fail("%s: transport must be %q or %q, got %q", label, TransportStdio, TransportHTTP, server.Transport)
	}

	for i, name := range server.PassEnv {
		validateEnvName(name, fmt.Sprintf("passEnv[%d]", i), label, fail)
	}
	if server.AuthEnv != "" {
		validateEnvName(server.AuthEnv, "authEnv", label, fail)
	}
}

// validateEnvName refuses anything that is not a bare variable name.
//
// `TOKEN=abc123` is the shape of the mistake worth catching: it reads like it works,
// it would be committed to a repository, and the value would be in the config file
// this whole design exists to keep secrets out of.
//
// No branch echoes the entry, for that same reason — an operator who pasted the token
// itself where its name belongs has made exactly the mistake whose error message must
// not end up in a startup log. The field and its position say which entry it was.
func validateEnvName(name, field, label string, fail func(string, ...any)) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		fail("%s: %s has an empty entry", label, field)
		return
	}
	if strings.Contains(trimmed, "=") {
		fail("%s: %s takes variable names, not values — put the secret in the environment", label, field)
		return
	}
	if !envNamePattern.MatchString(trimmed) {
		fail("%s: %s is not a variable name — it must look like REPORTS_TOKEN, with the secret itself in the environment", label, field)
	}
}

// EnabledMCPServers filters a loaded list down to the servers to connect to.
func EnabledMCPServers(servers []MCPServer) []MCPServer {
	out := make([]MCPServer, 0, len(servers))
	for _, server := range servers {
		if server.Enabled {
			out = append(out, server)
		}
	}
	return out
}
