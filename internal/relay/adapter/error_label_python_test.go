package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

type refusedRPC struct{}

func (refusedRPC) Call(context.Context, string, map[string]any) (json.RawMessage, error) {
	return nil, &delivery.HostError{Kind: "ConnectionRefusedError", Message: "[Errno 111] Connection refused"}
}

func Test28PythonErrorLabelsPersisted(t *testing.T) {
	// The lifecycle half is live-Python compared by Test28HostDecodedValueParity.
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ops.sqlite3"), ledger.Options{Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: refusedRPC{}, Ledger: l})
	defer a.Close()
	_, err = a.Send(context.Background(), "refused", "thread", "message", &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := l.Get(context.Background(), "refused")
	if err != nil {
		t.Fatal(err)
	}
	want := "ConnectionRefusedError: [Errno 111] Connection refused"
	if receipt["error"] != want {
		t.Fatalf("transport label %v", receipt)
	}
	settings, _ := json.Marshal(authorized())
	// The Python driver keeps the wall clock: the receipt's times are not compared.
	out := pyDriverNormalized(t, "error_label_capture.py", append(settings, '\n'), withoutWallClock)
	var python map[string]any
	if err := json.Unmarshal(out, &python); err != nil {
		t.Fatal(err)
	}
	if python["error"] != want {
		t.Fatalf("Python transport label %v", python)
	}
	var typed interface{ PythonExceptionKind() string }
	if !errors.As(&HostUnavailable{"unavailable"}, &typed) {
		t.Fatal("HostUnavailable is not typed")
	}
}
