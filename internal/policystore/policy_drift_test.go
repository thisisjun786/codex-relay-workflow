package policystore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/affordance"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// CRW-1324 item 5: the warning SessionStart gives names a command, and that command (and the policy write that runs the same
// installer step) brings the record back to the file. This drives the REAL installer in a temporary home: a hand edit makes
// SessionStart warn, `register-mcp --re-register-policy` clears it, and a policy write (edit and re-register in one operation)
// leaves no drift either.
func TestPolicyEditAndReRegisterClearTheSessionStartWarning(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CRW_HOME", filepath.Join(root, "crw-home"))
	file := filepath.Join(root, "execution-policy.json")
	if err := os.WriteFile(file, []byte(policyText), 0o644); err != nil {
		t.Fatal(err)
	}
	installer := func(args ...string) {
		t.Helper()
		var stdout, stderr strings.Builder
		if code := install.Main(context.Background(), args, scope.Env(os.Environ()), &stdout, &stderr); code != install.OK {
			t.Fatalf("crw install %v: exit %d\n%s%s", args, code, stdout.String(), stderr.String())
		}
	}
	installer("register-mcp", "--owner", "plugin", "--execution-policy", file)

	cwd := t.TempDir()
	hook := func() string {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"cwd": cwd, "hook_event_name": "SessionStart", "session_id": "S"})
		return affordance.RunMapAffordanceSessionStart(string(payload), cwd, func(k string) (string, bool) { return os.LookupEnv(k) })
	}
	warned := func(answer string) bool {
		return strings.Contains(answer, "--re-register-policy --execution-policy "+file)
	}

	if warned(hook()) {
		t.Fatal("a freshly registered policy was warned about")
	}
	// A hand edit that the bridge would still parse, the way an operator changes the file.
	if err := os.WriteFile(file, []byte(policyText+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !warned(hook()) {
		t.Fatalf("a hand-edited policy produced no SessionStart warning naming the re-register command:\n%s", hook())
	}
	installer("register-mcp", "--re-register-policy", "--execution-policy", file)
	if warned(hook()) {
		t.Fatal("the warning stayed after the re-register command it names")
	}

	// The policy write is the one operation that edits the file and re-registers it: the host is not left in the warned state.
	env := map[string]string{"HOME": root, "CODEX_HOME": codexHome, "XDG_STATE_HOME": filepath.Join(root, "state")}
	located := Locate(envOf(env))
	before, _ := os.ReadFile(file)
	result := Write(context.Background(), envOf(env), WriteOptions{Running: unavailableRunning()},
		WriteRequest{ExpectedDigest: located.RegisteredDigest, Change: removeLegacy()})
	if result.Kind != WriteStored {
		t.Fatalf("write kind = %q (%v), want %q", result.Kind, result.Errors, WriteStored)
	}
	if after, _ := os.ReadFile(file); string(after) == string(before) {
		t.Fatal("the policy write did not change the file")
	}
	if warned(hook()) {
		t.Fatal("a policy write left the record and the file in disagreement")
	}
}
