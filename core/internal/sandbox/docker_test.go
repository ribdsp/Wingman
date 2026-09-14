package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The fake docker CLI's behaviours, as snippets in the platform's own shell. Each runs
// after fakeDocker's preamble has logged the arguments it was called with, and branches
// on the first argument where a test needs `docker rm` to behave differently from
// `docker run`.
const (
	fakeRunsUnix    = "echo ran-in-container"
	fakeRunsWindows = "echo ran-in-container"

	fakeHangsUnix = `if [ "$1" = rm ]; then exit 0; fi
sleep 10`
	fakeHangsWindows = `if "%1"=="rm" exit /b 0
for /L %%i in (1,1,2000000000) do @rem`

	fakeRemoveFailsUnix = `if [ "$1" = rm ]; then
  echo 'Error response from daemon: driver is busy' 1>&2
  exit 1
fi
sleep 10`
	fakeRemoveFailsWindows = `if "%1"=="rm" goto rm
for /L %%i in (1,1,2000000000) do @rem
exit /b 0
:rm
echo Error response from daemon: driver is busy 1>&2
exit /b 1`

	fakeAlreadyGoneUnix = `if [ "$1" = rm ]; then
  echo 'Error: No such container: wingman-x' 1>&2
  exit 1
fi
sleep 10`
	fakeAlreadyGoneWindows = `if "%1"=="rm" goto rm
for /L %%i in (1,1,2000000000) do @rem
exit /b 0
:rm
echo Error: No such container: wingman-x 1>&2
exit /b 1`
)

// newDocker builds a docker backend without contacting a daemon, over a fresh workspace
// root, and returns one user's workspace with it.
func newDocker(t *testing.T, binary string) (*Docker, string) {
	t.Helper()
	w := workspaces(t)
	workspace, err := w.For("0191e2abuser")
	if err != nil {
		t.Fatalf("build a workspace: %v", err)
	}
	if binary == "" {
		binary = "docker"
	}
	// Built by hand rather than through NewDocker, so that these tests run on a machine
	// with no docker installed. NewDocker's own checks have their own test.
	return &Docker{
		workspaces: w,
		image:      "wingman/sandbox:test",
		network:    "none",
		memory:     "512m",
		binary:     binary,
		user:       "1000:1000",
	}, workspace
}

// hasPair reports whether args contains flag immediately followed by value.
//
// Position matters: `--network` followed by something else, with "none" elsewhere in the
// list, is a container with a network.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1] == value
		}
	}
	return false
}

func hasArg(args []string, arg string) bool {
	for _, candidate := range args {
		if candidate == arg {
			return true
		}
	}
	return false
}

func TestNewDocker_refusesWhatItCannotRun(t *testing.T) {
	// Arrange
	w := workspaces(t)
	cases := map[string]struct {
		workspaces *Workspaces
		cfg        DockerConfig
	}{
		// Every path check goes through Workspaces. Without one there is nothing to check a
		// mount against, and the mount is the one writable thing in the container.
		"no workspaces":  {nil, DockerConfig{Image: "wingman/sandbox:test"}},
		"no image":       {w, DockerConfig{}},
		"a blank image":  {w, DockerConfig{Image: "   "}},
		"no such binary": {w, DockerConfig{Image: "wingman/sandbox:test", Binary: "wingman-not-docker"}},
	}

	// Act, Assert
	for label, c := range cases {
		if _, err := NewDocker(c.workspaces, c.cfg); err == nil {
			t.Errorf("%s was accepted", label)
		}
	}
}

func TestNewDocker_fillsInTheDefaultsThatKeepTheSandboxShut(t *testing.T) {
	// Arrange
	// A missing line in an env file must not be the difference between a sandbox and a
	// host with a shell on the internet, so the fallbacks are the restrictive ones rather
	// than docker's.
	w := workspaces(t)
	binary, _ := fakeDocker(t, fakeRunsUnix, fakeRunsWindows)

	// Act
	d, err := NewDocker(w, DockerConfig{Image: "wingman/sandbox:test", Binary: binary})

	// Assert
	if err != nil {
		t.Fatalf("build the docker sandbox: %v", err)
	}
	if d.network != "none" {
		// docker's own default is a bridge with working internet access.
		t.Errorf("network = %q; want none", d.network)
	}
	if d.memory != defaultDockerMemory {
		// docker's own default is unlimited, which on a small VPS means one runaway script
		// takes the database down with it.
		t.Errorf("memory = %q; want %q", d.memory, defaultDockerMemory)
	}
}

