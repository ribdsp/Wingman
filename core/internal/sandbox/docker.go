package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// containerWorkspace is where the run's workspace is mounted inside the container.
	// Fixed, so that a path in one run's transcript means the same thing in another's.
	containerWorkspace = "/workspace"

	// defaultDockerBinary is looked up on PATH rather than hard-coded to /usr/bin/docker,
	// because Docker Desktop, Colima and a distribution package all put it somewhere
	// different.
	defaultDockerBinary = "docker"

	// defaultDockerMemory is a fallback for a backend built in code without one. The
	// config always supplies a limit; docker's own default is unlimited, which on a small
	// VPS means one runaway script takes the database down with it.
	defaultDockerMemory = "512m"

	// maxContainerPIDs stops a fork bomb inside the sandbox becoming a fork bomb on the
	// host. 256 is more processes than a build needs and fewer than it takes to matter.
	maxContainerPIDs = 256

	// tmpfsSize bounds the writable /tmp a read-only image needs. Execution is left
	// permitted there because compilers and installers write and run their own
	// intermediates; the size limit is what stops /tmp being used as storage.
	tmpfsSize = "64m"

	// containerLabel marks every container this service starts, so that an operator
	// cleaning up after an outage can find them with one docker ps filter.
	containerLabel = "wingman.core=sandbox"

	// removeTimeout bounds the cleanup of a container whose client was killed. It runs
	// on a fresh context because the one that was cancelled is exactly why there is
	// something to clean up.
	removeTimeout = 15 * time.Second
)

// DockerConfig is what the docker backend needs from the operator's configuration.
type DockerConfig struct {
	Image   string
	Network string
	Memory  string
	// Binary is the docker CLI to run, "docker" by default.
	Binary string
}

// Docker runs each command in its own throwaway container.
//
// This is the backend to run in front of other people. It shells out to the docker CLI
// rather than talking to the daemon's API: the CLI is the interface docker documents as
// stable, every operator already has it, and the alternative is a dependency on
// docker/docker that pulls in a large part of the docker source tree to build one
// argument list. The list is built in dockerArgs, which is a pure function and is
// tested flag by flag — that is where the isolation actually lives.
type Docker struct {
	workspaces *Workspaces
	image      string
	network    string
	memory     string
	binary     string
	// user is the host uid:gid the container runs as, empty where the host has no such
	// thing. See hostUser.
	user string
}

// NewDocker builds the docker backend.
//
// The CLI is looked up here so that a box without docker fails at startup. The daemon
// is deliberately not contacted: a daemon that is restarting should not stop this
// service from booting, and its absence shows up on the first tool call as an error
// naming docker.
func NewDocker(workspaces *Workspaces, cfg DockerConfig) (*Docker, error) {
	if workspaces == nil {
		return nil, errors.New("docker sandbox: no workspaces were configured")
	}

	d := &Docker{
		workspaces: workspaces,
		image:      strings.TrimSpace(cfg.Image),
		network:    strings.TrimSpace(cfg.Network),
		memory:     strings.TrimSpace(cfg.Memory),
		binary:     strings.TrimSpace(cfg.Binary),
		user:       hostUser(),
	}
	if d.image == "" {
		return nil, errors.New("docker sandbox: no image was configured")
	}
	if d.network == "" {
		// Not left to docker, whose default is a bridge with working internet access.
		// "unset" must mean the safer of the two, or a missing line in an env file is the
		// difference between a sandbox and a host with a shell on the internet.
		d.network = "none"
	}
	if d.memory == "" {
		d.memory = defaultDockerMemory
	}
	if d.binary == "" {
		d.binary = defaultDockerBinary
	}
	if _, err := exec.LookPath(d.binary); err != nil {
		return nil, fmt.Errorf("docker sandbox: %s: %w", d.binary, err)
	}
	return d, nil
}

