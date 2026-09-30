package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

type heldRPC struct{ entered, release chan struct{} }

func (r *heldRPC) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "thread/read" && params["threadId"] == "thread-a" {
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
		return json.Marshal(map[string]any{"turn": map[string]any{"id": "turn-" + params["threadId"].(string)}})
	}
	return nil, &HostUnavailable{"unexpected method"}
}
func concurrencyCase(t *testing.T, kind string) {
	t.Helper()
	root := t.TempDir()
	rpc := &heldRPC{make(chan struct{}), make(chan struct{})}
	l, err := ledger.OpenWithOptions(filepath.Join(root, "go.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: rpc, Ledger: l, Drain: -1})
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	settings := &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}
	done := make(chan any, 1)
	go func() {
		r, err := a.SendMessage("req-a", "thread-a", "hello", settings)
		if err != nil {
			done <- map[string]any{"error": err.Error()}
		} else {
			done <- plain(r)
		}
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-rpc.entered:
	case <-timer.C:
		t.Fatal("RPC not reached")
	}
	result := map[string]any{}
	if kind == "replay" {
		r, err := a.SendMessage("req-a", "thread-a", "hello", settings)
		if err != nil {
			t.Fatal(err)
		}
		result["replayed"] = plain(r)
		_, err = a.SendMessage("req-a", "thread-a", "DIFFERENT", settings)
		if err == nil {
			t.Fatal("conflicting replay accepted")
		}
		result["conflict"] = err.Error()
	} else if kind == "busy" {
		r, err := a.SendMessage("req-busy", "thread-a", "second", settings)
		if err != nil {
			t.Fatal(err)
		}
		result["busy"] = plain(r)
	} else {
		r, err := a.SendMessage("req-b", "thread-b", "hello", settings)
		if err != nil {
			t.Fatal(err)
		}
		result["b"] = plain(r)
	}
	if kind == "shutdown" {
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		close(rpc.release)
	}
	select {
	case result["a"] = <-done:
	case <-timer.C:
		t.Fatal("caller not settled")
	}
	spec := map[string]any{"root": root, "case": kind, "settings": authorized(), "resume": resume()}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	out := pyDriver(t, "concurrency_capture.py", raw)
	var want any
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(result)
	expected, _ := json.Marshal(want)
	if !bytes.Equal(actual, expected) {
		t.Fatalf("Go %s\nPython %s", actual, expected)
	}
}
func Test28_BAD_12_BusyRecipients(t *testing.T) {
	s := sendScenario(resume(), []any{"send", "req-active", "thread-1", "hello"})
	s.answers[0] = map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}
	capture(t, s)
	concurrencyCase(t, "busy")
}
func Test28_BAD_13_IndependentRecipientsAndShutdown(t *testing.T) {
	concurrencyCase(t, "independent")
	concurrencyCase(t, "shutdown")
}
