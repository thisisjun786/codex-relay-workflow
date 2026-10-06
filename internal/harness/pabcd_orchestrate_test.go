package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The orchestrate row of crw pabcd (CXC v0.2.40 pabcd-state/src/cli.ts:143-152). The row is the
// only place the terminal entry's shape shows: a parse error goes to STDERR with exit 1 and the
// library's own answer never runs (cli.ts:146-149), help and every other answer go to stdout with
// the library's code, and a delegated mutation prints the transition's answer the same way.
//
// Red first on the baseline: with no row the dispatcher answers "invalid choice" with exit 2 for
// every case here.

// orchestrateTestHome is pabcdCLITestHome plus an unset CODEX_THREAD_ID: the row reads the real
// process environment (the oracle's process.env), and this host exports a native thread id that
// would otherwise drive the status path into native resolution. HOME, CODEX_HOME and CRW_HOME are
// temporary, so nothing here can reach a real home.
func orchestrateTestHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		dir := filepath.Join(root, key)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	orchestrateTestUnsetenv(t, "CODEX_THREAD_ID")
	orchestrateTestUnsetenv(t, "CODEX_SQLITE_HOME")
	t.Chdir(root)
	return root
}

func orchestrateTestUnsetenv(t *testing.T, key string) {
	t.Helper()
	value, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// orchestrateTestSeed writes a session file with the given phase, the shape the corpus fixtures use.
func orchestrateTestSeed(t *testing.T, cwd, id, phase string) {
	t.Helper()
	path := state.StatePath(cwd, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("{\"phase\":%q,\"sessionId\":%q}", phase, id)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// orchestrateTestTree snapshots the workspace so a case can prove a read wrote nothing.
func orchestrateTestTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func orchestrateTestSameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("tree changed: %d entries before, %d after", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("tree changed at %s", k)
		}
	}
}

// TestPabcdOrchestrateRow drives the row through the dispatcher: the streams, the trailing newline
// and the exit codes of cli.ts:143-152.
func TestPabcdOrchestrateRow(t *testing.T) {
	t.Run("help_goes_to_stdout", func(t *testing.T) {
		orchestrateTestHome(t)
		help := cli.RenderOrchestrateHelp("") + "\n"
		for _, args := range [][]string{{"orchestrate", "--help"}, {"orchestrate", "-h"}, {"orchestrate", "help"}, {"orchestrate", "status", "--help"}} {
			code, out, errOut := pabcdCLITestRun(args, "")
			if code != 0 || out != help || errOut != "" {
				t.Fatalf("%v: %d %q %q", args, code, out, errOut)
			}
		}
	})

	t.Run("parse_error_goes_to_stderr", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "rec-s1", "P")
		// cli.ts:146-149: the terminal branch writes renderOrchestrateParseError and exits 1, so
		// stdout stays empty and the library's own stdout answer never runs.
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "Z", "--session", "rec-s1"}, "")
		if code != 1 || out != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		want := "orchestrate: current=P session=rec-s1; unknown orchestrate verb 'Z' (expected I|P|A|B|C|D|status|reset); run crw pabcd orchestrate --help\n"
		if errOut != want {
			t.Fatalf("stderr %q, want %q", errOut, want)
		}
	})

	t.Run("parse_error_without_a_session", func(t *testing.T) {
		orchestrateTestHome(t)
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "wat"}, "")
		if code != 1 || out != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		want := "orchestrate: unknown orchestrate verb 'wat' (expected I|P|A|B|C|D|status|reset); run crw pabcd orchestrate --help\n"
		if errOut != want {
			t.Fatalf("stderr %q, want %q", errOut, want)
		}
	})

	t.Run("refusals_go_to_stdout", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "rec-s1", "IDLE")
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "P"}, "")
		if code != 1 || errOut != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		if !strings.HasPrefix(out, "orchestrate P: mutating verbs require an explicit --session <id>") || !strings.HasSuffix(out, "\n") {
			t.Fatalf("stdout %q", out)
		}
		code, out, errOut = pabcdCLITestRun([]string{"orchestrate", "P", "--session", "ghost"}, "")
		if code != 1 || errOut != "" || !strings.Contains(out, "unknown session 'ghost'") || !strings.HasSuffix(out, "\n") {
			t.Fatalf("unknown session: %d %q %q", code, out, errOut)
		}
	})

	t.Run("status_reads_and_creates_nothing", func(t *testing.T) {
		root := orchestrateTestHome(t)
		before := orchestrateTestTree(t, root)
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "status"}, "")
		if code != 0 || out != "no active session\n" || errOut != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		orchestrateTestSameTree(t, before, orchestrateTestTree(t, root))
	})
}

