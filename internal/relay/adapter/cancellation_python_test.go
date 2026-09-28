package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

type cancellationRPC struct {
	stage   string
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   []string
}

func (r *cancellationRPC) Call(ctx context.Context, method string, _ map[string]any) (json.RawMessage, error) {
	r.mu.Lock()
	r.calls = append(r.calls, method)
	r.mu.Unlock()
	if method == r.stage {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	switch method {
	case "thread/read":
		return json.Marshal(map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}})
	case "thread/resume":
		return json.Marshal(resume())
	case "turn/start":
		return json.Marshal(map[string]any{"turn": map[string]any{"id": "turn"}})
	}
	return nil, errors.New("unexpected method")
}

func Test28CallerCancellationStageParity(t *testing.T) {
	stages := []string{"before", "thread/resume", "turn/start"}
	got := make([]map[string]any, 0, len(stages))
	for _, stage := range stages {
		rpc := &cancellationRPC{stage: stage, entered: make(chan struct{}), release: make(chan struct{})}
		l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ops.sqlite3"), ledger.Options{Encode: encodeReceipt})
		if err != nil {
			t.Fatal(err)
		}
		a := New(Options{RPC: rpc, Ledger: l, Timeout: time.Second, CallerSlack: time.Second})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if stage == "before" {
			cancel()
		}
		done := make(chan error, 1)
		go func() {
			_, err := a.Send(ctx, "cancel-"+stage, "thread", "message", &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}, nil, 0)
			done <- err
		}()
		if stage != "before" {
			waitEdge(t, rpc.entered)
			cancel()
		}
		if err := waitEdge(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s caller error: %T %v", stage, err, err)
		}
		close(rpc.release)
		a.transport.pending.Wait()
		rpc.mu.Lock()
		calls := append([]string{}, rpc.calls...)
		rpc.mu.Unlock()
		var status any
		if receipt, err := l.Get(context.Background(), "cancel-"+stage); err == nil {
			status = receipt["status"]
		}
		got = append(got, map[string]any{"stage": stage, "calls": calls, "status": status})
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
	input, _ := json.Marshal(map[string]any{"settings": authorized(), "resume": resume()})
	repo, _ := filepath.Abs("../../..")
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/cancellation_capture.py"))
	cmd.Dir = repo
	cmd.Stdin = bytes.NewReader(input)
	want, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python cancellation oracle: %v\n%s", err, want)
	}
	var expected []map[string]any
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(got)
	canonical, _ := json.Marshal(expected)
	if !bytes.Equal(actual, canonical) {
		t.Fatalf("Go %s\nPython %s", actual, want)
	}
}
