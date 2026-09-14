package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// pickScript chooses between a POSIX shell script and its cmd.exe equivalent, so that
// one test asserts one behaviour on both platforms rather than being skipped on either.
func pickScript(unix, windows string) string {
	if runtime.GOOS == "windows" {
		return windows
	}
	return unix
}

// sleepSeconds is how long the busy script would take if nothing killed it.
const sleepSeconds = 10

// busyScript occupies the shell itself for far longer than any test's deadline.
//
// The cmd.exe half is a loop inside the interpreter rather than `ping -n 10`, because
// platform_windows.go cannot kill a grandchild: a surviving ping would hold the
// workspace as its working directory and the temporary directory could not be removed.
// That gap is the local backend's, and it is documented where it lives; what this
// script is here to exercise is the deadline.
func busyScript() string {
	return pickScript("sleep 10", "for /L %i in (1,1,2000000000) do @rem")
}

// newLocal builds the local backend over a fresh workspace root, and returns the
// workspace of one user with it.
func newLocal(t *testing.T) (*Local, string) {
	t.Helper()
	w := workspaces(t)
	local, err := NewLocal(w)
	if err != nil {
		t.Fatalf("build the local sandbox: %v", err)
	}
	workspace, err := w.For("0191e2abuser")
	if err != nil {
		t.Fatalf("build a workspace: %v", err)
	}
	return local, workspace
}

func TestNewLocal_refusesToRunWithoutAWorkspaceRoot(t *testing.T) {
	// Arrange, Act
	_, err := NewLocal(nil)

	// Assert
	// Every path check this backend makes goes through Workspaces. Without one there is
	// nothing to check a path against, which is worse than not starting.
	if err == nil {
		t.Fatal("a local sandbox with no workspaces was built")
	}
}

func TestLocal_execRunsTheCommandInsideTheWorkspace(t *testing.T) {
	// Arrange
	local, workspace := newLocal(t)

	// Act
	// Redirection to a relative path reads the same in sh and in cmd.exe.
	result, err := local.Exec(context.Background(), workspace, "echo ran > marker.txt")

	// Assert
	if err != nil {
		t.Fatalf("the command could not be run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", result.ExitCode, result.Stderr)
	}
	// A relative path in a script has to land in the run's own workspace. If the working
	// directory were this process's, a tool writing a file would be writing into the
	// service's installation directory.
	if _, err := os.Stat(filepath.Join(workspace, "marker.txt")); err != nil {
		t.Errorf("the script did not write into the workspace: %v", err)
	}
}

func TestLocal_execReportsAFailingCommandAsOutputRatherThanAsAnError(t *testing.T) {
	// Arrange
	// A command that fails is something the model is supposed to read and work around. A
	// Go error would instead end the step, so the difference matters to the loop.
	local, workspace := newLocal(t)

	// Act
	result, err := local.Exec(context.Background(), workspace, "exit 3")

	// Assert
	if err != nil {
		t.Fatalf("a non-zero exit was reported as a Go error: %v", err)
	}
	if result.ExitCode != 3 {
		t.Errorf("ExitCode = %d; want 3", result.ExitCode)
	}
}

