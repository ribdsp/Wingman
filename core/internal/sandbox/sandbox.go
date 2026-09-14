// Package sandbox runs a tool's commands somewhere other than this process.
//
// Two backends, chosen by SANDBOX_BACKEND. The docker one is the isolation: no
// network, no capabilities, a memory limit and a read-only image with the run's
// workspace as the only writable place. The local one runs on the host as the service
// user and says so in its own doc comment — it exists so that a laptop without a
// docker daemon can still drive a run.
//
// Both share three rules that are not backend details:
//
//  1. A command runs inside the run's workspace and nowhere else. Every path is
//     checked against the configured root before a process is started.
//  2. A command never inherits this process's environment. That environment holds the
//     provider keys, the database DSN and the goal engine's key, and the sandbox is
//     where somebody else's generated script runs.
//  3. A command that outlives its deadline is killed and reported as output the model
//     can read, not as a failure of the run. A command the operator cancelled is the
//     other way round.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// maxCapturedBytes bounds what one command's output costs in memory.
	//
	// It is deliberately larger than internal/tool's own bound: the tool layer decides
	// what a model may read and marks its own truncation, and this bound exists only so
	// that a script printing a gigabyte cannot take the process down before that
	// happens.
	maxCapturedBytes = 256 * 1024

	// timedOutExitCode is what a command killed for running too long reports.
	//
	// 124 is what timeout(1) uses, so a model that has seen a shell before reads it as
	// "it was killed" rather than as something the script itself returned.
	timedOutExitCode = 124

	// signalExitCode is what a command killed by a signal reports. os/exec gives -1 for
	// a signalled process, and a negative exit status would read as success to anything
	// checking `!= 0` the wrong way round. 128+SIGKILL is the shell's own convention.
	signalExitCode = 137

	// waitDelay bounds how long Wait blocks after the process itself is gone.
	//
	// A script that leaves a background child holding stdout open — `long_job &` — keeps
	// the pipe open after the shell exits, and without this the worker would wait for a
	// process nobody is watching any more.
	waitDelay = 2 * time.Second

	// workspacePermissions is 0700 because a workspace is one user's files. On a
	// multi-user instance the difference between 0700 and 0755 is whether one user's
	// agent can read another's.
	workspacePermissions = 0o700
)

// workspaceNamePattern is what may become a directory name under the root.
//
// The id comes from our own database, so this is defence in depth. It is here because
// the cost of being wrong is a path outside the root: an id of ".." would be one
// string concatenation away from the parent of every workspace.
var workspaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ExecResult is what a backend reported about one command.
//
// A non-zero ExitCode is not an error. The command ran and failed, and the model is
// supposed to read the failure and try something else. A Go error means the command
// could not be run at all — no daemon, no workspace, a binary that is not installed.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Sandbox is what a backend implements.
//
// internal/tool declares this same shape in its own terms, so that neither package
// imports the other; cmd/core adapts one to the other in a line. See the comment on
// tool.Sandbox for why that is worth a line of wiring.
type Sandbox interface {
	// Exec runs script inside workspace and returns what it printed. The deadline on
	// ctx is the sandbox timeout, and respecting it is the only thing standing between
	// a `sleep infinity` and a worker that never returns.
	Exec(ctx context.Context, workspace, script string) (ExecResult, error)
}

// Workspaces owns the directory tree the sandboxes work in.
//
// One directory per user, created on first use and reused afterwards, because a run
// that starts from an empty directory cannot build on what the last one left. Nothing
// here is shared between users: a workspace is where an agent writes files on
// somebody's behalf, and two users sharing one is one user reading another's data
// through a tool call that looks entirely legitimate in the audit log.
type Workspaces struct {
	root string
}

// NewWorkspaces resolves the root and makes sure it exists.
func NewWorkspaces(root string) (*Workspaces, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return nil, errors.New("sandbox workspace root is empty")
	}

	// Absolute from here on. A relative root would resolve against the working
	// directory of whatever started the process, which is not the same thing between a
	// systemd unit, a container and a terminal.
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox workspace root: %w", err)
	}
	if err := os.MkdirAll(absolute, workspacePermissions); err != nil {
		return nil, fmt.Errorf("create sandbox workspace root: %w", err)
	}
	return &Workspaces{root: absolute}, nil
}

// Root is the parent directory of every workspace.
func (w *Workspaces) Root() string { return w.root }

