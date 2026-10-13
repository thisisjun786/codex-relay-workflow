package affordance

import (
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pluginwiring"
)

// policyDriftSections is SessionStart's warning that the execution policy file no longer matches the bridge registration record
// (CRW-1324, item 5). The packaged launcher refuses to start codex-thread-bridge under a policy whose bytes differ from the record,
// so after a policy edit the bridge silently drops out of every new session and the screen shows only a handshake failure. The
// warning names the command that brings the record back to the file; it blocks nothing, and a host whose policy and record agree,
// or that has no record, gets nothing.
func policyDriftSections(env host.LookupEnv) []harness.ContextSection {
	home, ok := codexHomeOf(env)
	if !ok {
		return nil
	}
	drift := pluginwiring.ReadPolicyDrift(home)
	if !drift.Drifted {
		return nil
	}
	text := "[crw] The execution policy " + drift.Path + " no longer matches the bridge registration (" + drift.Reason + "), so the bridge launcher refuses to start and codex-thread-bridge is missing from every new session, this one included."
	if drift.Actual != "" {
		text += " The record names digest " + short(drift.Registered) + ", the file hashes to " + short(drift.Actual) + "."
	}
	text += " Re-register the policy as it now stands: `crw install register-mcp --re-register-policy --execution-policy " + drift.Path + "`, or change the policy in the policy editor (crw gui), which edits the file and re-registers it in one step."
	return []harness.ContextSection{{Text: ResolveCRWCommands(text, env)}}
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// codexHomeOf is the Codex home the host reads its records from: CODEX_HOME, else .codex below the user's home.
func codexHomeOf(env host.LookupEnv) (string, bool) {
	if value, set := env("CODEX_HOME"); set && value != "" {
		return value, true
	}
	home, err := host.Home(env)
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".codex"), true
}
