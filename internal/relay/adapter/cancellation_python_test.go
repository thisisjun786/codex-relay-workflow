package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// cancellationStage names what is suspended when the caller cancels: a host call, or the
// pre-start guard. busy makes thread/read answer active, the refusal Python never records
// once its caller has cancelled.
type cancellationStage struct {
	name, held string
	busy       bool
}

var cancellationStages = []cancellationStage{
	{name: "before"},
	{name: "thread/read", held: "thread/read"},
	{name: "thread/read (active)", held: "thread/read", busy: true},
	{name: "thread/resume", held: "thread/resume"},
	{name: "guard", held: "guard"},
	{name: "turn/start", held: "turn/start"},
}

type cancellationRPC struct {
	cancellationStage
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   []string
}

// hold is the suspended call. Its answer arrives only after the caller has cancelled.
func (r *cancellationRPC) hold(ctx context.Context) error {
	close(r.entered)
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *cancellationRPC) Call(ctx context.Context, method string, _ map[string]any) (json.RawMessage, error) {
	r.mu.Lock()
	r.calls = append(r.calls, method)
	r.mu.Unlock()
	if method == r.held {
		if err := r.hold(ctx); err != nil {
			return nil, err
		}
	}
	switch method {
	case "thread/read":
		status := "idle"
		if r.busy {
			status = "active"
		}
		return json.Marshal(map[string]any{"thread": map[string]any{"status": map[string]any{"type": status}}})
	case "thread/resume":
		return json.Marshal(resume())
	case "turn/start":
		return json.Marshal(map[string]any{"turn": map[string]any{"id": "turn"}})
	}
	return nil, errors.New("unexpected method")
}

// heldCaller is a caller whose cancellation shows at once through Done and Err, but reaches
// the contexts derived from it only when propagate runs. That is the gap between a cancel and
// the goroutine context.AfterFunc starts for it, held open for as long as the test needs.
type heldCaller struct {
	context.Context
	done chan struct{}
	mu   sync.Mutex
	err  error
	next int
	held map[int]func()
}

func newHeldCaller() *heldCaller {
	return &heldCaller{Context: context.Background(), done: make(chan struct{}), held: map[int]func(){}}
}
func (c *heldCaller) Done() <-chan struct{} { return c.done }
func (c *heldCaller) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
func (c *heldCaller) cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = context.Canceled
		close(c.done)
	}
}

// AfterFunc is how the context package registers what a cancellation propagates to.
func (c *heldCaller) AfterFunc(f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.next
	c.next++
	c.held[id] = f
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, pending := c.held[id]
		delete(c.held, id)
		return pending
	}
}
func (c *heldCaller) propagate() {
	c.mu.Lock()
	held := c.held
	c.held = map[int]func(){}
	c.mu.Unlock()
	for _, f := range held {
		f()
	}
}

// cancellationRows cancels the caller while each stage is suspended and releases that stage's
// answer only after the caller has been answered. With prompt propagation the cancellation
// usually reaches the call first; with held propagation the answer always lands first. Python
// takes the cancellation either way, so both must equal the oracle.
func cancellationRows(t *testing.T, held bool) []map[string]any {
	t.Helper()
	got := make([]map[string]any, 0, len(cancellationStages))
	for _, stage := range cancellationStages {
		rpc := &cancellationRPC{cancellationStage: stage, entered: make(chan struct{}), release: make(chan struct{})}
		l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ops.sqlite3"), ledger.Options{Encode: encodeReceipt})
		if err != nil {
			t.Fatal(err)
		}
		a := New(Options{RPC: rpc, Ledger: l, Timeout: time.Second, CallerSlack: time.Second})
		var ctx context.Context
		var cancel func()
		propagate := func() {}
		if held {
			caller := newHeldCaller()
			ctx, cancel, propagate = caller, caller.cancel, caller.propagate
		} else {
			ctx, cancel = context.WithCancel(context.Background())
		}
		var guard Guard
		guardBudget := 0
		if stage.held == "guard" {
			guardBudget = 1
			guard = func(ctx context.Context) (map[string]any, error) {
				if err := rpc.hold(ctx); err != nil {
					return nil, err
				}
				return map[string]any{"code": "managed_paused", "message": "paused"}, nil
			}
		}
		if stage.name == "before" {
			cancel()
		}
		done := make(chan error, 1)
		go func() {
			_, err := a.Send(ctx, "cancel", "thread", "message", &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}, guard, guardBudget)
			done <- err
		}()
		if stage.name != "before" {
			waitEdge(t, rpc.entered)
			cancel()
		}
		if err := waitEdge(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s caller error: %T %v", stage.name, err, err)
		}
		close(rpc.release)
		a.transport.pending.Wait()
		propagate()
		cancel()
		rpc.mu.Lock()
		calls := append([]string{}, rpc.calls...)
		rpc.mu.Unlock()
		var status any
		if receipt, err := l.Get(context.Background(), "cancel"); err == nil {
			status = receipt["status"]
		}
		got = append(got, map[string]any{"stage": stage.name, "calls": calls, "status": status})
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

func Test28CallerCancellationStageParity(t *testing.T) {
	prompt := cancellationRows(t, false)
	held := cancellationRows(t, true)
	stages := make([]map[string]any, len(cancellationStages))
	for i, stage := range cancellationStages {
		stages[i] = map[string]any{"name": stage.name, "held": stage.held, "busy": stage.busy}
	}
	input, _ := json.Marshal(map[string]any{"settings": authorized(), "resume": resume(), "stages": stages})
	want := pyDriver(t, "cancellation_capture.py", input)
	var expected []map[string]any
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(expected)
	for _, run := range []struct {
		propagation string
		rows        []map[string]any
	}{{"prompt", prompt}, {"held", held}} {
		if actual, _ := json.Marshal(run.rows); !bytes.Equal(actual, canonical) {
			t.Errorf("%s propagation\nGo %s\nPython %s", run.propagation, actual, want)
		}
	}
}
