package dag

import (
	"path/filepath"
	"strings"
	"testing"
)

// CRW-281: pause, resume, cancel and archive are typed changes of the revision document. This is the operator's round trip over the
// command line, one process per command on one store: what is read back is what the log holds.

func lifeChange(op, node string) doc {
	d := doc{"op": op}
	if node != "" {
		d["node_id"] = node
	}
	return d
}

// forkJoinStateDigest and the node slice digests are what forkJoin("plan", ...) digested to before the lifecycle changes existed: a plan that
// has none keeps its digests, so every plan already stored still verifies.
const forkJoinStateDigest = "608cf71310279ae9f5dcb19b069a7740dd2e9a8aa34c44f14d90b285d127b493"

var forkJoinSliceDigests = map[string]string{
	"design": "026c2346ed12f4aa89aeba04640fa8a5c3761dc343c38e8aa87e5c93bc9cdb54",
	"impl-a": "99b1abed7f2af931a9710395297c3d6f1461672975cb0505f19fa1d98db018c8",
	"impl-b": "accca2048c4bb65f8cc5f624fa3d23517ce9a42998c5436fef0d47742a9aa170",
	"join":   "75278fa2aef6739e8d973763edfafef7869b0f72c8d66b8218e1ab58862b6dce",
	"ship":   "7c7c16f6d1f60b909a8b2a9510384e168cd1a9069216d7df33bfc3f26eacc585",
}

func TestLifecycleChangesRoundTripTheCLI(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	put := func(name string, d doc) map[string]any {
		t.Helper()
		out, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, name+".json", d))
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", name, code, out)
		}
		return parseOut(t, out)
	}
	show := func(args ...string) map[string]any {
		t.Helper()
		out, code := crw(t, state, append([]string{"dag-plan-show", "--plan", "plan"}, args...)...)
		if code != 0 {
			t.Fatalf("show %v: exit %d\n%s", args, code, out)
		}
		return parseOut(t, out)
	}
	lifecycle := func(snapshot map[string]any, node string) any { return nodeByID(snapshot["nodes"])[node]["lifecycle"] }

	first := put("r1", forkJoin("plan", "r1"))
	if first["state_digest"] != forkJoinStateDigest {
		t.Fatalf("a plan without lifecycle changes digests to %v, want the digest it always had", first["state_digest"])
	}
	for id, want := range forkJoinSliceDigests {
		if first["node_digests"].(map[string]any)[id] != want {
			t.Fatalf("slice digest of %s moved: %v", id, first["node_digests"])
		}
	}

	// a pause: one revision, no node or edge row touched, no slice digest moved (a pause is not a change of what a node is), one state digest changed
	paused := put("r2", revDoc("plan", "r2", 1, lifeChange("pause_node", "design")))
	if paused["revision_no"] != float64(2) || paused["state_digest"] == first["state_digest"] {
		t.Fatalf("the pause did not become a revision of its own: %v", paused)
	}
	for id, want := range forkJoinSliceDigests {
		if paused["node_digests"].(map[string]any)[id] != want {
			t.Fatalf("the pause moved the slice digest of %s", id)
		}
	}
	snap := show("--verify")
	if lifecycle(snap, "design") != "paused" || lifecycle(snap, "impl-a") != nil || snap["plan_state"] != nil || snap["log_verified"] != true {
		t.Fatalf("after the pause:\n%v", snap)
	}
	if snap["state_digest"] != paused["state_digest"] {
		t.Fatal("the plan read back digests to something the revision did not record")
	}

	// resuming is the plan it was before: the same state digest
	resumed := put("r3", revDoc("plan", "r3", 2, lifeChange("resume_node", "design")))
	if resumed["state_digest"] != forkJoinStateDigest {
		t.Fatalf("a resumed plan digests to %v, want the digest of the plan it was before the pause", resumed["state_digest"])
	}
	if snap = show(); lifecycle(snap, "design") != nil {
		t.Fatalf("a resumed node still reads %v", lifecycle(snap, "design"))
	}

	// cancel, archive and the plan's pause
	put("r4", revDoc("plan", "r4", 3, lifeChange("cancel_node", "impl-a"), lifeChange("archive_node", "impl-b")))
	put("r5", revDoc("plan", "r5", 4, lifeChange("pause_plan", "")))
	snap = show("--verify")
	if lifecycle(snap, "impl-a") != "cancelled" || lifecycle(snap, "impl-b") != "archived" || snap["plan_state"] != "paused" || snap["log_verified"] != true {
		t.Fatalf("after cancel, archive and the plan's pause:\n%v", snap)
	}
	put("r6", revDoc("plan", "r6", 5, lifeChange("resume_plan", "")))
	if snap = show(); snap["plan_state"] != nil || lifecycle(snap, "impl-a") != "cancelled" {
		t.Fatalf("after the plan resumed:\n%v", snap)
	}

	// the plan as of any revision is the fold of the log up to it
	if old := show("--revision", "2"); lifecycle(old, "design") != "paused" || lifecycle(old, "impl-a") != nil || old["state_digest"] != paused["state_digest"] {
		t.Fatalf("revision 2 read back as\n%v", old)
	}
	if old := show("--revision", "5"); old["plan_state"] != "paused" {
		t.Fatalf("revision 5 read back as\n%v", old)
	}

	// the log carries the typed changes themselves
	out, code := crw(t, state, "dag-plan-log", "--plan", "plan", "--after", "1")
	if code != 0 {
		t.Fatalf("log: exit %d\n%s", code, out)
	}
	var ops []string
	for _, e := range parseOut(t, out)["events"].([]any) {
		for _, c := range e.(map[string]any)["changes"].([]any) {
			ops = append(ops, c.(map[string]any)["op"].(string))
		}
	}
	if got := strings.Join(ops, " "); got != "pause_node resume_node cancel_node archive_node pause_plan resume_plan" {
		t.Fatalf("the log carries %q", got)
	}
}

