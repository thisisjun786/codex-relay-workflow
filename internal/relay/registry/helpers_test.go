package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Fixture identities, as tests/support.py.
const (
	parent       = testsupport.Parent
	child        = testsupport.Child
	issue        = testsupport.Issue
	host         = testsupport.Host
	dispatchTurn = testsupport.DispatchTurn
	root         = "/work"
)

// fakeISO is FakeClock().iso(): 1_700_000_000 in UTC with microseconds.
var fakeISO = ISO(time.Unix(1_700_000_000, 0))

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return &Registry{Store: s, Now: func() string { return fakeISO }, Policy: ResolveRolePolicy(map[string]string{})}
}

func ns(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }

func fixture() Registration {
	return Registration{
		Parent:            Endpoint{parent, host, ns("/parent"), ns("cxc-parent")},
		Child:             Endpoint{child, host, ns(root), ns("cxc-child")},
		IssueKey:          issue,
		ArtifactRoots:     []string{root},
		AllowedRecipients: []string{parent},
		DispatchRequestID: "dispatch-1",
		DispatchTurnID:    ns(dispatchTurn),
	}
}

// python is testdata/python_registry.json, recorded from the real Python registry by
// testdata/gen_python.py.
var python = sync.OnceValues(func() (map[string][]map[string]any, error) {
	raw, err := os.ReadFile("testdata/python_registry.json")
	if err != nil {
		return nil, err
	}
	var out map[string][]map[string]any
	return out, json.Unmarshal(raw, &out)
})

func pythonSteps(t *testing.T, name string) []map[string]any {
	t.Helper()
	all, err := python()
	if err != nil {
		t.Fatal(err)
	}
	steps, ok := all[name]
	if !ok {
		t.Fatalf("no python scenario %q", name)
	}
	return steps
}

// outcome is one step as gen_python.py records it.
func outcome(t *testing.T, value any, err error) map[string]any {
	t.Helper()
	if err != nil {
		var refused *store.RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("unexpected error: %v", err)
		}
		return map[string]any{"refused": map[string]any{"reason": refused.Reason, "detail": refused.Detail}}
	}
	var buf []byte
	switch v := value.(type) {
	case contract.OrderedObject, []any:
		var b bytesBuffer
		if err := contract.Emit(&b, v); err != nil {
			t.Fatal(err)
		}
		buf = b
	default:
		var jerr error
		buf, jerr = json.Marshal(v)
		if jerr != nil {
			t.Fatal(jerr)
		}
	}
	var decoded any
	if err := json.Unmarshal(buf, &decoded); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"ok": decoded}
}

type bytesBuffer []byte

func (b *bytesBuffer) Write(p []byte) (int, error) { *b = append(*b, p...); return len(p), nil }

// sameAsPython compares every step's whole JSON with Python's.
func sameAsPython(t *testing.T, name string, got []map[string]any) {
	t.Helper()
	want := pythonSteps(t, name)
	if len(got) != len(want) {
		t.Fatalf("%s: %d steps, python has %d", name, len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			g, _ := json.MarshalIndent(got[i], "", " ")
			w, _ := json.MarshalIndent(want[i], "", " ")
			t.Errorf("%s step %d differs from Python\n go: %s\n py: %s", name, i, g, w)
		}
	}
}

func refusalReason(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}

func mustReason(t *testing.T, err error, reason contract.RefusalReason) {
	t.Helper()
	if got := refusalReason(err); got != string(reason) {
		t.Fatalf("want refusal %s, got %v", reason, err)
	}
}

func ctx() context.Context { return context.Background() }

// generationValue is registry.generation(...)'s dict.
func generationValue(g Generation, ok bool) any {
	if !ok {
		return nil
	}
	return g.Record()
}
