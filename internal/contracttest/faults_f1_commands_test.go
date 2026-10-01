package contracttest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The package replay tests inject clock/entropy and compare all transitions and
// rows. This separate real-binary test proves that all five names are routed, answering what
// the golden holds (first taken as what the Python CLI answered).
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
			got := run(exec.Command(binary, append([]string{"relay", "--state", filepath.Join(home, "go"), "--json"}, args...)...))
			checkProcess(t, "answer", got, golden.Substitute(home, "<HOME>"), golden.Substitute(root, "<ROOT>"))
		})
	}
}
