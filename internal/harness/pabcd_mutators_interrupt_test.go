package harness

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1074: the loop steer, memory allow-write and scan record rows end on the first SIGINT, as the
// orchestrate row has since CRW-871. The oracle (CXC v0.2.40) is a short-lived process with no signal
// handler, so the first interrupt ends it at once (signal exit, shell 130) with empty streams and
// nothing written; the port turns the signal into the invocation's context (cmd/crw serve) and these
// rows were registered with Run, so the context never reached them.

// pabcd1074Seed makes the workspace the three rows act on: session s in IDLE and, for steer, a goalplan
// bound to it. The tree is snapshotted by the caller after this returns.
func pabcd1074Seed(t *testing.T, bindPlan bool) string {
	t.Helper()
	root := orchestrateTestHome(t)
	if err := state.WriteState(root, state.DefaultState("s", "")); err != nil {
		t.Fatal(err)
	}
	if bindPlan {
		pabcd1074BindPlan(t, root)
	}
	return root
}

// pabcd1074BindPlan writes a goalplan and binds session s to it, the state loop init leaves in a git
// workspace; the workspace here has no git source identity, which loop init --session refuses.
func pabcd1074BindPlan(t *testing.T, root string) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{
		Objective: "interrupt probe",
		Criteria:  []goalplan.NewGoalplanCriterion{{Scenario: "it ends", ExpectedEvidence: "exit 130"}},
		Now:       func() string { return "2026-08-29T00:00:00.000Z" },
	})
	if err := goalplan.WriteGoalplan(root, plan); err != nil {
		t.Fatal(err)
	}
	bound := state.DefaultState("s", "")
	bound.Slug = plan.Slug
	if err := state.WriteState(root, bound); err != nil {
		t.Fatal(err)
	}
}

// pabcd1074Rows is the three rows with the argument lists the issue names. steer takes its batch from
// the file the case supplies.
func pabcd1074Rows(batch string) map[string][]string {
	return map[string][]string{
		"loop steer":         {"loop", "steer", "--session", "s", "--batch-json", batch},
		"memory allow-write": {"memory", "allow-write", "--session", "s"},
		"scan record":        {"scan", "record", "--session", "s", "--known", "goal=after-interrupt"},
	}
}

const pabcd1074Batch = `{"idempotencyKey":"k1","rationale":"r","evidence":"e","ops":[{"kind":"annotate","note":"n"}]}`

// pabcd1074WithoutLock is the tree without the lock files a held lock leaves for the test itself.
func pabcd1074WithoutLock(tree map[string]string) map[string]string {
	for name := range tree {
		if strings.HasSuffix(name, ".lock") {
			delete(tree, name)
		}
	}
	return tree
}

// TestPabcdLoopSteerEndsOnTheFirstInterruptWhileTheBatchStreamWaits reads the batch from a FIFO nobody
// has opened for writing, the in-process shape of --batch-json /proc/self/fd/0 on an unfinished pipe.
// On the base the read ignored the context and the row was still waiting 5 s after the signal.
func TestPabcdLoopSteerEndsOnTheFirstInterruptWhileTheBatchStreamWaits(t *testing.T) {
	root := pabcd1074Seed(t, true)
	fifo := filepath.Join(t.TempDir(), "batch.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // release the reader blocked in open so a failing run does not leak it
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			f.Close()
		}
	})
	before := orchestrateTestTree(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- PabcdContext(ctx, pabcd1074Rows(fifo)["loop steer"], strings.NewReader(""), &stdout, &stderr, Verbs())
	}()
	time.Sleep(50 * time.Millisecond) // the row is inside the batch read
	cancel()
	select {
	case code := <-done:
		if code != Interrupted || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("interrupted steer: code %d, stdout %q, stderr %q; want %d with nothing written", code, stdout.String(), stderr.String(), Interrupted)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop steer row was still reading its batch 5 s after the SIGINT")
	}
	orchestrateTestSameTree(t, before, orchestrateTestTree(t, root))
}

