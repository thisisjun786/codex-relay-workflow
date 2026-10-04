package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRolloutTestFile(t *testing.T, home, name, content string) string {
	t.Helper()
	path := filepath.Join(home, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func rolloutTestLine(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

// buildCodexHome ports the rollout portion of recall/test/fixtures.ts:25-130.
// Later memory/database fixtures extend this substrate; no real Codex home is consulted.
func buildCodexHome(t *testing.T, now time.Time) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	for _, f := range []struct {
		id           string
		days         int
		sub, archive bool
	}{
		{"main", 0, false, false}, {"sub", 0, true, false}, {"old", 40, false, false}, {"archived", 10, false, true},
	} {
		d := now.Add(-time.Duration(f.days) * 24 * time.Hour).UTC()
		p := map[string]any{"id": f.id, "cwd": "/proj/alpha", "instructions": strings.Repeat("x", 40000), "originator": "codex-tui", "git": map[string]any{"repository_url": "https://example.test/group/repo.git"}}
		if f.sub {
			p["thread_source"], p["agent_nickname"], p["originator"] = "subagent", "Popper", "codex_exec"
		}
		doc := rolloutTestLine(t, map[string]any{"type": "session_meta", "payload": p})
		for _, message := range []struct{ role, text string }{{"user", "# AGENTS.md instructions injected"}, {"user", "please deploy the index"}, {"assistant", "index deployed"}} {
			doc += rolloutTestLine(t, map[string]any{"type": "response_item", "timestamp": d.Format(time.RFC3339), "payload": map[string]any{"type": "message", "role": message.role, "content": []any{map[string]any{"type": "input_text", "text": message.text}}}})
		}
		name := "rollout-" + d.Format("2006-01-02T15-04-05") + "-" + f.id + ".jsonl"
		dir := filepath.Join("sessions", d.Format("2006/01/02"))
		if f.archive {
			dir = "archived_sessions"
		}
		writeRolloutTestFile(t, home, filepath.Join(dir, name), doc)
	}
	return home
}
