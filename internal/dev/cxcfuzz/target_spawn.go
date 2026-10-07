//go:build dev

package cxcfuzz

import (
	"math/rand"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role/spawn"
)

// spawnTarget fuzzes the spawn hook's exported classifiers: the Go side is internal/role/spawn and
// the oracle side is subagent-config/dist/spawn-attach-hook.js. The issue names eight Go helpers;
// the six below are the ones whose oracle counterpart is exported. StripControlMarkers and
// DenyEnvelope have no exported oracle counterpart (stripControlMarkers at 409 and denyEnvelope at
// 450 are internal there), and the issue's rule leaves such a helper out of the target, so neither is
// reproduced in the shim. The same rule leaves out IsFernetTokenShape (internal, and not named).
func spawnTarget() Target {
	return Target{
		Name:     "spawn",
		Generate: spawnGenerate,
		Go:       spawnGo,
		Oracle:   Oracle{Command: "node", Shim: shimPath("spawn"), Root: DefaultOracleRoot},
		Compare:  spawnCompare,
	}
}

// The fn names the input's "fn" field selects.
const (
	spawnFnInferRole           = "InferRole"
	spawnFnIsV2SpawnInput      = "IsV2SpawnInput"
	spawnFnIsFullHistoryFork   = "IsFullHistoryFork"
	spawnFnIsSpawnToolName     = "IsSpawnToolName"
	spawnFnIsCollaborationTool = "IsCollaborationToolName"
	spawnFnMentionedFolders    = "MentionedFolders"
)

// spawnRefusal is the answer both sides give an input outside the grammar: an fn the table does not
// name, or an argument list that is too short. It is answered rather than raised, because the
// harness compares an answer's bytes and the two runtimes' error envelopes (Go's "GoError" against
// V8's error name) can never match, so a raised refusal would read as a difference that is only the
// harness's error spelling. The shim answers this exact value, so the two sides agree on the
// refusal itself; the helpers below are total, so no generated case can raise on either side.
const spawnRefusal = "the input is outside the target's grammar"

// The characters the generator emits: real code points, so a message holds the character itself
// rather than the six-character text of an escape. The harness writes the message as JSON and its
// reader turns each back into the character, which is what the classifiers' whitespace and line
// rules act on.
const (
	charLineSeparator  = "\u2028"
	charParagraphBreak = "\u2029"
	charCarriageReturn = "\r"
	charNewline        = "\n"
	charNul            = "\x00"
	charNoBreakSpace   = "\u00a0"
	charIdeographic    = "\u3000"
	charByteOrderMark  = "\ufeff"
	charCombiningMark  = "\u0301"
	charAstralPair     = "\U0001F600"
	charTab            = "\t"
)

// spawnLoneSurrogates are lone surrogates as the three WTF-8 bytes a Go string holds them in: two
// high (U+D800, U+D801) and one low (U+DC00), which the issue's generator list asks for. The
// harness writes each back as its \udXXX escape and the oracle's reader makes it a JavaScript lone
// surrogate code unit, while the port's own CRW-543 test pins that two folder names differing only
// in which surrogate they hold stay apart.
var spawnLoneSurrogates = []string{"\xed\xa0\x80", "\xed\xa0\x81", "\xed\xb0\x80"}

