//go:build dev

package ci

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The spawn policy page of the shared library (crw-canon/references/spawn-policy.md) says which agent calls a flow run makes by itself,
// which signals add a call, which work the run does inline, what is recorded, and the two kinds of refusal no option overrides. The ids
// of its rules are the constants of internal/flow/ops/rules.go, the one source of the strings; these checks read that file and the page.

const (
	spawnPolicyRef  = canonRefDir + "spawn-policy.md"
	spawnRulesGo    = "internal/flow/ops/rules.go"
	spawnRuleIDForm = `SP-[A-Z]+-[0-9]+`
)

var (
	spawnRuleID     = regexp.MustCompile(`\b` + spawnRuleIDForm + `\b`)
	spawnRuleRowKey = regexp.MustCompile("^`(" + spawnRuleIDForm + ")`$")
)

// engineRuleIDs is the value of every string constant of rules.go that is a rule id, read with go/parser.
func engineRuleIDs(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot(), filepath.FromSlash(spawnRulesGo)), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, expr := range value.Values {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if text, err := strconv.Unquote(lit.Value); err == nil && regexp.MustCompile(`^`+spawnRuleIDForm+`$`).MatchString(text) {
					ids = append(ids, text)
				}
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// engineIntConst is the value of an integer constant of rules.go.
func engineIntConst(t *testing.T, name string) int {
	t.Helper()
	data := readRepoText(t, spawnRulesGo)
	m := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*(\d+)`).FindStringSubmatch(data)
	if m == nil {
		t.Fatalf("%s has no integer constant %s", spawnRulesGo, name)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// spawnRuleRows is the rule ids that open a row of a table of the page (the first cell is the id in code font), in page order.
func spawnRuleRows(text string) []string {
	var ids []string
	for _, line := range lines(text) {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(trimmed, "|"), "|")
		if m := spawnRuleRowKey.FindStringSubmatch(strings.TrimSpace(cells[0])); m != nil {
			ids = append(ids, m[1])
		}
	}
	return ids
}

// spawnPolicyProblems judges one text of the page against the engine's rule ids.
func spawnPolicyProblems(text string, engine []string) []string {
	var problems []string
	rows := spawnRuleRows(text)
	sorted := slices.Clone(rows)
	slices.Sort(sorted)
	for _, id := range engine {
		if !slices.Contains(rows, id) {
			problems = append(problems, "no table row for "+id)
		}
	}
	for i, id := range sorted {
		if i > 0 && sorted[i-1] == id {
			problems = append(problems, "two table rows for "+id)
		}
		if !slices.Contains(engine, id) {
			problems = append(problems, "the table row "+id+" is no rule id of internal/flow/ops/rules.go")
		}
	}
	for _, id := range spawnRuleID.FindAllString(text, -1) {
		if !slices.Contains(engine, id) {
			problems = append(problems, "the text names "+id+", which is no rule id of internal/flow/ops/rules.go")
		}
	}
	return slices.Compact(problems)
}

func TestSpawnPolicyRulesMatchEngine(t *testing.T) {
	engine := engineRuleIDs(t)
	if len(engine) < 20 {
		t.Fatalf("rules.go yields %d rule ids: %v", len(engine), engine)
	}
	text := readRepoText(t, spawnPolicyRef)

	t.Run("the rule ids of the page are the constants of rules.go", func(t *testing.T) {
		if problems := spawnPolicyProblems(text, engine); len(problems) > 0 {
			t.Errorf("%s does not match %s:\n  %s", spawnPolicyRef, spawnRulesGo, strings.Join(problems, "\n  "))
		}
	})

	t.Run("the page has the parts the policy has", func(t *testing.T) {
		for _, heading := range []string{"Structural spawns", "Signals", "Inline work", "Records", "Refusals", "The QA calls of the final gate"} {
			if sectionOf(text, heading) == "" {
				t.Errorf("%s has no section %q", spawnPolicyRef, heading)
			}
		}
		qa := normalized(sectionOf(text, "The QA calls of the final gate"))
		if missing := wordingMissing(qa, []string{
			"SP-QA-1", "SP-QA-2",
			`(?i)scratch worktree`,
			`(?i)no native spawn`,
			`(?i)two (read-only )?(explorer|oracle)`,
			`(?i)INCONCLUSIVE[^.]*fails the gate`,
		}); len(missing) > 0 {
			t.Errorf("the QA section lacks: %s\n%s", strings.Join(missing, "\n  "), qa)
		}
		refusals := normalized(sectionOf(text, "Refusals"))
		if missing := wordingMissing(refusals, []string{
			`(?i)two kinds`,
			"`independence_refused`", "`pair_refused`", "`missing_step_decision`", "`policy_refused`", "`foreign_handle`", "`schema_not_strict`", "`author_unattributed`",
			"`native_type_refused`", "`native_limit_refused`", "`flow_bound_unavailable`", "`full_history_fork_refused`",
			`(?i)no option overrides`,
			"`held`", "`unknown`", "`null_model`", "`superseded`",
			`(?i)not refusals`,
		}); len(missing) > 0 {
			t.Errorf("the Refusals section lacks: %s\n%s", strings.Join(missing, "\n  "), refusals)
		}
	})

	t.Run("the numbers of the page are the constants of rules.go", func(t *testing.T) {
		signals := normalized(sectionOf(text, "Signals"))
		inline := normalized(sectionOf(text, "Inline work"))
		for _, c := range []struct {
			name, section, pattern string
		}{
			{"ExplorerConcurrency", signals, `at most %d (explorers )?(at once|concurrent)`},
			{"ExplorersPerMilestone", signals, `at most %d (explorers )?(a|per|for each) milestone`},
			{"ResearchPerTier", signals, `at most %d research continuations`},
			{"EvidenceExplorersPerNode", signals, `at most %d (explorers )?(a|per|for each) node`},
			{"TierRounds", signals, `%d reviewed rounds`},
			{"InlineFiles", inline, `(at most|<=) %d files`},
		} {
			want := engineIntConst(t, c.name)
			pattern := strings.Replace(c.pattern, "%d", strconv.Itoa(want), 1)
			if !regexp.MustCompile(pattern).MatchString(c.section) {
				t.Errorf("the page does not state %s = %d (pattern %s)", c.name, want, pattern)
			}
		}
	})

	// The judge itself: a missing id, a doubled row and an id the engine does not have are each reported.
	t.Run("fixture: the comparison reports each difference", func(t *testing.T) {
		engine := []string{"SP-A-1", "SP-B-1"}
		for label, c := range map[string]struct{ text, want string }{
			"missing":  {"| `SP-A-1` | x |\n", "no table row for SP-B-1"},
			"doubled":  {"| `SP-A-1` | x |\n| `SP-B-1` | x |\n| `SP-A-1` | y |\n", "two table rows for SP-A-1"},
			"foreign":  {"| `SP-A-1` | x |\n| `SP-B-1` | x |\n| `SP-C-9` | x |\n", "SP-C-9"},
			"in prose": {"| `SP-A-1` | x |\n| `SP-B-1` | x |\n\nSee SP-Z-2.\n", "names SP-Z-2"},
		} {
			found := false
			for _, p := range spawnPolicyProblems(c.text, engine) {
				found = found || strings.Contains(p, c.want)
			}
			if !found {
				t.Errorf("%s: no problem contains %q: %v", label, c.want, spawnPolicyProblems(c.text, engine))
			}
		}
		if p := spawnPolicyProblems("| `SP-A-1` | x |\n| `SP-B-1` | x |\n", engine); len(p) > 0 {
			t.Errorf("an equal list is reported: %v", p)
		}
	})
}

// The pages and skills that name the spawn policy point at the page, so a reader who meets a spawn decision finds its rule in one place.
func TestSpawnPolicyIsPointedAt(t *testing.T) {
	const link = "crw-canon/references/spawn-policy.md"
	for _, file := range []string{
		"plugins/crw/skills/crw-canon/SKILL.md",
		"plugins/crw/skills/crw-run/SKILL.md",
		"plugins/crw/skills/crw-pabcd/SKILL.md",
		"plugins/crw/skills/crw-canon/references/flow.md",
	} {
		data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, link) && !strings.Contains(text, "](references/spawn-policy.md") && !strings.Contains(text, "](spawn-policy.md") {
			t.Errorf("%s does not link the spawn policy page", file)
		}
	}
}