// TestPabcdOrchestrateReadAndDelegate drives the read half and the delegated mutation of
// RunOrchestrateRead (:466-548) and RunOrchestrateTransition (:549-1131).
func TestPabcdOrchestrateReadAndDelegate(t *testing.T) {
	t.Run("status_of_a_seeded_session", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "rec-s1", "P")
		before := orchestrateTestTree(t, root)
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "status", "--session", "rec-s1"}, "")
		if code != 0 || errOut != "" || out != "session=rec-s1 phase=P interview=false auditPassed=false checkPassed=false\n" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		orchestrateTestSameTree(t, before, orchestrateTestTree(t, root))
	})

	t.Run("status_json", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "rec-s1", "P")
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "status", "--session", "rec-s1", "--json"}, "")
		if code != 0 || errOut != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		if !strings.HasPrefix(out, "{\"phase\":\"P\",\"flags\":") || !strings.Contains(out, "\"sessionId\":\"rec-s1\",\"selection\":\"explicit\"") || !strings.HasSuffix(out, "\n") {
			t.Fatalf("stdout %q", out)
		}
	})

	t.Run("mutation_delegates_and_writes", func(t *testing.T) {
		root := orchestrateTestHome(t)
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "P", "--session", "cli"}, "")
		if code != 0 || errOut != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		want := "orchestrate P: current=IDLE -> P (IDLE \u2192 P, session cli) [formal P: architect proposal -> main executable plan -> same-architect reflection before A (crw-pabcd phase-plan)]\n"
		if out != want {
			t.Fatalf("stdout %q, want %q", out, want)
		}
		if _, err := os.Stat(state.StatePath(root, "cli")); err != nil {
			t.Fatalf("the delegated mutation wrote no state: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, ".crw", "ledger.jsonl")); err != nil {
			t.Fatalf("the delegated mutation wrote no ledger row: %v", err)
		}
	})

	t.Run("reset_returns_a_session_to_idle", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "rec-s1", "B")
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "reset", "--session", "rec-s1"}, "")
		if code != 0 || errOut != "" || out != "orchestrate reset: current=B -> IDLE (session rec-s1)\n" {
			t.Fatalf("reset: %d %q %q", code, out, errOut)
		}
		code, out, errOut = pabcdCLITestRun([]string{"orchestrate", "status", "--session", "rec-s1"}, "")
		if code != 0 || errOut != "" || out != "session=rec-s1 phase=IDLE interview=false auditPassed=false checkPassed=false\n" {
			t.Fatalf("status after reset: %d %q %q", code, out, errOut)
		}
	})

	t.Run("illegal_edge_is_refused_without_a_write", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "rec-s1", "IDLE")
		before := orchestrateTestTree(t, root)
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "B", "--session", "rec-s1", "--attest", "{\"from\":\"IDLE\",\"to\":\"B\",\"did\":\"skip\"}"}, "")
		if code != 1 || errOut != "" || out != "orchestrate B: current=IDLE session=rec-s1; illegal transition IDLE->B\n" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		orchestrateTestSameTree(t, before, orchestrateTestTree(t, root))
	})
}

