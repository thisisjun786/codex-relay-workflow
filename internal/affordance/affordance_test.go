package affordance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// testEnv runs crw as `crw`. Its Codex home is the temporary one TestMain gives the process, so the session-start leg records
// what it gave a session there and never under the account's real home.
func testEnv(k string) (string, bool) {
	switch k {
	case "CRW_BIN":
		return "crw", true
	case "CODEX_HOME":
		return os.LookupEnv("CODEX_HOME")
	}
	return "", false
}

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
	if got := RunMapAffordanceSessionStart(raw, ws, testEnv); strings.Contains(contextOf(t, got, "SessionStart"), "This session's id") {
		t.Fatal("invalid surrogate identity bound")
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
	for _, sid := range []string{"parent-session", "child-session"} {
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
		{env("", "/synthetic-home"), `'/synthetic-home/.local/share/crw-runtime/current/bin/crw'`},
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

// sessionEnv is testEnv with a Codex home of its own, where the guidance a session was given is recorded.
func sessionEnv(t *testing.T, bin string) host.LookupEnv {
	t.Helper()
	home := t.TempDir()
	return func(k string) (string, bool) {
		switch k {
		case "CRW_BIN":
			return bin, true
		case "CODEX_HOME":
			return home, true
		}
		return "", false
	}
}

// CRW-1146: a resumed session is not given again the guidance it was given, so source=resume re-issues only what can change (the
// session binding and the not-on-PATH banner); startup, compact, clear, a missing or an unknown or non-string source keep the whole
// list.
func TestSessionStartResumeKeepsOnlyTheChangeableSections(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "crw")
	full := hookAnswer(t, payload(big, "SessionStart", "SESSION", nil), big, env)
	if n := len(strings.Split(contextOf(t, full, "SessionStart"), "\n\n")); n != 8 {
		t.Fatalf("baseline has %d sections", n)
	}
	for _, source := range []any{"startup", "compact", "clear", "", "future-source", 7, nil, "RESUME", "resume "} {
		got := hookAnswer(t, payload(big, "SessionStart", "SESSION", map[string]any{"source": source}), big, env)
		if got != full {
			t.Errorf("source %v changed the output", source)
		}
	}
	resumed := hookAnswer(t, payload(big, "SessionStart", "SESSION", map[string]any{"source": "resume"}), big, env)
	if got := contextOf(t, resumed, "SessionStart"); got != RenderSessionBinding("SESSION", env) {
		t.Errorf("resume context is not exactly the session binding: %q", got)
	}
	// Without a session id nothing can be recorded or looked up, so a resume cannot be known to hold the pointers.
	if got := hookAnswer(t, payload(big, "SessionStart", "", map[string]any{"source": "resume"}), big, env); got == "" {
		t.Errorf("resume without a session id answered nothing")
	}
}

// The first start a session hears the pointers is not always its first start: the switch can be off then, or the hook run by an
// older build that kept no record. A resume of such a session gets everything once, and only the changeable part after that.
func TestSessionStartResumeOfASessionNeverGivenThePointersGivesThemOnce(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "crw")
	resume := payload(big, "SessionStart", "S1", map[string]any{"source": "resume"})
	full := hookAnswer(t, payload(big, "SessionStart", "elsewhere", nil), big, env)
	if got := hookAnswer(t, resume, big, env); got != strings.ReplaceAll(full, "elsewhere", "S1") {
		t.Fatalf("first resume of an unrecorded session did not give the whole list: %q", got)
	}
	if got := contextOf(t, hookAnswer(t, resume, big, env), "SessionStart"); got != RenderSessionBinding("S1", env) {
		t.Errorf("second resume: %q", got)
	}
	// The pointers say how to run crw, so a resume where that differs gives them whole again, with the banner.
	other := func(k string) (string, bool) {
		if k == "CRW_BIN" {
			return "chosen-crw", true
		}
		return env(k)
	}
	got := contextOf(t, hookAnswer(t, resume, big, other), "SessionStart")
	if !strings.Contains(got, "External skill catalogs are searchable") || !strings.Contains(got, "is not on PATH here") {
		t.Errorf("resume with crw elsewhere: %q", got)
	}
	// And a small workspace then a large one: the map pointer appearing is a change too.
	small := t.TempDir()
	sm := payload(small, "SessionStart", "S2", map[string]any{"source": "resume"})
	hookAnswer(t, payload(small, "SessionStart", "S2", nil), small, env)
	if got := contextOf(t, hookAnswer(t, sm, small, env), "SessionStart"); got != RenderSessionBinding("S2", env) {
		t.Errorf("small workspace resume: %q", got)
	}
	seed(t, small, 40)
	if got := contextOf(t, hookAnswer(t, sm, small, env), "SessionStart"); !strings.Contains(got, "Loop contract") {
		t.Errorf("resume after the workspace grew past the map threshold: %q", got)
	}
}