// The nesting a MentionedFolders case builds. The depth is the number the port's JSON-writer bound
// names (spawnHookRouteMaxDepth, internal/role/spawn/hook_route.go:32), so a case's depth follows the
// size the campaign passes up to it and reaches the bound from both sides.
//
// The mention grammar MentionedFolders reads has no nesting of its own: both of its patterns capture a
// flat run of characters, so a bracket run inside a folder name is ordinary string data to the oracle
// and to the port alike and exercises no depth. What does nest is the value the classifier is handed,
// and the nesting is put there - in the argument itself, not beside the call - so both sides walk the
// drawn number of containers to reach the message before MentionedFolders sees it: the port through
// pyjson and spawnUnwrapArgument, the shim through JSON.parse and unwrapArgument. The message itself
// is still drawn by spawnMessage, so the classifier keeps being driven on varied mention text. The
// pinned deep-nesting case is that argument at the bound.
//
// Nothing is put in a second argument: MentionedFolders reads only its first, so a value there would be
// serialized and parsed by the harness and reach neither side's code.
const (
	// spawnNestingBound is the port's JSON-writer depth bound (spawnHookRouteMaxDepth) that the
	// crw-614 case names, and spawnNestingSpread how far past it a case reaches, so a generated depth
	// straddles the boundary rather than sitting on one side of it.
	spawnNestingBound  = 4400
	spawnNestingSpread = 3
	// spawnNestingBytesPerLevel is what one nesting level costs in the canonical text: one '[' and one
	// ']'. The rest of the case is fixed, so a depth's bytes are arithmetic rather than measured.
	spawnNestingBytesPerLevel = 2
)

// spawnNestingCase is one case: the MentionedFolders call whose argument is the message nested depth
// containers deep. depth 0 is the bare message.
func spawnNestingCase(message string, depth int) pyjson.Object {
	var argument any = message
	for i := 0; i < depth; i++ {
		argument = []any{argument}
	}
	return pyjson.Object{
		{Key: "fn", Value: spawnFnMentionedFolders},
		{Key: "args", Value: []any{argument}},
	}
}

// spawnUnwrapArgument follows a chain of one-element arrays down to the value at its end: the generator
// nests the MentionedFolders argument to the depth a case drew, so the two sides walk the same
// containers before the classifier sees the message. A value that is not such a chain is itself.
func spawnUnwrapArgument(value any) any {
	for {
		list, ok := value.([]any)
		if !ok || len(list) != 1 {
			return value
		}
		value = list[0]
	}
}

// spawnNestingFixedBytes is the canonical text of a case of this message at depth 0, which is the whole
// case without the nesting. It is a function rather than a package-level constant because measuring it
// builds a case, and the runtime criterion forbids work in a package initializer.
func spawnNestingFixedBytes(message string) int {
	return len(canonical(spawnNestingCase(message, 0)))
}

// spawnNestingBytes is what one case of this message at this depth costs, in the canonical text the
// harness sends. It is arithmetic from the message and the depth, never a measurement of the built
// case, so a caller decides whether a depth fits before anything of that depth is built.
func spawnNestingBytes(message string, depth int) int {
	if depth < 0 {
		depth = 0
	}
	return spawnNestingFixedBytes(message) + spawnNestingBytesPerLevel*depth
}

// spawnNestingMaxBytes caps what one generated case may build: the byte cost of a case at the bound the
// crw-614 case names, which is the deepest case this target aims a generator at.
func spawnNestingMaxBytes(message string) int {
	return spawnNestingBytes(message, spawnNestingBound+spawnNestingSpread)
}

// spawnNestingLimit is the deepest depth the cap allows for this message. It is derived from the cap
// rather than assumed equal to the bound and its spread, so a case's bytes are decided by the cap and
// the two can never drift into a depth the cap forbids.
func spawnNestingLimit(message string) int {
	limit := (spawnNestingMaxBytes(message) - spawnNestingFixedBytes(message)) / spawnNestingBytesPerLevel
	if limit < 0 {
		limit = 0
	}
	return limit
}

// spawnNestingDepth is the nesting depth one case builds. The campaign's size is an arbitrary int
// (rng.Int()), so it is folded into the port's bound and its spread rather than used directly, which
// puts a case on either side of the 4,400-level bound; the byte cap then clamps it, so a depth the cap
// forbids is never built.
func spawnNestingDepth(size int, message string) int {
	if size < 0 {
		size = -size
	}
	depth := size % (spawnNestingBound + spawnNestingSpread + 1)
	if limit := spawnNestingLimit(message); depth > limit {
		depth = limit
	}
	return depth
}