// TestPabcdOrchestrateAttestBatch drives the CLI-level cases of the oracle's
// test/attest-batch.test.ts through the row: the batched refusals must reach the terminal with
// the row's own prefix and exit code, which the library tests alone do not show.
func TestPabcdOrchestrateAttestBatch(t *testing.T) {
	run := func(t *testing.T, phase string, args ...string) (int, string, string) {
		t.Helper()
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "s1", phase)
		return pabcdCLITestRun(args, "")
	}
	const prefix = "orchestrate B: current=A session=s1; "

	t.Run("a_to_b_batches_every_missing_field", func(t *testing.T) {
		code, out, errOut := run(t, "A", "orchestrate", "B", "--session", "s1", "--attest", "{\"from\":\"A\",\"to\":\"B\",\"did\":\"audited the plan\"}")
		if code != 1 || errOut != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		for _, want := range []string{prefix, "(1/2)", "\"auditOutput\"", "(2/2)", "\"auditVerdict\"", "agent_type \"explorer\"", "agent_type \"reviewer\"", "CRW-ROLE: reviewer before TASK:"} {
			if !strings.Contains(out, want) {
				t.Fatalf("stdout lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("near_pass_batches_the_residual_and_not_the_verdict", func(t *testing.T) {
		code, out, errOut := run(t, "A", "orchestrate", "B", "--session", "s1", "--attest", "{\"from\":\"A\",\"to\":\"B\",\"did\":\"audited the plan\",\"auditVerdict\":\"near-pass\"}")
		if code != 1 || errOut != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		if !strings.Contains(out, "auditResidual") || !strings.Contains(out, "auditOutput") {
			t.Fatalf("stdout lacks the residual demand:\n%s", out)
		}
		if strings.Contains(out, "requires \"auditVerdict\"") {
			t.Fatalf("the valid verdict was re-accused:\n%s", out)
		}
	})

	t.Run("contradiction_stays_silent_while_a_field_is_missing", func(t *testing.T) {
		code, out, _ := run(t, "A", "orchestrate", "B", "--session", "s1", "--attest", "{\"from\":\"A\",\"to\":\"B\",\"did\":\"audited the plan\",\"auditVerdict\":\"fail\"}")
		if code != 1 || strings.Contains(out, "is blocked") {
			t.Fatalf("missing-field case: %d %q", code, out)
		}
		code, out, _ = run(t, "A", "orchestrate", "B", "--session", "s1", "--attest", "{\"from\":\"A\",\"to\":\"B\",\"did\":\"audited the plan\",\"auditVerdict\":\"fail\",\"auditOutput\":\"VERDICT: PASS\"}")
		if code != 1 || !strings.Contains(out, "is blocked") {
			t.Fatalf("complete case: %d %q", code, out)
		}
	})

	t.Run("a_lone_reason_is_never_numbered", func(t *testing.T) {
		code, out, _ := run(t, "P", "orchestrate", "A", "--session", "s1", "--attest", "{\"from\":\"P\",\"to\":\"A\",\"did\":\"tbd\"}")
		if code != 1 || strings.Contains(out, "(1/") {
			t.Fatalf("got %d %q", code, out)
		}
		if !strings.HasPrefix(out, "orchestrate A: current=P session=s1; ") {
			t.Fatalf("stdout %q", out)
		}
	})

	t.Run("c_to_d_batches_check_output_exit_code_and_receipt", func(t *testing.T) {
		code, out, errOut := run(t, "C", "orchestrate", "D", "--session", "s1", "--attest", "{\"from\":\"C\",\"to\":\"D\"}")
		if code != 1 || errOut != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		for _, want := range []string{"(1/4)", "(2/4)", "(3/4)", "(4/4)", "checkOutput", "exitCode", "testReceiptPath"} {
			if !strings.Contains(out, want) {
				t.Fatalf("stdout lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("a_complete_failing_check_draws_the_exit_reason_alone", func(t *testing.T) {
		code, out, _ := run(t, "C", "orchestrate", "D", "--session", "s1", "--attest", "{\"from\":\"C\",\"to\":\"D\",\"did\":\"ran the suite\",\"checkOutput\":\"3 failing\",\"exitCode\":1}")
		if code != 1 || !strings.Contains(out, "exitCode 1") || strings.Contains(out, "testReceiptPath") {
			t.Fatalf("got %d %q", code, out)
		}
	})
}

// TestPabcdOrchestrateShapeHint drives the CLI-level cases of the oracle's
// test/attest-shape-hint.test.ts through the row: the hint is rendered on the error path
// (RunOrchestrateRead) and reaches the terminal with the row's stream and exit code.
func TestPabcdOrchestrateShapeHint(t *testing.T) {
	t.Run("inline_attest_without_from_to_names_the_edge", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "s1", "P")
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "A", "--session", "s1", "--attest", "{\"did\":\"wrote the plan\"}"}, "")
		if code != 1 || errOut != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		for _, want := range []string{"attest JSON missing valid from/to", "\"from\":\"P\",\"to\":\"A\"", "\"did\":\"...\"", "planUnit", "needs \"workPhaseId\"."} {
			if !strings.Contains(out, want) {
				t.Fatalf("stdout lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "testReceiptPath") || strings.Contains(out, "auditVerdict") {
			t.Fatalf("a menu instead of this edge's keys:\n%s", out)
		}
	})

	t.Run("attest_file_without_from_to_names_the_path", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "s2", "C")
		if err := os.WriteFile(filepath.Join(root, "bad-attest.json"), []byte("{\"did\":\"verified\"}"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "d", "--session", "s2", "--attest-file", "bad-attest.json"}, "")
		if code != 1 || errOut != "" {
			t.Fatalf("got %d %q %q", code, out, errOut)
		}
		for _, want := range []string{"bad-attest.json is missing valid from/to", "\"from\":\"C\",\"to\":\"D\"", "checkOutput", "exitCode"} {
			if !strings.Contains(out, want) {
				t.Fatalf("stdout lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("an_unresolvable_session_gets_the_status_pointer", func(t *testing.T) {
		orchestrateTestHome(t)
		code, out, _ := pabcdCLITestRun([]string{"orchestrate", "b", "--session", "never-created", "--attest", "{\"did\":\"x\"}"}, "")
		if code != 1 {
			t.Fatalf("got %d %q", code, out)
		}
		for _, want := range []string{"\"to\":\"B\"", "crw pabcd orchestrate status --session", "auditOutput", "\"from\":\"<see status>\""} {
			if !strings.Contains(out, want) {
				t.Fatalf("stdout lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("malformed_json_keeps_its_own_diagnosis", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "s3", "P")
		code, out, _ := pabcdCLITestRun([]string{"orchestrate", "a", "--session", "s3", "--attest", "{not json"}, "")
		if code != 1 || !strings.Contains(out, "attest JSON is not valid JSON") {
			t.Fatalf("got %d %q", code, out)
		}
		if strings.Contains(out, "\"did\":\"...\"") {
			t.Fatalf("a shape example muddied a syntax error:\n%s", out)
		}
	})

	t.Run("an_illegal_edge_names_the_legal_routes", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "s4", "P")
		code, out, _ := pabcdCLITestRun([]string{"orchestrate", "d", "--session", "s4", "--attest", "{\"did\":\"x\"}"}, "")
		if code != 1 || !strings.Contains(out, "P -> D is not a legal edge") || !strings.Contains(out, "legal from P is I|A") {
			t.Fatalf("got %d %q", code, out)
		}
		if strings.Contains(out, "\"from\":\"P\",\"to\":\"D\"") {
			t.Fatalf("a doomed attest was taught:\n%s", out)
		}
	})

	t.Run("a_legal_edge_still_gets_the_example", func(t *testing.T) {
		root := orchestrateTestHome(t)
		orchestrateTestSeed(t, root, "s5", "P")
		code, out, _ := pabcdCLITestRun([]string{"orchestrate", "a", "--session", "s5", "--attest", "{\"did\":\"x\"}"}, "")
		if code != 1 || !strings.Contains(out, "\"from\":\"P\",\"to\":\"A\"") || strings.Contains(out, "not a legal edge") {
			t.Fatalf("got %d %q", code, out)
		}
	})
}
