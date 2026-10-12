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

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestChildPushGuardLegReadsTheRelayStore (CRW-1284): the declared leg reads the relationship from the store the completion hook's
// settings name. The child is refused a push of dev and pushes its own branch; the parent, and any session of a home without those
// settings, is not judged; a child whose checkout has the parent's branch has no branch of its own; unreadable settings refuse.
func TestChildPushGuardLegReadsTheRelayStore(t *testing.T) {
	dir := t.TempDir()
	home, kid, parent, db := filepath.Join(dir, "codex"), filepath.Join(dir, "kid"), filepath.Join(dir, "parent"), filepath.Join(dir, "state", "relay.sqlite3")
	for repo, branch := range map[string]string{kid: "r2-x", parent: "dev"} {
		if out, err := exec.Command("git", "init", "-q", "-b", branch, repo).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	for _, d := range []string{home, filepath.Dir(db)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(context.Background(), db, filepath.Join(dir, "app.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-1','ISS-1','active','par','host',?,'kid','host',?,1,'[]','[]','2026-10-12T10:04:17Z','2026-10-12T10:04:17Z')", parent, kid); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	settings := `{"configVersion": 1, "event": "Stop", "relayExecutable": "/nonexistent/relay", "markerRoot": "` + dir + `/markers", "dbPath": "` + db +
		`", "mode": "observe", "timeoutSeconds": 5, "journalRoot": "` + dir + `/journal", "journalPolicy": "every_invocation", "installedBy": "CRW-37", "isolationAssertedBy": null}`
	t.Setenv("CODEX_HOME", home)
	answer := func(session, cwd, command string) string {
		payload, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": session, "turn_id": "t", "cwd": cwd, "tool_name": "Bash", "tool_input": map[string]any{"command": command}})
		var out bytes.Buffer
		claimed, code := runComponentHook(invocation{ctx: context.Background(), args: []string{"pre-tool-use", "--leg", "pre-tool-use-guarding-child-push"}, stdout: &out}, bytes.NewReader(payload), componentHooks())
		if !claimed || code != 0 {
			t.Fatalf("claimed %v, exit %d", claimed, code)
		}
		return out.String()
	}
	if out := answer("kid", kid, "git push origin HEAD:dev"); out != "" {
		t.Fatalf("a home without the completion hook's settings has no relay child: %s", out)
	}
	if err := os.WriteFile(filepath.Join(home, "crw-completion-hook.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ session, cwd, command, want string }{
		{"kid", kid, "git push origin HEAD:dev", "통합은 부모·통합기 몫"}, {"kid", kid, "git push origin r2-x", ""}, {"kid", kid, "git push -u origin HEAD", ""},
		{"par", parent, "git push origin HEAD:dev", ""}, {"kid", parent, "git push origin dev", "no branch of its own"},
	} {
		if out := answer(c.session, c.cwd, c.command); c.want == "" && out != "" || !strings.Contains(out, c.want) || c.want != "" && !strings.Contains(out, `"permissionDecision":"deny"`) {
			t.Errorf("%s in %s: %s: %s", c.session, c.cwd, c.command, out)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "crw-completion-hook.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := answer("par", parent, "git push origin HEAD:dev"); !strings.Contains(out, "cannot be read") {
		t.Errorf("settings that cannot be read: %s", out)
	}
}
