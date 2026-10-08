package hook

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

const gateSession = "s1"

// gateScene is a workspace, a home and a CODEX_HOME in a temporary directory; root is the protected memories directory.
func gateScene(t *testing.T) (cwd, root string, env host.LookupEnv) {
	t.Helper()
	dir := t.TempDir()
	cwd = filepath.Join(dir, "work")
	vars := map[string]string{"HOME": filepath.Join(dir, "home"), "CODEX_HOME": filepath.Join(dir, "codex-home")}
	for _, d := range []string{cwd, vars["HOME"], vars["CODEX_HOME"]} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return cwd, filepath.Join(vars["CODEX_HOME"], "memories"), func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func gateEnvOf(vars map[string]string) host.LookupEnv {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func gatePayload(t *testing.T, cwd string, over map[string]any) string {
	t.Helper()
	p := map[string]any{"hook_event_name": "PreToolUse", "session_id": gateSession, "turn_id": "t1", "cwd": cwd,
		"tool_name": "memoriesadd_ad_hoc_note", "tool_input": map[string]any{"filename": "note.md", "note": "x"}}
	for k, v := range over {
		p[k] = v
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gateBash(t *testing.T, cwd, command string) string {
	t.Helper()
	return gatePayload(t, cwd, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command}})
}

// gateDeny checks the deny-only envelope and returns its reason.
func gateDeny(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		H struct {
			Event    string `json:"hookEventName"`
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
			Context  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if !strings.HasSuffix(out, "\n") || json.Unmarshal([]byte(out), &v) != nil || v.H.Event != "PreToolUse" || v.H.Decision != "deny" || v.H.Reason == "" || v.H.Context != v.H.Reason {
		t.Fatalf("not a deny envelope: %q", out)
	}
	return v.H.Reason
}

func gateSeed(t *testing.T, cwd string, edit func(*state.State)) {
	t.Helper()
	s := state.DefaultState(gateSession, "")
	edit(&s)
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
}

func gateTurn(s string) *string { return &s }

// gateTree is every file under dir with its bytes, to show that a path wrote nothing.
func gateTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		b, _ := os.ReadFile(p)
		out[p] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMemoryGateToolNames(t *testing.T) {
	cwd, _, env := gateScene(t)
	for _, name := range []string{"memoriesadd_ad_hoc_note", "memories.add_ad_hoc_note", "memories_add_ad_hoc_note", "add_ad_hoc_note"} {
		if got := memoryGateClassify(name, map[string]any{"filename": "n.md"}, cwd, env); got != (MemoryWriteAttempt{Surface: "tool", Target: "n.md"}) {
			t.Errorf("%s: %+v", name, got)
		}
	}
	for _, name := range []string{"memoriessearch", "memories.list", "memories.read"} {
		if got := memoryGateClassify(name, map[string]any{}, cwd, env); got.Surface != "" {
			t.Errorf("read tool %s classified as %+v", name, got)
		}
	}
	// Unparseable arguments reach the hook as a bare string; an empty or absent filename names no note.
	for _, input := range []any{nil, "{broken", map[string]any{}, []any{"x"}, map[string]any{"filename": 7}} {
		if got := memoryGateClassify("memoriesadd_ad_hoc_note", input, cwd, env); got != (MemoryWriteAttempt{Surface: "tool", Target: "(ad hoc note)"}) {
			t.Errorf("input %v: %+v", input, got)
		}
	}
}

func TestMemoryGateDeniesAnUnrequestedNote(t *testing.T) {
	cwd, _, env := gateScene(t)
	reason := gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, nil), env))
	for _, want := range []string{"MEMORY-WRITE-GATE", "Blocked a write of a memory note (note.md)", "allow-write --session s1", "remember this", "stored per cwd", "from " + cwd + "."} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason lacks %q: %s", want, reason)
		}
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a deny must leave the workspace as it was: %v", err)
	}
}

