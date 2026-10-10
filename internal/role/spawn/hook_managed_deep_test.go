package spawn

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CRW-1122: a managed spawn whose tool_input nests past the depth the answer can be written at is denied before the issuance, where
// the oracle (and the port before) printed nothing, so the host ran the caller's input without the managed routing and the
// attempt was never recorded as issued.

const spawnManagedDeepLedger = `{"version":1,"sessionId":"rec-s1","id":"one","role":"executor","candidates":[{"model":"rec/exec-primary","effort":"high"}],"attempts":[{"id":"att-1","candidate":{"model":"rec/exec-primary","effort":"high"},"claimed":true,"agentId":null,"observedModel":null,"code":null,"taskFailure":null,"status":"claimed","reconciliation":null,"spawnIssued":false,"toolUseId":null}],"status":"active"}`

func spawnManagedDeepRig(t *testing.T) (*spawnHookRig, string) {
	t.Helper()
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	ledger := filepath.Join(rig.ws, ".crw", "dispatches", "rec-s1", "one.json")
	spawnHookMust(t, os.MkdirAll(filepath.Dir(ledger), 0o755))
	spawnHookMust(t, os.WriteFile(ledger, []byte(spawnManagedDeepLedger), 0o644))
	return rig, ledger
}

// spawnManagedDeepPayload is a root spawn of the claimed attempt with a conflicting caller model and effort and, when depth is above
// zero, an unrelated field of depth nested arrays.
func spawnManagedDeepPayload(ws, message string, depth int) string {
	junk := ""
	if depth > 0 {
		junk = `,"junk":` + strings.Repeat("[", depth) + strings.Repeat("]", depth)
	}
	return `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":` + strconv.Quote(ws) +
		`,"tool_use_id":"native-1","tool_input":{"agent_type":"executor","message":` + strconv.Quote(message) +
		`,"model":"caller/model","reasoning_effort":"low"` + junk + `}}`
}

func TestSpawnHookManagedDeepInputIsDeniedBeforeIssuance(t *testing.T) {
	const message = "[CRW-DISPATCH:one:att-1]\nTASK: locate the owner"
	// tool_input is level 1 and the junk's outermost array level 2, so depth nested arrays reach level depth+1.
	for _, depth := range []int{spawnHookRouteMaxDepth, spawnHookRouteMaxDepth + 1, 7000} {
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			rig, ledger := spawnManagedDeepRig(t)
			got := RunSpawnAttachHook(spawnManagedDeepPayload(rig.ws, message, depth), rig.env)
			if !strings.Contains(got, `"permissionDecision":"deny"`) || strings.Contains(got, "updatedInput") || !strings.Contains(got, "managed dispatch") {
				t.Fatalf("deep managed spawn = %.300q", got)
			}
			if data, _ := os.ReadFile(ledger); string(data) != spawnManagedDeepLedger {
				t.Fatalf("a denied spawn changed the ledger: %s", data)
			}
		})
	}
	// Just below the bound the packet carries the managed candidate and is issued once.
	rig, ledger := spawnManagedDeepRig(t)
	got := RunSpawnAttachHook(spawnManagedDeepPayload(rig.ws, message, spawnHookRouteMaxDepth-1), rig.env)
	if !strings.Contains(got, `"permissionDecision":"allow"`) || !strings.Contains(got, `"model":"rec/exec-primary"`) || !strings.Contains(got, `"reasoning_effort":"high"`) {
		t.Fatalf("managed spawn at the bound = %.300q", got)
	}
	if data, _ := os.ReadFile(ledger); !strings.Contains(string(data), `"spawnIssued": true`) && !strings.Contains(string(data), `"spawnIssued":true`) {
		t.Fatalf("the spawn at the bound was not issued: %s", data)
	}
}

// The final-gate refusal still comes first for a deep managed packet.
func TestSpawnHookManagedDeepInputFinalGateStillFirst(t *testing.T) {
	rig, ledger := spawnManagedDeepRig(t)
	spawnLegEnv(t, rig)
	spawnHookDeepGatePlan(t, rig.ws, "")
	got := RunSpawnAttachHook(spawnManagedDeepPayload(rig.ws, "[CRW-DISPATCH:one:att-1]\n[CRW-FINAL-GATE] review the final gate", 7000), rig.env)
	if want := DenyEnvelope(spawnHookDeepGateOracleReason(t)); got != want {
		t.Fatalf("deep managed final-gate packet:\n got %.300q\nwant %.300q", got, want)
	}
	if data, _ := os.ReadFile(ledger); string(data) != spawnManagedDeepLedger {
		t.Fatalf("the refused spawn changed the ledger: %s", data)
	}
}
