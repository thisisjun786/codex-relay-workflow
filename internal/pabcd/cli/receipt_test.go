package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "--receipt-helper" {
		receiptHelper(os.Args[2:])
		os.Exit(0)
	}
	testsupport.Main(m)
}

// A real child of the runner: no Node, shell, or mocked process outcome.
func receiptHelper(a []string) {
	switch a[0] {
	case "write":
		if err := os.WriteFile(a[1], []byte(a[2]), 0o644); err != nil {
			panic(err)
		}
	case "exit":
		if a[1] == "3" {
			os.Exit(3)
		}
	case "signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		os.Exit(90)
	case "argv":
		_ = json.NewEncoder(os.Stdout).Encode(a[1:])
	case "cwd":
		cwd, _ := os.Getwd()
		fmt.Fprint(os.Stdout, cwd)
	case "env":
		for _, key := range a[1:] {
			fmt.Fprintf(os.Stdout, "%s=%s\n", key, os.Getenv(key))
		}
	case "block":
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "hold":
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "descendant":
		child := exec.Command(os.Args[0], "--receipt-helper", "hold")
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			panic(err)
		}
		fmt.Fprintln(os.Stdout, child.Process.Pid)
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "atomic-failure":
		// git status must not refresh its index under the helper's file-size limit: the failure belongs to receipt publication.
		if err := os.Setenv("GIT_OPTIONAL_LOCKS", "0"); err != nil {
			panic(err)
		}
		signal.Ignore(syscall.SIGXFSZ)
		var limit syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
			panic(err)
		}
		limit.Cur = 24
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
			panic(err)
		}
		args := ReceiptCLIArgs{Verb: "test", Cwd: a[1], Session: "s1", Command: []string{os.Args[0], "--receipt-helper", "exit", "0"}}
		result, err := RunReceiptCLI(args, ReceiptRunOptions{Stdout: io.Discard, Stderr: io.Discard})
		if err == nil {
			fmt.Fprintln(os.Stdout, result)
			os.Exit(91)
		}
		fmt.Fprint(os.Stdout, "publication refused")
	default:
		panic("unknown receipt helper")
	}
}

func receiptMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func receiptWrite(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	receiptMust(t, os.MkdirAll(filepath.Dir(path), 0o755))
	receiptMust(t, os.WriteFile(path, []byte(content), 0o644))
}
func receiptGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
func receiptRepo(t *testing.T) string {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			receiptMust(t, os.Unsetenv(key))
		}
	}
	root := t.TempDir()
	for key, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": root, "GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"} {
		t.Setenv(key, value)
	}
	receiptGit(t, root, "init", "-q", "-b", "main")
	receiptWrite(t, root, "build/graph.json", "initial")
	receiptWrite(t, root, "src.txt", "source")
	receiptGit(t, root, "add", "-A")
	receiptGit(t, root, "commit", "-qm", "initial")
	receiptState(t, root, state.PhaseC, "check-epoch")
	return root
}
func receiptState(t *testing.T, root string, phase state.Phase, epoch string) state.State {
	t.Helper()
	s := state.DefaultState("s1", "")
	s.Phase, s.OrchestrationActive = phase, phase != state.PhaseIdle
	if epoch != "" {
		s.CheckEpoch = &epoch
	}
	receiptMust(t, state.WriteState(root, s))
	return s
}
func receiptCommand(t *testing.T, mode string, args ...string) []string {
	t.Helper()
	bin, err := os.Executable()
	receiptMust(t, err)
	return append([]string{bin, "--receipt-helper", mode}, args...)
}
func receiptRun(t *testing.T, args ReceiptCLIArgs, o ReceiptRunOptions) ReceiptCLIResult {
	t.Helper()
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	r, err := RunReceiptCLI(args, o)
	receiptMust(t, err)
	return r
}
func expectedReceiptPath(root string) string {
	return filepath.Join(root, ".crw/evidence/s1/test-receipt.json")
}
func receiptAbsent(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(expectedReceiptPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt survives: %v", err)
	}
}