func TestMemoryGateFailOpenAndFailClosedEdges(t *testing.T) {
	cwd, _, env := gateScene(t)
	for name, raw := range map[string]string{
		"not json":                      "not json",
		"empty":                         "",
		"a string":                      `"x"`,
		"null":                          "null",
		"wrong event":                   gatePayload(t, cwd, map[string]any{"hook_event_name": "PostToolUse"}),
		"no tool":                       gatePayload(t, cwd, map[string]any{"tool_name": ""}),
		"numeric tool":                  gatePayload(t, cwd, map[string]any{"tool_name": 7}),
		"unrelated tool":                gateBash(t, cwd, "ls"),
		"edit tool with a string input": gatePayload(t, cwd, map[string]any{"tool_name": "apply_patch", "tool_input": "junk"}),
	} {
		if got := HandleMemoryWriteGate(raw, env); got != "" {
			t.Errorf("%s answered %q", name, got)
		}
	}
	// No cwd or no session id leaves no state to prove a request, so the write is denied (it is not a crash).
	for name, over := range map[string]map[string]any{
		"no cwd":            {"cwd": ""},
		"no session":        {"session_id": ""},
		"string tool input": {"tool_input": "{broken"},
	} {
		gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, over), env))
		_ = name
	}
}

func TestMemoryGateMarkerIsSpentOncePerTurn(t *testing.T) {
	cwd, _, env := gateScene(t)
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteRequested, s.MemoryWriteTurn = true, gateTurn("t1") })
	before := gateTree(t, cwd)
	gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, map[string]any{"turn_id": "t9"}), env))
	if after := gateTree(t, cwd); !reflect.DeepEqual(before, after) {
		t.Errorf("a wrong-turn deny changed the workspace: %v then %v", before, after)
	}
	if got := HandleMemoryWriteGate(gatePayload(t, cwd, nil), env); got != "" {
		t.Fatalf("the turn that asked: %q", got)
	}
	if s := state.ReadState(cwd, gateSession); s.MemoryWriteRequested || s.MemoryWriteTurn != nil {
		t.Errorf("the marker is not spent: %+v", s)
	}
	gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, nil), env))
	// A call that names no turn spends the marker outright, the conservative reading.
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteRequested, s.MemoryWriteTurn = true, gateTurn("t1") })
	if got := HandleMemoryWriteGate(gatePayload(t, cwd, map[string]any{"turn_id": ""}), env); got != "" {
		t.Errorf("turnless call: %q", got)
	}
}

func TestMemoryGateGrantIsSpentOnceAndKeepsTheMarkerScoped(t *testing.T) {
	cwd, _, env := gateScene(t)
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	if got := HandleMemoryWriteGate(gatePayload(t, cwd, nil), env); got != "" {
		t.Fatalf("granted call: %q", got)
	}
	if state.ReadState(cwd, gateSession).MemoryWriteGrant {
		t.Error("the grant is not spent")
	}
	gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, nil), env))

	// Spending a grant must not turn the turn-scoped marker beside it into one every later turn can spend.
	gateSeed(t, cwd, func(s *state.State) {
		s.MemoryWriteGrant, s.MemoryWriteRequested, s.MemoryWriteTurn = true, true, gateTurn("t1")
	})
	if got := HandleMemoryWriteGate(gatePayload(t, cwd, map[string]any{"turn_id": "t9"}), env); got != "" {
		t.Fatalf("t9 with the grant: %q", got)
	}
	gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, map[string]any{"turn_id": "t10"}), env))
	if got := HandleMemoryWriteGate(gatePayload(t, cwd, nil), env); got != "" {
		t.Errorf("t1 with the marker: %q", got)
	}
	gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, nil), env))
}

