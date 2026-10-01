package adapter

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

type closingRPC struct {
	*heldRPC
	closing, finish chan struct{}
}

func (r *closingRPC) Close() error { close(r.closing); <-r.finish; return nil }
func waitEdge[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("transport state signal not received")
		var zero T
		return zero
	}
}
func transportEdge(t *testing.T, kind string) {
	t.Helper()
	root := t.TempDir()
	rpc := &closingRPC{&heldRPC{make(chan struct{}), make(chan struct{})}, make(chan struct{}), make(chan struct{})}
	l, err := ledger.OpenWithOptions(filepath.Join(root, "go.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	options := Options{RPC: rpc, Ledger: l, Drain: -1, Timeout: 10 * time.Second}
	// The caller's time budget is the behavior under test. The worker's 90s
	// execution budget is deliberately independent of it.
	if kind == "abandoned" {
		options.CallerSlack = -9900 * time.Millisecond
	}
	a := New(options)
	closed := false
	t.Cleanup(func() {
		if !closed {
			close(rpc.finish)
			if err := a.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	settings := &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}
	result := map[string]any{}
	send := func(id, thread string) any {
		r, e := a.SendMessage(id, thread, "hello", settings)
		if e != nil {
			return map[string]any{"error": e.Error()}
		}
		return plain(r)
	}
	switch kind {
	case "abandoned":
		done := make(chan any, 1)
		go func() { done <- send("req-a", "thread-a") }()
		waitEdge(t, rpc.entered)
		select {
		case result["abandoned"] = <-done:
		case <-time.After(5 * time.Second):
			result["abandoned"] = map[string]any{"error": "caller did not give up within its budget"}
		}
		// Only the deliberately stalled caller has a short budget.
		a.callerSlack = 10 * time.Second
		result["other"] = send("req-b", "thread-b")
		facts, e := a.ReadThread("thread-b")
		if e != nil {
			t.Fatal(e)
		}
		result["read"] = map[string]any{"runtime_status": facts.RuntimeStatus, "can_accept_input": facts.CanAcceptInput}
		settled := make(chan struct{})
		go func() { a.transport.pending.Wait(); close(settled) }()
		close(rpc.release)
		waitEdge(t, settled)
		receipt, e := a.GetOperation("req-a")
		if e != nil {
			t.Fatal(e)
		}
		result["settled"] = plain(receipt)
	case "racing-close":
		done := make(chan error, 1)
		go func() { done <- a.Close() }()
		waitEdge(t, rpc.closing)
		result["submission"] = send("never-sent", "thread-b")
		close(rpc.finish)
		if err := waitEdge(t, done); err != nil {
			t.Fatal(err)
		}
		closed = true
	case "stopping":
		// Go has no independent stopping hint. Python's hint alone must not stop
		// a worker: acceptance and the shutdown sentinel own that transition.
		result["serving"] = send("still-serving", "thread-b")
	default:
		t.Fatal(kind)
	}
	expectJSON(t, "transport", result)
}
func Test28_BAD_13_TransportEdgesMatchTheGolden(t *testing.T) {
	shareGoldens(t)
	for _, kind := range []string{"abandoned", "racing-close", "stopping"} {
		t.Run(kind, func(t *testing.T) { transportEdge(t, kind) })
	}
}