func TestReceiptParserOracle(t *testing.T) {
	var cases []struct {
		Argv   []string
		Cwd    string
		Result json.RawMessage
	}
	b, err := os.ReadFile("testdata/parser.json")
	receiptMust(t, err)
	receiptMust(t, json.Unmarshal(b, &cases))
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			var expected struct{ Error string }
			receiptMust(t, json.Unmarshal(c.Result, &expected))
			got, err := ParseReceiptCLIArgs(c.Argv, c.Cwd)
			if expected.Error != "" {
				var parse ReceiptCLIParseError
				if !errors.As(err, &parse) || err.Error() != expected.Error {
					t.Fatalf("parse error %v, want %q", err, expected.Error)
				}
				return
			}
			receiptMust(t, err)
			var want ReceiptCLIArgs
			receiptMust(t, json.Unmarshal(c.Result, &want))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v", got, want)
			}
		})
	}
}

func TestReceiptGeneratedOracle(t *testing.T) {
	var cases []struct {
		Name      string
		Generated []string
		Result    ReceiptCLIResult
		Receipt   map[string]any
	}
	b, err := os.ReadFile("testdata/runtime.json")
	receiptMust(t, err)
	receiptMust(t, json.Unmarshal(b, &cases))
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			root := receiptRepo(t)
			mode, argv := "exit", []string{"0"}
			switch c.Name {
			case "generated-directory", "wrong-path", "generated-file":
				mode, argv = "write", []string{"build/graph.json", "changed"}
			case "undeclared-source":
				mode, argv = "write", []string{"src.txt", "changed"}
			case "generated-sibling":
				mode, argv = "write", []string{"build/other.json", "changed"}
			case "nonzero":
				argv = []string{"3"}
			case "signal":
				mode, argv = "signal", nil
			}
			args := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Generated: c.Generated, Command: receiptCommand(t, mode, argv...)}
			got := receiptRun(t, args, ReceiptRunOptions{})
			if got.Code == 0 {
				got.Output = "<RECEIPT>"
			}
			if got != c.Result {
				t.Fatalf("got %#v, want %#v", got, c.Result)
			}
			if c.Receipt == nil {
				receiptAbsent(t, root)
				return
			}
			var record map[string]any
			b, err := os.ReadFile(expectedReceiptPath(root))
			receiptMust(t, err)
			receiptMust(t, json.Unmarshal(b, &record))
			id := record["sourceIdentity"].(map[string]any)
			observed := map[string]any{"kind": record["kind"], "exitCode": record["exitCode"], "ownerSessionId": record["ownerSessionId"], "checkEpoch": record["checkEpoch"], "generatedPaths": record["generatedPaths"], "dirty": id["dirty"]}
			if !reflect.DeepEqual(observed, c.Receipt) {
				t.Fatalf("receipt %v, want %v", observed, c.Receipt)
			}
			if _, err := time.Parse(time.RFC3339Nano, record["createdAt"].(string)); err != nil {
				t.Fatal(err)
			}
			if parsed, err := gate.ParseSourceBoundReceipt(expectedReceiptPath(root), root, gate.ReceiptTest); err != nil || parsed.Command == nil || *parsed.Command != strings.Join(args.Command, " ") {
				t.Fatalf("reader: %#v, %v", parsed, err)
			}
			if verdict := gate.ValidateCheckReceipt(state.ReadState(root, "s1"), "s1", expectedReceiptPath(root), root); !verdict.OK {
				t.Fatal(verdict.Reason)
			}
		})
	}
}

