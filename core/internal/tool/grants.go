package tool

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// spendEntry is the on-disk shape of a spending contract. Required for a tool
// declared as spend, refused on any other, because a read-only tool with an
// actionType reads as though somebody meant it to be able to pay.
type spendEntry struct {
	ActionType     string `yaml:"actionType"`
	AmountArgument string `yaml:"amountArgument"`
	Currency       string `yaml:"currency"`
}

// grantEntry is the on-disk shape of one grant. It mirrors domain.ToolGrant but
// stays separate so the YAML contract can change without moving the decision core,
// and so YAML can never set a field the core does not expose.
type grantEntry struct {
	Name  string `yaml:"name"`
	Class string `yaml:"class"`
	// Description is for the humans reading the file. Nothing sends it to a model —
	// the tool's own description comes from the source that implements it, which is
	// the one that knows what it does.
	Description string `yaml:"description"`
	// Enabled defaults to true when omitted, because a tool an operator wrote down
	// is meant to be usable. Switching one off is an explicit act, and it is how a
	// tool is withdrawn without losing the record that it was once permitted.
	Enabled *bool       `yaml:"enabled"`
	Spend   *spendEntry `yaml:"spend"`
}

type grantFile struct {
	Tools []grantEntry `yaml:"tools"`
}

// Grants is an immutable set of tool declarations.
//
// It is loaded once at startup from a file no API writes. That is the same rule the
// goal engine applies to metric definitions and spending policies, and for the same
// reason: an agent that can add to the list of things it is allowed to do is an
// agent with no list.
type Grants struct {
	grants map[string]domain.ToolGrant
}

// LoadGrants reads and validates the grant file at path.
//
// Every problem is reported at once, so an operator fixes one file rather than one
// line per restart. A file that parses to nothing is accepted: it means no tool is
// callable, which is the safe state to ship and the state config/tools.yaml ships
// in.
func LoadGrants(path string) (*Grants, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		// A missing file is an error rather than an empty registry. "The operator
		// granted nothing" and "the operator's file is not where the service is
		// looking" are different situations, and only one of them is fine.
		return nil, fmt.Errorf("read tool grants %s: %w", path, err)
	}

	var parsed grantFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	// KnownFields, so `enable: false` is an error rather than a tool that is quietly
	// still on.
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse tool grants %s: %w", path, err)
	}
	// EOF means the file holds no document at all — every line is a comment, or the
	// operator deleted the list. That is the same configuration as an empty one:
	// nothing is granted, so nothing is callable.

	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	grants := make(map[string]domain.ToolGrant, len(parsed.Tools))
	for i, entry := range parsed.Tools {
		label := fmt.Sprintf("tools[%d]", i)
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			fail("%s: name is required", label)
			continue
		}
		label = fmt.Sprintf("tool %q", name)
		if !ValidName(name) {
			fail("%s: name must be 1-%d characters of letters, digits, _ or -, because both providers reject anything else",
				label, maxNameLength)
		}
		if _, exists := grants[name]; exists {
			fail("%s: declared twice", label)
			continue
		}

		class := domain.ToolClass(strings.ToLower(strings.TrimSpace(entry.Class)))
		switch class {
		case domain.ToolClassRead, domain.ToolClassWrite, domain.ToolClassSpend:
		case "":
			// Not defaulted to read. A grant with no class is a sentence somebody
			// did not finish, and finishing it with the most permissive reading is
			// how a payment tool ends up needing no approval.
			fail("%s: class is required and must be one of read, write, spend", label)
		default:
			fail("%s: class %q is not one of read, write, spend", label, entry.Class)
		}

		enabled := true
		if entry.Enabled != nil {
			enabled = *entry.Enabled
		}
		grants[name] = domain.ToolGrant{
			Name:    name,
			Class:   class,
			Enabled: enabled,
			Spend:   spendContract(class, entry.Spend, label, fail),
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("invalid tool grants %s:\n  - %s", path, strings.Join(problems, "\n  - "))
	}
	return &Grants{grants: grants}, nil
}

// spendContract validates the spend block against the class that was declared, and
// reports every problem with it through fail.
//
// It returns nil for anything other than a valid spend grant. domain.ClassifyTool
// denies a spend tool with no contract, so a file that failed validation could not
// pay anything even if it were somehow loaded — but this is the layer that says
// which line is wrong, and that is the difference between a fixable message and a
// tool that is mysteriously always denied.
func spendContract(class domain.ToolClass, entry *spendEntry, label string, fail func(string, ...any)) *domain.ToolSpend {
	if class != domain.ToolClassSpend {
		if entry != nil {
			// Refused rather than ignored. An actionType on a read-only tool is
			// somebody's intention written down, and the next person to widen the
			// class would inherit a contract nobody reviewed.
			fail("%s: spend is only for class spend, and this tool is %q", label, class)
		}
		return nil
	}
	if entry == nil {
		fail("%s: class spend requires a spend block with actionType, amountArgument and currency", label)
		return nil
	}

	contract := domain.ToolSpend{
		ActionType:     strings.TrimSpace(entry.ActionType),
		AmountArgument: strings.TrimSpace(entry.AmountArgument),
		Currency:       strings.ToUpper(strings.TrimSpace(entry.Currency)),
	}
	if contract.ActionType == "" {
		// The key the goal engine's policy is written against. Without it every call
		// files under the empty action, which has no policy and so escalates to a
		// human every time — a queue nobody can act on rather than a gate.
		fail("%s: spend.actionType is required, and must match an action in the goal engine's policy file", label)
	}
	if contract.AmountArgument == "" {
		fail("%s: spend.amountArgument is required: the gate needs the amount this call would spend", label)
	}
	if contract.Currency == "" {
		// Not defaulted. The gate compares the currency against its policy and
		// refuses a mismatch, so guessing one here turns an operator's omission into
		// a denial they cannot explain.
		fail("%s: spend.currency is required", label)
	}
	if contract.ActionType == "" || contract.AmountArgument == "" || contract.Currency == "" {
		return nil
	}
	return &contract
}

// NewGrants builds a set in memory. It exists for tests and for the CLI
// subcommands; the running service loads its grants from the operator's file.
func NewGrants(grants ...domain.ToolGrant) *Grants {
	set := make(map[string]domain.ToolGrant, len(grants))
	for _, grant := range grants {
		set[strings.TrimSpace(grant.Name)] = grant
	}
	return &Grants{grants: set}
}

// Get returns the grant for a name. The second result is false when nothing was
// declared, which callers must treat as denied rather than as unrestricted.
func (g *Grants) Get(name string) (domain.ToolGrant, bool) {
	grant, ok := g.grants[strings.TrimSpace(name)]
	return grant, ok
}

// Classify is the per-call gate: domain.ClassifyTool against these grants.
//
// It is a method rather than an exported map so no caller can hold a reference to
// the set and edit it, and so the decision keeps living in the pure package where
// every branch of it has a test.
func (g *Grants) Classify(name string, attended bool) domain.ToolVerdict {
	return domain.ClassifyTool(g.grants, name, attended)
}

// Names returns every declared name, sorted.
func (g *Grants) Names() []string {
	names := make([]string, 0, len(g.grants))
	for name := range g.grants {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Len returns how many tools are declared, switched off ones included.
func (g *Grants) Len() int { return len(g.grants) }
