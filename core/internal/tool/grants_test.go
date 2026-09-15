package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// writeGrants puts a grant file on disk and returns its path.
func writeGrants(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tools.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write grant file: %v", err)
	}
	return path
}

func TestLoadGrants_readsWhatTheOperatorDeclared(t *testing.T) {
	// Arrange
	path := writeGrants(t, `
tools:
  - name: shell
    class: read
    description: Run commands in the sandbox
  - name: post_message
    class: write
  - name: pay_invoice
    class: spend
    spend:
      actionType: supplier.invoice
      amountArgument: amount
      currency: usd
  - name: delete_customer
    class: write
    enabled: false
`)

	// Act
	grants, err := LoadGrants(path)

	// Assert
	if err != nil {
		t.Fatalf("a valid grant file was refused: %v", err)
	}
	if grants.Len() != 4 {
		t.Errorf("loaded %d grants; want 4", grants.Len())
	}

	shell, declared := grants.Get("shell")
	if !declared {
		t.Fatal("shell was not loaded")
	}
	if shell.Class != domain.ToolClassRead {
		t.Errorf("shell class = %q; want read", shell.Class)
	}
	// Omitted means on: a tool an operator wrote down is meant to be usable, and
	// switching one off is an explicit act.
	if !shell.Enabled {
		t.Error("a grant with no enabled field was loaded switched off")
	}

	withdrawn, declared := grants.Get("delete_customer")
	if !declared {
		// Kept rather than dropped, so the record of what was once permitted
		// survives being withdrawn.
		t.Fatal("a disabled grant was dropped instead of loaded")
	}
	if withdrawn.Enabled {
		t.Error("enabled: false was ignored")
	}

	paying, _ := grants.Get("pay_invoice")
	if paying.Spend == nil {
		t.Fatal("the spending contract was dropped; ClassifyTool would deny a tool the operator granted")
	}
	if paying.Spend.ActionType != "supplier.invoice" || paying.Spend.AmountArgument != "amount" {
		t.Errorf("spend contract = %+v; want the declared action and amount argument", *paying.Spend)
	}
	// Upper-cased on the way in, because the goal engine compares the currency
	// against its policy and would refuse "usd" against a policy written in "USD".
	if paying.Spend.Currency != "USD" {
		t.Errorf("spend currency = %q; want USD", paying.Spend.Currency)
	}
}

func TestLoadGrants_refusesAFileThatIsNotThere(t *testing.T) {
	// Arrange, Act
	_, err := LoadGrants(filepath.Join(t.TempDir(), "absent.yaml"))

	// Assert
	// Not an empty registry. "The operator granted nothing" and "the file is not
	// where the service is looking" are different situations, and only one is fine.
	if err == nil {
		t.Fatal("a missing grant file was read as an empty one")
	}
}

func TestLoadGrants_acceptsAFileThatGrantsNothing(t *testing.T) {
	// Arrange
	// Both spellings of empty: the explicit list config/tools.yaml ships with, and a
	// file whose every line is a comment because the operator deleted the list.
	cases := map[string]string{
		"an explicit empty list": "tools: []\n",
		"comments only":          "# nothing granted yet\n",
	}

	// Act, Assert
	for label, body := range cases {
		grants, err := LoadGrants(writeGrants(t, body))
		if err != nil {
			t.Errorf("%s was refused: %v", label, err)
			continue
		}
		if grants.Len() != 0 {
			t.Errorf("%s loaded %d grants", label, grants.Len())
		}
		// Nothing callable is the state the file ships in, and it has to be a
		// working state rather than one that stops the service from starting.
		if verdict := grants.Classify("shell", true); verdict.Allowed {
			t.Errorf("%s allowed a call anyway", label)
		}
	}
}

func TestLoadGrants_refusesAFieldNobodyDeclared(t *testing.T) {
	// Arrange
	// `enable` rather than `enabled`. Ignoring the unknown key would leave the tool
	// on, which is the opposite of what was written down.
	path := writeGrants(t, "tools:\n  - name: pay_invoice\n    class: spend\n    enable: false\n")

	// Act
	_, err := LoadGrants(path)

	// Assert
	if err == nil {
		t.Fatal("a misspelled field was accepted")
	}
}

