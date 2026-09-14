package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	// bodyArgument is the argument an endpoint that declares a body takes. One fixed
	// name, because the model is told the shape in the description and a per-endpoint
	// name would be another thing an operator can get wrong.
	bodyArgument = "body"
	// maxHTTPRedirects bounds a same-host redirect chain.
	maxHTTPRedirects = 3
)

// HTTPAPI is a Runner backed by one operator-declared HTTP API.
//
// Nothing here is discovered. The endpoints, their methods, their parameters and the
// variable holding the credential all come from a file the operator wrote, and this
// type will not send a request that file does not describe. That is the difference
// between offering an agent an API and offering it whatever the API's publisher adds
// next.
type HTTPAPI struct {
	service HTTPService
	// tools is the declared endpoints by name, for routing a call.
	tools map[string]HTTPTool
	// userAgent identifies this service to the API being called. Vendors rate-limit
	// and support on it, so it says what we are rather than nothing.
	userAgent string
	// lookupEnv is os.LookupEnv outside tests.
	lookupEnv func(string) (string, bool)
	client    *http.Client
}

// NewHTTPAPI builds a runner for one declared service.
func NewHTTPAPI(service HTTPService, clientVersion string, lookupEnv func(string) (string, bool)) *HTTPAPI {
	if service.Timeout <= 0 {
		// LoadHTTPServices always sets one; a service built in code might not. Zero here
		// would mean an already-expired deadline rather than an unlimited one, so every
		// call would fail before it was sent.
		service.Timeout = defaultHTTPTimeout
	}
	h := &HTTPAPI{
		service:   service,
		tools:     make(map[string]HTTPTool, len(service.Tools)),
		userAgent: "wingman-core/" + clientVersion,
		lookupEnv: lookupEnv,
	}
	for _, declared := range service.Tools {
		h.tools[declared.Name] = declared
	}
	// No client timeout: the deadline comes from the request context, which is the
	// step's own deadline narrowed by this service's. A timeout on the client as well
	// would be a second, invisible limit that cancels a request the step still had
	// time for.
	h.client = &http.Client{CheckRedirect: h.checkRedirect}
	return h
}

// Label identifies this runner.
func (h *HTTPAPI) Label() string { return "http:" + h.service.Name }

// Tools lists the declared endpoints.
//
// It cannot fail and it touches no network. An HTTP API's tool list is a config file,
// so unlike an MCP server there is nothing to be unavailable at the moment a run
// starts — a service that is down fails the call that needs it, not the whole run.
func (h *HTTPAPI) Tools(context.Context) ([]Definition, error) {
	definitions := make([]Definition, 0, len(h.service.Tools))
	for _, declared := range h.service.Tools {
		definitions = append(definitions, Definition{
			Name:        declared.Name,
			Description: declared.Description,
			InputSchema: httpSchema(declared),
			Source:      SourceHTTP,
			Origin:      h.service.Name,
		})
	}
	return definitions, nil
}

// Call sends one declared request and returns what came back.
func (h *HTTPAPI) Call(ctx context.Context, invocation Invocation) (Result, error) {
	declared, ok := h.tools[invocation.Name]
	if !ok {
		return Result{}, fmt.Errorf("http service %s was asked for tool %q", h.service.Name, invocation.Name)
	}

	arguments, err := invocation.Arguments()
	if err != nil {
		return Result{}, err
	}

	// The run's workspace is not passed. A declared API is somebody else's server; it
	// has no view of this sandbox, and a path from inside it would mean nothing there.
	//
	// The step's deadline still applies: WithTimeout takes whichever of the two is
	// sooner, so a service allowed 20 seconds inside a step with 5 left gets 5.
	ctx, cancel := context.WithTimeout(ctx, h.service.Timeout)
	defer cancel()

	request, err := h.buildRequest(ctx, declared, arguments)
	if err != nil {
		var wrong argumentError
		if errors.As(err, &wrong) {
			// The model named the wrong argument, or left one out. It goes back as a
			// result it can read and correct, not as an error that ends the step.
			return Result{Content: "error: " + wrong.Error(), IsError: true}, nil
		}
		return Result{}, err
	}

	response, err := h.client.Do(request)
	if err != nil {
		// Not wrapped with the URL. An operator who put a token in the base URL would
		// otherwise find it in this run's transcript, which is stored and rendered.
		return Result{}, fmt.Errorf("http service %s: %s: %w", h.service.Name, declared.Name, redactURL(err, h.service.BaseURL))
	}
	defer func() { _ = response.Body.Close() }()

	// One more byte than the bound, so a body that is exactly at it is not reported as
	// truncated and one over it is.
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPResponseBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("http service %s: %s: read response: %w", h.service.Name, declared.Name, err)
	}
	if len(body) > maxHTTPResponseBytes {
		body = append(body[:maxHTTPResponseBytes], []byte("\n[response truncated]")...)
	}

	return httpResult(response.StatusCode, body), nil
}

