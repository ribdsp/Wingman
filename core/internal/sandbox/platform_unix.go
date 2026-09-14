//go:build !windows

package sandbox

import (
	"os/exec"
	"syscall"
)

// defaultPath is what a child gets when this process has no PATH at all.
const defaultPath = "/usr/local/bin:/usr/bin:/bin"

// defaultShell is sh, not bash. The model is told it is running a shell script, and sh
// is the one that exists in a scratch container as well as on a developer's machine.
func defaultShell() []string { return []string{"/bin/sh", "-c"} }

// platformEnv is the handful of this process's variables that a child needs in order to
// behave normally, and that carry nothing sensitive.
func platformEnv() []string {
	return passThrough("LANG", "LC_ALL", "TERM", "TZ")
}

// sandboxEnv is everything a command may see.
//
// HOME and TMPDIR point at the workspace, so a tool that writes config or a temporary
// file writes it where the run's own files are and it is cleaned up with them. Nothing
// else of this process's environment is here: ANTHROPIC_API_KEY, DATABASE_URL and
// GOAL_ENGINE_API_KEY all live in it, and `env` is one tool call away.
func sandboxEnv(workspace string) []string {
	env := []string{
		pathEnv(),
		"HOME=" + workspace,
		"TMPDIR=" + workspace,
		"SHELL=/bin/sh",
	}
	return append(env, platformEnv()...)
}

// configureProcess puts the command in its own process group.
//
// So that terminate can kill the group rather than the shell alone. `sh -c "sleep 900
// &"` returns immediately and leaves a grandchild behind; killing only the shell would
// leave that process running until the host was rebooted.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate kills the command and everything it started.
func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// A negative pid is the process group. It is tried first and the single process
	// second, because a failure here — the group is already gone, Setpgid did not take —
	// must still end with the shell itself killed.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
