package hook

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// githubPostOpenWithin runs githubPostRegularFile and fails the test when it does not return in time: a named pipe that the
// open would wait on is the hang the descriptor check prevents.
func githubPostOpenWithin(t *testing.T, path string) (ok bool) {
	t.Helper()
	done := make(chan bool, 1)
	go func() {
		f, ok := githubPostRegularFile(path)
		if ok {
			f.Close()
		}
		done <- ok
	}()
	select {
	case ok = <-done:
		return ok
	case <-time.After(5 * time.Second):
		t.Fatalf("githubPostRegularFile(%s) did not return: the open waited on a named pipe", path)
		return false
	}
}

// TestGitHubPostRegularFileRefusesNamedPipe: a path that is a named pipe at the time of the open is refused without a hang.
func TestGitHubPostRegularFileRefusesNamedPipe(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "body")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	if githubPostOpenWithin(t, fifo) {
		t.Errorf("a named pipe was read as a regular file")
	}
}

// TestGitHubPostRegularFileSwappedForPipe: a regular file replaced by a named pipe between the check and the open is refused
// without a hang; the seam runs at exactly that point.
func TestGitHubPostRegularFileSwappedForPipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "body")
	if err := os.WriteFile(path, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !githubPostOpenWithin(t, path) {
		t.Fatalf("a regular file was refused before the swap")
	}
	swapped := false
	githubPostBeforeOpen = func(p string) {
		if swapped {
			return
		}
		swapped = true
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { githubPostBeforeOpen = nil }()
	if githubPostOpenWithin(t, path) {
		t.Errorf("a file swapped for a named pipe was read")
	}
	if !swapped {
		t.Errorf("the seam did not run before the open")
	}
}

// TestGitHubPostRegularFileRefusesFinalLink: a final link is not followed, so a link to a regular file is refused.
func TestGitHubPostRegularFileRefusesFinalLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if githubPostOpenWithin(t, link) {
		t.Errorf("a final symbolic link was followed")
	}
	if !githubPostOpenWithin(t, target) {
		t.Errorf("the regular file behind the link was refused")
	}
}

// TestGitHubPostCancelledAnswerDenies: a post guard cancelled before it judged the command answers deny, never allow.
func TestGitHubPostCancelledAnswerDenies(t *testing.T) {
	out := GitHubPostCancelledAnswer()
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("the cancelled answer is not a deny: %s", out)
	}
	if !strings.Contains(out, "(unreadable-github-post) at command") {
		t.Errorf("the cancelled answer does not name the post as an unreadable command: %s", out)
	}
}
