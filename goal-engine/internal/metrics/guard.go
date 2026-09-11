package metrics

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// maxQueryLength bounds an operator-authored metric query. Anything longer is
// almost certainly not a single scalar aggregate.
const maxQueryLength = 8000

// ValidateQuery checks that a configured SQL metric query is a single read-only
// statement.
//
// This is defense in depth, not the primary control. The primary controls are
// (1) metric queries can only be declared in the operator-owned config file,
// never through the API, and (2) the sampler connects as a read-only user and
// runs inside a READ ONLY transaction. This guard exists so that a copy-pasted
// or mistyped query fails at startup with a clear message instead of at 3am
// against production data.
func ValidateQuery(raw string) error {
	query := strings.TrimSpace(raw)
	if query == "" {
		return errors.New("query is empty: a metric query must be a single SELECT statement")
	}
	if len(query) > maxQueryLength {
		return fmt.Errorf("query is %d bytes, limit is %d: a metric query should return one scalar value", len(query), maxQueryLength)
	}

	// Comments are the usual way to hide a payload from a scanner, and a scalar
	// metric query has no need for them.
	for _, marker := range []string{"--", "/*", "*/"} {
		if strings.Contains(query, marker) {
			return fmt.Errorf("query contains the comment marker %q: comments are not allowed in metric queries", marker)
		}
	}
	if dollarQuotePattern.MatchString(query) {
		return errors.New("query uses dollar quoting ($$): not allowed in metric queries")
	}
	if escapeStringPattern.MatchString(query) {
		return errors.New(`query uses escape string syntax (E'...'): use a plain '...' literal instead`)
	}

	// String literals are data, not code. Removing them first keeps the checks
	// below from tripping over a value that happens to read like SQL, such as
	// status = 'create'.
	code, err := stripStringLiterals(query)
	if err != nil {
		return err
	}
	code = strings.TrimSpace(code)

	// One trailing semicolon is a habit, not an attack. A second statement is.
	if strings.Contains(strings.TrimSuffix(code, ";"), ";") {
		return errors.New("query contains more than one statement: only a single SELECT is allowed")
	}

	lowered := strings.ToLower(code)
	if !queryPrefixPattern.MatchString(lowered) {
		return errors.New("query must start with SELECT or WITH")
	}
	if match := forbiddenWordPattern.FindString(lowered); match != "" {
		return fmt.Errorf("query contains the forbidden keyword %q: metric queries must be read-only and must not touch system objects", match)
	}
	return nil
}

var (
	dollarQuotePattern  = regexp.MustCompile(`\$[A-Za-z_]*\$`)
	escapeStringPattern = regexp.MustCompile(`(?i)\bE'`)
	queryPrefixPattern  = regexp.MustCompile(`^(?:select|with)\b`)

	// forbiddenWords are matched on word boundaries against the query with its
	// string literals removed. The list covers writes, DDL, DCL, session and
	// transaction control, dynamic SQL, and the filesystem, network and
	// credential surfaces reachable from SQL.
	forbiddenWords = []string{
		// Writes and DDL.
		"insert", "update", "delete", "merge", "truncate", "drop", "alter",
		"create", "reindex", "vacuum", "analyze", "cluster", "refresh", "import",
		// Privileges.
		"grant", "revoke", "security", "definer",
		// Session and transaction control.
		"begin", "commit", "rollback", "savepoint", "set", "reset", "discard",
		"lock", "listen", "unlisten", "notify", "do", "call", "copy",
		// Dynamic SQL.
		"prepare", "execute", "deallocate",
		// Filesystem, process and network reach.
		"pg_read_file", "pg_read_binary_file", "pg_read_server_files",
		"pg_execute_server_program", "pg_stat_file", "pg_ls_dir", "pg_file_write",
		"pg_logical_emit_message", "lo_import", "lo_export", "lo_get", "dblink",
		"postgres_fdw", "pg_sleep", "pg_terminate_backend", "pg_cancel_backend",
		"pg_reload_conf", "pg_rotate_logfile", "query_to_xml",
		// Credentials and settings.
		"pg_authid", "pg_shadow", "pg_user", "pg_roles", "pg_settings",
		"current_setting", "set_config",
	}

	forbiddenWordPattern = regexp.MustCompile(`\b(?:` + strings.Join(forbiddenWords, "|") + `)\b`)
)

// stripStringLiterals replaces every single-quoted literal with a space,
// preserving word boundaries. It reports an error for an unterminated literal,
// which is malformed SQL anyway.
func stripStringLiterals(query string) (string, error) {
	var out strings.Builder
	out.Grow(len(query))

	inLiteral := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		if inLiteral {
			if c != '\'' {
				continue
			}
			// '' is an escaped quote and stays inside the literal.
			if i+1 < len(query) && query[i+1] == '\'' {
				i++
				continue
			}
			inLiteral = false
			continue
		}
		if c == '\'' {
			inLiteral = true
			out.WriteByte(' ')
			continue
		}
		out.WriteByte(c)
	}
	if inLiteral {
		return "", errors.New("query has an unterminated string literal")
	}
	return out.String(), nil
}
