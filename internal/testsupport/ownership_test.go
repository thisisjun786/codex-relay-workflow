package testsupport

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestFencePythonFixturePassesTheRealPythonAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "relay.sqlite3")
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	FencePythonFixture(t, s.DB, path, "")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "../.."))
	script := `
import sys
from codex_session_relay import ownership
from codex_session_relay.store import Store
p=sys.argv[1]
m,r=ownership.metadata(p),ownership.mirror(p)
expected={"owner":m.get("owner"),"epoch":int(m["owner_epoch"]),"storeId":m.get("store_id"),"database":ownership.physical(p),"pythonCompatibilityBuild":m.get("python_compatibility_build"),"rollbackAllowed":m.get("rollback_allowed")=="1"}
wrong={k:{"record":r.get(k),"expected":v} for k,v in expected.items() if r.get(k)!=v}
assert not wrong, wrong
Store(p).close()
`
	command := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script, path)
	command.Dir = root
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("real Python admission: %v\n%s", err, output)
	}
}
