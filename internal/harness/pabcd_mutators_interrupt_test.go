package harness

import (
	"bytes"
	"context"
	"errors"
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
// the test holds s.json.lock, starts the row, waits until the process reports that it is blocked on that lock
// and sends one SIGINT. The row must end with 130, print nothing, leave the test's lock and every other byte as
// it was.
//
// The report is the ready file of a crw built with -tags sigintprobe (state.lockWaitProbe): the wait runs in
// the row, after serve has registered the handler, and the probe build lets it last 30 s, so the signal always
// finds the process in the wait and the wait cannot end by itself first (CRW-1167). Nothing is retried: a run
// that ends before it reports, dies of the signal or answers anything but 130 fails.
func TestPabcdSessionLockRowsBinaryEndOnTheFirstInterrupt(t *testing.T) {
	probe := testsupport.BuildCRW(t, "-tags", "sigintprobe")
	for _, name := range []string{"memory allow-write", "scan record"} {
		t.Run(name, func(t *testing.T) { pabcd1167LockWaitRun(t, probe, name, false) })
	}
}

// pabcd1167Ended waits for the child's end, killing its process group when it outlives the budget.
func pabcd1167Ended(t *testing.T, cmd *exec.Cmd, done <-chan struct{}, stdout, stderr *bytes.Buffer, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("%s\nstdout:\n%s\nstderr:\n%s", what, stdout.String(), stderr.String())
	}
}