// Exec runs script in a container with workspace mounted.
func (d *Docker) Exec(ctx context.Context, workspace, script string) (ExecResult, error) {
	if strings.TrimSpace(script) == "" {
		return ExecResult{}, errors.New("docker sandbox: the script is empty")
	}
	if err := d.workspaces.Contains(workspace); err != nil {
		return ExecResult{}, fmt.Errorf("docker sandbox: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return ExecResult{}, fmt.Errorf("docker sandbox: workspace: %w", err)
	}
	if !info.IsDir() {
		return ExecResult{}, errors.New("docker sandbox: the workspace is not a directory")
	}

	name, err := containerName()
	if err != nil {
		return ExecResult{}, fmt.Errorf("docker sandbox: %w", err)
	}

	// The docker CLI gets a minimal environment too, not this process's. It needs the
	// DOCKER_* variables to find the daemon an operator configured, and nothing else.
	result, runErr := runProcess(ctx, "", d.dockerArgs(name, workspace, script), dockerEnv())

	if ctx.Err() == nil {
		return result, runErr
	}

	// The client exiting does not stop the container, and --rm only fires when the
	// container's own process ends. A killed or timed-out client therefore leaves a
	// container holding its memory limit until somebody notices, so it is removed by the
	// name it was started under.
	removeErr := d.remove(name)
	switch {
	case removeErr == nil:
		return result, runErr
	case runErr != nil:
		return result, errors.Join(runErr, removeErr)
	default:
		// The script's own result stands; the cleanup failure is appended so that it is in
		// the transcript an operator reads rather than nowhere at all.
		result.Stderr = appendLine(result.Stderr, "[sandbox: "+removeErr.Error()+"]")
		return result, nil
	}
}

// dockerArgs is the whole isolation, as an argument list.
//
// Every flag here is load-bearing and none of them is a default worth trusting docker
// for. Removing one is a change to what a generated script may do to the host, which
// is an argument for a pull request rather than a diff.
func (d *Docker) dockerArgs(name, workspace, script string) []string {
	args := []string{
		d.binary, "run",
		// Removed when it exits. The name exists for the case where it does not.
		"--rm",
		"--name", name,
		"--label", containerLabel,
		// "none" by default: a sandbox that can reach the internet is a different threat
		// model, and it is one an operator turns on deliberately.
		"--network", d.network,
		"--memory", d.memory,
		// Equal to the memory limit, which is how docker is told not to give the container
		// swap. Without it the limit is a limit on speed rather than on memory.
		"--memory-swap", d.memory,
		"--pids-limit", strconv.Itoa(maxContainerPIDs),
		// A tool call needs no capabilities at all, and no way to acquire any: cap-drop
		// alone still leaves a setuid binary inside the image able to escalate.
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		// The image's own filesystem is not a place to keep state — a tool that writes
		// outside the workspace is writing somewhere that vanishes with the container, so
		// failing loudly is more useful than succeeding invisibly.
		"--read-only",
		"--tmpfs", "/tmp:rw,nosuid,size=" + tmpfsSize,
		"--volume", workspace + ":" + containerWorkspace,
		"--workdir", containerWorkspace,
		// HOME inside the container, so a tool writing config writes it in the workspace
		// where the run's own files are.
		"--env", "HOME=" + containerWorkspace,
	}
	if d.user != "" {
		// The host's own uid, so files the agent creates in the mounted workspace belong to
		// the service rather than to root. Without it, one docker run leaves a workspace
		// this service can no longer write to.
		args = append(args, "--user", d.user)
	}
	// sh, whatever the image declares. An image is a filesystem here, not an
	// application: honouring an ENTRYPOINT would mean `docker run image sh -c script`
	// becoming arguments to somebody else's program, so one image swap could change what
	// every script means.
	args = append(args, "--entrypoint", "sh", d.image, "-c", script)
	return args
}

// remove deletes a container the client no longer has hold of.
func (d *Docker) remove(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()

	result, err := runProcess(ctx, "", []string{d.binary, "rm", "--force", name}, dockerEnv())
	if err != nil {
		return fmt.Errorf("the sandbox container %s could not be removed: %v", name, err)
	}
	if result.ExitCode != 0 && !strings.Contains(strings.ToLower(result.Stderr), "no such container") {
		// "No such container" is the ordinary case: the container ended on its own and
		// --rm already took it away.
		return fmt.Errorf("the sandbox container %s could not be removed: docker exited %d", name, result.ExitCode)
	}
	return nil
}

// containerName is unique per command, and unguessable.
//
// Unguessable because the name is what remove acts on: a predictable one would let
// anything else with a docker socket delete a container by guessing which run was
// next.
func containerName() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("name a sandbox container: %w", err)
	}
	return "wingman-" + hex.EncodeToString(raw), nil
}

// dockerEnv is what the docker CLI itself may see.
//
// The DOCKER_* variables are how an operator points the CLI at a daemon that is not on
// the local socket, so they are passed through, along with the platform's own
// must-haves. This process's secrets are not: the CLI has no use for them, and a docker
// command line ends up in a process list.
func dockerEnv() []string {
	env := []string{pathEnv()}
	env = append(env, platformEnv()...)
	return append(env, passThrough(
		"HOME",
		"DOCKER_HOST",
		"DOCKER_CONTEXT",
		"DOCKER_CONFIG",
		"DOCKER_CERT_PATH",
		"DOCKER_TLS_VERIFY",
	)...)
}

// hostUser is the uid:gid to run a container as, or empty where there is no such thing.
//
// os.Getuid reports -1 on Windows, where the host has no numeric uid to hand to a Linux
// container and docker's own default is correct.
func hostUser() string {
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}
