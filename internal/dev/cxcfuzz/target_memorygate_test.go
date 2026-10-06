//go:build dev

package cxcfuzz

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The memory gate target is registered under its name, with a Node shim and a comparator.
func TestMemorygateTargetIsRegistered(t *testing.T) {
	target, ok := Lookup("memorygate")
	if !ok {
		t.Fatalf("memorygate is not registered; registered: %v", Names())
	}
	if target.Oracle.Command != "node" || !strings.HasSuffix(target.Oracle.Shim, filepath.Join("testdata", "memorygate", "shim.mjs")) {
		t.Fatalf("oracle %+v", target.Oracle)
	}
	if _, err := os.Stat(target.Oracle.Shim); err != nil {
		t.Fatalf("the shim is missing: %v", err)
	}
}

// The comparator reads the decision first: an oracle deny the port allows is a miss, a port-only
// deny is extra, and two denies that differ only in the brand text of the reason are the same
// answer, because the port renames it (name-substitution R23/R32 and the cli table).
func TestMemorygateCompareJudgesDecisionAndReason(t *testing.T) {
	deny := func(reason string) string {
		return `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"` + reason + `"}}`
	}
	portReason := "[crw MEMORY-WRITE-GATE] Blocked a write: run `crw pabcd memory allow-write`."
	oracleReason := "[codexclaw MEMORY-WRITE-GATE] Blocked a write: run `cxc memory allow-write`."
	for _, c := range []struct {
		name       string
		goSide     any
		oracleSide any
		want       Kind
	}{
		{"both allow", "", "", Same},
		{"brand text is normalised", deny(portReason), deny(oracleReason), Same},
		{"oracle denies, port allows", "", deny(oracleReason), Miss},
		{"port denies, oracle allows", deny(portReason), "", Extra},
		{"both deny, reasons differ", deny("one"), deny("two"), Differ},
		{"a branded destination is not a rename", deny("[crw MEMORY-WRITE-GATE] Blocked a write of a file under the Codex memories directory (/w/codexclaw.md): this session has no explicit user request"), deny("[codexclaw MEMORY-WRITE-GATE] Blocked a write of a file under the Codex memories directory (/w/codexclaw.md): this session has no explicit user request"), Same},
		{"a real destination difference still differs", deny("[crw MEMORY-WRITE-GATE] Blocked a write of a file under the Codex memories directory (/w/a.md)"), deny("[codexclaw MEMORY-WRITE-GATE] Blocked a write of a file under the Codex memories directory (/w/b.md)"), Differ},
		{"a worker failure is not an allow", pyjson.Object{{Key: "error", Value: "boom"}}, deny(oracleReason), Differ},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryGateCompare(c.goSide, c.oracleSide).Kind; got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The Go side answers hook.HandleMemoryWriteGate over the payload with the case's own homes, and a
// destination under the root is denied.
func TestMemorygateGoDeniesAWriteUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	payload := pyjson.Object{
		{Key: "hook_event_name", Value: "PreToolUse"},
		{Key: "session_id", Value: "s1"},
		{Key: "cwd", Value: root + "/work"},
		{Key: "tool_name", Value: "Bash"},
		{Key: "tool_input", Value: pyjson.Object{{Key: "command", Value: "echo hi > " + rootPlaceholder + "/codex-home/memories/n.md"}}},
	}
	got, err := memoryGateGo(pyjson.Object{{Key: "payload", Value: payload}}, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := got.(string)
	if !strings.Contains(text, "\"permissionDecision\":\"deny\"") || !strings.Contains(text, root+"/codex-home/memories/n.md") {
		t.Fatalf("answer %q", text)
	}
	read := pyjson.Object{
		{Key: "hook_event_name", Value: "PreToolUse"},
		{Key: "session_id", Value: "s1"},
		{Key: "cwd", Value: root + "/work"},
		{Key: "tool_name", Value: "Bash"},
		{Key: "tool_input", Value: pyjson.Object{{Key: "command", Value: "rg foo " + rootPlaceholder + "/codex-home/memories/MEMORY.md 2>/dev/null"}}},
	}
	pass, err := memoryGateGo(pyjson.Object{{Key: "payload", Value: read}}, RootEnv(root))
	if err != nil || pass != "" {
		t.Fatalf("a read answered %v, %v", pass, err)
	}
}

// The generator builds a PreToolUse payload over an fs scenario with the memories root and a link
// into it, and the scenario is one the harness can materialise (so a refused case is a defect in
// the generator, not a property of the target).
func TestMemorygateGeneratorBuildsMaterialisableScenarios(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 300; i++ {
		input := memoryGateGenerate(rng, rng.Int())
		root := t.TempDir()
		if err := PrepareRoot(root); err != nil {
			t.Fatal(err)
		}
		if _, err := Scenarios(root, input); err != nil {
			t.Fatalf("the scenario is refused: %v\n%s", err, canonical(input))
		}
	}
}

// The seed cases replay through the Go side with no Node and no worker, each holding its tag.
func TestMemorygateSeedCasesReplay(t *testing.T) {
	target, ok := Lookup("memorygate")
	if !ok {
		t.Fatal("memorygate is not registered")
	}
	cases, err := LoadCases(filepath.Join("testdata", "memorygate"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 6 {
		t.Fatalf("%d seed cases, want at least 6", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if problem := CheckCase(target, c); problem != "" {
				t.Fatal(problem)
			}
		})
	}
}
