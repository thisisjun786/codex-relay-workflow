package cli_test

import (
	"os"
	"path/filepath"
	"testing"
)

// The fix-X case kept from the merge-evidence rules matrix: a required-status-check context
// that is an object, answered by both command shapes as Python answered it (recorded). The
// rest of the matrix ran only under the parity build tag against live Python and went with it
// in todo 44.

func Test24FixXBytes(t *testing.T) {
	t.Parallel()
	root, _ := filepath.Abs("../../..")
	binary, alias := packageBinary(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xdg", "XDG_CONFIG_HOME="+home+"/config", "XDG_CACHE_HOME="+home+"/cache", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_REFUSE_LIVE_STATE=", "PATH="+root+"/internal/relay/cli/testdata:"+os.Getenv("PATH"), `CRW_FORGE_RULES_JSON=[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":{"z":1,"a":false}}]}}]`)
	assertEvidenceBytes(t, env, filepath.Join(root, ".venv/bin/python"), alias, binary, "ready", []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7"}, "")
}
