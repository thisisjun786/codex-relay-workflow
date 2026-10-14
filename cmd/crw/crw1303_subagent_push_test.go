package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// subagentPushFixture is a temporary CODEX_HOME with the completion hook's settings, a temporary relay store holding one
// relationship (child "01a1357c-...", the thread of a relay child), the child's and the parent's checkouts, and PLUGIN_ROOT set so
// the guard's decisions are recorded under the home. Nothing outside the temporary directory is read or written.
type subagentPushFixture struct {
	t                 *testing.T
	home, kid, parent string
	settings          string
}

const (
	crw1303Kid     = "01a1357c-25e3-71e1-9120-17a30d441d91"
	crw1303Spawned = "01a1357d-48f8-7391-9d70-efab8d0dcfff"
	crw1303Parent  = "01a13570-0000-7000-8000-000000000001"
)

func newSubagentPushFixture(t *testing.T) *subagentPushFixture {
	t.Helper()
	dir := t.TempDir()
	f := &subagentPushFixture{t: t, home: filepath.Join(dir, "codex"), kid: filepath.Join(dir, "kid"), parent: filepath.Join(dir, "parent")}
	db := filepath.Join(dir, "state", "relay.sqlite3")
	for repo, branch := range map[string]string{f.kid: "r2-x", f.parent: "dev"} {
		if out, err := exec.Command("git", "init", "-q", "-b", branch, repo).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	plugin := filepath.Join(dir, "plugin")
	for _, d := range []string{f.home, filepath.Dir(db), plugin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(context.Background(), db, filepath.Join(dir, "app.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-1','ISS-1','active','par','host',?,?,'host',?,1,'[]','[]','2026-10-14T10:00:00Z','2026-10-14T10:00:00Z')", f.parent, crw1303Kid, f.kid); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f.settings = `{"configVersion": 1, "event": "Stop", "relayExecutable": "/nonexistent/relay", "markerRoot": "` + dir + `/markers", "dbPath": "` + db +
		`", "mode": "observe", "timeoutSeconds": 5, "journalRoot": "` + dir + `/journal", "journalPolicy": "every_invocation", "installedBy": "CRW-37", "isolationAssertedBy": null}`
	t.Setenv("CODEX_HOME", f.home)
	t.Setenv("PLUGIN_ROOT", plugin)
	f.writeSettings(f.settings)
	return f
}

func (f *subagentPushFixture) writeSettings(raw string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.home, "crw-completion-hook.json"), []byte(raw), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// answer is the declared leg's stdout for a PreToolUse Bash payload; agent "" is a payload without agent_id and agent_type.
func (f *subagentPushFixture) answer(session, agent, cwd, command string) string {
	f.t.Helper()
	o := map[string]any{"hook_event_name": "PreToolUse", "session_id": session, "turn_id": "t", "cwd": cwd, "tool_name": "Bash", "tool_input": map[string]any{"command": command}}
	if agent != "" {
		o["agent_id"], o["agent_type"] = agent, "worker"
	}
	payload, err := json.Marshal(o)
	if err != nil {
		f.t.Fatal(err)
	}
	var out bytes.Buffer
	claimed, code := runComponentHook(invocation{ctx: context.Background(), args: []string{"pre-tool-use", "--leg", "pre-tool-use-guarding-child-push"}, stdout: &out}, bytes.NewReader(payload), componentHooks())
	if !claimed || code != 0 {
		f.t.Fatalf("claimed %v, exit %d", claimed, code)
	}
	return out.String()
}

// records is the decision records so far, and fails when any record file holds text of a command run in this test (the leg's own name holds "push", so that word is not a test).
func (f *subagentPushFixture) records() []map[string]any {
	f.t.Helper()
	files, _ := filepath.Glob(filepath.Join(f.home, "crw", "hook-decisions", "*.jsonl"))
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			f.t.Fatal(err)
		}
		for _, leak := range []string{"git push", "HEAD", "origin", "--force", "--mirror", "--delete", "r2-x", "ls -la"} {
			if strings.Contains(string(raw), leak) {
				f.t.Errorf("%s holds %q: a decision record names no command text:\n%s", filepath.Base(p), leak, raw)
			}
		}
	}
	return harness.ReadDecisions(f.home)
}

// TestCRW1303SubagentThreadPushIsJudgedAsItsChild: a thread a relay child spawns reaches the declared child-push leg with the
// child's session_id (the live hook payload of a thread-spawned sub-agent carries its parent's session_id beside its own thread id
// as agent_id), so the leg judges its push exactly as the child's own: a push to dev warns, the child's own branch passes, a force,
// delete or mirror push onto dev or main stays the block (rule shared-branch-history). Each refusal leaves a record that names
// the leg, the rule, the session and the sub-agent's agent_id and never the command text. A sub-agent of a parent session is not
// judged and leaves no record.
func TestCRW1303SubagentThreadPushIsJudgedAsItsChild(t *testing.T) {
	f := newSubagentPushFixture(t)
	push := "git push origin HEAD:dev"
	want := f.answer(crw1303Kid, "", f.kid, push)
	w, warned := guardWarning(want)
	if !warned || !strings.Contains(w, "통합은 부모·통합기 몫") {
		t.Fatalf("the child's own push to dev: %q", want)
	}
	if got := f.answer(crw1303Kid, crw1303Spawned, f.kid, push); got != want {
		t.Errorf("a sub-agent thread's push to dev is not answered as the child's own:\nsub-agent: %s\nchild:     %s", got, want)
	}
	if out := f.answer(crw1303Kid, crw1303Spawned, f.kid, "git push origin HEAD:r2-x"); out != "" {
		t.Errorf("a sub-agent thread's push of the child's own branch: %q", out)
	}
	for _, command := range []string{"git push -f origin HEAD:dev", "git push --delete origin main", "git push --mirror origin"} {
		out := f.answer(crw1303Kid, crw1303Spawned, f.kid, command)
		if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "통합은 부모·통합기 몫") {
			t.Errorf("a sub-agent thread: %s: %s", command, out)
		}
	}
	before := len(f.records())
	for _, session := range []string{crw1303Parent, "01a13570-0000-7000-8000-000000000002"} {
		if out := f.answer(session, crw1303Spawned, f.parent, push); out != "" {
			t.Errorf("a sub-agent thread of session %s (no relay child) is judged: %q", session, out)
		}
		if out := f.answer(session, "", f.parent, "git push -f origin HEAD:dev"); out != "" {
			t.Errorf("session %s (no relay child) is judged: %q", session, out)
		}
	}
	if after := len(f.records()); after != before {
		t.Errorf("sessions the relay does not know as a child left %d decision records", after-before)
	}
	// The records: the sub-agent's warning and the three blocks name leg, rule, session and agent_id; the child's own warning has no agent.
	var warns, blocks, own int
	for _, r := range f.records() {
		if r["leg"] != childPushLeg || r["sessionId"] != crw1303Kid {
			t.Errorf("record %v", r)
		}
		switch r["decision"] {
		case "warn":
			if r["agentId"] == nil {
				own++
			} else if r["agentId"] == crw1303Spawned {
				warns++
			}
		case "block":
			if r["rule"] != "shared-branch-history" || r["agentId"] != crw1303Spawned {
				t.Errorf("block record %v", r)
			}
			blocks++
		}
	}
	if warns != 1 || own != 1 || blocks != 3 {
		t.Errorf("records: %d sub-agent warnings, %d child warnings, %d blocks, want 1, 1, 3: %v", warns, own, blocks, f.records())
	}
}

