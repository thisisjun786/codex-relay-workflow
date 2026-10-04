package recall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func cwdPointerValue(p *string) any {
	if p != nil {
		return *p
	}
	return nil
}

// Node is a recorder only. The Go replay seeds temporary SQLite and markdown inputs.
func TestCwdContextOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/cwdcontext/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Kind               string
		In, Out, FoldedOut json.RawMessage
		Wire               []string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 100 {
		t.Fatal("oracle case grid unexpectedly small", len(cases))
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("%03d/%s", i, c.Kind), func(t *testing.T) {
			home := cwdTestHome(t)
			var got any
			switch c.Kind {
			case "sqliteStrings":
				var in struct{ SQL []string }
				if err := json.Unmarshal(c.In, &in); err != nil {
					t.Fatal(err)
				}
				idx := filepath.Join(home, "index.sqlite")
				db, err := openIndex(idx)
				if err != nil {
					t.Fatal(err)
				}
				for _, statement := range in.SQL {
					recallSQL(t, db, statement)
				}
				db.Close()
				got = ListCwdSessions("/repo", 5, CwdSessionOptions{IndexPath: idx, Home: home, ReadOriginUrl: func(string) string { return "" }})
			case "summary":
				var in struct {
					Files       map[string]string
					Dirs, Links []string
				}
				if err := json.Unmarshal(c.In, &in); err != nil {
					t.Fatal(err)
				}
				dir := "memories/rollout_summaries"
				if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
					t.Fatal(err)
				}
				for name, content := range in.Files {
					writeRolloutTestFile(t, home, filepath.Join(dir, name), content)
				}
				for _, name := range in.Dirs {
					if err := os.Mkdir(filepath.Join(home, dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				for _, name := range in.Links {
					if err := os.Symlink("absent-target", filepath.Join(home, dir, name)); err != nil {
						t.Fatal(err)
					}
				}
				got = LoadSummaryIndex(home)
			case "list":
				var in struct {
					Cwd, Origin *string
					N, Limit    *int
					Mode        string
					Legacy      bool
					Threads     [][]string
					Files       []struct {
						Path, Source, Date     string
						Cwd, ThreadID, RepoKey *string
						Msgs                   []struct {
							Text, Role string
							Synthetic  int
						}
					}
				}
				if err := json.Unmarshal(c.In, &in); err != nil {
					t.Fatal(err)
				}
				idx := filepath.Join(home, "index.sqlite")
				if in.Mode != "missing" {
					db, err := openIndex(idx)
					if err != nil {
						t.Fatal(err)
					}
					for _, file := range in.Files {
						cwd, src, date := "/repo", "main", "2026-01-02"
						if file.Cwd != nil {
							cwd = *file.Cwd
						}
						if file.Source != "" {
							src = file.Source
						}
						if file.Date != "" {
							date = file.Date
						}
						_, err := recallStmt(t, db, "INSERT INTO files(path,mtime_ms,size,thread_id,cwd,source,date,repo_key) VALUES(?,0,0,?,?,?,?,?)").Run(file.Path, cwdPointerValue(file.ThreadID), cwd, src, date, cwdPointerValue(file.RepoKey))
						if err != nil {
							t.Fatal(err)
						}
						for ord, message := range file.Msgs {
							role := message.Role
							if role == "" {
								role = "user"
							}
							_, err := recallStmt(t, db, "INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES(?,?,?, ?,?,?,?)").Run(file.Path, ord, "ts", role, "content", message.Synthetic, message.Text)
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					if in.Legacy {
						recallSQL(t, db, "DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key")
					}
					if in.Mode == "noMsgs" {
						recallSQL(t, db, "DROP TABLE msgs")
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
					if in.Mode == "broken" {
						if err := os.WriteFile(idx, []byte("not sqlite"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				if in.Threads != nil {
					db, err := openDbReadWrite(filepath.Join(home, "state_2.sqlite"))
					if err != nil {
						t.Fatal(err)
					}
					recallSQL(t, db, "CREATE TABLE threads(id TEXT PRIMARY KEY,title TEXT,cwd TEXT,git_branch TEXT,git_origin_url TEXT,updated_at_ms INTEGER)")
					for _, row := range in.Threads {
						if _, err := recallStmt(t, db, "INSERT INTO threads VALUES(?, '', '/other',NULL,?,0)").Run(row[0], row[1]); err != nil {
							t.Fatal(err)
						}
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
				}
				cwd, n, origin := "/repo", 5, ""
				if in.Cwd != nil {
					cwd = *in.Cwd
				}
				if in.N != nil {
					n = *in.N
				}
				if in.Origin != nil {
					origin = *in.Origin
				}
				sessions := ListCwdSessions(cwd, n, CwdSessionOptions{IndexPath: idx, Home: home, ExcerptChars: in.Limit, ReadOriginUrl: func(string) string { return origin }})
				got = sessions
				// Check surrogate escapes BEFORE decoding JSON can replace them with U+FFFD.
				wire, err := json.Marshal(sessions)
				if err != nil {
					t.Fatal(err)
				}
				var encoded []map[string]json.RawMessage
				if err := json.Unmarshal(wire, &encoded); err != nil {
					t.Fatal(err)
				}
				for j, quote := range c.Wire {
					if strings.Contains(quote, `\ud`) && string(encoded[j]["excerpt"]) != quote {
						t.Fatalf("serialized excerpt %s, oracle %s", encoded[j]["excerpt"], quote)
					}
				}
			default:
				t.Fatal("unknown oracle kind", c.Kind)
			}
			var want any
			expected := c.Out
			if FoldCwdCase() && len(c.FoldedOut) != 0 {
				expected = c.FoldedOut
			}
			if err := json.Unmarshal(expected, &want); err != nil {
				t.Fatal(err)
			}
			if actual := canon(t, got); !reflect.DeepEqual(actual, want) {
				t.Fatalf("got %v, oracle %v", actual, want)
			}
		})
	}
}

func TestCwdContextClipJSON(t *testing.T) {
	for _, c := range []struct {
		text  string
		limit int
		quote string
	}{
		{"😀abc", 4, `"\ud83d..."`}, {"😀abcdef", 5, `"😀..."`}, {"�abc", 4, `"�abc"`},
		{"abcdef", 0, `"abc..."`}, {"abcdef", 1, `"abcd..."`}, {"abcdef", 2, `"abcde..."`}, {"abcdef", -10, `"..."`},
	} {
		excerpt, clip := cwdExcerpt(c.text, c.limit)
		session := CwdSession{Excerpt: excerpt, excerptClip: clip}
		encoded, err := json.Marshal(session)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["excerpt"]) != c.quote {
			t.Fatalf("limit %d: %s, want %s", c.limit, fields["excerpt"], c.quote)
		}
		if clip != nil {
			session.Excerpt = "changed �"
			encoded, err := json.Marshal(session)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `"excerpt":"changed �"`) || strings.Contains(string(encoded), `\ud83d`) {
				t.Fatal("stale metadata overrode caller-modified excerpt", string(encoded))
			}
		}
	}
}

func TestCwdContextLegacyJoinAndBound(t *testing.T) {
	home := cwdTestHome(t)
	idx := cwdTestIndex(t, home)
	db, err := openIndex(idx)
	if err != nil {
		t.Fatal(err)
	}
	recallSQL(t, db, "DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key")
	db.Close()
	state, err := openDbReadWrite(filepath.Join(home, "state_2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	recallSQL(t, state, "CREATE TABLE threads(id TEXT PRIMARY KEY,title TEXT,cwd TEXT,git_branch TEXT,git_origin_url TEXT,updated_at_ms INTEGER); BEGIN")
	stmt := recallStmt(t, state, "INSERT INTO threads VALUES(?, '', '/checkout',NULL,?,0)")
	for i := 0; i < 5001; i++ {
		id := fmt.Sprintf("id-%04d", i)
		if i == 4999 {
			id = "a4"
		}
		if i == 5000 {
			id = "a3"
		}
		if _, err := stmt.Run(id, cwdFixtureOrigin); err != nil {
			t.Fatal(err)
		}
	}
	recallSQL(t, state, "COMMIT")
	state.Close()
	ids := sameOriginThreadIDs(home, normalizeRepoKey(cwdFixtureOrigin))
	if len(ids) != 5000 || ids[4999] != "a4" {
		t.Fatal("metadata ID bound/order", len(ids), ids[len(ids)-1])
	}
	opts := CwdSessionOptions{IndexPath: idx, ReadOriginUrl: func(string) string { return cwdFixtureOrigin }}
	if got := ListCwdSessions(cwdFixturePath, 5, opts); len(got) != 3 || got[0].Excerpt != "land the parser rewrite" {
		t.Fatalf("legacy join (including 5000 binds): %#v", got)
	}
	if err := os.WriteFile(filepath.Join(home, "state_3.sqlite"), []byte("bad metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ListCwdSessions(cwdFixturePath, 5, opts); len(got) != 2 {
		t.Fatal("metadata failure must keep exact-cwd results", got)
	}
	if got := ListCwdSessions(cwdFixturePath, 1, opts); len(got) != 1 || got[0].Excerpt != "audit the envelope" {
		t.Fatal("topN", got)
	}
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
			synthetic := 0
			if entry.Synthetic {
				synthetic = 1
			}
			_, err = recallStmt(t, db, "INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES(?,?,?,?,?,?,?)").Run(file.Path, i, entry.TS, entry.Role, entry.MatchField, synthetic, entry.Text)
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
