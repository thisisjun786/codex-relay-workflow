package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func pabcdCLITestRun(args []string, input string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Pabcd(args, strings.NewReader(input), &out, &errOut, Verbs())
	return code, out.String(), errOut.String()
}

func pabcdCLITestHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		dir := filepath.Join(root, key)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	t.Chdir(root)
	return root
}

func TestPabcdCLIArgumentsAndStreams(t *testing.T) {
	pabcdCLITestHome(t)
	for _, tc := range []struct {
		args        []string
		code        int
		out, errOut string
	}{
		{[]string{"--help"}, 0, "usage: crw pabcd [-h] {freeze,plan,receipt,evidence,memory} ...\n", ""},
		{[]string{"plan", "init"}, 1, "", "plan: plan init requires a <slug> argument\n"},
		{[]string{"receipt", "test", "--help"}, 1, "", "receipt: unexpected argument '--help' before --\n"},
		{[]string{"receipt", "test"}, 1, "receipt test: --session <id> is required\n", ""},
		{[]string{"evidence", "--help"}, 1, "", "evidence: unknown evidence verb '--help' (expected resolve)\n"},
		{[]string{"memory"}, 0, cli.MemoryUsage + "\n", ""},
		{[]string{"memory", "allow-write", "--help"}, 0, cli.MemoryUsage + "\n", ""},
		{[]string{"memory", "help"}, 2, "", "memory: unknown memory verb 'help' (expected allow-write)\n" + cli.MemoryUsage + "\n"},
	} {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			code, out, errOut := pabcdCLITestRun(tc.args, "")
			if code != tc.code || out != tc.out || errOut != tc.errOut {
				t.Fatalf("got %d %q %q; want %d %q %q", code, out, errOut, tc.code, tc.out, tc.errOut)
			}
		})
	}
	for _, verb := range []string{"plan", "receipt"} {
		code, out, errOut := pabcdCLITestRun([]string{verb, "--help"}, "")
		if code != 0 || errOut != "" || !strings.HasPrefix(out, "crw pabcd "+verb+" — ") || !strings.HasSuffix(out, "\n") {
			t.Errorf("%s help: %d %q %q", verb, code, out, errOut)
		}
	}
}

