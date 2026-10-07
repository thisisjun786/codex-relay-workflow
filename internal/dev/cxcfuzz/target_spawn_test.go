//go:build dev

package cxcfuzz

import (
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The target is in the registry under its own name, so `crw-dev fuzz spawn` finds it, and its oracle
// is the shim beside its cases.
func TestSpawnTargetIsRegistered(t *testing.T) {
	target, ok := Lookup("spawn")
	if !ok {
		t.Fatalf("spawn is not registered (registered: %v)", Names())
	}
	if target.Generate == nil || target.Go == nil || target.Compare == nil {
		t.Fatalf("the target is incomplete: %+v", target)
	}
	if target.Oracle.Command != "node" || !strings.HasSuffix(target.Oracle.Shim, filepath.Join("testdata", "spawn", "shim.mjs")) {
		t.Fatalf("the oracle is %+v", target.Oracle)
	}
}

// The Go side dispatches on the input's fn and answers the value the comparison sees.
func TestSpawnGoAnswersEachFunction(t *testing.T) {
	for _, c := range []struct {
		fn   string
		args []any
		want any
	}{
		{"InferRole", []any{"worker", "x"}, "executor"},
		{"InferRole", []any{"explorer", "CRW-ROLE: reviewer"}, "reviewer"},
		{"InferRole", []any{"", "please review this"}, "reviewer"},
		{"IsV2SpawnInput", []any{pyjson.Object{{Key: "task_name", Value: "t"}}}, true},
		{"IsV2SpawnInput", []any{pyjson.Object{}}, false},
		{"IsFullHistoryFork", []any{pyjson.Object{{Key: "task_name", Value: "t"}}}, true},
		{"IsFullHistoryFork", []any{pyjson.Object{{Key: "fork_turns", Value: "none"}}}, false},
		{"IsFullHistoryFork", []any{pyjson.Object{{Key: "fork_context", Value: true}}}, true},
		{"IsSpawnToolName", []any{"spawn_agent"}, true},
		{"IsSpawnToolName", []any{"collaboration_spawn_agent"}, true},
		{"IsSpawnToolName", []any{"other"}, false},
		{"IsCollaborationToolName", []any{"spawn_agent"}, false},
		{"IsCollaborationToolName", []any{"collaborationspawn_agent"}, true},
	} {
		input := pyjson.Object{{Key: "fn", Value: c.fn}, {Key: "args", Value: c.args}}
		value, err := spawnGo(input, Env{})
		if err != nil {
			t.Fatalf("%s: %v", c.fn, err)
		}
		if canonical(value) != canonical(c.want) {
			t.Errorf("%s(%v) = %v, want %v", c.fn, c.args, value, c.want)
		}
	}
}

// MentionedFolders answers a set, so the target sorts it, and two folder names that differ only in
// which lone surrogate they hold stay apart (CRW-543).
func TestSpawnMentionedFoldersIsSortedAndKeepsSurrogates(t *testing.T) {
	value, err := spawnGo(pyjson.Object{
		{Key: "fn", Value: "MentionedFolders"},
		{Key: "args", Value: []any{"$crw-search $crw-dev skill:///x/crw-\xed\xa0\x80/SKILL.md skill:///x/crw-\xed\xa0\x81/SKILL.md"}},
	}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("MentionedFolders answered %T", value)
	}
	if len(list) != 4 {
		t.Fatalf("the folders are %v, want four", list)
	}
	for i := 1; i < len(list); i++ {
		previous, _ := list[i-1].(string)
		current, _ := list[i].(string)
		if previous > current {
			t.Fatalf("the folders are not sorted: %v", list)
		}
	}
}

// An input outside the grammar is answered with the refusal rather than raising: the harness compares
// an answer's bytes, and the two runtimes' error envelopes would never agree, so a raised refusal
// would read as a difference that is only the error spelling.
func TestSpawnGoAnswersTheRefusal(t *testing.T) {
	for _, input := range []pyjson.Object{
		{{Key: "fn", Value: "Nope"}, {Key: "args", Value: []any{}}},
		{},
	} {
		value, err := spawnGo(input, Env{})
		if err != nil {
			t.Fatalf("%v: %v", input, err)
		}
		if canonical(value) != canonical(spawnRefusal) {
			t.Errorf("%v answered %v, want the refusal", input, value)
		}
	}
}

// The generator is deterministic for one rng and always names an fn the target carries.
func TestSpawnGenerateIsDeterministic(t *testing.T) {
	first := canonical(spawnGenerate(rand.New(rand.NewSource(7)), 1))
	second := canonical(spawnGenerate(rand.New(rand.NewSource(7)), 1))
	if first != second {
		t.Fatalf("the generator is not deterministic:\n%s\n%s", first, second)
	}
	value, err := decode(first)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := value.(pyjson.Object)
	if !ok {
		t.Fatalf("the generator answered %T", value)
	}
	name, _ := object.Lookup("fn")
	text, _ := name.(string)
	known := false
	for _, candidate := range spawnFunctionNames() {
		if candidate == text {
			known = true
		}
	}
	if !known {
		t.Fatalf("the generator named %q, which the target does not carry", text)
	}
}

// The two numbers the c3 tests pin: the port's JSON-writer depth bound the crw-614 case names, and the
// largest input one generated case may build. They are written here rather than read from the target,
// so the test states what it expects rather than what the code happens to do.
const (
	spawnNestingBoundWanted = 4400
	spawnNestingCapWanted   = 1 << 20
)

// c1 (CRW-938): the shim isolates every case before it consults the oracle, the way the echo,
// memorygate and doctor shims do: the five variables a case's homes and its temporary directory live
// in go under request.root. Red before the fix: the shim answered the caller's environment, which is
// what Campaign hands the pool (cxcfuzz.go:181-185, worker.go:80-82).
func TestSpawnShimIsolatesTheCaseRoot(t *testing.T) {
	requireNode(t)
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	decoy := t.TempDir()
	pool := spawnFakePool(t, writeFakeSpawnOracle(t), append(os.Environ(), spawnDecoyEnv(decoy)...))
	defer func() { _ = pool.Close() }()
	reply, err := pool.Call(spawnMentionedFoldersCase(), root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decode(reply)
	if err != nil {
		t.Fatal(err)
	}
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("the shim answered %s, want the five paths", reply)
	}
	want := spawnHomePaths(root)
	sort.Strings(want)
	if len(list) != len(want) {
		t.Fatalf("the shim answered %v, want %v", list, want)
	}
	for i, item := range list {
		if name, _ := item.(string); name != want[i] {
			t.Fatalf("the shim answered %v, want the case root's %v", list, want)
		}
	}
}

// c1 (CRW-938): the pool's start-up handshake is a null input with an empty root, and the shim answers
// it with the refusal rather than consulting the oracle: a readiness probe must not read or write a
// home. The case sent after it still isolates, so the probe left the worker's environment alone.
func TestSpawnShimAnswersTheHandshakeInertly(t *testing.T) {
	requireNode(t)
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	decoy := t.TempDir()
	pool := spawnFakePool(t, writeFakeSpawnOracle(t), append(os.Environ(), spawnDecoyEnv(decoy)...))
	defer func() { _ = pool.Close() }()
	got, err := pool.Call("null", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != canonical(spawnRefusal) {
		t.Fatalf("the handshake answered %s, want the refusal %s", got, canonical(spawnRefusal))
	}
	reply, err := pool.Call(spawnMentionedFoldersCase(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, filepath.Join(root, "home")) {
		t.Fatalf("the case after the handshake answered %s, want the case root's home", reply)
	}
}

// c3 (CRW-938): the generator builds a really nested MentionedFolders input whose nesting depth follows
// the size the campaign passes, around the port's 4,400-level bound, and whose bytes are decided before
// anything is built. Red before the fix: spawnGenerate ignored size, so every generated input nested
// the same - a flat message and no nested argument.
func TestSpawnGenerateNestsBySize(t *testing.T) {
	// The port's 4,400-level bound plus the spread this target draws past it, plus one: the modulus a
	// generated depth is taken over.
	const span = spawnNestingBoundWanted + 3 + 1
	seen := map[int]bool{}
	for size := 0; size < 1024; size++ {
		message := spawnMentionedMessage(t, size)
		if got, want := spawnMentionNesting(message), size%span; got != want {
			t.Fatalf("size %d: the mention nests %d deep, want %d", size, got, want)
		}
		seen[size%span] = true
	}
	if len(seen) < 5 {
		t.Fatalf("the generated nesting follows size only weakly: %d distinct depths", len(seen))
	}
	// The bytes are decided before anything is built: a size far past the cap still builds an input
	// under it, and the mention's nesting never passes the bound plus its spread.
	for _, size := range []int{1 << 20, 1 << 30, 1 << 62} {
		message := spawnMentionedMessage(t, size)
		if len(message) > spawnNestingCapWanted {
			t.Errorf("size %d built a %d-byte message, past the %d cap", size, len(message), spawnNestingCapWanted)
		}
		if depth := spawnMentionNesting(message); depth > spawnNestingBoundWanted+3 {
			t.Errorf("size %d built a mention %d deep, past the port's bound plus its spread", size, depth)
		}
	}
}

// c3 (CRW-938): the depths a campaign draws straddle the port's 4,400-level bound, so the boundary is
// driven from both sides rather than from one.
func TestSpawnNestingStraddlesTheBound(t *testing.T) {
	below, above := false, false
	for size := 0; size <= spawnNestingBoundWanted+16; size++ {
		switch depth := spawnMentionNesting(spawnMentionedMessage(t, size)); {
		case depth > spawnNestingBoundWanted:
			above = true
		case depth > 0:
			below = true
		}
	}
	if !below || !above {
		t.Fatalf("the drawn depths do not straddle the bound (below %v, above %v)", below, above)
	}
}

// c3 (CRW-938): the pinned crw-614-deep-nesting case is a really nested input, not a flat run of one
// character, and both sides answer it.
func TestSpawnDeepNestingCaseIsReallyNested(t *testing.T) {
	target, ok := Lookup("spawn")
	if !ok {
		t.Fatalf("spawn is not registered (registered: %v)", Names())
	}
	cases, err := LoadCases(filepath.Join("testdata", "spawn"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name != "crw-614-deep-nesting" {
			continue
		}
		value, err := decode(c.Input)
		if err != nil {
			t.Fatal(err)
		}
		object, ok := value.(pyjson.Object)
		if !ok {
			t.Fatalf("the pinned case is %T, want the case object", value)
		}
		args, _ := object.Lookup("args")
		list, _ := args.([]any)
		if len(list) == 0 {
			t.Fatal("the pinned case carries no argument")
		}
		message, _ := list[0].(string)
		if depth := spawnMentionNesting(message); depth < spawnNestingBoundWanted {
			t.Fatalf("the pinned case nests %d deep, want at least the port's %d-level bound", depth, spawnNestingBoundWanted)
		}
		if problem := CheckCase(target, c); problem != "" {
			t.Fatal(problem)
		}
		return
	}
	t.Fatal("the pinned case crw-614-deep-nesting is missing")
}

// spawnFakePool starts one spawn worker against a fake oracle tree, with the caller's environment.
func spawnFakePool(t *testing.T, oracle string, env []string) *Pool {
	t.Helper()
	pool, err := NewPool(Oracle{Command: "node", Shim: shimPath("spawn"), Root: oracle}, 1, DefaultTimeout, DefaultStartupTimeout, env)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// spawnMentionedFoldersCase is one MentionedFolders case as JSON text.
func spawnMentionedFoldersCase() string {
	return canonical(pyjson.Object{{Key: "fn", Value: spawnFnMentionedFolders}, {Key: "args", Value: []any{"$crw-dev"}}})
}

// spawnDecoyEnv is a caller's environment: five paths that are not the case root's, so a shim that
// isolates answers the case root's instead.
func spawnDecoyEnv(decoy string) []string {
	return []string{
		"HOME=" + filepath.Join(decoy, "home"),
		"CODEX_HOME=" + filepath.Join(decoy, "codex-home"),
		"CRW_HOME=" + filepath.Join(decoy, "crw-home"),
		"CODEXCLAW_HOME=" + filepath.Join(decoy, "codexclaw-home"),
		"TMPDIR=" + filepath.Join(decoy, "tmp"),
	}
}

// spawnHomePaths is the five paths a case root holds.
func spawnHomePaths(root string) []string {
	return []string{
		filepath.Join(root, "home"),
		filepath.Join(root, "codex-home"),
		filepath.Join(root, "crw-home"),
		filepath.Join(root, "codexclaw-home"),
		filepath.Join(root, "tmp"),
	}
}

// writeFakeSpawnOracle writes a stand-in for the oracle's spawn-attach-hook module under a temporary
// tree and returns that tree. Its mentionedFolders answers the five variables the shim isolates, so a
// test reads back which environment the shim left in place without depending on the real oracle tree.
func writeFakeSpawnOracle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "subagent-config", "dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "export function mentionedFolders() {\n" +
		"  return new Set([process.env.HOME, process.env.CODEX_HOME, process.env.CRW_HOME, process.env.CODEXCLAW_HOME, process.env.TMPDIR]);\n" +
		"}\n" +
		"export function inferRole() { return \"\"; }\n" +
		"export function isV2SpawnInput() { return false; }\n" +
		"export function isFullHistoryFork() { return false; }\n" +
		"export function isSpawnToolName() { return false; }\n" +
		"export function isCollaborationToolName() { return false; }\n"
	if err := os.WriteFile(filepath.Join(dir, "spawn-attach-hook.js"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// spawnMentionedMessage is the message of the first MentionedFolders case the generator draws.
func spawnMentionedMessage(t *testing.T, size int) string {
	t.Helper()
	for seed := int64(0); seed < 1000; seed++ {
		object, ok := spawnGenerate(rand.New(rand.NewSource(seed)), size).(pyjson.Object)
		if !ok {
			continue
		}
		name, _ := object.Lookup("fn")
		if text, _ := name.(string); text != spawnFnMentionedFolders {
			continue
		}
		args, _ := object.Lookup("args")
		list, _ := args.([]any)
		if len(list) != 1 {
			t.Fatalf("size %d: the case carries %d arguments, want the message alone", size, len(list))
		}
		message, _ := list[0].(string)
		return message
	}
	t.Fatalf("no seed drew a MentionedFolders case for size %d", size)
	return ""
}

// spawnMentionNesting is the nesting the generator writes into a mention's skill-link path: the run of
// brackets around the folder, which is the one nesting the syntax MentionedFolders reads has.
func spawnMentionNesting(message string) int {
	deepest := 0
	for at := 0; at < len(message); at++ {
		if message[at] != '[' {
			continue
		}
		open := 0
		for at+open < len(message) && message[at+open] == '[' {
			open++
		}
		rest := message[at+open:]
		if !strings.HasPrefix(rest, "crw-dev") {
			continue
		}
		if !strings.HasPrefix(rest[len("crw-dev"):], strings.Repeat("]", open)) {
			continue
		}
		if open > deepest {
			deepest = open
		}
	}
	return deepest
}