// For returns the workspace of one user, creating it if this is their first run.
func (w *Workspaces) For(userID string) (string, error) {
	name := strings.TrimSpace(userID)
	if !workspaceNamePattern.MatchString(name) {
		// Not echoed as a path. A rejected id is a bug on our side, and the message that
		// helps is which id it was rather than which directory it would have been.
		return "", fmt.Errorf("sandbox workspace: %q is not usable as a directory name", name)
	}

	path := filepath.Join(w.root, name)
	if err := os.MkdirAll(path, workspacePermissions); err != nil {
		return "", fmt.Errorf("create sandbox workspace: %w", err)
	}
	return path, nil
}

// Contains checks that path is the root or somewhere inside it.
//
// Called by both backends before starting a process, because a workspace arrives as a
// string on an invocation and "inside the root" is the whole of what a workspace
// means. Symlinks are resolved first: a workspace that is a link to / would otherwise
// pass a purely lexical check and then be mounted into a container as though it were
// one user's files.
func (w *Workspaces) Contains(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return errors.New("sandbox workspace is empty")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return fmt.Errorf("resolve sandbox workspace: %w", err)
	}

	root := resolveLinks(w.root)
	target := resolveLinks(absolute)

	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		// The offending path is not in the message. It is either ours, in which case the
		// root and the user id say more, or it came from somewhere it should not have, in
		// which case echoing it into a log is doing that caller a favour.
		return errors.New("sandbox workspace is outside the configured workspace root")
	}
	return nil
}

// resolveLinks resolves symlinks when it can and leaves the path alone when it cannot.
//
// A path that does not exist yet is not an escape attempt — it is a workspace about to
// be created — so the lexical check stands on its own and the backend's own stat
// reports the missing directory a moment later.
func resolveLinks(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}

// runProcess starts argv, waits for it, and reports what it printed.
//
// The one place a child process is created, so that the deadline handling, the output
// bound and the refusal to inherit this process's environment are written once and
// both backends get them.
func runProcess(ctx context.Context, dir string, argv, env []string) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, errors.New("sandbox: nothing to run")
	}

	var stdout, stderr boundedBuffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	// Never nil. A nil Env means "inherit", and this process's environment is where the
	// provider keys and the database password live.
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	configureProcess(cmd)
	cmd.Cancel = func() error { return terminate(cmd) }
	cmd.WaitDelay = waitDelay

	err := cmd.Run()
	result := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}

	switch {
	case err == nil:
		return result, nil

	case errors.Is(ctx.Err(), context.Canceled):
		// Somebody cancelled the run. That is not output for the model to correct — the
		// loop is stopping — so it ends the step instead.
		return ExecResult{}, fmt.Errorf("sandbox: command cancelled: %w", ctx.Err())

	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// The command did run; it ran too long. The model needs to see that, because the
		// fix is a smaller command and it cannot work that out from silence.
		result.ExitCode = timedOutExitCode
		result.Stderr = appendLine(result.Stderr, "[killed: the command ran past the sandbox timeout]")
		return result, nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		if result.ExitCode < 0 {
			// Signalled rather than exited, and os/exec reports that as -1.
			result.ExitCode = signalExitCode
			// exit.String() is the promoted ProcessState.String(), which names the
			// signal — "signal: killed" — rather than repeating the exit code.
			result.Stderr = appendLine(result.Stderr, fmt.Sprintf("[killed: %s]", exit.String()))
		}
		return result, nil
	}

	// Not the script's failure: the binary is missing, or the working directory is not
	// there. argv[0] is named because "docker" or "/bin/sh" is exactly what the operator
	// has to install; the rest of argv is not, because it holds the model's script.
	return ExecResult{}, fmt.Errorf("sandbox: run %s: %w", argv[0], err)
}

// appendLine adds a line to captured output that may or may not end in a newline.
func appendLine(text, line string) string {
	if strings.TrimSpace(text) == "" {
		return line
	}
	return strings.TrimRight(text, "\n") + "\n" + line
}

// boundedBuffer keeps the first maxCapturedBytes it is written and drops the rest.
//
// It keeps accepting writes rather than returning an error, because a short write is
// how os/exec reports a broken pipe to the child: a command whose output was longer
// than we wanted to keep would start failing halfway through, which is a different
// outcome from the one we asked for.
type boundedBuffer struct {
	kept []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := maxCapturedBytes - len(b.kept); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.kept = append(b.kept, p[:room]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return string(b.kept) }
