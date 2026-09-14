package tool

import (
	"context"
	"fmt"
	"strings"
)

// ShellToolName is the name the built-in shell is granted under. An operator who
// wants an agent to be able to run commands writes this name in tools.yaml; until
// then the tool exists and is never offered.
const ShellToolName = "shell"

// ExecResult is what a sandbox reported about one command.
//
// A non-zero ExitCode is not an error. The command ran, it failed, and the model is
// supposed to read the failure and try something else — so it comes back as output
// with Result.IsError set, not as a Go error. A Go error from Exec means the command
// could not be run at all.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Sandbox is the part of internal/sandbox this package needs.
//
// It is declared here, where it is used, and in terms of types declared here, so
// that internal/tool does not import internal/sandbox and internal/sandbox does not
// import internal/tool. The backends satisfy it through a one-line adapter in
// cmd/core, which is where the wiring belongs; the alternative is one of the two
// packages depending on the other's types for no reason beyond convenience.
type Sandbox interface {
	// Exec runs script inside workspace and returns what it printed. The context
	// carries the sandbox timeout; Exec must respect it, because it is the only
	// thing standing between a `sleep infinity` and a worker that never returns.
	Exec(ctx context.Context, workspace, script string) (ExecResult, error)
}

// Shell is the built-in tool that runs a shell script in the run's workspace.
type Shell struct {
	sandbox Sandbox
}

// NewShell builds the runner.
func NewShell(sandbox Sandbox) *Shell { return &Shell{sandbox: sandbox} }

// Label identifies this runner.
func (s *Shell) Label() string { return "builtin:" + ShellToolName }

// Tools describes the one tool this runner offers.
//
// The description is written for the model rather than for an operator: it says what
// the environment is, because a model that does not know the sandbox has no network
// will spend a tool call finding out, and that call is billed.
func (s *Shell) Tools(context.Context) ([]Definition, error) {
	return []Definition{{
		Name:   ShellToolName,
		Source: SourceBuiltin,
		Origin: ShellToolName,
		Description: "Run a shell script in this run's own workspace directory. " +
			"The workspace persists for the whole run. Output is combined stdout and " +
			"stderr, truncated in the middle if it is long. Network access may be " +
			"switched off, and the script is killed if it outlives the sandbox timeout.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"script": map[string]any{
					"type":        "string",
					"description": "The shell script to run.",
				},
			},
			"required": []any{"script"},
			// The model is told not to invent arguments, since anything else here
			// would be silently dropped and it would keep sending them.
			"additionalProperties": false,
		},
	}}, nil
}

// Call runs the script.
func (s *Shell) Call(ctx context.Context, invocation Invocation) (Result, error) {
	if invocation.Name != ShellToolName {
		return Result{}, fmt.Errorf("shell runner was asked for tool %q", invocation.Name)
	}
	if s.sandbox == nil {
		return Result{}, fmt.Errorf("tool %s: no sandbox is configured", ShellToolName)
	}
	if strings.TrimSpace(invocation.Workspace) == "" {
		// Refused rather than defaulted to the working directory. The working
		// directory of this process is the service's own installation, so a missing
		// workspace would turn a sandboxed tool call into a command on the host as
		// whatever user the service runs as.
		return Result{}, fmt.Errorf("tool %s: no workspace was given for this run", ShellToolName)
	}

	arguments, err := invocation.Arguments()
	if err != nil {
		return Result{}, err
	}
	script, err := StringArgument(arguments, "script")
	if err != nil {
		// The model's mistake, not the loop's: it called the tool with arguments the
		// tool does not take. Handing that back as output lets it correct itself,
		// where a Go error would stop the run.
		return Result{Content: fmt.Sprintf("error: %v", err), IsError: true}, nil
	}

	executed, err := s.sandbox.Exec(ctx, invocation.Workspace, script)
	if err != nil {
		return Result{}, fmt.Errorf("tool %s: %w", ShellToolName, err)
	}
	return shellResult(executed), nil
}

// shellResult flattens a command's three outputs into the one string a model reads.
func shellResult(executed ExecResult) Result {
	var content strings.Builder
	stdout := strings.TrimRight(executed.Stdout, "\n")
	stderr := strings.TrimRight(executed.Stderr, "\n")

	if stdout != "" {
		content.WriteString(stdout)
	}
	if stderr != "" {
		if content.Len() > 0 {
			content.WriteString("\n")
		}
		// Labelled, because the two streams mean different things and a model that
		// cannot tell them apart reads a warning as a result.
		content.WriteString("[stderr]\n")
		content.WriteString(stderr)
	}
	if executed.ExitCode != 0 {
		if content.Len() > 0 {
			content.WriteString("\n")
		}
		// Always stated for a failure, even when the command printed nothing at all —
		// otherwise a silent failure and a silent success look identical.
		content.WriteString(fmt.Sprintf("[exit status %d]", executed.ExitCode))
	}

	return Result{Content: content.String(), IsError: executed.ExitCode != 0}
}