func TestReceiptGuardsOracle(t *testing.T) {
	var cases []struct {
		Name           string
		Result         ReceiptCLIResult
		PriorPreserved bool
	}
	b, err := os.ReadFile("testdata/boundary.json")
	receiptMust(t, err)
	receiptMust(t, json.Unmarshal(b, &cases))
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			root := receiptRepo(t)
			receiptWrite(t, root, ".crw/evidence/s1/test-receipt.json", "prior receipt")
			a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
			switch c.Name {
			case "missing-session":
				a.Session = ""
			case "missing-command":
				a.Command = nil
			case "phase-b":
				receiptState(t, root, state.PhaseB, "check-epoch")
			case "missing-epoch":
				receiptState(t, root, state.PhaseC, "")
			case "missing-executable":
				a.Command = []string{"crw-receipt-missing-command"}
			case "relative-path":
				receiptMust(t, os.Mkdir(filepath.Join(root, "tool-bin"), 0o755))
				receiptMust(t, os.Symlink(a.Command[0], filepath.Join(root, "tool-bin/probe")))
				t.Setenv("PATH", "tool-bin:"+os.Getenv("PATH"))
				a.Command[0] = "probe"
			}
			got := receiptRun(t, a, ReceiptRunOptions{})
			if got.Code == 0 {
				got.Output = "<RECEIPT>"
			}
			if got != c.Result {
				t.Fatalf("got %#v, want %#v", got, c.Result)
			}
			b, err := os.ReadFile(expectedReceiptPath(root))
			preserved := err == nil && string(b) == "prior receipt"
			if preserved != c.PriorPreserved {
				t.Fatalf("prior preserved=%v, want %v", preserved, c.PriorPreserved)
			}
		})
	}
}

func TestReceiptArgvNoShellAndEncoding(t *testing.T) {
	root := receiptRepo(t)
	var output bytes.Buffer
	literal := []string{"a;touch sentinel", "a&&b", "two words", "", "<html>", "line\u2028break\u2029", `literal\u2028`}
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "\ufeff s1 \ufeff", Command: receiptCommand(t, "argv", literal...)}
	got := receiptRun(t, a, ReceiptRunOptions{Stdout: &output})
	if got.Code != 0 || got.Output != expectedReceiptPath(root) {
		t.Fatalf("run %#v", got)
	}
	var actual []string
	receiptMust(t, json.Unmarshal(output.Bytes(), &actual))
	if !reflect.DeepEqual(actual, literal) {
		t.Fatalf("argv %q, want %q", actual, literal)
	}
	if _, err := os.Stat(filepath.Join(root, "sentinel")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shell interpreted argv")
	}
	b, err := os.ReadFile(got.Output)
	receiptMust(t, err)
	if bytes.Contains(b, []byte(`\u003c`)) || !bytes.Contains(b, []byte("line\u2028break\u2029")) || !bytes.Contains(b, []byte(`literal\\u2028`)) || b[len(b)-1] != '\n' {
		t.Fatalf("not JSON.stringify bytes: %s", b)
	}
}

func TestReceiptFailuresClearPrior(t *testing.T) {
	for _, kind := range []string{"nonzero", "signal", "unavailable", "source-before", "source-after", "trailing-generated"} {
		t.Run(kind, func(t *testing.T) {
			root := receiptRepo(t)
			receiptWrite(t, root, ".crw/evidence/s1/test-receipt.json", "prior receipt")
			a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
			prefix := "receipt test: "
			switch kind {
			case "nonzero":
				a.Command = receiptCommand(t, "exit", "3")
				prefix += "the command exited 3"
			case "signal":
				a.Command = receiptCommand(t, "signal")
				prefix += "the command did not run to completion"
			case "unavailable":
				receiptMust(t, os.Rename(filepath.Join(root, ".git"), filepath.Join(t.TempDir(), "git")))
				prefix += "git could not resolve"
			case "source-before":
				s := state.ReadState(root, "s1")
				s.BoundSourceRoot = &root
				receiptMust(t, state.WriteState(root, s))
				prefix += "SOURCE-ROOT:"
			case "source-after":
				a.Command = receiptCommand(t, "write", ".crw/sources/s1.json", "broken")
				receiptMust(t, os.MkdirAll(filepath.Join(root, ".crw/sources"), 0o755))
				prefix += "SOURCE-ROOT:"
			case "trailing-generated":
				a.Generated = []string{"build/"}
				a.Command = receiptCommand(t, "write", "build/graph.json", "changed")
				prefix += "the command changed the source"
			}
			got := receiptRun(t, a, ReceiptRunOptions{})
			if got.Code == 0 || !strings.HasPrefix(got.Output, prefix) {
				t.Fatalf("run %#v, want %q", got, prefix)
			}
			receiptAbsent(t, root)
		})
	}
}

