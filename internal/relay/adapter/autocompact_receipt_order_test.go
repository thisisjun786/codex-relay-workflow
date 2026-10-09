package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// CRW-1000 (verification round 1): the limit a resume carried is recorded when the request is about to go out, not before the connection is known to take it, and it is stored before the request leaves.

// closeAfterRole closes the connection the first time the send resolves the role of the recipient, which happens after thread/read answered and before the watch of the turn is admitted: the same
// order as a socket lost between the two.
type closeAfterRole struct {
	bridge.ExecutionPolicy
	once  sync.Once
	close func()
}

func (p *closeAfterRole) Role(name string) (execution.Role, bool) {
	role, ok := p.ExecutionPolicy.Role(name)
	p.once.Do(p.close)
	return role, ok
}

// A send whose connection was lost before thread/resume left records no limit as requested: the host saw no resume, and a receipt that says the limit was requested would be false.
func TestASendLostBeforeTheResumeLeftRecordsNoLimit(t *testing.T) {
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	defer client.Close()
	record := childRecord(false, "")
	policy := &closeAfterRole{ExecutionPolicy: autoCompactChildPolicy(t, record), close: func() { client.Close() }}
	a := lostResumeAdapter(t, client, policy)
	receipt := sendRecord(t, a, "send-lost-before-resume", record)
	if host.Count("thread/resume") != 0 {
		t.Fatalf("the injection did not keep the resume from the host: %d", host.Count("thread/resume"))
	}
	if value, present := autoCompactSentValue(t, receipt); present {
		t.Fatalf("no resume left, but the receipt records the limit %d as requested: %v", value, receipt)
	}
}

// receiptAtResume reads the stored receipt of the send at the moment thread/resume is called, then loses the answer.
type receiptAtResume struct {
	*mcpRPC
	a       *Adapter
	request string
	entered bool
	readErr error
	stored  map[string]any
}

func (r *receiptAtResume) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "thread/resume" {
		r.entered = true
		stored, err := r.a.GetOperation(context.Background(), r.request)
		r.readErr = err
		r.stored, _ = plain(stored).(map[string]any)
		return nil, errors.New("the answer was lost after the request went out")
	}
	return r.mcpRPC.Call(ctx, method, params)
}

// The record is stored before the request leaves, so a process that stops while it waits for the answer leaves the limit in the ledger.
func TestTheLimitOfAResumeIsStoredBeforeTheRequestGoesOut(t *testing.T) {
	record := childRecord(false, "")
	rpc := &receiptAtResume{mcpRPC: &mcpRPC{}, request: "send-stored-before-resume"}
	a := lostResumeAdapter(t, rpc, autoCompactChildPolicy(t, record))
	rpc.a = a
	receipt := sendRecord(t, a, rpc.request, record)
	if !rpc.entered || rpc.readErr != nil {
		t.Fatalf("the receipt could not be read at the resume: entered=%t err=%v", rpc.entered, rpc.readErr)
	}
	if _, present := autoCompactSentValue(t, receipt); !present {
		t.Fatalf("the final receipt lost the limit: %v", receipt)
	}
	if value, present := autoCompactSentValue(t, rpc.stored); !present || value != 550000 {
		t.Fatalf("the receipt stored when the resume went out has no limit: %v", rpc.stored)
	}
}
