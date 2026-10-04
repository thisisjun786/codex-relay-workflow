package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const cwdFixturePath = "/hash/worktrees/slot/project"
const cwdFixtureOrigin = "https://example.test/group/project.git"

func cwdTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CRW_HOME", filepath.Join(home, "sidecar"))
	return home
}

// Seed the real index from the landed rollout helpers, not a second ingest implementation.
// The separate ingest port owns incremental ingest; this fixture only inserts parsed rows.
func cwdTestIndex(t *testing.T, home string) string {
	t.Helper()
	for _, row := range []struct {
		id, cwd, origin string
		messages        []string
		sub             bool
	}{
		{"a1", cwdFixturePath, "", []string{"<recommended_plugins> harness", "wire the hook budget"}, false},
		{"a2", cwdFixturePath, "", []string{"audit the envelope"}, false},
		{"a3", "/other/project", "https://example.test/group/other.git", []string{"foreign project opener"}, false},
		{"a4", "/checkout/project", cwdFixtureOrigin, []string{"land the parser rewrite"}, false},
		{"a5", cwdFixturePath, cwdFixtureOrigin, []string{"subagent opener"}, true},
	} {
		p := map[string]any{"id": row.id, "cwd": row.cwd}
		if row.origin != "" {
			p["git"] = map[string]any{"repository_url": row.origin}
		}
		if row.sub {
			p["thread_source"] = "subagent"
		}
		doc := rolloutTestLine(t, map[string]any{"type": "session_meta", "payload": p})
		for _, message := range row.messages {
			doc += rolloutTestLine(t, map[string]any{"type": "response_item", "timestamp": "2026-01-02T00:00:00Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": message}}}})
		}
		writeRolloutTestFile(t, home, "sessions/2026/01/02/"+row.id+".jsonl", doc)
	}
	idx, err := indexPath()
	if err != nil {
		t.Fatal(err)
	}
	db, err := openIndex(idx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	files, err := ListRolloutFiles(home, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		meta, err := ReadRolloutMeta(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := ParseRollout(string(content), false)
		if err != nil {
			t.Fatal(err)
		}
		value := func(p *string) any {
			if p != nil {
				return *p
			}
			return nil
		}
		_, err = recallStmt(t, db, "INSERT INTO files(path,mtime_ms,size,thread_id,cwd,source,date,repo_key) VALUES(?,0,?,?,?,?,?,?)").Run(file.Path, len(content), value(meta.ThreadID), value(meta.Cwd), string(meta.Source), file.Date, value(meta.RepoKey))
		if err != nil {
			t.Fatal(err)
		}
		for i, entry := range entries {
			_, err = recallStmt(t, db, "INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES(?,?,?,?,?,?,?)").Run(file.Path, i, entry.TS, entry.Role, entry.MatchField, entry.Synthetic, entry.Text)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return idx
}

// CXC recall/test/cwd-context.test.ts:122-176: cwd, scope, federation and fallback.
func TestCwdContextEnumeration(t *testing.T) {
	home := cwdTestHome(t)
	idx := cwdTestIndex(t, home)
	options := CwdSessionOptions{IndexPath: idx, Home: home, ReadOriginUrl: func(string) string { return "" }}
	got := ListCwdSessions(cwdFixturePath, 5, options)
	if got == nil {
		t.Fatal("present index returned fallback instead of cwd sessions")
	}
	excerpts := []string{}
	for _, session := range got {
		excerpts = append(excerpts, session.Excerpt)
	}
	if want := []string{"audit the envelope", "wire the hook budget"}; !reflect.DeepEqual(excerpts, want) {
		t.Fatalf("excerpts %v, want %v", excerpts, want)
	}
	if empty := ListCwdSessions("/nonexistent/cwd", 5, options); empty == nil || len(empty) != 0 {
		t.Fatalf("no matches must be [], got %#v", empty)
	}
	options.ReadOriginUrl = func(string) string { return cwdFixtureOrigin }
	if federation := ListCwdSessions(cwdFixturePath, 5, options); len(federation) != 3 || federation[0].Excerpt != "land the parser rewrite" {
		t.Fatalf("same-origin federation = %#v", federation)
	}
	options.ReadOriginUrl = func(string) string { return "git@example.test:group/unrelated.git" }
	if other := ListCwdSessions(cwdFixturePath, 5, options); len(other) != 2 {
		t.Fatalf("foreign origin = %#v", other)
	}
	for _, query := range []struct {
		cwd string
		n   int
	}{{"", 5}, {cwdFixturePath, 0}, {cwdFixturePath, -1}} {
		if result := ListCwdSessions(query.cwd, query.n, options); result != nil {
			t.Fatal("invalid request did not fall back", result)
		}
	}
	options.IndexPath = filepath.Join(home, "absent", "index.sqlite")
	if ListCwdSessions(cwdFixturePath, 5, options) != nil {
		t.Fatal("absent index did not fall back")
	}
}

// CXC recall/test/cwd-context.test.ts:211-252: summary heading and failure tolerance.
func TestCwdContextSummaries(t *testing.T) {
	home := cwdTestHome(t)
	writeRolloutTestFile(t, home, "memories/rollout_summaries/good.md", "thread_id: a1\ncwd: /repo\n\n# Fixed the parser\n\n# Second heading\n")
	writeRolloutTestFile(t, home, "memories/rollout_summaries/headless.md", "thread_id: a2\ncwd: /repo\n")
	writeRolloutTestFile(t, home, "memories/rollout_summaries/bare.md", "# Heading without id\n")
	writeRolloutTestFile(t, home, "memories/rollout_summaries/notes.txt", "thread_id: a3\n# Ignored\n")
	want := map[string]SummaryEntry{"a1": {Relpath: "good.md", Title: "Fixed the parser"}}
	if got := LoadSummaryIndex(home); !reflect.DeepEqual(got, want) {
		t.Fatalf("summaries %#v, want %#v", got, want)
	}
	if got := LoadSummaryIndex(filepath.Join(home, "missing")); got == nil || len(got) != 0 {
		t.Fatal("missing summaries must return an empty map", got)
	}
	if got := LoadSummaryIndex(); !reflect.DeepEqual(got, want) {
		t.Fatal("default home", got)
	}
	if _, err := json.Marshal(want); err != nil {
		t.Fatal(err)
	}
}
