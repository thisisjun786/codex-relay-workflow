package supervisor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// set1Fixture restores the store a Python report test left (the former
// testdata/report_capture.py), holding the one event the test reported on, and opens it as Go's.
func set1Fixture(t *testing.T, id string) (*store.Store, string) {
	t.Helper()
	root := t.TempDir()
	treeFixture(t, id, root)
	ownCopied(t, filepath.Join(root, "relay.sqlite3"), "go")
	s, err := store.Open(context.Background(), filepath.Join(root, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
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
	s, root := set1Fixture(t, "RC-13-preview")
	var event string
	if err := s.DB.QueryRow("SELECT event_id FROM events").Scan(&event); err != nil {
		t.Fatal(err)
	}
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
	golden.CheckJSON(t, "preview", asJSON(t, map[string]any{"eventId": event, "requestId": request, "before": before, "sent": frozen.Get("message"), "after": after}), treeGolden(t, root)...)
}
