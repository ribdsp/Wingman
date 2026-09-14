package sandbox

import (
	"os/exec"
)

// defaultPath is what a child gets when this process has no PATH at all.
const defaultPath = `C:\Windows\system32;C:\Windows`

// defaultShell is cmd.exe. PowerShell would be a second quoting dialect for no gain:
// the model is told it is running a shell script, and on Windows the local backend is a
// development convenience whose production counterpart is the docker one.
func defaultShell() []string { return []string{"cmd", "/C"} }

// platformEnv is the handful of this process's variables that a child needs in order to
// behave normally, and that carry nothing sensitive.
//
// SystemRoot, windir, COMSPEC and PATHEXT are here because a Windows process that
// cannot find them fails in ways that look like the caller's fault: COMSPEC alone is
// how the loader finds cmd.exe.
func platformEnv() []string {
	return passThrough("SystemRoot", "windir", "COMSPEC", "PATHEXT", "NUMBER_OF_PROCESSORS", "TZ")
}

// sandboxEnv is everything a command may see.
//
// USERPROFILE, HOME, TEMP and TMP point at the workspace, so a tool that writes config
// or a temporary file writes it where the run's own files are. Nothing else of this
// process's environment is here beyond platformEnv: ANTHROPIC_API_KEY, DATABASE_URL and
// GOAL_ENGINE_API_KEY all live in it, and `set` is one tool call away.
func sandboxEnv(workspace string) []string {
	env := []string{
		pathEnv(),
		"USERPROFILE=" + workspace,
		"HOME=" + workspace,
		"TEMP=" + workspace,
		"TMP=" + workspace,
	}
	return append(env, platformEnv()...)
}

// configureProcess sets nothing.
//
// There is no Setpgid here, and no job object either: a job object is the only way to
// kill a process tree on Windows and it needs golang.org/x/sys plus about eighty lines
// of handle management. What that costs is stated rather than hidden — on Windows a
// command that detaches a background child can leave that child running after the
// timeout kills the shell. It is one more reason the docker backend is the supported
// one, and why the local backend is documented as a development convenience.
func configureProcess(*exec.Cmd) {}

// terminate kills the command itself.
func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
