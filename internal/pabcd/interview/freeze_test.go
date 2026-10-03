package interview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// testdata/oracle-freeze.json holds what the CXC v0.2.40 oracle answered for freeze.ts and freeze-cli.ts, recorded once under
// Node 24 (ICU 78.3, en-US) with testdata/record-freeze-oracle.mjs; no Node runs here. The slices are the oracle's inputs and
// answers; a scenario is a whole run of the command (output, manifest bytes, tree) over a tree the test rebuilds.
type freezeRecorded struct {
	AsciiOrder string
	Matrix     struct {
		Strings []string
		Rows    []string
	}
	Sets []struct {
		Files    []PlanFileHash
		Sorted   []string
		PlanHash string
		Hash     string
	}
	Slugs []struct{ Objective, Slug string }
	Stale []struct {
		Name     string
		Manifest FreezeManifest
		Current  []PlanFileHash
		Result   StaleCheckResult
	}
	Argv []struct {
		Argv      []string
		Cwd       *string
		SessionID string
		DryRun    bool
		Help      bool
	}
	Scenarios map[string]struct {
		Files []freezeEntry
		Steps []freezeStep
	}
}

func freezeOracle(t *testing.T) freezeRecorded {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "oracle-freeze.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o freezeRecorded
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if len(o.Slugs) < 15 || len(o.Stale) < 8 || len(o.Argv) < 15 || len(o.Scenarios) < 25 || len(o.Sets) < 4 {
		t.Fatalf("recorded cases: %d slugs, %d stale, %d argv, %d scenarios, %d sets", len(o.Slugs), len(o.Stale), len(o.Argv), len(o.Scenarios), len(o.Sets))
	}
	return o
}

func freezePaths(files []PlanFileHash) (paths []string) {
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return paths
}

// The plan files hash in the order of JavaScript's localeCompare: every ASCII character, a matrix of path-like strings and sorted
// path sets are compared with what Node answered.
func TestFreezeCollationMatchesTheRecordedOracle(t *testing.T) {
	o := freezeOracle(t)
	var chars []string
	for c := 1; c < 128; c++ {
		chars = append(chars, string(rune(c)))
	}
	slices.SortStableFunc(chars, localeCompare)
	if got := strings.Join(chars, ""); got != o.AsciiOrder {
		t.Errorf("ASCII order\n got %q\nwant %q", got, o.AsciiOrder)
	}
	sign := func(n int) byte { return "=<>"[(n+3)%3] }
	for i, a := range o.Matrix.Strings {
		for j, b := range o.Matrix.Strings {
			if got := sign(localeCompare(a, b)); got != o.Matrix.Rows[i][j] {
				t.Errorf("localeCompare(%q, %q) = %q, oracle %q", a, b, got, o.Matrix.Rows[i][j])
			}
		}
	}
	for i, s := range o.Sets {
		given := slices.Clone(s.Files)
		m := BuildFreezeManifest(BuildManifestInput{Objective: "o", PlanFiles: s.Files, Now: func() string { return "T" }})
		if got := freezePaths(m.PlanFiles); !slices.Equal(got, s.Sorted) || m.PlanHash != s.PlanHash || ComputePlanHash(s.Files) != s.Hash {
			t.Errorf("set %d: order %q hash %s (computePlanHash %s), oracle %q %s", i, got, m.PlanHash, ComputePlanHash(s.Files), s.Sorted, s.PlanHash)
		}
		if !slices.Equal(given, s.Files) {
			t.Errorf("set %d: the input list was reordered", i)
		}
	}
}

func freezeBundle() EvidenceBundle {
	return EvidenceBundle{OpenAssumptions: []string{"- Assume X (low)"}, Contradictions: []Contradiction{}, AcceptanceCriteria: []string{"AC1"}}
}

// freeze.test.ts "manifest uses exact pinned field names + planHash over per-file sha256".
func TestFreezeManifestUsesThePinnedFieldNames(t *testing.T) {
	files := []PlanFileHash{{"b.md", Sha256("B")}, {"a.md", Sha256("A")}}
	m := BuildFreezeManifest(BuildManifestInput{Objective: "Build the Thing!", PlanFiles: files, EvidenceBundle: freezeBundle(), Now: func() string { return "2026-06-30T00:00:00Z" }})
	raw, _ := json.Marshal(m)
	want := "{\"frozenAt\":\"2026-06-30T00:00:00Z\",\"planFiles\":[{\"path\":\"a.md\",\"sha256\":\"" + Sha256("A") + "\"},{\"path\":\"b.md\",\"sha256\":\"" + Sha256("B") +
		"\"}],\"planHash\":\"" + ComputePlanHash(files) + "\",\"objective\":\"Build the Thing!\",\"slug\":\"build-the-thing\",\"evidenceBundle\":{\"dimensions\":null," +
		"\"openAssumptions\":[\"- Assume X (low)\"],\"contradictions\":[],\"acceptanceCriteria\":[\"AC1\"],\"researchReportRef\":null}}"
	if string(raw) != want {
		t.Errorf("manifest\n got %s\nwant %s", raw, want)
	}
	if Sha256("A") != "559aead08264d5795d3909718cdd05abd49572e84fe55590eef31a88a08fdffd" {
		t.Error("sha256 is not the hex digest of the UTF-8 text")
	}
	if now := BuildFreezeManifest(BuildManifestInput{}).FrozenAt; !regexp.MustCompile("^\\d{4}-\\d\\d-\\d\\dT\\d\\d:\\d\\d:\\d\\d\\.\\d{3}Z$").MatchString(now) {
		t.Errorf("default frozenAt %q is not toISOString()", now)
	}
}