func TestReceiptBoundWorktreeAndGitEnv(t *testing.T) {
	root := receiptRepo(t)
	wt := filepath.Join(t.TempDir(), "bound")
	receiptGit(t, root, "worktree", "add", "-qb", "bound", wt)
	receiptState(t, root, state.PhaseA, "")
	_, err := session.Bind(root, "s1", wt)
	receiptMust(t, err)
	receiptState(t, root, state.PhaseC, "check-epoch")
	var output bytes.Buffer
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "cwd")}
	got := receiptRun(t, a, ReceiptRunOptions{Stdout: &output})
	if got.Code != 0 || output.String() != wt || got.Output != expectedReceiptPath(root) {
		t.Fatalf("bound result %#v, cwd %q", got, output.String())
	}
	parsed, err := gate.ParseSourceBoundReceipt(got.Output, root, gate.ReceiptTest)
	receiptMust(t, err)
	if parsed.SourceIdentity.SourceRoot == nil || *parsed.SourceIdentity.SourceRoot != wt {
		t.Fatal("bound root not recorded")
	}
	// The binding probe strips four; object routing is an existing oracle defect of the source-binding owner.
	keys := []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"}
	for _, key := range keys {
		t.Setenv(key, "wrong")
	}
	output.Reset()
	a.Command = receiptCommand(t, "env", keys...)
	got = receiptRun(t, a, ReceiptRunOptions{Stdout: &output})
	if got.Code != 0 {
		t.Fatal(got)
	}
	for _, key := range keys {
		if !strings.Contains(output.String(), key+"=\n") {
			t.Fatalf("routing env retained: %s", output.String())
		}
	}
}

func TestReceiptUnboundGitEnvStripsAllRouting(t *testing.T) {
	root := receiptRepo(t)
	keys := []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"}
	for _, key := range keys {
		t.Setenv(key, "wrong")
	}
	var output bytes.Buffer
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "env", keys...)}
	got := receiptRun(t, a, ReceiptRunOptions{Stdout: &output})
	if got.Code != 0 {
		t.Fatal(got)
	}
	for _, key := range keys {
		if !strings.Contains(output.String(), key+"=\n") {
			t.Fatalf("routing env retained: %s", output.String())
		}
	}
}

func TestReceiptCancellationOnlyOwnChild(t *testing.T) {
	root := receiptRepo(t)
	inR, inW, err := os.Pipe()
	receiptMust(t, err)
	defer inR.Close()
	defer inW.Close()
	other := exec.Command(receiptCommand(t, "block")[0], "--receipt-helper", "block")
	other.Stdin = inR
	receiptMust(t, other.Start())
	defer func() { _ = other.Process.Kill(); _ = other.Wait() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readyR, readyW, err := os.Pipe()
	receiptMust(t, err)
	defer readyR.Close()
	defer readyW.Close()
	go func() { b := make([]byte, 6); _, _ = io.ReadFull(readyR, b); cancel() }()
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "block")}
	got := receiptRun(t, a, ReceiptRunOptions{Context: ctx, Stdin: inR, Stdout: readyW})
	if got.Code != 1 || !strings.Contains(got.Output, "terminated by signal") {
		t.Fatal(got)
	}
	receiptMust(t, other.Process.Signal(syscall.Signal(0)))
	receiptAbsent(t, root)
}

func TestReceiptAtomicPublicationFailure(t *testing.T) {
	var change struct{ GoExpected struct{ ReceiptExists bool } }
	b, err := os.ReadFile("testdata/atomic-change.json")
	receiptMust(t, err)
	receiptMust(t, json.Unmarshal(b, &change))
	root := receiptRepo(t)
	cmd := exec.Command(receiptCommand(t, "atomic-failure", root)[0], "--receipt-helper", "atomic-failure", root)
	output, err := cmd.CombinedOutput()
	receiptMust(t, err)
	if string(output) != "publication refused" {
		t.Fatalf("failure not observed: %q", output)
	}
	receiptAbsent(t, root)
	if change.GoExpected.ReceiptExists {
		t.Fatal("recorded intentional change expects a surviving partial record")
	}
	temps, err := filepath.Glob(filepath.Join(root, ".crw/evidence/s1/.*.tmp"))
	receiptMust(t, err)
	if len(temps) != 0 {
		t.Fatalf("partial temporary record survives: %v", temps)
	}
}

