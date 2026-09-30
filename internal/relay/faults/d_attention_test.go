package faults

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestDAttentionWholeOutputAgainstPython(t *testing.T) {
	home, e := os.MkdirTemp("/dev/shm", "fault-d-attention-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	goDir := filepath.Join(home, "go")
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	code, reply := cliCall(t, goDir, "fault-observe", "--observation", observation)
	if code != 0 {
		t.Fatal(reply)
	}
	compare := func(label string) {
		t.Helper()
		gc, g := cliCall(t, goDir, "fault-attention")
		checkGolden(t, "relay fault-attention: "+label, []string{label}, runPathsOf(t, home), map[string]any{"code": gc, "reply": g})
	}
	compare("awaiting target")
	lapse := "UPDATE fault_publications SET state='issued',lease_until=0"
	s, e := store.Open(context.Background(), filepath.Join(goDir, "relay.sqlite3"), "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Q(context.Background()).ExecContext(context.Background(), lapse); e != nil {
		t.Fatal(e)
	}
	s.Close()
	compare("lapsed issued")
}
