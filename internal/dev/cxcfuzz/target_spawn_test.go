//go:build dev

package cxcfuzz

import (
	"encoding/json"
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

// The largest filler one generated case may build, written here rather than read from the target, so
// the test states what it expects rather than what the code happens to do.
const spawnMentionCapWanted = 1 << 16

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

// c1 (CRW-938): the worker survives a handshake it cannot consult the oracle for. The shim loads the
// oracle once at module scope and remembers a load that failed, so a missing or hidden oracle tree
// answers an error reply instead of ending the process before its stdin listener exists. Red against
// the shim that let the load throw: the worker died and the pool reported a dead worker rather than an
// answer. Loading at module scope keeps the cost on the worker's start-up budget, which
// TestSpawnSlowOracleLoadIsChargedToStartup pins.
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

// c3 (corrected, CRW-938): the size still shapes a generated case, as the length of the mention text,
// and the bytes are decided from the size before anything is built. This target makes no depth claim:
// MentionedFolders and the other classifiers it compares read strings, and the 4,400-level bound is
// RunSpawnAttachHook's writer bound (internal/role/spawn/hook_route.go, spawnHookRouteMaxDepth), which
// a classifier comparison never reaches. Red before the fix: spawnGenerate ignored size, so every
// generated case carried the same length. Red again before the correction of 2026-10-08: the size was
// spent on a chain of one-element arrays that both harnesses stripped before the classifier ran, so
// the case claimed a depth neither side ever exercised.
func TestSpawnGenerateCarriesTheSizeAsLength(t *testing.T) {
	seen := map[int]bool{}
	for size := 0; size < 512; size++ {
		document := spawnMentionedDocument(t, size)
		args, _ := document.Lookup("args")
		list, _ := args.([]any)
		if len(list) != 1 {
			t.Fatalf("size %d: the case carries %d arguments, want the message alone", size, len(list))
		}
		// The argument is the message itself: nothing the harness could strip stands between it and
		// the classifier, which is what "no depth claim" means here.
		if _, ok := list[0].(string); !ok {
			t.Fatalf("size %d: the classifier is handed %T, want the message string", size, list[0])
		}
		seen[spawnMentionBytes(size)] = true
	}
	if len(seen) < 64 {
		t.Fatalf("the generated length follows size only weakly: %d distinct filler sizes", len(seen))
	}
	// The bytes are decided before anything is built: a size far past the cap still builds a case
	// under it, and no size can make the harness build an unbounded input.
	for _, size := range []int{1 << 20, 1 << 30, 1 << 62} {
		text := canonical(spawnMentionedDocument(t, size))
		if len(text) > spawnMentionCapWanted+1024 {
			t.Errorf("size %d built a %d-byte case, past the %d cap", size, len(text), spawnMentionCapWanted)
		}
	}
	if got := spawnMentionBytes(1 << 62); got >= spawnMentionCapWanted {
		t.Fatalf("a huge size builds %d filler bytes, want under the %d cap", got, spawnMentionCapWanted)
	}
}

// c3 (CRW-938): the generator keeps driving MentionedFolders on varied text, so the classifier the
// target exists for is exercised on more than one mention.
func TestSpawnMentionedFoldersKeepsTheDrawnMessage(t *testing.T) {
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
		message, ok := list[0].(string)
		if !ok {
			t.Fatalf("seed %d: the argument is %T, want the message string", seed, list[0])
		}
		messages[message] = true
	}
	if len(messages) < 16 {
		t.Fatalf("the generator draws only %d distinct messages, so the message is not driven", len(messages))
	}
	// The drawn message is what the two sides classify: the answer names the folder the message mentions.
	for _, message := range spawnWords() {
		document := pyjson.Object{
			{Key: "fn", Value: spawnFnMentionedFolders},
			{Key: "args", Value: []any{message}},
		}
		value, err := spawnGo(document, Env{})
		if err != nil {
			t.Fatal(err)
		}
		want := spawnSortedFolders(spawn.MentionedFolders(message))
		if canonical(value) != canonical(want) {
			t.Fatalf("the case answered %s for %q, want %s", canonical(value), message, canonical(want))
		}
	}
}

