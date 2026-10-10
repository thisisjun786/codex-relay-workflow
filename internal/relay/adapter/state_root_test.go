package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/stateroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// nativeRPC answers every call as mcpRPC does, except that thread/read reports the thread at cwd.
type nativeRPC struct {
	*mcpRPC
	cwd string
}

func (r nativeRPC) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	raw, err := r.mcpRPC.Call(ctx, method, params)
	if method == "thread/read" {
		return json.Marshal(map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": r.cwd, "status": map[string]any{"type": "notLoaded"}}})
	}
	if method == "thread/resume" && err == nil {
		// The host runs the thread at the cwd it was asked to.
		return withCwd(raw, params["cwd"])
	}
	return raw, err
}

// withCwd is a resume answer reporting cwd as the cwd the thread runs at (none when cwd is not a
// string).
func withCwd(raw json.RawMessage, cwd any) (json.RawMessage, error) {
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	if text, ok := cwd.(string); ok {
		answer["cwd"] = text
	}
	return json.Marshal(answer)
}

// ranAtRPC is nativeRPC whose host runs a resumed thread at ranAt whatever cwd it was asked for.
type ranAtRPC struct {
	nativeRPC
	ranAt string
}

func (r ranAtRPC) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	raw, err := r.mcpRPC.Call(ctx, method, params)
	if method == "thread/read" || err != nil || method != "thread/resume" {
		return r.nativeRPC.Call(ctx, method, params)
	}
	return withCwd(raw, r.ranAt)
}

func recordAt(cwd string) *delivery.TaskSettings {
	record := childRecord(false, "")
	record.Data = delivery.Obj(contract.OrderedObject(record.Data).Set("cwd", cwd))
	return record
}

