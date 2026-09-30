package contracttest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The package replay tests inject clock/entropy and compare all transitions and
// rows. This separate real-binary test proves that all five names are routed, answering what
// the Python CLI answered (recorded, internal/testsupport/pyoracle).
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
			run := func(cmd *exec.Cmd) processAnswer {
				t.Helper()
				cmd.Dir = root
				cmd.Env = env
				answer, err := runProcess(cmd)
				if err != nil {
					t.Fatal(err)
				}
				return answer
			}
			python := pythonProcess(t, "python", func() (processAnswer, error) {
				return run(exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay", "--state", filepath.Join(home, "py"), "--json"}, args...)...)), nil
			}, pyoracle.Substitute(home, "<HOME>"), pyoracle.Substitute(root, "<ROOT>"))
			got := run(exec.Command(binary, append([]string{"relay", "--state", filepath.Join(home, "go"), "--json"}, args...)...))
			if python.exit != got.exit || !bytes.Equal(python.stdout, got.stdout) || !bytes.Equal(python.stderr, got.stderr) {
				t.Fatalf("Python %d %s stderr %s\nGo %d %s stderr %s", python.exit, python.stdout, python.stderr, got.exit, got.stdout, got.stderr)
			}
		})
	}
}