func TestMemoryGateOneGrantAllowsOneConcurrentWrite(t *testing.T) {
	cwd, _, env := gateScene(t)
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	raw := gatePayload(t, cwd, nil)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if HandleMemoryWriteGate(raw, env) == "" {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 1 || state.ReadState(cwd, gateSession).MemoryWriteGrant {
		t.Errorf("%d calls spent one grant (grant left: %v)", allowed.Load(), state.ReadState(cwd, gateSession).MemoryWriteGrant)
	}
}

func TestMemoryGateRefusesWhenTheAuthorizationCannotBeSpent(t *testing.T) {
	grant := func(s *state.State) { s.MemoryWriteGrant = true }
	t.Run("held lock", func(t *testing.T) {
		cwd, _, env := gateScene(t)
		gateSeed(t, cwd, grant)
		lock := state.StatePath(cwd, gateSession) + ".lock"
		if err := os.WriteFile(lock, []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}
		if reason := gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, nil), env)); !strings.Contains(reason, "locked or cannot be written") || !strings.Contains(reason, lock) {
			t.Errorf("reason: %s", reason)
		}
		if !state.ReadState(cwd, gateSession).MemoryWriteGrant {
			t.Error("a refused call spent the grant")
		}
	})
	t.Run("failed write", func(t *testing.T) {
		cwd, _, env := gateScene(t)
		gateSeed(t, cwd, grant)
		failing := func(string, state.State) error { return errors.New("disk full") }
		if reason := gateDeny(t, memoryGateHandle(gatePayload(t, cwd, nil), env, failing)); !strings.Contains(reason, "locked or cannot be written") {
			t.Errorf("reason: %s", reason)
		}
		if got := HandleMemoryWriteGate(gatePayload(t, cwd, nil), env); got != "" {
			t.Errorf("the retry after a failed write: %q", got)
		}
	})
	t.Run("records the writer cannot keep", func(t *testing.T) {
		cwd, _, env := gateScene(t)
		gateSeed(t, cwd, grant)
		path := state.StatePath(cwd, gateSession)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		m["unverifiedSubagents"] = []any{1} // an entry the state reader drops
		lossy, _ := json.Marshal(m)
		if err := os.WriteFile(path, lossy, 0o644); err != nil {
			t.Fatal(err)
		}
		if reason := gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, nil), env)); !strings.Contains(reason, "cannot rewrite") {
			t.Errorf("reason: %s", reason)
		}
		if after, _ := os.ReadFile(path); string(after) != string(lossy) {
			t.Errorf("the state was rewritten: %s", after)
		}
	})
}

func TestMemoryGatePatchTargets(t *testing.T) {
	for _, c := range []struct {
		patch string
		want  []string
	}{
		{"+++ b/a/b.md\n+x", []string{"a/b.md"}},
		{"+++ /a/b.md\n+++ b/\n", []string{"/a/b.md", "b/"}},
		{"  *** Add File:  /x \r\n*** Delete File: y\r\n*** Update File: z", []string{"/x", "y", "z"}},
		{"*** Move File: m\n*** Move to: n/o.md", []string{"m", "n/o.md"}},
		{"*** Add File: a\u2028b\n*** Add File:\n*** Add File: ", []string{"a\u2028b"}},
		{"--- a/x\n*** Begin Patch\n*** End Patch", []string{}},
	} {
		if got := memoryGatePatchTargets(c.patch); !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Errorf("%q: %q, want %q", c.patch, got, c.want)
		}
	}
}

