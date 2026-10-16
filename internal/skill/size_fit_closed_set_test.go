package skill

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The size fields of an issue line (size_fit, size_reason and the issue-size answer) are closed sets that three places name: the code
// (SizeFits, SizeReasons and IssueSizeAnswers, which `crw flow compile` and `crw skill size-fit` both read), the table of crw-plan that
// tells the planner what to write, and crw-add-issue, which writes the fields. TestSizeFitClosedSet holds the three to one set.

func planSkillText(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// closedSetsOf reads the table of the size fields: one row for each field, the values of the second cell in code font.
func closedSetsOf(text string) map[string][]string {
	sets := map[string][]string{}
	code := regexp.MustCompile("`([a-z0-9_]+)`")
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(trimmed, "|"), "|")
		if len(cells) < 2 {
			continue
		}
		field := code.FindStringSubmatch(cells[0])
		if field == nil || !slices.Contains([]string{"size_fit", "size_reason", "issue_size"}, field[1]) {
			continue
		}
		for _, m := range code.FindAllStringSubmatch(cells[1], -1) {
			sets[field[1]] = append(sets[field[1]], m[1])
		}
	}
	return sets
}

// closedSetProblems compares the table text with the code's sets.
func closedSetProblems(table string, fits, reasons, answers []string) []string {
	var problems []string
	sets := closedSetsOf(table)
	for field, want := range map[string][]string{"size_fit": fits, "size_reason": reasons, "issue_size": answers} {
		got := slices.Clone(sets[field])
		slices.Sort(got)
		want = slices.Sorted(slices.Values(want))
		if !slices.Equal(got, want) {
			problems = append(problems, field+" is "+strings.Join(got, ", ")+" in the table, "+strings.Join(want, ", ")+" in the code")
		}
	}
	return problems
}

func TestSizeFitClosedSet(t *testing.T) {
	plan := planSkillText(t, "plugins/crw/skills/crw-plan/SKILL.md")
	if problems := closedSetProblems(plan, SizeFits, SizeReasons, IssueSizeAnswers); len(problems) > 0 {
		t.Errorf("crw-plan/SKILL.md does not name the closed sets of the code:\n  %s", strings.Join(problems, "\n  "))
	}

	// The reasons that name a count carry the line of the count: regions_gt3 is the line sizeFitMaxRegions.
	for reason, line := range map[string]int{"regions_gt": sizeFitMaxRegions, "packages_contract_gt": sizeFitMaxContractPackage, "steps_gt": sizeFitMaxSteps} {
		if !slices.Contains(SizeReasons, reason+strconv.Itoa(line)) {
			t.Errorf("no reason %s%d: the closed set and the lines of the suggester differ", reason, line)
		}
	}
	if !slices.Contains(SizeReasons, "verify_gt"+strconv.Itoa(sizeFitMaxVerifyMinutes)+"m") {
		t.Errorf("no reason verify_gt%dm: the closed set and the lines of the suggester differ", sizeFitMaxVerifyMinutes)
	}

	// crw-add-issue writes the fields and names no value of its own: every value in code font that has the form of a size reason, a
	// size fit or an answer is one of the sets, and it points at the table instead of restating it.
	add := planSkillText(t, "plugins/crw/skills/crw-add-issue/SKILL.md")
	known := slices.Concat(SizeFits, SizeReasons, IssueSizeAnswers)
	for _, m := range regexp.MustCompile("`((?:regions|packages_contract|steps|verify)_gt[0-9]+m?|own_goal|fits_executor|over_line[a-z_]*)`").FindAllStringSubmatch(add, -1) {
		if !slices.Contains(known, m[1]) {
			t.Errorf("crw-add-issue names `%s`, which is in no closed set", m[1])
		}
	}
	for _, want := range []string{"`size_fit`", "`size_reason`", "`issue_size`", "(../crw-plan/SKILL.md#"} {
		if !strings.Contains(add, want) {
			t.Errorf("crw-add-issue does not name %s", want)
		}
	}

	// A suggestion is always a member of the set the table names, whatever the counts.
	for _, c := range []SizeFitCounts{{}, {EditRegions: ptr(9)}, {ContractPackages: ptr(3)}, {Steps: ptr(6)}, {VerifyMinutes: ptr(21)}} {
		p := SuggestSizeFit(SizeFitInput{Issue: "CRW-1", Answer: "ok", Counts: c})
		if !slices.Contains(SizeReasons, p.SizeReason) || !slices.Contains(SizeFits, p.SuggestedSizeFit) {
			t.Errorf("counts %+v: the suggester proposes %s / %s, outside the closed sets", c, p.SuggestedSizeFit, p.SizeReason)
		}
	}

	t.Run("fixture: a table with a missing, an extra or a renamed value is reported", func(t *testing.T) {
		good := "| Field | Values |\n|---|---|\n| `size_fit` | `executor`, `split`, `bundle_split` |\n| `size_reason` | `regions_gt3`, `own_goal`, `fits_executor` |\n| `issue_size` | `ok`, `over_line`, `over_line_accepted` |\n"
		fits, reasons, answers := []string{"executor", "split", "bundle_split"}, []string{"regions_gt3", "own_goal", "fits_executor"}, []string{"ok", "over_line", "over_line_accepted"}
		if p := closedSetProblems(good, fits, reasons, answers); len(p) > 0 {
			t.Fatalf("the good table is reported: %v", p)
		}
		for label, table := range map[string]string{
			"missing": strings.Replace(good, "`split`", "", 1),
			"extra":   strings.Replace(good, "`own_goal`,", "`own_goal`, `big`,", 1),
			"renamed": strings.Replace(good, "`regions_gt3`", "`regions_gt4`", 1),
			"no row":  strings.Replace(good, "| `issue_size` | `ok`, `over_line`, `over_line_accepted` |\n", "", 1),
		} {
			if p := closedSetProblems(table, fits, reasons, answers); len(p) == 0 {
				t.Errorf("%s: the changed table is accepted", label)
			}
		}
	})
}

func ptr(n int) *int { return &n }
