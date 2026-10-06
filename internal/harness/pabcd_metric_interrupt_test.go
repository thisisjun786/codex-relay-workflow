package harness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-620: an interrupted crw pabcd metric ingest answers the way the oracle's process does. Node
// has no SIGINT handler, so the first interrupt ends it at once and nothing is recorded; the port
// turns that signal into the invocation's context (cmd/crw serve) and must answer Interrupted (130)
// with empty streams and no record, whether the read is still waiting or has just finished.
//
// Linking testsupport keeps CRW_REFUSE_LIVE_STATE=1 for this test binary (decisions.md 46); the
// process cases below run the built crw itself against temporary homes.

// TestMain isolates this test binary's home and relay state, as every package that runs the built
// crw does, and removes the crw testsupport.CRW builds when CRW_TEST_BINARY is not set.
func TestMain(m *testing.M) { testsupport.Main(m) }

// c620BlockingReader holds its first read open until the test releases it, like a terminal whose
// line has not been finished: the row must be answerable while that read is still waiting.
type c620BlockingReader struct {
	started chan struct{}
	release chan struct{}
}

func (r *c620BlockingReader) Read([]byte) (int, error) {
	close(r.started)
	<-r.release
	return 0, io.EOF
}

// c620CancelBeforeEOF cancels the invocation as its read ends: the row then sees a finished read
// and an interrupt together, the moment the second check exists for.
type c620CancelBeforeEOF struct {
	lines  string
	cancel context.CancelFunc
	sent   bool
}

func (r *c620CancelBeforeEOF) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.lines), nil
	}
	r.cancel()
	return 0, io.EOF
}

// c620MetricRecord is the record file the metric rows write under the workspace's .crw.
func c620MetricRecord(root string) string { return filepath.Join(root, ".crw", "metrics.jsonl") }

