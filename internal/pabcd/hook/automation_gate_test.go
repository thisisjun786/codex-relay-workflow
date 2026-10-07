// The CXC v0.2.40 automation ownership gate's own cases, ported from
// plugins/codexclaw/components/pabcd-state/test/automation-ownership-gate.test.ts (15 tests),
// plus two sweeps the oracle's file does not carry: every documented input key, and the
// truthiness of hook_event_name. Every case runs on a temporary CODEX_HOME.
package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	automationOwner = "019f9d73-4c28-7723-ab52-346aca1d9bcb"
	automationOther = "019f9d73-4c28-7723-ab52-346aca1d9bcc"
	automationID    = "heartbeat-one"
)

func automationLookup(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, set := vars[key]; return value, set }
}

// automationStoreText is the oracle's store(): a valid own heartbeat.
func automationStoreText(owner string) string {
	return "version = 1\nid = \"" + automationID + "\"\nkind = \"heartbeat\"\nname = \"Example\"\nprompt = \"A prompt\"\nrrule = \"FREQ=HOURLY\"\nstatus = \"ACTIVE\"\ntarget_thread_id = \"" + owner + "\"\ncreated_at = 123\nupdated_at = 456\n"
}

type automationFixture struct {
	root string
	home string
	dir  string
	file string
	vars map[string]string
}

// automationFixture writes one automation store under a temporary CODEX_HOME.
func automationNewFixture(t *testing.T, content string) automationFixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(home, "automations", automationID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "automation.toml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return automationFixture{root: root, home: home, dir: dir, file: file, vars: map[string]string{"CODEX_HOME": home}}
}

// automationPayload is the oracle's ptu(): a PreToolUse payload whose tool_input is input. A key
// whose value is nil is dropped, as JSON.stringify drops undefined.
func automationPayload(t *testing.T, input any, extra map[string]any) string {
	t.Helper()
	payload := map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      automationOwner,
		"tool_name":       "mcp__codex_app__automation_update",
		"tool_input":      input,
	}
	for key, value := range extra {
		if value == nil {
			delete(payload, key)
			continue
		}
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func automationDeleteInput() map[string]any {
	return map[string]any{"mode": "delete", "id": automationID}
}

// automationDenied is the oracle's denied(): the envelope, the event and the reason prefix.
func automationDenied(t *testing.T, result string) {
	t.Helper()
	var out map[string]any
	if json.Unmarshal([]byte(result), &out) != nil {
		t.Fatalf("not JSON: %q", result)
	}
	specific, _ := out["hookSpecificOutput"].(map[string]any)
	if specific["hookEventName"] != "PreToolUse" || specific["permissionDecision"] != "deny" {
		t.Fatalf("not the PreToolUse deny envelope: %q", result)
	}
	reason, _ := specific["permissionDecisionReason"].(string)
	if !strings.HasPrefix(reason, "AUTOMATION-OWNERSHIP-01:") {
		t.Fatalf("the reason is not the gate's: %q", reason)
	}
}

// TestAutomationGateOwnHeartbeatPasses is the oracle's "own heartbeat update, suggested update and
// delete pass without changing store".
func TestAutomationGateOwnHeartbeatPasses(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	for _, mode := range []string{"update", "suggested_update", "delete"} {
		raw := automationPayload(t, map[string]any{"mode": mode, "id": automationID}, nil)
		if got := HandleAutomationOwnershipGate(raw, automationLookup(f.vars)); got != "" {
			t.Errorf("%s of the caller's own heartbeat: %q", mode, got)
		}
	}
	after, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != automationStoreText(automationOwner) {
		t.Errorf("the store changed: %s", after)
	}
}

// TestAutomationGateMatcherAliases is the oracle's "exact matcher aliases agree and unrelated names
// remain unaffected".
func TestAutomationGateMatcherAliases(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOther))
	aliases := []string{"automation_update", "mcp__codex_app__automation_update", "mcp__codex_app.automation_update",
		"mcp__codex_app_automation_update", "codex_app.automation_update", "codex_app_automation_update"}
	for _, name := range aliases {
		if !automationUpdateToolName(name) {
			t.Errorf("%q is not matched", name)
		}
		raw := automationPayload(t, automationDeleteInput(), map[string]any{"tool_name": name})
		automationDenied(t, HandleAutomationOwnershipGate(raw, automationLookup(f.vars)))
	}
	for _, name := range []string{"exec_command", "mcp__other__automation_update", "prefixautomation_update", "mcp__codex_appautomation_update"} {
		if automationUpdateToolName(name) {
			t.Errorf("%q is matched", name)
		}
		raw := automationPayload(t, automationDeleteInput(), map[string]any{"tool_name": name})
		if got := HandleAutomationOwnershipGate(raw, automationLookup(f.vars)); got != "" {
			t.Errorf("%q is answered: %q", name, got)
		}
	}
}

