package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// CRW-1121 (pre-merge evaluation of 2be6b6f2): a normalized prompt is not inserted again, an edited input is a dispatch of its own,
// and an event lock that cannot be had cannot let a later delivery overwrite a claimed record.

// --- CRW-1121 d1: a prompt the hook normalized in an earlier answer is recognized when the answer is applied again.

func TestSpawnReappliedPromptThatNormalizationChangedIsNotInsertedAgain(t *testing.T) {
	skills := map[string]string{"crw-dev": "---\nname: crw-dev\ndescription: d\n---\nBody\n"}
	for _, prompt := range []string{"First paragraph.\n\n\nSecond paragraph.", "Read $crw-dev first.\n\n\nThen go."} {
		var c spawnHookCase
		store := `{"roles":{"explorer":{"mode":"default","promptOverride":` + spawnHookRouteStringify(prompt) + `}}}`
		c.Env.Store = &store
		rig := spawnHookNewRig(t, skills, c)
		input := `{"agent_type":"explorer","message":"TASK: look around"}`
		first := RunSpawnAttachHook(spawnReapplyPayload(rig.ws, "call-1", input), rig.env)
		updated := spawnReapplyUpdated(t, first, input)
		second := RunSpawnAttachHook(spawnReapplyPayload(rig.ws, "call-1", updated), rig.env)
		got := spawnReapplyText(t, spawnReapplyUpdated(t, second, updated))
		head := strings.Fields(prompt)[0]
		if n := strings.Count(got, head); n != 1 {
			t.Fatalf("prompt %q: the head appears %d times after the second application:\n%s", prompt, n, got)
		}
		third := RunSpawnAttachHook(spawnReapplyPayload(rig.ws, "call-1", spawnReapplyUpdated(t, second, updated)), rig.env)
		if again := spawnReapplyText(t, spawnReapplyUpdated(t, third, spawnReapplyUpdated(t, second, updated))); again != got {
			t.Fatalf("a third application changed the message:\n%s\n%s", got, again)
		}
	}
}

// --- CRW-1121 d3: an input of the call that the hook did not answer is another dispatch, claimed or not.

func TestEvidenceAssignmentEditedInputBeforeTheClaimGetsItsOwnAssignment(t *testing.T) {
	r := newAssignedRig(t)
	first, _ := r.spawnCall("TASK: first\nCRW-WORKTREE: "+r.wt, "call-1")
	firstID := assignedID.FindStringSubmatch(first)
	if firstID == nil {
		t.Fatalf("first answer has no assignment: %q", first)
	}
	edited, out := r.spawnCall(strings.Replace(first, "TASK: first", "TASK: second", 1), "call-1")
	ids := assignedID.FindAllStringSubmatch(edited, -1)
	if strings.Contains(out, `"deny"`) || len(ids) != 1 || ids[0][1] == firstID[1] {
		t.Fatalf("an edited, unclaimed input shares the assignment %s: %v\n%s", firstID[1], ids, out)
	}
	// The earlier, unclaimed registration of the call is replaced by the edited input's: one open record is left, and it is the new one.
	records, _ := filepath.Glob(filepath.Join(r.cwd, ".crw", "evidence-assignments", "*", "*.json"))
	if len(records) != 1 || !strings.Contains(records[0], ids[0][1]) {
		t.Fatalf("records after the edit: %v", records)
	}
	// The edited input, answered, delivered again is that same event.
	if again, out := r.spawnCall(edited, "call-1"); strings.Contains(out, `"deny"`) || again != "" && again != edited {
		t.Fatalf("the edited input's answer again changed:\n%s", out)
	}
}

// --- CRW-1121 d2 and CRW-1124 d2: an event lock that cannot be had cannot let a later delivery overwrite a claimed record.

func TestEvidenceAssignmentWithoutAnEventLockDoesNotOverwriteAClaimedRecord(t *testing.T) {
	r := newAssignedRig(t)
	notDir := filepath.Join(t.TempDir(), "file")
	spawnHookMust(t, os.WriteFile(notDir, nil, 0o600))
	base := r.rig.env
	r.rig.env = func(key string) (string, bool) {
		if key == "TMPDIR" {
			return notDir, true
		}
		return base(key)
	}
	var mu sync.Mutex
	calls := 0
	reached, release := make(chan struct{}), make(chan struct{})
	spawnHookBeforePersist = func() {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 { // the delayed delivery: it looked the record up before the other one wrote it
			close(reached)
			<-release
		}
	}
	t.Cleanup(func() { spawnHookBeforePersist = func() {} })
	input := "TASK: first\nCRW-WORKTREE: " + r.wt
	var delayed struct{ msg, out string }
	var done sync.WaitGroup
	done.Add(1)
	go func() { defer done.Done(); delayed.msg, delayed.out = r.spawnCall(input, "same-call") }()
	<-reached
	first, firstOut := r.spawnCall(input, "same-call")
	id := assignedID.FindStringSubmatch(first)
	if id == nil {
		t.Fatalf("the first delivery has no assignment: %s", firstOut)
	}
	r.deliver("worker", first) // its child claims the record
	receipt := r.put(filepath.Join(r.wt, ".crw", "evidence", "receipt.txt"), "verified")
	if out := r.stop("worker", "t1", "EVIDENCE_RECORDED: "+receipt); out != "" {
		t.Fatalf("the delivered child was refused: %s", out)
	}
	records, _ := filepath.Glob(filepath.Join(r.cwd, ".crw", "evidence-assignments", "*", id[1]+".json"))
	if len(records) != 1 {
		t.Fatalf("records %v", records)
	}
	before, err := os.ReadFile(records[0])
	spawnHookMust(t, err)
	if !strings.Contains(string(before), `"status":"claimed"`) {
		t.Fatalf("the record is not claimed: %s", before)
	}
	close(release)
	done.Wait()
	after, err := os.ReadFile(records[0])
	spawnHookMust(t, err)
	if string(after) != string(before) {
		t.Fatalf("the delayed delivery overwrote the claimed record:\n%s\n%s", before, after)
	}
	if !assignedID.MatchString(delayed.msg) || assignedID.FindStringSubmatch(delayed.msg)[1] != id[1] {
		t.Fatalf("the delayed delivery answered with another assignment: %q", delayed.msg)
	}
}