func TestLocal_execKeepsStderrApartFromStdout(t *testing.T) {
	// Arrange
	// Kept apart because the model is shown both and told which is which; merged, a
	// warning printed by a tool reads as part of its answer.
	local, workspace := newLocal(t)
	script := pickScript("echo answer; echo warning 1>&2", "echo answer & echo warning 1>&2")

	// Act
	result, err := local.Exec(context.Background(), workspace, script)

	// Assert
	if err != nil {
		t.Fatalf("the command could not be run: %v", err)
	}
	if !strings.Contains(result.Stdout, "answer") {
		t.Errorf("Stdout = %q; want it to carry the answer", result.Stdout)
	}
	if strings.Contains(result.Stdout, "warning") {
		t.Errorf("Stdout = %q; want the warning on stderr only", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "warning") {
		t.Errorf("Stderr = %q; want it to carry the warning", result.Stderr)
	}
}

func TestLocal_execDoesNotHandTheCommandThisProcessSecrets(t *testing.T) {
	// Arrange
	// The whole reason sandboxEnv is an allowlist. This process holds the provider keys,
	// the database DSN and the goal engine's key, and what runs in the sandbox is a script
	// a language model wrote from whatever somebody typed into a chat.
	const secret = "sk-ant-test-4f19c2"
	t.Setenv("ANTHROPIC_API_KEY", secret)
	local, workspace := newLocal(t)
	script := pickScript("echo $ANTHROPIC_API_KEY", "echo %ANTHROPIC_API_KEY%")

	// Act
	result, err := local.Exec(context.Background(), workspace, script)

	// Assert
	if err != nil {
		t.Fatalf("the command could not be run: %v", err)
	}
	if strings.Contains(result.Stdout, secret) || strings.Contains(result.Stderr, secret) {
		t.Errorf("the command was handed this process's key: stdout %q, stderr %q", result.Stdout, result.Stderr)
	}
}

func TestLocal_execKillsACommandThatRunsPastItsDeadline(t *testing.T) {
	// Arrange
	local, workspace := newLocal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// Act
	started := time.Now()
	result, err := local.Exec(ctx, workspace, busyScript())
	elapsed := time.Since(started)

	// Assert
	if err != nil {
		// The command ran; it ran too long. That is output the model can act on — the fix
		// is a smaller command — rather than a failure of the step.
		t.Fatalf("a timeout was reported as a Go error: %v", err)
	}
	if result.ExitCode != timedOutExitCode {
		t.Errorf("ExitCode = %d; want %d", result.ExitCode, timedOutExitCode)
	}
	if !strings.Contains(result.Stderr, "[killed:") {
		// Said in words as well as in the exit code, because a model reading a transcript
		// has no man page for 124.
		t.Errorf("Stderr = %q; want it to say the command was killed", result.Stderr)
	}
	if elapsed > (sleepSeconds-2)*time.Second {
		t.Errorf("the command was not killed: it ran for %s", elapsed)
	}
}

func TestLocal_execEndsTheStepWhenTheRunIsCancelled(t *testing.T) {
	// Arrange
	// The other way round from a timeout: nobody is waiting for output, because the run
	// itself is stopping. Reporting exit 124 here would put a step in the transcript that
	// looks like the model's problem to solve.
	local, workspace := newLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	defer cancel()

	// Act
	_, err := local.Exec(ctx, workspace, busyScript())

	// Assert
	if err == nil {
		t.Fatal("a cancelled command returned output instead of ending the step")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("the error does not say it was cancelled: %v", err)
	}
}

func TestLocal_execRefusesAWorkspaceItDoesNotOwn(t *testing.T) {
	// Arrange
	local, workspace := newLocal(t)
	elsewhere := t.TempDir()
	file := filepath.Join(workspace, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cases := map[string]string{
		"outside the root":  elsewhere,
		"a file":            file,
		"nothing":           "",
		"one that is gone":  filepath.Join(workspace, "deleted"),
		"the parent of all": filepath.Dir(local.workspaces.Root()),
	}

	// Act, Assert
	for label, path := range cases {
		if _, err := local.Exec(context.Background(), path, "echo hello"); err == nil {
			t.Errorf("%s was accepted as a workspace", label)
		}
	}
}

func TestLocal_execRefusesAnEmptyScript(t *testing.T) {
	// Arrange
	// A blank script means the model asked for a shell tool call and filled in nothing.
	// Running it would spend a step producing no output, and the model would have no idea
	// why.
	local, workspace := newLocal(t)

	// Act, Assert
	for _, script := range []string{"", "   ", "\n\t"} {
		if _, err := local.Exec(context.Background(), workspace, script); err == nil {
			t.Errorf("script %q was accepted", script)
		}
	}
}

func TestLocal_execReportsACommandKilledBySignalAsKilled(t *testing.T) {
	// Arrange
	// os/exec reports a signalled process as exit code -1, and a negative status reads as
	// success to anything checking the wrong way round. 128+SIGKILL is the shell's own
	// convention, so a model that has seen a shell before reads 137 correctly.
	if runtime.GOOS == "windows" {
		// A Windows process has no signal to be killed by, and cmd.exe has no kill builtin.
		// This branch is exercised where it exists.
		t.Skip("there are no signals on Windows")
	}
	local, workspace := newLocal(t)

	// Act
	result, err := local.Exec(context.Background(), workspace, "kill -9 $$")

	// Assert
	if err != nil {
		t.Fatalf("a signalled command was reported as a Go error: %v", err)
	}
	if result.ExitCode != signalExitCode {
		t.Errorf("ExitCode = %d; want %d", result.ExitCode, signalExitCode)
	}
	if !strings.Contains(result.Stderr, "[killed:") {
		t.Errorf("Stderr = %q; want it to say the command was killed", result.Stderr)
	}
}

func TestTerminate_doesNothingToACommandThatNeverStarted(t *testing.T) {
	// Arrange
	// os/exec calls Cancel for a context that is already done when Run is reached, which
	// can be before there is a process to kill. Reaching into a nil Process there would
	// turn a cancelled run into a panic in the worker.
	cmd := &exec.Cmd{}

	// Act
	err := terminate(cmd)

	// Assert
	if err != nil {
		t.Errorf("terminate on an unstarted command returned %v", err)
	}
}

func TestPathEnv_fallsBackToThePlatformsOwnPath(t *testing.T) {
	// Arrange
	// A sandbox with an empty PATH cannot run so much as `ls`, and the failure looks like
	// the script's fault rather than the environment's.
	t.Setenv("PATH", "")

	// Act
	got := pathEnv()

	// Assert
	if got != "PATH="+defaultPath {
		t.Errorf("pathEnv() = %q; want the platform default %q", got, defaultPath)
	}
}

func TestSandboxEnv_carriesAPathAndAWorkspaceHomeAndNoSecrets(t *testing.T) {
	// Arrange
	// Asserted directly as well as through a command, because the command's shell only
	// proves what one `echo` could see. This is the list itself.
	const secret = "postgres://wingman:hunter2@db.internal:5432/core"
	t.Setenv("DATABASE_URL", secret)
	t.Setenv("GOAL_ENGINE_API_KEY", "goal-engine-key-4f19")
	workspace := filepath.Join(t.TempDir(), "0191e2abuser")

	// Act
	env := sandboxEnv(workspace)

	// Assert
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH=") {
		// A sandbox with no PATH cannot run so much as `ls`, and the failure looks like the
		// script's fault.
		t.Errorf("the environment has no PATH: %v", env)
	}
	if strings.Contains(joined, secret) || strings.Contains(joined, "goal-engine-key-4f19") {
		t.Fatal("the sandbox environment carries this process's secrets")
	}
	home := "HOME=" + workspace
	if runtime.GOOS == "windows" {
		home = "USERPROFILE=" + workspace
	}
	if !strings.Contains(joined, home) {
		// So that a tool writing config or a temporary file writes it where the run's own
		// files are, rather than into the service user's home directory.
		t.Errorf("the environment does not point home at the workspace: %v", env)
	}
}