// TestAutomationGateForeignOwnership is the oracle's "foreign ownership denies regardless of
// caller-supplied target or owner assertions".
func TestAutomationGateForeignOwnership(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOther))
	for _, extra := range []map[string]any{
		{"targetThreadId": automationOwner},
		{"ownerThreadId": automationOwner},
	} {
		input := automationDeleteInput()
		for key, value := range extra {
			input[key] = value
		}
		raw := automationPayload(t, input, nil)
		automationDenied(t, HandleAutomationOwnershipGate(raw, automationLookup(f.vars)))
	}
}

// TestAutomationGateChildIdentity is the oracle's "child identity never inherits root association
// and invalid native identity denies".
func TestAutomationGateChildIdentity(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	for _, extra := range []map[string]any{
		{"agent_id": "child"}, {"agent_type": "executor"}, {"agent_id": automationOwner},
		{"agent_id": float64(1)}, {"agent_type": map[string]any{}},
		{"session_id": ""}, {"session_id": nil}, {"session_id": "../owner"},
	} {
		raw := automationPayload(t, automationDeleteInput(), extra)
		automationDenied(t, HandleAutomationOwnershipGate(raw, automationLookup(f.vars)))
	}
	raw := automationPayload(t, automationDeleteInput(), map[string]any{"agent_id": nil, "agent_type": ""})
	if got := HandleAutomationOwnershipGate(raw, automationLookup(f.vars)); got != "" {
		t.Errorf("an empty marker is answered: %q", got)
	}
}

// TestAutomationGateNeverReadsStoreForViewsOrUnrelatedTools is the oracle's "views and unrelated
// tools do not access even a throwing store configuration": the lookup fails the test if consulted.
func TestAutomationGateNeverReadsStoreForViewsOrUnrelatedTools(t *testing.T) {
	env := func(string) (string, bool) { t.Fatal("the store was consulted"); return "", false }
	view := automationPayload(t, map[string]any{"mode": "view", "id": "../anything"}, map[string]any{"agent_id": "child"})
	if got := HandleAutomationOwnershipGate(view, env); got != "" {
		t.Errorf("a view is answered: %q", got)
	}
	unrelated := automationPayload(t, nil, map[string]any{"tool_name": "unrelated"})
	if got := HandleAutomationOwnershipGate(unrelated, env); got != "" {
		t.Errorf("an unrelated tool is answered: %q", got)
	}
}

// TestAutomationGateHeartbeatCreateRules is the oracle's "heartbeat creates accept only implicit or
// explicit native caller and no existing id".
func TestAutomationGateHeartbeatCreateRules(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	for _, mode := range []string{"create", "suggested_create"} {
		for _, extra := range []map[string]any{{}, {"targetThreadId": automationOwner}} {
			input := map[string]any{"mode": mode, "kind": "heartbeat"}
			for key, value := range extra {
				input[key] = value
			}
			if got := HandleAutomationOwnershipGate(automationPayload(t, input, nil), automationLookup(f.vars)); got != "" {
				t.Errorf("%s %v is answered: %q", mode, extra, got)
			}
		}
		for _, extra := range []map[string]any{
			{"targetThreadId": automationOther}, {"targetThreadId": nil}, {"id": automationID}, {"id": nil}, {"kind": "cron"},
		} {
			input := map[string]any{"mode": mode, "kind": "heartbeat"}
			for key, value := range extra {
				input[key] = value
			}
			automationDenied(t, HandleAutomationOwnershipGate(automationPayload(t, input, nil), automationLookup(f.vars)))
		}
		child := automationPayload(t, map[string]any{"mode": mode, "kind": "heartbeat"}, map[string]any{"agent_id": "child"})
		automationDenied(t, HandleAutomationOwnershipGate(child, automationLookup(f.vars)))
		bare := automationPayload(t, map[string]any{"mode": mode}, nil)
		automationDenied(t, HandleAutomationOwnershipGate(bare, automationLookup(f.vars)))
	}
}

