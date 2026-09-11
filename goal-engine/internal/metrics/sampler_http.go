package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes caps how much of an endpoint's response is read. A metric is
// one number; anything larger is a misconfiguration or a hostile response.
const maxResponseBytes = 1 << 20 // 1 MiB

// HTTPSampler reads scalar metrics from JSON endpoints.
//
// Endpoint URLs come from the operator-owned config file, never from the API, so
// this is not a user-controlled fetch. The limits below still apply because a
// third-party dashboard can misbehave without being hostile.
type HTTPSampler struct {
	client *http.Client
	clock  func() time.Time
}

// NewHTTPSampler builds a sampler with the given client. A nil client gets a
// default one with the provided timeout.
func NewHTTPSampler(client *http.Client, timeout time.Duration) *HTTPSampler {
	if client == nil {
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &HTTPSampler{client: client, clock: time.Now}
}

// Sample fetches the endpoint and extracts the configured JSON path.
func (s *HTTPSampler) Sample(ctx context.Context, def Definition) (Sample, error) {
	if def.Source != SourceHTTP {
		return Sample{}, fmt.Errorf("metric %s: %w", def.Key, ErrNotPullable)
	}

	method := def.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, def.URL, nil)
	if err != nil {
		return Sample{}, fmt.Errorf("metric %s: build request: %w", def.Key, err)
	}
	req.Header.Set("Accept", "application/json")
	for name, value := range def.Headers {
		req.Header.Set(name, value)
	}
	if def.AuthHeader != "" && def.authValue != "" {
		req.Header.Set(def.AuthHeader, def.authValue)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return Sample{}, fmt.Errorf("metric %s: request failed: %w", def.Key, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Sample{}, fmt.Errorf("metric %s: endpoint returned HTTP %d", def.Key, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Sample{}, fmt.Errorf("metric %s: read response: %w", def.Key, err)
	}
	if len(body) > maxResponseBytes {
		return Sample{}, fmt.Errorf("metric %s: response exceeds %d bytes", def.Key, maxResponseBytes)
	}

	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return Sample{}, fmt.Errorf("metric %s: response is not valid JSON: %w", def.Key, err)
	}

	raw, err := walkJSONPath(payload, def.JSONPath)
	if err != nil {
		return Sample{}, fmt.Errorf("metric %s: %w", def.Key, err)
	}
	value, err := toFloat(raw)
	if err != nil {
		return Sample{}, fmt.Errorf("metric %s: path %q: %w", def.Key, def.JSONPath, err)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Sample{}, fmt.Errorf("metric %s: path %q yielded a non-finite value", def.Key, def.JSONPath)
	}

	return Sample{
		MetricKey:  def.Key,
		Value:      value,
		ObservedAt: s.clock().UTC(),
		Source:     SourceHTTP,
		Note:       "path=" + def.JSONPath,
	}, nil
}

// walkJSONPath resolves a dotted path such as "data.mrr" or "items.0.value".
func walkJSONPath(payload any, path string) (any, error) {
	current := payload
	for _, segment := range strings.Split(path, ".") {
		if segment == "" {
			return nil, fmt.Errorf("json path %q has an empty segment", path)
		}
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[segment]
			if !ok {
				return nil, fmt.Errorf("json path %q: key %q not found", path, segment)
			}
			current = next
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil {
				return nil, fmt.Errorf("json path %q: %q is not an array index", path, segment)
			}
			if index < 0 || index >= len(node) {
				return nil, fmt.Errorf("json path %q: index %d is out of range (length %d)", path, index, len(node))
			}
			current = node[index]
		default:
			return nil, fmt.Errorf("json path %q: cannot descend into %T at %q", path, current, segment)
		}
	}
	return current, nil
}

// toFloat accepts a JSON number or a numeric string, and rejects everything else.
func toFloat(raw any) (float64, error) {
	switch v := raw.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("value %q is not a number: %w", v.String(), err)
		}
		return f, nil
	case float64:
		return v, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, fmt.Errorf("value %q is not numeric", v)
		}
		return f, nil
	case nil:
		return 0, fmt.Errorf("value is null")
	default:
		return 0, fmt.Errorf("value has type %T, expected a number", raw)
	}
}
