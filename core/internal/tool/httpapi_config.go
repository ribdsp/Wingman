package tool

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Where a declared parameter goes in the request.
const (
	// ParameterPath substitutes into the path template, as {invoiceId}.
	ParameterPath = "path"
	// ParameterQuery appends to the query string.
	ParameterQuery = "query"
)

const (
	// defaultHTTPTimeout bounds one request to a declared API.
	defaultHTTPTimeout = 20 * time.Second
	// minHTTPTimeout is a floor, not a suggestion. A timeout of zero would mean "no
	// limit" to net/http, which is how one unresponsive vendor holds a run's worker
	// open until the process is restarted.
	minHTTPTimeout = time.Second
	// maxHTTPTimeout stops one declared endpoint outlasting the step that called it.
	maxHTTPTimeout = 2 * time.Minute
	// maxHTTPResponseBytes bounds what is read off the wire. The result is truncated
	// again before it reaches the model; this bound exists so a vendor streaming a
	// gigabyte cannot exhaust memory before truncation gets the chance.
	maxHTTPResponseBytes = 4 * MaxOutputBytes
)

var (
	// pathTemplatePattern finds {name} placeholders.
	pathTemplatePattern = regexp.MustCompile(`\{([^{}]*)\}`)
	// parameterNamePattern is deliberately narrow: these names are written by an
	// operator, read by a model, and interpolated into a URL.
	parameterNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
)

// HTTPMethods that may be declared. Anything outside this set is refused.
//
// No DELETE. Not because it is technically harder, but because a declared endpoint
// that destroys a record on a `GET`-shaped tool call is the single most expensive
// mistake this file could enable, and an operator who genuinely wants one can declare
// the vendor's POST-shaped equivalent. Removing this restriction needs an argument.
var httpMethods = map[string]bool{
	http.MethodGet:   true,
	http.MethodPost:  true,
	http.MethodPut:   true,
	http.MethodPatch: true,
}

// HTTPParameter is one argument a declared endpoint takes.
type HTTPParameter struct {
	Name string
	// In is ParameterPath or ParameterQuery.
	In          string
	Description string
	Required    bool
}

// HTTPTool is one endpoint offered to the model as a tool.
type HTTPTool struct {
	Name        string
	Description string
	Method      string
	// Path is a template relative to the service's base URL, as /v1/invoices/{id}.
	Path       string
	Parameters []HTTPParameter
	// BodyDescription, when set, offers the model a `body` object argument that is
	// sent as the JSON request body. Empty means the endpoint takes no body, and one
	// the model tried to send would be dropped rather than forwarded.
	BodyDescription string
}

// HTTPService is one API whose endpoints are offered as tools.
type HTTPService struct {
	Name    string
	BaseURL string
	// AuthEnv names the environment variable holding the credential. The credential
	// itself is never in the config file.
	AuthEnv string
	// AuthHeader is the header it is sent in, "Authorization" by default.
	AuthHeader string
	// AuthPrefix is what precedes the credential in that header, "Bearer" by default.
	// Some APIs want a bare token, which is an explicitly empty prefix.
	AuthPrefix *string
	Timeout    time.Duration
	Tools      []HTTPTool
	Enabled    bool
}

// The on-disk shapes.
type (
	httpParameterEntry struct {
		Name        string `yaml:"name"`
		In          string `yaml:"in"`
		Description string `yaml:"description"`
		Required    *bool  `yaml:"required"`
	}

	httpToolEntry struct {
		Name            string               `yaml:"name"`
		Description     string               `yaml:"description"`
		Method          string               `yaml:"method"`
		Path            string               `yaml:"path"`
		Parameters      []httpParameterEntry `yaml:"parameters"`
		BodyDescription string               `yaml:"bodyDescription"`
	}

	httpServiceEntry struct {
		Name       string          `yaml:"name"`
		BaseURL    string          `yaml:"baseUrl"`
		AuthEnv    string          `yaml:"authEnv"`
		AuthHeader string          `yaml:"authHeader"`
		AuthPrefix *string         `yaml:"authPrefix"`
		Timeout    string          `yaml:"timeout"`
		Enabled    *bool           `yaml:"enabled"`
		Tools      []httpToolEntry `yaml:"tools"`
	}

	httpFile struct {
		Services []httpServiceEntry `yaml:"services"`
	}
)

