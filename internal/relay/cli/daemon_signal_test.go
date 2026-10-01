package cli

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

// signalHelperEnv makes this test binary the helper process of
// TestExecuteAsLeavesNoInterruptCatchBehindASupervisedWorker in place of its tests: the helper is a
// process of its own because what it observes, SIGINT's disposition, belongs to the whole process.
const signalHelperEnv = "CRW_CLI_SIGNAL_HELPER"

// The helper's exit statuses that say what happened to the SIGINT it sent itself.
const (
	helperSurvived = 97 // still running after the wait: SIGINT was caught
	helperIgnored  = 98 // SIGINT was ignored from the start: nothing could be observed
)

// The helper runs from init, before TestMain, so no test flag or isolation code of the test binary
// has touched the process's signals; the parent passes the isolated environment it already holds.
func init() {
	if state := os.Getenv(signalHelperEnv); state != "" {
		os.Exit(runSignalHelper(state))
	}
}

// supervisedWorkerLine is the line the service supervisor gives a worker (service.SpawnWorker),
// reduced to what answers at once: the token is not this state directory's, so the run is refused
// with supervised_token_mismatch (exit 2) before it touches a descriptor, and the one the parser
// itself refuses with exit 2 says something else.
func supervisedWorkerLine(state string) []string {
	return []string{"--state", state, "--socket", "/absent", "daemon", "--max-ticks", "0", "--allow-isolated-scope",
		"--supervised-token", "test-run", "--supervised-lock-fd", "3", "--supervised-scope-fd", "4"}
}

// runSignalHelper runs the supervised worker's line through ExecuteAs, in this process, and then
// interrupts the process: it ends by SIGINT's default disposition, or it lives to say so.
func runSignalHelper(state string) int {
	if signal.Ignored(os.Interrupt) {
		return helperIgnored
	}
	var stdout, stderr bytes.Buffer
	code := ExecuteAs(context.Background(), "codex-session-relay", supervisedWorkerLine(state), &stdout, &stderr)
	fmt.Printf("exit=%d\n%s%s", code, stdout.String(), stderr.String())
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		fmt.Println("kill:", err)
		return 1
	}
	time.Sleep(2 * time.Second)
	return helperSurvived
}

// The supervised worker's catch of SIGINT is the process's to hold until it exits (decision 42:
// one interrupt can reach a worker twice, and the second would otherwise end it before its exit is
// recorded). The process is cmd/crw's main, so ExecuteAs, which tests call in their own process,
// registers none: what it ran leaves the process's SIGINT as it found it.
func TestExecuteAsLeavesNoInterruptCatchBehindASupervisedWorker(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), signalHelperEnv+"="+filepath.Join(t.TempDir(), "state"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("no wait status: %v\n%s", err, output.String())
	}
	if status.ExitStatus() == helperIgnored {
		t.Fatal("SIGINT is ignored in this environment (an inherited disposition), so the test cannot tell a caught SIGINT from a restored one: run it with SIGINT not ignored")
	}
	// The worker path ran: the line answered its own refusal, not a parser's.
	if !strings.Contains(output.String(), "exit=2\n") || !strings.Contains(output.String(), "supervised_token_mismatch") {
		t.Fatalf("the helper did not run the supervised worker's line to its refusal:\n%s", output.String())
	}
	if !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Fatalf("after the worker path returned, the process's SIGINT was still caught (exit %d); ExecuteAs must leave its disposition as it found it\n%s", status.ExitStatus(), output.String())
	}
}

// SupervisedWorker answers for a line as the dispatcher would read it, for every form the service
// supervisor gives a worker (service.SpawnWorker: an optional --socket after --state, a duration or an
// instant as its bound, a scope descriptor or -1, and --allow-isolated-scope) and for the lines near it
// that are not a worker's. cmd/crw's main registers the worker's SIGINT catch on this answer, and a
// worker that did not get it would be ended by the second copy of an interrupt.
func TestSupervisedWorkerReadsALineAsTheDispatcherDoes(t *testing.T) {
	for _, tc := range []struct {
		name string
		line []string
		want bool
	}{
		{"supervisor's line with a socket and a duration", []string{"--state", "/s", "--socket", "/k", "daemon", "--deadline", "3600", "--supervised-token", "t", "--supervised-lock-fd", "3", "--supervised-scope-fd", "4"}, true},
		{"supervisor's line with an instant, no socket, no scope, isolated", []string{"--state", "/s", "daemon", "--deadline-monotonic", "12.5", "--supervised-token", "t", "--supervised-lock-fd", "3", "--supervised-scope-fd", "-1", "--allow-isolated-scope"}, true},
		{"the descriptor given with =", []string{"daemon", "--supervised-lock-fd=3"}, true},
		{"the descriptor given before other options", []string{"--state", "/s", "daemon", "--supervised-lock-fd", "3", "--max-ticks", "0"}, true},
		{"daemon without the lock descriptor", []string{"--state", "/s", "daemon", "--supervised-token", "t", "--max-ticks", "0"}, false},
		{"daemon with a bound only", []string{"daemon", "--deadline", "60"}, false},
		{"service run is the supervisor, not its worker", []string{"service", "run", "--allow-isolated-scope"}, false},
		{"another command given the option", []string{"status", "--supervised-lock-fd", "3"}, false},
		{"daemon help", []string{"daemon", "--supervised-lock-fd", "3", "--help"}, false},
		{"a descriptor that is no integer", []string{"daemon", "--supervised-lock-fd", "three"}, false},
		{"a root option the parser does not know", []string{"--no-such-option", "daemon", "--supervised-lock-fd", "3"}, false},
		{"root help", []string{"--help"}, false},
		{"root options only", []string{"--state", "/s"}, false},
		{"an empty line", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SupervisedWorker(tc.line); got != tc.want {
				t.Fatalf("SupervisedWorker(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}
