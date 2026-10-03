package hook_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

const threadAllow = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
const threadFull = "approval_policy = \"never\"\nsandbox_mode = \"danger-full-access\"\n"
const threadOpt = `{"permissions":{"agentCreatedThreadAutoAllow":true}}`
const threadMeta = `{"type":"session_meta","payload":{"id":"fixture-id","thread_source":"agent_created_thread"}}`

type threadFixture struct {
	t                                    *testing.T
	root, cwd, codex, global, transcript string
	input                                map[string]any
}

func threadSetup(t *testing.T) *threadFixture {
	t.Helper()
	r := t.TempDir()
	f := &threadFixture{t: t, root: r, cwd: filepath.Join(r, "project"), codex: filepath.Join(r, "codex"), global: filepath.Join(r, "global"), transcript: filepath.Join(r, "rollout.jsonl")}
	for _, d := range []string{f.cwd, f.codex, f.global} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{"HOME": r, "CODEX_HOME": f.codex, "CODEX_SQLITE_HOME": f.codex, "CRW_HOME": f.global, "CRW_PABCD": ""} {
		t.Setenv(k, v)
	}
	f.write(f.transcript, threadMeta+"\n")
	f.config(threadFull)
	f.write(filepath.Join(f.global, "config.json"), threadOpt)
	f.input = map[string]any{"hook_event_name": "PermissionRequest", "cwd": f.cwd, "session_id": "fixture-id", "transcript_path": f.transcript, "permission_mode": "default", "tool_name": "Bash"}
	return f
}

func (f *threadFixture) write(path, s string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *threadFixture) config(s string) { f.write(filepath.Join(f.codex, "config.toml"), s) }
func (f *threadFixture) remove(path string) {
	f.t.Helper()
	if err := os.Remove(path); err != nil {
		f.t.Fatal(err)
	}
}
func (f *threadFixture) link(target, path string) {
	f.t.Helper()
	if err := os.Symlink(target, path); err != nil {
		f.t.Fatal(err)
	}
}
func (f *threadFixture) raw(event, raw string) string {
	f.t.Helper()
	leg := "permission-request-allowing-agent-thread"
	if event == "session-start" {
		leg = "session-start-advising-agent-thread-permissions"
	}
	var out, stderr strings.Builder
	code := harness.Hook(context.Background(), []string{event, "--leg", leg}, strings.NewReader(raw), &out, &stderr, os.LookupEnv, harness.Legs())
	if code != 0 || stderr.Len() != 0 {
		f.t.Fatalf("hook exit=%d stderr=%q", code, stderr.String())
	}
	return out.String()
}
func (f *threadFixture) send(advisory bool, change map[string]any) string {
	f.t.Helper()
	p := make(map[string]any)
	for k, v := range f.input {
		p[k] = v
	}
	event := "permission-request"
	if advisory {
		event, p["hook_event_name"] = "session-start", "SessionStart"
	}
	for k, v := range change {
		p[k] = v
	}
	b, err := json.Marshal(p)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.raw(event, string(b))
}
func (f *threadFixture) want(advisory bool, change map[string]any, want string) {
	f.t.Helper()
	if got := f.send(advisory, change); got != want {
		f.t.Fatalf("change=%v: got %q, want %q", change, got, want)
	}
}
func (f *threadFixture) advised(change map[string]any) {
	f.t.Helper()
	got := f.send(true, change)
	var p struct {
		Message string `json:"systemMessage"`
		Output  struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(got), &p) != nil || !strings.HasSuffix(got, "\n") || p.Output.Event != "SessionStart" ||
		!strings.Contains(p.Message, "Full Access") || !strings.Contains(p.Message, "agentCreatedThreadAutoAllow") ||
		!strings.Contains(p.Message, "one-time network") || !strings.Contains(p.Output.Context, "network") || !strings.Contains(p.Output.Context, "git") {
		f.t.Fatalf("bad advisory: %q", got)
	}
}
func (f *threadFixture) snapshot() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(f.root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.root, path)
		if e.IsDir() {
			out[rel] = "directory"
			return nil
		}
		b, err := os.ReadFile(path)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// All 26 oracle declarations, including the four generated grammar cases (29 runnable cases).