// TestCRW1303UnjudgeableLineageFailsOpenWithARecord: when the relay's record of the session cannot be read (settings that do not
// parse, a store that is not a database) a sub-agent thread's push is not blocked: it passes with the [crw warning] text and a
// decision record that names the sub-agent, never the command text, whatever the push (a force push onto dev included, since
// the session is not known to be a child). A command that spells no push passes silently.
func TestCRW1303UnjudgeableLineageFailsOpenWithARecord(t *testing.T) {
	f := newSubagentPushFixture(t)
	bad := filepath.Join(filepath.Dir(f.home), "bad.sqlite3")
	if err := os.WriteFile(bad, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	for what, settings := range map[string]string{
		"settings that cannot be parsed": "{",
		"a store that is not a database": strings.Replace(f.settings, `"dbPath": "`, `"dbPath": "`+bad+`", "x": "`, 1),
	} {
		f.writeSettings(settings)
		for _, command := range []string{"git push origin HEAD:dev", "git push -f origin HEAD:dev"} {
			out := f.answer(crw1303Kid, crw1303Spawned, f.kid, command)
			w, warned := guardWarning(out)
			if !warned || !strings.Contains(w, "cannot be read") || strings.Contains(out, "permissionDecision") {
				t.Errorf("%s: %s: %q", what, command, out)
			}
		}
		if out := f.answer(crw1303Kid, crw1303Spawned, f.kid, "ls -la"); out != "" {
			t.Errorf("%s: a command that spells no push: %q", what, out)
		}
	}
	rs := f.records()
	if len(rs) != 4 {
		t.Fatalf("%d decision records, want one per unjudged push (4): %v", len(rs), rs)
	}
	for _, r := range rs {
		if r["leg"] != childPushLeg || r["decision"] != "warn" || r["agentId"] != crw1303Spawned || r["sessionId"] != crw1303Kid || r["rule"] == "" || r["rule"] == "shared-branch-history" {
			t.Errorf("record %v", r)
		}
	}
}