// buildRequest turns the model's arguments into the one request this endpoint allows.
func (h *HTTPAPI) buildRequest(
	ctx context.Context,
	declared HTTPTool,
	arguments map[string]any,
) (*http.Request, error) {
	if err := h.checkArgumentNames(declared, arguments); err != nil {
		return nil, err
	}

	path := declared.Path
	query := url.Values{}
	for _, parameter := range declared.Parameters {
		raw, present := arguments[parameter.Name]
		if !present {
			if parameter.Required {
				return nil, argumentError{fmt.Sprintf("argument %q is required", parameter.Name)}
			}
			continue
		}

		value, err := scalarArgument(parameter.Name, raw)
		if err != nil {
			return nil, err
		}
		switch parameter.In {
		case ParameterPath:
			if strings.TrimSpace(value) == "" {
				// An empty path segment collapses the URL into a different endpoint —
				// /v1/invoices/ is a list where /v1/invoices/{id} was a fetch.
				return nil, argumentError{fmt.Sprintf("argument %q cannot be empty", parameter.Name)}
			}
			// Escaped, because the value came from a model reading somebody's data. A
			// bare ../ or ?admin=1 in an id would otherwise be a different request to a
			// different path than the operator declared.
			path = strings.ReplaceAll(path, "{"+parameter.Name+"}", url.PathEscape(value))
		case ParameterQuery:
			query.Set(parameter.Name, value)
		}
	}

	body, err := httpBody(declared, arguments)
	if err != nil {
		return nil, err
	}

	target := h.service.BaseURL + path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, declared.Method, target, reader)
	if err != nil {
		// Only reachable if a validated base URL and a validated path still do not
		// parse together, so it names neither.
		return nil, fmt.Errorf("http service %s: %s: build request: %w", h.service.Name, declared.Name, redactURL(err, h.service.BaseURL))
	}

	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", h.userAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if err := h.authorise(request); err != nil {
		return nil, err
	}
	return request, nil
}

// checkArgumentNames refuses an argument this endpoint never declared.
//
// Refused rather than dropped, even though the schema says additionalProperties is
// false. A model that sent `id` where the parameter is `invoiceId` would otherwise get
// the unfiltered list back and treat it as the answer to its question; being told the
// name is wrong is something it can act on.
func (h *HTTPAPI) checkArgumentNames(declared HTTPTool, arguments map[string]any) error {
	allowed := make(map[string]bool, len(declared.Parameters)+1)
	for _, parameter := range declared.Parameters {
		allowed[parameter.Name] = true
	}
	if declared.BodyDescription != "" {
		allowed[bodyArgument] = true
	}

	unknown := make([]string, 0, len(arguments))
	for name := range arguments {
		if !allowed[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	known := make([]string, 0, len(allowed))
	for name := range allowed {
		known = append(known, name)
	}
	sort.Strings(unknown)
	sort.Strings(known)
	return argumentError{fmt.Sprintf(
		"tool %s does not take %s; it takes %s",
		declared.Name, strings.Join(unknown, ", "), joinOrNone(known),
	)}
}

// authorise adds the operator's credential, if this service has one.
func (h *HTTPAPI) authorise(request *http.Request) error {
	if h.service.AuthEnv == "" {
		return nil
	}

	credential, present := h.lookupEnv(h.service.AuthEnv)
	if !present || strings.TrimSpace(credential) == "" {
		// Named and missing is a misconfiguration, not an anonymous API: sending the
		// request without the header would give the model a 401 to reason about and
		// leave the operator to work backwards from it.
		return fmt.Errorf("http service %s: %s is not set in the environment", h.service.Name, h.service.AuthEnv)
	}

	prefix := "Bearer "
	if h.service.AuthPrefix != nil {
		// An explicitly empty prefix is an API that wants the bare token, which is
		// common enough that it has to be expressible.
		prefix = strings.TrimSpace(*h.service.AuthPrefix)
		if prefix != "" {
			prefix += " "
		}
	}
	request.Header.Set(h.service.AuthHeader, prefix+credential)
	return nil
}

// checkRedirect keeps a request on the host it was declared against.
//
// net/http drops the Authorization header on a cross-domain redirect, but following
// one at all would still send the request — its path, its query, whatever the model
// put in them — to a host the operator never named. A vendor that moved is a config
// change, not something to discover at runtime.
func (h *HTTPAPI) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= maxHTTPRedirects {
		return fmt.Errorf("http service %s: too many redirects", h.service.Name)
	}
	if request.URL.Host != via[0].URL.Host {
		return fmt.Errorf("http service %s: redirected to another host, which it will not follow", h.service.Name)
	}
	return nil
}

// httpSchema describes the declared endpoint to the model.
func httpSchema(declared HTTPTool) map[string]any {
	properties := map[string]any{}
	var required []string

	for _, parameter := range declared.Parameters {
		// Every parameter is a string. The config declares no types, and a schema
		// claiming a number where the vendor's API takes a string would have the model
		// correcting an error that is ours. Numbers and booleans the model sends anyway
		// are accepted and formatted — see scalarArgument.
		property := map[string]any{"type": "string"}
		if parameter.Description != "" {
			property["description"] = parameter.Description
		}
		properties[parameter.Name] = property
		if parameter.Required {
			required = append(required, parameter.Name)
		}
	}

	if declared.BodyDescription != "" {
		properties[bodyArgument] = map[string]any{
			"type":        "object",
			"description": declared.BodyDescription,
		}
		required = append(required, bodyArgument)
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
		// False, so a model that invents a parameter is corrected by the provider
		// before the call is made rather than by us after it.
		"additionalProperties": false,
	}
	if len(required) > 0 {
		sort.Strings(required)
		names := make([]any, 0, len(required))
		for _, name := range required {
			names = append(names, name)
		}
		schema["required"] = names
	}
	return schema
}

// httpBody encodes the request body, when the endpoint declares one.
func httpBody(declared HTTPTool, arguments map[string]any) ([]byte, error) {
	if declared.BodyDescription == "" {
		return nil, nil
	}

	raw, present := arguments[bodyArgument]
	if !present {
		return nil, argumentError{fmt.Sprintf("argument %q is required and must be an object", bodyArgument)}
	}
	object, ok := raw.(map[string]any)
	if !ok {
		// Refused rather than forwarded as-is. An endpoint declared as taking an object
		// that receives a string is a model that put the whole body in one field, and
		// sending it would be a 400 the model reads as "the API is broken".
		return nil, argumentError{fmt.Sprintf("argument %q must be an object, got %T", bodyArgument, raw)}
	}

	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, argumentError{fmt.Sprintf("argument %q cannot be encoded as JSON: %v", bodyArgument, err)}
	}
	return encoded, nil
}

