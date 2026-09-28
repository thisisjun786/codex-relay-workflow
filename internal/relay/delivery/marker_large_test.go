package delivery

import (
	"context"
	"os"
	"os/exec"
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
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(root, ".venv/bin/python"), "-c", `import json,sys;from codex_session_relay.marker import _read_fact;v,state=_read_fact(sys.argv[1]);assert state=='present';assert len(v['padding'])==5*1024*1024;print(json.dumps(v,separators=(',',':')))`, path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	// Both decoders must preserve the entire large value, not only its class.
	python, err := loads(strings.TrimSpace(string(out)))
	if err != nil || !reflect.DeepEqual(value, python) {
		t.Fatal("Python value mismatch", err)
	}
	cancel()
	if _, state := readFactContext(ctx, path); state != factUnreadable {
		t.Fatal("cancelled read proceeded")
	}
}
