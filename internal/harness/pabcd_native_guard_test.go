package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1108 (B2-01): every terminal command that changes a session, not only orchestrate, is bound to the native session it runs
// in. With CODEX_THREAD_ID …279 a mutation that names …280 is refused before its lock, its files and its ledger are touched; the
// same commands without the native environment, and the reading verbs, are not refused by the guard.
func TestPabcdTerminalMutationsRefuseAForeignSessionUnderANativeOne(t *testing.T) {
	const native, foreign = "0190cafe-0279-7000-8000-000000000279", "0190cafe-0279-7000-8000-000000000280"
	root := orchestrateTestHome(t)
	repoB := filepath.Join(root, "repo-B")
	if err := os.Mkdir(repoB, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoB)
	orchestrateTestSeed(t, repoB, native, "IDLE")
	orchestrateTestSeed(t, repoB, foreign, "C")
	mutations := [][]string{
		{"memory", "allow-write", "--session", foreign},
		{"scan", "record", "--session", foreign, "--contradictions", "0", "--high", "0"},
		{"review-round", "open", "--session", foreign},
		{"review-round", "abort", "--session", foreign},
		{"receipt", "test", "--session", foreign, "--", "true"},
		{"evidence", "resolve", "--session", foreign, "--agent", "a1", "--receipt", "r.json"},
		{"metric", "record", "--session", foreign, "--name", "m", "--value", "1"},
		{"metric", "ingest", "--session", foreign, "--source", "evaluate.sh"},
		{"metric", "kind", "--session", foreign, "maximize"},
		{"divergence", "mode", "on", "--session", foreign, "--collapse", "P", "--reason", "r"},
		{"divergence", "candidate", "add", "--session", foreign, "--kind", "add-1", "--title", "t", "--rationale", "r", "--source", "https://example.com"},
		{"loop", "init", "--objective", "o", "--session", foreign},
		{"loop", "add-task", "--session", foreign, "--work-phase", "wp1", "--id", "t1", "--title", "t"},
		{"loop", "steer", "--session", foreign, "--batch-json", "{}"},
	}
	t.Setenv("CODEX_THREAD_ID", native)
	for _, args := range mutations {
		before := orchestrateTestTree(t, repoB)
		code, out, errOut := pabcdCLITestRun(args, "METRIC m=1\n")
		both := out + errOut
		if code != 1 || !strings.Contains(both, "--session '"+foreign+"' is not the native Codex session") || !strings.Contains(both, "Nothing was written.") {
			t.Errorf("%v under native %s: code %d out %q err %q", args, native, code, out, errOut)
			continue
		}
		orchestrateTestSameTree(t, before, orchestrateTestTree(t, repoB))
		if _, err := os.Lstat(state.SessionLockPath(repoB, foreign)); !os.IsNotExist(err) {
			t.Errorf("%v: the refusal left a lock: %v", args, err)
		}
	}
	if state.ReadState(repoB, foreign).MemoryWriteGrant {
		t.Fatal("the foreign session's memory grant was written")
	}

	// the reading verbs stay reads of any id
	for _, args := range [][]string{
		{"metric", "show", "--session", foreign},
		{"divergence", "candidate", "list", "--session", foreign},
		{"review-round", "show", "--session", foreign},
	} {
		_, out, errOut := pabcdCLITestRun(args, "")
		if strings.Contains(out+errOut, "is not the native Codex session") {
			t.Errorf("%v: a read was refused: %q %q", args, out, errOut)
		}
	}

	// the own native id passes the guard, and so does any id without the native environment
	code, out, errOut := pabcdCLITestRun([]string{"memory", "allow-write", "--session", native}, "")
	if code != 0 || !state.ReadState(repoB, native).MemoryWriteGrant {
		t.Fatalf("own native memory grant: code %d out %q err %q", code, out, errOut)
	}
	orchestrateTestUnsetenv(t, "CODEX_THREAD_ID")
	code, out, errOut = pabcdCLITestRun([]string{"memory", "allow-write", "--session", foreign}, "")
	if code != 0 || !state.ReadState(repoB, foreign).MemoryWriteGrant {
		t.Fatalf("explicit id without a native environment: code %d out %q err %q", code, out, errOut)
	}
	for _, args := range mutations[1:] {
		_, out, errOut := pabcdCLITestRun(args, "METRIC m=1\n")
		if strings.Contains(out+errOut, "is not the native Codex session") {
			t.Errorf("%v without a native environment: refused by the guard: %q %q", args, out, errOut)
		}
	}
}
