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

// The complete synthetic substrate of recall/test/fixtures.ts. The original
// rollout-only buildCodexHome above remains unchanged for its existing callers.
const (
	recallThreadMain     = "019f0000-0000-7000-8000-00000000aaaa"
	recallThreadSub      = "019f0000-0000-7000-8000-00000000bbbb"
	recallThreadOld      = "019f0000-0000-7000-8000-00000000cccc"
	recallThreadArchived = "019f0000-0000-7000-8000-00000000eeee"
	recallOriginAlpha    = "https://github.com/example/alpha.git"
	recallOriginBeta     = "git@github.com:example/beta.git"
)

func recallFixtureMessage(t *testing.T, role, text, iso string) string {
	t.Helper()
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return rolloutTestLine(t, map[string]any{"timestamp": iso, "type": "response_item", "payload": map[string]any{
		"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}},
	}})
}

func recallFixtureMeta(t *testing.T, id, cwd, iso, origin string, sub bool) string {
	t.Helper()
	p := map[string]any{"id": id, "timestamp": iso, "cwd": cwd, "originator": "codex-tui", "cli_version": "0.130.0", "instructions": strings.Repeat("x", 40000)}
	if origin != "" {
		p["git"] = map[string]any{"repository_url": origin, "branch": "main"}
	}
	if sub {
		p["originator"], p["thread_source"], p["agent_nickname"] = "codex_exec", "subagent", "Popper"
		p["source"] = map[string]any{"subagent": map[string]any{"parent_thread_id": recallThreadMain, "depth": 1}}
	}
	return rolloutTestLine(t, map[string]any{"timestamp": iso, "type": "session_meta", "payload": p})
}

func recallFixtureTool(t *testing.T, name, args, output, iso string) string {
	t.Helper()
	return rolloutTestLine(t, map[string]any{"timestamp": iso, "type": "response_item", "payload": map[string]any{
		"type": "function_call", "name": name, "arguments": args, "call_id": "tool_1",
	}}) + rolloutTestLine(t, map[string]any{"timestamp": iso, "type": "response_item", "payload": map[string]any{
		"type": "function_call_output", "call_id": "tool_1", "output": output,
	}})
}

func recallFixtureRollout(t *testing.T, home string, now time.Time, days int, hour, id, cwd, origin string, sub, archived bool, messages [][2]string) string {
	t.Helper()
	d := now.Add(-time.Duration(days) * 24 * time.Hour).UTC()
	iso := d.Format("2006-01-02T15:04:05.000Z")
	doc := recallFixtureMeta(t, id, cwd, iso, origin, sub)
	for _, m := range messages {
		doc += recallFixtureMessage(t, m[0], m[1], iso)
	}
	dir := filepath.Join("sessions", d.Format("2006/01/02"))
	if archived {
		dir = "archived_sessions"
	}
	return writeRolloutTestFile(t, home, filepath.Join(dir, "rollout-"+d.Format("2006-01-02")+"T"+hour+"-00-00-"+id+".jsonl"), doc)
}

func recallFixtureDB(t *testing.T, home, name, schema, insert string, rows ...[]any) {
	t.Helper()
	db, err := openDbReadWrite(filepath.Join(home, name))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if insert == "" {
		return
	}
	stmt, err := db.Prepare(insert)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, err := stmt.Run(row...); err != nil {
			t.Fatal(err)
		}
	}
}

