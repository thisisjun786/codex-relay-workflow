package spawn

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// CRW-1122 (pre-merge evaluation of 2be6b6f2): the recorded answer of an event is revalidated against the dispatch and the final gate
// before it is returned, and a broken stdout pipe ends the hook with the blocking status.

// --- CRW-1122 d1: the cached allow answer does not outlive the dispatch or the final gate it was given under.

func spawnR5ManagedPayload(ws, message string) string {
	return spawnManagedDeepPayload(ws, message, 0)
}

func TestSpawnReplayRevalidatesTheManagedDispatch(t *testing.T) {
	rig, ledger := spawnManagedDeepRig(t)
	payload := spawnR5ManagedPayload(rig.ws, "[CRW-DISPATCH:one:att-1]\nTASK: coordinate CRW-SUBSPAWN-ALLOWED")
	first := RunSpawnAttachHook(payload, rig.env)
	if !strings.Contains(first, `"permissionDecision":"allow"`) || !strings.Contains(first, "[CRW-SUBSPAWN-GRANT:") {
		t.Fatalf("first answer = %.300q", first)
	}
	// While the attempt is still the claimed one, the same event is answered with the same answer.
	if again := RunSpawnAttachHook(payload, rig.env); again != first {
		t.Fatalf("the same event again:\n%.300q\n%.300q", again, first)
	}
	// The input the event was answered with is the same event too.
	answered := spawnReapplyUpdated(t, first, "")
	answeredPayload := spawnR5ManagedPayload(rig.ws, "") // only its shape: replaced below
	answeredPayload = strings.Replace(answeredPayload, spawnR5Input(rig.ws), answered, 1)
	if again := RunSpawnAttachHook(answeredPayload, rig.env); again != first {
		t.Fatalf("the answered input again:\n%.300q\n%.300q", again, first)
	}
	// The dispatch is no longer active: neither delivery runs on the old answer.
	data, err := os.ReadFile(ledger)
	spawnHookMust(t, err)
	var doc map[string]any
	spawnHookMust(t, json.Unmarshal(data, &doc))
	doc["status"] = "superseded"
	data, err = json.Marshal(doc)
	spawnHookMust(t, err)
	spawnHookMust(t, os.WriteFile(ledger, data, 0o644))
	for name, p := range map[string]string{"original input": payload, "answered input": answeredPayload} {
		if got := RunSpawnAttachHook(p, rig.env); !strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "managed dispatch") {
			t.Fatalf("%s after the dispatch ended = %.300q", name, got)
		}
	}
}

// spawnR5Input is the tool_input spawnManagedDeepPayload writes for an empty message.
func spawnR5Input(ws string) string {
	payload := spawnManagedDeepPayload(ws, "", 0)
	return payload[strings.Index(payload, `"tool_input":`)+len(`"tool_input":`) : len(payload)-1]
}

func TestSpawnReplayRevalidatesTheFinalGate(t *testing.T) {
	rig := spawnReapplyRig(t, "")
	payload := spawnReapplyPayload(rig.ws, "gate-call", `{"agent_type":"explorer","message":"[CRW-FINAL-GATE] review the final gate CRW-SUBSPAWN-ALLOWED"}`)
	first := RunSpawnAttachHook(payload, rig.env)
	if !strings.Contains(first, `"permissionDecision":"allow"`) || !strings.Contains(first, "[CRW-SUBSPAWN-GRANT:") {
		t.Fatalf("first answer = %.300q", first)
	}
	// A gate whose receipt is missing now refuses the same event.
	spawnHookDeepGatePlan(t, rig.ws, "")
	if got, want := RunSpawnAttachHook(payload, rig.env), DenyEnvelope(spawnHookDeepGateOracleReason(t)); got != want {
		t.Fatalf("the same event after the gate changed:\n got %.300q\nwant %.300q", got, want)
	}
}

// --- CRW-1122 d2: a broken stdout pipe ends the hook with the blocking status, not with SIGPIPE.

func TestSpawnHookBrokenPipeEndsWithTheBlockingStatus(t *testing.T) {
	if os.Getenv("CRW_SPAWN_PIPE_HELPER") == "1" {
		rig := spawnHookNewRig(t, nil, spawnHookCase{})
		os.Exit(RunHook(context.Background(), strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"s","cwd":`+
			jsonQuote(rig.ws)+`,"agent_id":"child","tool_use_id":"c","tool_input":{"message":"TASK: x"}}`), os.Stdout, rig.env))
	}
	reader, writer, err := os.Pipe()
	spawnHookMust(t, err)
	spawnHookMust(t, reader.Close()) // nobody reads: the write meets a broken pipe
	cmd := exec.Command(os.Args[0], "-test.run=^TestSpawnHookBrokenPipeEndsWithTheBlockingStatus$")
	cmd.Env = append(os.Environ(), "CRW_SPAWN_PIPE_HELPER=1")
	cmd.Stdout = writer
	err = cmd.Run()
	writer.Close()
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("helper = %v, want exit status %d", err, SpawnHookOutputFailed)
	}
	if status, _ := exit.Sys().(syscall.WaitStatus); status.Signaled() || exit.ExitCode() != SpawnHookOutputFailed {
		t.Fatalf("the hook ended with %v (signaled %v), want exit status %d", exit, status.Signaled(), SpawnHookOutputFailed)
	}
}

func jsonQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
