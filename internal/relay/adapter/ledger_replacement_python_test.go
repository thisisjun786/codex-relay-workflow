package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

type ledgerReplacementRPC struct {
	mu    sync.Mutex
	calls []string
}

func (r *ledgerReplacementRPC) Call(_ context.Context, method string, _ map[string]any) (json.RawMessage, error) {
	r.mu.Lock()
	r.calls = append(r.calls, method)
	r.mu.Unlock()
	switch method {
	case "thread/read":
		return json.Marshal(map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}})
	case "thread/resume":
		return json.Marshal(resume())
	case "turn/start":
		return json.Marshal(map[string]any{"turn": map[string]any{"id": "turn-after-replacement"}})
	}
	return nil, &HostUnavailable{"unexpected method"}
}

func Test28LedgerReplacementAfterGuardMatchesPython(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "socket")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	rpc := &ledgerReplacementRPC{calls: []string{}}
	a, err := Open(socket, root, Options{RPC: rpc})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := a.LedgerIdentityRecord(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	guard := func(ctx context.Context) (map[string]any, error) {
		if err := a.RequireLedger(ctx, identity); err != nil {
			return nil, err
		}
		path := identity["realPath"].(string)
		if err := os.Rename(path, path+".open"); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
			return nil, err
		}
		return nil, nil
	}
	receipt, sendErr := a.Send(context.Background(), "replacement-window", "thread", "message", &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}, guard, 1)
	rpc.mu.Lock()
	calls := append([]string{}, rpc.calls...)
	rpc.mu.Unlock()
	result := map[string]any{"returned": sendErr == nil, "status": field(receipt, "status"), "turnId": field(receipt, "turnId"), "calls": calls}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(map[string]any{"root": filepath.Join(root, "python"), "settings": authorized(), "resume": resume()})
	out := pyDriver(t, "ledger_replacement_capture.py", input)
	var want map[string]any
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(result)
	expected, _ := json.Marshal(want)
	if !bytes.Equal(actual, expected) {
		t.Fatalf("Go %s\nPython %s", actual, expected)
	}
}