// TestPabcdSessionLockRowsEndOnTheFirstInterrupt holds the session lock, starts the row, ends the
// invocation while the row waits and releases the lock before the wait budget runs out, so the signal is
// what decides the answer. On the base each row took the lock after the release and published: memory
// allow-write the grant and exit 0, scan record the tracker and the scan_completed row and exit 0.
func TestPabcdSessionLockRowsEndOnTheFirstInterrupt(t *testing.T) {
	for _, name := range []string{"memory allow-write", "scan record"} {
		t.Run(name, func(t *testing.T) {
			root := pabcd1074Seed(t, false)
			before := orchestrateTestTree(t, root)
			lockPath := state.StatePath(root, "s") + ".lock"
			if err := os.WriteFile(lockPath, []byte("held"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stdout, stderr bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- PabcdContext(ctx, pabcd1074Rows("")[name], strings.NewReader(""), &stdout, &stderr, Verbs())
			}()
			time.Sleep(40 * time.Millisecond) // the row is inside the lock wait (the budget is about 250 ms)
			cancel()
			time.Sleep(35 * time.Millisecond)
			if err := os.Remove(lockPath); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-done:
				if code != Interrupted || stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("interrupted %s: code %d, stdout %q, stderr %q; want %d with nothing written", name, code, stdout.String(), stderr.String(), Interrupted)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the %s row was still running 5 s after the SIGINT", name)
			}
			orchestrateTestSameTree(t, pabcd1074WithoutLock(before), pabcd1074WithoutLock(orchestrateTestTree(t, root)))
		})
	}
}

// TestPabcdMutatorRowsOnAnEndedContextWriteNothing hands PabcdContext a context that has already ended:
// each row answers Interrupted with nothing printed and nothing written.
func TestPabcdMutatorRowsOnAnEndedContextWriteNothing(t *testing.T) {
	for _, name := range []string{"loop steer", "memory allow-write", "scan record"} {
		t.Run(name, func(t *testing.T) {
			root := pabcd1074Seed(t, true)
			batch := filepath.Join(root, "batch.json")
			if err := os.WriteFile(batch, []byte(pabcd1074Batch), 0o600); err != nil {
				t.Fatal(err)
			}
			before := orchestrateTestTree(t, root)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var stdout, stderr bytes.Buffer
			code := PabcdContext(ctx, pabcd1074Rows(batch)[name], strings.NewReader(""), &stdout, &stderr, Verbs())
			if code != Interrupted || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("%s under an ended context: code %d, stdout %q, stderr %q; want %d with nothing printed", name, code, stdout.String(), stderr.String(), Interrupted)
			}
			orchestrateTestSameTree(t, before, orchestrateTestTree(t, root))
		})
	}
}

// TestPabcdMutatorRowsWithoutAnInterruptAnswerAsBefore is the control: a live context changes nothing.
func TestPabcdMutatorRowsWithoutAnInterruptAnswerAsBefore(t *testing.T) {
	root := pabcd1074Seed(t, true)
	batch := filepath.Join(root, "batch.json")
	if err := os.WriteFile(batch, []byte(pabcd1074Batch), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, want string }{
		{"loop steer", "loop steer: applied k1 (1 op(s): annotate)\n"},
		{"memory allow-write", "memory allow-write: session s may perform ONE memory write; grant recorded for cwd " + root + "; the next write consumes this grant.\n"},
		{"scan record", "scan record: round 1 recorded for session s (contradictions=0, high=0)\n"},
	} {
		var stdout, stderr bytes.Buffer
		code := PabcdContext(context.Background(), pabcd1074Rows(batch)[tc.name], strings.NewReader(""), &stdout, &stderr, Verbs())
		if code != 0 || stdout.String() != tc.want || stderr.Len() != 0 {
			t.Fatalf("%s: code %d, stdout %q, stderr %q; want %q", tc.name, code, stdout.String(), stderr.String(), tc.want)
		}
	}
	if !state.ReadState(root, "s").MemoryWriteGrant {
		t.Fatal("memory allow-write did not record the grant")
	}
}

// pabcd1074Run starts the built crw on args in root with a private home and the given stdin.
func pabcd1074Run(t *testing.T, crw, home, root string, args []string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(crw, append([]string{"pabcd"}, args...)...)
	cmd.Dir = root
	cmd.Env = []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, "codex"),
		"CRW_HOME=" + filepath.Join(home, "crw"),
		"PATH=" + os.Getenv("PATH"),
		testsupport.RefuseLiveStateEnv + "=1",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	return cmd, &stdout, &stderr
}

