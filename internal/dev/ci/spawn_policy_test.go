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

	// retiredSpawnRules are rule ids rules.go keeps only so that old decision rows stay readable (CRW-1418: the `child` session path
	// is retired, a node sized `child` goes up as a `size_split` escalation). The page has no row for them and does not name them.
	retiredSpawnRules = map[string]bool{"SP-CHILD-1": true, "SP-CHILD-2": true}
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
		if !retiredSpawnRules[id] && !slices.Contains(rows, id) {
			problems = append(problems, "no table row for "+id)
		}
	}
	for i, id := range sorted {
		if i > 0 && sorted[i-1] == id {
			problems = append(problems, "two table rows for "+id)
		}
		switch {
		case retiredSpawnRules[id]:
			problems = append(problems, "the table row "+id+" is a retired rule id")
		case !slices.Contains(engine, id):
			problems = append(problems, "the table row "+id+" is no rule id of internal/flow/ops/rules.go")
		}
	}
	for _, id := range spawnRuleID.FindAllString(text, -1) {
		switch {
		case retiredSpawnRules[id]:
			problems = append(problems, "the text names "+id+", a retired rule id")
		case !slices.Contains(engine, id):
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

	// The inline route and the route of an out-of-authority question (CRW-1347): SP-INL-1 is a research question's, never an open
	// question of a DESIGN's, in the page and in the description of rules.go, and the SP-EXPL-1 row names each scope notation the
	// router reads (the authority:* constants of rules.go) and that such a question goes to the orchestrator and to no explorer.
	t.Run("the inline and authority routes of the page are those of rules.go", func(t *testing.T) {
		rules := readRepoText(t, spawnRulesGo)
		desc := regexp.MustCompile(`(?m)^\s*RuleInl1\s*=\s*"SP-INL-1"\s*//(.*)$`).FindStringSubmatch(rules)
		row := ""
		expl := ""
		for _, line := range lines(text) {
			switch {
			case strings.HasPrefix(strings.TrimSpace(line), "| `SP-INL-1` |"):
				row = line
			case strings.HasPrefix(strings.TrimSpace(line), "| `SP-EXPL-1` |"):
				expl = line
			}
		}
		for where, text := range map[string]string{"the RuleInl1 description of " + spawnRulesGo: func() string {
			if desc == nil {
				return ""
			}
			return desc[1]
		}(), "the SP-INL-1 row of " + spawnPolicyRef: row} {
			if !regexp.MustCompile(`(?i)a research question`).MatchString(text) || !regexp.MustCompile(`(?i)never (to )?an open question of a DESIGN`).MatchString(text) {
				t.Errorf("%s does not say SP-INL-1 is a research question's and never an open question of a DESIGN's: %q", where, text)
			}
		}
		notations := regexp.MustCompile(`"(authority:[a-z-]+)"`).FindAllStringSubmatch(rules, -1)
		if len(notations) != 4 {
			t.Fatalf("%s has %d authority notations, want 4: %v", spawnRulesGo, len(notations), notations)
		}
		for _, n := range notations {
			if !strings.Contains(expl, "`"+n[1]+"`") {
				t.Errorf("the SP-EXPL-1 row does not name the notation %s: %q", n[1], expl)
			}
		}
		// CRW-1353: the owner thread is named by the level table, the child (project session).
		if !regexp.MustCompile(`(?i)child[^|]*no explorer|no explorer[^|]*child`).MatchString(expl) {
			t.Errorf("the SP-EXPL-1 row does not say an out-of-authority question goes to the child and to no explorer: %q", expl)
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

	// A retired id (rules.go keeps it for old rows) needs no row, and a row or a prose mention of it is reported.
	t.Run("fixture: a retired rule id has no row and is not named", func(t *testing.T) {
		engine := []string{"SP-A-1", "SP-B-1", "SP-CHILD-1", "SP-CHILD-2"}
		if p := spawnPolicyProblems("| `SP-A-1` | x |\n| `SP-B-1` | x |\n", engine); len(p) > 0 {
			t.Errorf("a page without the retired ids is reported: %v", p)
		}
		for label, c := range map[string]struct{ text, want string }{
			"as a row": {"| `SP-A-1` | x |\n| `SP-B-1` | x |\n| `SP-CHILD-1` | x |\n", "the table row SP-CHILD-1 is a retired rule id"},
			"in prose": {"| `SP-A-1` | x |\n| `SP-B-1` | x |\n\nSee SP-CHILD-2.\n", "the text names SP-CHILD-2, a retired rule id"},
		} {
			p := spawnPolicyProblems(c.text, engine)
			if !slices.Contains(p, c.want) {
				t.Errorf("%s: no problem is %q: %v", label, c.want, p)
			}
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
