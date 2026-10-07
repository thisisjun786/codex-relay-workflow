package harness

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// c6Run drives the dispatcher with an arbitrary stdin reader; pabcdCLITestRun takes a string only.
func c6Run(args []string, in io.Reader) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Pabcd(args, in, &out, &errOut, Verbs())
	return code, out.String(), errOut.String()
}

// c6FillReader yields filler bytes lazily, so a limit-sized input exists only as a stream.
type c6FillReader struct{ left int64 }

func (r *c6FillReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.left {
		n = r.left
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'x'
	}
	r.left -= n
	return int(n), nil
}

// c6CountReader is c6FillReader plus a byte counter, to pin how much the adapter consumes.
type c6CountReader struct {
	left int64
	read int64
}

func (r *c6CountReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.left {
		n = r.left
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'x'
	}
	r.left -= n
	r.read += n
	return int(n), nil
}

// c6ProbeReader records whether anything read it at all.
type c6ProbeReader struct{ reads int }

func (r *c6ProbeReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

// c6FailReader delivers one METRIC line and then a non-EOF read error.
type c6FailReader struct{ sent bool }

func (r *c6FailReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "METRIC a=1\n"), nil
	}
	return 0, io.ErrUnexpectedEOF
}

const c6VerbList = "freeze,plan,receipt,evidence,memory,reset,config,scan,review-round,metric,divergence,loop,orchestrate"