// LoadHTTPServices reads and validates the HTTP tool config at path.
//
// Every endpoint is declared by hand. That is the point: an OpenAPI document is a
// vendor's file, and compiling one straight into tools would let whoever publishes it
// decide what an autonomous agent may attempt. What is here is the shape such a
// document would compile down to, with the operator as the compiler.
//
// As with the grants and the MCP servers, every problem is reported at once and a file
// with no document in it is a valid configuration meaning no HTTP tools.
func LoadHTTPServices(path string) ([]HTTPService, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read http tool config %s: %w", path, err)
	}

	var parsed httpFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse http tool config %s: %w", path, err)
	}

	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	seenService := map[string]bool{}
	// Tool names are unique across every service, not per service. They are the names
	// an operator writes in tools.yaml, so two services offering get_invoice would
	// share one grant and the operator could not permit one without the other.
	seenTool := map[string]string{}

	services := make([]HTTPService, 0, len(parsed.Services))
	for i, entry := range parsed.Services {
		label := fmt.Sprintf("services[%d]", i)
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			fail("%s: name is required", label)
			continue
		}
		label = fmt.Sprintf("service %q", name)
		if !serverNamePattern.MatchString(name) {
			fail("%s: name must be lowercase alphanumeric segments separated by _ or -", label)
		}
		if seenService[name] {
			fail("%s: declared twice", label)
			continue
		}
		seenService[name] = true

		service := HTTPService{
			Name:       name,
			BaseURL:    strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/"),
			AuthEnv:    strings.TrimSpace(entry.AuthEnv),
			AuthHeader: strings.TrimSpace(entry.AuthHeader),
			AuthPrefix: entry.AuthPrefix,
			Timeout:    defaultHTTPTimeout,
			Enabled:    entry.Enabled == nil || *entry.Enabled,
		}
		if service.AuthHeader == "" {
			service.AuthHeader = "Authorization"
		}
		if entry.Timeout != "" {
			parsedTimeout, err := time.ParseDuration(entry.Timeout)
			switch {
			case err != nil:
				fail("%s: timeout %q is not a duration", label, entry.Timeout)
			case parsedTimeout < minHTTPTimeout || parsedTimeout > maxHTTPTimeout:
				fail("%s: timeout must be between %s and %s", label, minHTTPTimeout, maxHTTPTimeout)
			default:
				service.Timeout = parsedTimeout
			}
		}

		// The tools first, because whether the service has any callable endpoint is part
		// of whether the service itself is complete.
		service.Tools = validateHTTPTools(entry.Tools, label, seenTool, name, fail)
		validateHTTPService(service, label, fail)
		services = append(services, service)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("invalid http tool config %s:\n  - %s", path, strings.Join(problems, "\n  - "))
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services, nil
}

// validateHTTPService checks the service itself.
func validateHTTPService(service HTTPService, label string, fail func(string, ...any)) {
	if service.BaseURL == "" {
		fail("%s: baseUrl is required", label)
	} else if parsed, err := url.Parse(service.BaseURL); err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		// Not echoed, for the same reason an MCP url is not: an operator who put a key
		// in the query string would otherwise find it in a startup log.
		fail("%s: baseUrl must be an absolute http or https URL", label)
	} else if parsed.RawQuery != "" || parsed.Fragment != "" {
		// A query string on the base URL would be silently dropped once a tool adds its
		// own parameters, and the request would go out without whatever it carried.
		fail("%s: baseUrl must not carry a query string or fragment", label)
	} else if parsed.User != nil {
		// https://user:pass@host is a credential in a config file, which is the one
		// thing this file is designed not to hold. An API wanting basic auth is
		// expressible as authEnv with authPrefix: Basic.
		fail("%s: baseUrl must not embed credentials — name them with authEnv instead", label)
	}

	if service.AuthEnv != "" {
		validateEnvName(service.AuthEnv, "authEnv", label, fail)
	}
	if strings.ContainsAny(service.AuthHeader, " \t\r\n:") {
		fail("%s: authHeader %q is not a header name", label, service.AuthHeader)
	}
	if len(service.Tools) == 0 {
		// An operator halfway through writing the file. Left as an error because a
		// service with no endpoints does nothing, and silence would read as "the API is
		// connected" when nothing about it is callable.
		fail("%s: declares no tools", label)
	}
}

