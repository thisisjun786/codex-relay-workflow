package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1122 d1 (post-evaluation verification of 4c55891f): a recorded answer is given again only under the binding it was issued with,
// and the final gate of a replay reads the packet text the first delivery read.

// --- the final gate of a replay reads the message before the items, as the first delivery does.

func TestSpawnReplayFinalGateReadsTheMessageBeforeItems(t *testing.T) {
	rig := spawnReapplyRig(t, "")
	payload := spawnReapplyPayload(rig.ws, "gate-call", `{"agent_type":"explorer","message":"[CRW-FINAL-GATE] review the final gate CRW-SUBSPAWN-ALLOWED","items":[{"type":"text","text":"unrelated"}]}`)
	first := RunSpawnAttachHook(payload, rig.env)
	if !strings.Contains(first, `"permissionDecision":"allow"`) || !strings.Contains(first, "[CRW-SUBSPAWN-GRANT:") {
		t.Fatalf("first answer = %.300q", first)
	}
	spawnHookDeepGatePlan(t, rig.ws, "") // the receipt the gate needs is missing now
	if got, want := RunSpawnAttachHook(payload, rig.env), DenyEnvelope(spawnHookDeepGateOracleReason(t)); got != want {
		t.Fatalf("the replay of a message gate packet that also has items:\n got %.300q\nwant %.300q", got, want)
	}
}

// A null message is no message, so the items are the packet in the replay too.
func TestSpawnReplayFinalGateReadsTheItemsOfANullMessage(t *testing.T) {
	rig := spawnReapplyRig(t, "")
	payload := spawnReapplyPayload(rig.ws, "gate-call", `{"agent_type":"explorer","message":null,"items":[{"type":"text","text":"[CRW-FINAL-GATE] review the final gate CRW-SUBSPAWN-ALLOWED"}]}`)
	first := RunSpawnAttachHook(payload, rig.env)
	if !strings.Contains(first, `"permissionDecision":"allow"`) || !strings.Contains(first, "[CRW-SUBSPAWN-GRANT:") {
		t.Fatalf("first answer = %.300q", first)
	}
	spawnHookDeepGatePlan(t, rig.ws, "")
	if got, want := RunSpawnAttachHook(payload, rig.env), DenyEnvelope(spawnHookDeepGateOracleReason(t)); got != want {
		t.Fatalf("the replay of an items gate packet:\n got %.300q\nwant %.300q", got, want)
	}
}

// --- a managed replay is given again only under the root, record, role, candidate and native call it was issued for.

func TestSpawnReplayRefusesAnotherPhysicalDispatchRoot(t *testing.T) {
	rig, ledger := spawnManagedDeepRig(t)
	payload := spawnR5ManagedPayload(rig.ws, "[CRW-DISPATCH:one:att-1]\nTASK: coordinate CRW-SUBSPAWN-ALLOWED")
	if first := RunSpawnAttachHook(payload, rig.env); !strings.Contains(first, `"permissionDecision":"allow"`) {
		t.Fatalf("first answer = %.300q", first)
	}
	// The working directory is moved away and a directory of the same name holds a fresh, unissued record of the same marker.
	spawnHookMust(t, os.Rename(rig.ws, rig.ws+"-old"))
	spawnHookMust(t, os.MkdirAll(filepath.Dir(ledger), 0o755))
	spawnHookMust(t, os.WriteFile(ledger, []byte(spawnManagedDeepLedger), 0o644))
	again := RunSpawnAttachHook(payload, rig.env)
	if !strings.Contains(again, `"permissionDecision":"deny"`) || !strings.Contains(again, "managed dispatch") {
		t.Fatalf("the replay under another physical dispatch root = %.300q", again)
	}
	if data, _ := os.ReadFile(ledger); string(data) != spawnManagedDeepLedger {
		t.Fatalf("the refused replay changed the new record: %s", data)
	}
}

func TestSpawnReplayRefusesAChangedCandidate(t *testing.T) {
	rig, ledger := spawnManagedDeepRig(t)
	payload := spawnR5ManagedPayload(rig.ws, "[CRW-DISPATCH:one:att-1]\nTASK: coordinate CRW-SUBSPAWN-ALLOWED")
	if first := RunSpawnAttachHook(payload, rig.env); !strings.Contains(first, `"permissionDecision":"allow"`) {
		t.Fatalf("first answer = %.300q", first)
	}
	data, err := os.ReadFile(ledger)
	spawnHookMust(t, err)
	spawnHookMust(t, os.WriteFile(ledger, []byte(strings.ReplaceAll(string(data), "rec/exec-primary", "rec/replacement")), 0o644))
	if again := RunSpawnAttachHook(payload, rig.env); !strings.Contains(again, `"permissionDecision":"deny"`) || !strings.Contains(again, "managed dispatch") {
		t.Fatalf("the replay after the attempt's candidate changed = %.300q", again)
	}
}

// The record still shows the attempt claimed and current, but no longer issued to this native call: the replay is refused.
func TestSpawnReplayRefusesAnAttemptNotIssuedToTheCall(t *testing.T) {
	rig, ledger := spawnManagedDeepRig(t)
	payload := spawnR5ManagedPayload(rig.ws, "[CRW-DISPATCH:one:att-1]\nTASK: coordinate CRW-SUBSPAWN-ALLOWED")
	first := RunSpawnAttachHook(payload, rig.env)
	if !strings.Contains(first, `"permissionDecision":"allow"`) {
		t.Fatalf("first answer = %.300q", first)
	}
	if again := RunSpawnAttachHook(payload, rig.env); again != first {
		t.Fatalf("the replay of the issued attempt:\n%.300q\n%.300q", again, first)
	}
	spawnHookMust(t, os.WriteFile(ledger, []byte(spawnManagedDeepLedger), 0o644)) // the same record, not issued
	if again := RunSpawnAttachHook(payload, rig.env); !strings.Contains(again, `"permissionDecision":"deny"`) || !strings.Contains(again, "managed dispatch") {
		t.Fatalf("the replay of an attempt not issued to the call = %.300q", again)
	}
}
