package affordance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func testEnv(k string) (string, bool) { return "crw", k == "CRW_BIN" }

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".golden"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func put(t *testing.T, p, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, ws string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		put(t, filepath.Join(ws, "src", fmt.Sprintf("f%03d.ts", i)), "export const x = 1;\n")
	}
}

func payload(ws, event, sid string, extra map[string]any) string {
	o := map[string]any{"cwd": ws, "hook_event_name": event, "session_id": sid}
	for k, v := range extra {
		o[k] = v
	}
	b, _ := json.Marshal(o)
	return string(b)
}

func contextOf(t *testing.T, raw, event string) string {
	t.Helper()
	var o struct {
		Output struct{ HookEventName, AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(raw), &o); err != nil || o.Output.HookEventName != event {
		t.Fatalf("%s envelope: %v %q", event, err, raw)
	}
	return o.Output.AdditionalContext
}

func TestEveryRendererMatchesRecordedOracle(t *testing.T) {
	for name, got := range map[string]string{
		"map40": RenderMapAffordance(40, testEnv), "map60": RenderMapAffordance(60, testEnv),
		"map120": RenderMapAffordance(120, testEnv), "skill": RenderSkillSearchAffordance(testEnv),
		"kwrite": RenderKwriteAffordance(), "binding": RenderSessionBinding("SESSION", testEnv),
		"loop": RenderLoopAffordance(testEnv), "stack": RenderStackedPrAffordance(),
		"background": RenderBackgroundTerminalAffordance(), "questions": RenderQuestionAffordance(),
	} {
		t.Run(name, func(t *testing.T) {
			if got+"\n" != golden(t, name) {
				t.Fatal("renderer differs from substituted oracle")
			}
		})
	}
}

func TestExactEnvelopesAndEscapes(t *testing.T) {
	ws := t.TempDir()
	if got := RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, testEnv); got != golden(t, "session") {
		t.Fatal("SessionStart differs")
	}
	// One input pins HTML punctuation, line separators, a lone surrogate, an
	// astral character and a literal backslash-u sequence independently of Go.
	raw := `{"session_id":"<>&\u2028\u2029\ud800😀\\u2028"}`
	if got := RunMapAffordanceSessionStart(raw, ws, testEnv); got != golden(t, "escaped") {
		t.Fatal("JSON escaping differs from oracle")
	}
	RunPostCompactAffordance(payload(ws, "PostCompact", "SESSION", nil))
	if got := RunUserPromptAffordance(payload(ws, "UserPromptSubmit", "SESSION", nil), testEnv); got != golden(t, "compact") {
		t.Fatal("compact envelope differs")
	}
}

func TestCountExtensionsSkipsAndCap(t *testing.T) {
	ws := t.TempDir()
	for _, dir := range []string{"node_modules", ".git", "dist", "build", "target", "out", "coverage", "__pycache__", "venv", ".venv", "env", ".next", ".cache", "vendor", ".hidden"} {
		put(t, filepath.Join(ws, dir, "junk.ts"), "x")
	}
	for _, ext := range []string{"ts", "tsx", "js", "jsx", "mjs", "cjs", "py", "rs", "go", "java", "rb", "c", "h", "cpp", "hpp", "cs", "swift", "kt", "scala", "lua", "ex", "exs", "php"} {
		put(t, filepath.Join(ws, "src", "f."+ext), "x")
	}
	for _, name := range []string{".ts", "upper.TS", "notes.md", "extensionless"} {
		put(t, filepath.Join(ws, name), "x")
	}
	put(t, filepath.Join(ws, ".hidden.ts"), "x") // hidden files, unlike dirs, count
	if got := CountSourceFiles(ws); got != 24 {
		t.Fatalf("count=%d", got)
	}
	if err := os.Symlink(filepath.Join(ws, "src"), filepath.Join(ws, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "src", "f.ts"), filepath.Join(ws, "link.ts")); err != nil {
		t.Fatal(err)
	}
	if CountSourceFiles(ws) != 24 || CountSourceFiles(filepath.Join(ws, "missing")) != 0 || CountSourceFiles(filepath.Join(ws, ".hidden.ts")) != 0 {
		t.Fatal("symlink/unreadable counting differs")
	}
	seed(t, ws, 90)
	if CountSourceFiles(ws) != CountCap {
		t.Fatal("count cap changed")
	}
}