func TestMemoryGateEditSurface(t *testing.T) {
	cwd, root, env := gateScene(t)
	patch := func(body string) map[string]any {
		return map[string]any{"command": "*** Begin Patch\n" + body + "*** End Patch\n"}
	}
	for _, c := range []struct {
		name, tool string
		input      any
		surface    string
	}{
		{"add inside", "apply_patch", patch("*** Add File: " + root + "/extensions/ad_hoc/notes/x.md\n+hi\n"), "edit"},
		{"unified header", "apply_patch", map[string]any{"command": "--- a/x\n+++ " + root + "/MEMORY.md\n+hi\n"}, "edit"},
		{"move destination", "apply_patch", patch("*** Update File: a.md\n*** Move to: " + root + "/n.md\n"), "edit"},
		{"relative path", "apply_patch", patch("*** Add File: ../codex-home/memories/x.md\n+hi\n"), "edit"},
		{"Write", "Write", map[string]any{"file_path": root + "/n.md", "content": "x"}, "edit"},
		{"Edit", "Edit", map[string]any{"file_path": root + "/MEMORY.md", "old_string": "a", "new_string": "b"}, "edit"},
		{"elsewhere", "apply_patch", patch("*** Update File: src/index.ts\n+hi\n"), ""},
		{"sibling sharing the prefix", "apply_patch", patch("*** Add File: " + root + "-backup/x.md\n+hi\n"), ""},
		{"the root's parent", "Write", map[string]any{"file_path": filepath.Dir(root) + "/n.md"}, ""},
		{"line separator in the name", "apply_patch", patch("*** Add File: " + root + "/a\u2028b.md\n+hi\n"), "edit"},
		{"backslash directory in the name", "Write", map[string]any{"file_path": root + "/\\../x.md"}, "edit"},
		{"string input", "apply_patch", "junk", ""},
		{"no command", "apply_patch", map[string]any{}, ""},
	} {
		if got := memoryGateClassify(c.tool, c.input, cwd, env); got.Surface != c.surface {
			t.Errorf("%s: %+v, want surface %q", c.name, got, c.surface)
		}
	}
}

func TestMemoryGateShellSurface(t *testing.T) {
	cwd, root, env := gateScene(t)
	heredoc := "mkdir -p /w/devlog/_plan/notes && cat > /w/devlog/_plan/notes/00_brief.md <<'EOF'\n" + root + "\nEOF"
	live := "mkdir -p /Users/jun/.codex/worktrees/3412/codexclaw/devlog/notes /tmp/mfu && cat > /Users/jun/.codex/worktrees/3412/codexclaw/devlog/notes/00_brief.md <<'EOF'\n~/.codex/memories\nEOF"
	writes := []string{
		"echo hi > " + root + "/notes.md", "echo hi>" + root + "/n.md", "echo hi>>" + root + "/n.md", "echo hi >| " + root + "/n.md",
		"sed -i 's/a/b/' " + root + "/MEMORY.md", "rg foo /w | tee " + root + "/out.md", "cp /w/a.md " + root + "/b.md", "mv /w/a.md " + root + "/b.md",
		"perl -i -pe 's/a/b/' " + root + "/MEMORY.md", "ruby -i -pe 's/a/b/' " + root + "/MEMORY.md",
		"py -c \"open(r'" + root + "/n.md','w').write('x')\"", "node --eval \"require('fs').writeFileSync('" + root + "/n.md','x')\"",
		"node -erequire('fs').writeFileSync('" + root + "/n.md','x')", "bash -c 'echo x > " + root + "/n.md'",
	}
	reads := []string{
		"sed -n '1p' " + root + "/MEMORY.md", "rg foo " + root + "/MEMORY.md 2>/dev/null", "rg foo " + root, "cat " + root + "/MEMORY.md",
		heredoc, live, "echo 'a>b'", "grep -- '->' /w/f", "echo hi > /w/out.txt", "cp " + root + "/a.md /w/b.md", "rg '<prose>' " + root + "/MEMORY.md",
		"python3 -c \"from pathlib import Path; print(Path('" + root + "/MEMORY.md').read_text()); print('x -> y')\"",
		"echo hi > " + root + "-backup/n.md",
	}
	for _, command := range writes {
		for _, tool := range []string{"Bash", "exec_command"} {
			if got := memoryGateClassify(tool, map[string]any{"command": command}, cwd, env); got.Surface != "shell" {
				t.Errorf("%s must be gated for %s: %+v", command, tool, got)
			}
		}
	}
	for _, command := range reads {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
			t.Errorf("%s must pass: %+v", command, got)
		}
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": "sed -i 's/a/b/' " + root + "/../memories/MEMORY.md"}, cwd, env); got.Target != root+"/MEMORY.md" {
		t.Errorf("the target is the cleaned path: %+v", got)
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": ""}, cwd, env); got.Surface != "" {
		t.Errorf("empty command: %+v", got)
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": []any{"echo hi > " + root + "/n.md"}}, cwd, env); got.Surface != "" {
		t.Errorf("a non-string command: %+v", got)
	}
}