// pabcd1167WantInterrupted judges an ended child: it exited by itself with 130 (not killed by a signal) and wrote
// nothing.
func pabcd1167WantInterrupted(t *testing.T, cmd *exec.Cmd, stdout, stderr *bytes.Buffer) {
	t.Helper()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Exited() || status.ExitStatus() != Interrupted {
		t.Fatalf("the child ended with %v, want exit code %d (a child killed by the signal never ran its handler)\nstdout:\n%s\nstderr:\n%s", cmd.ProcessState, Interrupted, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("the interrupted run wrote to its streams\nstdout:\n%q\nstderr:\n%q", stdout.String(), stderr.String())
	}
}

// pabcd1167LockWaitRun is one SIGINT run of a lock-taking row (memory allow-write, scan record) on the probe
// build: the test holds the session lock, the row waits for it, and the signal is sent only after the process has
// said so through its ready file. With stateFIFO the session file is a FIFO whose writer stays open, as in the
// d1 case; the held lock keeps the row from ever reading it.
func pabcd1167LockWaitRun(t *testing.T, probe, name string, stateFIFO bool) {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "work")
	if err := os.MkdirAll(filepath.Join(root, ".crw", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := state.StatePath(root, "s")
	if stateFIFO {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		holder, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
	} else if err := state.WriteState(root, state.DefaultState("s", "")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	var before map[string]string
	if !stateFIFO {
		before = pabcd1074Tree(t, home)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd, stdout, stderr := pabcd1074Run(t, probe, home, root, pabcd1074Rows("")[name])
	cmd.Env = append(cmd.Env, "CRW_SIGINTPROBE_READY_FILE="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	reported := false
	t.Cleanup(func() {
		if !reported {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-done:
			reported = true
			t.Fatalf("the run ended before it reported waiting for the lock: %v\nstdout:\n%s\nstderr:\n%s", cmd.ProcessState, stdout.String(), stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not report waiting for the lock within 10 s")
		}
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("the SIGINT was not delivered: %v", err)
	}
	pabcd1167Ended(t, cmd, done, stdout, stderr, "the run was still alive 5 s after the SIGINT")
	reported = true
	pabcd1167WantInterrupted(t, cmd, stdout, stderr)
	if held, err := os.ReadFile(path + ".lock"); err != nil || string(held) != "held" {
		t.Fatalf("the test's lock was not left as it was: %q %v", held, err)
	}
	if stateFIFO {
		if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("the session state was replaced: %v %v", info, err)
		}
		return
	}
	orchestrateTestSameTree(t, before, pabcd1074Tree(t, home))
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
// session lock). The reader now refuses a state file that is not a regular file without reading it.
//
// Without a signal the run ends by itself with that refusal (exit 1), the lock gone and the FIFO in place. Nothing
// is blocked then, so a SIGINT cannot be placed against it: the refusal is over within milliseconds and nothing
// shows from outside that the handler (installed at the top of main, after the runtime and every package
// initialiser) exists, and a signal that beats it meets SIGINT's default disposition, by design (decision 42, cmd/crw
// serve; CRW-1167 saw 94 of 600 such runs killed under CPU load). The SIGINT cases therefore wait for an observed
// blocked state first, on the same FIFO session file, and then demand exit 130 with nothing printed:
//   - memory allow-write and scan record: blocked on the session lock the test holds, reported by the probe build's
//     ready file (pabcd1167LockWaitRun);
//   - loop steer: blocked reading its batch from a FIFO, which the test's own open for writing proves, as it
//     returns only once the child has opened the FIFO for reading (pabcd1167SteerRun).
func TestPabcdMutatorRowsBinaryEndOnASessionFileThatIsAFIFOWithAnOpenWriter(t *testing.T) {
	crw := testsupport.CRW(t)
	probe := testsupport.BuildCRW(t, "-tags", "sigintprobe")
	for _, name := range []string{"loop steer", "memory allow-write", "scan record"} {
		t.Run(name+"/no signal", func(t *testing.T) { pabcd1167FIFORefusal(t, crw, name) })
		t.Run(name+"/SIGINT", func(t *testing.T) {
			if name == "loop steer" {
				pabcd1167SteerRun(t, crw)
				return
			}
			pabcd1167LockWaitRun(t, probe, name, true)
		})
	}
}

// pabcd1167FIFOHome is a workspace whose session file s is a FIFO with an open writer; the writer is closed with the
// test.
func pabcd1167FIFOHome(t *testing.T) (home, root, path string) {
	t.Helper()
	home = t.TempDir()
	root = filepath.Join(home, "work")
	if err := os.MkdirAll(filepath.Join(root, ".crw", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	path = state.StatePath(root, "s")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close() })
	return home, root, path
}

// pabcd1167FIFORefusal is the unsignalled run: the refusal's exit 1, no lock left, the FIFO still in place.
func pabcd1167FIFORefusal(t *testing.T, crw, name string) {
	t.Helper()
	home, root, path := pabcd1167FIFOHome(t)
	cmd, stdout, stderr := pabcd1074Run(t, crw, home, root, pabcd1074Rows(pabcd1074Batch)[name])
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	pabcd1167Ended(t, cmd, done, stdout, stderr, "the run did not end on a FIFO session file")
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code %d, want the refusal's 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("the session lock was left behind: %v", err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the session state was replaced: %v %v", info, err)
	}
}

// pabcd1167SteerRun is the SIGINT run of loop steer: its batch comes from a FIFO the test opens for writing and
// never writes to. The open returns only when the child has opened the FIFO for reading, which it does inside the
// row, after serve registered the handler, and the child then waits for the batch until the signal ends it.
func pabcd1167SteerRun(t *testing.T, crw string) {
	t.Helper()
	home, root, path := pabcd1167FIFOHome(t)
	batch := filepath.Join(t.TempDir(), "batch.fifo")
	if err := syscall.Mkfifo(batch, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, stdout, stderr := pabcd1074Run(t, crw, home, root, pabcd1074Rows(batch)["loop steer"])
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	writer := pabcd1167StartBatchWriter(func() (*os.File, error) { return os.OpenFile(batch, os.O_WRONLY, 0) })
	ended := false
	t.Cleanup(func() {
		if !ended {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		writer.release(batch)
	})
	switch err := writer.await(done, 10*time.Second); err {
	case nil:
	case errBatchRunEnded:
		ended = true
		t.Fatalf("the run ended before it opened its batch: %v\nstdout:\n%s\nstderr:\n%s", cmd.ProcessState, stdout.String(), stderr.String())
	default:
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("the SIGINT was not delivered: %v", err)
	}
	pabcd1167Ended(t, cmd, done, stdout, stderr, "the run was still alive 5 s after the SIGINT")
	ended = true
	pabcd1167WantInterrupted(t, cmd, stdout, stderr)
	if _, err := os.Lstat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("the session lock was left behind: %v", err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the session state was replaced: %v %v", info, err)
	}
}

var (
	errBatchRunEnded = errors.New("the run ended")
	errBatchTimeout  = errors.New("the run did not open its batch FIFO within 10 s")
)

// pabcd1167BatchWriter is the test's O_WRONLY open of the batch FIFO, run in a goroutine because it blocks until the
// child opens the FIFO for reading. The goroutine's single result is received once, by await or else by release.
type pabcd1167BatchWriter struct {
	result   chan pabcd1167BatchOpen
	received bool
	open     pabcd1167BatchOpen
}

type pabcd1167BatchOpen struct {
	f   *os.File
	err error
}

func pabcd1167StartBatchWriter(open func() (*os.File, error)) *pabcd1167BatchWriter {
	w := &pabcd1167BatchWriter{result: make(chan pabcd1167BatchOpen, 1)}
	go func() {
		f, err := open()
		w.result <- pabcd1167BatchOpen{f, err}
	}()
	return w
}

// await returns nil once the writer opened, the open's error, errBatchRunEnded when done closed first or
// errBatchTimeout.
func (w *pabcd1167BatchWriter) await(done <-chan struct{}, timeout time.Duration) error {
	select {
	case w.open = <-w.result:
		w.received = true
		return w.open.err
	case <-done:
		return errBatchRunEnded
	case <-time.After(timeout):
		return errBatchTimeout
	}
}

// release frees a writer still blocked in its open, or not yet in it, and closes the file. The reader it opens
// stays open until the goroutine's result is in (unless await already took it): a writer whose open begins after the
// child ended finds that reader and returns, where a reader closed at once left it blocked for good. A reader that
// cannot be opened is retried.
func (w *pabcd1167BatchWriter) release(batch string) {
	var r *os.File
	for !w.received {
		if r == nil {
			if f, err := os.OpenFile(batch, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				r = f
			}
		}
		select {
		case w.open = <-w.result:
			w.received = true
		case <-time.After(10 * time.Millisecond):
		}
	}
	if r != nil {
		r.Close()
	}
	if w.open.f != nil {
		w.open.f.Close()
	}
}

// TestPabcd1167BatchWriterReleasesAfterAFailedOpen: when the writer's open fails, the test's cleanup still gets
// the goroutine's result out of release; a release that waited for a second result would hang the package.
func TestPabcd1167BatchWriterReleasesAfterAFailedOpen(t *testing.T) {
	openErr := errors.New("open failed")
	w := pabcd1167StartBatchWriter(func() (*os.File, error) { return nil, openErr })
	if err := w.await(make(chan struct{}), 5*time.Second); err != openErr {
		t.Fatalf("await = %v, want the open's error", err)
	}
	released := make(chan struct{})
	go func() { w.release(filepath.Join(t.TempDir(), "absent.fifo")); close(released) }()
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("release hung after the failed open: the only result was consumed by await")
	}
}

// TestPabcd1171BatchWriterReleaseOutlastsALateOpen: the child ended before the writer goroutine reached its open, so
// no reader exists when the open starts. release must keep a reader open until the goroutine has its result; a release
// that closed its reader at once left the late open blocked forever and itself waiting for it.
func TestPabcd1171BatchWriterReleaseOutlastsALateOpen(t *testing.T) {
	batch := filepath.Join(t.TempDir(), "batch.fifo")
	if err := syscall.Mkfifo(batch, 0o600); err != nil {
		t.Fatal(err)
	}
	opening := make(chan struct{})
	w := pabcd1167StartBatchWriter(func() (*os.File, error) {
		<-opening
		return os.OpenFile(batch, os.O_WRONLY, 0)
	})
	released := make(chan struct{})
	go func() { w.release(batch); close(released) }()
	// the writer reaches its open only after release has started and its first reader open is over
	time.Sleep(300 * time.Millisecond)
	close(opening)
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		// unblock the stuck goroutine so the package can end
		if r, err := os.OpenFile(batch, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			defer r.Close()
		}
		t.Fatal("release hung: the writer's open began after release had closed its reader")
	}
}