// A lifecycle change is judged like every other change, before anything is written, and a refusal names the rule.
func TestLifecycleRefusalsOverTheCLI(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if out, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, "r1.json", forkJoin("plan", "r1"))); code != 0 {
		t.Fatalf("put: %d\n%s", code, out)
	}
	if out, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, "r2.json", revDoc("plan", "r2", 1, lifeChange("cancel_node", "design")))); code != 0 {
		t.Fatalf("cancel: %d\n%s", code, out)
	}
	for name, c := range map[string]struct {
		change doc
		rule   string
	}{
		"resuming a cancelled node":      {lifeChange("resume_node", "design"), "invalid_lifecycle_transition"},
		"pausing a node that is not in":  {lifeChange("pause_node", "ghost"), "unknown_node"},
		"resuming a plan that is active": {lifeChange("resume_plan", ""), "invalid_lifecycle_transition"},
		"a node id on a plan change":     {doc{"op": "pause_plan", "node_id": "design"}, "unknown_field"},
		"a plan change on a node change": {doc{"op": "pause_node"}, "missing_field"},
		"a reason is not a field":        {doc{"op": "pause_node", "node_id": "join", "reason": "because"}, "unknown_field"},
	} {
		out, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, strings.ReplaceAll(name, " ", "_")+".json", revDoc("plan", "rx-"+strings.ReplaceAll(name, " ", "_"), 2, c.change)))
		answer := parseOut(t, out)
		if code != 2 || answer["reason"] != "malformed_receipt" || !strings.Contains(out, "["+c.rule+"]") {
			t.Errorf("%s: exit %d, want a refusal naming %s\n%s", name, code, c.rule, out)
		}
	}
	if out, code := crw(t, state, "dag-plan-show", "--plan", "plan"); code != 0 || !strings.Contains(out, "\"revision_no\": 2") && !strings.Contains(out, "\"revision_no\":2") {
		t.Fatalf("a refused revision moved the head: %d\n%s", code, out)
	}
}