func TestMemoryGateHomeForms(t *testing.T) {
	cwd, _, _ := gateScene(t)
	home := filepath.Join(t.TempDir(), "home")
	defaultRoot := filepath.Join(home, ".codex", "memories")
	for _, c := range []struct {
		vars map[string]string
		dest string
		want bool
	}{
		{map[string]string{"HOME": home}, "~/.codex/memories/n.md", true},
		{map[string]string{"HOME": home}, `~\.codex\memories\n.md`, true},
		{map[string]string{"HOME": home}, `%USERPROFILE%\.codex\memories\n.md`, true},
		{map[string]string{"HOME": home}, `$env:USERPROFILE\.codex\memories\n.md`, true},
		{map[string]string{"HOME": home}, "$HOME/.codex/memories/n.md", true},
		{map[string]string{"HOME": home}, "${HOME}/.codex/memories/n.md", true},
		{map[string]string{"HOME": home}, "\"$HOME/.codex/memories/n.md\"", true},
		{map[string]string{"HOME": home}, "~/.codex/memories-backup/n.md", false},
		{map[string]string{"HOME": home}, "~user/.codex/memories/n.md", true}, // another user's home is not provable: fail closed (CRW-1028)
		{map[string]string{"HOME": home, "CODEX_HOME": filepath.Join(home, "ch")}, "$CODEX_HOME/memories/n.md", true},
		{map[string]string{"HOME": home, "CODEX_HOME": filepath.Join(home, "ch")}, "${CODEX_HOME}/memories/n.md", true},
		{map[string]string{"HOME": home}, "$CODEX_HOME/memories/n.md", true}, // an unset variable is not provable: fail closed (CRW-1028)
	} {
		env := gateEnvOf(c.vars)
		got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > " + c.dest}, cwd, env)
		if (got.Surface == "shell") != c.want {
			t.Errorf("%v %s: %+v, want gated %v", c.vars, c.dest, got, c.want)
		}
	}
	_ = defaultRoot
	// The apply_patch route reads the same prefixes.
	env := gateEnvOf(map[string]string{"HOME": home})
	patch := map[string]any{"command": "*** Add File: ~\\.codex\\memories\\x.md\n+hi\n"}
	if got := memoryGateClassify("apply_patch", patch, cwd, env); got.Surface != "edit" {
		t.Errorf("backslash tilde patch: %+v", got)
	}
}