func TestPabcdCLIPlanPublishesAndRefusesOverwrite(t *testing.T) {
	root := pabcdCLITestHome(t)
	args := []string{"plan", "init", "260101_demo", "--phases", "2"}
	code, out, errOut := pabcdCLITestRun(args, "")
	want := "plan init: scaffolded devlog/_plan/260101_demo (000_plan.md + 2 phase doc(s)).\nWrite every doc to diff-level BEFORE P -> A; the P>A gate requires planUnit to carry numbered docs.\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("create: %d %q %q", code, out, errOut)
	}
	unit := filepath.Join(root, "devlog", "_plan", "260101_demo")
	for _, name := range []string{"000_plan.md", "010_phase1.md", "020_phase2.md"} {
		if b, err := os.ReadFile(filepath.Join(unit, name)); err != nil || len(b) == 0 {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	file := filepath.Join(unit, "000_plan.md")
	if err := os.WriteFile(file, []byte("keep this plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = pabcdCLITestRun(args, "")
	if code != 1 || errOut != "" || out != "plan init: "+unit+" already exists — refusing to overwrite. Write your docs there.\n" {
		t.Fatalf("repeat: %d %q %q", code, out, errOut)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "keep this plan\n" {
		t.Fatalf("overwrote plan: %q %v", b, err)
	}
}

func TestPabcdCLIMemoryGrantAndUnreadableState(t *testing.T) {
	root := pabcdCLITestHome(t)
	args := []string{"memory", "allow-write", "--session", "s1"}
	code, out, errOut := pabcdCLITestRun(args, "")
	if code != 0 || errOut != "" || out != "memory allow-write: session s1 may perform ONE memory write; grant recorded for cwd "+root+"; the next write consumes this grant.\n" || !state.ReadState(root, "s1").MemoryWriteGrant {
		t.Fatalf("grant: %d %q %q", code, out, errOut)
	}
	path := state.StatePath(root, "s1")
	if err := os.WriteFile(path, []byte("unreadable"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = pabcdCLITestRun(args, "")
	if code != 1 || out != "" || errOut != "memory allow-write: could not record the grant (session state is unreadable; refusing to overwrite it)\n" {
		t.Fatalf("refusal: %d %q %q", code, out, errOut)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "unreadable" {
		t.Fatalf("overwritten state: %q %v", b, err)
	}
}

func TestPabcdCLIEvidenceResolvesSeededVerdict(t *testing.T) {
	root := pabcdCLITestHome(t)
	s := state.DefaultState("s1", "")
	s.UnverifiedSubagents = []state.UnverifiedSubagent{{AgentID: "a1", TurnID: "t1", Resolvable: true}}
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	rel := ".crw/evidence/proof.md"
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("checked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"evidence", "resolve", "--session", "s1", "--agent", "a1", "--turn", "t1", "--receipt", rel}
	code, out, errOut := pabcdCLITestRun(args, "")
	if code != 0 || errOut != "" || out != "evidence resolve: agent a1 resolved against "+rel+"\n" || len(state.ReadState(root, "s1").UnverifiedSubagents) != 0 {
		t.Fatalf("resolve: %d %q %q", code, out, errOut)
	}
	code, out, errOut = pabcdCLITestRun(args, "")
	if code != 1 || errOut != "" || out != "evidence resolve: no resolvable unverified record for agent 'a1' in session s1\n" {
		t.Fatalf("repeat: %d %q %q", code, out, errOut)
	}
}

// Re-executed by receipt test: exercise real child stdin, both streams and exit status.
func TestPabcdCLIReceiptChild(t *testing.T) {
	if os.Getenv("CRW_PABCD_CLI_CHILD") != "1" {
		return
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(90)
	}
	fmt.Fprint(os.Stdout, "child:"+string(b))
	fmt.Fprintln(os.Stderr, "child-stderr")
	if os.Getenv("CRW_PABCD_CLI_CHILD_FAIL") == "1" {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestPabcdCLIReceiptStreamsAndResult(t *testing.T) {
	root := pabcdCLITestHome(t)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": root, "GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"} {
		t.Setenv(k, v)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "fixture"}} {
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %q %v", out, err)
		}
	}
	epoch := "check-epoch"
	s := state.DefaultState("s1", "")
	s.Phase = state.PhaseC
	s.OrchestrationActive = true
	s.CheckEpoch = &epoch
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"receipt", "test", "--session", "s1", "--", bin, "-test.run=^TestPabcdCLIReceiptChild$"}
	t.Setenv("CRW_PABCD_CLI_CHILD", "1")
	t.Setenv("CRW_PABCD_CLI_CHILD_FAIL", "1")
	code, out, errOut := pabcdCLITestRun(args, "input\n")
	if code != 3 || out != "child:input\nreceipt test: the command exited 3; no receipt written\n" || errOut != "child-stderr\n" {
		t.Fatalf("child failure: %d %q %q", code, out, errOut)
	}
	path := cli.ReceiptPathFor(root, "s1")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed command published receipt: %v", err)
	}
	t.Setenv("CRW_PABCD_CLI_CHILD_FAIL", "0")
	code, out, errOut = pabcdCLITestRun(args, "input\n")
	if code != 0 || out != "child:input\n"+path+"\n" || errOut != "child-stderr\n" {
		t.Fatalf("child success: %d %q %q", code, out, errOut)
	}
	var receipt struct {
		Kind           string
		ExitCode       int
		OwnerSessionID string
	}
	if b, err := os.ReadFile(path); err != nil || json.Unmarshal(b, &receipt) != nil || receipt.Kind != "test" || receipt.ExitCode != 0 || receipt.OwnerSessionID != "s1" {
		t.Fatalf("receipt: %q %v %+v", b, err, receipt)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = pabcdCLITestRun(args, "")
	if code != 1 || out != "" || !strings.HasPrefix(errOut, "crw cli failed: unlink "+path+": ") || !strings.HasSuffix(errOut, "\n") {
		t.Fatalf("thrown failure: %d %q %q", code, out, errOut)
	}
}
