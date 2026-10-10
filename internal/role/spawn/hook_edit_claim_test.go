package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1121 (post-evaluation verification of 4c55891f): an edited input never removes an earlier assignment of its call that a child
// claimed after the hook read it as open.

// --- an edited input removes the call's earlier assignment only while no child has claimed it.

func TestSpawnEditedInputKeepsAnAssignmentClaimedAfterItsSnapshot(t *testing.T) {
	r := newAssignedRig(t)
	first, _ := r.spawnCall("TASK: first\nCRW-WORKTREE: "+r.wt, "call-1")
	id := assignedID.FindStringSubmatch(first)
	if id == nil {
		t.Fatal("no first assignment")
	}
	records, _ := filepath.Glob(filepath.Join(r.cwd, ".crw", "evidence-assignments", "*", id[1]+".json"))
	if len(records) != 1 {
		t.Fatalf("records %v", records)
	}
	receipt := r.put(filepath.Join(r.wt, ".crw", "evidence", "receipt.txt"), "verified")
	var before []byte
	// The edited input read the earlier record as open; its child claims it before the edited input is allowed.
	spawnHookBeforePersist = func() {
		r.deliver("worker", first)
		if out := r.stop("worker", "t1", "EVIDENCE_RECORDED: "+receipt); out != "" {
			t.Fatalf("the claim was refused: %s", out)
		}
		var err error
		before, err = os.ReadFile(records[0])
		spawnHookMust(t, err)
		if !strings.Contains(string(before), `"status":"claimed"`) {
			t.Fatalf("not claimed: %s", before)
		}
	}
	t.Cleanup(func() { spawnHookBeforePersist = func() {} })
	edited, out := r.spawnCall(strings.Replace(first, "TASK: first", "TASK: second", 1), "call-1")
	if !strings.Contains(out, `"permissionDecision":"allow"`) {
		t.Fatalf("the edited input = %s", out)
	}
	after, err := os.ReadFile(records[0])
	if err != nil || string(after) != string(before) {
		t.Fatalf("the record claimed after the snapshot was removed or changed: %v\nbefore=%s\nafter=%s", err, before, after)
	}
	if got := assignedID.FindStringSubmatch(edited); got == nil || got[1] == id[1] {
		t.Fatalf("the edited input does not have an assignment of its own: %q", edited)
	}
}