// With crw off PATH the banner is the one changeable section a resume repeats.
func TestSessionStartResumeRepeatsTheBannerWhenCrwIsOffPath(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "chosen-crw")
	hookAnswer(t, payload(big, "SessionStart", "SESSION", nil), big, env)
	got := contextOf(t, hookAnswer(t, payload(big, "SessionStart", "SESSION", map[string]any{"source": "resume"}), big, env), "SessionStart")
	if parts := strings.Split(got, "\n\n"); len(parts) != 2 || parts[0] != RenderSessionBinding("SESSION", env) || !strings.Contains(parts[1], "is not on PATH here") {
		t.Errorf("resume with crw off PATH: %q", got)
	}
}

// limitedWriter takes at most n bytes in all, then fails: a closed pipe (n = 0) or a short write.
type limitedWriter struct{ n int }

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) <= w.n {
		w.n -= len(p)
		return len(p), nil
	}
	k := w.n
	w.n = 0
	return k, errors.New("write failed")
}

// CRW-1180 (S4-F4b): a resume that gave the whole list and the compaction of the same turn stack it twice. The compact right after a whole
// resume answer is silent, once; a start between them, a changed list and a session the resume did not answer for are not the pair.
func TestSessionStartCompactRightAfterAWholeResumeIsSilentOnce(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "crw")
	src := func(id, source string) string {
		return payload(big, "SessionStart", id, map[string]any{"source": source})
	}
	// A session never given the list hears it whole on its resume.
	wholeResume := func(id string) {
		t.Helper()
		if got := contextOf(t, hookAnswer(t, src(id, "resume"), big, env), "SessionStart"); !strings.Contains(got, "Loop contract") {
			t.Fatalf("resume of %s, never given the list: %q", id, got)
		}
		userPrompt(t, big, id, "turn-of-"+id, env) // the prompt of the resume's own turn reaches the prompt hook before the compaction
	}
	full := hookAnswer(t, payload(big, "SessionStart", "S", nil), big, env)
	// The resume of a session that was given the list is short; the compact that follows says the list, and only the list: the
	// binding is in the context from the resume (the compaction record precedes it).
	if got := contextOf(t, hookAnswer(t, src("S", "resume"), big, env), "SessionStart"); got != RenderSessionBinding("S", env) {
		t.Fatalf("short resume: %q", got)
	}
	userPrompt(t, big, "S", "turn-of-S", env)
	compact := contextOf(t, hookAnswer(t, src("S", "compact"), big, env), "SessionStart")
	if want := strings.TrimPrefix(contextOf(t, full, "SessionStart"), RenderSessionBinding("S", env)+"\n\n"); compact != want {
		t.Errorf("compact after a short resume = %q, want the list without the binding the resume gave: %q", compact, want)
	}
	if strings.Contains(compact, RenderSessionBinding("S", env)) {
		t.Errorf("compact after a short resume repeated the binding: %q", compact)
	}
	if got := contextOf(t, hookAnswer(t, src("S", "compact"), big, env), "SessionStart"); got != contextOf(t, full, "SessionStart") {
		t.Errorf("the next compact did not say the binding and the list: %q", got)
	}
	// After a whole resume the compact of the same turn adds nothing, and only that one.
	wholeResume("N")
	if got := hookAnswer(t, src("N", "compact"), big, env); got != "" {
		t.Errorf("compact in the turn of a whole resume repeated the list: %q", got)
	}
	if got := contextOf(t, hookAnswer(t, src("N", "compact"), big, env), "SessionStart"); !strings.Contains(got, "Loop contract") || !strings.Contains(got, "`N`") {
		t.Errorf("the next compact did not say the whole list: %q", got)
	}
	// A start between the resume and the compact begins a new generation.
	wholeResume("C")
	hookAnswer(t, src("C", "clear"), big, env)
	if got := hookAnswer(t, src("C", "compact"), big, env); got == "" {
		t.Error("compact after a clear was silenced")
	}
	// The list changing between the resume and the compact is said.
	wholeResume("D")
	other := func(k string) (string, bool) {
		if k == "CRW_BIN" {
			return "chosen-crw", true
		}
		return env(k)
	}
	if got := contextOf(t, hookAnswer(t, src("D", "compact"), big, other), "SessionStart"); !strings.Contains(got, "Loop contract") {
		t.Errorf("compact after the list changed: %q", got)
	}
	// Another session's compact is not the pair, and a compact without a session id cannot be matched.
	wholeResume("E")
	if got := hookAnswer(t, src("M", "compact"), big, env); got == "" {
		t.Error("another session's compact was silenced")
	}
	if got := hookAnswer(t, src("", "compact"), big, env); got == "" {
		t.Error("a compact without a session id was silenced")
	}
}

