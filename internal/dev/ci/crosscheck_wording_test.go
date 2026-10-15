//go:build dev

package ci

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CRW-1442 (bundle 1442-r3-close, part A): the milestone close runs the crosscheck in place of reviewer-final (CRW-1473):
// crw.crosscheck, one round of two passes by a review engine of another model family than every author, merged into one verdict by
// one rule (ops.CrosscheckRule); a needs_changes verdict stops the close as crosscheck_pending, and land-head has its fix rounds
// (CRW-1490). The pages a reader takes the close from say so: script.md's close section with the round-1 lines of the engine
// configuration table the parent decided, its land-head section, journal.md's rows and report body, and crw-run's account of what
// goes up. The check pins only what part A ships and part B keeps: the reason words, the rule sentence and the round-1 column.

const (
	crosscheckGoPath = "internal/flow/ops/crosscheck.go"
	journalDocPath   = "docs/flow/journal.md"
	crwRunSkillPath  = "plugins/crw/skills/crw-run/SKILL.md"
)

// crosscheckRuleOf is ops.CrosscheckRule as crosscheck.go spells it: the string literals of the const, joined.
func crosscheckRuleOf(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\nconst CrosscheckRule = (.*?)\n\n`).FindStringSubmatch(readRepoText(t, crosscheckGoPath))
	if m == nil {
		t.Fatalf("%s has no const CrosscheckRule", crosscheckGoPath)
	}
	var b strings.Builder
	for _, lit := range regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).FindAllString(m[1], -1) {
		s, err := strconv.Unquote(lit)
		if err != nil {
			t.Fatalf("%s: CrosscheckRule literal %s: %v", crosscheckGoPath, lit, err)
		}
		b.WriteString(s)
	}
	if b.Len() == 0 {
		t.Fatalf("%s: CrosscheckRule has no string literal", crosscheckGoPath)
	}
	return b.String()
}

// crosscheckRoundOne is the round-1 column of the engine configuration table (the parent's decision of 10-16 00:0x): the authors of
// the milestone by a word of the row's first cell, and what round 1 runs on or is refused with.
var crosscheckRoundOne = []struct{ authors, roundOne string }{
	{"claude only", "astra"},
	{"a gpt author", "grok"},
	{"a grok author", "astra"},
	{"gpt and grok authors", "crosscheck_no_independent_engine"},
	{"undecided", "crosscheck_author_undecided"},
}

// closeDocs are the texts the check reads.
type closeDocs struct{ script, journal, run string }

// closeDocsProblems judges the texts against the close part A ships; rule is ops.CrosscheckRule.
func closeDocsProblems(d closeDocs, rule string) []string {
	var problems []string
	section := sectionOf(d.script, "The milestone close")
	flat := normalized(section)
	if flat == "" {
		return []string{"script.md has no section \"The milestone close\""}
	}
	for _, want := range []string{"`crw.crosscheck", "crosscheck_pending", "crosscheck_no_verdict", "crosscheck_no_independent_engine", "crosscheck_author_undecided",
		"crosscheck_no_run_dir", "close_unverified"} {
		if !strings.Contains(flat, want) {
			problems = append(problems, "the close section lacks "+want)
		}
	}
	if !strings.Contains(flat, normalized(rule)) {
		problems = append(problems, "the close section does not hold the rule sentence of ops.CrosscheckRule word for word")
	}
	if !regexp.MustCompile(`(?i)decision of the parent|parent's decision`).MatchString(flat) {
		problems = append(problems, "the close section does not say the engine configuration is the parent's decision")
	}
	rows := tableRows(section, "Authors of the milestone")
	for _, want := range crosscheckRoundOne {
		var matched [][]string
		for _, row := range rows {
			if strings.Contains(strings.ToLower(row[0]), want.authors) {
				matched = append(matched, row)
			}
		}
		switch {
		case len(matched) != 1:
			problems = append(problems, "the configuration table has "+strconv.Itoa(len(matched))+" rows for \""+want.authors+"\", want 1")
		case len(matched[0]) < 2 || !strings.Contains(matched[0][1], want.roundOne):
			problems = append(problems, "the configuration table's round 1 for \""+want.authors+"\" is not "+want.roundOne)
		}
	}
	land := normalized(sectionOf(d.script, "Landing a head made outside a run"))
	for _, want := range []string{"fix_round", "land_fix_rounds", "LAND_FIX_ROUNDS"} {
		if !strings.Contains(land, want) {
			problems = append(problems, "the land-head section lacks "+want)
		}
	}
	journal := normalized(d.journal)
	for _, want := range []string{"`crw.crosscheck`", "close_unverified", "`Crosscheck:`", "`Reflection:`"} {
		if !strings.Contains(journal, want) {
			problems = append(problems, "journal.md lacks "+want)
		}
	}
	if !strings.Contains(normalized(d.run), "crosscheck_pending") {
		problems = append(problems, "crw-run/SKILL.md lacks crosscheck_pending")
	}
	return problems
}

