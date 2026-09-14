package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// workspaces builds a Workspaces rooted in a temporary directory.
func workspaces(t *testing.T) *Workspaces {
	t.Helper()
	w, err := NewWorkspaces(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("build workspaces: %v", err)
	}
	return w
}

func TestNewWorkspaces_createsARootAndMakesItAbsolute(t *testing.T) {
	// Arrange
	// Relative, and two levels deep, because the configured default is the relative
	// "workspaces" and an operator may point it at a disk that has nothing on it yet.
	base := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	// Act
	w, err := NewWorkspaces("var/workspaces")

	// Assert
	if err != nil {
		t.Fatalf("a relative root was refused: %v", err)
	}
	// Absolute from here on: a relative root would resolve against the working directory
	// of whatever started the process, which differs between a unit, a container and a
	// terminal.
	if !filepath.IsAbs(w.Root()) {
		t.Errorf("Root() = %q; want an absolute path", w.Root())
	}
	info, err := os.Stat(w.Root())
	if err != nil || !info.IsDir() {
		t.Fatalf("the root was not created: %v", err)
	}
}

func TestNewWorkspaces_refusesARootItWasNotGiven(t *testing.T) {
	// Arrange, Act, Assert
	// Loudly rather than defaulting to the working directory, which is this service's own
	// installation directory.
	for _, root := range []string{"", "   "} {
		if _, err := NewWorkspaces(root); err == nil {
			t.Errorf("root %q was accepted", root)
		}
	}
}

func TestWorkspaces_forGivesEachUserTheirOwnDirectory(t *testing.T) {
	// Arrange
	w := workspaces(t)

	// Act
	first, err := w.For("0191e2ab-user-one")
	if err != nil {
		t.Fatalf("first workspace: %v", err)
	}
	second, err := w.For("0191e2ab-user-two")
	if err != nil {
		t.Fatalf("second workspace: %v", err)
	}
	again, err := w.For("0191e2ab-user-one")
	if err != nil {
		t.Fatalf("second call for the first user: %v", err)
	}

	// Assert
	if first == second {
		t.Fatal("two users were given the same workspace")
	}
	// The same user keeps theirs, because a run that starts from an empty directory
	// cannot build on what the last one left.
	if again != first {
		t.Errorf("the same user got %q then %q", first, again)
	}
	for _, path := range []string{first, second} {
		if err := w.Contains(path); err != nil {
			t.Errorf("%q is not inside the root: %v", path, err)
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("%q was not created: %v", path, err)
		}
		if runtime.GOOS == "windows" {
			// Windows does not carry unix permission bits, and Go reports a synthesised
			// mode. The check that means something here is the one CI runs on Linux.
			continue
		}
		if mode := info.Mode().Perm(); mode != workspacePermissions {
			// 0700 because a workspace is one user's files. On a multi-user instance the
			// difference is whether one user's agent can read another's.
			t.Errorf("mode = %o; want %o", mode, workspacePermissions)
		}
	}
}

func TestWorkspaces_forRefusesAnIDThatIsNotADirectoryName(t *testing.T) {
	// Arrange
	// The id comes from our own database, so this is defence in depth. It is here because
	// the cost of being wrong is a path outside the root: ".." is one join away from the
	// parent of every workspace.
	w := workspaces(t)
	cases := map[string]string{
		"nothing":            "",
		"whitespace":         "   ",
		"the parent":         "..",
		"a traversal":        "../../etc",
		"a separator":        "a/b",
		"a windows separato": `a\b`,
		"an absolute path":   "/etc/passwd",
		"a hidden file":      ".ssh",
		"a leading dash":     "-rf",
		"a space":            "user one",
		"too long":           strings.Repeat("u", 65),
	}

	// Act, Assert
	for label, id := range cases {
		if _, err := w.For(id); err == nil {
			t.Errorf("%s (%q) was accepted as a workspace name", label, id)
		}
	}
}

func TestWorkspaces_containsAcceptsTheRootAndWhatIsUnderIt(t *testing.T) {
	// Arrange
	w := workspaces(t)
	nested := filepath.Join(w.Root(), "0191e2ab", "reports", "september")
	if err := os.MkdirAll(nested, workspacePermissions); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Act, Assert
	for _, path := range []string{w.Root(), filepath.Join(w.Root(), "0191e2ab"), nested} {
		if err := w.Contains(path); err != nil {
			t.Errorf("%q was refused: %v", path, err)
		}
	}
}

