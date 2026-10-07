package manage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// The two contract points the merged review of PR #780 raised against crw manage supervisor: a
// host-value option is refused before -h/--help is honored, and a failed write of the help output
// is reported rather than swallowed. Every name here carries the supervisorReview780 prefix so it
// cannot collide with a sibling issue's file in this package.

// supervisorReview780RunWithFailStdout runs one supervisor command line through the registry with a
// stdout that refuses every write, so a run whose usage never left the writer is distinguishable
// from one that wrote it.
func supervisorReview780RunWithFailStdout(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var errOut strings.Builder
	code := Run(context.Background(), args, strings.NewReader(""), coreFailWriter{}, &errOut)
	return code, errOut.String()
}

// C1: a host-value option is refused before -h/--help is honored. The refusal is the one the
// command already gives without help (exit 2 and the host-value note), and no relay call is made,
// so a command line that tries to override a host value cannot end as a successful help request.
func TestSupervisorReview780RemovedOptionBeatsHelp(t *testing.T) {
	for _, args := range [][]string{
		{"register", "--cwd=/override", "--help"},
		{"register", "--settings-file=/x", "-h"},
		{"register", "--cwd", "/override", "--help"},
		{"register", "--settings-file", "/x", "-h"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			record := supervisorFakeProcess(t, 0)
			section := supervisorTestSection()
			supervisorUseConfig(t, supervisorTestConfig(&section))
			code, _, errOut := supervisorRunLine(t, append([]string{"supervisor"}, args...)...)
			if code != usageExit {
				t.Fatalf("exit %d, want %d (stderr %q)", code, usageExit, errOut)
			}
			if !strings.Contains(errOut, supervisorHostValuesMessage) {
				t.Errorf("stderr %q does not name %q", errOut, supervisorHostValuesMessage)
			}
			if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
				t.Errorf("the relay was called %q", calls)
			}
		})
	}
}

// C2: a failed write of the help output is reported on stderr and the run ends with exit 1. The
// usage never reached the caller, so a help request that wrote nothing must not read as success.
func TestSupervisorReview780HelpWriteFailureExitsOne(t *testing.T) {
	for _, args := range [][]string{
		{"supervisor", "register", "--help"},
		{"supervisor", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, errOut := supervisorReview780RunWithFailStdout(t, args...)
			if code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, errOut)
			}
			if !strings.Contains(errOut, "crw manage supervisor: error: write the usage:") {
				t.Errorf("stderr %q does not carry the usage write failure", errOut)
			}
		})
	}
}

// The contrast: a plain --help with no host-value option is a successful help request (exit 0) that
// prints the usage on stdout and calls no relay. Without it the two rules above could be satisfied
// by refusing every help request.
func TestSupervisorReview780PlainHelpStaysZero(t *testing.T) {
	for _, args := range [][]string{
		{"supervisor", "--help"},
		{"supervisor", "register", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			record := supervisorFakeProcess(t, 0)
			section := supervisorTestSection()
			supervisorUseConfig(t, supervisorTestConfig(&section))
			code, out, errOut := supervisorRunLine(t, args...)
			if code != 0 {
				t.Fatalf("exit %d, want 0 (stderr %q)", code, errOut)
			}
			if !strings.Contains(out, "usage: crw manage supervisor") {
				t.Errorf("stdout %q does not carry the usage", out)
			}
			if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
				t.Errorf("a help request called the relay %q", calls)
			}
		})
	}
}

// supervisorReview780HelpPipeEnv names the child mode of the re-executed test binary. It carries the
// command line the child must run against its own stdout, one argument per line. A newline is the
// separator because an environment variable cannot hold the NUL byte that would be natural here,
// and every argument this test passes is a fixed literal with no newline in it.
const supervisorReview780HelpPipeEnv = "CRW_MANAGE_TEST_HELP_PIPE"

// supervisorReview780ReportPipeEnv names the child mode that writes a JSON report to the process's
// own stdout, so the report path reaches the real fd 1 the same way the usage path does.
const supervisorReview780ReportPipeEnv = "CRW_MANAGE_TEST_REPORT_PIPE"

// supervisorReview780PassThroughPipeEnv names the child mode that writes a relay answer through
// supervisorPassThrough, the third stdout path this command has.
const supervisorReview780PassThroughPipeEnv = "CRW_MANAGE_TEST_PASSTHROUGH_PIPE"

