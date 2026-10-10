package spawn

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// CRW-1121 with CRW-1115 (verification round 3 of the merge of dev at 28a99e86): the same native event delivered again gets the
// same answer and the same evidence assignment, whether or not it mints or spends a grant, and replacing the evidence block of an
// earlier answer that carried the role's prompt override keeps exactly one prompt.

// A second delivery of one native event with its original input reuses the event's assignment: the answers are the same, one
// record exists, and no open record without a child is left to refuse an unrelated native receipt at SubagentStop.
func TestEvidenceAssignmentRedeliveredEventReusesItsAssignment(t *testing.T) {
	r := newAssignedRig(t)
	records := func() []string {
		got, _ := filepath.Glob(filepath.Join(r.cwd, ".crw", "evidence-assignments", "*", "*.json"))
		return got
	}
	first, firstOut := r.spawnCall("TASK: first\nCRW-WORKTREE: "+r.wt, "same-call")
	second, secondOut := r.spawnCall("TASK: first\nCRW-WORKTREE: "+r.wt, "same-call")
	if firstOut != secondOut || first == "" {
		t.Fatalf("a redelivery of the same event got another answer:\n%s\n%s", firstOut, secondOut)
	}
	if got := records(); len(got) != 1 {
		t.Fatalf("a redelivery of the same event registered another assignment: %v", got)
	}
	// The input the event answered with, delivered again, is the same event too: allowed as it is ("") or answered the same.
	if again, out := r.spawnCall(first, "same-call"); strings.Contains(out, `"deny"`) || again != "" && again != first || len(records()) != 1 {
		t.Fatalf("the event's own answer delivered again changed it (records %v):\n%s", records(), out)
	}
	r.deliver("worker", second)
	receipt := r.put(filepath.Join(r.wt, ".crw", "evidence", "receipt.txt"), "verified")
	if out := r.stop("worker", "t1", "EVIDENCE_RECORDED: "+receipt); out != "" {
		t.Fatalf("the delivered child was refused: %s", out)
	}
	native := r.put(filepath.Join(r.cwd, ".crw", "evidence", "native.txt"), "verified native")
	if out := r.stop("unrelated", "t2", "EVIDENCE_RECORDED: "+native); strings.Contains(out, `"decision":"block"`) {
		t.Fatalf("an open record left by the redelivery blocks an unrelated native receipt: %s", out)
	}
}

// The evidence block of an earlier answer that carried the prompt override is replaced by this call's own block, and the prompt
// stays once, right after the guard, with the new block behind it: for a copy behind the same guard, behind a guard the hook
// replaces, and for a call without a tool_use_id, in the message and the items form.
func TestEvidenceAssignmentReplacedBlockKeepsOnePrompt(t *testing.T) {
	const prompt = "Follow this exact instruction."
	for _, form := range []struct {
		name string
		wrap func(text string) string
	}{
		{"v1 message", func(s string) string { return `{"agent_type":"explorer","message":` + spawnHookRouteStringify(s) + `}` }},
		{"v1 items", func(s string) string {
			return `{"agent_type":"explorer","items":[{"type":"text","text":` + spawnHookRouteStringify(s) + `}]}`
		}},
		{"v2 message", func(s string) string {
			return `{"agent_type":"explorer","task_name":"t","fork_turns":"none","message":` + spawnHookRouteStringify(s) + `}`
		}},
	} {
		for _, c := range []struct {
			name, tool string
			guard      bool // swap the guard for another one the hook writes
		}{
			{"same guard, another call", "call-B", false},
			{"replaced guard, another call", "call-B", true},
			{"same guard, no tool_use_id", "", false},
		} {
			t.Run(form.name+"/"+c.name, func(t *testing.T) {
				rig := spawnReapplyRig(t, prompt)
				wt := t.TempDir()
				input := form.wrap("TASK: first\nCRW-WORKTREE: " + wt)
				first := spawnReapplyText(t, spawnReapplyUpdated(t, RunSpawnAttachHook(spawnReapplyPayload(rig.ws, "call-A", input), rig.env), input))
				firstID := assignedID.FindStringSubmatch(first)
				if firstID == nil || strings.Count(first, prompt) != 1 {
					t.Fatalf("first answer: %q", first)
				}
				copied := first
				if c.guard {
					guard, other := V1ScopeBlock, LeafGuardBlock
					if !strings.HasPrefix(first, guard) {
						guard, other = other, guard
					}
					copied = other + strings.TrimPrefix(first, guard)
				}
				input2 := form.wrap(copied)
				payload := spawnReapplyPayload(rig.ws, c.tool, input2)
				if c.tool == "" {
					payload = `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":` + strconv.Quote(rig.ws) +
						`,"tool_input":` + input2 + `}`
				}
				answer := RunSpawnAttachHook(payload, rig.env)
				if strings.Contains(answer, `"deny"`) {
					t.Fatalf("the copy was refused: %s", answer)
				}
				text := spawnReapplyText(t, spawnReapplyUpdated(t, answer, input2))
				if n := strings.Count(text, prompt); n != 1 {
					t.Fatalf("the copy has %d prompts, want 1:\n%s", n, text)
				}
				ids := assignedID.FindAllStringSubmatch(text, -1)
				if len(ids) != 1 || ids[0][1] == firstID[1] {
					t.Fatalf("the copy's assignment blocks %v, want one of its own:\n%s", ids, text)
				}
				rest, _ := spawnHookOwnedGuard(text)
				if !strings.HasPrefix(rest, prompt+"\n\n"+EvidenceAssignmentMarker) {
					t.Fatalf("the prompt does not follow the guard with the block behind it:\n%s", text)
				}
			})
		}
	}
}