// spawnMentionedFoldersInput is the whole MentionedFolders case, in the grammar the target reads: the
// drawn message, nested to the depth the size draws. The depth is decided from the message's bytes and
// the cap before the nesting is built.
func spawnMentionedFoldersInput(rng *rand.Rand, size int) pyjson.Object {
	message := spawnMessage(rng, 1+rng.Intn(8))
	return spawnNestingCase(message, spawnNestingDepth(size, message))
}

// spawnWords are the message pieces the generator assembles: the role-like spellings, the mention
// shapes, the header lines, and the characters that make the classifiers' folding and scanning
// interesting.
func spawnWords() []string {
	words := []string{
		"",
		"a",
		"review",
		"audit",
		"verify",
		"verification",
		"red-team",
		"red team",
		"\uB9AC\uBDF0",
		"\uAC80\uC99D",
		"TASK:",
		"CRW-ROLE: architect",
		"CRW-ROLE: reviewer",
		"CRW-ROLE: explorer",
		"CRW-ROLE:executor",
		"CRW-ROLE: reviewer" + charTab,
		"CRW-ROLE: reviewer" + charCarriageReturn,
		"$crw-dev",
		"$crw:crw-dev",
		"$CRW-Dev",
		"$crw-dev-testing",
		"$crw-search",
		"$crw-lunasearch",
		"[$crw-dev](skill:///x/crw-dev/SKILL.md)",
		"skill:///x/crw-\u0130ST/SKILL.md",
		"skill:///x/crw-\u039f\u03a3/SKILL.md",
		"@/path/to/thing",
		"CRW-SUBSPAWN-ALLOWED",
		"[CRW-SUBSPAWN-GRANT:0000000000000000000000000000000000000000000000000000000000000000]",
		charLineSeparator,
		charParagraphBreak,
		charCarriageReturn,
		charNewline,
		charNul,
		charNoBreakSpace,
		charIdeographic,
		charByteOrderMark,
		charCombiningMark,
		charAstralPair,
		charTab,
	}
	for _, surrogate := range spawnLoneSurrogates {
		words = append(words, "skill:///x/crw-"+surrogate+"/SKILL.md")
	}
	return words
}

// spawnFunctionNames is the fn table, in a fixed order.
func spawnFunctionNames() []string {
	return []string{
		spawnFnInferRole,
		spawnFnIsV2SpawnInput,
		spawnFnIsFullHistoryFork,
		spawnFnIsSpawnToolName,
		spawnFnIsCollaborationTool,
		spawnFnMentionedFolders,
	}
}

// spawnGenerate builds one input: {"fn": name, "args": [...]}. It is deterministic for a given rng.
// size drives the MentionedFolders case's nesting depth, which is the one structural parameter this
// target has to reach a boundary with.
func spawnGenerate(rng *rand.Rand, size int) any {
	names := spawnFunctionNames()
	name := names[rng.Intn(len(names))]
	var args []any
	switch name {
	case spawnFnInferRole:
		agentTypes := []any{"worker", "executor", "architect", "reviewer", "explorer", "other", "", nil, 7, true}
		args = []any{agentTypes[rng.Intn(len(agentTypes))], spawnMessage(rng, 1+rng.Intn(6))}
	case spawnFnIsV2SpawnInput, spawnFnIsFullHistoryFork:
		args = []any{spawnToolInput(rng)}
	case spawnFnIsSpawnToolName, spawnFnIsCollaborationTool:
		toolNames := []any{"spawn_agent", "collaborationspawn_agent", "collaboration.spawn_agent", "collaboration_spawn_agent", "Spawn_Agent", "spawn_agent ", "", nil, 3, true}
		args = []any{toolNames[rng.Intn(len(toolNames))]}
	case spawnFnMentionedFolders:
		return spawnMentionedFoldersInput(rng, size)
	}
	return pyjson.Object{{Key: "fn", Value: name}, {Key: "args", Value: args}}
}

