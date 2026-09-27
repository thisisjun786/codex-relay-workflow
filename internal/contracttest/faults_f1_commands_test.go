package contracttest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The package replay tests inject clock/entropy and compare all transitions and
// rows. This separate real-binary test proves that all five names are routed.
func TestFaultF1CommandsBuiltCLI(t *testing.T) {
	binary, e := crwBinary()
	if e != nil {
		t.Fatal(e)
	}
	root, e := Root()
	if e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{
		{"fault-sweep"},
		{"fault-sweep", "--readings", "not-json"},
		{"fault-claim", "--publication", "missing", "--owner", "operator"},
		{"fault-operation", "--publication", "missing", "--claim-token", "token"},
		{"fault-reconcile", "--publication", "missing"},
		{"fault-complete", "--publication", "missing"},
	} {
		t.Run(args[0]+"/"+args[len(args)-1], func(t *testing.T) {
			home := t.TempDir()
			env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xs", "XDG_CONFIG_HOME="+home+"/xc", "CODEX_HOME="+home+"/ch")
			run := func(cmd *exec.Cmd) (int, []byte, []byte) {
				t.Helper()
				cmd.Dir = root
				cmd.Env = env
				var out, err bytes.Buffer
				cmd.Stdout = &out
				cmd.Stderr = &err
				code := 0
				if e := cmd.Run(); e != nil {
					if x, ok := e.(*exec.ExitError); ok {
						code = x.ExitCode()
					} else {
						t.Fatal(e)
					}
				}
				return code, out.Bytes(), err.Bytes()
			}
			pc, po, pe := run(exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay", "--state", filepath.Join(home, "py"), "--json"}, args...)...))
			gc, goOut, ge := run(exec.Command(binary, append([]string{"relay", "--state", filepath.Join(home, "go"), "--json"}, args...)...))
			if pc != gc || !bytes.Equal(po, goOut) || !bytes.Equal(pe, ge) {
				t.Fatalf("Python %d %s stderr %s\nGo %d %s stderr %s", pc, po, pe, gc, goOut, ge)
			}
		})
	}
}
