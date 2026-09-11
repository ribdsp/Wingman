// Package metrics declares where business metrics come from and how to read
// them.
//
// Metric definitions live in an operator-owned YAML file and are loaded once at
// startup. They are deliberately not creatable through the API: a metric
// definition contains a SQL query, so an agent able to create one would be able
// to run arbitrary reads against production data. The registry is read-only for
// the lifetime of the process.
package metrics

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SourceType is how a metric's value is obtained.
type SourceType string

const (
	// SourceSQL reads a scalar from a configured database.
	SourceSQL SourceType = "sql"
	// SourceHTTP reads a scalar from a JSON endpoint.
	SourceHTTP SourceType = "http"
	// SourcePush is written by an operator or an external job through the API.
	// The monitor never pulls it; it reads the latest stored sample.
	SourcePush SourceType = "push"
)

const (
	defaultMaxOpenConns = 4
	maxMetricKeyLength  = 120
)

var metricKeyPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// Datasource is a database the sampler may read from.
type Datasource struct {
	Name         string `yaml:"name"`
	Driver       string `yaml:"driver"`
	DSNEnv       string `yaml:"dsnEnv"`
	MaxOpenConns int    `yaml:"maxOpenConns"`

	// dsn is resolved from DSNEnv at load time. It is unexported so it cannot
	// be serialised back out into a log line or an API response.
	dsn string
}

// DSN returns the resolved connection string.
func (d Datasource) DSN() string { return d.dsn }

// Definition describes one metric.
type Definition struct {
	Key         string     `yaml:"key"`
	Description string     `yaml:"description"`
	Unit        string     `yaml:"unit"`
	Source      SourceType `yaml:"source"`

	// SQL source.
	Datasource string `yaml:"datasource"`
	Query      string `yaml:"query"`

	// HTTP source.
	URL          string            `yaml:"url"`
	Method       string            `yaml:"method"`
	Headers      map[string]string `yaml:"headers"`
	AuthHeader   string            `yaml:"authHeader"`
	AuthValueEnv string            `yaml:"authValueEnv"`
	// JSONPath is a dotted path into the response body, e.g. "data.mrr" or
	// "items.0.value".
	JSONPath string `yaml:"jsonPath"`

	authValue string
}

// AuthValue returns the resolved credential for an HTTP metric, if any.
func (d Definition) AuthValue() string { return d.authValue }

// file is the on-disk shape of the metrics config.
type file struct {
	Datasources []Datasource `yaml:"datasources"`
	Metrics     []Definition `yaml:"metrics"`
}

// Registry is an immutable set of metric definitions.
type Registry struct {
	metrics     map[string]Definition
	datasources map[string]Datasource
}

// Load reads and validates the metrics config at path, resolving every
// credential from the environment. Every problem found is reported at once.
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read metrics config %s: %w", path, err)
	}

	var parsed file
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse metrics config %s: %w", path, err)
	}

	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	datasources := make(map[string]Datasource, len(parsed.Datasources))
	for i, ds := range parsed.Datasources {
		label := fmt.Sprintf("datasources[%d]", i)
		ds.Name = strings.TrimSpace(ds.Name)
		if ds.Name == "" {
			fail("%s: name is required", label)
			continue
		}
		label = fmt.Sprintf("datasource %q", ds.Name)
		if _, exists := datasources[ds.Name]; exists {
			fail("%s: duplicate name", label)
			continue
		}
		if ds.Driver == "" {
			ds.Driver = "postgres"
		}
		if ds.Driver != "postgres" {
			fail("%s: driver %q is not supported, only postgres", label, ds.Driver)
		}
		if ds.MaxOpenConns <= 0 {
			ds.MaxOpenConns = defaultMaxOpenConns
		}
		if strings.TrimSpace(ds.DSNEnv) == "" {
			fail("%s: dsnEnv is required so the connection string stays out of this file", label)
		} else {
			ds.dsn = strings.TrimSpace(os.Getenv(ds.DSNEnv))
			if ds.dsn == "" {
				fail("%s: environment variable %s is not set", label, ds.DSNEnv)
			}
		}
		datasources[ds.Name] = ds
	}

	registry := make(map[string]Definition, len(parsed.Metrics))
	for i, def := range parsed.Metrics {
		label := fmt.Sprintf("metrics[%d]", i)
		def.Key = strings.TrimSpace(def.Key)
		if def.Key == "" {
			fail("%s: key is required", label)
			continue
		}
		label = fmt.Sprintf("metric %q", def.Key)
		if len(def.Key) > maxMetricKeyLength {
			fail("%s: key is longer than %d characters", label, maxMetricKeyLength)
		}
		if !metricKeyPattern.MatchString(def.Key) {
			fail("%s: key must be lowercase alphanumeric segments separated by . _ or -", label)
		}
		if _, exists := registry[def.Key]; exists {
			fail("%s: duplicate key", label)
			continue
		}

		switch def.Source {
		case SourceSQL:
			validateSQLMetric(&def, label, datasources, fail)
		case SourceHTTP:
			validateHTTPMetric(&def, label, fail)
		case SourcePush:
			if def.Query != "" || def.URL != "" {
				fail("%s: a push metric must not declare a query or url", label)
			}
		case "":
			fail("%s: source is required (sql, http or push)", label)
		default:
			fail("%s: unknown source %q, expected sql, http or push", label, def.Source)
		}

		registry[def.Key] = def
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("invalid metrics config %s:\n  - %s", path, strings.Join(problems, "\n  - "))
	}
	return &Registry{metrics: registry, datasources: datasources}, nil
}