// TestSupervisorReview780OutputPipeChild is not a test of its own. The parent tests re-execute this
// binary with one of the two env names below (the way core_testhelp_test.go starts the fake crw),
// and this function then writes to the process's own stdout and stderr, so the write reaches the
// real fd 1 and fd 2 instead of a fake writer.
func TestSupervisorReview780OutputPipeChild(t *testing.T) {
	spec := os.Getenv(supervisorReview780HelpPipeEnv)
	if spec != "" {
		os.Exit(Run(context.Background(), strings.Split(spec, "\n"), strings.NewReader(""), os.Stdout, os.Stderr))
	}
	if os.Getenv(supervisorReview780ReportPipeEnv) != "" {
		// The report path the issue names alongside the usage: one JSON value on stdout. A write
		// that cannot leave must come back as an error, not as a signal.
		if err := supervisorWrite(os.Stdout, supervisorRecord{OK: true, TaskID: "task-supervisor"}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(supervisorReview780PassThroughPipeEnv) != "" {
		// The relay pass-through, the third stdout path this command writes through: the relay's
		// own answer is written unchanged and a write that fails ends the run with 1 instead of the
		// status it was about to return.
		e := &Env{Stdin: strings.NewReader(""), Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv}
		os.Exit(supervisorPassThrough(e, []byte("{\"error\":\"refused\"}\n"), supervisorExitRefused))
	}
	t.Skip("only runs as the re-executed child of the pipe tests")
}

// supervisorReview780HelpPipeChild runs one supervisor command line in a child process whose
// stdout is a pipe. With the read end closed first, the write reaches a pipe nobody reads, which is
// the case a fake writer cannot produce. The child's TMPDIR points at the parent's temporary
// directory, so the child's own isolation root is removed with it rather than left behind.
func supervisorReview780HelpPipeChild(t *testing.T, args []string, closeReadEnd, stderrOnPipe bool) (code int, stdout, stderr string, signaled bool) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if closeReadEnd {
		// Close the read end before the child starts, so its first write finds no reader: this is
		// the closed pipe the process must survive.
		if err := read.Close(); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorReview780OutputPipeChild$")
	cmd.Env = append(os.Environ(),
		supervisorReview780HelpPipeEnv+"="+strings.Join(args, "\n"),
		"TMPDIR="+t.TempDir())
	cmd.Stdout = write
	var errOut bytes.Buffer
	if stderrOnPipe {
		// The shell's "2>&1": the diagnostic and the usage share one closed pipe, so a note that
		// cannot be delivered must not take the run's exit status with it.
		cmd.Stderr = write
	} else {
		cmd.Stderr = &errOut
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The parent's copy of the write end must go, or the read below never sees EOF.
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	var out []byte
	if !closeReadEnd {
		out, _ = io.ReadAll(read)
		if err := read.Close(); err != nil {
			t.Fatal(err)
		}
	}
	err = cmd.Wait()
	code = cmd.ProcessState.ExitCode()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("the child: %v", err)
		}
	}
	// A process killed by a signal reports exit code -1; that is the failure this test exists to
	// rule out, so it is reported rather than folded into the status.
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		signaled = true
	}
	return code, string(out), errOut.String(), signaled
}

// C2 (parent addition, pre-merge evaluation d1): the real stdout connected to a pipe whose read
// end is closed. os.File.Write turns EPIPE into a fatal SIGPIPE, so a command that only checks the
// error a writer returns ends with a signal status and no note at all. The write must reach the
// kernel through syscall.Write instead, which hands EPIPE back as an ordinary error: the help
// request then ends with exit 1 and the usage write failure on stderr, exactly as it does for a
// writer that refuses.
func TestSupervisorReview780HelpPipeEpipeExitsOne(t *testing.T) {
	for _, args := range [][]string{
		{"supervisor", "--help"},
		{"supervisor", "register", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, errOut, signaled := supervisorReview780HelpPipeChild(t, args, true, false)
			if signaled {
				t.Fatalf("the child died on a signal (exit %d, stderr %q): the EPIPE from a closed stdout pipe must be reported, not raised", code, errOut)
			}
			if code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, errOut)
			}
			if !strings.Contains(errOut, "crw manage supervisor: error: write the usage:") {
				t.Errorf("stderr %q does not carry the usage write failure", errOut)
			}
		})
	}
}

