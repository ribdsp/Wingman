package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a policy file into a temporary directory and returns its
// path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policies.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const validConfig = `
policies:
  - actionType: ads.topup
    description: Top up an ad account
    currency: usd
    autoApproveBelow: 100000
    hardCap: 5000000
    dailyCap: 2000000
  - actionType: vendor.payment
    currency: USD
    autoApproveBelow: 0
    hardCap: 10000000
    dailyCap: 10000000
    enabled: false
`

func TestLoadReadsAndNormalisesPolicies(t *testing.T) {
	// Arrange
	path := writeConfig(t, validConfig)

	// Act
	registry, err := Load(path)

	// Assert
	if err != nil {
		t.Fatalf("expected the config to load, got %v", err)
	}
	if registry.Len() != 2 {
		t.Fatalf("expected 2 policies, got %d", registry.Len())
	}

	topup, ok := registry.Get("ads.topup")
	if !ok {
		t.Fatal("expected ads.topup to be declared")
	}
	if topup.Currency != "USD" {
		t.Fatalf("expected the currency to be upper-cased, got %q", topup.Currency)
	}
	if !topup.Enabled {
		t.Fatal("expected an omitted enabled flag to default to true")
	}
	if topup.AutoApproveBelow != 100000 || topup.HardCap != 5000000 || topup.DailyCap != 2000000 {
		t.Fatalf("unexpected thresholds %+v", topup)
	}
}

func TestLoadKeepsAnExplicitlyDisabledPolicy(t *testing.T) {
	// A disabled policy is not the same as a missing one: it is a declaration that
	// this action type is currently off, and the decision core denies it outright
	// rather than asking a human.
	registry, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("expected the config to load, got %v", err)
	}

	payment, ok := registry.Get("vendor.payment")
	if !ok {
		t.Fatal("expected vendor.payment to be declared")
	}
	if payment.Enabled {
		t.Fatal("expected enabled: false to be honoured")
	}
}

func TestLookupReturnsNilForAnUndeclaredAction(t *testing.T) {
	// Callers pass this straight into domain.ApprovalRequest, where nil means
	// "ask a human". Returning a zero-valued policy instead would read as a
	// policy with every cap set to zero.
	registry, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("expected the config to load, got %v", err)
	}

	if got := registry.Lookup("payroll.transfer"); got != nil {
		t.Fatalf("expected nil for an undeclared action, got %+v", got)
	}
	if got := registry.Lookup("ads.topup"); got == nil {
		t.Fatal("expected a policy for a declared action")
	}
}

func TestLookupHandsBackACopy(t *testing.T) {
	// An agent-facing service holds this pointer. If it aliased the registry, one
	// careless assignment would raise every future limit for that action type.
	registry, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("expected the config to load, got %v", err)
	}

	first := registry.Lookup("ads.topup")
	first.HardCap = 999999999

	second := registry.Lookup("ads.topup")
	if second.HardCap != 5000000 {
		t.Fatalf("expected the registry to be unchanged, got %g", second.HardCap)
	}
}

func TestLoadRejectsACapBelowTheAutoApproveLine(t *testing.T) {
	// This is the dangerous misconfiguration: it looks like a limit, but every
	// request under the auto-approve line is already through before the cap is
	// consulted, so the cap can never bite.
	path := writeConfig(t, `
policies:
  - actionType: ads.topup
    currency: USD
    autoApproveBelow: 9000000
    hardCap: 5000000
    dailyCap: 2000000
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected the config to be rejected")
	}
	for _, want := range []string{"hardCap", "dailyCap"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %s to be reported, got %v", want, err)
		}
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	path := writeConfig(t, `
policies:
  - actionType: ""
    currency: USD
    hardCap: 1
    dailyCap: 1
  - actionType: Ads.Topup
    currency: dollars
    hardCap: 0
    dailyCap: -5
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected the config to be rejected")
	}
	for _, want := range []string{
		"actionType is required",
		"lowercase alphanumeric",
		"3-letter code",
		"hardCap is required",
		"must not be negative",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q to be reported, got %v", want, err)
		}
	}
}

func TestLoadRejectsDuplicateActionTypes(t *testing.T) {
	// Silently keeping the last one would mean the file no longer describes what
	// the engine enforces.
	path := writeConfig(t, `
policies:
  - actionType: ads.topup
    currency: USD
    hardCap: 100
    dailyCap: 100
  - actionType: ads.topup
    currency: USD
    hardCap: 999999
    dailyCap: 999999
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate actionType") {
		t.Fatalf("expected a duplicate to be reported, got %v", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	// A typo in a limit name would otherwise load as a policy with that limit
	// unset, which is exactly the failure this package exists to prevent.
	path := writeConfig(t, `
policies:
  - actionType: ads.topup
    currency: USD
    hardCap: 100
    dailyCap: 100
    dailyCapp: 5
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "dailyCapp") {
		t.Fatalf("expected the unknown field to be reported, got %v", err)
	}
}

func TestLoadRejectsAMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("expected a missing file to be an error")
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	_, err := Load(writeConfig(t, "policies: [oops"))
	if err == nil {
		t.Fatal("expected malformed yaml to be an error")
	}
}

func TestLoadAcceptsAnEmptyPolicySet(t *testing.T) {
	// An empty file is a valid, maximally cautious configuration: nothing is
	// auto-approvable, so every spend request goes to a human.
	registry, err := Load(writeConfig(t, "policies: []\n"))
	if err != nil {
		t.Fatalf("expected an empty config to load, got %v", err)
	}
	if registry.Len() != 0 {
		t.Fatalf("expected no policies, got %d", registry.Len())
	}
	if got := registry.ActionTypes(); len(got) != 0 {
		t.Fatalf("expected no action types, got %v", got)
	}
}

func TestPoliciesAndActionTypesAreSorted(t *testing.T) {
	registry, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("expected the config to load, got %v", err)
	}

	types := registry.ActionTypes()
	want := []string{"ads.topup", "vendor.payment"}
	if len(types) != len(want) {
		t.Fatalf("expected %v, got %v", want, types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, types)
		}
	}

	policies := registry.Policies()
	if len(policies) != 2 || policies[0].ActionType != "ads.topup" {
		t.Fatalf("expected policies sorted by action type, got %+v", policies)
	}
}

func TestLoadTrimsSurroundingWhitespace(t *testing.T) {
	registry, err := Load(writeConfig(t, `
policies:
  - actionType: "  ads.topup  "
    currency: "  usd  "
    hardCap: 100
    dailyCap: 100
`))
	if err != nil {
		t.Fatalf("expected the config to load, got %v", err)
	}
	if _, ok := registry.Get("ads.topup"); !ok {
		t.Fatalf("expected the trimmed key to be usable, got %v", registry.ActionTypes())
	}
}