func TestWorkspaces_containsRefusesWhatIsOutsideIt(t *testing.T) {
	// Arrange
	w := workspaces(t)
	parent := filepath.Dir(w.Root())
	cases := map[string]string{
		"nothing":            "",
		"the parent":         parent,
		"a sibling":          filepath.Join(parent, "somebody-else"),
		"a traversal":        filepath.Join(w.Root(), "..", "somebody-else"),
		"a shared prefix":    w.Root() + "-evil",
		"somewhere entirely": filepath.Join(parent, "etc", "passwd"),
	}

	// Act, Assert
	for label, path := range cases {
		err := w.Contains(path)
		if err == nil {
			t.Errorf("%s (%q) was accepted as a workspace", label, path)
			continue
		}
		// The offending path is not echoed. It is either ours, in which case the root says
		// more, or it came from somewhere it should not have.
		if strings.Contains(err.Error(), path) && path != "" {
			t.Errorf("%s put the path in the error: %v", label, err)
		}
	}
}

func TestWorkspaces_containsRefusesALinkThatLeavesTheRoot(t *testing.T) {
	// Arrange
	// A purely lexical check passes this: the path is under the root, and what it points
	// at is not. It would then be mounted into a container as though it were one user's
	// files.
	w := workspaces(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(outside, workspacePermissions); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(w.Root(), "0191e2ab")
	if err := os.Symlink(outside, link); err != nil {
		// Creating a symlink on Windows needs a privilege this process usually lacks. The
		// check that matters here is the one CI runs on Linux.
		t.Skipf("symlinks are not available: %v", err)
	}

	// Act
	err := w.Contains(link)

	// Assert
	if err == nil {
		t.Fatal("a workspace that is a link out of the root was accepted")
	}
}

func TestNewWorkspaces_refusesARootItCannotCreate(t *testing.T) {
	// Arrange
	// An operator pointing the root at a path under a file — a typo, or a mount that did
	// not come up — has to hear about it at startup rather than on the first tool call.
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Act
	_, err := NewWorkspaces(filepath.Join(file, "workspaces"))

	// Assert
	if err == nil {
		t.Fatal("a root under a file was accepted")
	}
}

func TestWorkspaces_forReportsAWorkspaceItCannotCreate(t *testing.T) {
	// Arrange
	// Something is already sitting where this user's workspace goes. Whatever the cause,
	// running the command against the wrong directory is not one of the options.
	w := workspaces(t)
	if err := os.WriteFile(filepath.Join(w.Root(), "0191e2abuser"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Act
	_, err := w.For("0191e2abuser")

	// Assert
	if err == nil {
		t.Fatal("a workspace that could not be created was returned anyway")
	}
}

func TestBoundedBuffer_keepsWhatFitsAndSwallowsTheRest(t *testing.T) {
	// Arrange
	var buffer boundedBuffer
	chunk := strings.Repeat("x", maxCapturedBytes/2)

	// Act
	written := 0
	for i := 0; i < 4; i++ {
		n, err := buffer.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		written += n
	}

	// Assert
	// Every write is reported as complete, because a short write is how os/exec tells the
	// child its pipe is broken — a command whose output ran long would start failing
	// halfway through, which is a different outcome from the one we asked for.
	if written != 4*len(chunk) {
		t.Errorf("reported %d bytes written; want %d", written, 4*len(chunk))
	}
	if got := len(buffer.String()); got != maxCapturedBytes {
		t.Errorf("kept %d bytes; want the bound %d", got, maxCapturedBytes)
	}
}

func TestAppendLine_addsOneLineWhateverTheOutputEndedWith(t *testing.T) {
	// Arrange
	cases := map[string]struct {
		text, want string
	}{
		"nothing":          {"", "[note]"},
		"whitespace":       {"  \n", "[note]"},
		"no newline":       {"out", "out\n[note]"},
		"a trailing break": {"out\n", "out\n[note]"},
		"several breaks":   {"out\n\n\n", "out\n[note]"},
	}

	// Act, Assert
	for label, c := range cases {
		if got := appendLine(c.text, "[note]"); got != c.want {
			t.Errorf("%s: appendLine(%q) = %q; want %q", label, c.text, got, c.want)
		}
	}
}

func TestRunProcess_refusesToStartNothing(t *testing.T) {
	// Arrange, Act
	_, err := runProcess(context.Background(), t.TempDir(), nil, nil)

	// Assert
	if err == nil {
		t.Fatal("an empty argv was accepted")
	}
}

func TestRunProcess_reportsABinaryThatIsNotInstalledWithoutEchoingTheScript(t *testing.T) {
	// Arrange
	// Which binary is missing is exactly what an operator has to install, so it is named.
	// The rest of argv is not: it holds the model's script, and a script can carry
	// anything the user typed into a chat.
	secret := "curl -H 'Authorization: Bearer sk-live-4f19' https://billing.internal"
	argv := []string{"wingman-not-an-executable", "-c", secret}

	// Act
	_, err := runProcess(context.Background(), t.TempDir(), argv, nil)

	// Assert
	if err == nil {
		t.Fatal("a missing binary was reported as a successful run")
	}
	if !strings.Contains(err.Error(), argv[0]) {
		t.Errorf("the error does not name the binary: %v", err)
	}
	if strings.Contains(err.Error(), "sk-live-4f19") {
		t.Errorf("the error carries the script: %v", err)
	}
}