func TestDockerArgs_isolatesTheContainer(t *testing.T) {
	// Arrange
	// Every pair here is load-bearing. This test exists so that removing one is a failing
	// test rather than a quiet change to what a generated script may do to the host.
	d, workspace := newDocker(t, "")

	// Act
	args := d.dockerArgs("wingman-0011223344556677", workspace, "echo hello")

	// Assert
	pairs := map[string]string{
		"--name":         "wingman-0011223344556677",
		"--label":        containerLabel,
		"--network":      "none",
		"--memory":       "512m",
		"--memory-swap":  "512m",
		"--pids-limit":   "256",
		"--cap-drop":     "ALL",
		"--security-opt": "no-new-privileges",
		"--tmpfs":        "/tmp:rw,nosuid,size=" + tmpfsSize,
		"--volume":       workspace + ":" + containerWorkspace,
		"--workdir":      containerWorkspace,
		"--env":          "HOME=" + containerWorkspace,
		"--entrypoint":   "sh",
		"--user":         "1000:1000",
	}
	for flag, value := range pairs {
		if !hasPair(args, flag, value) {
			t.Errorf("%s is not followed by %q: %v", flag, value, args)
		}
	}
	for _, flag := range []string{"--rm", "--read-only"} {
		if !hasArg(args, flag) {
			t.Errorf("%s is missing: %v", flag, args)
		}
	}
	if args[0] != d.binary || args[1] != "run" {
		t.Errorf("the command starts %v; want the binary then run", args[:2])
	}
}

func TestDockerArgs_leavesTheUserToDockerWhenTheHostHasNone(t *testing.T) {
	// Arrange
	// Windows has no numeric uid to hand a Linux container, so hostUser returns empty and
	// the flag has to be left out rather than passed as ":".
	d, workspace := newDocker(t, "")
	d.user = ""

	// Act
	args := d.dockerArgs("wingman-0011223344556677", workspace, "echo hello")

	// Assert
	if hasArg(args, "--user") {
		t.Errorf("--user was passed with nothing to pass: %v", args)
	}
}

func TestDockerArgs_keepsTheScriptAsASingleArgument(t *testing.T) {
	// Arrange
	// The script is one argv element, so nothing in it is ever parsed by anything on this
	// side of the container boundary. It is written by a language model from whatever a
	// user typed, and the shell that gets to interpret it is the one inside the sandbox.
	d, workspace := newDocker(t, "")
	script := `echo "it's here" && cat /etc/hostname; id $(whoami) | tr -d '\n'`

	// Act
	args := d.dockerArgs("wingman-0011223344556677", workspace, script)

	// Assert
	tail := args[len(args)-3:]
	if tail[0] != d.image || tail[1] != "-c" || tail[2] != script {
		// The order is the contract: image, then sh's own flag, then one string. Anything
		// after the script would be read by sh as $0 and $1.
		t.Errorf("the command ends %q; want the image, -c and the script unchanged", tail)
	}
}

func TestContainerName_isUnguessableAndMarked(t *testing.T) {
	// Arrange
	// Unguessable because the name is what remove acts on. Prefixed so that an operator
	// looking at docker ps during an incident knows whose container it is.
	seen := make(map[string]bool, 64)

	// Act, Assert
	for i := 0; i < 64; i++ {
		name, err := containerName()
		if err != nil {
			t.Fatalf("name a container: %v", err)
		}
		if !strings.HasPrefix(name, "wingman-") {
			t.Fatalf("name = %q; want a wingman- prefix", name)
		}
		if seen[name] {
			t.Fatalf("name %q was handed out twice", name)
		}
		seen[name] = true
	}
}

func TestDockerEnv_carriesTheDaemonAddressAndNoSecrets(t *testing.T) {
	// Arrange
	// The CLI needs DOCKER_HOST to find a daemon that is not on the local socket. It has
	// no use for anything else this process holds, and a docker command line shows up in
	// a process list.
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.4:2376")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-4f19c2")
	t.Setenv("DATABASE_URL", "postgres://wingman:hunter2@db.internal:5432/core")

	// Act
	env := dockerEnv()

	// Assert
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "DOCKER_HOST=tcp://10.0.0.4:2376") {
		t.Errorf("the daemon address was dropped: %v", env)
	}
	if !strings.Contains(joined, "PATH=") {
		t.Errorf("the CLI was given no PATH: %v", env)
	}
	if strings.Contains(joined, "sk-ant-test-4f19c2") || strings.Contains(joined, "hunter2") {
		t.Fatal("the docker CLI was handed this process's secrets")
	}
}

