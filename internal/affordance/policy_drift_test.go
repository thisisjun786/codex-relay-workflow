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

// layered is env with the given variables set over it (a value of "" sets it to empty).
func layered(base host.LookupEnv, set map[string]string) host.LookupEnv {
	return func(k string) (string, bool) {
		if v, ok := set[k]; ok {
			return v, true
		}
		return base(k)
	}
}

// CRW-1360 d3: the installer trims CODEX_HOME, so a CODEX_HOME with spaces around it still names the
// Codex home whose record the warning compares, and the recovery command carries the policy path as
// one shell word (a path with a space or a quote pastes back unchanged).
func TestSessionStartPolicyWarningTrimsCodexHomeAndQuotesThePath(t *testing.T) {
	ws := t.TempDir()
	env, _ := policyHost(t, `{"a":1}`, digestText(`{"a":0}`), true)
	home, _ := env("CODEX_HOME")
	got := contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, layered(env, map[string]string{"CODEX_HOME": "  " + home + "\t\n"})), "SessionStart")
	if !strings.Contains(got, "re-register") {
		t.Errorf("a padded CODEX_HOME hid the record:\n%s", got)
	}
	if got := contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, layered(env, map[string]string{"CODEX_HOME": " \t "})), "SessionStart"); strings.Contains(got, home) {
		t.Errorf("a blank CODEX_HOME read the explicit home:\n%s", got)
	}

	// a policy path with a space and a quote
	spaced := filepath.Join(t.TempDir(), "my policy's.json")
	record := filepath.Join(home, "crw-bridge-mcp.json")
	document, err := json.Marshal(map[string]any{
		"recordVersion": 2, "owner": "plugin", "serverName": "codex-thread-bridge",
		"bridgeExecutable": "/usr/local/bin/codex-thread-bridge", "args": []string{},
		"executionPolicy": map[string]any{"path": spaced, "digest": digestText("x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, document, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spaced, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, env), "SessionStart")
	want := "--re-register-policy --execution-policy '" + strings.ReplaceAll(spaced, "'", `'"'"'`) + "'"
	if !strings.Contains(got, want) {
		t.Errorf("the recovery command does not quote the path (want %s):\n%s", want, got)
	}
}

// CRW-1360 d3: the launcher refuses to start under CODEX_THREAD_BRIDGE_EXECUTION_POLICY or
// CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST when they name another policy or digest than the record
// (internal/pluginwiring/launch.go), whatever the file's bytes are; the warning says that cause and the
// repair it needs (unset the variable, or register the other file), and never fails the start.
func TestSessionStartPolicyWarningFollowsTheLauncherOverrides(t *testing.T) {
	ws := t.TempDir()
	const policy = `{"a":1}`
	// the file matches the record, so only an override can make the launcher refuse
	env, path := policyHost(t, policy, digestText(policy), true)
	run := func(set map[string]string) string {
		return contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, layered(env, set)), "SessionStart")
	}
	for name, set := range map[string]map[string]string{
		"the same policy path": {"CODEX_THREAD_BRIDGE_EXECUTION_POLICY": " " + path + " "},
		"the same digest":      {"CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST": digestText(policy)},
		"blank overrides":      {"CODEX_THREAD_BRIDGE_EXECUTION_POLICY": "  ", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST": ""},
	} {
		if got := run(set); strings.Contains(got, "codex-thread-bridge") {
			t.Errorf("%s: a launcher that starts was warned about:\n%s", name, got)
		}
	}
	other := filepath.Join(t.TempDir(), "other policy.json")
	got := run(map[string]string{"CODEX_THREAD_BRIDGE_EXECUTION_POLICY": other})
	for _, want := range []string{"CODEX_THREAD_BRIDGE_EXECUTION_POLICY", other, path, "Unset the variable", "--execution-policy '" + other + "'"} {
		if !strings.Contains(got, want) {
			t.Errorf("a policy override: the warning lacks %q:\n%s", want, got)
		}
	}
	got = run(map[string]string{"CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST": "feedface" + digestText("z")[8:]})
	for _, want := range []string{"CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST", "feedface", digestText(policy)[:12], "Unset the variable"} {
		if !strings.Contains(got, want) {
			t.Errorf("a digest override: the warning lacks %q:\n%s", want, got)
		}
	}
	// the override is judged first, since the launcher refuses on it before it reads the file
	drifted, _ := policyHost(t, policy, digestText("old"), true)
	got = contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, layered(drifted, map[string]string{"CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST": digestText("zz")})), "SessionStart")
	if !strings.Contains(got, "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST") || !strings.Contains(got, "Unset the variable") {
		t.Errorf("override and drift together:\n%s", got)
	}
}
