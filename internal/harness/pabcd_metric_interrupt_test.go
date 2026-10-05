package harness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