// spawnMessage joins n pieces into a message, so a case can hold a header, a mention and a
// surrogate in one string.
func spawnMessage(rng *rand.Rand, n int) string {
	words := spawnWords()
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, words[rng.Intn(len(words))])
	}
	// A long message now and then, so a case can reach the length the issue's generator names
	// without a case ever holding more than a few hundred kilobytes.
	if rng.Intn(32) == 0 {
		parts = append(parts, strings.Repeat(words[rng.Intn(len(words))], 1+rng.Intn(4096)))
	}
	return strings.Join(parts, " ")
}

// spawnToolInput builds an arbitrary toolInput object: the v2 markers, the wrong types, the null
// members and the nested values the issue asks for.
func spawnToolInput(rng *rand.Rand) pyjson.Object {
	var value pyjson.Object
	keys := []string{"task_name", "fork_turns", "fork_context", "message", "agent_type"}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	for _, key := range keys {
		if rng.Intn(3) == 0 {
			continue
		}
		switch rng.Intn(8) {
		case 0:
			value = value.Set(key, nil)
		case 1:
			value = value.Set(key, rng.Intn(64))
		case 2:
			value = value.Set(key, rng.Intn(2) == 0)
		case 3:
			value = value.Set(key, []any{rng.Intn(4), "x"})
		case 4:
			value = value.Set(key, pyjson.Object{{Key: "nested", Value: []any{rng.Intn(4)}}})
		case 5:
			value = value.Set(key, spawnWords()[rng.Intn(len(spawnWords()))])
		case 6:
			value = value.Set(key, []string{"all", "none", "", "1", "ALL", "  all  ", "0"}[rng.Intn(7)])
		default:
			value = value.Set(key, "none")
		}
	}
	return value
}

// spawnGo is the target's Go side: it reads the input's fn and args, calls the matching helper and
// answers the value the comparison sees.
func spawnGo(input any, env Env) (any, error) {
	object, ok := input.(pyjson.Object)
	if !ok {
		return nil, errNotAnObject()
	}
	name, _ := object.Lookup("fn")
	fn, _ := name.(string)
	args, _ := object.Lookup("args")
	list, _ := args.([]any)
	switch fn {
	case spawnFnInferRole:
		message, _ := spawnArg(list, 1).(string)
		return string(spawn.InferRole(spawnArg(list, 0), message)), nil
	case spawnFnIsV2SpawnInput:
		return spawn.IsV2SpawnInput(spawnToolInputArg(list)), nil
	case spawnFnIsFullHistoryFork:
		return spawn.IsFullHistoryFork(spawnToolInputArg(list)), nil
	case spawnFnIsSpawnToolName:
		return spawn.IsSpawnToolName(spawnArg(list, 0)), nil
	case spawnFnIsCollaborationTool:
		return spawn.IsCollaborationToolName(spawnArg(list, 0)), nil
	case spawnFnMentionedFolders:
		message, _ := spawnUnwrapArgument(spawnArg(list, 0)).(string)
		return spawnSortedFolders(spawn.MentionedFolders(message)), nil
	default:
		return spawnRefusal, nil
	}
}

// spawnArg is one argument, or nil when the list is short.
func spawnArg(list []any, i int) any {
	if i >= len(list) {
		return nil
	}
	return list[i]
}

// spawnToolInputArg reads the toolInput argument as the map the two helpers take: an argument that
// is not an object reads as an empty one, which is what the shim hands the oracle.
func spawnToolInputArg(list []any) map[string]any {
	object, ok := spawnArg(list, 0).(pyjson.Object)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(object))
	for _, item := range object {
		out[item.Key] = item.Value
	}
	return out
}

// spawnSortedFolders is a folder set as the sorted array both sides compare: MentionedFolders
// answers a set, so the two sides are compared after sorting.
func spawnSortedFolders(folders map[string]bool) []any {
	names := make([]string, 0, len(folders))
	for name := range folders {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, name := range names {
		out = append(out, name)
	}
	return out
}

// spawnCompare is the issue's comparison: the two answers in the harness's canonical form, compared
// as bytes.
func spawnCompare(goOut, oracleOut any) Verdict {
	return compareJSON(goOut, oracleOut)
}