// hookAnswer is what the session-start leg writes when the dispatcher runs it (RunHook), recording what the session was given.
func hookAnswer(t *testing.T, raw, cwd string, env host.LookupEnv) string {
	t.Helper()
	var out strings.Builder
	startHook(t, raw, cwd, env, &out)
	return out.String()
}

// startHook runs the session-start leg as the dispatcher does (RunHook), the path that records what the session was given.
func startHook(t *testing.T, raw, cwd string, env host.LookupEnv, out io.Writer) {
	t.Helper()
	if code := RunHook(context.Background(), "session-start", strings.NewReader(raw), out, env, cwd); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// CRW-1146: the guidance counts as given only when the hook wrote all of it. A start or resume whose output could not be written
// (a closed pipe, a short write) leaves no record, so the next resume of that session gives the whole list, then only the binding.
func TestSessionStartGuidanceThatWasNotWrittenIsNotRecorded(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	for _, tc := range []struct {
		name, source string
		budget       int
	}{{"resume-closed", "resume", 0}, {"resume-short", "resume", 100}, {"startup-closed", "startup", 0}, {"startup-short", "startup", 100}} {
		t.Run(tc.name, func(t *testing.T) {
			env := sessionEnv(t, "crw")
			sid := "S-" + tc.name
			resume := payload(big, "SessionStart", sid, map[string]any{"source": "resume"})
			// The whole list as a session with no record gets it, rendered against a home of its own.
			want := RunMapAffordanceSessionStart(resume, big, sessionEnv(t, "crw"))
			startHook(t, payload(big, "SessionStart", sid, map[string]any{"source": tc.source}), big, env, &limitedWriter{n: tc.budget})
			var got strings.Builder
			startHook(t, resume, big, env, &got)
			if got.String() != want {
				t.Fatalf("resume after an unwritten answer has %d sections, want the whole list of %d", len(strings.Split(contextOf(t, got.String(), "SessionStart"), "\n\n")), len(strings.Split(contextOf(t, want, "SessionStart"), "\n\n")))
			}
			var again strings.Builder
			startHook(t, resume, big, env, &again)
			if ctx := contextOf(t, again.String(), "SessionStart"); ctx != RenderSessionBinding(sid, env) {
				t.Errorf("resume after the whole list was written: %q", ctx)
			}
		})
	}
}

// Rendering the answer records nothing; only the hook that wrote it does.
func TestRenderingTheSessionStartAnswerRecordsNothing(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	home := t.TempDir()
	env := func(k string) (string, bool) {
		switch k {
		case "CRW_BIN":
			return "crw", true
		case "CODEX_HOME":
			return home, true
		}
		return "", false
	}
	RunMapAffordanceSessionStart(payload(big, "SessionStart", "SESSION", nil), big, env)
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("rendering wrote under the Codex home: %v %v", entries, err)
	}
}