func TestReceiptHelpAndPath(t *testing.T) {
	root := t.TempDir()
	got := receiptRun(t, ReceiptCLIArgs{Verb: "help", Cwd: root}, ReceiptRunOptions{})
	if got.Code != 0 || !strings.Contains(got.Output, "crw pabcd receipt test") {
		t.Fatal(got)
	}
	entries, err := os.ReadDir(root)
	receiptMust(t, err)
	if len(entries) != 0 {
		t.Fatal("help wrote state")
	}
	if got := ReceiptPathFor(root, " a/b "); got != filepath.Join(root, ".crw/evidence/a-b/test-receipt.json") {
		t.Fatal(got)
	}
	if got := ReceiptPathFor(root, ".."); got != filepath.Join(root, ".crw/test-receipt.json") {
		t.Fatal("oracle dotdot key changed")
	}
}

func TestReceiptRelativeCwdAndPath(t *testing.T) {
	root := receiptRepo(t)
	bin, err := os.Executable()
	receiptMust(t, err)
	receiptMust(t, os.Mkdir(filepath.Join(root, "tool-bin"), 0o755))
	receiptMust(t, os.Symlink(bin, filepath.Join(root, "tool-bin/probe")))
	t.Chdir(filepath.Dir(root))
	t.Setenv("PATH", "tool-bin:"+os.Getenv("PATH"))
	a := ReceiptCLIArgs{Verb: "test", Cwd: filepath.Base(root), Session: "s1", Command: []string{"probe", "--receipt-helper", "exit", "0"}}
	got := receiptRun(t, a, ReceiptRunOptions{})
	if got.Code != 0 || got.Output != expectedReceiptPath(a.Cwd) {
		t.Fatal(got)
	}
}

func TestReceiptRefusesDirectoryBeforeSpawn(t *testing.T) {
	root := receiptRepo(t)
	path := expectedReceiptPath(root)
	receiptMust(t, os.MkdirAll(path, 0o755))
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "write", "sentinel", "ran")}
	_, err := RunReceiptCLI(a, ReceiptRunOptions{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil {
		t.Fatal("directory was removed instead of refused")
	}
	info, err := os.Stat(path)
	receiptMust(t, err)
	if !info.IsDir() {
		t.Fatal("directory changed")
	}
	if _, err := os.Stat(filepath.Join(root, "sentinel")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("command ran after directory refusal")
	}
}

type receiptPIDWriter struct {
	ids    chan int
	cancel context.CancelFunc
}

func (w receiptPIDWriter) Write(b []byte) (int, error) {
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err == nil {
		w.ids <- pid
		w.cancel()
	}
	return len(b), nil
}

func TestReceiptCancellationWithInheritedStream(t *testing.T) {
	root := receiptRepo(t)
	inR, inW, err := os.Pipe()
	receiptMust(t, err)
	defer inR.Close()
	defer inW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ids := make(chan int, 1)
	result := make(chan ReceiptCLIResult, 1)
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "descendant")}
	go func() {
		got, err := RunReceiptCLI(a, ReceiptRunOptions{Context: ctx, Stdin: inR, Stdout: receiptPIDWriter{ids, cancel}, Stderr: io.Discard})
		if err != nil {
			got.Output = err.Error()
		}
		result <- got
	}()
	pid := <-ids
	holder, err := os.FindProcess(pid)
	receiptMust(t, err)
	defer holder.Kill() // the PID came from the helper this test started; never a process-name sweep
	select {
	case got := <-result:
		if got.Code != 1 || !strings.Contains(got.Output, "terminated by signal") {
			t.Fatal(got)
		}
		receiptMust(t, holder.Signal(syscall.Signal(0)))
		receiptAbsent(t, root)
	case <-time.After(2 * time.Second):
		_ = holder.Kill()
		<-result // release inherited descriptors before failing; leave no overlapping process
		t.Fatal("cancelled runner waited for grandchild pipe EOF")
	}
}
