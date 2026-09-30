package cli_test

import (
	"os"
	"testing"
)

// The fix-X case kept from the merge-evidence rules matrix: a required-status-check context
// that is an object, answered by both command shapes as the goldens hold it (Python's answer,
// at first). The rest of the matrix ran only under the parity build tag against live Python and
// went with it in todo 44.

func Test24FixXBytes(t *testing.T) {
	t.Parallel()
	binary, alias := packageBinary(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xdg", "XDG_CONFIG_HOME="+home+"/config", "XDG_CACHE_HOME="+home+"/cache", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_REFUSE_LIVE_STATE=", `CRW_FORGE_RULES_JSON=[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":{"z":1,"a":false}}]}}]`)
	assertEvidenceBytes(t, env, alias, binary, "ready", []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7"}, "")
}