// freeze.test.ts "deriveSlug lowercases, dashes non-alnum, trims, caps 48", then the recorded answers, among them the characters
// whose JavaScript lowercase differs from Go's (U+0130 is an i and a combining dot).
func TestFreezeDeriveSlug(t *testing.T) {
	if DeriveSlug("  Hello, World!!  ") != "hello-world" || len(DeriveSlug(strings.Repeat("a", 60))) != 48 || DeriveSlug("***") != "interview" {
		t.Error("the three pinned slugs differ")
	}
	for _, c := range freezeOracle(t).Slugs {
		if got := DeriveSlug(c.Objective); got != c.Slug {
			t.Errorf("DeriveSlug(%q) = %q, oracle %q", c.Objective, got, c.Slug)
		}
	}
}

// freeze.test.ts "OPEN ASSUMPTIONS is hash-covered via plan file content".
func TestFreezeOpenAssumptionsAreHashCovered(t *testing.T) {
	v1 := []PlanFileHash{{"plan.md", Sha256("# Plan\n## OPEN ASSUMPTIONS\n- A1")}}
	v2 := []PlanFileHash{{"plan.md", Sha256("# Plan\n## OPEN ASSUMPTIONS\n- A1\n- A2")}}
	m := BuildFreezeManifest(BuildManifestInput{Objective: "o", PlanFiles: v1, EvidenceBundle: freezeBundle()})
	if CheckStale(m, v1).Stale || !CheckStale(m, v2).Stale {
		t.Error("a changed assumption must change the plan hash and make the manifest stale")
	}
}

// freeze.test.ts "checkStale catches changed/missing/new files AND planHash mismatch", then the recorded verdicts and reasons,
// with changedFiles in UTF-16 order.
func TestFreezeCheckStale(t *testing.T) {
	files := []PlanFileHash{{"plan.md", Sha256("v1")}}
	m := BuildFreezeManifest(BuildManifestInput{Objective: "o", PlanFiles: files})
	added := CheckStale(m, []PlanFileHash{{"plan.md", Sha256("v1")}, {"x.md", Sha256("e")}})
	if CheckStale(m, files).Stale || !CheckStale(m, []PlanFileHash{{"plan.md", Sha256("v2")}}).Stale || !added.Stale || !slices.Contains(added.ChangedFiles, "x.md") {
		t.Error("changed, missing and new files must be stale, an equal plan fresh")
	}
	for _, c := range freezeOracle(t).Stale {
		got := CheckStale(c.Manifest, c.Current)
		if got.Stale != c.Result.Stale || got.Reason != c.Result.Reason || !slices.Equal(got.ChangedFiles, c.Result.ChangedFiles) || got.ChangedFiles == nil {
			t.Errorf("%s:\n got %+v\nwant %+v", c.Name, got, c.Result)
		}
	}
}

// freeze.test.ts "activation directive instructs get_goal -> objective-only create_goal + verify + re-freeze", pinned as a golden:
// the constant is the oracle's text recorded in contract/schema/cxc/injected-text.json, under CRW's names.
func TestFreezeGoalActivationDirectiveIsTheRecordedConstant(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	sub, err := cxccorpus.LoadSubstitution(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "contract", "schema", "cxc", "injected-text.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Modules map[string]map[string]json.RawMessage
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	oracle := func(name string) (s string) {
		if err := json.Unmarshal(doc.Modules["components/pabcd-state/dist/freeze.js"][name], &s); err != nil || s == "" {
			t.Fatalf("%s is not recorded: %v", name, err)
		}
		return s
	}
	if got, want := GoalActivationDirective, sub.Expected(oracle("GOAL_ACTIVATION_DIRECTIVE")); got != want {
		t.Errorf("directive\n got %q\nwant %q", got, want)
	}
	if PlanSubdir != oracle("PLAN_SUBDIR") || FreezeManifestDir != oracle("FREEZE_MANIFEST_DIR") || FreezeManifestFile != oracle("FREEZE_MANIFEST_FILE") {
		t.Error("a path constant differs from the oracle's")
	}
	for _, pattern := range []string{"get_goal", "(?i)objective ONLY", "(?i)no token_budget", "(?i)Verify a goal row", "(?i)recompute planHash"} {
		if !regexp.MustCompile(pattern).MatchString(GoalActivationDirective) {
			t.Errorf("the directive lacks %s", pattern)
		}
	}
}
