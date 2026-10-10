package spawn

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// CRW-1118: a subagent's one-time recursion grant is checked first and spent only with an answer that lets the spawn run, so a
// spawn refused for another reason leaves it to the corrected retry; the spent grant is bound to the native call, so the same
// delivery is answered again and another call cannot use it.

// spawnGrantClaimRig mints one grant for the rig's workspace and session rec-s1 and returns the grant marker and its file.
func spawnGrantClaimRig(t *testing.T) (*spawnHookRig, string, string) {
	t.Helper()
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	scope := map[string]any{"cwd": rig.ws, "session_id": "rec-s1"}
	nonce, ok := MintRecursionGrant(scope, rig.tmp, time.Now())
	if !ok {
		t.Fatal("mint failed")
	}
	key, _ := spawnGrantKey(scope)
	return rig, spawnGrantMarker + nonce + "]", filepath.Join(rig.tmp, spawnGrantDirName(os.Getuid()), key, spawnGrantFile(nonce))
}

// spawnGrantClaimPayload is a spawn by the subagent child-1 with the native call id tool and message.
func spawnGrantClaimPayload(ws, tool, message string) string {
	return `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":` + strconv.Quote(ws) +
		`,"agent_id":"child-1","agent_type":"worker","tool_use_id":` + strconv.Quote(tool) + `,"tool_input":{"message":` + strconv.Quote(message) + `}}`
}

func spawnGrantClaimDenied(got string) bool {
	return strings.Contains(got, `"permissionDecision":"deny"`)
}

func TestSpawnHookRefusedSpawnKeepsTheGrantForTheRetry(t *testing.T) {
	rig, marker, file := spawnGrantClaimRig(t)
	spawnLegEnv(t, rig)
	// A managed marker with no dispatch record is refused.
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-1", "[CRW-DISPATCH:none:att-1]\nTASK: x "+marker), rig.env); !strings.Contains(got, "managed dispatch") {
		t.Fatalf("managed spawn without a record = %.200q", got)
	}
	// A final-gate packet without its receipts is refused.
	spawnHookDeepGatePlan(t, rig.ws, "")
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-2", "[CRW-FINAL-GATE] review "+marker), rig.env); !strings.Contains(got, "final gate") {
		t.Fatalf("final-gate spawn without receipts = %.200q", got)
	}
	if _, err := os.Lstat(file); err != nil {
		t.Fatalf("a refused spawn spent the grant: %v", err)
	}
	// The corrected retry uses the grant once.
	first := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-3", "TASK: the fixed work "+marker), rig.env)
	if spawnGrantClaimDenied(first) {
		t.Fatalf("the corrected retry = %.200q", first)
	}
	if _, err := os.Lstat(file); err == nil {
		t.Fatal("the allowed spawn left the grant unspent")
	}
	// The same delivery again gets the same answer; another call gets the recursion deny.
	if again := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-3", "TASK: the fixed work "+marker), rig.env); again != first {
		t.Fatalf("the same call again:\n got %.200q\nwant %.200q", again, first)
	}
	if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-4", "TASK: the fixed work "+marker), rig.env); got != DenyEnvelope(RecurseDenyReason) {
		t.Fatalf("another call with the spent grant = %.200q", got)
	}
}

// Two calls racing for one grant: exactly one is let through.
func TestSpawnHookGrantRaceAllowsOneCall(t *testing.T) {
	for round := 0; round < 20; round++ {
		rig, marker, _ := spawnGrantClaimRig(t)
		answers := make([]string, 2)
		var wg sync.WaitGroup
		for i := range answers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				answers[i] = RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "race-"+strconv.Itoa(i), "TASK: work "+marker), rig.env)
			}()
		}
		wg.Wait()
		if spawnGrantClaimDenied(answers[0]) == spawnGrantClaimDenied(answers[1]) {
			t.Fatalf("round %d: both or neither allowed: %.120q / %.120q", round, answers[0], answers[1])
		}
	}
}

// expiresAt must be a finite integer of milliseconds within the minting TTL.
func TestSpawnHookGrantExpiryMustBeAFiniteIntegerWithinTheTTL(t *testing.T) {
	now := time.Now().UnixMilli()
	for body, allowed := range map[string]bool{
		`{"expiresAt":1e999}`: false,
		`{"expiresAt":` + strconv.FormatInt(now+60000, 10) + `.5}`:                                     false,
		`{"expiresAt":` + strconv.FormatInt(now+int64(spawnGrantTTL/time.Millisecond)+60000, 10) + `}`: false,
		`{"expiresAt":` + strconv.FormatInt(now-1000, 10) + `}`:                                        false,
		`{"expiresAt":"` + strconv.FormatInt(now+60000, 10) + `"}`:                                     false,
		`{"expiresAt":` + strconv.FormatInt(now+60000, 10) + `}`:                                       true,
	} {
		rig, marker, file := spawnGrantClaimRig(t)
		spawnHookMust(t, os.WriteFile(file, []byte(body), 0o600))
		if got := RunSpawnAttachHook(spawnGrantClaimPayload(rig.ws, "call-1", "TASK: work "+marker), rig.env); spawnGrantClaimDenied(got) == allowed {
			t.Errorf("%s: answer %.120q, allowed %v", body, got, allowed)
		}
	}
}
