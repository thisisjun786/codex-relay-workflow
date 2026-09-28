package sync

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func selectionReplay(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(root, "internal/relay/sync/testdata/selection_capture.py"), base)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, ".venv"), "UV_CACHE_DIR="+base+"/uv")
	raw, e := cmd.Output()
	if e != nil {
		t.Fatal(e)
	}
	var captures []storeCapture
	if e = json.Unmarshal(raw, &captures); e != nil {
		t.Fatal(e)
	}
	for _, c := range captures {
		binary := builtBinary(t)
		run := exec.Command(binary, append([]string{"relay"}, c.Argv...)...)
		c.Stdout = strings.ReplaceAll(c.Stdout, "'crw relay'", binary+" relay")
		env := []string{}
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "CODEX_SESSION_RELAY_STATE=") && !strings.HasPrefix(v, "XDG_STATE_HOME=") {
				env = append(env, v)
			}
		}
		run.Env = append(env, "XDG_STATE_HOME="+base+"/state")
		var out, errout bytes.Buffer
		run.Stdout, run.Stderr = &out, &errout
		code := processCode(run.Run())
		if code != c.Exit || out.String() != c.Stdout || errout.String() != c.Stderr {
			t.Fatalf("ambiguous selection %v Python exit=%d Go=%d\nPython:%s\nGo:%s\nPython stderr:%s\nGo stderr:%s", c.Argv, c.Exit, code, c.Stdout, out.String(), c.Stderr, errout.String())
		}
	}
}
