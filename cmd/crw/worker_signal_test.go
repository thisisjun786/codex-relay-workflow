package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// workerSignalEnv makes this test binary the helper process of the tests below in place of its
// tests. The helper is a process of its own because what these tests observe, SIGINT's
// disposition and how a process ends, belongs to the whole process, and the helper runs from init,
// before TestMain, so nothing of the test binary has touched its signals; the parent passes the
// isolated environment it holds.
const (
	workerSignalEnv      = "CRW_MAIN_SIGNAL_HELPER"
	workerSignalStateEnv = "CRW_MAIN_SIGNAL_STATE"
)

// The helper's exit status when SIGINT was ignored from the start: no case could then tell a caught
// SIGINT from a released one.
const helperIgnored = 98

func init() {
	if mode := os.Getenv(workerSignalEnv); mode != "" {
		os.Exit(runWorkerSignalHelper(mode, os.Getenv(workerSignalStateEnv)))
	}
}

// workerLines are the command lines the helper runs through serve. The worker's is the line the
// service supervisor gives a worker (service.SpawnWorker) reduced to what answers at once: its token
// is not this state directory's, so the run is refused with supervised_token_mismatch (exit 2)
// before it touches a descriptor, which says the worker path ran where the parser's own exit 2
// would not. The same line without the lock and scope descriptors is no worker's, and answers
// supervised_invocation_incomplete (exit 2).
func workerLines(state string) (worker, other []string) {
	other = []string{"--state", state, "--socket", "/absent", "daemon", "--max-ticks", "0", "--allow-isolated-scope", "--supervised-token", "test-run"}
	worker = append(append([]string{}, other...), "--supervised-lock-fd", "3", "--supervised-scope-fd", "4")
	return worker, other
}

// runWorkerSignalHelper runs one line through serve, in this process, and then interrupts the
// process again and again, covering a copy that arrives after the run returned and one that arrives
// after the first has released SIGINT. It ends by SIGINT, or lives to say so and exits with the
// run's own status. The modes starting with "ordering" replace the worker predicate with a function
// that delivers SIGINT while serve decides, to show the interrupt is registered before the command
// line is read: the one ending in "-worker" answers that the line is a worker's, the other that it is
// not.
func runWorkerSignalHelper(mode, state string) int {
	if signal.Ignored(os.Interrupt) {
		return helperIgnored
	}
	worker, other := workerLines(state)
	program, line := "crw", append([]string{"relay"}, worker...)
	switch mode {
	case "relay-worker":
	case "binary-worker":
		program, line = "codex-session-relay", worker
	case "other-line":
		line = append([]string{"relay"}, other...)
	case "ordering-worker", "ordering-other":
		answer := mode == "ordering-worker"
		workerLine = func([]string) bool {
			fmt.Println("seam called")
			if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
				fmt.Println("kill:", err)
				os.Exit(1)
			}
			time.Sleep(100 * time.Millisecond)
			return answer
		}
	default:
		fmt.Println("unknown mode", mode)
		return 1
	}
	var out bytes.Buffer
	code := serve(program, line, &out, &out, time.Now())
	fmt.Printf("exit=%d\n%s", code, out.String())
	for range 20 {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			fmt.Println("kill:", err)
			return 1
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Println("survived")
	return code
}

// runWorkerSignalProcess runs the helper in one mode and returns what it printed and how it ended.
func runWorkerSignalProcess(t *testing.T, mode string) (string, syscall.WaitStatus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), workerSignalEnv+"="+mode, workerSignalStateEnv+"="+filepath.Join(t.TempDir(), "state"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("%s: no wait status: %v\n%s", mode, err, output.String())
	}
	if status.Exited() && status.ExitStatus() == helperIgnored {
		t.Fatalf("%s: SIGINT is ignored in this environment (an inherited disposition), so the test cannot tell a caught SIGINT from a released one: run it with SIGINT not ignored", mode)
	}
	return output.String(), status
}

func wantOutput(t *testing.T, mode, output string, want ...string) {
	t.Helper()
	for _, text := range want {
		if !strings.Contains(output, text) {
			t.Fatalf("%s: the helper's output lacks %q:\n%s", mode, text, output)
		}
	}
}

// A supervised worker is interrupted twice for one request (decision 42), and the copy can arrive
// after the run has returned and before the process exits: it must not end the worker before its exit
// status is reported, which is the status the supervisor records. The worker's line, under either
// program name, keeps SIGINT caught through every copy and exits with its own status; every other
// line gets SIGINT's default disposition back once the first has been honoured, so a second copy ends it.
func TestSupervisedWorkerKeepsInterruptsCaughtUntilItsProcessExits(t *testing.T) {
	for _, mode := range []string{"relay-worker", "binary-worker"} {
		t.Run(mode, func(t *testing.T) {
			output, status := runWorkerSignalProcess(t, mode)
			wantOutput(t, mode, output, "exit=2\n", "supervised_token_mismatch", "survived\n")
			if !status.Exited() || status.ExitStatus() != 2 {
				t.Fatalf("the worker did not end with its own exit status 2 (signaled %v by %v, exit %d):\n%s", status.Signaled(), status.Signal(), status.ExitStatus(), output)
			}
		})
	}
	t.Run("other-line", func(t *testing.T) {
		output, status := runWorkerSignalProcess(t, "other-line")
		wantOutput(t, "other-line", output, "exit=2\n", "supervised_invocation_incomplete")
		if !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Fatalf("a line that is no worker's kept SIGINT caught after its run returned (exit %d):\n%s", status.ExitStatus(), output)
		}
	})
}

// The interrupt is registered before the command line is read, as it always was: an interrupt that
// arrives while the line is being read, here delivered by the replaced predicate, is caught for a
// worker's line (and the worker then survives every later copy) and for any other line (which then has
// SIGINT's default disposition back, so the next copy ends it).
func TestTheInterruptIsRegisteredBeforeTheCommandLineIsRead(t *testing.T) {
	t.Run("ordering-worker", func(t *testing.T) {
		output, status := runWorkerSignalProcess(t, "ordering-worker")
		wantOutput(t, "ordering-worker", output, "seam called\n")
		if !status.Exited() {
			t.Fatalf("the process was ended by SIGINT (signal %v), by the copy delivered while the line was read or by a later one:\n%s", status.Signal(), output)
		}
		wantOutput(t, "ordering-worker", output, "survived\n")
	})
	t.Run("ordering-other", func(t *testing.T) {
		output, status := runWorkerSignalProcess(t, "ordering-other")
		wantOutput(t, "ordering-other", output, "seam called\n")
		if !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Fatalf("after an interrupt delivered while the line was read, the release did not happen: a later copy did not end the process (exit %d):\n%s", status.ExitStatus(), output)
		}
	})
}