func buildRecallCodexHome(t *testing.T, now time.Time) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	main := recallFixtureRollout(t, home, now, 0, "01", recallThreadMain, "/proj/alpha", recallOriginAlpha, false, false, [][2]string{
		{"user", "# AGENTS.md instructions injected preamble mentioning zebra"},
		{"user", "please deploy the trigram index for korean search"},
		{"assistant", "deployed the trigram index; korean 한글 검색 works now"},
	})
	iso := now.UTC().Format("2006-01-02T15:04:05.000Z")
	more := recallFixtureTool(t, "exec_command", `{"cmd":"rg zebra-in-tool-arguments"}`, "tool output: zebra-in-tool-output", iso) +
		recallFixtureMessage(t, "user", "한글 트라이그램 결과 확인해줘", iso) + recallFixtureMessage(t, "assistant", "확인 완료: 트라이그램 인덱스 정상", iso)
	f, err := os.OpenFile(main, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(more); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	recallFixtureRollout(t, home, now, 0, "02", recallThreadSub, "/proj/alpha/sub", recallOriginAlpha, true, false, [][2]string{{"user", "subagent task about the trigram index"}, {"assistant", "subagent reply mentioning zebra too"}})
	recallFixtureRollout(t, home, now, 40, "01", recallThreadOld, "/proj/beta", recallOriginBeta, false, false, [][2]string{{"user", "ancient question about the trigram index"}, {"assistant", "ancient answer"}})
	recallFixtureRollout(t, home, now, 10, "05", recallThreadArchived, "/proj/alpha", "", false, true, [][2]string{{"user", "archived aardwolf question about the trigram index"}, {"assistant", "archived aardwolf answer"}})
	recallFixtureDB(t, home, "state_2.sqlite", "CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '', git_branch TEXT, git_origin_url TEXT, updated_at_ms INTEGER)", "INSERT INTO threads VALUES (?, ?, ?, ?, ?, ?)",
		[]any{recallThreadMain, "deploy trigram index", "/proj/alpha", "main", recallOriginAlpha, now.UnixMilli()}, []any{recallThreadSub, "subagent lane", "/proj/alpha/sub", nil, recallOriginAlpha, now.UnixMilli()})
	recallFixtureDB(t, home, "state_1.sqlite", "CREATE TABLE threads (id TEXT PRIMARY KEY)", "")
	writeRolloutTestFile(t, home, "memories/MEMORY.md", "# Task Group: search infrastructure\n\n## Task 1: ship the trigram sidecar index, success\n\nkeywords: trigram, sidecar, 한글 검색\n")
	writeRolloutTestFile(t, home, "memories/windows-notes.md", "# CRLF notes\r\n\r\nthe wombat migration finished on windows\r\n")
	writeRolloutTestFile(t, home, "memories/rollout_summaries/summary-aaa.md", "thread_id: "+recallThreadMain+"\nupdated_at: "+iso+"\n\n# Deployed the trigram index\n\nRollout context: korean trigram deployment succeeded.\n")
	recallFixtureDB(t, home, "memories_1.sqlite", "CREATE TABLE stage1_outputs (thread_id TEXT PRIMARY KEY, source_updated_at INTEGER NOT NULL, raw_memory TEXT NOT NULL, rollout_summary TEXT NOT NULL)", "INSERT INTO stage1_outputs VALUES (?, ?, ?, ?)",
		[]any{recallThreadMain, now.Unix(), "raw memory about trigram deployment", "summary duplicate"}, []any{recallThreadSub, now.Unix(), "db-only memory row about quagga migrations", "quagga summary"})
	return home
}

// addRecallNlGoldenCorpus ports the optional natural-language fixture extension.
func addRecallNlGoldenCorpus(t *testing.T, home string, now time.Time) {
	t.Helper()
	for _, row := range []struct{ hour, suffix, user, reply string }{
		{"11", "d333", "로컬 소스를 실제 서비스에 연결했다", "로컬 소스를 실제 서비스에 연결하고 정상 동작까지 확인. bun link 로 두 CLI 를 묶고 healthz 10100 응답."},
		{"12", "d222", "코덱스 재시작하니 플러그인이 사라짐", "Codex를 재시작하면 BundledPluginsMarketplace 플러그인이 사라지는 문제가 재현된다. plugin wipe."},
		{"13", "d111", "2.49.0 provenance 확인해", "2.49.0 npm latest gitHead가 그 소스인지 검증한 기록이다. 패키지가 provenance SLSA 로 확인됐다."},
	} {
		recallFixtureRollout(t, home, now, 0, row.hour, "019f0000-0000-7000-8000-00000000"+row.suffix, "/proj/alpha", recallOriginAlpha, false, false, [][2]string{{"user", row.user}, {"assistant", row.reply}})
	}
	for _, row := range [][2]string{
		{"nl-d3.md", "# dogfooding\n\n로컬 소스를 실제 서비스에 연결하고 정상 동작까지 확인. bun link healthz 10100.\n"},
		{"nl-p2.md", "# plugin wipe\n\nCodex restart 후 BundledPluginsMarketplace 플러그인이 사라지는 문제.\n"},
		{"nl-r2.md", "# 2.49.0 provenance\n\n2.49.0 npm 패키지가 그 소스인지 검증한 기록. gitHead SLSA.\n"},
		{"nl-c4-mismatch.md", "# Notes\n\nTouched NaiControlsPanel and negativePrompt.\n"},
		{"nl-c4-real.md", "# Real\n\nThe LSP server crashed.\n"},
		{"nl-c5.md", "# 운영 노트\n\n첫 배포 이후 인덱스 재생성이 필요했다.\n"},
	} {
		writeRolloutTestFile(t, home, "memories/"+row[0], row[1])
	}
}
