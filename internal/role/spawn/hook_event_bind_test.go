package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-1121 and CRW-1118 (verification round 1): a hook event is one unit. Deliveries of one root event that race mint one grant; a
// subagent's spent grant is bound to the input of the call that spent it and to the answer it got, so the same call again gets that
// answer, with the input it was answered with as well, and a different input under the same call id gets nothing.

// Two deliveries of one subagent call racing for its grant: one spends it, and both get the same answer.
func TestSpawnHookConcurrentDeliveriesOfOneSubagentCallGetOneAnswer(t *testing.T) {
	for round := 0; round < 10; round++ {
		rig, marker, _ := spawnGrantClaimRig(t)
		payload := spawnGrantClaimPayload(rig.ws, "call-1", "TASK: work "+marker)
		answers := make([]string, 2)
		var wg sync.WaitGroup
		for i := range answers {
			wg.Add(1)
			go func() { defer wg.Done(); answers[i] = RunSpawnAttachHook(payload, rig.env) }()
		}
		wg.Wait()
		if spawnGrantClaimDenied(answers[0]) || answers[0] != answers[1] {
			t.Fatalf("round %d: %.200q / %.200q", round, answers[0], answers[1])
		}
	}
}

// The grant a call spent authorizes that call's input and nothing else under the same call id.
func TestSpawnHookSpentGrantBindsTheInputOfItsCall(t *testing.T) {
	rig, marker, _ := spawnGrantClaimRig(t)
	first := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-1", "TASK: original "+marker), rig.env)
	if spawnGrantClaimDenied(first) {
		t.Fatalf("the spending call = %.200q", first)
	}
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-1", "TASK: entirely different child "+marker), rig.env); got != DenyEnvelope(RecurseDenyReason) {
		t.Fatalf("a changed input under the same call id = %.200q", got)
	}
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-1", "TASK: original "+marker), rig.env); got != first {
		t.Fatalf("the same input again:\n got %.200q\nwant %.200q", got, first)
	}
}

// The same event again gets the answer it got, whatever the settings say now.
func TestSpawnHookSpentGrantReplaysTheSavedAnswer(t *testing.T) {
	rig, marker, _ := spawnGrantClaimRig(t)
	payload := spawnGrantClaimPayload(rig.ws, "call-1", "TASK: original "+marker)
	first := RunSpawnAttachHook(payload, rig.env)
	home, _ := rig.env("CRW_HOME")
	spawnHookMust(t, os.WriteFile(filepath.Join(home, "subagents.json"),
		[]byte(`{"roles":{"explorer":{"mode":"model","model":"changed/model","promptOverride":"Changed instructions"}}}`), 0o600))
	if second := RunSpawnAttachHook(payload, rig.env); second != first {
		t.Fatalf("the same event after a settings edit:\n got %.300q\nwant %.300q", second, first)
	}
}

// The input a subagent's answer replaced it with, applied again under the same call id, is answered the same way: its grant marker is
// gone from it, and it is still the spend of the same grant.
func TestSpawnHookSpentGrantReplaysTheAnsweredInput(t *testing.T) {
	rig, marker, _ := spawnGrantClaimRig(t)
	first := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-1", "TASK: original "+marker), rig.env)
	updated := spawnReapplyUpdated(t, first, "")
	again := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":` + spawnHookQuote(rig.ws) +
		`,"agent_id":"child-1","agent_type":"worker","tool_use_id":"call-1","tool_input":` + updated + `}`
	if second := RunSpawnAttachHook(again, rig.env); second != first {
		t.Fatalf("the answered input again:\n got %.300q\nwant %.300q", second, first)
	}
	// Another call carrying that input has no grant to spend.
	other := strings.Replace(again, `"call-1"`, `"call-2"`, 1)
	if got := RunSpawnAttachHook(other, rig.env); got != DenyEnvelope(RecurseDenyReason) {
		t.Fatalf("another call with the answered input = %.200q", got)
	}
}

// A call that stopped holding the grant's reservation finishes only with the input it reserved it for.
func TestSpawnHookReservationBindsTheInputOfItsCall(t *testing.T) {
	rig, marker, file := spawnGrantClaimRig(t)
	tag, _ := spawnGrantTag("call-a")
	spawnHookMust(t, os.Rename(file, file+".reserved-"+tag))
	reserved := spawnHookRouteStringify(pyjson.Object{{Key: "message", Value: "TASK: work " + marker}})
	// A reservation with no recorded input is not usable: nothing says what it was reserved for.
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-a", "TASK: work "+marker), rig.env); got != DenyEnvelope(RecurseDenyReason) {
		t.Fatalf("a reservation without a recorded input = %.200q", got)
	}
	spawnHookMust(t, os.WriteFile(file+".input-"+tag, []byte(reserved), 0o600))
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-b", "TASK: work "+marker), rig.env); got != DenyEnvelope(RecurseDenyReason) {
		t.Fatalf("another call with a stale reservation = %.200q", got)
	}
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-a", "TASK: other work "+marker), rig.env); got != DenyEnvelope(RecurseDenyReason) {
		t.Fatalf("the reserving call with another input = %.200q", got)
	}
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-a", "TASK: work "+marker), rig.env); spawnGrantClaimDenied(got) {
		t.Fatalf("the reserving call with the reserved input = %.200q", got)
	}
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-b", "TASK: work "+marker), rig.env); got != DenyEnvelope(RecurseDenyReason) {
		t.Fatalf("another call after the commit = %.200q", got)
	}
	if _, err := os.Lstat(file); err == nil {
		t.Fatal("the spent grant came back")
	}
}

// A subagent never reads the record of a root event: that answer carries the grant the root event minted.
func TestSpawnHookSubagentNeverReadsARootEventsAnswer(t *testing.T) {
	rig := spawnReapplyRig(t, "")
	input := `{"agent_type":"explorer","message":"CRW-SUBSPAWN-ALLOWED coordinate"}`
	root := RunSpawnAttachHook(spawnReapplyPayload(rig.ws, "same-call", input), rig.env)
	if !strings.Contains(root, "[CRW-SUBSPAWN-GRANT:") {
		t.Fatalf("the root event minted no grant: %.200q", root)
	}
	child := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":` + spawnHookQuote(rig.ws) +
		`,"agent_id":"child-1","agent_type":"worker","tool_use_id":"same-call","tool_input":` + input + `}`
	if got := RunSpawnAttachHook(child, rig.env); got != DenyEnvelope(RecurseDenyReason) {
		t.Fatalf("a subagent with the root event's id and input = %.200q", got)
	}
}