// TestPabcdC6Verbs drives the three verb rows this issue adds through the dispatcher: the
// argv/stdin/stream/exit mapping of cli.ts's metric, divergence and review-round branches
// over the CRW-544/545/546 libraries. Red first: with no rows the dispatcher answers
// "invalid choice" with exit 2 for every case.
func TestPabcdC6Verbs(t *testing.T) {
	t.Run("arguments_and_streams", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		reviewRoundHelp := cli.RenderReviewRoundHelp() + "\n"
		metricHelp := cli.RenderMetricHelp() + "\n"
		divergenceHelp := cli.RenderDivergenceHelp() + "\n"
		metricUnknown, _ := cli.RunMetricCLI([]string{"frob", "--session", "s1"}, root, "")
		divergenceUnknown, _ := cli.RunDivergenceCli([]string{"frob", "--session", "s1"}, root)
		for _, tc := range []struct {
			name        string
			args        []string
			code        int
			out, errOut string
		}{
			{"dispatcher_help", []string{"--help"}, 0, "usage: crw pabcd [-h] {" + c6VerbList + "} ...\n", ""},
			{"review_round_bare", []string{"review-round"}, 0, reviewRoundHelp, ""},
			{"review_round_word_help", []string{"review-round", "help"}, 0, reviewRoundHelp, ""},
			{"review_round_dashdash_help", []string{"review-round", "--help"}, 0, reviewRoundHelp, ""},
			{"review_round_upper_help", []string{"review-round", "HELP"}, 0, reviewRoundHelp, ""},
			{"metric_bare", []string{"metric"}, 0, metricHelp, ""},
			{"metric_word_help", []string{"metric", "help"}, 0, metricHelp, ""},
			{"metric_dashdash_help", []string{"metric", "--help"}, 0, metricHelp, ""},
			{"metric_h", []string{"metric", "-h"}, 0, metricHelp, ""},
			{"divergence_bare", []string{"divergence"}, 0, divergenceHelp, ""},
			{"divergence_word_help", []string{"divergence", "help"}, 0, divergenceHelp, ""},
			{"divergence_dashdash_help", []string{"divergence", "--help"}, 0, divergenceHelp, ""},
			{"divergence_h", []string{"divergence", "-h"}, 0, divergenceHelp, ""},
			{"review_round_unknown_subcommand", []string{"review-round", "frob"}, 1, "", "review-round: unknown review-round verb 'frob' (expected open|show|abort)\n"},
			{"review_round_open_dashdash_help_refused", []string{"review-round", "open", "--help"}, 1, "review-round: --session <id> is required\n", ""},
			{"metric_record_dashdash_help_refused", []string{"metric", "record", "--help"}, 1, "metric: --session <id> is required\n", ""},
			{"divergence_candidate_dashdash_help_refused", []string{"divergence", "candidate", "--help"}, 1, "divergence: --session <id> is required\n", ""},
			{"review_round_show_unbound_session", []string{"review-round", "show", "--session", "s1"}, 1, "review-round show: this session has no bound goalplan\n", ""},
			{"metric_unknown_topic_prints_library_usage", []string{"metric", "frob", "--session", "s1"}, metricUnknown.Code, metricUnknown.Output + "\n", ""},
			{"divergence_unknown_topic_prints_library_usage", []string{"divergence", "frob", "--session", "s1"}, divergenceUnknown.Code, divergenceUnknown.Output + "\n", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				code, out, errOut := c6Run(tc.args, strings.NewReader(""))
				if code != tc.code || out != tc.out || errOut != tc.errOut {
					t.Fatalf("got %d %q %q; want %d %q %q", code, out, errOut, tc.code, tc.out, tc.errOut)
				}
			})
		}
	})

	t.Run("metric_ingest_reads_stdin_first", func(t *testing.T) {
		pabcdCLITestHome(t)
		code, out, errOut := c6Run([]string{"metric", "ingest", "--help"}, &c6FillReader{left: MaxStdinBytes + 1})
		if code != 1 || out != "" || errOut != "metric: stdin exceeds 4194304 bytes\n" {
			t.Fatalf("ingest --help with oversized stdin: %d %q %q", code, out, errOut)
		}
		probe := &c6ProbeReader{}
		code, out, errOut = c6Run([]string{"metric", "ingest"}, probe)
		if probe.reads == 0 {
			t.Fatal("session-less metric ingest did not read stdin")
		}
		if code != 1 || out != "metric: --session <id> is required\n" || errOut != "" {
			t.Fatalf("session-less ingest: %d %q %q", code, out, errOut)
		}
	})

	t.Run("metric_ingest_bounded_stdin", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		line := "METRIC end=7\n"
		in := io.MultiReader(&c6FillReader{left: int64(MaxStdinBytes - len(line) - 1)}, strings.NewReader("\n"+line))
		code, out, errOut := c6Run([]string{"metric", "ingest", "--session", "s1", "--json"}, in)
		if code != 0 || errOut != "" || !strings.HasPrefix(out, "{\"records\":[") || !strings.Contains(out, "\"metricName\":\"end\"") || !strings.Contains(out, "\"value\":7") {
			t.Fatalf("limit-sized ingest: %d %q %q", code, out, errOut)
		}
		body, err := os.ReadFile(filepath.Join(root, ".crw", "metrics.jsonl"))
		if err != nil || !strings.Contains(string(body), "\"metricName\":\"end\"") {
			t.Fatalf("limit-sized record not stored: %v %q", err, string(body))
		}
		overRoot := pabcdCLITestHome(t)
		leading := "METRIC start=1\n"
		over := io.MultiReader(strings.NewReader(leading), &c6FillReader{left: int64(MaxStdinBytes + 1 - len(leading))})
		code, out, errOut = c6Run([]string{"metric", "ingest", "--session", "s1"}, over)
		if code != 1 || out != "" || errOut != "metric: stdin exceeds 4194304 bytes\n" {
			t.Fatalf("oversized ingest: %d %q %q", code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(overRoot, ".crw", "metrics.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("oversized ingest wrote a partial record: %v", err)
		}
		big := &c6CountReader{left: 2 * int64(MaxStdinBytes)}
		code, out, errOut = c6Run([]string{"metric", "ingest", "--session", "s1"}, big)
		if code != 1 || out != "" || errOut != "metric: stdin exceeds 4194304 bytes\n" || big.read > MaxStdinBytes+1 {
			t.Fatalf("far-over ingest consumed %d bytes: %d %q %q", big.read, code, out, errOut)
		}
		failRoot := pabcdCLITestHome(t)
		code, out, errOut = c6Run([]string{"metric", "ingest", "--session", "s1"}, &c6FailReader{})
		if code != 0 || out != "metric ingest: recorded 0 METRIC line(s)\n" || errOut != "" {
			t.Fatalf("read-error ingest: %d %q %q", code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(failRoot, ".crw", "metrics.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("read-error ingest stored a record: %v", err)
		}
	})

	t.Run("no_other_verb_reads_stdin", func(t *testing.T) {
		pabcdCLITestHome(t)
		for _, args := range [][]string{
			{"metric", "record", "--session", "s1", "--name", "x", "--value", "1"},
			{"metric", "show", "--session", "s1"},
			{"metric", "kind", "--session", "s1"},
			{"metric", "parse-line", "--session", "s1"},
			{"metric", "help"},
			{"metric", "--help"},
			{"metric", "INGEST", "--session", "s1"},
			{"metric", "--help", "ingest"},
			{"metric", "record", "ingest", "--session", "s1"},
			{"divergence", "mode", "on", "--session", "s1", "--collapse", "D", "--reason", "x"},
			{"divergence", "candidate", "list", "--session", "s1"},
			{"divergence", "help"},
			{"divergence", "--help"},
			{"review-round", "show", "--session", "s1"},
			{"review-round", "open", "--session", "s1"},
			{"review-round", "help"},
			{"review-round", "--help"},
		} {
			probe := &c6ProbeReader{}
			c6Run(args, probe)
			if probe.reads != 0 {
				t.Errorf("%v read stdin %d time(s)", args, probe.reads)
			}
		}
	})

	t.Run("cwd_override", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		other := filepath.Join(root, "other")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := c6Run([]string{"divergence", "mode", "on", "--session", "s1", "--collapse", "D", "--reason", "plateau", "--cwd", other, "--json"}, strings.NewReader(""))
		if code != 0 || errOut != "" || !strings.Contains(out, "\"active\":true") {
			t.Fatalf("mode on --cwd: %d %q %q", code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(other, ".crw", "divergence", "s1.mode.json")); err != nil {
			t.Fatalf("mode file not under the overridden cwd: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, ".crw", "divergence", "s1.mode.json")); !os.IsNotExist(err) {
			t.Fatalf("mode file landed under the real cwd: %v", err)
		}
		st := state.DefaultState("s1", "")
		st.Phase = state.PhaseA
		if err := state.WriteState(other, st); err != nil {
			t.Fatal(err)
		}
		code, out, errOut = c6Run([]string{"review-round", "open", "--session", "s1", "--cwd", other}, strings.NewReader(""))
		if code != 1 || out != "review-round open: this session has no bound goalplan\n" || errOut != "" {
			t.Fatalf("open --cwd other: %d %q %q", code, out, errOut)
		}
		code, out, errOut = c6Run([]string{"review-round", "open", "--session", "s1"}, strings.NewReader(""))
		if code != 1 || out != "review-round open: session is at IDLE, not A — a plan audit is opened during Audit\n" || errOut != "" {
			t.Fatalf("open without --cwd: %d %q %q", code, out, errOut)
		}
	})

	t.Run("deleted_cwd", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		missing := filepath.Join(root, "removed")
		if err := os.Mkdir(missing, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(missing)
		if err := os.Remove(missing); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"review-round", "--help"}, {"metric", "--help"}, {"divergence", "--help"}} {
			code, out, errOut := c6Run(args, strings.NewReader(""))
			if code != 1 || out != "" || !strings.HasPrefix(errOut, "crw cli failed: ") || !strings.HasSuffix(errOut, "\n") {
				t.Fatalf("%v: %d %q %q", args, code, out, errOut)
			}
		}
		code, out, errOut := c6Run([]string{"metric", "ingest", "--session", "s1"}, &c6FillReader{left: MaxStdinBytes + 1})
		if code != 1 || out != "" || errOut != "metric: stdin exceeds 4194304 bytes\n" {
			t.Fatalf("oversized ingest in a deleted cwd: %d %q %q", code, out, errOut)
		}
	})

	t.Run("crw_cli_failed_metric", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		if err := os.WriteFile(filepath.Join(root, ".crw"), []byte("not a directory\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := c6Run([]string{"metric", "record", "--session", "s1", "--name", "x", "--value", "1"}, strings.NewReader(""))
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "crw cli failed: ") || !strings.HasSuffix(errOut, "\n") {
			t.Fatalf("metric write failure: %d %q %q", code, out, errOut)
		}
	})

	t.Run("crw_cli_failed_divergence", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		if err := os.WriteFile(filepath.Join(root, ".crw"), []byte("not a directory\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := c6Run([]string{"divergence", "mode", "on", "--session", "s1", "--collapse", "D", "--reason", "x"}, strings.NewReader(""))
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "crw cli failed: ") || !strings.HasSuffix(errOut, "\n") {
			t.Fatalf("divergence write failure: %d %q %q", code, out, errOut)
		}
	})

	t.Run("crw_cli_failed_review_round", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		st := state.DefaultState("s1", "../escape")
		if err := state.WriteState(root, st); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := c6Run([]string{"review-round", "abort", "--session", "s1"}, strings.NewReader(""))
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "crw cli failed: ") || !strings.HasSuffix(errOut, "\n") {
			t.Fatalf("review-round lock failure: %d %q %q", code, out, errOut)
		}
	})

	t.Run("writes", func(t *testing.T) {
		pabcdCLITestHome(t)
		code, out, errOut := c6Run([]string{"metric", "record", "--session", "s1", "--name", "latency_ms", "--value", "120"}, strings.NewReader(""))
		if code != 0 || out != "metric record: latency_ms=120 best=120 source=operator-entered\n" || errOut != "" {
			t.Fatalf("metric record: %d %q %q", code, out, errOut)
		}
		code, out, errOut = c6Run([]string{"divergence", "mode", "on", "--session", "s1", "--collapse", "D", "--reason", "plateau", "--json"}, strings.NewReader(""))
		if code != 0 || errOut != "" || !strings.Contains(out, "\"active\":true") || !strings.Contains(out, "\"objectiveKind\":\"maximize\"") {
			t.Fatalf("divergence mode on: %d %q %q", code, out, errOut)
		}
		code, out, errOut = c6Run([]string{"review-round", "show", "--session", "s1"}, strings.NewReader(""))
		if code != 1 || out != "review-round show: this session has no bound goalplan\n" || errOut != "" {
			t.Fatalf("review-round show: %d %q %q", code, out, errOut)
		}
	})
}
