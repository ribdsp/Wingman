// Package policy declares the spending limits autonomous actions are checked
// against.
//
// Policies live in an operator-owned YAML file and are loaded once at startup.
// They are deliberately not writable through the API: an agent that could edit
// its own spending limits has no limits. An action type with no policy is not
// unlimited — the decision core treats a missing policy as "ask a human".
package policy

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
)

const maxActionTypeLength = 120

var (
	actionTypePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	currencyPattern   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// entry is the on-disk shape of one policy. It mirrors domain.ApprovalPolicy but
// stays separate so the YAML contract can change without moving the decision
// core, and so YAML can never set a field the core does not expose.
type entry struct {
	ActionType string `yaml:"actionType"`
	// Description is for the humans reading the file, not for the engine.
	Description      string  `yaml:"description"`
	Currency         string  `yaml:"currency"`
	AutoApproveBelow float64 `yaml:"autoApproveBelow"`
	HardCap          float64 `yaml:"hardCap"`
	DailyCap         float64 `yaml:"dailyCap"`
	// Enabled defaults to true when omitted, because a policy an operator wrote
	// down is meant to apply. Disabling one is an explicit act.
	Enabled *bool `yaml:"enabled"`
}

type file struct {
	Policies []entry `yaml:"policies"`
}

// Registry is an immutable set of approval policies.
type Registry struct {
	policies map[string]domain.ApprovalPolicy
}

// Load reads and validates the policy config at path. Every problem found is
// reported at once, so an operator fixes one file rather than one line per
// restart.
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy config %s: %w", path, err)
	}

	var parsed file
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse policy config %s: %w", path, err)
	}

	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	policies := make(map[string]domain.ApprovalPolicy, len(parsed.Policies))
	for i, raw := range parsed.Policies {
		label := fmt.Sprintf("policies[%d]", i)
		actionType := strings.TrimSpace(raw.ActionType)
		if actionType == "" {
			fail("%s: actionType is required", label)
			continue
		}
		label = fmt.Sprintf("policy %q", actionType)
		if len(actionType) > maxActionTypeLength {
			fail("%s: actionType is longer than %d characters", label, maxActionTypeLength)
		}
		if !actionTypePattern.MatchString(actionType) {
			fail("%s: actionType must be lowercase alphanumeric segments separated by . _ or -", label)
		}
		if _, exists := policies[actionType]; exists {
			fail("%s: duplicate actionType", label)
			continue
		}

		currency := strings.ToUpper(strings.TrimSpace(raw.Currency))
		if currency == "" {
			fail("%s: currency is required, otherwise a cap has no unit", label)
		} else if !currencyPattern.MatchString(currency) {
			fail("%s: currency %q must be a 3-letter code", label, raw.Currency)
		}

		validateLimits(raw, label, fail)

		enabled := true
		if raw.Enabled != nil {
			enabled = *raw.Enabled
		}
		policies[actionType] = domain.ApprovalPolicy{
			ActionType:       actionType,
			Currency:         currency,
			AutoApproveBelow: raw.AutoApproveBelow,
			HardCap:          raw.HardCap,
			DailyCap:         raw.DailyCap,
			Enabled:          enabled,
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("invalid policy config %s:\n  - %s", path, strings.Join(problems, "\n  - "))
	}
	return &Registry{policies: policies}, nil
}

// validateLimits checks the three thresholds relate to each other sensibly. A
// cap below the auto-approve line is the dangerous case: it reads as a limit but
// every request under the line would already have been approved.
func validateLimits(raw entry, label string, fail func(string, ...any)) {
	for name, value := range map[string]float64{
		"autoApproveBelow": raw.AutoApproveBelow,
		"hardCap":          raw.HardCap,
		"dailyCap":         raw.DailyCap,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			fail("%s: %s must be a finite number", label, name)
		}
		if value < 0 {
			fail("%s: %s must not be negative", label, name)
		}
	}

	if raw.HardCap <= 0 {
		fail("%s: hardCap is required and must be positive", label)
	}
	if raw.DailyCap <= 0 {
		fail("%s: dailyCap is required and must be positive", label)
	}
	if raw.HardCap > 0 && raw.AutoApproveBelow > raw.HardCap {
		fail("%s: autoApproveBelow (%g) is above hardCap (%g), so the cap would never be reached",
			label, raw.AutoApproveBelow, raw.HardCap)
	}
	if raw.DailyCap > 0 && raw.AutoApproveBelow > raw.DailyCap {
		fail("%s: autoApproveBelow (%g) is above dailyCap (%g), so a single action could clear the whole day",
			label, raw.AutoApproveBelow, raw.DailyCap)
	}
}

// Get returns the policy for an action type. The second result is false when no
// policy is declared, which callers must treat as "requires a human" rather than
// as "no limit".
func (r *Registry) Get(actionType string) (domain.ApprovalPolicy, bool) {
	p, ok := r.policies[strings.TrimSpace(actionType)]
	return p, ok
}

// Lookup returns a pointer suitable for domain.ApprovalRequest, or nil when no
// policy is declared.
func (r *Registry) Lookup(actionType string) *domain.ApprovalPolicy {
	p, ok := r.Get(actionType)
	if !ok {
		return nil
	}
	// A copy is returned so a caller cannot reach back and edit the registry.
	return &p
}

// ActionTypes returns every declared action type, sorted.
func (r *Registry) ActionTypes() []string {
	types := make([]string, 0, len(r.policies))
	for actionType := range r.policies {
		types = append(types, actionType)
	}
	sort.Strings(types)
	return types
}

// Policies returns every declared policy, sorted by action type.
func (r *Registry) Policies() []domain.ApprovalPolicy {
	out := make([]domain.ApprovalPolicy, 0, len(r.policies))
	for _, actionType := range r.ActionTypes() {
		out = append(out, r.policies[actionType])
	}
	return out
}

// Len returns the number of declared policies.
func (r *Registry) Len() int { return len(r.policies) }
