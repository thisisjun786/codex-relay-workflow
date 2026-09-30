package faults

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestDAttentionWholeOutputAgainstPython(t *testing.T) {
	home, e := os.MkdirTemp("/dev/shm", "fault-d-attention-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	goDir, pyDir := filepath.Join(home, "go"), filepath.Join(home, "python")
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	code, reply := cliCall(t, goDir, "fault-observe", "--observation", observation)
	if code != 0 {
		t.Fatal(reply)
	}
	pythonCopy(t, goDir, pyDir)
	compare := func(label string) {
		t.Helper()
		gc, g := cliCall(t, goDir, "fault-attention")
		out := pyAnswer(t, "relay fault-attention", []string{label}, pyRunPaths(t, home), func() ([]byte, error) {
			cmd := exec.Command("uv", "run", "--no-sync", "codex-session-relay", "--state", pyDir, "--json", "fault-attention")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
			return cmd.Output()
		})
		var p map[string]any
		if e := json.Unmarshal(out, &p); e != nil {
			t.Fatal(e)
		}
		if gc != 0 || !reflect.DeepEqual(g, p) {
			t.Fatalf("%s: Go %d %v Python %v", label, gc, g, p)
		}
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
	seedPython(t, filepath.Join(pyDir, "relay.sqlite3"), seedSQL(lapse)...)
	compare("lapsed issued")
}