// pabcd1074Tree snapshots the child's whole home, the workspace included.
func pabcd1074Tree(t *testing.T, home string) map[string]string {
	t.Helper()
	return pabcd1074WithoutLock(orchestrateTestTree(t, home))
}

// pabcd1074ChildReadsStdin reports whether the child has opened a second descriptor on the pipe that
// is its fd 0, which is what os.ReadFile("/proc/self/fd/0") does just before it blocks in read.
func pabcd1074ChildReadsStdin(pid int) bool {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	stdin, err := os.Readlink(filepath.Join(dir, "0"))
	if err != nil {
		return false
	}
	fds, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	same := 0
	for _, fd := range fds {
		if got, err := os.Readlink(filepath.Join(dir, fd.Name())); err == nil && got == stdin {
			same++
		}
	}
	return same >= 2
}

// TestPabcdLoopSteerBinaryEndsOnTheFirstInterruptOnAnOpenStdin is the issue's reproduction on the built
// crw: --batch-json /proc/self/fd/0 on a pipe nobody finishes, one SIGINT. The oracle dies at once with
// empty streams; the base was still alive after the signal and, once x and EOF arrived, printed a JSON
// error and exited 1.
func TestPabcdLoopSteerBinaryEndsOnTheFirstInterruptOnAnOpenStdin(t *testing.T) {
	if _, err := os.Stat("/proc/self/fdinfo"); err != nil {
		t.Skip("the case reads /proc to see the child reach its stdin")
	}
	crw := testsupport.CRW(t)
	home := t.TempDir()
	root := filepath.Join(home, "work")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	pabcd1074BindPlan(t, root)
	before := pabcd1074Tree(t, home)
	cmd, stdout, stderr := pabcd1074Run(t, crw, home, root, []string{"loop", "steer", "--session", "s", "--batch-json", "/proc/self/fd/0"})
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
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
	for deadline := time.Now().Add(10 * time.Second); !pabcd1074ChildReadsStdin(cmd.Process.Pid); time.Sleep(5 * time.Millisecond) {
		select {
		case <-done:
			ended = true
			t.Fatalf("the run ended before it read its stdin\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not reach its stdin read within 10 s")
		}
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		ended = true
	case <-time.After(5 * time.Second):
		// The base: still alive. Finish it the way the issue does (x and EOF) so the failure shows what it printed.
		_, _ = stdin.Write([]byte("x"))
		_ = stdin.Close()
		select {
		case <-done:
			ended = true
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			ended = true
		}
		t.Fatalf("the run was still alive 5 s after the SIGINT\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	_, _ = stdin.Write([]byte("x")) // too late to matter: the process is gone
	_ = stdin.Close()
	if code := cmd.ProcessState.ExitCode(); code != Interrupted {
		t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, Interrupted, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("the interrupted run wrote to its streams\nstdout:\n%q\nstderr:\n%q", stdout.String(), stderr.String())
	}
	orchestrateTestSameTree(t, before, pabcd1074Tree(t, home))
}

// TestPabcdSessionLockRowsBinaryEndOnTheFirstInterrupt is the issue's lock reproduction on the built crw:
// the test holds s.json.lock, starts the row, sends one SIGINT 40 ms after the process is up and drops the
// lock 35 ms later. The row must end with 130, print nothing and leave every byte as it was.
//
// Nothing shows from outside that the child has reached the lock wait, so a run that the signal reached
// before the process installed its handler (killed by SIGINT, no exit code) or that gave its wait up
// before the signal (the host was too busy to start it in time) is repeated, up to five times.
func TestPabcdSessionLockRowsBinaryEndOnTheFirstInterrupt(t *testing.T) {
	crw := testsupport.CRW(t)
	for _, name := range []string{"memory allow-write", "scan record"} {
		t.Run(name, func(t *testing.T) {
			for attempt := 1; ; attempt++ {
				home := t.TempDir()
				root := filepath.Join(home, "work")
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := state.WriteState(root, state.DefaultState("s", "")); err != nil {
					t.Fatal(err)
				}
				lockPath := state.StatePath(root, "s") + ".lock"
				if err := os.WriteFile(lockPath, []byte("held"), 0o600); err != nil {
					t.Fatal(err)
				}
				before := pabcd1074Tree(t, home)
				cmd, stdout, stderr := pabcd1074Run(t, crw, home, root, pabcd1074Rows("")[name])
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				time.Sleep(40 * time.Millisecond)
				_ = cmd.Process.Signal(syscall.SIGINT)
				time.Sleep(35 * time.Millisecond)
				_ = os.Remove(lockPath)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-done
					t.Fatalf("the run was still alive 5 s after the SIGINT\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
				}
				code := cmd.ProcessState.ExitCode()
				if attempt < 5 && (code == -1 || code == 1 && strings.Contains(stdout.String(), "lock")) {
					continue
				}
				if code != Interrupted {
					t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, Interrupted, stdout.String(), stderr.String())
				}
				if stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("the interrupted run wrote to its streams\nstdout:\n%q\nstderr:\n%q", stdout.String(), stderr.String())
				}
				orchestrateTestSameTree(t, before, pabcd1074Tree(t, home))
				return
			}
		})
	}
}

