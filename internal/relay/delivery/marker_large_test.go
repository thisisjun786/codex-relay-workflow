package delivery

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func Test33LargeMarkerFactPython(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intent.json")
	raw := `{"padding":"` + strings.Repeat("x", 5<<20) + `","dispatchRequestIdHash":"assignment"}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	value, state := readFactContext(ctx, path)
	if state != factPresent {
		t.Fatal("valid 5 MiB fact unreadable", state)
	}
	unbounded, unboundedState := readFact(path)
	if unboundedState != state || !reflect.DeepEqual(value, unbounded) {
		t.Fatal("deadline changed fact semantics")
	}
	// The whole large value, not only its class, is checked against the golden (it began as what
	// the fence's _read_fact decoded).
	golden.CheckJSON(t, "read_fact", normalizeJSON(t, jsonable(value)))
	cancel()
	if _, state := readFactContext(ctx, path); state != factUnreadable {
		t.Fatal("cancelled read proceeded")
	}
}
