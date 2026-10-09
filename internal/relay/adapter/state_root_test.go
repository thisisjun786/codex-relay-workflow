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
	return raw, err
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
	if receipt["status"] != "failed" || rpcError["code"] != "state_root_conflict" {
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
