package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Local runs a command on this host, as the user this service runs as.
//
// It is not isolation and does not pretend to be. A script that reads
// ~/.ssh/id_rsa will read it; one that runs `curl` reaches the internet; one that
// fills the disk fills the operator's disk. What it does have is the workspace as its
// working directory, a minimal environment with none of this process's secrets in it,
// and a deadline that kills it.
//
// It exists because a laptop without a docker daemon should still be able to drive a
// run end to end, and because a single-operator box where the agent's tools are the
// operator's own tools is a real deployment. SANDBOX_BACKEND=docker is the one to run
// in front of other people, and deployment.md says so in those words.
type Local struct {
	workspaces *Workspaces
	// shell is the interpreter and its "run this string" flag, per platform.
	shell []string
}

// NewLocal builds the local backend.
//
// The shell is looked up here rather than at the first tool call, so that a box
// without one fails at startup where an operator is watching.
func NewLocal(workspaces *Workspaces) (*Local, error) {
	if workspaces == nil {
		return nil, errors.New("local sandbox: no workspaces were configured")
	}
	shell := defaultShell()
	if _, err := exec.LookPath(shell[0]); err != nil {
		return nil, fmt.Errorf("local sandbox: shell %s: %w", shell[0], err)
	}
	return &Local{workspaces: workspaces, shell: shell}, nil
}

// Exec runs script in workspace.
func (l *Local) Exec(ctx context.Context, workspace, script string) (ExecResult, error) {
	if strings.TrimSpace(script) == "" {
		return ExecResult{}, errors.New("local sandbox: the script is empty")
	}
	if err := l.workspaces.Contains(workspace); err != nil {
		return ExecResult{}, fmt.Errorf("local sandbox: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return ExecResult{}, fmt.Errorf("local sandbox: workspace: %w", err)
	}
	if !info.IsDir() {
		return ExecResult{}, errors.New("local sandbox: the workspace is not a directory")
	}

	// Copied rather than appended to in place: append onto l.shell would write the
	// script into the shared slice's spare capacity, so two runs at once would each see
	// the other's script.
	argv := make([]string, 0, len(l.shell)+1)
	argv = append(argv, l.shell...)
	argv = append(argv, script)

	return runProcess(ctx, workspace, argv, sandboxEnv(workspace))
}

// passThrough copies the named variables from this process's environment, skipping the
// ones that are not set.
//
// An allowlist rather than a filter. A denylist would mean every new secret this
// service learns to read has to be remembered here too, and the one that is forgotten
// is the one the agent prints.
func passThrough(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if value, present := os.LookupEnv(name); present && value != "" {
			out = append(out, name+"="+value)
		}
	}
	return out
}

// pathEnv is the child's PATH: this process's, or the platform's default when this
// process has none. A sandbox with an empty PATH cannot run so much as `ls`.
func pathEnv() string {
	if value := strings.TrimSpace(os.Getenv("PATH")); value != "" {
		return "PATH=" + value
	}
	return "PATH=" + defaultPath
}