// supervisorReview780ExpectedUsage is the whole output each help request must write, pinned here as
// literals rather than taken from the command's own constants: a comparison against the constants
// would pass even if a line went missing from the writer, which is the regression this test exists
// to catch.
var supervisorReview780ExpectedUsage = map[string]string{
	"supervisor --help": "usage: crw manage supervisor {register,show} ...\n" +
		"  register\tbind the management thread as the store supervisor and record its pair\n" +
		"  show\t\tprint the recorded binding and the recorded settings\n",
	"supervisor register --help": "usage: crw manage supervisor register\n",
}

// The contrast: with the pipe's read end open the same child writes the whole usage and exits 0.
// Without it the rule above could be satisfied by failing every help request, and the exact-output
// comparison is what keeps a truncated usage from passing as a successful help request.
func TestSupervisorReview780HelpPipeOpenPipeStaysZero(t *testing.T) {
	for _, args := range [][]string{
		{"supervisor", "--help"},
		{"supervisor", "register", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, errOut, signaled := supervisorReview780HelpPipeChild(t, args, false, false)
			if signaled {
				t.Fatalf("the child died on a signal (exit %d, stderr %q)", code, errOut)
			}
			if code != 0 {
				t.Fatalf("exit %d, want 0 (stderr %q)", code, errOut)
			}
			if want := supervisorReview780ExpectedUsage[strings.Join(args, " ")]; out != want {
				t.Errorf("stdout = %q, want the whole usage %q", out, want)
			}
		})
	}
}

// C2 (pre-merge evaluation d1): stdout and stderr are the same closed pipe, the shape "cmd 2>&1 |
// head -1" leaves behind. The note cannot be delivered, but that is no reason for the run to die:
// the diagnostic must be attempted without raising SIGPIPE and the promised exit 1 must survive.
func TestSupervisorReview780HelpPipeBothStreamsClosedExitsOne(t *testing.T) {
	for _, args := range [][]string{
		{"supervisor", "--help"},
		{"supervisor", "register", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, _, signaled := supervisorReview780HelpPipeChild(t, args, true, true)
			if signaled {
				t.Fatalf("the child died on a signal (exit %d): the diagnostic write must not raise SIGPIPE", code)
			}
			if code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
		})
	}
}

// supervisorReview780ChildOnClosedPipe re-executes the test binary in one of the named child modes
// with stdout on a pipe whose read end this parent has already closed, so the child's write reaches
// a pipe nobody reads. It returns the child's exit status and whether a signal ended it.
func supervisorReview780ChildOnClosedPipe(t *testing.T, mode string) (code int, signaled bool) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorReview780OutputPipeChild$")
	cmd.Env = append(os.Environ(), mode+"=1", "TMPDIR="+t.TempDir())
	cmd.Stdout = write
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	code = cmd.ProcessState.ExitCode()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("the child: %v", err)
		}
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		signaled = true
	}
	return code, signaled
}

// C2 (the decided answer names the JSON report as well as the usage): a report written to a closed
// pipe is an ordinary error the caller can act on, not a signal that ends the process first. The
// report path is exercised directly rather than through register so the write is the only thing
// under test. The child exits 1 when the write reports an error and 0 when it does not, so the
// parent asserts both that no signal arrived and that the error really came back: a regression that
// swallowed the write error would exit 0 and fail here.
func TestSupervisorReview780ReportPipeIsAnErrorNotASignal(t *testing.T) {
	code, signaled := supervisorReview780ChildOnClosedPipe(t, supervisorReview780ReportPipeEnv)
	if signaled {
		t.Fatalf("the child died on a signal (exit %d): a report written to a closed pipe must return an error, not raise SIGPIPE", code)
	}
	if code != 1 {
		t.Fatalf("exit %d, want 1: the report write must report the closed pipe as an error", code)
	}
}

// C2 (the third stdout path): supervisorPassThrough writes the relay's own answer unchanged, and it
// was routed through the same helper as the usage and the report. A closed pipe there must end the
// run with the write failure's status, not with a signal, and the status it was about to return
// (the relay's own refusal) must not survive a write that never happened.
func TestSupervisorReview780PassThroughPipeIsAnErrorNotASignal(t *testing.T) {
	code, signaled := supervisorReview780ChildOnClosedPipe(t, supervisorReview780PassThroughPipeEnv)
	if signaled {
		t.Fatalf("the child died on a signal (exit %d): the relay pass-through must report a closed pipe as an error", code)
	}
	if code != 1 {
		t.Fatalf("exit %d, want 1: the pass-through write failure must end the run with 1, not with the status it was about to return (%d)", code, supervisorExitRefused)
	}
}
