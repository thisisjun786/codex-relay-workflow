package harness

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-632: the writers that still waited for their lock without a deadline end on the first interrupt,
// as the ingest does since CRW-627. The oracle (CXC v0.2.40) has no lock and no signal handler: its
// process dies at the first SIGINT and writes nothing more, so a run held on the lock must end with
// Interrupted (130), print nothing and leave the locked file as the holder had it. On the base each of
// these runs is still waiting 5 s after the signal, because the wait happens in the kernel.
//
// The four locks differ in shape: metric record and divergence candidate add lock a file (the append
// descriptor is what /proc shows, as c627AppendOpen reads it), metric kind and divergence mode lock the
// kind/divergence directory (a plain descriptor on the directory). A run stopped at a directory lock has
// already made that directory with MkdirAll, so its state check asks for the session file, not the dir.
//
// Linking testsupport keeps CRW_REFUSE_LIVE_STATE=1 for this test binary (decisions.md 46). This file
// reuses the package's TestMain (pabcd_metric_interrupt_test.go:34); package harness has one.

// crw632Case is one writer: the command that waits on a lock, the lock the test holds, and the state
// the interrupted run must leave.
type crw632Case struct {
	name string
	args []string
	// prepare creates the workspace state and takes the lock, returning the locked path (as /proc shows
	// it) and whether it is a directory. Its cleanup releases the lock.
	prepare func(t *testing.T, root string) (target string, dir bool)
	check   func(t *testing.T, root string)
}

// crw632Resolved is the path /proc reports for a descriptor on path.
func crw632Resolved(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// crw632HoldFile creates a regular file and locks it exclusively through a descriptor opened without
// O_APPEND, so an append descriptor on the file can only be the child's (the same shape c627HoldLedger
// uses for the ledger).
func crw632HoldFile(t *testing.T, path string) (target string, release func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() }) // closing drops the lock
	return crw632Resolved(t, path), func() { f.Close() }
}

// crw632HoldDir locks a directory exclusively, which is where metric kind and divergence mode wait.
func crw632HoldDir(t *testing.T, path string) (target string, release func()) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return crw632Resolved(t, path), func() { f.Close() }
}

// crw632FileAbsent is the state check of a run stopped at a directory lock: the session file the writer
// would have replaced is not there, whatever the directory holds.
func crw632FileAbsent(path string) func(t *testing.T, root string) {
	return func(t *testing.T, root string) {
		t.Helper()
		if _, err := os.Lstat(filepath.Join(root, path)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the interrupted run left %s: %v", path, err)
		}
	}
}

// crw632FileUnchanged is the state check of a run stopped at a file lock: the file still holds exactly
// the bytes the holder had.
func crw632FileUnchanged(path string, want string) func(t *testing.T, root string) {
	return func(t *testing.T, root string) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(raw) != want {
			t.Fatalf("the interrupted run changed %s: %v %q, want %q", path, err, raw, want)
		}
	}
}

// crw632Cases is the four writers, each with the lock it waits on.
func crw632Cases() []crw632Case {
	ledger, archive := ".crw/metrics.jsonl", ".crw/divergence/candidates.jsonl"
	return []crw632Case{
		{
			name: "metric record waits on the ledger",
			args: []string{"metric", "record", "--session", "s1", "--name", "n", "--value", "1"},
			prepare: func(t *testing.T, root string) (string, bool) {
				target, _ := crw632HoldFile(t, filepath.Join(root, ledger))
				return target, false
			},
			check: crw632FileUnchanged(ledger, ""),
		},
		{
			name: "metric kind waits on the kind directory",
			args: []string{"metric", "kind", "--session", "s1", "maximize"},
			prepare: func(t *testing.T, root string) (string, bool) {
				target, _ := crw632HoldDir(t, filepath.Join(root, ".crw", "objective-kind"))
				return target, true
			},
			check: crw632FileAbsent(".crw/objective-kind/s1.json"),
		},
		{
			name: "divergence mode waits on the divergence directory",
			args: []string{"divergence", "mode", "on", "--session", "s1"},
			prepare: func(t *testing.T, root string) (string, bool) {
				target, _ := crw632HoldDir(t, filepath.Join(root, ".crw", "divergence"))
				return target, true
			},
			check: crw632FileAbsent(".crw/divergence/s1.mode.json"),
		},
		{
			name: "divergence candidate add waits on the archive",
			args: []string{"divergence", "candidate", "add", "--session", "s1", "--kind", "strong-1",
				"--title", "t", "--rationale", "r", "--source", "u"},
			prepare: func(t *testing.T, root string) (string, bool) {
				target, _ := crw632HoldFile(t, filepath.Join(root, archive))
				return target, false
			},
			check: crw632FileUnchanged(archive, ""),
		},
	}
}