func TestHostUser_isEmptyWhereThereIsNoNumericUser(t *testing.T) {
	// Arrange, Act
	user := hostUser()

	// Assert
	if runtime.GOOS == "windows" {
		// os.Getuid reports -1 here, and ":" is not a uid:gid pair.
		if user != "" {
			t.Errorf("hostUser() = %q; want empty on Windows", user)
		}
		return
	}
	if !strings.Contains(user, ":") {
		t.Errorf("hostUser() = %q; want uid:gid", user)
	}
}

func TestDocker_execRefusesAWorkspaceItDoesNotOwn(t *testing.T) {
	// Arrange
	// Checked before the daemon is contacted, because the workspace becomes a bind mount:
	// a path outside the root would be mounted into a container as though it were one
	// user's files.
	d, workspace := newDocker(t, "")
	file := filepath.Join(workspace, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cases := map[string]string{
		"outside the root": t.TempDir(),
		"a file":           file,
		"nothing":          "",
		"one that is gone": filepath.Join(workspace, "deleted"),
	}

	// Act, Assert
	for label, path := range cases {
		if _, err := d.Exec(context.Background(), path, "echo hello"); err == nil {
			t.Errorf("%s was accepted as a workspace", label)
		}
	}
}

func TestDocker_execRefusesAnEmptyScript(t *testing.T) {
	// Arrange
	d, workspace := newDocker(t, "")

	// Act, Assert
	for _, script := range []string{"", "   ", "\n\t"} {
		if _, err := d.Exec(context.Background(), workspace, script); err == nil {
			t.Errorf("script %q was accepted", script)
		}
	}
}

func TestDocker_execReportsAMissingDockerWithoutEchoingTheScript(t *testing.T) {
	// Arrange
	// The daemon is not contacted at startup on purpose — a restarting daemon should not
	// stop this service booting — so a broken installation surfaces here, on the first
	// tool call.
	d, workspace := newDocker(t, "wingman-not-docker")
	script := "curl -H 'Authorization: Bearer sk-live-4f19' https://billing.internal"

	// Act
	_, err := d.Exec(context.Background(), workspace, script)

	// Assert
	if err == nil {
		t.Fatal("a missing docker CLI was reported as a successful run")
	}
	if !strings.Contains(err.Error(), "wingman-not-docker") {
		// Naming the binary is the whole point of the message: it is what the operator has
		// to install.
		t.Errorf("the error does not name the binary: %v", err)
	}
	if strings.Contains(err.Error(), "sk-live-4f19") {
		t.Errorf("the error carries the script: %v", err)
	}
}

func TestDocker_execRunsTheContainerAndReturnsWhatItPrinted(t *testing.T) {
	// Arrange
	binary, logPath := fakeDocker(t, fakeRunsUnix, fakeRunsWindows)
	d, workspace := newDocker(t, binary)

	// Act
	result, err := d.Exec(context.Background(), workspace, "echo hello")

	// Assert
	if err != nil {
		t.Fatalf("the container could not be started: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, stderr = %q", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "ran-in-container") {
		t.Errorf("Stdout = %q; want what the container printed", result.Stdout)
	}
	invocations := readLog(t, logPath)
	if !strings.Contains(invocations, "run") {
		t.Errorf("docker was not asked to run anything: %q", invocations)
	}
	if strings.Contains(invocations, " rm ") {
		// Nothing was cancelled, so --rm is enough and there is nothing to clean up.
		t.Errorf("a container that ended on its own was removed by hand: %q", invocations)
	}
}

func TestDocker_execRemovesTheContainerWhenTheDeadlinePasses(t *testing.T) {
	// Arrange
	// The client exiting does not stop the container, and --rm only fires when the
	// container's own process ends. Without this, a timed-out run leaves a container
	// holding its memory limit until somebody notices.
	binary, logPath := fakeDocker(t, fakeHangsUnix, fakeHangsWindows)
	d, workspace := newDocker(t, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	// Act
	result, err := d.Exec(ctx, workspace, "echo hello")

	// Assert
	if err != nil {
		t.Fatalf("a timeout was reported as a Go error: %v", err)
	}
	if result.ExitCode != timedOutExitCode {
		t.Errorf("ExitCode = %d; want %d", result.ExitCode, timedOutExitCode)
	}
	invocations := readLog(t, logPath)
	if !strings.Contains(invocations, "rm --force wingman-") {
		t.Errorf("the container was not removed by name: %q", invocations)
	}
	if strings.Contains(result.Stderr, "[sandbox: ") {
		// The cleanup worked, so there is nothing for an operator to do and nothing to say
		// about it in the transcript.
		t.Errorf("Stderr = %q; want nothing about a cleanup that succeeded", result.Stderr)
	}
}

func TestDocker_execSaysSoWhenTheContainerCouldNotBeRemoved(t *testing.T) {
	// Arrange
	// The script's own result stands — it did run and it was killed — but a container that
	// is still holding memory has to reach the transcript an operator reads rather than
	// nowhere at all.
	binary, _ := fakeDocker(t, fakeRemoveFailsUnix, fakeRemoveFailsWindows)
	d, workspace := newDocker(t, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	// Act
	result, err := d.Exec(ctx, workspace, "echo hello")

	// Assert
	if err != nil {
		t.Fatalf("a failed cleanup ended the step: %v", err)
	}
	if result.ExitCode != timedOutExitCode {
		t.Errorf("ExitCode = %d; want %d", result.ExitCode, timedOutExitCode)
	}
	if !strings.Contains(result.Stderr, "[sandbox: ") {
		t.Errorf("Stderr = %q; want it to carry the cleanup failure", result.Stderr)
	}
	if !strings.Contains(result.Stderr, "[killed:") {
		// And still say what happened to the command itself, which is what the model reads.
		t.Errorf("Stderr = %q; want it to still say the command was killed", result.Stderr)
	}
}

func TestDocker_execTreatsAContainerThatIsAlreadyGoneAsRemoved(t *testing.T) {
	// Arrange
	// The ordinary case: the container ended on its own between the timeout and the
	// cleanup, and --rm already took it away. Reporting that as a cleanup failure would
	// put a line in every timed-out transcript.
	binary, _ := fakeDocker(t, fakeAlreadyGoneUnix, fakeAlreadyGoneWindows)
	d, workspace := newDocker(t, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	// Act
	result, err := d.Exec(ctx, workspace, "echo hello")

	// Assert
	if err != nil {
		t.Fatalf("an already-removed container ended the step: %v", err)
	}
	if strings.Contains(result.Stderr, "[sandbox: ") {
		t.Errorf("Stderr = %q; want nothing about a cleanup that had nothing to do", result.Stderr)
	}
}

func TestDocker_execReportsBothWhenACancelledRunAlsoLeavesAContainer(t *testing.T) {
	// Arrange
	// Cancellation ends the step, so there is no transcript to append a note to — the two
	// failures have to travel together or the container that is still holding memory goes
	// unmentioned.
	binary, _ := fakeDocker(t, fakeRemoveFailsUnix, fakeRemoveFailsWindows)
	d, workspace := newDocker(t, binary)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	defer cancel()

	// Act
	_, err := d.Exec(ctx, workspace, "echo hello")

	// Assert
	if err == nil {
		t.Fatal("a cancelled run returned output instead of ending the step")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("the error does not say the run was cancelled: %v", err)
	}
	if !strings.Contains(err.Error(), "could not be removed") {
		t.Errorf("the error does not mention the container left behind: %v", err)
	}
}

// fakeDocker writes an executable that stands in for the docker CLI, and returns it with
// the path of the file it logs its arguments to.
//
// It exists because Docker.Exec's own behaviour — the container name, the timeout, the
// cleanup that follows one — is worth a test, and a test that needs a running daemon
// would not be in the unit suite. body is a snippet in the platform's shell; it runs
// after the arguments have been logged.
func fakeDocker(t *testing.T, unixBody, windowsBody string) (binary, logPath string) {
	t.Helper()

	dir := t.TempDir()
	logPath = filepath.Join(dir, "invocations.log")

	if runtime.GOOS == "windows" {
		binary = filepath.Join(dir, "fake-docker.cmd")
		// A space before the redirection on purpose: cmd.exe reads a digit immediately
		// before ">>" as a file descriptor, and a container name can end in one.
		script := "@echo off\r\necho %* >>\"" + logPath + "\"\r\n" +
			strings.ReplaceAll(windowsBody, "\n", "\r\n") + "\r\n"
		writeExecutable(t, binary, script)
		return binary, logPath
	}

	binary = filepath.Join(dir, "fake-docker")
	script := "#!/bin/sh\necho \"$@\" >> \"" + logPath + "\"\n" + unixBody + "\n"
	writeExecutable(t, binary, script)
	return binary, logPath
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

// readLog returns what the fake docker CLI was invoked with, or "" if it never was.
func readLog(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read the invocation log: %v", err)
	}
	return string(raw)
}