// TestAutomationGateMutationShapeDenies is the oracle's "retargets, unsupported modes, unknown
// fields, kinds and destinations deny".
func TestAutomationGateMutationShapeDenies(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	for _, extra := range []map[string]any{
		{"targetThreadId": automationOther}, {"target_thread_id": automationOwner}, {"targetThreadId": nil},
		{"mode": "new-mode"}, {"mode": ""}, {"mode": nil}, {"kind": "cron"}, {"kind": "unknown"},
		{"destination": "remote"}, {"owner": automationOwner},
	} {
		input := map[string]any{"mode": "update", "id": automationID}
		for key, value := range extra {
			if value == nil {
				input[key] = nil
				continue
			}
			input[key] = value
		}
		automationDenied(t, HandleAutomationOwnershipGate(automationPayload(t, input, nil), automationLookup(f.vars)))
	}
	allowed := map[string]any{"mode": "update", "id": automationID, "targetThreadId": automationOwner, "kind": "heartbeat"}
	if got := HandleAutomationOwnershipGate(automationPayload(t, allowed, nil), automationLookup(f.vars)); got != "" {
		t.Errorf("a fully specified own update is answered: %q", got)
	}
}

// TestAutomationGateMalformedPayloads is the oracle's "malformed dedicated payload and mutation
// input deny, other hook events pass".
func TestAutomationGateMalformedPayloads(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	raws := []string{"", "{", "null", "[]", "{}",
		strings.Repeat("x", automationMaxPayloadBytes+1),
		automationPayload(t, nil, nil),
		automationPayload(t, "{}", nil),
		automationPayload(t, []any{}, nil),
		automationPayload(t, map[string]any{}, nil),
		automationPayload(t, automationDeleteInput(), map[string]any{"tool_name": nil}),
		automationPayload(t, automationDeleteInput(), map[string]any{"hook_event_name": nil}),
	}
	for _, raw := range raws {
		automationDenied(t, HandleAutomationOwnershipGate(raw, automationLookup(f.vars)))
	}
	post := automationPayload(t, automationDeleteInput(), map[string]any{"hook_event_name": "PostToolUse"})
	if got := HandleAutomationOwnershipGate(post, automationLookup(f.vars)); got != "" {
		t.Errorf("a PostToolUse call is answered: %q", got)
	}
}

// TestAutomationGateUnsafeIds is the oracle's "missing, unsafe, encoded and ambiguous automation
// ids deny".
func TestAutomationGateUnsafeIds(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	ids := []any{nil, "", "missing", "../heartbeat-one", "a/b", "a\\b", "%2e%2e", ".", "a\x00b", strings.Repeat("a", 201)}
	for _, id := range ids {
		raw := automationPayload(t, map[string]any{"mode": "delete", "id": id}, nil)
		automationDenied(t, HandleAutomationOwnershipGate(raw, automationLookup(f.vars)))
	}
}

