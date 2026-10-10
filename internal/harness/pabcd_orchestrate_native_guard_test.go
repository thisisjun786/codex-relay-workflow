package harness

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1108 (B2-01, notes/B2.md): the terminal row with native CODEX_THREAD_ID …279 ran
// `orchestrate P --session …280 --cwd repo-B` and changed the foreign session IDLE->P with exit 0. The
// row now refuses it before the lock, the state file and the ledger are touched; the same id still
// enters P, and a terminal without the native environment keeps its explicit id.
func TestPabcdOrchestrateRowRefusesAForeignSessionUnderANativeOne(t *testing.T) {
	const native, foreign = "0190cafe-0279-7000-8000-000000000279", "0190cafe-0279-7000-8000-000000000280"
	root := orchestrateTestHome(t)
	repoB := filepath.Join(root, "repo-B")
	if err := os.Mkdir(repoB, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(os.Getenv("CODEX_HOME"), "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"CREATE TABLE threads (id TEXT, cwd TEXT, archived INTEGER, source TEXT)", "INSERT INTO threads VALUES ('" + native + "', '" + repoB + "', 0, 'cli')"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	orchestrateTestSeed(t, repoB, native, "IDLE")
	orchestrateTestSeed(t, repoB, foreign, "IDLE")
	t.Setenv("CODEX_THREAD_ID", native)

	before := orchestrateTestTree(t, repoB)
	code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "P", "--session", foreign, "--cwd", repoB}, "")
	if code != 1 || errOut != "" || !strings.Contains(out, "--session '"+foreign+"'") || !strings.Contains(out, "Nothing was written.") {
		t.Fatalf("foreign mutation under a native session: code %d out %q err %q", code, out, errOut)
	}
	orchestrateTestSameTree(t, before, orchestrateTestTree(t, repoB))
	if _, err := os.Lstat(state.StatePath(repoB, foreign) + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("the refusal left a lock: %v", err)
	}

	code, out, errOut = pabcdCLITestRun([]string{"orchestrate", "P", "--session", native, "--cwd", repoB}, "")
	if code != 0 || errOut != "" || !strings.Contains(out, "session "+native) || state.ReadState(repoB, native).Phase != state.PhaseP {
		t.Fatalf("own native P: code %d out %q err %q", code, out, errOut)
	}

	orchestrateTestUnsetenv(t, "CODEX_THREAD_ID")
	code, out, errOut = pabcdCLITestRun([]string{"orchestrate", "P", "--session", foreign, "--cwd", repoB}, "")
	if code != 0 || errOut != "" || state.ReadState(repoB, foreign).Phase != state.PhaseP {
		t.Fatalf("explicit id without a native environment: code %d out %q err %q", code, out, errOut)
	}
}
