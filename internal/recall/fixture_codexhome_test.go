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

// buildIngestCodexHome adds ingest's metadata/state cases without changing the
// rollout fixture above. Dates stay relative to the clock, as fixtures.ts does.
func buildIngestCodexHome(t *testing.T) string {
	t.Helper()
	home := buildCodexHome(t, time.Now().UTC())
	files, err := ListRolloutFiles(home, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		head, rest, _ := strings.Cut(string(b), "\n")
		var meta map[string]any
		if err := json.Unmarshal([]byte(head), &meta); err != nil {
			t.Fatal(err)
		}
		p := meta["payload"].(map[string]any)
		switch p["id"] {
		case "old":
			p["cwd"], p["git"] = "/proj/beta", map[string]any{"repository_url": "git@github.com:example/beta.git"}
		case "archived":
			delete(p, "git")
		default:
			p["git"] = map[string]any{"repository_url": "https://github.com/example/alpha.git"}
		}
		writeRolloutTestFile(t, home, strings.TrimPrefix(f.Path, home+string(os.PathSeparator)), rolloutTestLine(t, meta)+rest)
	}
	state := recallDB(t, filepath.Join(home, "state_2.sqlite"))
	recallSQL(t, state, "CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT, cwd TEXT, git_branch TEXT, git_origin_url TEXT, updated_at_ms INTEGER)")
	for _, id := range []string{"main", "sub"} {
		if _, err := recallStmt(t, state, "INSERT INTO threads VALUES (?, '', '/proj/alpha', NULL, ?, 1)").Run(id, "https://github.com/example/alpha.git"); err != nil {
			t.Fatal(err)
		}
	}
	decoy := recallDB(t, filepath.Join(home, "state_1.sqlite"))
	recallSQL(t, decoy, "CREATE TABLE threads(id TEXT PRIMARY KEY)")
	t.Setenv("CODEX_HOME", home)
	return home
}
