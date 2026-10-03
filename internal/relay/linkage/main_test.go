package linkage

import (
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain isolates the relay state as every package's does, and clears the execution-policy
// variable once for the whole run. The in-process relay commands (runCLI) read a role policy from
// it, so a policy named in the invoking environment must not reach them; no test sets another
// value, so the clearing is process-wide and tests can run in parallel (t.Setenv forbids that).
func TestMain(m *testing.M) {
	testsupport.Main(m, func(string) (func() error, error) {
		return nil, os.Setenv(execution.EnvPolicy, "")
	})
}