func TestLoadGrants_refusesMalformedYAML(t *testing.T) {
	// Arrange, Act
	_, err := LoadGrants(writeGrants(t, "tools: [oops\n"))

	// Assert
	if err == nil {
		t.Fatal("malformed yaml was accepted")
	}
}

func TestLoadGrants_reportsEveryProblemAtOnce(t *testing.T) {
	// Arrange
	// An operator fixes one file rather than one line per restart — the same rule the
	// goal engine's policy loader follows.
	path := writeGrants(t, `
tools:
  - name: ""
    class: read
  - name: reports.read
    class: read
  - name: mystery
    class: wrtie
  - name: unclassified
  - name: shell
    class: read
  - name: shell
    class: write
`)

	// Act
	_, err := LoadGrants(path)

	// Assert
	if err == nil {
		t.Fatal("a file with six problems was accepted")
	}
	message := err.Error()
	for _, want := range []string{
		"name is required",
		`"reports.read"`,
		`"wrtie"`,
		"class is required",
		"declared twice",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the report does not mention %q:\n%s", want, message)
		}
	}
}

func TestLoadGrants_refusesANameNoProviderWouldAccept(t *testing.T) {
	// Arrange
	// Caught at startup rather than on the wire. A name outside the vendors' pattern
	// makes them reject the entire request, so this one tool would break every call
	// the run makes.
	cases := map[string]string{
		"a dot":       "reports.read",
		"a space":     "read report",
		"a slash":     "read/report",
		"64 and over": strings.Repeat("x", 65),
	}

	// Act, Assert
	for label, name := range cases {
		_, err := LoadGrants(writeGrants(t, "tools:\n  - name: \""+name+"\"\n    class: read\n"))
		if err == nil {
			t.Errorf("%s was accepted as a tool name", label)
		}
	}
}

func TestLoadGrants_refusesAGrantWithNoUsableClass(t *testing.T) {
	// Arrange
	// A missing class is a sentence somebody did not finish. ClassifyTool would deny
	// the call at runtime, but an operator hearing about it at startup is the
	// difference between a typo and an outage at 04:00.
	cases := map[string]string{
		"absent":  "tools:\n  - name: pay_invoice\n",
		"empty":   "tools:\n  - name: pay_invoice\n    class: \"\"\n",
		"unknown": "tools:\n  - name: pay_invoice\n    class: admin\n",
	}

	// Act, Assert
	for label, body := range cases {
		if _, err := LoadGrants(writeGrants(t, body)); err == nil {
			t.Errorf("a %s class was accepted", label)
		}
	}
}

func TestLoadGrants_readsAClassRegardlessOfHowItWasCapitalised(t *testing.T) {
	// Arrange
	path := writeGrants(t, "tools:\n  - name: pay_invoice\n    class: SPEND\n"+
		"    spend:\n      actionType: supplier.invoice\n      amountArgument: amount\n      currency: USD\n")

	// Act
	grants, err := LoadGrants(path)

	// Assert
	// Case is not the operator's mistake to pay for: "Spend" plainly means spend, and
	// reading it as an unknown class would deny a tool they granted.
	if err != nil {
		t.Fatalf("an upper-case class was refused: %v", err)
	}
	if grant, _ := grants.Get("pay_invoice"); grant.Class != domain.ToolClassSpend {
		t.Errorf("class = %q; want spend", grant.Class)
	}
}

func TestLoadGrants_refusesASpendingToolWithAnIncompleteContract(t *testing.T) {
	// Arrange
	// The gate needs a policy key, an amount and a unit. Missing any of them, the
	// request it receives is one it cannot decide — so this is caught here, where the
	// message names the line, rather than at runtime as a tool that is always denied.
	cases := map[string]string{
		"no spend block":     "tools:\n  - name: pay_invoice\n    class: spend\n",
		"no action type":     "tools:\n  - name: pay_invoice\n    class: spend\n    spend:\n      amountArgument: amount\n      currency: USD\n",
		"no amount argument": "tools:\n  - name: pay_invoice\n    class: spend\n    spend:\n      actionType: supplier.invoice\n      currency: USD\n",
		"no currency":        "tools:\n  - name: pay_invoice\n    class: spend\n    spend:\n      actionType: supplier.invoice\n      amountArgument: amount\n",
		"blank fields":       "tools:\n  - name: pay_invoice\n    class: spend\n    spend:\n      actionType: \"  \"\n      amountArgument: \"  \"\n      currency: \"  \"\n",
	}

	// Act, Assert
	for label, body := range cases {
		if _, err := LoadGrants(writeGrants(t, body)); err == nil {
			t.Errorf("a spending grant with %s was accepted", label)
		}
	}
}