// userPrompt runs the dispatcher's user-prompt-submit leg for a prompt of a turn.
func userPrompt(t *testing.T, ws, sid, turn string, env host.LookupEnv) {
	t.Helper()
	var out strings.Builder
	raw := payload(ws, "UserPromptSubmit", sid, map[string]any{"turn_id": turn, "prompt": "go on"})
	if code := RunHook(context.Background(), "user-prompt-submit", strings.NewReader(raw), &out, env, ws); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// CRW-1180 (verification P1): the window is no evidence of "the same turn". A compact in a turn after the resume's is a compaction of
// its own, which empties the context that held what the resume said, so it says the guidance again, within the window too. The first
// prompt after the resume (and a second hook call for its turn) belongs to the pair's turn; a prompt of another turn ends the pair.
func TestSessionStartCompactInALaterTurnSaysTheGuidance(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "crw")
	src := func(id, source string) string {
		return payload(big, "SessionStart", id, map[string]any{"source": source})
	}
	whole := func(id string) {
		t.Helper()
		if got := contextOf(t, hookAnswer(t, src(id, "resume"), big, env), "SessionStart"); !strings.Contains(got, "Loop contract") {
			t.Fatalf("resume of %s: %q", id, got)
		}
	}
	// The pair's own turn: its prompt arrives between the resume and the compact, and may arrive twice.
	whole("A")
	userPrompt(t, big, "A", "turn-1", env)
	userPrompt(t, big, "A", "turn-1", env)
	if got := hookAnswer(t, src("A", "compact"), big, env); got != "" {
		t.Errorf("compact in the turn of the resume's first prompt repeated the list: %q", got)
	}
	// A later turn: the compact says the whole list again.
	whole("B")
	userPrompt(t, big, "B", "turn-1", env)
	userPrompt(t, big, "B", "turn-2", env)
	if got := contextOf(t, hookAnswer(t, src("B", "compact"), big, env), "SessionStart"); !strings.Contains(got, "Loop contract") {
		t.Errorf("compact in a later turn was silenced: %q", got)
	}
	// Without turn ids every prompt counts, so the second one ends the pair.
	whole("C")
	userPrompt(t, big, "C", "", env)
	userPrompt(t, big, "C", "", env)
	if got := hookAnswer(t, src("C", "compact"), big, env); got == "" {
		t.Error("compact after two prompts without turn ids was silenced")
	}
	// A prompt of another session does not end the pair.
	whole("D")
	userPrompt(t, big, "D", "turn-1", env)
	userPrompt(t, big, "other", "turn-2", env)
	if got := hookAnswer(t, src("D", "compact"), big, env); got != "" {
		t.Errorf("another session's prompt ended the pair: %q", got)
	}
	// The same holds for the short resume: its pair ends with the second turn's prompt, and the compact says the binding again.
	hookAnswer(t, payload(big, "SessionStart", "E", nil), big, env)
	hookAnswer(t, src("E", "resume"), big, env)
	userPrompt(t, big, "E", "turn-1", env)
	userPrompt(t, big, "E", "turn-2", env)
	if got := contextOf(t, hookAnswer(t, src("E", "compact"), big, env), "SessionStart"); !strings.Contains(got, RenderSessionBinding("E", env)) {
		t.Errorf("compact in a later turn after a short resume omitted the binding: %q", got)
	}
}

// CRW-1180 (verification P1): a short resume that gave the binding and the PATH banner leaves the compact of its turn to say the
// pointers only; the banner is left out when the resume gave the same one, and said when the command changed in between.
func TestSessionStartCompactAfterAShortResumeLeavesOutTheBannerItGave(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "/opt/bin/crw")
	src := func(source string) string { return payload(big, "SessionStart", "S", map[string]any{"source": source}) }
	hookAnswer(t, payload(big, "SessionStart", "S", nil), big, env)
	resume := contextOf(t, hookAnswer(t, src("resume"), big, env), "SessionStart")
	banner := "[crw] `crw` is not on PATH here; wherever docs say `crw`, run: /opt/bin/crw"
	if !strings.Contains(resume, banner) {
		t.Fatalf("short resume gave no banner: %q", resume)
	}
	userPrompt(t, big, "S", "turn-of-S", env)
	compact := contextOf(t, hookAnswer(t, src("compact"), big, env), "SessionStart")
	if strings.Contains(compact, banner) || strings.Contains(compact, RenderSessionBinding("S", env)) || !strings.Contains(compact, "Loop contract") {
		t.Errorf("compact after a short resume = %q", compact)
	}
}

// CRW-1180 (evaluation d1): without evidence that the session's prompt hook runs, the compact cannot tell the resume's turn from a later one,
// so it says the list. A session whose prompt hook ran before the resume pairs its compact whether or not the resume's own prompt came yet.
func TestSessionStartCompactNeedsEvidenceThatThePromptHookRuns(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "crw")
	src := func(id, source string) string {
		return payload(big, "SessionStart", id, map[string]any{"source": source})
	}
	// The prompt hook never ran for this session: the whole resume is followed by a whole compact, as before records.
	hookAnswer(t, src("H", "resume"), big, env)
	if got := contextOf(t, hookAnswer(t, src("H", "compact"), big, env), "SessionStart"); !strings.Contains(got, "Loop contract") {
		t.Errorf("compact of a session whose prompt hook never ran was silenced: %q", got)
	}
	// The prompt hook ran in the session's first life; the resume is whole (the switch changed), the compact comes first in the turn.
	hookAnswer(t, payload(big, "SessionStart", "P", nil), big, env)
	userPrompt(t, big, "P", "turn-0", env)
	other := func(k string) (string, bool) {
		if k == "CRW_BIN" {
			return "other-crw", true
		}
		return env(k)
	}
	hookAnswer(t, src("P", "resume"), big, other)
	if got := hookAnswer(t, src("P", "compact"), big, other); got != "" {
		t.Errorf("compact before the resume's first prompt repeated the list: %q", got)
	}
}
