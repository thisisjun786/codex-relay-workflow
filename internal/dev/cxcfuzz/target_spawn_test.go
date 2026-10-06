//go:build dev

package cxcfuzz

import (
	"math/rand"
	"path/filepath"
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