func TestLoadGrants_refusesASpendContractOnAToolThatDoesNotSpend(t *testing.T) {
	// Arrange
	// Ignored, this would sit in the file as somebody's intention, and whoever later
	// widened the class would inherit a contract nobody reviewed.
	body := "tools:\n  - name: read_report\n    class: read\n" +
		"    spend:\n      actionType: supplier.invoice\n      amountArgument: amount\n      currency: USD\n"

	// Act
	_, err := LoadGrants(writeGrants(t, body))

	// Assert
	if err == nil {
		t.Fatal("a read-only tool with a spending contract was accepted")
	}
	if !strings.Contains(err.Error(), "class spend") {
		t.Errorf("the message does not say which class a spend block belongs to: %v", err)
	}
}

func TestGrants_classify_isTheDomainLadderAndNotASecondOne(t *testing.T) {
	// Arrange
	// The verdicts belong to domain.ClassifyTool, which has a test per branch. What
	// is being checked here is that this package asks it rather than deciding for
	// itself — a second gate in front of the first is a weaker copy of it, and the
	// weaker of two gates is the one that decides.
	grants := NewGrants(
		domain.ToolGrant{Name: "read_report", Class: domain.ToolClassRead, Enabled: true},
		domain.ToolGrant{Name: "post_message", Class: domain.ToolClassWrite, Enabled: true},
		domain.ToolGrant{Name: "pay_invoice", Class: domain.ToolClassSpend, Enabled: true,
			Spend: &domain.ToolSpend{ActionType: "supplier.invoice", AmountArgument: "amount", Currency: "USD"}},
		domain.ToolGrant{Name: "delete_customer", Class: domain.ToolClassWrite, Enabled: false},
	)

	cases := []struct {
		name          string
		attended      bool
		allowed       bool
		needsApproval bool
	}{
		{name: "read_report", attended: false, allowed: true},
		{name: "post_message", attended: true, allowed: true},
		{name: "post_message", attended: false, needsApproval: true},
		{name: "pay_invoice", attended: true, needsApproval: true},
		{name: "delete_customer", attended: true},
		{name: "never_declared", attended: true},
	}

	// Act, Assert
	for _, c := range cases {
		verdict := grants.Classify(c.name, c.attended)
		if verdict.Allowed != c.allowed {
			t.Errorf("%s attended=%v: allowed = %v; want %v", c.name, c.attended, verdict.Allowed, c.allowed)
		}
		if verdict.NeedsApproval != c.needsApproval {
			t.Errorf("%s attended=%v: needsApproval = %v; want %v",
				c.name, c.attended, verdict.NeedsApproval, c.needsApproval)
		}
		if !verdict.Allowed && !verdict.NeedsApproval && verdict.Stop != domain.StopToolDenied {
			t.Errorf("%s attended=%v: stop reason = %q; want tool_denied", c.name, c.attended, verdict.Stop)
		}
	}
}

func TestGrants_names_areSortedSoTheToolListDoesNotReorderBetweenRuns(t *testing.T) {
	// Arrange
	grants := NewGrants(
		domain.ToolGrant{Name: "shell", Class: domain.ToolClassRead, Enabled: true},
		domain.ToolGrant{Name: "pay_invoice", Class: domain.ToolClassSpend, Enabled: false},
		domain.ToolGrant{Name: "post_message", Class: domain.ToolClassWrite, Enabled: true},
	)

	// Act
	names := grants.Names()

	// Assert
	want := []string{"pay_invoice", "post_message", "shell"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v; want %v", names, want)
	}
	// Disabled grants are counted: Len answers "what has been declared", which is the
	// question an operator reading the file is asking.
	if grants.Len() != 3 {
		t.Errorf("len = %d; want 3", grants.Len())
	}
}

func TestNewGrants_trimsNamesSoAConfiguredSpaceDoesNotDenySilently(t *testing.T) {
	// Arrange, Act
	grants := NewGrants(domain.ToolGrant{Name: "  shell  ", Class: domain.ToolClassRead, Enabled: true})

	// Assert
	if _, declared := grants.Get("shell"); !declared {
		t.Error("a name with surrounding space was stored under it")
	}
}