// Two deliveries of one event at once are serialized by the event's lock: one registers the assignment and the other reuses it.
func TestEvidenceAssignmentConcurrentDeliveriesShareOneRecord(t *testing.T) {
	r := newAssignedRig(t)
	payload, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "spawn_agent", "session_id": "s1", "cwd": r.cwd,
		"tool_use_id": "same-call", "tool_input": map[string]any{"agent_type": "worker", "message": "TASK: first\nCRW-WORKTREE: " + r.wt}})
	spawnHookMust(t, err)
	answers := make([]string, 4)
	var wg sync.WaitGroup
	for i := range answers {
		wg.Go(func() { answers[i] = RunSpawnAttachHook(string(payload), r.rig.env) })
	}
	wg.Wait()
	for _, answer := range answers {
		if answer != answers[0] || !assignedID.MatchString(answer) {
			t.Fatalf("concurrent deliveries of one event got different answers:\n%s", strings.Join(answers, "\n"))
		}
	}
	if records, _ := filepath.Glob(filepath.Join(r.cwd, ".crw", "evidence-assignments", "*", "*.json")); len(records) != 1 {
		t.Fatalf("records: %v", records)
	}
}

// The input the event answered with, delivered again after its child claimed the assignment, is still that event: it reuses the
// claimed record instead of registering a dispatch no child will claim, so an unrelated native receipt still passes. The same call
// with another input is another dispatch and gets an assignment of its own (verification round 4).
func TestEvidenceAssignmentOwnAnswerRedeliveredAfterClaimKeepsOneAssignment(t *testing.T) {
	r := newAssignedRig(t)
	records := func() []string {
		got, _ := filepath.Glob(filepath.Join(r.cwd, ".crw", "evidence-assignments", "*", "*.json"))
		return got
	}
	first, _ := r.spawnCall("TASK: first\nCRW-WORKTREE: "+r.wt, "same-call")
	firstID := assignedID.FindStringSubmatch(first)
	if firstID == nil {
		t.Fatalf("first answer has no assignment: %q", first)
	}
	r.deliver("worker", first)
	receipt := r.put(filepath.Join(r.wt, ".crw", "evidence", "receipt.txt"), "verified")
	if out := r.stop("worker", "t1", "EVIDENCE_RECORDED: "+receipt); out != "" {
		t.Fatalf("the delivered child was refused: %s", out)
	}
	if again, out := r.spawnCall(first, "same-call"); strings.Contains(out, `"deny"`) || again != "" && again != first {
		t.Fatalf("the event's own answer delivered again after the claim changed it:\n%s", out)
	}
	if got := records(); len(got) != 1 {
		t.Fatalf("the event's own answer delivered again after the claim registered another assignment: %v", got)
	}
	native := r.put(filepath.Join(r.cwd, ".crw", "evidence", "native.txt"), "verified native")
	if out := r.stop("unrelated", "t2", "EVIDENCE_RECORDED: "+native); strings.Contains(out, `"decision":"block"`) {
		t.Fatalf("an open record left by the redelivery blocks an unrelated native receipt: %s", out)
	}
	edited, out := r.spawnCall(strings.Replace(first, "TASK: first", "TASK: second", 1), "same-call")
	ids := assignedID.FindAllStringSubmatch(edited, -1)
	if strings.Contains(out, `"deny"`) || len(ids) != 1 || ids[0][1] == firstID[1] {
		t.Fatalf("another input of the call shares the claimed assignment %s: %v\n%s", firstID[1], ids, out)
	}
}