// TestAutomationGateUnsupportedStore is the oracle's "unsupported, duplicate, conflicting or
// missing store fields deny".
func TestAutomationGateUnsupportedStore(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	own := automationStoreText(automationOwner)
	quotedOwner := "target_thread_id = \"" + automationOwner + "\""
	cases := []string{
		strings.Replace(own, quotedOwner+"\n", "", 1),
		strings.Replace(own, quotedOwner, "target_thread_id = \"\"", 1),
		own + "target_thread_id = \"" + automationOther + "\"\n",
		own + quotedOwner + "\n",
		own + "name = \"duplicate\"\n",
		own + "[ownership]\nowner = \"bad\"\n",
		own + "owner_thread_id = \"extra\"\n",
		strings.Replace(own, "kind = \"heartbeat\"", "kind = \"cron\"", 1),
		strings.Replace(own, "kind = \"heartbeat\"", "kind = \"unknown\"", 1),
		strings.Replace(own, "version = 1", "version = 2", 1),
		strings.Replace(own, "id = \""+automationID+"\"", "id = \"different\"", 1),
		strings.Replace(own, "created_at = 123", "created_at = [123]", 1),
		strings.Replace(own, "created_at = 123", "created_at = 01", 1),
		strings.Replace(own, "created_at = 123", "created_at = 9007199254740993", 1),
		strings.Replace(own, "version = 1", "version = true", 1),
		strings.Replace(own, quotedOwner, "target_thread_id = \"\"\""+automationOwner+"\"\"\"", 1),
		strings.Replace(own, "name = \"Example\"", "\"name\" = \"Example\"", 1),
		strings.Replace(own, "name = \"Example\"", "name = \"unterminated", 1),
		own + "\x00",
		own + "# control\x00\n",
		strings.ReplaceAll(own, "\n", "\r"),
		strings.Repeat("x", automationMaxStoreBytes+1),
	}
	for i, content := range cases {
		if err := os.WriteFile(f.file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		got := HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars))
		if got == "" {
			t.Errorf("case %d was allowed: %q", i, content)
			continue
		}
		automationDenied(t, got)
	}
}

// TestAutomationGateMultilinePromptCannotSpoofOwner is the oracle's "multiline basic and literal
// prompts cannot spoof an owner".
func TestAutomationGateMultilinePromptCannotSpoofOwner(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	for _, delimiter := range []string{"\"\"\"", "'''"} {
		prompt := "prompt = " + delimiter + "\n# not a real key\ntarget_thread_id = \"" + automationOwner + "\"\n[table]\n" + delimiter
		foreign := strings.Replace(automationStoreText(automationOther), "prompt = \"A prompt\"", prompt, 1)
		if err := os.WriteFile(f.file, []byte(foreign), 0o644); err != nil {
			t.Fatal(err)
		}
		automationDenied(t, HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)))
		own := strings.Replace(automationStoreText(automationOwner), "prompt = \"A prompt\"", prompt, 1)
		if err := os.WriteFile(f.file, []byte(own), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)); got != "" {
			t.Errorf("delimiter %q own prompt: %q", delimiter, got)
		}
		missing := strings.Replace(foreign, "target_thread_id = \""+automationOther+"\"\n", "", 1)
		if err := os.WriteFile(f.file, []byte(missing), 0o644); err != nil {
			t.Fatal(err)
		}
		automationDenied(t, HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)))
	}
}

// TestAutomationGateEscapesAndUnicode is the oracle's "escaped quotes, comment text and unicode stay
// inside supported strings".
func TestAutomationGateEscapesAndUnicode(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	own := automationStoreText(automationOwner)
	content := strings.Replace(own, "prompt = \"A prompt\"",
		"prompt = \"line\\nquote\\\" # target_thread_id = fake \\uAC00\" # comment", 1)
	content = strings.Replace(content, "name = \"Example\"", "name = 'literal # comment'", 1)
	if err := os.WriteFile(f.file, []byte(strings.ReplaceAll(content, "\n", "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)); got != "" {
		t.Errorf("escaped quotes and unicode: %q", got)
	}
	wrapped := strings.Replace(own, "prompt = \"A prompt\"", "prompt = \"\"\"a\\\"\"\"\ntarget_thread_id = \"fake\"\nend\"\"\"", 1)
	if err := os.WriteFile(f.file, []byte(wrapped), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)); got != "" {
		t.Errorf("a continued multiline prompt: %q", got)
	}
	for _, prompt := range []string{
		"prompt = \"\"\"unterminated", "prompt = \"bad\\q\"", "prompt = \"\"\"bad\\q\"\"\"", "prompt = \"\\uD800\"",
	} {
		broken := strings.Replace(own, "prompt = \"A prompt\"", prompt, 1)
		if err := os.WriteFile(f.file, []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		automationDenied(t, HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)))
	}
}