func TestCloseDocsNameTheCrosscheck(t *testing.T) {
	rule := crosscheckRuleOf(t)
	docs := closeDocs{script: readRepoText(t, scriptDocPath), journal: readRepoText(t, journalDocPath), run: readRepoText(t, crwRunSkillPath)}
	for _, p := range closeDocsProblems(docs, rule) {
		t.Error(p)
	}

	t.Run("fixture: each missing piece is reported", func(t *testing.T) {
		good := closeDocs{
			script: "## Landing a head made outside a run\n\nA `fix_round` under `LAND_FIX_ROUNDS` hands a fix packet; at the cap `land_fix_rounds`.\n\n" +
				"## The milestone close\n\n`crw.crosscheck(milestone, gate, opts)` runs. " + rule + " A needs_changes stops `crosscheck_pending`; " +
				"`crosscheck_no_verdict`, `crosscheck_no_run_dir`, `close_unverified`. The table is the parent's decision.\n\n" +
				"| Authors of the milestone | Round 1 |\n|---|---|\n| claude only | astra |\n| a gpt author | grok |\n| a grok author | astra |\n" +
				"| gpt and grok authors | refused `crosscheck_no_independent_engine` |\n| an undecided author pair | refused `crosscheck_author_undecided` |\n\n" +
				"## The next section\n",
			journal: "A row `crw.crosscheck`; the refusal `close_unverified`; the body lines `Crosscheck:` and `Reflection:`.",
			run:     "a crosscheck_pending goes up",
		}
		if p := closeDocsProblems(good, rule); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		for label, mutate := range map[string]func(closeDocs) closeDocs{
			"no close section": func(d closeDocs) closeDocs {
				d.script = strings.Replace(d.script, "## The milestone close", "## Other", 1)
				return d
			},
			"no op":       func(d closeDocs) closeDocs { d.script = strings.Replace(d.script, "`crw.crosscheck(", "`crw.check(", 1); return d },
			"no rule":     func(d closeDocs) closeDocs { d.script = strings.Replace(d.script, "only minor_separable", "minor_separable", 1); return d },
			"no pending":  func(d closeDocs) closeDocs { d.script = strings.Replace(d.script, "`crosscheck_pending`", "`pending`", 1); return d },
			"no run dir":  func(d closeDocs) closeDocs { d.script = strings.Replace(d.script, "`crosscheck_no_run_dir`", "", 1); return d },
			"no decision": func(d closeDocs) closeDocs { d.script = strings.Replace(d.script, "the parent's decision", "a default", 1); return d },
			"gpt row on astra": func(d closeDocs) closeDocs {
				d.script = strings.Replace(d.script, "| a gpt author | grok |", "| a gpt author | astra |", 1)
				return d
			},
			"no undecided row": func(d closeDocs) closeDocs {
				d.script = strings.Replace(d.script, "| an undecided author pair | refused `crosscheck_author_undecided` |\n", "", 1)
				return d
			},
			"no fix rounds": func(d closeDocs) closeDocs { d.script = strings.Replace(d.script, "`land_fix_rounds`", "the cap", 1); return d },
			"no row":        func(d closeDocs) closeDocs { d.journal = strings.Replace(d.journal, "`crw.crosscheck`", "a row", 1); return d },
			"no body line":  func(d closeDocs) closeDocs { d.journal = strings.Replace(d.journal, "`Crosscheck:`", "", 1); return d },
			"run silent":    func(d closeDocs) closeDocs { d.run = "nothing goes up"; return d },
		} {
			if p := closeDocsProblems(mutate(good), rule); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})
}