// c620HomeListings lists the home state roots the child could reach, so a run that wrote into a
// live-home path changes this string.
func c620HomeListings(t *testing.T, home string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{".codex", ".crw", "codex", "crw"} {
		entries, err := os.ReadDir(filepath.Join(home, name))
		if errors.Is(err, os.ErrNotExist) {
			b.WriteString(name + ": absent\n")
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		b.WriteString(name + ": " + strings.Join(names, " ") + "\n")
	}
	return b.String()
}

// TestPabcdMetricIngestStopsOnContextEndWhileWaiting pins the answer while the ingest read is
// still waiting for its input: the run ends with Interrupted (130) and touches neither stream nor
// the record file. The oracle's process dies on that signal and records nothing; without the fix
// the row stays in the read and records the lines once stdin closes.
func TestPabcdMetricIngestStopsOnContextEndWhileWaiting(t *testing.T) {
	root := pabcdCLITestHome(t)
	reader := &c620BlockingReader{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(reader.release) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- PabcdContext(ctx, []string{"metric", "ingest", "--session", "s1"}, reader, &stdout, &stderr, Verbs())
	}()
	select {
	case <-reader.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the ingest read did not start within 5 s")
	}
	cancel()
	select {
	case code := <-done:
		if code != Interrupted || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("waiting ingest under an ended context: code %d, stdout %q, stderr %q; want %d with nothing written", code, stdout.String(), stderr.String(), Interrupted)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the ingest row was still waiting for stdin 5 s after its context ended")
	}
	if _, err := os.Stat(c620MetricRecord(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the interrupted ingest wrote a record: %v", err)
	}
}

// TestPabcdMetricIngestChecksTheContextWhenTheReadHasEnded pins the second check: the read can
// finish in the moment the interrupt arrives, and the row must answer the same way instead of
// recording the lines it has already read.
func TestPabcdMetricIngestChecksTheContextWhenTheReadHasEnded(t *testing.T) {
	root := pabcdCLITestHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &c620CancelBeforeEOF{lines: "METRIC end=7\n", cancel: cancel}
	var stdout, stderr bytes.Buffer
	code := PabcdContext(ctx, []string{"metric", "ingest", "--session", "s1"}, reader, &stdout, &stderr, Verbs())
	if code != Interrupted || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("ingest whose read ended with the interrupt: code %d, stdout %q, stderr %q; want %d with nothing written", code, stdout.String(), stderr.String(), Interrupted)
	}
	if _, err := os.Stat(c620MetricRecord(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the interrupted ingest recorded the lines that had arrived: %v", err)
	}
}

// TestPabcdMetricRowsUnchangedWithoutAnInterrupt keeps the rest of the row as it was: an
// uninterrupted ingest still records and answers, and a row that is not the ingest read is not
// held by the context at all.
func TestPabcdMetricRowsUnchangedWithoutAnInterrupt(t *testing.T) {
	root := pabcdCLITestHome(t)
	code, out, errOut := pabcdCLITestRun([]string{"metric", "ingest", "--session", "s1"}, "METRIC end=7\n")
	if code != 0 || out != "metric ingest: recorded 1 METRIC line(s)\n" || errOut != "" {
		t.Fatalf("uninterrupted ingest: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	body, err := os.ReadFile(c620MetricRecord(root))
	if err != nil || !strings.Contains(string(body), `"metricName":"end"`) {
		t.Fatalf("uninterrupted ingest did not record: %v %q", err, string(body))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := PabcdContext(ctx, []string{"metric", "--help"}, strings.NewReader(""), &stdout, &stderr, Verbs()); code != 0 || stdout.String() != cli.RenderMetricHelp()+"\n" || stderr.Len() != 0 {
		t.Fatalf("metric --help under an ended context: code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

// TestPabcdMetricIngestEndsOnTheFirstInterrupt runs the built crw as a terminal does: the ingest
// waits on an input that never ends, and the process gets the first SIGINT, which cmd/crw serve
// turns into the invocation's context. The oracle's Node process ends at once on that signal, so
// the port must end with 130, write nothing and record nothing - with a line already in the pipe
// too. The settle before the signal lets the process reach the blocked read, so the interrupt
// lands where the case is about; a run ended by the signal itself reports exit -1 and fails the
// exit-status check below.
func TestPabcdMetricIngestEndsOnTheFirstInterrupt(t *testing.T) {
	crw := testsupport.CRW(t)
	for _, tc := range []struct {
		name  string
		write string
	}{
		{"no line written", ""},
		{"one METRIC line written", "METRIC end=7\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, "work")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			before := c620HomeListings(t, home)
			stdinRead, stdinWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdinRead.Close()
			defer stdinWrite.Close()
			cmd := exec.Command(crw, "pabcd", "metric", "ingest", "--session", "s1")
			cmd.Dir = root
			cmd.Env = []string{
				"HOME=" + home,
				"CODEX_HOME=" + filepath.Join(home, "codex"),
				"CRW_HOME=" + filepath.Join(home, "crw"),
				"PATH=" + os.Getenv("PATH"),
				testsupport.RefuseLiveStateEnv + "=1",
			}
			cmd.Stdin = stdinRead
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
			})
			if tc.write != "" {
				if _, err := io.WriteString(stdinWrite, tc.write); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(time.Second)
			if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
				waited = true
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
				waited = true
				t.Fatalf("the ingest still ran 5 s after the SIGINT\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			}
			if code := cmd.ProcessState.ExitCode(); code != Interrupted {
				t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, Interrupted, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("the interrupted ingest wrote to its streams\nstdout:\n%q\nstderr:\n%q", stdout.String(), stderr.String())
			}
			if _, err := os.Stat(c620MetricRecord(root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the interrupted ingest wrote a record: %v", err)
			}
			if after := c620HomeListings(t, home); after != before {
				t.Fatalf("the run changed the child's home listings\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// CRW-627: the first interrupt also ends the record window. CRW-620 covered the stdin read; once the ingest records, the oracle's
// process still dies at the signal (it has no lock and no handler) and keeps the lines it had appended, while the port waited for the
// ledger lock without a deadline and then wrote every remaining line. These cases hold that lock from the test and end the run by the
// invocation's context.

// c627HoldLedger creates the workspace's ledger and locks it exclusively through a descriptor of this test, opened without O_APPEND
// so that an append descriptor on the ledger can only be the ingest's (it is close-on-exec, so a child does not inherit it). The
// cleanup releases the lock; the returned release does too.
func c627HoldLedger(t *testing.T, root string) (release func()) {
	t.Helper()
	ledger := c620MetricRecord(root)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(ledger, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { f.Close() }) } // closing drops the lock
	t.Cleanup(release)
	return release
}

// c627AppendOpen reports whether process pid holds a descriptor on path opened with O_APPEND, as /proc shows it (the flags are octal
// and carry other bits too). The ingest opens the ledger that way only in appendRow, just before it asks for the lock.
func c627AppendOpen(pid int, path string) bool {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	fds, err := os.ReadDir(filepath.Join(dir, "fd"))
	if err != nil {
		return false
	}
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join(dir, "fd", fd.Name())); err != nil || target != path {
			continue
		}
		info, err := os.ReadFile(filepath.Join(dir, "fdinfo", fd.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(info), "\n") {
			if flags, ok := strings.CutPrefix(line, "flags:"); ok {
				if n, err := strconv.ParseUint(strings.TrimSpace(flags), 8, 64); err == nil && n&unix.O_APPEND != 0 {
					return true
				}
			}
		}
	}
	return false
}

// TestPabcdMetricIngestEndsOnTheFirstInterruptWhileTheLedgerIsLocked runs the built crw while another writer holds the ledger lock and
// sends the first SIGINT once the child has opened the ledger for its append, which is where it waits. The oracle's process would die
// at the signal; the run must end with 130, print nothing and leave the ledger as the holder had it. Before the fix the wait ignored
// the signal, and nothing after the stdin check looked at the context, so a signal sent from that state could not end with 130.
func TestPabcdMetricIngestEndsOnTheFirstInterruptWhileTheLedgerIsLocked(t *testing.T) {
	if _, err := os.Stat("/proc/self/fdinfo"); err != nil {
		t.Skip("the case reads /proc to see the child reach the ledger lock")
	}
	crw := testsupport.CRW(t)
	home := t.TempDir()
	root := filepath.Join(home, "work")
	c627HoldLedger(t, root)
	ledger, err := filepath.EvalSymlinks(c620MetricRecord(root))
	if err != nil {
		t.Fatal(err)
	}
	before := c620HomeListings(t, home)
	cmd := exec.Command(crw, "pabcd", "metric", "ingest", "--session", "s1")
	cmd.Dir = root
	cmd.Env = []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, "codex"),
		"CRW_HOME=" + filepath.Join(home, "crw"),
		"PATH=" + os.Getenv("PATH"),
		testsupport.RefuseLiveStateEnv + "=1",
	}
	cmd.Stdin = strings.NewReader("METRIC a=1\nMETRIC b=2\n") // copied through a pipe that closes after the second line
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
	for deadline := time.Now().Add(5 * time.Second); !c627AppendOpen(cmd.Process.Pid, ledger); time.Sleep(10 * time.Millisecond) {
		select {
		case <-done:
			ended = true
			t.Fatalf("the ingest ended before it reached the ledger lock\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the ingest did not reach the ledger lock within 5 s")
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
		t.Fatalf("the ingest still ran 5 s after the SIGINT\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if code := cmd.ProcessState.ExitCode(); code != Interrupted {
		t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, Interrupted, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("the interrupted ingest wrote to its streams\nstdout:\n%q\nstderr:\n%q", stdout.String(), stderr.String())
	}
	if raw, err := os.ReadFile(c620MetricRecord(root)); err != nil || len(raw) != 0 {
		t.Fatalf("the interrupted ingest changed the ledger the holder had: %v %q", err, raw)
	}
	if after := c620HomeListings(t, home); after != before {
		t.Fatalf("the run changed the child's home listings\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRunMetricCLIContextEndsWithTheContextWhileTheLedgerIsLocked pins the error the library gives the verb: it is the context's own,
// so errors.Is recognises it. It is an identity and propagation check, not evidence of where the wait was: the settle only gives the
// call time to reach the lock, and cancelling earlier gives the same error. A build that ignores ctx blocks and fails the bound.
func TestRunMetricCLIContextEndsWithTheContextWhileTheLedgerIsLocked(t *testing.T) {
	root := t.TempDir()
	release := c627HoldLedger(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cli.RunMetricCLIContext(ctx, []string{"ingest", "--session", "s1"}, root, "METRIC a=1\nMETRIC b=2\n")
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ingest cancelled in the lock wait returned %v, want an error that is context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		release()
		<-done
		t.Fatal("the ingest was still waiting for the ledger lock 5 s after its context ended")
	}
	if raw, err := os.ReadFile(c620MetricRecord(root)); err != nil || len(raw) != 0 {
		t.Fatalf("the cancelled ingest changed the ledger the holder had: %v %q", err, raw)
	}
}

// c627CancelAfterRows is a context that cancels itself the first time its Err is asked for once the ledger holds two rows: the
// interrupt lands after the last row was recorded. Done is the wrapped context's, so Err never contradicts it.
type c627CancelAfterRows struct {
	context.Context
	cancel context.CancelFunc
	ledger string
}

func (c c627CancelAfterRows) Err() error {
	if raw, _ := os.ReadFile(c.ledger); strings.Count(string(raw), "\n") >= 2 {
		c.cancel()
	}
	return c.Context.Err()
}

// TestPabcdMetricIngestAnswersInterruptedWhenTheContextEndsAfterRecording pins the check after the run: an interrupt that lands once
// every row is recorded still answers 130 with nothing printed, and the rows recorded before it stay. The lock is free here, so this
// covers the post-run check, not a lock wait.
func TestPabcdMetricIngestAnswersInterruptedWhenTheContextEndsAfterRecording(t *testing.T) {
	root := pabcdCLITestHome(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := c627CancelAfterRows{Context: base, cancel: cancel, ledger: c620MetricRecord(root)}
	var stdout, stderr bytes.Buffer
	code := PabcdContext(ctx, []string{"metric", "ingest", "--session", "s1"}, strings.NewReader("METRIC a=1\nMETRIC b=2\n"), &stdout, &stderr, Verbs())
	if code != Interrupted || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("ingest whose context ended after the rows were recorded: code %d, stdout %q, stderr %q; want %d with nothing written", code, stdout.String(), stderr.String(), Interrupted)
	}
	if raw, err := os.ReadFile(c620MetricRecord(root)); err != nil || strings.Count(string(raw), "\n") != 2 {
		t.Fatalf("the rows recorded before the interrupt did not stay: %v %q", err, raw)
	}
}