func validateSQLMetric(def *Definition, label string, datasources map[string]Datasource, fail func(string, ...any)) {
	if strings.TrimSpace(def.Datasource) == "" {
		fail("%s: datasource is required for a sql metric", label)
	} else if _, ok := datasources[def.Datasource]; !ok {
		fail("%s: datasource %q is not declared", label, def.Datasource)
	}
	if err := ValidateQuery(def.Query); err != nil {
		fail("%s: %v", label, err)
	}
	if def.URL != "" {
		fail("%s: url is not valid for a sql metric", label)
	}
}

func validateHTTPMetric(def *Definition, label string, fail func(string, ...any)) {
	if def.Method == "" {
		def.Method = "GET"
	}
	def.Method = strings.ToUpper(def.Method)
	if def.Method != "GET" && def.Method != "POST" {
		fail("%s: method %q is not supported, use GET or POST", label, def.Method)
	}

	parsedURL, err := url.Parse(strings.TrimSpace(def.URL))
	switch {
	case strings.TrimSpace(def.URL) == "":
		fail("%s: url is required for an http metric", label)
	case err != nil:
		fail("%s: url is not parseable: %v", label, err)
	case parsedURL.Scheme != "http" && parsedURL.Scheme != "https":
		fail("%s: url scheme must be http or https", label)
	case parsedURL.Host == "":
		fail("%s: url has no host", label)
	}

	if strings.TrimSpace(def.JSONPath) == "" {
		fail("%s: jsonPath is required for an http metric", label)
	}
	if def.Query != "" {
		fail("%s: query is not valid for an http metric", label)
	}
	if def.AuthValueEnv != "" {
		if def.AuthHeader == "" {
			def.AuthHeader = "Authorization"
		}
		def.authValue = strings.TrimSpace(os.Getenv(def.AuthValueEnv))
		if def.authValue == "" {
			fail("%s: environment variable %s is not set", label, def.AuthValueEnv)
		}
	} else if def.AuthHeader != "" {
		fail("%s: authHeader is set but authValueEnv is not", label)
	}
}

// Get returns the definition for key.
func (r *Registry) Get(key string) (Definition, bool) {
	def, ok := r.metrics[key]
	return def, ok
}

// Keys returns every declared metric key, sorted.
func (r *Registry) Keys() []string {
	keys := make([]string, 0, len(r.metrics))
	for k := range r.metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Definitions returns every declared metric, sorted by key.
func (r *Registry) Definitions() []Definition {
	defs := make([]Definition, 0, len(r.metrics))
	for _, key := range r.Keys() {
		defs = append(defs, r.metrics[key])
	}
	return defs
}

// Datasources returns every declared datasource, sorted by name.
func (r *Registry) Datasources() []Datasource {
	names := make([]string, 0, len(r.datasources))
	for name := range r.datasources {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]Datasource, 0, len(names))
	for _, name := range names {
		out = append(out, r.datasources[name])
	}
	return out
}

// Len returns the number of declared metrics.
func (r *Registry) Len() int { return len(r.metrics) }
