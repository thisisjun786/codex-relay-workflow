package bridge

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// pythonRound3 is testdata/python_round3.json: caller-visible bytes recorded from the real
// Python bridge by testdata/gen_round3.py against the Python suite's own FakeServer.
func pythonRound3(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/python_round3.json")
	if err != nil {
		t.Fatal(err)
	}
	var recorded map[string]any
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// jsonShaped is v as a caller receives it: encoded and decoded again.
func jsonShaped(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameJSON(t *testing.T, name string, got, want any) {
	t.Helper()
	if g, w := jsonShaped(t, got), jsonShaped(t, want); !reflect.DeepEqual(g, w) {
		gotRaw, _ := json.MarshalIndent(g, "", " ")
		wantRaw, _ := json.MarshalIndent(w, "", " ")
		t.Fatalf("%s differs from Python\n got: %s\nwant: %s", name, gotRaw, wantRaw)
	}
}

func receiptKeys(receipt map[string]any) []any {
	keys := []any{}
	for key := range receipt {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].(string) < keys[j].(string) })
	return keys
}

func Test_round3_a_busy_thread_answers_with_the_python_code_and_steer_guidance(t *testing.T) {
	want := object(pythonRound3(t)["busy"])
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}})
	receipt, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "busy", ThreadID: "thread-1", Message: "hi", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, "rpcError", receipt["rpcError"], want["rpcError"])
	sameJSON(t, "error", receipt["error"], want["error"])
}

func Test_round3_a_creation_that_came_back_different_names_the_field_and_both_values(t *testing.T) {
	recorded := pythonRound3(t)
	for name, change := range map[string]map[string]any{
		"create_model_mismatch":   {"model": "other"},
		"create_sandbox_mismatch": {"sandbox": map[string]any{"type": "dangerFullAccess"}},
	} {
		t.Run(name, func(t *testing.T) {
			b, host := testBridge(t)
			cwd := t.TempDir()
			start := startReply(cwd)
			for key, value := range change {
				start.Result[key] = value
			}
			host.Respond("thread/start", start)
			input := createInput(cwd, "mm")
			input.Prompt = "p"
			receipt, err := b.CreateThread(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			want := object(recorded[name])
			sameJSON(t, "rpcError", receipt["rpcError"], want["rpcError"])
			sameJSON(t, "error", receipt["error"], want["error"])
		})
	}
}

func Test_round3_a_worktree_creation_that_came_back_different_names_both_values(t *testing.T) {
	recorded := pythonRound3(t)
	for name, change := range map[string]map[string]any{
		"worktree_model_mismatch":   {"model": "other"},
		"worktree_sandbox_mismatch": {"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{"/w"}}},
	} {
		t.Run(name, func(t *testing.T) {
			b, host := testBridge(t)
			input := worktreeInput(t)
			input.Prompt = "p"
			start := worktreeStart(input.Destination)
			for key, value := range change {
				start[key] = value
			}
			host.Respond("thread/start", fakehost.Reply{Result: start})
			receipt, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			want := object(recorded[name])
			if receipt["status"] != want["status"] || receipt["error"] != want["error"] {
				t.Fatalf("status=%v error=%q\nwant %v %q", receipt["status"], receipt["error"], want["status"], want["error"])
			}
		})
	}
}

func Test_round3_a_send_receipt_reports_the_requests_its_dispatch_met(t *testing.T) {
	want := object(pythonRound3(t)["approvalRequests"])
	b, host, input := interactiveSend(t)
	input.Expected["approval_policy"] = "on-request"
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-2"}}, ServerRequests: []string{"item/commandExecution/requestApproval"}})
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	got := object(jsonShaped(t, receipt["approvalRequests"]))
	if got == nil {
		t.Fatalf("receipt has no approvalRequests: %v", receipt)
	}
	for _, entry := range got["thisThread"].([]any) {
		if _, ok := object(entry)["at"].(float64); !ok {
			t.Fatalf("entry has no numeric at: %v", entry)
		}
		delete(object(entry), "at")
	}
	sameJSON(t, "approvalRequests", got, want)
}

func Test_round3_capabilities_are_the_python_document(t *testing.T) {
	want := object(pythonRound3(t)["capabilities"])
	report, _ := capabilityReport(t)
	report["socket"] = "<SOCKET>"
	sameJSON(t, "get_capabilities", report, want)
}

func Test_round3_a_worktree_receipt_carries_the_python_fields(t *testing.T) {
	recorded := pythonRound3(t)
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.Prompt = "p"
	worktreeHost(host, input.Destination)
	// The Python fake answers the post-dispatch thread/read, so the receipt carries its annotation.
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": input.Destination, "model": "explicit-model", "reasoningEffort": "high"}}})
	accepted, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || accepted["status"] != "accepted" {
		t.Fatalf("accepted=%v err=%v", accepted, err)
	}
	sameJSON(t, "accepted keys", receiptKeys(accepted), recorded["worktree_accepted_keys"])
	if accepted["recovery"] != object(recorded["dispatching_row"])["recovery"] {
		t.Fatalf("recovery=%q", accepted["recovery"])
	}
	taken := worktreeInput(t)
	taken.RequestID = "taken"
	if err := os.Mkdir(taken.Destination, 0700); err != nil {
		t.Fatal(err)
	}
	failed, err := b.CreateWorktreeThread(context.Background(), taken)
	if err != nil {
		t.Fatal(err)
	}
	want := object(recorded["worktree_validation_failure"])
	sameJSON(t, "validation failure keys", receiptKeys(failed), want["keys"])
	association := object(failed["desktopProjectAssociation"])
	if len(association) != 2 || association["status"] != "unverified" || association["sourceRepository"] != taken.Source {
		t.Fatalf("desktopProjectAssociation=%v", association)
	}
}
