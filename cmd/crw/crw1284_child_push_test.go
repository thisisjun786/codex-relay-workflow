package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
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
	// CRW-1307: the child's refusals warn (the push passes with the refusal as a warning), except a force, delete or --mirror
	// push onto dev or main, which stays a deny.
	for _, c := range []struct{ session, cwd, command, want string }{
		{"kid", kid, "git push origin HEAD:dev", "통합은 자식·통합기 몫"}, {"kid", kid, "git push origin HEAD:r2-x", ""}, {"kid", kid, "git push -u origin HEAD", "names no destination"},
		{"par", parent, "git push origin HEAD:dev", ""}, {"kid", parent, "git push origin HEAD:dev", "no branch of its own"},
	} {
		_, warned := guardWarning(answer(c.session, c.cwd, c.command))
		if out := answer(c.session, c.cwd, c.command); c.want == "" && out != "" || !strings.Contains(out, c.want) || c.want != "" && !warned {
			t.Errorf("%s in %s: %s: %s", c.session, c.cwd, c.command, out)
		}
	}
	for _, command := range []string{"git push -f origin HEAD:dev", "git push --delete origin main", "git push --mirror origin"} {
		if out := answer("kid", kid, command); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "통합은 자식·통합기 몫") {
			t.Errorf("kid: %s: %s", command, out)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "crw-completion-hook.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := answer("par", parent, "git push origin HEAD:dev"); !strings.Contains(out, "cannot be read") || strings.Contains(out, "permissionDecision") {
		t.Errorf("settings that cannot be read: %s", out)
	}
}

// TestChildPushGuardLegAnswersUnreadInput: the leg is behind the switch, and an input over the bound or one the host cancels before
// it arrives whole is released with a warning and a record (CRW-1307, fail-open), as the GitHub post guard releases it; a cancelled
// leg answers without waiting for its input.
func TestChildPushGuardLegAnswersUnreadInput(t *testing.T) {
	args := []string{"pre-tool-use", "--leg", "pre-tool-use-guarding-child-push"}
	if leg, ok := switchedLeg(args, componentHooks()); !ok || leg != args[2] {
		t.Errorf("the switch does not gate the leg: %q %v", leg, ok)
	}
	run := func(ctx context.Context, in io.Reader) string {
		var out bytes.Buffer
		done := make(chan struct{})
		go func() {
			defer close(done)
			if claimed, code := runComponentHook(invocation{ctx: ctx, args: args, stdout: &out}, in, componentHooks()); !claimed || code != 0 {
				t.Errorf("claimed %v, exit %d", claimed, code)
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the leg did not answer")
		}
		return out.String()
	}
	if out := run(context.Background(), strings.NewReader(strings.Repeat(" ", pabcdhook.GitHubPostMaxStdinBytes+1))); !strings.Contains(out, "could not be read") || strings.Contains(out, "permissionDecision") {
		t.Errorf("an input over the bound: %q", out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	if w, warned := guardWarning(run(ctx, in)); !warned || !strings.Contains(w, "the leg was cancelled") {
		t.Errorf("a cancelled leg: %q", w)
	}
}