// The JavaScript spelling of absolutize: one quote off each end, ASCII-only case folding of the prefixes, and the path resolved.
func TestMemoryGateAbsolutize(t *testing.T) {
	g := newMemoryGateEnv(gateEnvOf(map[string]string{"HOME": "/h"}))
	first := func(raw, cwd string) string {
		if p := g.abs(raw, cwd); len(p) > 0 {
			return p[0].clean
		}
		return ""
	}
	for raw, want := range map[string]string{
		"~": "/h", "'~/x'": "/h/x", `~\x\y`: "/h/x/y", `%UserProfile%/a`: "/h/a", `$env:USERPROFILE\a`: "/h/a", "$Home/a": "/h/a", "$HOME": "/h",
		"${home}/a": "/h/a", "\"/a/b/\"": "/a/b", "./x": "/w/x", "../x": "/x", "a/../../x": "/x", "": "", "\"": "/w/\"", "''": "/w/''", "   ": "/w/   ",
		"~user/x": "/w/~user/x", "$HOMEx/a": "/w/$HOMEx/a", "%USERPROFİLE%": "/w/%USERPROFİLE%", "'a": "/w/a", `a\b`: "/w/a/b", "\"'a'\"": "/w/'a'",
	} {
		if got := first(raw, "/w"); got != want {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
	}
	if got := first("x", ""); got != "" {
		t.Errorf("a relative path with no cwd: %q", got)
	}
	// A backslash is a separator for the oracle and a name character on POSIX: both readings are candidates, and the joined
	// form keeps its "..".
	if got := g.abs(`a\..\b`, "/w"); len(got) != 2 || got[0] != (memoryGatePath{"/w/b", "/w/a/../b"}) || got[1] != (memoryGatePath{"/w/a\\..\\b", "/w/a\\..\\b"}) {
		t.Errorf("candidates: %+v", got)
	}
	if got := g.abs("a ", "/w"); len(got) != 2 || got[0].clean != "/w/a" || got[1].clean != "/w/a " {
		t.Errorf("trimmed and exact candidates: %+v", got)
	}
	if got := g.abs("~/x/../y", "/w"); len(got) != 2 || got[0] != (memoryGatePath{"/h/y", "/h/x/../y"}) || got[1] != (memoryGatePath{"/w/~/y", "/w/~/x/../y"}) {
		t.Errorf("home prefix: %+v", got)
	}
}

func TestMemoryGateFollowsSymlinks(t *testing.T) {
	cwd, root, env := gateScene(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(cwd, name)); err != nil {
			t.Fatal(err)
		}
	}
	link(root, "alias")
	link(sub, "deep")
	link(filepath.Join(root, "new.md"), "dangling")
	link(filepath.Join(cwd, "loop-b"), "loop-a")
	link(filepath.Join(cwd, "loop-a"), "loop-b")
	link(cwd, "home-again")
	link(filepath.Join(root, "new.md"), "tw ") // names that end in a space or a quote are exact names
	link(filepath.Join(root, "new.md"), "tq'")
	link(root, "%USERPROFILE%")                                                                           // a name that only looks like a home prefix
	if err := os.Symlink(filepath.Join(cwd, "outside.md"), filepath.Join(root, "entry.md")); err != nil { // a link inside the root that leads out
		t.Fatal(err)
	}
	for command, want := range map[string]bool{
		"echo hi > alias/n.md":          true,
		"echo hi > deep/../n.md":        true,
		"echo hi > ./dangling":          true,
		"echo hi > \"tw \"":             true,
		"echo hi > \"tq'\"":             true,
		"echo hi > ./tw":                false,
		"mv a.md alias/entry.md":        true,
		"echo hi > %USERPROFILE%/n.md":  true,
		"cp /w/a.md alias/b.md":         true,
		"echo hi > home-again/out.md":   false,
		"echo hi > loop-a/x":            false,
		"echo hi > alias-missing/n.md":  false,
		"echo hi > deep/../../other.md": false,
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); (got.Surface == "shell") != want {
			t.Errorf("%s: %+v, want gated %v", command, got, want)
		}
	}
	// A cwd that holds "link/.." is followed from the place the link led to.
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > n.md"}, cwd+"/deep/..", env); got.Surface != "shell" {
		t.Errorf("cwd with link/..: %+v", got)
	}
	// A relative cwd is made absolute without cleaning, and a long chain of links in the cwd does not use up the budget of the name.
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("work", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sub, "work/deep"); err != nil {
		t.Fatal(err)
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > n.md"}, "work/deep/..", env); got.Surface != "shell" {
		t.Errorf("relative cwd with link/..: %+v", got)
	}
	chain := filepath.Join(t.TempDir(), "c0")
	if err := os.MkdirAll(chain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "new.md"), filepath.Join(chain, "target")); err != nil {
		t.Fatal(err)
	}
	head := chain
	for i := 1; i <= 40; i++ {
		next := filepath.Join(filepath.Dir(chain), "c"+strconv.Itoa(i))
		if err := os.Symlink(head, next); err != nil {
			t.Fatal(err)
		}
		head = next
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > target"}, head, env); got.Surface != "shell" {
		t.Errorf("a name opened from a cwd behind 40 links: %+v", got)
	}
	// The deny names the path as written, cleaned, not the place it leads to.
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > alias/n.md"}, cwd, env); got.Target != filepath.Join(cwd, "alias", "n.md") {
		t.Errorf("target: %+v", got)
	}
	// An empty HOME expands to nothing in the shell, so $HOME/x is the absolute /x.
	elsewhere := t.TempDir()
	emptyHome := gateEnvOf(map[string]string{"HOME": "", "CODEX_HOME": filepath.Join(elsewhere, "ch")})
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > \"$HOME" + filepath.Join(elsewhere, "ch", "memories", "n.md") + "\""}, cwd, emptyHome); got.Surface != "shell" {
		t.Errorf("empty HOME: %+v", got)
	}
	// A root that leads to "/" holds every absolute path.
	slash := t.TempDir()
	if err := os.MkdirAll(filepath.Join(slash, "ch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(slash, "ch", "memories")); err != nil {
		t.Fatal(err)
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > " + cwd + "/n.md"}, cwd, gateEnvOf(map[string]string{"HOME": slash, "CODEX_HOME": filepath.Join(slash, "ch")})); got.Surface != "shell" {
		t.Errorf("a root that is a link to /: %+v", got)
	}
	// A CODEX_HOME that is itself a link protects the directory it leads to.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "via")); err != nil {
		t.Fatal(err)
	}
	linked := gateEnvOf(map[string]string{"HOME": dir, "CODEX_HOME": filepath.Join(dir, "via")})
	for command, want := range map[string]bool{"echo hi > " + dir + "/real/memories/n.md": true, "echo hi > " + dir + "/real/other.md": false} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, linked); (got.Surface == "shell") != want {
			t.Errorf("linked CODEX_HOME %s: %+v, want gated %v", command, got, want)
		}
	}
	// A home prefix is expanded without cleaning the ".." that follows a link.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex", "memories", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".codex", "memories", "sub"), filepath.Join(home, "alias")); err != nil {
		t.Fatal(err)
	}
	// The same when the link's own name holds a backslash, which the cleaned text no longer shows.
	if err := os.Symlink(filepath.Join(home, ".codex", "memories", "sub"), filepath.Join(home, `bad\alias`)); err != nil {
		t.Fatal(err)
	}
	// And names that end in a space or a quote, behind a home prefix.
	for _, name := range []string{"trail ", "quote'"} { // their trimmed names do not exist
		if err := os.Symlink(filepath.Join(home, ".codex", "memories", "new.md"), filepath.Join(home, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, dest := range []string{"$HOME/alias/../n.md", "~/alias/../n.md", "${HOME}/alias/../n.md", "$env:USERPROFILE/alias/../n.md", `"$HOME/bad\alias/../n.md"`, "\"$HOME/trail \"", "\"$HOME/quote'\""} {
		if got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > " + dest}, cwd, gateEnvOf(map[string]string{"HOME": home})); got.Surface != "shell" {
			t.Errorf("%s: %+v", dest, got)
		}
	}
}

// On POSIX a backslash is a character of a name, so a directory called `\..` inside the root is a place the oracle's
// separator reading sent somewhere else.
func TestMemoryGateReadsABackslashAsPartOfAName(t *testing.T) {
	cwd, root, env := gateScene(t)
	if err := os.MkdirAll(filepath.Join(root, `\..`), 0o755); err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]bool{
		"echo hi > '" + root + `/\../note.md'`:               true,
		"echo hi > '" + filepath.Dir(root) + `/\../note.md'`: false,
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); (got.Surface == "shell") != want {
			t.Errorf("%s: %+v, want gated %v", command, got, want)
		}
	}
}
