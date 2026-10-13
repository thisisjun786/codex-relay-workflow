package affordance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// policyHost is an isolated Codex home whose bridge registration record names the execution policy at policy with the
// digest registered. It returns the lookup the hook reads and the policy path; the real host is never touched.
func policyHost(t *testing.T, policyBytes, registered string, writePolicy bool) (host.LookupEnv, string) {
	t.Helper()
	home := t.TempDir()
	policy := filepath.Join(t.TempDir(), "execution-policy.json")
	if writePolicy {
		if err := os.WriteFile(policy, []byte(policyBytes), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	document, err := json.Marshal(map[string]any{
		"recordVersion": 2, "owner": "plugin", "serverName": "codex-thread-bridge",
		"bridgeExecutable": "/usr/local/bin/codex-thread-bridge", "args": []string{},
		"executionPolicy": map[string]any{"path": policy, "digest": registered},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "crw-bridge-mcp.json"), document, 0o600); err != nil {
		t.Fatal(err)
	}
	return func(k string) (string, bool) {
		switch k {
		case "CRW_BIN":
			return "crw", true
		case "CODEX_HOME":
			return home, true
		}
		return "", false
	}, policy
}

func digestText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// CRW-1324 item 5: a policy file whose bytes no longer hash to the digest the bridge registration record names makes the bridge
// launcher refuse to start, so codex-thread-bridge is missing from every new session. SessionStart says so and names the
// command that re-registers; a policy that matches its record adds nothing.
func TestSessionStartWarnsWhenThePolicyDigestDiffersFromTheBridgeRecord(t *testing.T) {
	ws := t.TempDir()
	const policy = `{"edited":"after registration"}`

	env, path := policyHost(t, policy, digestText(`{"edited":"before"}`), true)
	got := contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, env), "SessionStart")
	for _, want := range []string{
		"codex-thread-bridge", path, digestText(`{"edited":"before"}`)[:12], digestText(policy)[:12],
		"crw install register-mcp --re-register-policy --execution-policy " + path,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning lacks %q:\n%s", want, got)
		}
	}

	// The same files agreeing say nothing: the answer is the one a host with no record gets.
	env, _ = policyHost(t, policy, digestText(policy), true)
	if got := contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, env), "SessionStart"); strings.Contains(got, "re-register") {
		t.Errorf("a matching policy was warned about:\n%s", got)
	}

	// A policy file that is gone fails the launcher the same way, and is named.
	env, path = policyHost(t, policy, digestText(policy), false)
	got = contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, env), "SessionStart")
	if !strings.Contains(got, "--re-register-policy --execution-policy "+path) || !strings.Contains(got, "does not exist") {
		t.Errorf("a missing policy file is not named:\n%s", got)
	}
}

// A host with no bridge record has nothing to disagree with.
func TestSessionStartWithoutABridgeRecordHasNoPolicyWarning(t *testing.T) {
	ws := t.TempDir()
	got := contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, sessionEnv(t, "crw")), "SessionStart")
	if strings.Contains(got, "re-register") {
		t.Errorf("no record, yet a warning:\n%s", got)
	}
}

// A resume repeats the warning with the binding (it can change between starts, like the PATH banner), and the hook that writes
// the answer carries it too.
func TestSessionStartResumeRepeatsThePolicyWarning(t *testing.T) {
	ws := t.TempDir()
	env, path := policyHost(t, `{"a":1}`, digestText(`{"a":0}`), true)
	hookAnswer(t, payload(ws, "SessionStart", "SESSION", nil), ws, env)
	got := contextOf(t, hookAnswer(t, payload(ws, "SessionStart", "SESSION", map[string]any{"source": "resume"}), ws, env), "SessionStart")
	parts := strings.Split(got, "\n\n")
	if len(parts) != 2 || parts[0] != RenderSessionBinding("SESSION", env) || !strings.Contains(parts[1], "--re-register-policy --execution-policy "+path) {
		t.Errorf("resume with a drifted policy: %q", got)
	}
}