func TestDirectoryCapIsBreadthFirstAndIncludesRoot(t *testing.T) {
	ws := t.TempDir()
	// 50 branches of 80 directories give 4001 directories including root.
	// All 50 files are at the deepest level, so breadth-first's 4000-dir cap
	// sees exactly 49 of them regardless of native sibling enumeration order.
	for branch := 0; branch < 50; branch++ {
		dir := filepath.Join(ws, fmt.Sprintf("branch%d", branch))
		for depth := 0; depth < 80; depth++ {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if depth < 79 {
				dir = filepath.Join(dir, "d")
			}
		}
		put(t, filepath.Join(dir, "leaf.go"), "x")
	}
	if got := CountSourceFiles(ws); got != 49 {
		t.Fatalf("breadth-first cap: %d", got)
	}
}

func TestCountingUsesOracleDirectoryOrderAtCap(t *testing.T) {
	ws := t.TempDir()
	for i := 0; i < MaxDirsVisited-1; i++ {
		if err := os.Mkdir(filepath.Join(ws, fmt.Sprintf("z%04d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Created last, sorted first by Node readdirSync and os.ReadDir. Native
	// insertion order would hit the directory cap before seeing this source.
	put(t, filepath.Join(ws, "a-first", "source.go"), "x")
	if got := CountSourceFiles(ws); got != 1 {
		t.Fatalf("directory ordering: %d", got)
	}
}

func TestSessionStartThresholdFallbackAndPointers(t *testing.T) {
	small, big := t.TempDir(), t.TempDir()
	seed(t, small, 39)
	seed(t, big, 40)
	for _, tc := range []struct {
		raw, fallback string
		mapLine       bool
	}{
		{"", small, false}, {"{", big, true}, {"null", small, false}, {"[]", small, false},
		{payload(big, "SessionStart", "", nil), small, true},
		{"\ufeff" + payload(big, "SessionStart", "", nil), small, true},
		{`{"cwd":123,"session_id":false,"unused":1e400}`, small, false},
		{`{} {}`, small, false}, {`{"cwd":"/dev/null"}`, big, false},
	} {
		ctx := contextOf(t, RunMapAffordanceSessionStart(tc.raw, tc.fallback, testEnv), "SessionStart")
		if strings.Contains(ctx, "crw map") != tc.mapLine || !strings.Contains(ctx, "crw skill search") {
			t.Fatalf("fallback/size gate: %q", tc.raw)
		}
		if len(strings.Split(ctx, "\n\n")) != 6+btoi(tc.mapLine) {
			t.Fatal("unconditional pointers changed")
		}
	}
	for _, sid := range []string{"parent-session", "child-session", strings.Repeat("x", 33000)} {
		ctx := contextOf(t, RunMapAffordanceSessionStart(payload(small, "SessionStart", sid, nil), small, testEnv), "SessionStart")
		if strings.Split(ctx, "\n\n")[0] != RenderSessionBinding(sid, testEnv) {
			t.Fatal("binding changed or truncated")
		}
	}
	if _, err := os.Stat(filepath.Join(small, ".crw")); !os.IsNotExist(err) {
		t.Fatal("SessionStart wrote state")
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestInvocationIsResolvedAtRenderTime(t *testing.T) {
	env := func(bin, home string) host.LookupEnv {
		return func(k string) (string, bool) {
			switch k {
			case "CRW_BIN":
				return bin, true
			case "HOME":
				return home, true
			}
			return "", false
		}
	}
	for _, tc := range []struct {
		lookup host.LookupEnv
		inv    string
	}{
		{env("crw", "/different-home"), "crw"},
		{env("  chosen-crw  ", "/different-home"), "chosen-crw"},
		{env("", "/synthetic-home"), `"/synthetic-home/.local/share/crw-runtime/current/bin/crw"`},
	} {
		if got := ResolveCRWCommands("`crw session current` and `crw map src` and `crw orchestrate P`", tc.lookup); got != "`"+tc.inv+" relay session current` and `"+tc.inv+" map src` and `"+tc.inv+" pabcd orchestrate P`" {
			t.Fatalf("resolver: %s", got)
		}
		ctx := contextOf(t, RunMapAffordanceSessionStart("", t.TempDir(), tc.lookup), "SessionStart")
		if strings.Contains(ctx, "is not on PATH here") != (tc.inv != "crw") {
			t.Fatal("banner seam")
		}
	}
	for _, s := range []string{"load $crw:crw-loop", "send !crw start", "parent owns crw orchestration", "`crw-loop`"} {
		if ResolveCRWCommands(s, env("chosen", "")) != s {
			t.Fatal("non-command rewritten")
		}
	}
	for _, space := range []string{"\v", "\u00a0", "\u1680", "\u2000", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000", "\ufeff"} {
		s := "`crw " + space + "map src`"
		if ResolveCRWCommands(s, env("chosen", "")) != s {
			t.Fatal("JavaScript whitespace was consumed")
		}
	}
}

func TestCompactLifecycleCoalescesAndLeavesNoFSM(t *testing.T) {
	ws := t.TempDir()
	compact, prompt := payload(ws, "PostCompact", "SESSION", nil), payload(ws, "UserPromptSubmit", "SESSION", nil)
	if RunUserPromptAffordance(prompt, testEnv) != "" {
		t.Fatal("unqueued output")
	}
	if _, err := os.Stat(filepath.Join(ws, ".crw")); !os.IsNotExist(err) {
		t.Fatal("read created state")
	}
	RunPostCompactAffordance(compact)
	RunPostCompactAffordance(compact)
	path := RecoveryPath(prompt, "UserPromptSubmit", false)
	if data, err := os.ReadFile(path); err != nil || string(data) != "pending\n" {
		t.Fatal("marker content", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatal("marker mode")
	}
	if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
		t.Fatal("directory mode")
	}
	if data, _ := os.ReadFile(filepath.Join(ws, ".crw", ".gitignore")); string(data) != crwdir.GitignoreText {
		t.Fatal("first writer ignore")
	}
	if got := RunUserPromptAffordance(prompt, testEnv); got != golden(t, "compact") {
		t.Fatal("recovery changed")
	}
	if RunUserPromptAffordance(prompt, testEnv) != "" {
		t.Fatal("repeated output")
	}
	entries, _ := os.ReadDir(filepath.Join(ws, ".crw"))
	if len(entries) != 2 {
		t.Fatal("unexpected FSM/goal files")
	}
	entries, _ = os.ReadDir(filepath.Dir(path))
	if len(entries) != 0 {
		t.Fatal("marker not consumed")
	}
}

func TestRecoveryIdentityRefusalsAndAcceptedEmptyStamps(t *testing.T) {
	ws, other := t.TempDir(), t.TempDir()
	for _, raw := range []string{"", "{", "null", "[]", `{}`, payload("relative", "PostCompact", "root", nil), payload(ws, "PostCompact", "", nil), payload(ws, "PostCompact", "\ufeff", nil), payload(ws, "PostCompact", strings.Repeat("x", 257), nil), "\ufeff" + payload(ws, "PostCompact", "root", nil)} {
		RunPostCompactAffordance(raw)
	}
	for _, v := range []any{"child", false, 0, []any{}, map[string]any{}} {
		for _, k := range []string{"agent_id", "agent_type"} {
			RunPostCompactAffordance(payload(ws, "PostCompact", "root", map[string]any{k: v}))
		}
	}
	if _, err := os.Stat(filepath.Join(ws, ".crw")); !os.IsNotExist(err) {
		t.Fatal("invalid root created state")
	}
	RunPostCompactAffordance(payload(ws, "PostCompact", "root", map[string]any{"agent_id": nil, "agent_type": ""}))
	for _, raw := range []string{"{", payload(ws, "PostCompact", "root", nil), payload(ws, "UserPromptSubmit", "other", nil), payload(other, "UserPromptSubmit", "root", nil), payload(ws, "UserPromptSubmit", "root", map[string]any{"agent_type": "worker"}), payload(ws, "UserPromptSubmit", "root", map[string]any{"agent_id": "child"})} {
		if RunUserPromptAffordance(raw, testEnv) != "" {
			t.Fatal("non-owner stole marker")
		}
	}
	if RunUserPromptAffordance(payload(ws, "UserPromptSubmit", "root", map[string]any{"agent_id": "", "agent_type": nil}), testEnv) == "" {
		t.Fatal("root did not consume")
	}
}

func TestRecoveryPathScopeSymlinksAndNonfiles(t *testing.T) {
	ws := t.TempDir()
	sid := "../../escaped/.."
	path := RecoveryPath(payload(ws, "PostCompact", sid, nil), "PostCompact", true)
	if filepath.Dir(path) != filepath.Join(ws, ".crw", "affordance-recovery") || len(filepath.Base(path)) != len(".pending")+64 {
		t.Fatal("session escaped")
	}
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(ws, alias); err != nil {
		t.Fatal(err)
	}
	if RecoveryPath(payload(alias, "UserPromptSubmit", sid, nil), "UserPromptSubmit", false) != path {
		t.Fatal("workspace alias differs")
	}
	for _, where := range []string{".crw", filepath.Join(".crw", "affordance-recovery")} {
		for _, symlink := range []bool{true, false} {
			root, outside := t.TempDir(), t.TempDir()
			target := filepath.Join(root, where)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, target, "not a directory")
			}
			if RunPostCompactAffordance(payload(root, "PostCompact", "root", nil)) != "" || RunUserPromptAffordance(payload(root, "UserPromptSubmit", "root", nil), testEnv) != "" {
				t.Fatal("guard emitted")
			}
			if e, _ := os.ReadDir(outside); len(e) != 0 {
				t.Fatal("symlink target changed")
			}
		}
	}
	outside := filepath.Join(t.TempDir(), "kept")
	put(t, outside, "keep")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	RunPostCompactAffordance(payload(ws, "PostCompact", sid, nil))
	if RunUserPromptAffordance(payload(ws, "UserPromptSubmit", sid, nil), testEnv) != "" {
		t.Fatal("symlink consumed")
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatal("target overwritten")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if RunUserPromptAffordance(payload(ws, "UserPromptSubmit", sid, nil), testEnv) != "" {
		t.Fatal("directory consumed")
	}
}

func TestUnicodeSessionBoundAndSingleConsumer(t *testing.T) {
	ws := t.TempDir()
	if RecoveryPath(payload(ws, "PostCompact", strings.Repeat("😀", 129), nil), "PostCompact", true) != "" {
		t.Fatal("UTF16 cap")
	}
	if RecoveryPath(payload(ws, "PostCompact", strings.Repeat("😀", 128), nil), "PostCompact", true) == "" {
		t.Fatal("UTF16 boundary")
	}
	lone := `{"hook_event_name":"PostCompact","session_id":"\ud800","cwd":` + fmt.Sprintf("%q", ws) + `}`
	if RecoveryPath(lone, "PostCompact", true) != RecoveryPath(payload(ws, "PostCompact", "\ufffd", nil), "PostCompact", true) {
		t.Fatal("Node surrogate hash")
	}
	RunPostCompactAffordance(payload(ws, "PostCompact", "root", nil))
	var wg sync.WaitGroup
	outputs := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outputs <- RunUserPromptAffordance(payload(ws, "UserPromptSubmit", "root", nil), testEnv)
		}()
	}
	wg.Wait()
	close(outputs)
	n := 0
	for out := range outputs {
		if out != "" {
			n++
			if out != golden(t, "compact") {
				t.Fatal("consumer output")
			}
		}
	}
	if n != 1 {
		t.Fatal("consumers", n)
	}
}

// CRW-1146: a resumed session already holds the static pointers from its first start, so source=resume re-issues only what can
// change (the session binding and the not-on-PATH banner); startup, compact, clear, a missing or an unknown or non-string source
// keep the whole list.
func TestSessionStartResumeKeepsOnlyTheChangeableSections(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	full := RunMapAffordanceSessionStart(payload(big, "SessionStart", "SESSION", nil), big, testEnv)
	if n := len(strings.Split(contextOf(t, full, "SessionStart"), "\n\n")); n != 8 {
		t.Fatalf("baseline has %d sections", n)
	}
	for _, source := range []any{"startup", "compact", "clear", "", "future-source", 7, nil, "RESUME", "resume "} {
		got := RunMapAffordanceSessionStart(payload(big, "SessionStart", "SESSION", map[string]any{"source": source}), big, testEnv)
		if got != full {
			t.Errorf("source %v changed the output", source)
		}
	}
	resumed := RunMapAffordanceSessionStart(payload(big, "SessionStart", "SESSION", map[string]any{"source": "resume"}), big, testEnv)
	if got := contextOf(t, resumed, "SessionStart"); got != RenderSessionBinding("SESSION", testEnv) {
		t.Errorf("resume context is not exactly the session binding: %q", got)
	}
	// The banner depends on where crw is, so a resumed session hears it again.
	notOnPath := func(k string) (string, bool) { return "chosen-crw", k == "CRW_BIN" }
	got := contextOf(t, RunMapAffordanceSessionStart(payload(big, "SessionStart", "SESSION", map[string]any{"source": "resume"}), big, notOnPath), "SessionStart")
	if parts := strings.Split(got, "\n\n"); len(parts) != 2 || parts[0] != RenderSessionBinding("SESSION", notOnPath) || !strings.Contains(parts[1], "is not on PATH here") {
		t.Errorf("resume with crw off PATH: %q", got)
	}
	// Nothing to say is no output, not an empty envelope.
	if got := RunMapAffordanceSessionStart(payload(big, "SessionStart", "", map[string]any{"source": "resume"}), big, testEnv); got != "" {
		t.Errorf("resume without a session id and with crw on PATH answered %q", got)
	}
}