// scalarArgument formats one parameter value for a URL.
func scalarArgument(name string, raw any) (string, error) {
	switch typed := raw.(type) {
	case string:
		return typed, nil
	case bool:
		return strconv.FormatBool(typed), nil
	case float64:
		// Every JSON number decodes as a float64, so an id of 41 arrives as 41.0 and
		// must not be sent that way. -1 asks for the shortest form that round-trips,
		// which prints 41 as "41" and 0.5 as "0.5".
		return strconv.FormatFloat(typed, 'f', -1, 64), nil
	default:
		return "", argumentError{fmt.Sprintf("argument %q must be a string, number or boolean, got %T", name, raw)}
	}
}

// httpResult turns a response into what the model sees.
func httpResult(status int, body []byte) Result {
	text := strings.TrimRight(string(body), "\n")
	if status >= 400 {
		// A 4xx or a 5xx is output, not a Go error: the model asked for an invoice that
		// does not exist, or the vendor is down, and either way the model is what
		// decides whether to try something else. The body is included because a
		// vendor's own message ("field `amount` must be positive") is the only thing
		// that makes the failure correctable.
		if text == "" {
			return Result{Content: fmt.Sprintf("[HTTP %d %s]", status, http.StatusText(status)), IsError: true}
		}
		return Result{Content: fmt.Sprintf("[HTTP %d %s]\n%s", status, http.StatusText(status), text), IsError: true}
	}
	if text == "" {
		// A 204, or a 200 with nothing in it. Silence would read to the model as a tool
		// that does not work.
		return Result{Content: fmt.Sprintf("[HTTP %d %s, empty response]", status, http.StatusText(status))}
	}
	return Result{Content: text}
}

// argumentError is a mistake in the model's arguments rather than a fault.
//
// It exists to keep the two apart at the one place that has to tell them apart: the
// model's mistakes go back to the model as a result it can correct, and everything
// else ends the step.
type argumentError struct{ message string }

func (a argumentError) Error() string { return a.message }

// redactURL removes the service's base URL from an error's text.
//
// net/url and net/http both put the whole URL in their errors, and the base URL is the
// one part of a request that can carry a credential an operator wrote into config.
func redactURL(err error, baseURL string) error {
	if baseURL == "" || err == nil {
		return err
	}
	text := err.Error()
	if !strings.Contains(text, baseURL) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(text, baseURL, "<baseUrl>"))
}

// joinOrNone renders a list of argument names for a message to the model.
func joinOrNone(names []string) string {
	if len(names) == 0 {
		return "no arguments"
	}
	return strings.Join(names, ", ")
}
