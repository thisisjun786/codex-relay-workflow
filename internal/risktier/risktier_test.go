package risktier

import (
	"bytes"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

const (
	head = "1111111111111111111111111111111111111111"
	base = "2222222222222222222222222222222222222222"
)

var at = time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)

func input(changes ...Change) Input {
	return Input{Issue: "CRW-1041", Node: "node-a", Head: head, Dev: base, DecidedAt: at, DecidedBy: "classifier", Changes: changes}
}

func edit(p string) Change { return Change{Path: p, Change: "modify"} }

func must(t *testing.T, in Input) Record {
	t.Helper()
	r, err := Decide(in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	return r
}

func TestClassifyPathSets(t *testing.T) {
	low := map[string]string{
		"docs/relay/risk-tier.md":                     "docs_prose",
		"docs/port-cxc/known-defects/CRW-1041.md":     "docs_prose",
		"internal/skill/search/testdata/ranking.json": "testdata_data",
	}
	for p, rule := range low {
		if v := Classify(p); v.Tier != Low || v.Rule != rule {
			t.Errorf("Classify(%q) = %+v, want low/%s", p, v, rule)
		}
	}
	high := []string{
		// the high paths the decided answer names
		"internal/relay/store/store.go", "internal/runtime/install/install.go", "internal/hookswitch/switch.go",
		"internal/policystore/policy_write.go", "internal/relay/store/ledger.go", "internal/runtime/cutover/cutover.go",
		// executable hook and skill instruction text, whatever its extension
		"plugins/crw/skills/crw-run/SKILL.md", "plugins/crw/skills/crw-run/references/dispatch.md",
		"plugins/crw/skills/crw-check/references/schedule-judgment.md", "plugins/crw/wiring/hooks/hooks.json",
		"plugins/crw/skills/crw-qa/agents/a.yaml", "AGENTS.md", "POLICY.md", "SECURITY.md",
		// contract goldens, schemas, fixtures, and the security corpus
		"contract/golden/ack-proof.json", "contract/schema/policy-decision.schema.json", "contract/fixtures/hook/a.json",
		"internal/relay/argparse/specs.json", "internal/skill/testdata/golden/a.json", "internal/runtime/install/testdata/fixtures/a.json",
		"internal/dev/cxccorpus/corpus.go", "internal/pabcd/hook/testdata/oracle-directives.json",
		"internal/pabcd/hook/testdata/shellwrite/case.json", "internal/pyjson/testdata/corpus/a.json",
		// testdata that is not plain data
		"internal/pabcd/hook/testdata/record-directives.mjs", "internal/relay/store/testdata/draft7-verdicts.json.gz",
		// a small fix is not low: an unknown or unlisted path is high
		"internal/skill/issue_ready.go", "cmd/crw/main.go", "scripts/x.sh", "web/src/a.ts", "go.mod", "Makefile", ".github/workflows/ci.yml",
		"README.md", "internal/affordance/affordance_test.go",
		// docs that are not prose, or only look like docs
		"docs/tool.go", "docs/x.MD", "Docs/x.md", "docs/crw-run/dispatch-cases/recorded/a.md", "docs/review/security-notes.md",
		"docs/relay/dag-scheduler.md.sh", "docs",
		// a path that is not a clean path inside the repository is unclassifiable
		"", "docs/../internal/relay/a.go", "/docs/a.md", "docs//a.md", "docs/a.md/", "./docs/a.md", "docs/a\x00.md", "docs\\a.md", "docs/\xff.md",
	}
	for _, p := range high {
		if v := Classify(p); v.Tier != High || v.Rule == "" {
			t.Errorf("Classify(%q) = %+v, want high with a rule", p, v)
		}
	}
}

func TestClassifyNamesTheRuleThatDecides(t *testing.T) {
	for p, rule := range map[string]string{
		"contract/golden/a.json":                             "shared_contract",
		"internal/relay/argparse/specs.json":                 "shared_contract",
		"internal/skill/testdata/golden/a.json":              "shared_contract",
		"plugins/crw/skills/crw-run/SKILL.md":                "executable_instruction",
		"AGENTS.md":                                          "executable_instruction",
		"internal/dev/cxccorpus/corpus.go":                   "not_on_allowlist",
		"internal/runtime/install/testdata/fixtures/a.json":  "high_tree",
		"internal/skill/testdata/fixtures/a.json":            "corpus_or_fixture",
		"internal/relay/store/store.go":                      "high_tree",
		"internal/skill/issue_ready.go":                      "not_on_allowlist",
		"docs/../a":                                          "invalid_path",
		"internal/pabcd/hook/testdata/record-directives.mjs": "not_on_allowlist",
	} {
		if v := Classify(p); v.Tier != High || v.Rule != rule {
			t.Errorf("Classify(%q) = %+v, want high/%s", p, v, rule)
		}
	}
}

// Every shared contract surface the scheduler lists is high here as well.
func TestEverySharedSurfaceIsHigh(t *testing.T) {
	for _, p := range []string{"contract/schema/a.json", "contract/golden/a.json", "contract/fixtures/a.json", "internal/relay/argparse/specs.json",
		"internal/x/testdata/golden/a.json", "contract/schema/premerge-record.schema.json"} {
		if !dagsched.SharedSurface(p) {
			t.Fatalf("%s is not a shared surface any more: update this list", p)
		}
		if v := Classify(p); v.Tier != High {
			t.Errorf("shared surface %s is %+v", p, v)
		}
	}
}

func TestClassifyRegionByKind(t *testing.T) {
	cases := []struct {
		r    Region
		want Tier
	}{
		{Region{Path: "docs", Kind: "tree"}, Low},
		{Region{Path: "docs/relay", Kind: "tree"}, Low},
		{Region{Path: "internal/skill/search/testdata", Kind: "tree"}, Low},
		{Region{Path: "internal/skill/testdata", Kind: "tree"}, High},
		{Region{Path: "internal/role/testdata", Kind: "tree"}, High},
		{Region{Path: "docs/crw-run/dispatch-cases/recorded", Kind: "tree"}, High},
		{Region{Path: "internal/skill/testdata/golden", Kind: "tree"}, High},
		{Region{Path: "internal/relay", Kind: "tree"}, High},
		{Region{Path: "internal", Kind: "tree"}, High},
		{Region{Path: ".", Kind: "tree"}, High},
		{Region{Path: "docs/relay/risk-tier.md", Kind: "file"}, Low},
		{Region{Path: "docs/relay/risk-tier.md", Kind: "symbol", Key: "Heading"}, Low},
		{Region{Path: "internal/skill/issue_ready.go", Kind: "file"}, High},
		{Region{Path: "internal/skill/issue_ready.go", Kind: "symbol", Key: "readyItems"}, High},
	}
	for _, c := range cases {
		if v := ClassifyRegion(c.r); v.Tier != c.want {
			t.Errorf("ClassifyRegion(%+v) = %+v, want %s", c.r, v, c.want)
		}
	}
}

func TestDocsOnlyDiffWithoutDeclarationIsDerivedLow(t *testing.T) {
	r := must(t, input(edit("docs/relay/risk-tier.md"), Change{Path: "docs/port-cxc/known-defects/CRW-1041.md", Change: "add"}))
	if r.DeclaredTier != nil || r.RegionTier != nil || r.ChangedTier != Low || r.DerivedTier != Low || r.Tier != Low || r.ExecutionTier != Low {
		t.Fatalf("%+v", r)
	}
	if r.Schema != "policy-decision/1" || r.Policy != "risk-tier/1" || r.Head != head || r.Dev != base || r.Issue != "CRW-1041" || r.Node != "node-a" {
		t.Fatalf("identity fields: %+v", r)
	}
	if r.DecidedAt != "2026-10-11T09:30:00Z" || r.DecidedBy != "classifier" {
		t.Fatalf("decided: %+v", r)
	}
	if len(r.ChangedPaths) != 2 || r.ChangedPaths[0].Path != "docs/port-cxc/known-defects/CRW-1041.md" || r.ChangedPaths[0].Rule != "docs_prose" {
		t.Fatalf("changed paths are sorted and carry their rule: %+v", r.ChangedPaths)
	}
	if r.Reclassified || len(r.OutsideRegions) != 0 || r.Exception != nil || len(r.SkippedGates) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestAuthorCanOnlyRaise(t *testing.T) {
	in := input(edit("docs/relay/risk-tier.md"))
	in.Declared = High
	r := must(t, in)
	if r.DerivedTier != Low || r.Tier != High || r.ExecutionTier != High || r.DeclaredTier == nil || *r.DeclaredTier != High {
		t.Fatalf("a high declaration raises a low derivation: %+v", r)
	}
	in = input(edit("internal/relay/store/store.go"))
	in.Declared = Low
	r = must(t, in)
	if r.DerivedTier != High || r.Tier != High || r.ExecutionTier != High || *r.DeclaredTier != Low {
		t.Fatalf("a low declaration cannot lower a high derivation: %+v", r)
	}
}

func TestSmallFixInUnlistedPathIsHigh(t *testing.T) {
	in := input(edit("internal/skill/issue_ready.go"))
	in.Declared = Low
	if r := must(t, in); r.Tier != High {
		t.Fatalf("%+v", r)
	}
}

func TestOneHighPathMakesTheWholeDiffHigh(t *testing.T) {
	r := must(t, input(edit("docs/a.md"), edit("docs/b.md"), edit("internal/hookswitch/switch.go")))
	if r.ChangedTier != High || r.Tier != High {
		t.Fatalf("%+v", r)
	}
}

func TestEmptyDiffIsHigh(t *testing.T) {
	in := input()
	in.Regions = []Region{{Path: "docs", Kind: "tree"}}
	r := must(t, in)
	if r.ChangedTier != High || r.Tier != High || len(r.ChangedPaths) != 0 {
		t.Fatalf("no changed path establishes nothing low: %+v", r)
	}
}

func TestDiffThatLeavesDeclaredRegionsIsReclassified(t *testing.T) {
	in := input(edit("docs/a.md"), edit("internal/relay/store/store.go"))
	in.Regions = []Region{{Path: "docs", Kind: "tree"}}
	r := must(t, in)
	if r.RegionTier == nil || *r.RegionTier != Low || r.ChangedTier != High || r.DerivedTier != High || r.Tier != High {
		t.Fatalf("%+v", r)
	}
	if !r.Reclassified || !reflect.DeepEqual(r.OutsideRegions, []string{"internal/relay/store/store.go"}) {
		t.Fatalf("reclassified %v outside %v", r.Reclassified, r.OutsideRegions)
	}
	// An outside path that is itself low leaves the regions without changing the result.
	in = input(edit("docs/a.md"), edit("internal/skill/search/testdata/ranking.json"))
	in.Regions = []Region{{Path: "docs", Kind: "tree"}}
	r = must(t, in)
	if r.Tier != Low || r.Reclassified || !reflect.DeepEqual(r.OutsideRegions, []string{"internal/skill/search/testdata/ranking.json"}) {
		t.Fatalf("%+v", r)
	}
}

func TestDiffInsideDeclaredRegionsIsNotReclassified(t *testing.T) {
	in := input(edit("docs/a.md"), edit("internal/relay/store/store.go"))
	in.Regions = []Region{{Path: "docs", Kind: "tree"}, {Path: "internal/relay/store/store.go", Kind: "symbol", Key: "Open"}}
	r := must(t, in)
	if r.Reclassified || len(r.OutsideRegions) != 0 || r.Tier != High || *r.RegionTier != High {
		t.Fatalf("%+v", r)
	}
}

func TestDeclaredHighRegionRaisesALowDiff(t *testing.T) {
	in := input(edit("docs/a.md"))
	in.Regions = []Region{{Path: "internal/relay", Kind: "tree"}}
	r := must(t, in)
	if *r.RegionTier != High || r.ChangedTier != Low || r.DerivedTier != High || r.Tier != High {
		t.Fatalf("a region only raises: %+v", r)
	}
	if r.Reclassified != false || !reflect.DeepEqual(r.OutsideRegions, []string{"docs/a.md"}) {
		t.Fatalf("%+v", r)
	}
}

func TestRegionCoverage(t *testing.T) {
	in := input(edit("docs/a.md"), edit("docs/dir/b.md"), edit("docsx/c.md"))
	in.Regions = []Region{{Path: "docs", Kind: "tree"}}
	r := must(t, in)
	if !reflect.DeepEqual(r.OutsideRegions, []string{"docsx/c.md"}) {
		t.Fatalf("a tree covers its own subtree and not a name that shares its prefix: %v", r.OutsideRegions)
	}
}

func TestRenameCopyAndTypeChange(t *testing.T) {
	for name, c := range map[string]Change{
		"high old path": {Path: "docs/moved.md", OldPath: "internal/relay/store/store.go", Change: "rename"},
		"high new path": {Path: "internal/relay/store/store.go", OldPath: "docs/old.md", Change: "rename"},
		"copy":          {Path: "docs/copy.md", OldPath: "plugins/crw/skills/crw-run/SKILL.md", Change: "copy"},
		"type change":   {Path: "docs/link.md", Change: "type_change"},
	} {
		if r := must(t, input(c)); r.Tier != High || r.ChangedPaths[0].Tier != High {
			t.Errorf("%s: %+v", name, r)
		}
	}
	r := must(t, input(Change{Path: "docs/new.md", OldPath: "docs/old.md", Change: "rename"}))
	if r.Tier != Low || r.ChangedPaths[0].OldPath != "docs/old.md" {
		t.Fatalf("%+v", r)
	}
	in := input(Change{Path: "docs/new.md", OldPath: "docs/old.md", Change: "rename"})
	in.Regions = []Region{{Path: "docs/new.md", Kind: "file"}}
	if r := must(t, in); !reflect.DeepEqual(r.OutsideRegions, []string{"docs/old.md"}) {
		t.Fatalf("a rename leaves the regions through its old path: %v", r.OutsideRegions)
	}
}

func TestDecideRefusesMalformedInput(t *testing.T) {
	bad := map[string]func(*Input){
		"no issue":            func(i *Input) { i.Issue = "" },
		"no node":             func(i *Input) { i.Node = " " },
		"short head":          func(i *Input) { i.Head = "abc" },
		"upper head":          func(i *Input) { i.Head = strings.Repeat("A", 40) },
		"bad dev":             func(i *Input) { i.Dev = "zz" },
		"no time":             func(i *Input) { i.DecidedAt = time.Time{} },
		"no decider":          func(i *Input) { i.DecidedBy = "" },
		"unknown tier":        func(i *Input) { i.Declared = "medium" },
		"unknown change":      func(i *Input) { i.Changes[0].Change = "touch" },
		"rename w/o old path": func(i *Input) { i.Changes[0] = Change{Path: "docs/a.md", Change: "rename"} },
		"old path on modify":  func(i *Input) { i.Changes[0].OldPath = "docs/b.md" },
		"empty change path":   func(i *Input) { i.Changes[0].Path = "" },
		"unknown region kind": func(i *Input) { i.Regions = []Region{{Path: "docs", Kind: "glob"}} },
		"unknown region edit": func(i *Input) { i.Regions = []Region{{Path: "docs", Kind: "tree", Change: "touch"}} },
		"symbol w/o key":      func(i *Input) { i.Regions = []Region{{Path: "docs/a.md", Kind: "symbol"}} },
		"key on file":         func(i *Input) { i.Regions = []Region{{Path: "docs/a.md", Kind: "file", Key: "x"}} },
		"empty region path":   func(i *Input) { i.Regions = []Region{{Path: "", Kind: "tree"}} },
	}
	for name, mutate := range bad {
		in := input(edit("docs/a.md"))
		mutate(&in)
		if _, err := Decide(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDecisionIsIndependentOfInputOrder(t *testing.T) {
	a := input(edit("docs/b.md"), edit("docs/a.md"), edit("internal/relay/store/store.go"))
	a.Regions = []Region{{Path: "internal/relay", Kind: "tree"}, {Path: "docs", Kind: "tree"}}
	b := input(edit("internal/relay/store/store.go"), edit("docs/a.md"), edit("docs/b.md"))
	b.Regions = []Region{{Path: "docs", Kind: "tree"}, {Path: "internal/relay", Kind: "tree"}}
	x, _ := json.Marshal(must(t, a))
	y, _ := json.Marshal(must(t, b))
	if !bytes.Equal(x, y) {
		t.Fatalf("%s\n%s", x, y)
	}
}

func exception(tier Tier) *Exception {
	return &Exception{By: "management-session", Reason: "the reference page only restates a table", At: at.Add(time.Minute), ExecutionTier: tier}
}

func TestExceptionChangesTheExecutionTierAndNotTheTier(t *testing.T) {
	in := input(edit("plugins/crw/skills/crw-run/references/layout.md"))
	in.Exception = exception(Low)
	r := must(t, in)
	if r.Tier != High || r.DerivedTier != High || r.ExecutionTier != Low {
		t.Fatalf("the exception lowers the tier the gates run and leaves the classification: %+v", r)
	}
	if r.Exception == nil || r.Exception.Reason == "" || r.Exception.By == "" || r.Exception.At != "2026-10-11T09:31:00Z" || r.Exception.ExecutionTier != Low {
		t.Fatalf("%+v", r.Exception)
	}
	in = input(edit("docs/a.md"))
	in.Exception = exception(High)
	if r = must(t, in); r.Tier != Low || r.ExecutionTier != High {
		t.Fatalf("an exception can also hold a low change at high: %+v", r)
	}
}

func TestExceptionNeedsAReasonAWhoAndAChange(t *testing.T) {
	for name, mutate := range map[string]func(*Exception){
		"no reason": func(e *Exception) { e.Reason = "  " },
		"no by":     func(e *Exception) { e.By = "" },
		"no time":   func(e *Exception) { e.At = time.Time{} },
		"no tier":   func(e *Exception) { e.ExecutionTier = "" },
		"same tier": func(e *Exception) { e.ExecutionTier = High },
		"unknown":   func(e *Exception) { e.ExecutionTier = "medium" },
	} {
		in := input(edit("plugins/crw/skills/crw-run/references/layout.md"))
		in.Exception = exception(Low)
		mutate(in.Exception)
		if _, err := Decide(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestSkippedGatesAreRecordedOnlyWhereTheyRan(t *testing.T) {
	r := must(t, input(edit("docs/a.md")))
	data, _ := json.Marshal(r)
	if !strings.Contains(string(data), `"skipped_gates":[]`) || !strings.Contains(string(data), `"changed_paths":[`) || !strings.Contains(string(data), `"declared_regions":[]`) || !strings.Contains(string(data), `"outside_regions":[]`) {
		t.Fatalf("empty lists are lists, not null: %s", data)
	}
	skip := []SkippedGate{{Gate: "independent_review", Reason: "the batch review covers it"}}
	in := input(edit("docs/a.md"))
	in.SkippedGates = skip
	if r = must(t, in); len(r.SkippedGates) != 1 || r.SkippedGates[0].Gate != "independent_review" {
		t.Fatalf("%+v", r)
	}
	for name, mutate := range map[string]func(*Input){
		"high execution tier": func(i *Input) { i.Changes = []Change{edit("internal/relay/store/store.go")} },
		"premerge record":     func(i *Input) { i.SkippedGates = []SkippedGate{{Gate: "premerge_record", Reason: "skipped"}} },
		"dag accept":          func(i *Input) { i.SkippedGates = []SkippedGate{{Gate: "dag_accept", Reason: "skipped"}} },
		"no reason":           func(i *Input) { i.SkippedGates = []SkippedGate{{Gate: "independent_review", Reason: " "}} },
		"not a name":          func(i *Input) { i.SkippedGates = []SkippedGate{{Gate: "Independent Review", Reason: "x"}} },
		"the same gate twice": func(i *Input) { i.SkippedGates = append(skip, skip[0]) },
	} {
		in := input(edit("docs/a.md"))
		in.SkippedGates = skip
		mutate(&in)
		if _, err := Decide(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A high change held at low by a recorded exception may skip a gate, with the exception beside it.
	in = input(edit("plugins/crw/skills/crw-run/references/layout.md"))
	in.Exception = exception(Low)
	in.SkippedGates = skip
	if r = must(t, in); r.ExecutionTier != Low || len(r.SkippedGates) != 1 {
		t.Fatalf("%+v", r)
	}
}

func encode(t *testing.T, r Record) []byte {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDecodeReadsWhatDecideWrote(t *testing.T) {
	in := input(edit("docs/a.md"), edit("internal/relay/store/store.go"))
	in.Declared = Low
	in.Regions = []Region{{Path: "docs", Kind: "tree"}}
	in.Exception = exception(Low)
	in.SkippedGates = []SkippedGate{{Gate: "independent_review", Reason: "covered by the batch review"}}
	want := must(t, in)
	got, err := Decode(encode(t, want))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v\n%+v", got, want)
	}
}

func TestDecodeRefusesATamperedRecord(t *testing.T) {
	in := input(edit("docs/a.md"), edit("internal/relay/store/store.go"))
	in.Regions = []Region{{Path: "docs", Kind: "tree"}}
	good := encode(t, must(t, in))
	if _, err := Decode(good); err != nil {
		t.Fatal(err)
	}
	patch := func(mutate func(map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		out, _ := json.Marshal(m)
		return out
	}
	path := func(m map[string]any, i int) map[string]any { return m["changed_paths"].([]any)[i].(map[string]any) }
	cases := map[string]func(map[string]any){
		"tier lowered":                 func(m map[string]any) { m["tier"] = "low"; m["execution_tier"] = "low" },
		"derived tier lowered":         func(m map[string]any) { m["derived_tier"] = "low" },
		"changed tier lowered":         func(m map[string]any) { m["changed_tier"] = "low" },
		"path tier lowered":            func(m map[string]any) { path(m, 1)["tier"] = "low" },
		"path rule forged":             func(m map[string]any) { path(m, 1)["rule"] = "docs_prose" },
		"execution tier w/o exception": func(m map[string]any) { m["execution_tier"] = "low" },
		"outside regions emptied":      func(m map[string]any) { m["outside_regions"] = []any{} },
		"reclassified cleared":         func(m map[string]any) { m["reclassified"] = false },
		"a high path dropped":          func(m map[string]any) { m["changed_paths"] = m["changed_paths"].([]any)[:1] },
		"skipped gate under high": func(m map[string]any) {
			m["skipped_gates"] = []any{map[string]any{"gate": "independent_review", "reason": "x"}}
		},
		"unknown member":     func(m map[string]any) { m["score"] = 1 },
		"wrong schema":       func(m map[string]any) { m["schema"] = "policy-decision/2" },
		"wrong policy":       func(m map[string]any) { m["policy"] = "risk-tier/2" },
		"null list":          func(m map[string]any) { m["skipped_gates"] = nil },
		"time not canonical": func(m map[string]any) { m["decided_at"] = "2026-10-11T18:30:00+09:00" },
		"missing member":     func(m map[string]any) { delete(m, "execution_tier") },
	}
	for name, mutate := range cases {
		if _, err := Decode(patch(mutate)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Decode(append(append([]byte{}, good...), []byte(" {}")...)); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := Decode([]byte(`[]`)); err == nil {
		t.Error("a list accepted")
	}
}

func schemaFile(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "contract", "schema", "policy-decision.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(&jsonschema.ResolveOptions{BaseURI: "https://crw.invalid/"})
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func validates(t *testing.T, schema *jsonschema.Resolved, data []byte) error {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return schema.Validate(v)
}

func TestSchemaAcceptsEveryRecordDecideWrites(t *testing.T) {
	schema := schemaFile(t)
	withEverything := input(edit("docs/a.md"), Change{Path: "docs/n.md", OldPath: "docs/o.md", Change: "rename"})
	withEverything.Declared = Low
	withEverything.Regions = []Region{{Path: "docs", Kind: "tree"}, {Path: "docs/a.md", Kind: "symbol", Key: "S", Change: "edit"}}
	withEverything.Exception = exception(High)
	for name, in := range map[string]Input{
		"low":        input(edit("docs/a.md")),
		"high":       input(edit("internal/relay/store/store.go")),
		"empty diff": input(),
		"everything": withEverything,
	} {
		if err := validates(t, schema, encode(t, must(t, in))); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSchemaRefusesWhatTheRecordCannotSay(t *testing.T) {
	schema := schemaFile(t)
	good := encode(t, must(t, input(edit("docs/a.md"))))
	if err := validates(t, schema, good); err != nil {
		t.Fatal(err)
	}
	patch := func(mutate func(map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(good, &m)
		mutate(m)
		out, _ := json.Marshal(m)
		return out
	}
	cases := map[string]func(map[string]any){
		"not this schema":   func(m map[string]any) { m["schema"] = "premerge-record/1" },
		"a score":           func(m map[string]any) { m["score"] = 0.9 },
		"a grader":          func(m map[string]any) { m["grader"] = map[string]any{"model": "x"} },
		"medium tier":       func(m map[string]any) { m["tier"] = "medium" },
		"no execution tier": func(m map[string]any) { delete(m, "execution_tier") },
		"short head":        func(m map[string]any) { m["head"] = "abc" },
		"empty issue":       func(m map[string]any) { m["issue"] = "" },
		"time zone offset":  func(m map[string]any) { m["decided_at"] = "2026-10-11T18:30:00+09:00" },
		"no skipped gates":  func(m map[string]any) { delete(m, "skipped_gates") },
		"gate w/o reason":   func(m map[string]any) { m["skipped_gates"] = []any{map[string]any{"gate": "independent_review"}} },
		"gate with empty reason": func(m map[string]any) {
			m["skipped_gates"] = []any{map[string]any{"gate": "independent_review", "reason": ""}}
		},
		"gate not a name": func(m map[string]any) { m["skipped_gates"] = []any{map[string]any{"gate": "Review!", "reason": "x"}} },
		"exception w/o reason": func(m map[string]any) {
			m["exception"] = map[string]any{"by": "m", "at": "2026-10-11T09:30:00Z", "execution_tier": "low"}
		},
		"exception empty reason": func(m map[string]any) {
			m["exception"] = map[string]any{"by": "m", "reason": "", "at": "2026-10-11T09:30:00Z", "execution_tier": "low"}
		},
		"path w/o rule":   func(m map[string]any) { delete(m["changed_paths"].([]any)[0].(map[string]any), "rule") },
		"path bad change": func(m map[string]any) { m["changed_paths"].([]any)[0].(map[string]any)["change"] = "touch" },
		"null list":       func(m map[string]any) { m["changed_paths"] = nil },
		"region bad kind": func(m map[string]any) {
			m["declared_regions"] = []any{map[string]any{"path": "docs", "kind": "glob", "change": "edit", "tier": "low", "rule": "docs_prose_tree"}}
		},
	}
	for name, mutate := range cases {
		if validates(t, schema, patch(mutate)) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A high tree stays high whatever low rule its file would otherwise match: plain testdata under the relay,
// runtime and policy trees is not low (CRW-1041 verification round 1).
func TestHighTreesBeatTheLowAllowlist(t *testing.T) {
	files := []string{
		"internal/relay/registry/testdata/gate_cases.json", "internal/runtime/record/testdata/host-record-v1.json",
		"internal/policystore/testdata/a.json", "internal/hookswitch/testdata/notes.txt", "internal/relay/store/testdata/a.md",
	}
	for _, p := range files {
		if v := Classify(p); v.Tier != High || v.Rule != RuleHighTree {
			t.Errorf("Classify(%q) = %+v, want high/high_tree", p, v)
		}
		in := input(Change{Path: p, Change: "modify"})
		if r := must(t, in); r.Tier != High || r.ChangedTier != High {
			t.Errorf("a diff of %s is tier %s (changed %s)", p, r.Tier, r.ChangedTier)
		}
	}
	for _, p := range []string{"internal/relay/registry/testdata", "internal/runtime/record/testdata", "internal/policystore/testdata/sub"} {
		if v := ClassifyRegion(Region{Path: p, Kind: "tree"}); v.Tier != High || v.Rule != RuleHighTree {
			t.Errorf("tree region %q = %+v, want high/high_tree", p, v)
		}
		if v := ClassifyRegion(Region{Path: p + "/a.json", Kind: "file"}); v.Tier != High {
			t.Errorf("file region %q = %+v, want high", p+"/a.json", v)
		}
	}
	// a listed testdata tree elsewhere stays low, an unlisted one is high
	if v := Classify("internal/skill/search/testdata/ranking.json"); v.Tier != Low {
		t.Errorf("listed testdata outside the high trees = %+v", v)
	}
	if v := Classify("internal/skill/testdata/cases.json"); v.Tier != High || v.Rule != RuleTestdataUnlisted {
		t.Errorf("unlisted testdata = %+v", v)
	}
}

// Agent instruction files protect themselves in every directory, not only at the root.
func TestNestedInstructionFilesAreHigh(t *testing.T) {
	for _, p := range []string{"docs/CLAUDE.md", "docs/relay/AGENTS.md", "internal/skill/testdata/CLAUDE.md", "internal/skill/testdata/sub/AGENTS.md",
		"docs/agents.md", "docs/claude.md", "docs/Claude.MD"} {
		want := RuleExecutableText
		if v := Classify(p); v.Tier != High || v.Rule != want {
			t.Errorf("Classify(%q) = %+v, want high/%s", p, v, want)
		}
		if r := must(t, input(Change{Path: p, Change: "modify"})); r.Tier != High {
			t.Errorf("a diff of %s is tier %s", p, r.Tier)
		}
		if v := ClassifyRegion(Region{Path: p, Kind: "file"}); v.Tier != High {
			t.Errorf("file region %s = %+v", p, v)
		}
	}
	// a name that only contains the word is ordinary prose
	if v := Classify("docs/relay/agents-and-claude.md"); v.Tier != Low {
		t.Errorf("docs/relay/agents-and-claude.md = %+v", v)
	}
	// a rename out of an instruction file is judged on the old path too
	r := must(t, input(Change{Path: "docs/notes.md", OldPath: "docs/CLAUDE.md", Change: "rename"}))
	if r.Tier != High {
		t.Errorf("rename from docs/CLAUDE.md is tier %s", r.Tier)
	}
}

// A JSON null is a schema violation everywhere but declared_tier and region_tier; Decode must not read it as a zero value.
func TestDecodeRefusesNullsTheSchemaForbids(t *testing.T) {
	schema := schemaFile(t)
	in := input(edit("docs/a.md"), Change{Path: "docs/n.md", OldPath: "docs/o.md", Change: "rename"})
	in.Regions = []Region{{Path: "docs/a.md", Kind: "symbol", Key: "S"}}
	in.Declared = Low
	in.Exception = exception(High)
	good := encode(t, must(t, in))
	if _, err := Decode(good); err != nil {
		t.Fatal(err)
	}
	patch := func(mutate func(map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(good, &m)
		mutate(m)
		out, _ := json.Marshal(m)
		return out
	}
	item := func(m map[string]any, key string, i int) map[string]any { return m[key].([]any)[i].(map[string]any) }
	cases := map[string]func(map[string]any){
		"reclassified null":  func(m map[string]any) { m["reclassified"] = nil },
		"exception null":     func(m map[string]any) { m["exception"] = nil },
		"old_path null":      func(m map[string]any) { item(m, "changed_paths", 0)["old_path"] = nil },
		"region key null":    func(m map[string]any) { item(m, "declared_regions", 0)["key"] = nil },
		"tier null":          func(m map[string]any) { m["tier"] = nil },
		"issue null":         func(m map[string]any) { m["issue"] = nil },
		"exception by null":  func(m map[string]any) { m["exception"].(map[string]any)["by"] = nil },
		"outside list item":  func(m map[string]any) { m["outside_regions"] = []any{nil} },
		"changed_tier null":  func(m map[string]any) { m["changed_tier"] = nil },
		"change path null":   func(m map[string]any) { item(m, "changed_paths", 0)["path"] = nil },
		"skipped gates null": func(m map[string]any) { m["skipped_gates"] = nil },
	}
	for name, mutate := range cases {
		data := patch(mutate)
		if validates(t, schema, data) == nil {
			t.Errorf("%s: the schema accepted it", name)
		}
		if _, err := Decode(data); err == nil {
			t.Errorf("%s: Decode accepted it", name)
		}
	}
	// the two nullable members stay accepted as null
	var m map[string]any
	_ = json.Unmarshal(encode(t, must(t, input(edit("docs/a.md")))), &m)
	if m["declared_tier"] != nil || m["region_tier"] != nil {
		t.Fatal("the plain low record is expected to carry null declared_tier and region_tier")
	}
	if _, err := Decode(encode(t, must(t, input(edit("docs/a.md"))))); err != nil {
		t.Errorf("null declared_tier and region_tier refused: %v", err)
	}
}

// securityCorpusFiles are recorded answers that tests replay against the security readers and gates
// (internal/dev/cxcfuzz cases_test.go, internal/pabcd/gate oracle_test.go). Their names carry none of the
// reserved words, so only an explicit list keeps them high (CRW-1041 verification round 2).
var securityCorpusFiles = []string{
	"internal/dev/cxcfuzz/testdata/worktreedel/cases.json",
	"internal/dev/cxcfuzz/testdata/memorygate/cases.json",
	"internal/dev/cxcfuzz/testdata/shellwrite/cases.json",
	"internal/dev/cxcfuzz/testdata/spawn/cases.json",
	"internal/dev/cxcfuzz/testdata/state/cases.json",
	"internal/pabcd/gate/testdata/cases.json",
}

func TestSecurityCorpusIsHigh(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	for _, p := range securityCorpusFiles {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			t.Fatalf("the corpus file %s is gone: update this list and the classifier's: %v", p, err)
		}
		if v := Classify(p); v.Tier != High || v.Rule != RuleSecurityCorpus {
			t.Errorf("Classify(%q) = %+v, want high/%s", p, v, RuleSecurityCorpus)
		}
		for _, r := range []Region{{Path: p, Kind: "file"}, {Path: p, Kind: "symbol", Key: "case"}, {Path: path.Dir(p), Kind: "tree"}} {
			if v := ClassifyRegion(r); v.Tier != High {
				t.Errorf("region %+v = %+v, want high", r, v)
			}
		}
		in := input(edit(p))
		rec := must(t, in)
		if rec.ChangedTier != High || rec.Tier != High || rec.ExecutionTier != High {
			t.Errorf("a diff of only %s: changed %s, tier %s, execution %s", p, rec.ChangedTier, rec.Tier, rec.ExecutionTier)
		}
		in.Regions = []Region{{Path: p, Kind: "file"}}
		in.Declared = Low
		if rec := must(t, in); rec.RegionTier == nil || *rec.RegionTier != High || rec.Tier != High || rec.ExecutionTier != High {
			t.Errorf("a declared low diff of only %s inside its own region: %+v", p, rec)
		}
	}
	for _, tree := range []string{"internal/dev/cxcfuzz/testdata", "internal/pabcd/gate/testdata", "internal/dev/cxcfuzz/testdata/new-target"} {
		if v := ClassifyRegion(Region{Path: tree, Kind: "tree"}); v.Tier != High || v.Rule != RuleSecurityCorpus {
			t.Errorf("tree region %s = %+v, want high/%s", tree, v, RuleSecurityCorpus)
		}
	}
	// a new target's corpus is high before anyone lists it
	if v := Classify("internal/dev/cxcfuzz/testdata/new-target/cases.json"); v.Tier != High {
		t.Errorf("a new cxcfuzz corpus = %+v", v)
	}
}

// Decode accepts only the exact spelling the schema gives: encoding/json matches member names without
// regard to case and omitempty erases an empty optional member, so neither may decide what a record says.
func TestDecodeRefusesWhatTheSchemaRefuses(t *testing.T) {
	schema := schemaFile(t)
	in := input(edit("docs/a.md"), Change{Path: "docs/n.md", OldPath: "docs/o.md", Change: "rename"})
	in.Regions = []Region{{Path: "docs", Kind: "tree"}, {Path: "docs/a.md", Kind: "symbol", Key: "S"}}
	in.Exception = exception(High)
	good := encode(t, must(t, in))
	if _, err := Decode(good); err != nil {
		t.Fatal(err)
	}
	patch := func(mutate func(map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(good, &m)
		mutate(m)
		out, _ := json.Marshal(m)
		return out
	}
	item := func(m map[string]any, key string, i int) map[string]any { return m[key].([]any)[i].(map[string]any) }
	modify := func(m map[string]any) map[string]any {
		for _, e := range m["changed_paths"].([]any) {
			if e.(map[string]any)["change"] == "modify" {
				return e.(map[string]any)
			}
		}
		t.Fatal("no modify entry")
		return nil
	}
	cases := map[string]func(map[string]any){
		"upper-case duplicate member": func(m map[string]any) { m["TIER"] = "low" },
		"upper-case member alone":     func(m map[string]any) { m["TIER"] = m["tier"]; delete(m, "tier") },
		"upper-case path":             func(m map[string]any) { e := item(m, "changed_paths", 0); e["PATH"] = e["path"]; delete(e, "path") },
		"empty old_path":              func(m map[string]any) { modify(m)["old_path"] = "" },
		"empty region key":            func(m map[string]any) { item(m, "declared_regions", 0)["key"] = "" },
		"mixed-case exception member": func(m map[string]any) {
			e := m["exception"].(map[string]any)
			e["Reason"] = e["reason"]
			delete(e, "reason")
		},
	}
	for name, mutate := range cases {
		data := patch(mutate)
		if validates(t, schema, data) == nil {
			t.Errorf("%s: the schema accepted it", name)
		}
		if _, err := Decode(data); err == nil {
			t.Errorf("%s: Decode accepted it", name)
		}
	}
}