func threadInFlightAt(t *testing.T, root string) []byte {
	t.Helper()
	s := state.DefaultState("thread-1", "work")
	s.Phase, s.OrchestrationActive = state.PhaseP, true
	epoch := "epoch-1"
	s.PlanEpoch = &epoch
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(state.StatePath(root, "thread-1"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// CRW-1140 criterion 1 on relay delivery: a record whose cwd moved to B while the host reports the
// thread at A, whose PABCD work is in flight, is not delivered. The send fails before thread/resume
// with state_root_conflict; A's bytes are kept and B gets no state.
func TestARelayDeliveryDoesNotResumeInFlightWorkAtAnotherCwd(t *testing.T) {
	t.Setenv("CRW_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	before := threadInFlightAt(t, a)
	record := recordAt(b)
	rpc := &mcpRPC{}
	adapter := lostResumeAdapter(t, nativeRPC{rpc, a}, autoCompactChildPolicy(t, record))
	receipt := sendRecord(t, adapter, "send-moved", record)
	rpcError, _ := receipt["rpcError"].(map[string]any)
	if receipt["status"] != "not_attempted" || receipt["retrySafe"] != true || rpcError["code"] != "state_root_conflict" {
		t.Fatalf("receipt=%v", receipt)
	}
	if message, _ := rpcError["message"].(string); !strings.Contains(message, state.StatePath(a, "thread-1")) {
		t.Errorf("the refusal does not name the preserved state: %q", message)
	}
	if n := rpc.count("thread/resume") + rpc.count("turn/start"); n != 0 {
		t.Fatalf("the host was asked to resume or start a turn %d times", n)
	}
	if after, _ := os.ReadFile(state.StatePath(a, "thread-1")); string(after) != string(before) {
		t.Fatal("the native state changed")
	}
	if _, err := os.Stat(filepath.Join(b, ".crw")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the target cwd got a state directory: %v", err)
	}
}

// The record's cwd at the native root resumes as before.
func TestARelayDeliveryAtTheNativeCwdResumes(t *testing.T) {
	t.Setenv("CRW_HOME", t.TempDir())
	a := t.TempDir()
	threadInFlightAt(t, a)
	record := recordAt(a)
	rpc := &mcpRPC{}
	adapter := lostResumeAdapter(t, nativeRPC{rpc, a}, autoCompactChildPolicy(t, record))
	sendRecord(t, adapter, "send-native", record)
	if n := rpc.count("thread/resume"); n != 1 {
		t.Fatalf("thread/resume was called %d times", n)
	}
}

func anchorOf(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("CRW_HOME"), "state-roots", "thread-1.json"))
	if err != nil {
		return ""
	}
	var anchor struct{ NativeCwd string }
	if json.Unmarshal(raw, &anchor) != nil {
		t.Fatalf("anchor %s", raw)
	}
	return anchor.NativeCwd
}

// Review P1: the refusal happens before anything is sent, so it is recorded as not attempted and
// retry-safe. Once the conflict is resolved (the work at A reached IDLE) the same request ID is
// checked and resumed again instead of replaying the refusal.
func TestAStateRootRefusalIsRetrySafeWithTheSameRequestID(t *testing.T) {
	t.Setenv("CRW_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	threadInFlightAt(t, a)
	record := recordAt(b)
	rpc := &mcpRPC{}
	adapter := lostResumeAdapter(t, nativeRPC{rpc, a}, autoCompactChildPolicy(t, record))
	first := sendRecord(t, adapter, "send-retry", record)
	if first["status"] != "not_attempted" || first["retrySafe"] != true {
		t.Fatalf("receipt=%v", first)
	}
	if attempted, _ := first["attemptedEffects"].([]any); len(attempted) != 0 {
		t.Fatalf("attemptedEffects = %v", first["attemptedEffects"])
	}
	if err := state.WriteState(a, state.DefaultState("thread-1", "work")); err != nil {
		t.Fatal(err)
	}
	second := sendRecord(t, adapter, "send-retry", record)
	if second["status"] == "not_attempted" || rpc.count("thread/resume") != 1 {
		t.Fatalf("the retry did not resume: receipt=%v resumes=%d", second, rpc.count("thread/resume"))
	}
}

// Review P1: the thread was anchored at A, which holds its work in flight, and an external resume
// moved the cwd the host reports to B. The delivery, with a recorded cwd or settings-free, is
// judged against A.
func TestARelayDeliveryIsJudgedAgainstThePreservedAnchor(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(map[bool]string{false: "with-pair", true: "settings-free"}[free], func(t *testing.T) {
			t.Setenv("CRW_HOME", t.TempDir())
			a, b := t.TempDir(), t.TempDir()
			before := threadInFlightAt(t, a)
			if c := stateroot.Guard(os.LookupEnv, a, a, "thread-1"); c != nil {
				t.Fatal(c)
			}
			record := childRecord(free, "")
			record.Data = delivery.Obj(contract.OrderedObject(record.Data).Set("cwd", b))
			rpc := &mcpRPC{}
			adapter := lostResumeAdapter(t, nativeRPC{rpc, b}, autoCompactChildPolicy(t, record))
			receipt := sendRecord(t, adapter, "send-anchored", record)
			rpcError, _ := receipt["rpcError"].(map[string]any)
			if rpcError["code"] != "state_root_conflict" || rpc.count("thread/resume") != 0 {
				t.Fatalf("receipt=%v resumes=%d", receipt, rpc.count("thread/resume"))
			}
			if after, _ := os.ReadFile(state.StatePath(a, "thread-1")); string(after) != string(before) || anchorOf(t) != a {
				t.Fatalf("the native state or anchor changed (anchor %q)", anchorOf(t))
			}
		})
	}
}

// failingResumeRPC rejects thread/resume as the host would for an unknown rollout.
type failingResumeRPC struct{ nativeRPC }

func (r failingResumeRPC) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "thread/resume" {
		r.mcpRPC.Call(ctx, method, params)
		return nil, errors.New("no rollout found for thread id")
	}
	return r.nativeRPC.Call(ctx, method, params)
}

// Review P1: a resume the host rejects leaves the thread at its native root, so the delivery does
// not move the anchor to the cwd it asked for; one the host took does.
func TestTheAnchorFollowsADeliveryOnlyOnceTheHostTookIt(t *testing.T) {
	t.Setenv("CRW_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	if _, err := state.EnsureState(a, "thread-1"); err != nil {
		t.Fatal(err)
	}
	record := recordAt(b)
	rpc := &mcpRPC{}
	adapter := lostResumeAdapter(t, failingResumeRPC{nativeRPC{rpc, a}}, autoCompactChildPolicy(t, record))
	if receipt := sendRecord(t, adapter, "send-rejected", record); receipt["status"] == "accepted" || rpc.count("thread/resume") != 1 {
		t.Fatalf("receipt=%v", receipt)
	}
	if got := anchorOf(t); got != a {
		t.Fatalf("a rejected resume left the anchor at %q, want %q", got, a)
	}
	ok := lostResumeAdapter(t, nativeRPC{&mcpRPC{}, a}, autoCompactChildPolicy(t, record))
	// The scripted host answers the resume with its own cwd, which the delivery then reports as a
	// finding; the resume itself reached the host, which is what moves the anchor.
	sendRecord(t, ok, "send-taken", record)
	if got := anchorOf(t); got != b {
		t.Fatalf("anchor %q after the host took the resume, want %q", got, b)
	}
}

// d2: the anchor follows the cwd the host reported for the resumed thread, not the one the delivery
// asked for: a host that ran the thread at its native root leaves the anchor there, and the next
// SessionStart of a thread that begins work there is still judged against that root.
func TestTheAnchorFollowsTheCwdTheHostReported(t *testing.T) {
	for _, ranAtRoot := range []bool{true, false} {
		t.Run(map[bool]string{true: "ran at the native root", false: "ran elsewhere"}[ranAtRoot], func(t *testing.T) {
			t.Setenv("CRW_HOME", t.TempDir())
			a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
			if _, err := state.EnsureState(a, "thread-1"); err != nil {
				t.Fatal(err)
			}
			ranAt := c
			if ranAtRoot {
				ranAt = a
			}
			record := recordAt(b)
			adapter := lostResumeAdapter(t, ranAtRPC{nativeRPC{&mcpRPC{}, a}, ranAt}, autoCompactChildPolicy(t, record))
			sendRecord(t, adapter, "send-ran-at", record)
			if got := anchorOf(t); got != ranAt {
				t.Fatalf("anchor %q, want the cwd the host reported %q", got, ranAt)
			}
		})
	}
}

// d4: a resume the host took whose anchor cannot follow the thread to its new cwd starts no turn.
func TestARelayDeliveryWhoseAnchorCannotFollowTheResumeStartsNoTurn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Setenv("CRW_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	if _, err := state.EnsureState(a, "thread-1"); err != nil {
		t.Fatal(err)
	}
	if err := stateroot.Guard(os.LookupEnv, a, a, "thread-1"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(os.Getenv("CRW_HOME"), "state-roots")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	record := recordAt(b)
	rpc := &mcpRPC{}
	adapter := lostResumeAdapter(t, nativeRPC{rpc, a}, autoCompactChildPolicy(t, record))
	receipt := sendRecord(t, adapter, "send-unfollowed", record)
	rpcError, _ := receipt["rpcError"].(map[string]any)
	if receipt["status"] == "accepted" || rpcError["code"] != stateroot.AnchorCode || rpc.count("turn/start") != 0 {
		t.Fatalf("receipt=%v turns=%d", receipt, rpc.count("turn/start"))
	}
}

// d3: an anchor that cannot be trusted refuses the delivery before the resume, retry-safe.
func TestARelayDeliveryBesideAnUntrustworthyAnchorIsRefusedBeforeTheResume(t *testing.T) {
	t.Setenv("CRW_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	threadInFlightAt(t, a)
	if err := stateroot.Guard(os.LookupEnv, a, a, "thread-1"); err != nil {
		t.Fatal(err)
	}
	path := stateroot.AnchorPath(os.LookupEnv, "thread-1")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := recordAt(b)
	rpc := &mcpRPC{}
	adapter := lostResumeAdapter(t, nativeRPC{rpc, a}, autoCompactChildPolicy(t, record))
	receipt := sendRecord(t, adapter, "send-untrusted", record)
	rpcError, _ := receipt["rpcError"].(map[string]any)
	if receipt["status"] != "not_attempted" || receipt["retrySafe"] != true || rpcError["code"] != stateroot.AnchorUnreadableCode || rpc.count("thread/resume") != 0 {
		t.Fatalf("receipt=%v", receipt)
	}
}