// TestAutomationGateSymlinksDeny is the oracle's "symlink files, automation directories, store root
// and home deny".
func TestAutomationGateSymlinksDeny(t *testing.T) {
	for _, surface := range []string{"file", "dir", "automations", "home"} {
		f := automationNewFixture(t, automationStoreText(automationOwner))
		target := f.file
		switch surface {
		case "dir":
			target = f.dir
		case "automations":
			target = filepath.Join(f.home, "automations")
		case "home":
			target = f.home
		}
		outside := filepath.Join(f.root, "outside")
		if surface == "file" {
			if err := os.WriteFile(outside, []byte(automationStoreText(automationOwner)), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, target); err != nil {
			t.Fatal(err)
		}
		automationDenied(t, HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)))
	}
}

// TestAutomationGateNonRegularStoreDenies is the oracle's "missing files, non-regular files and
// invalid UTF-8 deny".
func TestAutomationGateNonRegularStoreDenies(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	if err := os.WriteFile(f.file, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	automationDenied(t, HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)))
	if err := os.Remove(f.file); err != nil {
		t.Fatal(err)
	}
	automationDenied(t, HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)))
	if err := os.Mkdir(f.file, 0o755); err != nil {
		t.Fatal(err)
	}
	automationDenied(t, HandleAutomationOwnershipGate(automationPayload(t, automationDeleteInput(), nil), automationLookup(f.vars)))
}

// TestAutomationGateEvaluatorRejectsUnknownAssociation is the oracle's "pure evaluator rejects
// conflicting snapshot ids and unknown association".
func TestAutomationGateEvaluatorRejectsUnknownAssociation(t *testing.T) {
	caller := map[string]any{"session_id": automationOwner}
	request := map[string]any{"mode": "delete", "id": automationID}
	snapshots := []*automationOwnershipSnapshot{
		nil,
		{ID: "other", Kind: "heartbeat", TargetThreadID: automationOwner},
		{ID: automationID, Kind: "cron", TargetThreadID: automationOwner},
		{ID: automationID, Kind: "heartbeat", TargetThreadID: ""},
	}
	for _, snapshot := range snapshots {
		if allow, _ := automationEvaluateMutation(request, caller, snapshot); allow {
			t.Errorf("snapshot %+v was allowed", snapshot)
		}
	}
}

// TestAutomationGateAcceptsEveryDocumentedInputKey pins the input-key set against the oracle's: one
// own-heartbeat update carrying all 14 keys must pass. A dropped case would deny here.
func TestAutomationGateAcceptsEveryDocumentedInputKey(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	input := map[string]any{
		"mode": "update", "id": automationID, "kind": "heartbeat", "name": "Example",
		"prompt": "A prompt", "rrule": "FREQ=HOURLY", "status": "ACTIVE",
		"targetThreadId": automationOwner, "destination": "thread", "notificationPolicy": "notify",
		"executionEnvironment": "local", "model": "gpt", "projectId": "p", "reasoningEffort": "high",
	}
	for key := range input {
		if !automationInputKey(key) {
			t.Fatalf("input key %q is not accepted", key)
		}
	}
	if got := HandleAutomationOwnershipGate(automationPayload(t, input, nil), automationLookup(f.vars)); got != "" {
		t.Errorf("a fully populated own update is answered: %q", got)
	}
}

// TestAutomationGateHookEventTruthiness pins the oracle's hook_event_name test: a truthy value that
// is not PreToolUse passes in silence, and only a falsy one denies.
func TestAutomationGateHookEventTruthiness(t *testing.T) {
	f := automationNewFixture(t, automationStoreText(automationOwner))
	for _, event := range []any{float64(5), map[string]any{}, "PostToolUse"} {
		raw := automationPayload(t, automationDeleteInput(), map[string]any{"hook_event_name": event})
		if got := HandleAutomationOwnershipGate(raw, automationLookup(f.vars)); got != "" {
			t.Errorf("a truthy event %v is answered: %q", event, got)
		}
	}
	for _, event := range []any{"", float64(0), false} {
		raw := automationPayload(t, automationDeleteInput(), map[string]any{"hook_event_name": event})
		automationDenied(t, HandleAutomationOwnershipGate(raw, automationLookup(f.vars)))
	}
	raw := automationPayload(t, automationDeleteInput(), map[string]any{"hook_event_name": nil})
	automationDenied(t, HandleAutomationOwnershipGate(raw, automationLookup(f.vars)))
}