// Oracle: CXC v0.2.40, pabcd-state/test/agent-thread-permissions.test.ts:39-396.
func TestAgentThreadOracle(t *testing.T) {
	cases := []struct {
		name string
		run  func(*threadFixture)
	}{
		{"covered tools", func(f *threadFixture) {
			for _, tool := range []string{"Bash", "write_stdin", "apply_patch", "mcp__codex_app__create_worktree", "mcp__codex_app__create_thread"} {
				f.want(false, map[string]any{"tool_name": tool}, threadAllow)
			}
			f.want(false, map[string]any{"tool_input": map[string]any{"description": "network-access example.com"}}, threadAllow)
		}},
		{"overflowing numbers", func(f *threadFixture) {
			for _, v := range []string{"1e9999", "-1e400", "9223372036854775808", "0x8000000000000000", "-9223372036854775809"} {
				f.config(threadFull + "foo = " + v + "\n")
				f.want(false, nil, "")
			}
			f.config(threadFull + "foo = 1e300\nbar = 9007199254740993\nbaz = 9223372036854775807\nqux = -9223372036854775808\nhex = 0x7fff_ffff_ffff_ffff\n")
			f.want(false, nil, threadAllow)
		}},
		{"symlinked default directories", func(f *threadFixture) {
			f.t.Setenv("CODEX_HOME", "")
			f.t.Setenv("CRW_HOME", "")
			pc, pg := filepath.Join(f.cwd, ".codex"), filepath.Join(f.cwd, ".crw")
			f.write(filepath.Join(pc, "config.toml"), threadFull)
			f.write(filepath.Join(pg, "config.json"), threadOpt)
			f.write(filepath.Join(f.root, ".crw", "config.json"), threadOpt)
			f.link(pc, filepath.Join(f.root, ".codex"))
			f.want(false, map[string]any{"cwd": f.root}, "")
			f.remove(filepath.Join(f.root, ".codex"))
			f.write(filepath.Join(f.root, ".codex", "config.toml"), threadFull)
			f.remove(filepath.Join(f.root, ".crw", "config.json"))
			f.remove(filepath.Join(f.root, ".crw"))
			f.link(pg, filepath.Join(f.root, ".crw"))
			f.want(false, map[string]any{"cwd": f.root}, "")
		}},
		{"dates times and multiline inline tables", func(f *threadFixture) {
			for _, v := range []string{"2026-13-42", "25:99:99", "[2026-99-99]", "2026-02-30", "2026-01-01T10:00:00+24:00", "{ a = 1,\n b = 2 }"} {
				f.config(threadFull + "foo = " + v + "\n")
				f.want(false, nil, "")
				f.want(true, nil, "")
			}
			for _, v := range []string{"2024-02-29", "1979-05-27T07:32:00Z", "07:32:00", "{ a = 1, b = \"x\" }"} {
				f.config(threadFull + "foo = " + v + "\n")
				f.want(false, nil, threadAllow)
			}
		}},
		{"network access with opt in", func(f *threadFixture) {
			f.want(false, map[string]any{"tool_input": map[string]any{"description": "network-access example.com"}}, threadAllow)
		}},
		{"network access without opt in", func(f *threadFixture) {
			f.write(filepath.Join(f.global, "config.json"), "{}")
			f.want(false, map[string]any{"tool_input": map[string]any{"description": "network-access example.com"}}, "")
		}},
		{"default off global setting", func(f *threadFixture) {
			path := filepath.Join(f.global, "config.json")
			f.remove(path)
			f.want(false, nil, "")
			for _, v := range []string{"{}", "{", `{"permissions":[]}`, `{"permissions":{"agentCreatedThreadAutoAllow":false}}`, `{"permissions":{"agentCreatedThreadAutoAllow":"true"}}`} {
				f.write(path, v)
				f.want(false, nil, "")
			}
		}},
		{"missing corrupt mismatched rollout", func(f *threadFixture) {
			for _, c := range []map[string]any{{"session_id": ""}, {"session_id": nil}, {"transcript_path": nil}, {"transcript_path": ""}, {"transcript_path": filepath.Join(f.root, "absent")}, {"transcript_path": "relative.jsonl"}} {
				f.want(false, c, "")
			}
			for _, v := range []string{"", "{bad", `{"type":"other","payload":{"id":"fixture-id","thread_source":"agent_created_thread"}}`, `{"type":"session_meta"}`, `{"type":"session_meta","payload":{}}`, `{"type":"session_meta","payload":{"id":"wrong","thread_source":"agent_created_thread"}}`, `{"type":"session_meta","payload":{"id":"fixture-id"}}`, `{"type":"session_meta","payload":{"id":"fixture-id","thread_source":"user"}}`, `{"padding":"` + strings.Repeat("x", 65536) + `"}`, "{\"type\":\"other\"}\n" + threadMeta} {
				f.write(f.transcript, v+"\n")
				f.want(false, nil, "")
			}
		}},
		{"nondefault and subagent presence", func(f *threadFixture) {
			for _, c := range []map[string]any{{"permission_mode": nil}, {"permission_mode": "full-access"}, {"agent_id": ""}, {"agent_id": nil}, {"agent_type": ""}, {"agent_type": nil}} {
				f.want(false, c, "")
			}
		}},
		{"forked user and subagent source", func(f *threadFixture) {
			for _, p := range []string{`{"id":"fixture-id","thread_source":"user"}`, `{"id":"fixture-id","thread_source":"subagent"}`, `{"id":"fixture-id","thread_source":"agent_created_thread","forked_from_id":"parent"}`} {
				f.write(f.transcript, `{"type":"session_meta","payload":`+p+"}\n")
				f.want(false, nil, "")
			}
		}},
		{"unknown conflicting Codex config", func(f *threadFixture) {
			f.remove(filepath.Join(f.codex, "config.toml"))
			f.want(false, nil, "")
			for _, v := range []string{"approval_policy = \"never\nsandbox_mode = \"danger-full-access\"", threadFull + "invalid top-level", threadFull + "approval_policy = \"never\"", "approval_policy = \"never\"", "sandbox_mode = \"danger-full-access\"", "approval_policy = \"on-request\"\nsandbox_mode = \"danger-full-access\"", "approval_policy = \"never\"\nsandbox_mode = \"workspace-write\"", threadFull + "profile = \"team\"", threadFull + "\"profile\" = \"team\"", threadFull + "[[profiles]", threadFull + "[profiles]]", threadFull + "[features]\nnot_valid = [", threadFull + "[features]\nx = \"unterminated", threadFull + "[features]\nx = 1 2", threadFull + "[features]\nx = [1 2]", threadFull + "[features]\nx = {a = 1 b = 2}"} {
				f.config(v)
				f.want(false, nil, "")
			}
			for _, tail := range []string{"[features]\nenabled = true\n[sandbox_workspace_write]\nwritable_roots = [\n \"/fixture\",\n]\n", "[features]\nsettings = { enabled = true, retries = 2 }\nnotes = \"\"\"hello\nworld\"\"\"\nraw = '''one\ntwo'''\n"} {
				f.config(threadFull + tail)
				f.want(false, nil, threadAllow)
			}
			f.config(threadFull + strings.Repeat("#", 1024*1024))
			f.want(false, nil, "")
		}},
		{"invalid header keys", func(f *threadFixture) {
			f.grammar([]string{"[bad key]\nx = true\n", "[x. bad key]\ny = 1\n", "[[bad key]]\ny = 1\n"})
		}},
		{"invalid numeric underscores", func(f *threadFixture) {
			for _, v := range []string{"1__2", "1_", "1._2", "1.2__3", "1e_2", "1e2_", "0x_1", "0x1__2", "0o7_", "0b1__0"} {
				f.grammar([]string{"foo = " + v + "\n"})
			}
		}},
		{"invalid basic string escapes", func(f *threadFixture) { f.grammar([]string{`foo = "\q"`, `foo = """\q"""`, `"\q" = 1`}) }},
		{"duplicate unrelated keys", func(f *threadFixture) {
			f.grammar([]string{"foo = 1\nfoo = 2\n", "foo = 1\n\"foo\" = 2\n", "[x]\na = 1\n\"a\" = 2\n", "a = 1\na.b = 2\n"})
		}},
		{"real dotted literal and multiline tables", func(f *threadFixture) {
			f.config(threadFull + "[features]\nsearch = true\nalpha . 'beta gamma' = 2\n[projects.\"/a/b\"]\ntrust_level = \"trusted\"\n[hooks.state.\"x@y:z.json:pre_tool_use:0:0\"]\nenabled = true\n[sandbox_workspace_write]\nwritable_roots = [\n \"/fixture-a\",\n \"/fixture-b\",\n]\n[literal.'raw key']\nvalue = 1\n")
			f.want(false, nil, threadAllow)
			f.advised(nil)
			f.config(threadFull + "[numbers]\nint = 1_000\nfloat = 1_000.2_50e+1_0\nhex = 0xA_B\noctal = 0o7_1\nbinary = 0b1_0\nmultiline = \"\"\"hello\\\n world\"\"\"\n")
			f.want(false, nil, threadAllow)
			f.config(threadFull + "foo = 1\nfoo.bar = 2\n")
			f.want(false, nil, "")
		}},
		{"repeated array and standard tables", func(f *threadFixture) {
			f.config(threadFull + "[[x]]\nkey = 1\n[[x]]\nkey = 2\n")
			f.want(false, nil, threadAllow)
			f.config(threadFull + "[x]\nkey = 1\n[x]\nother = 2\n")
			f.want(false, nil, "")
		}},
		{"project controlled Codex home", func(f *threadFixture) {
			d := filepath.Join(f.cwd, "codex")
			f.write(filepath.Join(d, "config.toml"), threadFull)
			f.t.Setenv("CODEX_HOME", d)
			f.want(false, nil, "")
			f.want(true, nil, "")
		}},
		{"symlinked Codex config into project", func(f *threadFixture) {
			p := filepath.Join(f.cwd, "config.toml")
			f.write(p, threadFull)
			f.remove(filepath.Join(f.codex, "config.toml"))
			f.link(p, filepath.Join(f.codex, "config.toml"))
			f.want(false, nil, "")
			f.want(true, nil, "")
		}},
		{"advisory needs string cwd", func(f *threadFixture) { f.advised(nil); f.want(true, map[string]any{"cwd": nil}, "") }},
		{"project Codex home untouched", func(f *threadFixture) {
			d := filepath.Join(f.cwd, "codex")
			f.write(filepath.Join(d, "config.toml"), threadFull)
			f.t.Setenv("CODEX_HOME", d)
			before := f.snapshot()
			f.want(false, nil, "")
			f.want(true, nil, "")
			if !reflect.DeepEqual(before, f.snapshot()) {
				f.t.Fatal("hook changed files")
			}
		}},
		{"project local opt in and uncovered tools", func(f *threadFixture) {
			f.write(filepath.Join(f.global, "config.json"), "{}")
			f.write(filepath.Join(f.cwd, "crw.json"), threadOpt)
			f.write(filepath.Join(f.cwd, ".crw", "config.json"), threadOpt)
			f.want(false, nil, "")
			f.t.Setenv("CRW_HOME", filepath.Join(f.cwd, ".crw"))
			f.want(false, nil, "")
			f.t.Setenv("CRW_HOME", "relative")
			f.want(false, nil, "")
			f.t.Setenv("CRW_HOME", f.global)
			f.write(filepath.Join(f.global, "config.json"), threadOpt)
			f.t.Setenv("CODEX_HOME", "relative")
			f.want(false, nil, "")
			f.t.Setenv("CODEX_HOME", f.codex)
			for _, tool := range []string{"Edit", "mcp_tool", "functions.exec", "", "mcp__", "mcp__server", "mcp____tool", "mcp__server__"} {
				f.want(false, map[string]any{"tool_name": tool}, "")
			}
			f.want(false, map[string]any{"tool_name": "mcp__codex_app__create_thread"}, threadAllow)
		}},
		{"global symlink and plain outside config", func(f *threadFixture) {
			p := filepath.Join(f.cwd, ".crw", "config.json")
			f.write(p, threadOpt)
			linked := filepath.Join(f.root, "linked-home")
			f.link(filepath.Dir(p), linked)
			f.t.Setenv("CRW_HOME", linked)
			f.want(false, nil, "")
			f.t.Setenv("CRW_HOME", f.global)
			outside := filepath.Join(f.global, "config.json")
			f.remove(outside)
			f.link(p, outside)
			f.want(false, nil, "")
			f.remove(outside)
			f.write(outside, threadOpt)
			f.want(false, nil, threadAllow)
		}},
		{"default home file and project symlinks", func(f *threadFixture) {
			f.t.Setenv("CODEX_HOME", "")
			f.t.Setenv("CRW_HOME", "")
			c, g := filepath.Join(f.root, ".codex", "config.toml"), filepath.Join(f.root, ".crw", "config.json")
			f.write(c, threadFull)
			f.write(g, threadOpt)
			f.want(false, map[string]any{"cwd": f.root}, threadAllow)
			p := filepath.Join(f.cwd, "config.toml")
			f.write(p, threadFull)
			f.remove(c)
			f.link(p, c)
			f.want(false, map[string]any{"cwd": f.root}, "")
			f.remove(c)
			f.write(c, threadFull)
			p = filepath.Join(f.cwd, "config.json")
			f.write(p, threadOpt)
			f.remove(g)
			f.link(p, g)
			f.want(false, nil, "")
		}},
		{"exact allow and otherwise no stdout", func(f *threadFixture) {
			f.want(false, nil, threadAllow)
			for _, raw := range []string{"{", "", "[]", "null", `{"hook_event_name":"Other"}`, `{"hook_event_name":null}`} {
				if got := f.raw("permission-request", raw); got != "" {
					f.t.Fatalf("raw=%q: %q", raw, got)
				}
			}
			f.want(false, map[string]any{"transcript_path": filepath.Join(f.root, "missing")}, "")
			f.write(filepath.Join(f.global, "config.json"), strings.Repeat(" ", 1024*1024)+"{}")
			f.want(false, nil, "")
		}},
		{"advises without opt in", func(f *threadFixture) { f.write(filepath.Join(f.global, "config.json"), "{}"); f.advised(nil) }},
		{"advisory silent outside degraded context", func(f *threadFixture) {
			for _, v := range []string{"approval_policy = \"on-request\"\nsandbox_mode = \"danger-full-access\"", "approval_policy = \"never\"\nsandbox_mode = \"workspace-write\"", threadFull + "profile = \"team\""} {
				f.config(v)
				f.want(true, nil, "")
			}
			f.config(threadFull)
			for _, c := range []map[string]any{{"permission_mode": "full-access"}, {"agent_id": nil}, {"agent_type": "worker"}} {
				f.want(true, c, "")
			}
			f.write(f.transcript, `{"type":"session_meta","payload":{"id":"fixture-id","thread_source":"user"}}`+"\n")
			f.want(true, nil, "")
			f.write(f.transcript, "{bad\n")
			f.want(true, nil, "")
		}},
		{"CLI fail open before root exit", func(f *threadFixture) {
			f.want(false, nil, threadAllow)
			f.advised(nil)
			for _, event := range []string{"permission-request", "session-start"} {
				for _, raw := range []string{"{bad", strings.Repeat("x", 4*1024*1024+1)} {
					if got := f.raw(event, raw); got != "" {
						f.t.Fatal(got)
					}
				}
			}
		}},
		{"CLI active with PABCD off no state", func(f *threadFixture) {
			f.t.Setenv("CRW_PABCD", "off")
			before := f.snapshot()
			f.want(false, nil, threadAllow)
			f.advised(nil)
			if !reflect.DeepEqual(before, f.snapshot()) {
				f.t.Fatal("env-off hook wrote state")
			}
			f.t.Setenv("CRW_PABCD", "")
			f.write(filepath.Join(f.cwd, "crw.json"), `{"pabcd":{"enabled":false}}`)
			before = f.snapshot()
			f.want(false, nil, threadAllow)
			f.advised(nil)
			if !reflect.DeepEqual(before, f.snapshot()) {
				f.t.Fatal("project-off hook wrote state")
			}
		}},
	}
	if len(cases) != 29 {
		t.Fatalf("oracle mapping has %d runnable cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(threadSetup(t)) })
	}
}

func (f *threadFixture) grammar(variants []string) {
	f.t.Helper()
	for _, tail := range variants {
		f.config(threadFull + tail)
		f.want(false, nil, "")
		f.want(true, nil, "")
	}
}

func TestAgentThreadBoundaries(t *testing.T) {
	t.Run("intentionally-changed project child starting with two dots", func(t *testing.T) {
		for _, key := range []string{"CODEX_HOME", "CRW_HOME"} {
			t.Run(key, func(t *testing.T) {
				f := threadSetup(t)
				path := filepath.Join(f.cwd, "..controlled")
				name, contents := "config.toml", threadFull
				if key == "CRW_HOME" {
					name, contents = "config.json", threadOpt
				}
				f.write(filepath.Join(path, name), contents)
				t.Setenv(key, path)
				f.want(false, nil, "")
				if key == "CODEX_HOME" {
					f.want(true, nil, "")
				} else {
					f.advised(nil)
				}
				outside := filepath.Join(f.root, "..outside")
				f.write(filepath.Join(outside, name), contents)
				t.Setenv(key, outside)
				f.want(false, nil, threadAllow)
			})
		}
	})
	t.Run("first record exact byte bounds and later lines", func(t *testing.T) {
		f := threadSetup(t)
		for _, lf := range []string{"", "\n"} {
			f.write(f.transcript, threadMeta+strings.Repeat(" ", 65536-len(threadMeta))+lf)
			f.want(false, nil, threadAllow)
			f.write(f.transcript, threadMeta+strings.Repeat(" ", 65537-len(threadMeta))+lf)
			f.want(false, nil, "")
		}
		f.write(f.transcript, threadMeta+"\n"+strings.Repeat("x", 65537))
		f.want(false, nil, threadAllow)
		f.write(f.transcript, `{"type":"session_meta","payload":{"id":"fixture-id","thread_source":"agent_created_thread","forked_from_id":null}}`)
		f.want(false, nil, threadAllow)
	})
	t.Run("config exact byte bounds", func(t *testing.T) {
		f := threadSetup(t)
		f.config(threadFull + "#" + strings.Repeat("x", 1024*1024-len(threadFull)-1))
		f.want(false, nil, threadAllow)
		f.config(threadFull + "#" + strings.Repeat("x", 1024*1024-len(threadFull)))
		f.want(false, nil, "")
		f.config(threadFull)
		f.write(filepath.Join(f.global, "config.json"), threadOpt+strings.Repeat(" ", 1024*1024-len(threadOpt)))
		f.want(false, nil, threadAllow)
		f.write(filepath.Join(f.global, "config.json"), threadOpt+strings.Repeat(" ", 1024*1024-len(threadOpt)+1))
		f.want(false, nil, "")
	})
	t.Run("JS whitespace and line terminators", func(t *testing.T) {
		f := threadSetup(t)
		f.config("\ufeff" + threadFull + "[features]\u00a0# comment\nx = true\n")
		f.want(false, nil, threadAllow)
		f.config("approval_policy = \"never\" # c\rx\nsandbox_mode = \"danger-full-access\"\n")
		f.want(false, nil, "")
		f.config(threadFull)
		for _, tail := range []string{"\n", "\r", "\u2028", "\u2029"} {
			f.want(false, map[string]any{"tool_name": "mcp__server__tool" + tail}, "")
		}
		t.Setenv("CRW_HOME", " \t"+f.global+"\u00a0")
		t.Setenv("CODEX_HOME", " \t"+f.codex+"\u00a0")
		f.want(false, nil, threadAllow)
	})
	t.Run("distinct lone surrogate rollout identity", func(t *testing.T) {
		f := threadSetup(t)
		f.write(f.transcript, `{"type":"session_meta","payload":{"id":"\ud800","thread_source":"agent_created_thread"}}`+"\n")
		base := strings.ReplaceAll(mustThreadJSON(t, f.input), `"fixture-id"`, `"\ud801"`)
		if got := f.raw("permission-request", base); got != "" {
			t.Fatalf("distinct surrogate allowed: %q", got)
		}
		base = strings.ReplaceAll(base, `\ud801`, `\ud800`)
		if got := f.raw("permission-request", base); got != threadAllow {
			t.Fatalf("same surrogate: %q", got)
		}
	})
}

func mustThreadJSON(t *testing.T, p map[string]any) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