// c3 (corrected, CRW-938): no pinned or generated spawn case carries nesting the classifier does not
// receive. The target compares six classifiers that read strings, so a container between the harness
// and the classifier is stripped by both sides and can never reach the product code: a case built that
// way exercises nothing about depth while its name claims a boundary. Red before the correction of
// 2026-10-08: the pinned case nested 4,400 one-element arrays and every generated MentionedFolders case
// nested a chain, all of it unwrapped before the classifier ran.
func TestSpawnCasesCarryNoNestingTheClassifierMisses(t *testing.T) {
	// Every pinned spawn case hands its classifier a value, not a container: the six classifiers read
	// strings or objects, and a chain of one-element arrays is neither.
	cases, err := LoadCases(filepath.Join("testdata", "spawn"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the spawn target has no pinned cases")
	}
	for _, c := range cases {
		value, err := decode(c.Input)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		object, ok := value.(pyjson.Object)
		if !ok {
			t.Fatalf("%s: the case is %T, want the case object", c.Name, value)
		}
		if depth := spawnStrippedDepth(t, object); depth != 0 {
			t.Fatalf("%s: the classifier is handed a chain %d deep, which the harness unwraps before it runs", c.Name, depth)
		}
	}
	// The same holds for every generated case: a size is spent on the mention text, never on
	// containers that stand between the harness and the classifier.
	for seed := int64(0); seed < 512; seed++ {
		for _, size := range []int{0, 1, 4399, 4400, 4401, 1 << 20} {
			object, ok := spawnGenerate(rand.New(rand.NewSource(seed)), size).(pyjson.Object)
			if !ok {
				continue
			}
			if depth := spawnStrippedDepth(t, object); depth != 0 {
				t.Fatalf("seed %d, size %d: the generated case hands the classifier a chain %d deep", seed, size, depth)
			}
		}
	}
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

// writeRecordingSpawnOracle writes a fake oracle tree whose module initialization records the HOME and
// CODEX_HOME it ran under, so a test can read back which environment the import saw. The record path
// travels in an environment variable of its own, which the shim does not touch.
func writeRecordingSpawnOracle(t *testing.T, record string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "subagent-config", "dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "import { writeFileSync } from \"node:fs\";\n" +
		"writeFileSync(process.env.CXCFUZZ_LOAD_RECORD, JSON.stringify({ home: process.env.HOME, codex_home: process.env.CODEX_HOME }));\n" +
		"export function mentionedFolders() { return new Set([\"crw-dev\"]); }\n" +
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

// spawnStrippedDepth is how many one-element containers stand between a case's argument and the value
// the classifier receives: a chain the harness itself would unwrap, so a case that carries one claims a
// depth neither side exercises. A case that hands the classifier its value directly has depth 0.
func spawnStrippedDepth(t *testing.T, document pyjson.Object) int {
	t.Helper()
	args, found := document.Lookup("args")
	if !found {
		return 0
	}
	list, ok := args.([]any)
	if !ok || len(list) != 1 {
		return 0
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
	return depth
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

// c8 (CRW-938, generation 2): the oracle's import-time initialization runs under a root the worker owns,
// never under the caller's environment. The pool hands the worker the caller's environment, so a
// module-scope import would run the oracle's initialization with the real HOME, CODEX_HOME, CRW_HOME,
// CODEXCLAW_HOME and TMPDIR. Red on head 070737c6: the shim imported the oracle at module scope, before
// any request set the five variables, so the initialization saw the caller's homes. Red again if the
// load is merely moved to the first case without an owned root: a case root the harness prepared is not
// the worker's own.
func TestSpawnOracleLoadRunsUnderAnOwnedRoot(t *testing.T) {
	requireNode(t)
	decoy := t.TempDir()
	// The worker's temporary directory: the load root is created under it, so the test can tell a root
	// the worker made for itself from the caller's decoy homes.
	workerTmp := t.TempDir()
	record := filepath.Join(t.TempDir(), "load-env.json")
	pool := spawnFakePool(t, writeRecordingSpawnOracle(t, record),
		append(append(os.Environ(), spawnDecoyEnv(decoy)...), "TMPDIR="+workerTmp, "CXCFUZZ_LOAD_RECORD="+record))
	defer func() { _ = pool.Close() }()

	got, err := pool.Call("null", "")
	if err != nil {
		t.Fatalf("the handshake answered no reply: %v", err)
	}
	if strings.TrimSpace(got) != canonical(spawnRefusal) {
		t.Fatalf("the handshake answered %s, want the refusal %s", got, canonical(spawnRefusal))
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the oracle's initialization recorded nothing: %v", err)
	}
	var seen struct {
		Home      string `json:"home"`
		CodexHome string `json:"codex_home"`
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatalf("the oracle's initialization recorded %s: %v", raw, err)
	}
	caller := spawnDecoyEnv(decoy)
	wantHome := strings.TrimPrefix(caller[0], "HOME=")
	wantCodex := strings.TrimPrefix(caller[1], "CODEX_HOME=")
	if seen.Home == wantHome {
		t.Fatalf("the oracle's initialization ran under the caller's HOME %s", seen.Home)
	}
	if seen.CodexHome == wantCodex {
		t.Fatalf("the oracle's initialization ran under the caller's CODEX_HOME %s", seen.CodexHome)
	}
	// Both point under one root, and that root is the worker's own: it lives under the harness TMPDIR
	// rather than beside the caller's homes, and it is not the case root either.
	if filepath.Dir(seen.Home) != filepath.Dir(seen.CodexHome) {
		t.Fatalf("the initialization ran under %s and %s, want one root's homes", seen.Home, seen.CodexHome)
	}
	root := filepath.Dir(seen.Home)
	if !strings.HasPrefix(root, workerTmp+string(os.PathSeparator)) {
		t.Fatalf("the initialization ran under %s, want a root the worker made under %s", root, workerTmp)
	}
	if root == decoy {
		t.Fatalf("the initialization ran under the caller's decoy %s", decoy)
	}
}

// c8 (CRW-938): the worker's own load root is removed when the worker stops, so the harness leaves no
// tree behind under the caller's TMPDIR. The handshake alone is enough to prove the load happened; the
// listing after Close proves the root is gone.
func TestSpawnOracleLoadRootIsRemoved(t *testing.T) {
	requireNode(t)
	workerTmp := t.TempDir()
	record := filepath.Join(t.TempDir(), "load-env.json")
	pool := spawnFakePool(t, writeRecordingSpawnOracle(t, record),
		append(append(os.Environ(), spawnDecoyEnv(t.TempDir())...), "TMPDIR="+workerTmp, "CXCFUZZ_LOAD_RECORD="+record))
	if _, err := pool.Call("null", ""); err != nil {
		t.Fatalf("the handshake answered no reply: %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if roots := spawnTree(t, workerTmp); len(roots) != 0 {
		t.Fatalf("the worker left its load root behind: %v", roots)
	}
}

// c1 (CRW-938): the oracle is loaded eagerly, under the caller's environment, so that load is safe only while
// the oracle's module initialization does no home I/O. This measures what the harness can observe of that,
// against the real oracle tree: the five homes point at a decoy directory that does not exist yet, the
// handshake is answered, and afterwards neither the decoy nor anything under it may exist. A load that needed
// a home would have had to create one, and a load that read one would have had nothing to read.
//
// What this cannot observe is a read the oracle swallows: the harness sees a worker's filesystem and answers,
// not its syscalls, so a failed read under a missing home leaves no trace. The claim the code makes is
// therefore limited to what is measured here - the load creates nothing and needs no home - and the earlier
// evaluation's objection that a function-call-time environment check cannot show this is why the check is on
// the load itself rather than on the environment at one moment.
func TestSpawnOracleLoadDoesNotTouchTheHomes(t *testing.T) {
	requireNode(t)
	hook := filepath.Join(DefaultOracleRoot, "subagent-config", "dist", "spawn-attach-hook.js")
	if _, err := os.Stat(hook); err != nil {
		t.Skipf("the CXC oracle tree is not extracted here: %v", err)
	}
	decoy := t.TempDir()
	// The homes are named but do not exist, so a load that required one could not proceed silently.
	homes := spawnHomePaths(decoy)
	for _, home := range homes {
		if _, err := os.Stat(home); err == nil {
			t.Fatalf("%s exists before the worker starts", home)
		}
	}
	pool := spawnFakePool(t, DefaultOracleRoot, append(os.Environ(), spawnDecoyEnv(decoy)...))
	defer func() { _ = pool.Close() }()
	got, err := pool.Call("null", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != canonical(spawnRefusal) {
		t.Fatalf("the handshake answered %s, want the refusal %s", got, canonical(spawnRefusal))
	}
	if files := spawnTree(t, decoy); len(files) != 0 {
		t.Fatalf("the oracle's initialization wrote under the homes it was handed: %v", files)
	}
	// The load also did not need the homes to exist: a case after it still answers, and it still answers
	// under the case's own root rather than reading the caller's decoy homes.
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	reply, err := pool.Call(spawnMentionedFoldersCase(), root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(reply) != canonical([]any{"crw-dev"}) {
		t.Fatalf("the case answered %s, want the mention the oracle names", reply)
	}
	if files := spawnTree(t, decoy); len(files) != 0 {
		t.Fatalf("a case wrote under the decoy homes: %v", files)
	}
}

// spawnTree is every path under dir, relative to it, so a test can show that nothing appeared there.
func spawnTree(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}