// TestPabcdLoopSteerRefusalOnAnEndedContextIsSilent: a malformed inline batch is refused before any lock, and the
// refusal is still the answer of a process the signal has already ended: Interrupted, nothing printed.
func TestPabcdLoopSteerRefusalOnAnEndedContextIsSilent(t *testing.T) {
	root := pabcd1074Seed(t, true)
	before := orchestrateTestTree(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := PabcdContext(ctx, []string{"loop", "steer", "--session", "s", "--batch-json", "{"}, strings.NewReader(""), &stdout, &stderr, Verbs())
	if code != Interrupted || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("malformed batch under an ended context: code %d, stdout %q, stderr %q; want %d with nothing printed", code, stdout.String(), stderr.String(), Interrupted)
	}
	orchestrateTestSameTree(t, before, orchestrateTestTree(t, root))
}

// TestPabcdMutatorRowsBinaryEndOnASessionFileThatIsAFIFOWithAnOpenWriter is the post-evaluation d1 case on the built
// crw: the session state is a FIFO whose writer stays open without EOF. The state reads that precede the first
// write used to block in read for good, so the first SIGINT could not end the run (and memory and scan kept their
// session lock). The reader now refuses a state file that is not a regular file without reading it: the run ends
// by itself with its refusal (exit 1), and a SIGINT sent at the start ends it too (130, or the refusal when the
// refusal came first), with the lock gone either way.
func TestPabcdMutatorRowsBinaryEndOnASessionFileThatIsAFIFOWithAnOpenWriter(t *testing.T) {
	crw := testsupport.CRW(t)
	for _, name := range []string{"loop steer", "memory allow-write", "scan record"} {
		for _, signalled := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/no signal", true: "/SIGINT"}[signalled], func(t *testing.T) {
				home := t.TempDir()
				root := filepath.Join(home, "work")
				if err := os.MkdirAll(filepath.Join(root, ".crw", "sessions"), 0o755); err != nil {
					t.Fatal(err)
				}
				path := state.StatePath(root, "s")
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				holder, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer holder.Close()
				cmd, stdout, stderr := pabcd1074Run(t, crw, home, root, pabcd1074Rows(pabcd1074Batch)[name])
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				if signalled {
					time.Sleep(20 * time.Millisecond)
					_ = cmd.Process.Signal(syscall.SIGINT)
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-done
					t.Fatalf("the run did not end on a FIFO session file (signalled %v)\nstdout:\n%s\nstderr:\n%s", signalled, stdout.String(), stderr.String())
				}
				code := cmd.ProcessState.ExitCode()
				switch {
				case !signalled && code != 1:
					t.Fatalf("exit code %d, want the refusal's 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
				case signalled && code != Interrupted && code != 1:
					t.Fatalf("exit code %d, want %d or the refusal's 1\nstdout:\n%s\nstderr:\n%s", code, Interrupted, stdout.String(), stderr.String())
				}
				if code == Interrupted && (stdout.Len() != 0 || stderr.Len() != 0) {
					t.Fatalf("the interrupted run wrote to its streams\nstdout:\n%q\nstderr:\n%q", stdout.String(), stderr.String())
				}
				if _, err := os.Lstat(path + ".lock"); !os.IsNotExist(err) {
					t.Fatalf("the session lock was left behind: %v", err)
				}
				if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
					t.Fatalf("the session state was replaced: %v %v", info, err)
				}
			})
		}
	}
}
