package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// spawnLegEnv points every home and the plugin root at temporary directories: this leg can reach
// the real Codex home through the hook observation record.
func spawnLegEnv(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for name, value := range map[string]string{
		"HOME":        filepath.Join(root, "home"),
		"CODEX_HOME":  filepath.Join(root, "codex"),
		"CRW_HOME":    filepath.Join(root, "crw"),
		"PLUGIN_ROOT": filepath.Join(root, "plugin"),
		"TMPDIR":      filepath.Join(root, "tmp"),
	} {
		t.Setenv(name, value)
		if err := os.MkdirAll(value, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// The recorded answers of CXC v0.2.40's hook main (spawn-attach-hook.ts:1136-1150), renamed by
// contract/schema/cxc/name-substitution.json: contract/fixtures/cxc/hook__pre-tool-use-attaching-skills__recursion_denied_without_grant.json
// and ...__oversized_stdin_denied.json. The entry adds no newline: the envelopes end with one.
const (
	spawnLegRecursionDeny = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"crw LEAF-TOPOLOGY-01: sub-agents are leaf agents and may not spawn their own sub-agents (multi_agent_v2 enforces no depth limit upstream, so recursion is denied by dispatcher policy). Finish your own scope and report the need for delegation in your final answer. A dispatcher can authorize recursion for a specific spawn by including the recursion grant token in the spawn message."}}` + "\n"
	spawnLegOversizeDeny  = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"crw spawn policy input exceeded 4 MiB; refusing to bypass the recursion and trust boundary"}}` + "\n"
)

// spawnLegRun drives the real entry: crw hook pre-tool-use --leg pre-tool-use-attaching-skills.
func spawnLegRun(t *testing.T, input string) (int, string, string) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = file
	t.Cleanup(func() { os.Stdin = old })
	var out, errOut strings.Builder
	code := run(context.Background(), "crw", []string{"hook", "pre-tool-use", "--leg", "pre-tool-use-attaching-skills"}, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestSpawnLegAnswersTheRecordedDenyEnvelopes(t *testing.T) {
	spawnLegEnv(t)
	for _, c := range []struct{ name, input, want string }{
		{"a subagent spawn is denied before anything else", `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"spawn_agent","agent_id":"child-1","agent_type":"explorer","tool_input":{"message":"spawn a helper"},"tool_use_id":"call-1"}`, spawnLegRecursionDeny},
		{"an input over 4 MiB is refused", `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"spawn_agent","tool_input":{"message":"big","agent_type":"explorer"}}` + strings.Repeat(" ", 4194305), spawnLegOversizeDeny},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := spawnLegRun(t, c.input)
			if code != 0 || errOut != "" || out != c.want {
				t.Fatalf("exit %d stderr %q\n got %q\nwant %q", code, errOut, out, c.want)
			}
		})
	}
}

func TestSpawnLegQuietCases(t *testing.T) {
	spawnLegEnv(t)
	for _, c := range []struct{ name, input string }{
		{"a payload for another event is left alone", `{"hook_event_name":"PostToolUse","tool_name":"spawn_agent","tool_input":{}}`},
		{"a tool that is not a spawn is left alone", `{"hook_event_name":"PreToolUse","tool_name":"shell","tool_input":{}}`},
		{"exactly 4 MiB is not an overflow", `{"hook_event_name":"PreToolUse","tool_name":"shell","tool_input":{}}` + strings.Repeat(" ", 4194304-len(`{"hook_event_name":"PreToolUse","tool_name":"shell","tool_input":{}}`))},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := spawnLegRun(t, c.input)
			if code != 0 || out != "" || errOut != "" {
				t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
			}
		})
	}
}
