//go:build dev

package cxcfuzz

import (
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role/spawn"
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

// c1 (CRW-938): the worker survives a handshake it cannot consult the oracle for. The shim's oracle
// import runs per request, after the case is isolated, so a missing or hidden oracle tree answers an
// error reply instead of ending the process before its stdin listener exists. Red before the fix: the
// import was at module load, so the worker died and the pool reported a dead worker rather than an
// answer, and a present tree would have run its module initialization under the caller's homes.
func TestSpawnShimAnswersWithoutTheOracleTree(t *testing.T) {
	requireNode(t)
	decoy := t.TempDir()
	pool := spawnFakePool(t, filepath.Join(decoy, "no-such-oracle"), append(os.Environ(), spawnDecoyEnv(decoy)...))
	defer func() { _ = pool.Close() }()
	got, err := pool.Call("null", "")
	if err != nil {
		t.Fatalf("the handshake with no oracle tree answered no reply: %v", err)
	}
	if strings.TrimSpace(got) != canonical(spawnRefusal) {
		t.Fatalf("the handshake answered %s, want the refusal %s", got, canonical(spawnRefusal))
	}
	// A case whose oracle cannot be loaded is answered with an error envelope rather than a dead
	// worker: the readiness probe above proved the listener is up, and this proves it stays up.
	if _, err := pool.Call(spawnMentionedFoldersCase(), t.TempDir()); err != nil {
		t.Fatalf("the case with no oracle tree answered no reply: %v", err)
	}
}

// c3 (CRW-938): the generator builds a really nested MentionedFolders input whose nesting depth follows
// the size the campaign passes, around the port's 4,400-level bound, and whose bytes are decided before
// anything is built. Red before the fix: spawnGenerate ignored size, so every generated input nested
// the same - a flat message and no nested argument. Red again before the correction the pre-merge
// evaluation of head 59ab846ba called for: the depth was a run of brackets inside one folder string,
// which both readers take as ordinary text, so the case's containers never nested at all.
func TestSpawnGenerateNestsBySize(t *testing.T) {
	// The port's 4,400-level bound plus the spread this target draws past it, plus one: the modulus a
	// generated depth is taken over.
	const span = spawnNestingBoundWanted + 3 + 1
	seen := map[int]bool{}
	for size := 0; size < 1024; size++ {
		document := spawnMentionedDocument(t, size)
		if got, want := spawnDocumentDepth(t, document), size%span; got != want {
			t.Fatalf("size %d: the case nests %d deep, want %d", size, got, want)
		}
		// The depth is real nesting, not a run of characters inside a string: the canonical text the
		// harness sends carries one array per level, which is what a JSON reader walks. The deepest
		// containers are the case object, the args array, and the chain the argument is.
		if got, want := spawnTextDepth(t, canonical(document)), size%span+2; got != want {
			t.Fatalf("size %d: the case text nests %d containers deep, want %d", size, got, want)
		}
		seen[size%span] = true
	}
	if len(seen) < 5 {
		t.Fatalf("the generated nesting follows size only weakly: %d distinct depths", len(seen))
	}
	// The bytes are decided before anything is built: a size far past the cap still builds an input
	// under it, and the case's nesting never passes the bound plus its spread.
	for _, size := range []int{1 << 20, 1 << 30, 1 << 62} {
		text := canonical(spawnMentionedDocument(t, size))
		if len(text) > spawnNestingCapWanted {
			t.Errorf("size %d built a %d-byte case, past the %d cap", size, len(text), spawnNestingCapWanted)
		}
		if depth := spawnTextDepth(t, text); depth > spawnNestingBoundWanted+4 {
			t.Errorf("size %d built a case %d containers deep, past the port's bound plus its spread", size, depth)
		}
	}
	// The cap, not the bound, is what forbids a deeper case: a depth past what the cap allows is
	// clamped rather than built, so no path silently degrades to a flat one.
	message := "$crw-dev"
	deepest := spawnNestingLimit(message)
	if got := spawnNestingBytes(message, deepest); got > spawnNestingMaxBytes(message) {
		t.Fatalf("the deepest allowed depth costs %d bytes, past the cap %d", got, spawnNestingMaxBytes(message))
	}
	// The limit is the deepest the cap allows: one level more would cost more than the cap.
	if got := spawnNestingBytes(message, deepest+1); got <= spawnNestingMaxBytes(message) {
		t.Fatalf("a depth one past the limit costs %d bytes, within the cap %d: the limit is too low", got, spawnNestingMaxBytes(message))
	}
	if depth := spawnNestingDepth(1<<40, message); depth > deepest {
		t.Fatalf("a huge size drew depth %d, past what the cap allows %d", depth, deepest)
	}
	// No size builds past the cap, and the deepest any size reaches is exactly the limit.
	reached := 0
	for size := 0; size < 3*(spawnNestingBoundWanted+spawnNestingSpread+1); size++ {
		depth := spawnDocumentDepth(t, spawnMentionedFoldersInput(rand.New(rand.NewSource(int64(size))), size))
		if depth > deepest {
			t.Fatalf("size %d built %d deep, past what the cap allows %d", size, depth, deepest)
		}
		if depth > reached {
			reached = depth
		}
	}
	if reached != deepest {
		t.Fatalf("the deepest drawn case is %d, want the cap's limit %d", reached, deepest)
	}
}

// c3 (CRW-938): the depths a campaign draws straddle the port's 4,400-level bound, so the boundary is
// driven from both sides rather than from one.
func TestSpawnNestingStraddlesTheBound(t *testing.T) {
	below, above := false, false
	for size := 0; size <= spawnNestingBoundWanted+16; size++ {
		switch depth := spawnDocumentDepth(t, spawnMentionedDocument(t, size)); {
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

// c3 (CRW-938): the nesting is added to the drawn message rather than replacing it, so the classifier
// the target exists for keeps being driven on varied text. The pre-merge evaluation of head d33afe26
// found the opposite - every MentionedFolders case carried one fixed mention - so this pins that the
// message is still drawn and still reaches MentionedFolders at the bottom of the chain.
func TestSpawnNestingKeepsTheDrawnMessage(t *testing.T) {
	messages := map[string]bool{}
	for seed := int64(0); seed < 256; seed++ {
		document, ok := spawnMentionedCaseForSeed(t, seed, 5)
		if !ok {
			continue
		}
		args, _ := document.Lookup("args")
		list, _ := args.([]any)
		if len(list) != 1 {
			t.Fatalf("seed %d: the case carries %d arguments, want the message alone", seed, len(list))
		}
		message, ok := spawnUnwrapArgument(list[0]).(string)
		if !ok {
			t.Fatalf("seed %d: the nested argument ends in %T, want the message", seed, spawnUnwrapArgument(list[0]))
		}
		messages[message] = true
	}
	if len(messages) < 16 {
		t.Fatalf("the generator draws only %d distinct messages, so the nesting replaced the message", len(messages))
	}
	// The drawn message is what the two sides classify: the Go side reads it through the chain, and the
	// answer names the folder the message mentions.
	for _, message := range spawnWords() {
		document := spawnNestingCase(message, 3)
		value, err := spawnGo(document, Env{})
		if err != nil {
			t.Fatal(err)
		}
		want := spawnSortedFolders(spawn.MentionedFolders(message))
		if canonical(value) != canonical(want) {
			t.Fatalf("the nested case answered %s for %q, want %s", canonical(value), message, canonical(want))
		}
	}
}

// c3 (CRW-938): the pinned crw-614-deep-nesting case is a really nested input, not a flat run of one
// character and not a run of brackets inside a string, and both sides answer it.
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
		if depth := spawnDocumentDepth(t, object); depth < spawnNestingBoundWanted {
			t.Fatalf("the pinned case nests %d deep, want at least the port's %d-level bound", depth, spawnNestingBoundWanted)
		}
		if depth := spawnTextDepth(t, c.Input); depth <= spawnNestingBoundWanted {
			t.Fatalf("the pinned case's text nests %d containers deep, want the containers themselves past the %d-level bound", depth, spawnNestingBoundWanted)
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

// writeSlowSpawnOracle writes a fake oracle tree whose module takes delay to initialize, so a test can
// tell a load charged to the worker's start-up budget from one charged to a case's timeout.
func writeSlowSpawnOracle(t *testing.T, delay time.Duration) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "subagent-config", "dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "await new Promise((resolve) => setTimeout(resolve, " + strconv.FormatInt(delay.Milliseconds(), 10) + "));\n" +
		"export function mentionedFolders() { return new Set([process.env.HOME, process.env.CODEX_HOME, process.env.CRW_HOME, process.env.CODEXCLAW_HOME, process.env.TMPDIR]); }\n" +
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

// spawnMentionedDocument is the first MentionedFolders case the generator draws.
func spawnMentionedDocument(t *testing.T, size int) pyjson.Object {
	t.Helper()
	for seed := int64(0); seed < 1000; seed++ {
		if object, ok := spawnMentionedCaseForSeed(t, seed, size); ok {
			return object
		}
	}
	t.Fatalf("no seed drew a MentionedFolders case for size %d", size)
	return nil
}

// spawnMentionedCaseForSeed is one generated case, when that seed draws a MentionedFolders case.
func spawnMentionedCaseForSeed(t *testing.T, seed int64, size int) (pyjson.Object, bool) {
	t.Helper()
	object, ok := spawnGenerate(rand.New(rand.NewSource(seed)), size).(pyjson.Object)
	if !ok {
		return nil, false
	}
	name, _ := object.Lookup("fn")
	if text, _ := name.(string); text != spawnFnMentionedFolders {
		return nil, false
	}
	args, _ := object.Lookup("args")
	list, _ := args.([]any)
	if len(list) != 1 {
		t.Fatalf("seed %d, size %d: the case carries %d arguments, want the message alone", seed, size, len(list))
	}
	return object, true
}

// spawnDocumentDepth is how many containers deep the case's argument sits, counted through the decoded
// document rather than read from a string: args[0], one array per level down to the mention.
func spawnDocumentDepth(t *testing.T, document pyjson.Object) int {
	t.Helper()
	args, found := document.Lookup("args")
	if !found {
		t.Fatalf("the case carries no args: %s", canonical(document))
	}
	list, ok := args.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("the case carries %s, want the one argument the classifier reads", canonical(document))
	}
	value := list[0]
	depth := 0
	for {
		chain, ok := value.([]any)
		if !ok || len(chain) != 1 {
			break
		}
		depth++
		value = chain[0]
	}
	if _, ok := value.(string); !ok {
		t.Fatalf("the nested argument ends in %T, want the mention the classifier reads", value)
	}
	return depth
}

// spawnTextDepth is how many containers deep a case's JSON text nests, counted by walking its bytes:
// the depth a JSON reader - the shim's JSON.parse and the port's pyjson decoder - walks the document
// to. A run of brackets inside a string is invisible to this count, which is the difference between a
// really nested case and one whose "nesting" is only characters in a folder name.
func spawnTextDepth(t *testing.T, text string) int {
	t.Helper()
	deepest, depth, quoted, escaped := 0, 0, false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && quoted:
			escaped = true
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '[' || c == '{':
			depth++
			if depth > deepest {
				deepest = depth
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	if quoted || depth != 0 {
		t.Fatalf("the text is not balanced JSON: %s", text)
	}
	return deepest
}

// c1 (CRW-938): the oracle's module initialization is charged to the worker's start-up budget, not to a
// case's timeout. The load happens before the stdin listener exists, so the pool's readiness handshake
// cannot reply until it is done and the time it takes is charged to startup; a load moved into the first
// case would instead be charged to that case's timeout and a slow import would kill a worker that had
// already answered its handshake. The pre-merge evaluation of head a9f956c3 found exactly that
// regression in a per-request import, so this pins the timing rather than only the answers.
func TestSpawnSlowOracleLoadIsChargedToStartup(t *testing.T) {
	requireNode(t)
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	decoy := t.TempDir()
	// A per-request timeout far below the load time, and a startup budget far above it: the worker
	// answers only if the load is paid by startup.
	pool, err := NewPool(
		Oracle{Command: "node", Shim: shimPath("spawn"), Root: writeSlowSpawnOracle(t, 750*time.Millisecond)},
		1, 50*time.Millisecond, 20*time.Second, append(os.Environ(), spawnDecoyEnv(decoy)...))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	reply, err := pool.Call(spawnMentionedFoldersCase(), root)
	if err != nil {
		t.Fatalf("a worker whose oracle took longer than the per-request timeout to load did not answer: %v", err)
	}
	if !strings.Contains(reply, filepath.Join(root, "home")) {
		t.Fatalf("the case answered %s, want the case root's home", reply)
	}
}