// validateHTTPTools checks each endpoint and returns the ones that parsed.
func validateHTTPTools(
	entries []httpToolEntry,
	serviceLabel string,
	seenTool map[string]string,
	service string,
	fail func(string, ...any),
) []HTTPTool {
	tools := make([]HTTPTool, 0, len(entries))
	for j, entry := range entries {
		label := fmt.Sprintf("%s tools[%d]", serviceLabel, j)
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			fail("%s: name is required", label)
			continue
		}
		label = fmt.Sprintf("%s tool %q", serviceLabel, name)
		if !ValidName(name) {
			fail("%s: name must be 1-%d characters of letters, digits, _ or -", label, maxNameLength)
		}
		if owner, taken := seenTool[name]; taken {
			fail("%s: name is already declared by service %q, and one name is one grant", label, owner)
			continue
		}
		seenTool[name] = service

		if strings.TrimSpace(entry.Description) == "" {
			// The description is the whole interface as far as the model is concerned. An
			// endpoint without one is either never called or called wrongly.
			fail("%s: description is required, because it is what the model chooses from", label)
		}

		method := strings.ToUpper(strings.TrimSpace(entry.Method))
		if method == "" {
			method = http.MethodGet
		}
		if !httpMethods[method] {
			fail("%s: method %q is not one this runner will send", label, method)
		}

		path := strings.TrimSpace(entry.Path)
		validateHTTPPath(path, label, fail)

		parameters := validateHTTPParameters(entry.Parameters, path, label, fail)
		if method == http.MethodGet && strings.TrimSpace(entry.BodyDescription) != "" {
			// Refused rather than dropped: an operator who described a body believes it
			// is being sent, and a GET that silently loses it fails in the vendor's logs
			// rather than in ours.
			fail("%s: a %s cannot carry a body", label, http.MethodGet)
		}

		tools = append(tools, HTTPTool{
			Name:            name,
			Description:     strings.TrimSpace(entry.Description),
			Method:          method,
			Path:            path,
			Parameters:      parameters,
			BodyDescription: strings.TrimSpace(entry.BodyDescription),
		})
	}
	return tools
}

// validateHTTPPath refuses a template that could leave the service it belongs to.
func validateHTTPPath(path, label string, fail func(string, ...any)) {
	if path == "" {
		fail("%s: path is required", label)
		return
	}
	if !strings.HasPrefix(path, "/") {
		fail("%s: path must start with /", label)
	}
	if strings.HasPrefix(path, "//") {
		// //evil.test/x resolves to another host entirely, which would send the
		// service's credential somewhere it was never granted.
		fail("%s: path must not start with //", label)
	}
	if strings.Contains(path, "..") {
		fail("%s: path must not contain ..", label)
	}
	if strings.Contains(path, "?") || strings.Contains(path, "#") {
		// Query parameters are declared, so they can be named to the model and escaped
		// on the way out. One written into the template is neither.
		fail("%s: declare query parameters rather than writing them into the path", label)
	}
	if strings.ContainsAny(path, " \t\r\n") {
		fail("%s: path must not contain whitespace", label)
	}
}

// validateHTTPParameters checks the declared arguments against the path template.
func validateHTTPParameters(
	entries []httpParameterEntry,
	path, label string,
	fail func(string, ...any),
) []HTTPParameter {
	parameters := make([]HTTPParameter, 0, len(entries))
	seen := map[string]bool{}
	declaredPath := map[string]bool{}

	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			fail("%s: a parameter has no name", label)
			continue
		}
		if !parameterNamePattern.MatchString(name) {
			fail("%s: parameter %q must start with a letter and hold only letters, digits or _", label, name)
		}
		if seen[name] {
			fail("%s: parameter %q is declared twice", label, name)
			continue
		}
		seen[name] = true

		in := strings.ToLower(strings.TrimSpace(entry.In))
		if in == "" {
			in = ParameterQuery
		}
		switch in {
		case ParameterPath:
			declaredPath[name] = true
		case ParameterQuery:
		default:
			fail("%s: parameter %q must be in %q or %q, got %q", label, name, ParameterPath, ParameterQuery, in)
		}

		required := in == ParameterPath
		if entry.Required != nil {
			required = *entry.Required
		}
		if in == ParameterPath && !required {
			// A path segment cannot be absent — the URL would have a literal {id} in it.
			// Correcting it silently would contradict a file the operator can read.
			fail("%s: path parameter %q cannot be optional", label, name)
		}

		parameters = append(parameters, HTTPParameter{
			Name:        name,
			In:          in,
			Description: strings.TrimSpace(entry.Description),
			Required:    required,
		})
	}

	// Both directions, because either mismatch produces a request that is wrong rather
	// than one that fails: an undeclared placeholder is sent literally, and a declared
	// one with nowhere to go is dropped.
	for _, match := range pathTemplatePattern.FindAllStringSubmatch(path, -1) {
		if placeholder := match[1]; !declaredPath[placeholder] {
			fail("%s: the path uses {%s}, which is not declared as a path parameter", label, placeholder)
		}
	}
	for name := range declaredPath {
		if !strings.Contains(path, "{"+name+"}") {
			fail("%s: path parameter %q does not appear in the path", label, name)
		}
	}
	return parameters
}

// EnabledHTTPServices filters a loaded list down to the services to offer.
func EnabledHTTPServices(services []HTTPService) []HTTPService {
	out := make([]HTTPService, 0, len(services))
	for _, service := range services {
		if service.Enabled {
			out = append(out, service)
		}
	}
	return out
}