// crw632ChildHolds reports whether process pid has reached the lock: for a file lock the append
// descriptor the writer opens just before it asks for it, for a directory lock a descriptor whose
// target is the directory.
func crw632ChildHolds(pid int, target string, dir bool) bool {
	if !dir {
		return c627AppendOpen(pid, target)
	}
	dirFd := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	fds, err := os.ReadDir(dirFd)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		if got, err := os.Readlink(filepath.Join(dirFd, fd.Name())); err == nil && got == target {
			return true
		}
	}
	return false
}

// TestPabcdWritersEndOnTheFirstInterruptWhileTheLockIsHeld runs the built crw once per writer while the
// test holds that writer's lock and sends the first SIGINT once the child has reached it. Each run must
// end within 5 s with 130, print nothing and leave the locked state as the holder had it. Before this
// change the wait ignored the signal in all four, so each case fails here at the 5 s bound.
func TestPabcdWritersEndOnTheFirstInterruptWhileTheLockIsHeld(t *testing.T) {
	if _, err := os.Stat("/proc/self/fdinfo"); err != nil {
		t.Skip("the case reads /proc to see the child reach the lock")
	}
	crw := testsupport.CRW(t)
	for _, tc := range crw632Cases() {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, "work")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			target, dir := tc.prepare(t, root)
			before := c620HomeListings(t, home)
			cmd := exec.Command(crw, append([]string{"pabcd"}, tc.args...)...)
			cmd.Dir = root
			cmd.Env = []string{
				"HOME=" + home,
				"CODEX_HOME=" + filepath.Join(home, "codex"),
				"CRW_HOME=" + filepath.Join(home, "crw"),
				"PATH=" + os.Getenv("PATH"),
				testsupport.RefuseLiveStateEnv + "=1",
			}
			cmd.Stdin = strings.NewReader("")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			ended := false
			t.Cleanup(func() {
				if !ended {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-done
				}
			})
			for deadline := time.Now().Add(5 * time.Second); !crw632ChildHolds(cmd.Process.Pid, target, dir); time.Sleep(10 * time.Millisecond) {
				select {
				case <-done:
					ended = true
					t.Fatalf("the run ended before it reached the lock\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the run did not reach the lock within 5 s")
				}
			}
			if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
				ended = true
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
				ended = true
				t.Fatalf("the run still ran 5 s after the SIGINT\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			}
			if code := cmd.ProcessState.ExitCode(); code != Interrupted {
				t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, Interrupted, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("the interrupted run wrote to its streams\nstdout:\n%q\nstderr:\n%q", stdout.String(), stderr.String())
			}
			tc.check(t, root)
			if after := c620HomeListings(t, home); after != before {
				t.Fatalf("the run changed the child's home listings\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// TestPabcdMetricKindReadsUnderAnEndedContext keeps the reading half of the kind row: without a kind
// argument the verb only reads, so it prints its answer even when the invocation's context has ended,
// exactly as metric show does. Only "kind <satisfy|maximize>" writes and can be cut short (CRW-632).
func TestPabcdMetricKindReadsUnderAnEndedContext(t *testing.T) {
	root := pabcdCLITestHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := PabcdContext(ctx, []string{"metric", "kind", "--session", "s1"}, strings.NewReader(""), &stdout, &stderr, Verbs()); code != 0 || stdout.String() != "metric kind: satisfy\n" || stderr.Len() != 0 {
		t.Fatalf("reading metric kind under an ended context: code %d, stdout %q, stderr %q; want the answer with exit 0", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(c620MetricRecord(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading metric kind wrote a record: %v", err)
	}
}
