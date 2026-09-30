package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func set1Capture(t *testing.T, id string) (map[string]any, *store.Store) {
	t.Helper()
	root := t.TempDir()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("testdata/report_capture.py")
	if err != nil {
		t.Fatal(err)
	}
	// The store Python's fixture left is recorded with the answer (pythonTree).
	output := pythonTree(t, id, root, func() ([]byte, error) {
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, id, root)
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "CODEX_HOME="+root, "TMPDIR="+os.TempDir())
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("capture: %v: %s", err, output)
		}
		return output, nil
	})
	var want map[string]any
	if err := json.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	ownCopied(t, filepath.Join(root, "relay.sqlite3"), "go")
	s, err := store.Open(context.Background(), filepath.Join(root, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return want, s
}

type reportScriptHost struct {
	*sendHost
	script string
}

func (h *reportScriptHost) ListTurnIDs(string, int) ([]any, error) { return []any{}, nil }

func (h *reportScriptHost) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	if h.script == "busy" {
		return delivery.Obj{{Key: "requestId", Value: id}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "failed"}, {Key: "threadId", Value: thread}, {Key: "retrySafe", Value: false}, {Key: "error", Value: "thread/read: Thread is active; message withheld. Wait for completion."}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "thread_busy"}, {Key: "message", Value: "Thread is active"}}}}, nil
	}
	if h.script == "approval_policy" {
		return delivery.Obj{{Key: "requestId", Value: id}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "failed"}, {Key: "threadId", Value: thread}, {Key: "retrySafe", Value: false}, {Key: "resumed", Value: delivery.Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "error", Value: "thread/resume: Interactive approvals unsupported; message withheld."}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "unsupported_approval_policy"}, {Key: "message", Value: "unsupported"}}}}, nil
	}
	h.sends = append(h.sends, message)
	return delivery.Obj{{Key: "requestId", Value: id}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "accepted"}, {Key: "threadId", Value: thread}, {Key: "turnId", Value: "turn-" + thread + "-1"}, {Key: "resumed", Value: delivery.Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "retrySafe", Value: false}}, nil
}

func Test24_RC_13_LivePreview(t *testing.T) {
	want, s := set1Capture(t, "RC-13-preview")
	event := want["eventId"].(string)
	d := delivery.NewService(s, delivery.NewFakeClock())
	before, err := d.PreviewMessage(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	host := &reportScriptHost{sendHost: &sendHost{status: "idle"}}
	record, err := d.Attempt(context.Background(), event, host, nil, "")
	if err != nil {
		t.Fatalf("actual send: %v", err)
	}
	if record == nil {
		t.Fatal("actual send returned no attempt")
	}
	var request any
	for _, field := range record {
		if field.Key == "requestId" {
			request = field.Value
			break
		}
	}
	if request == nil {
		t.Fatalf("send lacks requestId: %v", record)
	}
	frozen, err := s.One(context.Background(), "SELECT message FROM attempt_messages WHERE request_id=?", request)
	if err != nil || frozen == nil {
		t.Fatalf("frozen attempt: %v %v", frozen, err)
	}
	after, err := d.PreviewMessage(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, map[string]any{"eventId": event, "requestId": request, "before": before, "sent": frozen.Get("message"), "after": after}, want)
}

func compareReportValues(t *testing.T, got, want any) {
	t.Helper()
	bytes, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err = json.Unmarshal(bytes, &normalized); err != nil {
		t.Fatal(err)
	}
	if jsonText(normalized) != jsonText(want) {
		t.Errorf("Go=%s\nPython=%s", jsonText(normalized), jsonText(want))
	}
}
