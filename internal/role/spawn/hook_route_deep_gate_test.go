package spawn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file is CRW-858's red-first test. In spawnHookRoute the final-gate prerequisite check
// (CheckFinalGatePrereqs) must run before the depth return, as the oracle's checkFinalGatePrereqs
// (subagent-config/src/spawn-attach-hook.ts:1074-1079) runs before the JSON.stringify at :1103 whose
// RangeError a tool_input nested too deep causes: the deny wins. The prefix of every new
// package-level name here is spawnHookDeepGate.

// spawnHookDeepGateJunk is n nested arrays as JSON text, built without the parser: an unrelated
// tool_input field whose only role is to make spawnHookRouteDeep true.
func spawnHookDeepGateJunk(n int) string {
	return strings.Repeat("[", n) + strings.Repeat("]", n)
}

// spawnHookDeepGatePlan writes the final-gate session state and goalplan the check reads, and, when
// receipt is not empty, the test receipt the goalplan names. It mirrors hook_leg_test.go's
// TestSpawnLegFinalGateDeny setup.
func spawnHookDeepGatePlan(t *testing.T, ws, receipt string) {
	t.Helper()
	spawnHookMust(t, os.MkdirAll(filepath.Join(ws, ".crw", "goalplans", "demo"), 0o755))
	spawnHookMust(t, os.MkdirAll(filepath.Join(ws, ".crw", "sessions"), 0o755))
	spawnHookMust(t, os.WriteFile(filepath.Join(ws, ".crw", "goalplans", "demo", "goalplan.json"),
		[]byte(`{"finalGate":{"testReceiptPath":".crw/evidence/test.json"},"criteria":[{"id":"c1","surface":"logic"}]}`), 0o644))
	spawnHookMust(t, os.WriteFile(filepath.Join(ws, ".crw", "sessions", "rec-s1.json"),
		[]byte(`{"sessionId":"rec-s1","slug":"demo"}`), 0o644))
	if receipt != "" {
		spawnHookMust(t, os.MkdirAll(filepath.Join(ws, ".crw", "evidence"), 0o755))
		spawnHookMust(t, os.WriteFile(filepath.Join(ws, ".crw", "evidence", "test.json"), []byte(receipt), 0o644))
	}
}

// spawnHookDeepGatePayload is the recorded final-gate payload of hook_leg_test.go:180-187 with an
// unrelated junk field appended; junk is JSON text, and empty leaves the field out.
func spawnHookDeepGatePayload(ws, message, junk string) string {
	field := ""
	if junk != "" {
		field = `,"junk":` + junk
	}
	return `{"hook_event_name":"PreToolUse","session_id":"rec-s1","cwd":` + strconv.Quote(ws) +
		`,"turn_id":"rec-t1","tool_name":"spawn_agent","tool_input":{"message":` + strconv.Quote(message) + field +
		`},"tool_use_id":"rec-call-1"}`
}

// spawnHookDeepGateOracleReason is the recorded oracle deny reason of
// hook__pre-tool-use-attaching-skills__final_gate_missing_test_receipt_denied with the name
// substitution applied (R23 [codexclaw -> [crw, R26 .codexclaw -> .crw), the expected answer of the
// red test.
func spawnHookDeepGateOracleReason(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contract", "fixtures", "cxc",
		"hook__pre-tool-use-attaching-skills__final_gate_missing_test_receipt_denied.json"))
	spawnHookMust(t, err)
	var fixture struct {
		Expect struct {
			Steps []struct {
				StdoutJSON struct {
					HookSpecificOutput struct {
						PermissionDecisionReason string `json:"permissionDecisionReason"`
					} `json:"hookSpecificOutput"`
				} `json:"stdout_json"`
			} `json:"steps"`
		} `json:"expect"`
	}
	spawnHookMust(t, json.Unmarshal(raw, &fixture))
	if len(fixture.Expect.Steps) == 0 {
		t.Fatal("the recorded fixture has no step")
	}
	return strings.NewReplacer("[codexclaw ", "[crw ", ".codexclaw/", ".crw/").Replace(
		fixture.Expect.Steps[0].StdoutJSON.HookSpecificOutput.PermissionDecisionReason)
}

// TestSpawnHookDeepGateRefusesBeforeTheDepthReturn: a final-gate-marked packet whose unrelated field
// is nested 5,000 levels (under the 4 MiB bound) answers the deny envelope, because the final-gate
// prerequisite check runs before the depth return. On the baseline the depth return wins and the hook
// prints nothing.
func TestSpawnHookDeepGateRefusesBeforeTheDepthReturn(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	spawnLegEnv(t, rig)
	spawnHookDeepGatePlan(t, rig.ws, "")
	payload := spawnHookDeepGatePayload(rig.ws, "[CRW-FINAL-GATE] please review the final gate", spawnHookDeepGateJunk(5000))
	if got, want := RunSpawnAttachHook(payload, rig.env), DenyEnvelope(spawnHookDeepGateOracleReason(t)); got != want {
		t.Fatalf("a deep final-gate packet:\n got %q\nwant %q", got, want)
	}
}

// TestSpawnHookDeepGateDeepWithoutMarkerPrintsNothing: the same deep input without the final-gate
// marker still answers nothing, the depth return the oracle's RangeError gives.
func TestSpawnHookDeepGateDeepWithoutMarkerPrintsNothing(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	spawnLegEnv(t, rig)
	spawnHookDeepGatePlan(t, rig.ws, "")
	payload := spawnHookDeepGatePayload(rig.ws, "please review the diff", spawnHookDeepGateJunk(5000))
	if got := RunSpawnAttachHook(payload, rig.env); got != "" {
		t.Errorf("a deep packet without the marker prints nothing, got %.120q", got)
	}
}

// TestSpawnHookDeepGatePassesWithTheReceipt: a final-gate packet whose receipt is present passes the
// gate. With the deep field the answer is the depth return (""), the same as the baseline; with a
// shallow input it is the ordinary allow envelope, never a deny.
func TestSpawnHookDeepGatePassesWithTheReceipt(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	spawnLegEnv(t, rig)
	// A receipt whose identity is unavailable matches a working tree with no git, as Compare reads:
	// an unavailable side is never "different", so the gate passes.
	spawnHookDeepGatePlan(t, rig.ws, `{"kind":"test","sourceIdentity":{"kind":"unavailable","commitSha":"","dirty":false}}`)
	deep := spawnHookDeepGatePayload(rig.ws, "[CRW-FINAL-GATE] please review the final gate", spawnHookDeepGateJunk(5000))
	if got := RunSpawnAttachHook(deep, rig.env); got != "" {
		t.Errorf("a deep final-gate packet with a receipt reaches the depth return, got %.120q", got)
	}
	shallow := spawnHookDeepGatePayload(rig.ws, "[CRW-FINAL-GATE] please review the final gate", "")
	if got := RunSpawnAttachHook(shallow, rig.env); got == "" || strings.Contains(got, `"permissionDecision":"deny"`) {
		t.Errorf("a final-gate packet with a receipt is allowed, got %.120q", got)
	}
}
