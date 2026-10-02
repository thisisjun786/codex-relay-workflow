package dagsched

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The epoch commands through the built binary, one process per command, as a parent session uses them.

func epochCLIState(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	f := newFixtureAt(t, filepath.Join(state, "relay.sqlite3"))
	releasePlan(f, "rp")
	f.projectParent()
	f.declareLimit("project", "P-TEST", "runs", 10)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	return state
}

func reasonOf(t *testing.T, out string) string {
	t.Helper()
	reason, _ := parseOut(t, out)["reason"].(string)
	return reason
}

func TestCLIClaimAndTheFence(t *testing.T) {
	state := epochCLIState(t)

	out, code := crw(t, state, "dag-coordinator-claim", "--plan", "rp", "--actor", "parent", "--session-nonce", "session-1")
	first := parseOut(t, out)
	if code != 0 || first["schema"] != SchemaClaim || first["epoch"] != float64(1) || first["task_id"] != "parent" || first["binding_id"] != "bind-parent" || first["replayed"] != false || first["previous_epoch"] != float64(0) {
		t.Fatalf("claim (exit %d):\n%s", code, out)
	}
	if out, code = crw(t, state, "dag-coordinator-claim", "--plan", "rp", "--actor", "parent", "--session-nonce", "session-1"); code != 0 || parseOut(t, out)["replayed"] != true {
		t.Fatalf("a repeated claim (exit %d):\n%s", code, out)
	}
	if out, code = crw(t, state, "dag-coordinator-claim", "--plan", "rp", "--actor", "parent", "--session-nonce", "session-2"); code != 0 || parseOut(t, out)["epoch"] != float64(2) {
		t.Fatalf("the second session's claim (exit %d):\n%s", code, out)
	}
	if out, code = crw(t, state, "dag-coordinator-claim", "--plan", "rp", "--actor", "parent", "--session-nonce", "session-1"); code != 2 || reasonOf(t, out) != "stale_coordinator_epoch" {
		t.Fatalf("a replaced session's claim (exit %d):\n%s", code, out)
	}
	if out, code = crw(t, state, "dag-coordinator-claim", "--plan", "rp", "--actor", "intruder", "--session-nonce", "session-3"); code != 2 || reasonOf(t, out) != "scope_role_mismatch" {
		t.Fatalf("a task that is not the parent (exit %d):\n%s", code, out)
	}

	decision := func(epoch ...string) (string, int) {
		args := []string{"dag-decision-record", "--plan", "rp", "--actor", "parent", "--subject", "merge holds", "--digest", dig("subject"), "--disposition", "approved", "--authority-kind", "user", "--authority-ref", "ref"}
		return crw(t, state, append(args, epoch...)...)
	}
	for name, epoch := range map[string][]string{"the stale epoch": {"--expect-epoch", "1"}, "no epoch on a claimed plan": nil, "an epoch nobody claimed": {"--expect-epoch", "7"}} {
		if out, code := decision(epoch...); code != 2 || reasonOf(t, out) != "stale_coordinator_epoch" {
			t.Fatalf("%s (exit %d):\n%s", name, code, out)
		}
	}
	if out, code := decision("--expect-epoch=-1"); code != 4 {
		t.Fatalf("a negative epoch is a usage error (exit %d):\n%s", code, out)
	}
	if out, code := decision("--expect-epoch", "2"); code != 0 || parseOut(t, out)["ok"] != true {
		t.Fatalf("the current epoch (exit %d):\n%s", code, out)
	}

	// dag-plan-put: the revision document's own field is the epoch
	revision := func(epoch int64) string {
		raw, err := json.Marshal(doc{"schema": dag.SchemaRevision, "plan_id": "rp", "project_key": "P-TEST", "request_id": "cli-put", "expected_parent_revision": 1, "author_task_id": "parent", "coordinator_epoch": epoch,
			"changes": []any{doc{"op": dag.OpAddNode, "node": nodeDoc("Z", dag.NodeNonPR)}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if out, code := crw(t, state, "dag-plan-put", "--request", revision(1)); code != 2 || reasonOf(t, out) != "stale_coordinator_epoch" {
		t.Fatalf("a plan revision of the stale epoch (exit %d):\n%s", code, out)
	}
	if out, code := crw(t, state, "dag-plan-put", "--request", revision(2)); code != 0 || parseOut(t, out)["coordinator_epoch"] != float64(2) || parseOut(t, out)["revision_no"] != float64(2) {
		t.Fatalf("a plan revision of the current epoch (exit %d):\n%s", code, out)
	}

	// a cap basis for a project that is under an epoch names its plan
	basis := []string{"dag-cap-basis-record", "--limit", "lim-project-runs", "--revision", "1", "--w-minutes", "30", "--w-source", "measured", "--s-minutes", "5", "--s-source", "measured", "--actor", "parent"}
	if out, code := crw(t, state, basis...); code != 2 || reasonOf(t, out) != "stale_coordinator_epoch" {
		t.Fatalf("a basis without a plan (exit %d):\n%s", code, out)
	}
	if out, code := crw(t, state, append(basis, "--plan", "rp", "--expect-epoch", "1")...); code != 2 || reasonOf(t, out) != "stale_coordinator_epoch" {
		t.Fatalf("a basis under the stale epoch (exit %d):\n%s", code, out)
	}
	if out, code := crw(t, state, append(basis, "--plan", "rp", "--expect-epoch", "2")...); code != 0 {
		t.Fatalf("a basis under the current epoch (exit %d):\n%s", code, out)
	}

	// restart reads, and answers who holds the epoch
	out, code = crw(t, state, "dag-restart", "--plan", "rp", "--actor", "parent")
	report := parseOut(t, out)
	coordinator, _ := report["coordinator"].(map[string]any)
	if code != 0 || report["schema"] != SchemaRestart || coordinator["holds"] != true || coordinator["epoch"] != float64(2) || coordinator["session_nonce"] != "session-2" {
		t.Fatalf("restart (exit %d):\n%s", code, out)
	}
	if out, code = crw(t, state, "dag-restart", "--plan", "rp", "--actor", "intruder"); code != 0 || parseOut(t, out)["coordinator"].(map[string]any)["holds"] != false {
		t.Fatalf("restart for a task that does not hold the epoch (exit %d):\n%s", code, out)
	}
	if out, code = crw(t, state, "dag-adopt", "--plan", "rp", "--node", "A", "--actor", "parent", "--expect-epoch", "2"); code != 2 || reasonOf(t, out) != "unregistered_relationship" {
		t.Fatalf("adopt for a node that has no execution (exit %d):\n%s", code, out)
	}
	if out, code = crw(t, state, "dag-adopt", "--plan", "rp", "--node", "A", "--actor", "parent", "--expect-epoch", "1"); code != 2 || reasonOf(t, out) != "stale_coordinator_epoch" {
		t.Fatalf("adopt under the stale epoch (exit %d):\n%s", code, out)
	}
}

// The scheduler page names every command of the epoch and the reason a stale session is refused with, the flag that carries the epoch, and the limits of what it recovers.
func TestSchedulerPageDescribesTheEpoch(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/relay/dag-scheduler.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{"dag-coordinator-claim", "dag-adopt", "dag-restart", "--expect-epoch", "stale_coordinator_epoch", "needs_operator", "parent_handover"} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/relay/dag-scheduler.md does not mention %s", want)
		}
	}
	plans, err := os.ReadFile("../../../docs/relay/dag-plans.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plans), "stale_coordinator_epoch") {
		t.Error("docs/relay/dag-plans.md does not name the refusal a stale epoch gets")
	}
}
