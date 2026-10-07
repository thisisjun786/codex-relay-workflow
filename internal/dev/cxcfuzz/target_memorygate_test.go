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

// c1 (CRW-857): the Go side substitutes the ROOT placeholder in the decoded input's string values
// before the payload is serialized, exactly as the oracle's shim does (testdata/memorygate/shim.mjs
// substitute(), :19-27). A case root that itself holds a double quote, a backslash and an LF must
// not corrupt the JSON the gate reads: substituting into the serialized text built invalid JSON
// out of such a root, and the gate then answered nothing where the oracle denies.
func TestMemorygateRootSubstitutionSurvivesAHostileRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "q\"b\\s\nlf")
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	payload := pyjson.Object{
		{Key: "hook_event_name", Value: "PreToolUse"},
		{Key: "session_id", Value: "s1"},
		{Key: "turn_id", Value: "t1"},
		{Key: "cwd", Value: rootPlaceholder + "/work"},
		{Key: "tool_name", Value: "memoriesadd_ad_hoc_note"},
		{Key: "tool_input", Value: pyjson.Object{{Key: "filename", Value: "note.md"}, {Key: "note", Value: "x"}}},
	}
	got, err := memoryGateGo(pyjson.Object{{Key: "payload", Value: payload}}, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := got.(string)
	if answer == "" {
		t.Fatalf("the gate allowed a note write under the root %q", root)
	}
	if _, err := decode(answer); err != nil {
		t.Fatalf("the answer is not valid JSON: %v (answer %q)", err, answer)
	}
	decision, reason := memoryGateAnswer(answer)
	if decision != "deny" {
		t.Fatalf("decision %q, want deny (answer %q)", decision, answer)
	}
	if !strings.Contains(reason, root) {
		t.Fatalf("the deny reason does not carry the substituted root: %q", reason)
	}
}

// c3 (CRW-857): a symlink whose target starts with the ROOT placeholder takes the case root, and
// the gate answers over the tree it builds. Red first: the builder refused every absolute target.
func TestMemorygateAbsoluteLinkTargetBuildsAndAnswers(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	input := pyjson.Object{
		{Key: "fs", Value: []any{
			memoryGateDir("codex-home/memories"),
			memoryGateLink("work/abs", rootPlaceholder+"/codex-home/memories"),
		}},
		{Key: "payload", Value: pyjson.Object{
			{Key: "hook_event_name", Value: "PreToolUse"},
			{Key: "session_id", Value: "s1"},
			{Key: "cwd", Value: rootPlaceholder + "/work"},
			{Key: "tool_name", Value: "Write"},
			{Key: "tool_input", Value: pyjson.Object{{Key: "file_path", Value: "abs/n.md"}}},
		}},
	}
	if _, err := Scenarios(root, input); err != nil {
		t.Fatalf("the scenario is refused: %v", err)
	}
	got, err := memoryGateGo(input, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := got.(string)
	decision, reason := memoryGateAnswer(answer)
	if decision != "deny" {
		t.Fatalf("the gate did not deny a write through an absolute link target: %q", answer)
	}
	// The write reaches the memories root through the absolute link, so the gate denies it. The
	// oracle compares the destination text only and allows it; that divergence is the pinned
	// CRW-518 class, and this test pins the tree the builder must be able to build.
	if want := filepath.Join(root, "work", "abs", "n.md"); !strings.Contains(reason, want) {
		t.Fatalf("the deny reason names %q, want it to name %q", reason, want)
	}
}

// c3 (CRW-857): the memorygate generator mixes relative and absolute link targets, so both forms
// reach the campaigns. The seed is fixed, so the set it draws is the same on every run.
func TestMemorygateGeneratorMixesRelativeAndAbsoluteTargets(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	relative, absolute := false, false
	for i := 0; i < 400; i++ {
		input := memoryGateGenerate(rng, rng.Int())
		value, found := field(input, "fs")
		if !found {
			t.Fatal("the generated input has no fs array")
		}
		entries, _ := value.([]any)
		for _, entry := range entries {
			target, ok := field(entry, "target")
			text, isText := target.(string)
			if !ok || !isText {
				continue
			}
			if strings.HasPrefix(text, rootPlaceholder+"/") {
				absolute = true
			} else {
				relative = true
			}
		}
	}
	if !relative {
		t.Fatal("the generator never emitted a relative link target")
	}
	if !absolute {
		t.Fatal("the generator never emitted an absolute link target")
	}
}

// c4 (CRW-857): the two reproductions the issue asked for are pinned in cases.json in the shape
// it names, each judged by the tag its record justifies.
func TestMemorygateRequestedSeedsArePresent(t *testing.T) {
	cases, err := LoadCases(filepath.Join("testdata", "memorygate"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"link-newline-name":        "the Path(root, triple-quoted alias holding a real CR) form over a work/alias with an LF link",
		"crw650-open-triple-quote": "open() over a triple-quoted memories path holding the same quote character",
	}
	for _, c := range cases {
		_, ok := want[c.Name]
		if !ok {
			continue
		}
		if c.Tag != TagIntentionallyChanged {
			t.Errorf("%s: tag %q, want %q", c.Name, c.Tag, TagIntentionallyChanged)
		}
		if c.Record == "" {
			t.Errorf("%s: an intentionally-changed case needs a record", c.Name)
		}
		delete(want, c.Name)
	}
	for name, reason := range want {
		t.Errorf("the seed %s is missing from cases.json (%s)", name, reason)
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
