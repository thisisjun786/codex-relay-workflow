//go:build dev

package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The read-only skills have one owner for each judgment (CRW-1218): crw-status reports by level and
// chooses the next action, crw-check owns the audit and the schedule judgment, crw-plan and crw-status cite it.

const (
	statusSkillFile = "plugins/crw/skills/crw-status/SKILL.md"
	checkSkillFile  = "plugins/crw/skills/crw-check/SKILL.md"
	scheduleJudgeMD = "plugins/crw/skills/crw-check/references/schedule-judgment.md"
)

// reportSectionOf is the text of the section headed heading (an exact heading text) up to the next heading of the same or a higher level.
func reportSectionOf(text, heading string) (string, bool) {
	var out []string
	in, level := false, 0
	for _, line := range strings.Split(text, "\n") {
		m := regexp.MustCompile(`^(#{1,6}) (.*)$`).FindStringSubmatch(line)
		if m != nil {
			if in && len(m[1]) <= level {
				break
			}
			if !in && m[2] == heading {
				in, level = true, len(m[1])
				continue
			}
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n"), in
}

// TestScheduleJudgmentSingleSource: crw-check defines the schedule verdicts, their order and their report words;
// crw-status and crw-plan cite that reference and hold no second copy.
func TestScheduleJudgmentSingleSource(t *testing.T) {
	if splitExists("plugins/crw/skills/crw-status/references/schedule-progress.md") {
		t.Error("crw-status still has references/schedule-progress.md, a second copy of the schedule judgment")
	}
	judge := splitRead(t, scheduleJudgeMD)
	for _, word := range []string{"앞섬", "계획대로", "지연 위험", "지연", "판단 불가"} {
		if !strings.Contains(judge, word) {
			t.Errorf("%s does not give the report word %s", scheduleJudgeMD, word)
		}
	}
	// The report words are defined in the judgment reference alone: another file may use a word in an example or a
	// report, but a table row that gives a word its meaning is a second definition.
	definingRow := regexp.MustCompile(`(?m)^\| *(앞섬|계획대로|지연 위험|지연|판단 불가|Ahead|On plan|At risk|Late|Undecidable) *\|`)
	err := filepath.WalkDir(filepath.Join(repoRoot(), "plugins/crw/skills"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		rel, _ := filepath.Rel(repoRoot(), path)
		if filepath.ToSlash(rel) == scheduleJudgeMD {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := definingRow.FindString(string(data)); loc != "" {
			t.Errorf("%s has the table row %q: the verdicts are defined in %s alone", filepath.ToSlash(rel), loc, scheduleJudgeMD)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for file, link := range map[string]string{
		statusSkillFile: "](../crw-check/references/schedule-judgment.md",
		"plugins/crw/skills/crw-plan/references/scheduling.md": "](../../crw-check/references/schedule-judgment.md",
	} {
		if !strings.Contains(splitRead(t, file), link) {
			t.Errorf("%s does not cite the schedule judgment (%s)", file, link)
		}
	}
	// The verdict table is in the judgment reference alone.
	verdictRow := regexp.MustCompile(`(?m)^\| *\d+ *\|.*\| *(Ahead|On plan|Late|At risk|Undecidable)\b`)
	for _, file := range []string{statusSkillFile, checkSkillFile, "plugins/crw/skills/crw-plan/references/scheduling.md", "plugins/crw/skills/crw-plan/SKILL.md"} {
		if verdictRow.MatchString(splitRead(t, file)) {
			t.Errorf("%s holds a verdict decision table", file)
		}
	}
}

// TestScheduleReportWordsTranslateTheTable: each Report words reading cites exactly the decision-table rows that give
// its verdict, so the words are a translation of the rules and cannot define a second reading (CRW-1218 d2). The
// readings of a completed-late result and of an unconfirmed baseline follow the table: an achieved result after its
// deadline is Late (row 4), and an unconfirmed baseline makes the baseline axis Undecidable (row 1).
func TestScheduleReportWordsTranslateTheTable(t *testing.T) {
	judge := splitRead(t, scheduleJudgeMD)
	order, ok := reportSectionOf(judge, "Order the verdict")
	if !ok {
		t.Fatal("no Order the verdict section")
	}
	rowsOf := map[string][]string{}
	for _, m := range regexp.MustCompile(`(?m)^\| *(\d+) *\|.*\| *(Ahead|On plan|At risk|Late|Undecidable)\b[^|]*\|$`).FindAllStringSubmatch(order, -1) {
		rowsOf[m[2]] = append(rowsOf[m[2]], m[1])
	}
	if len(rowsOf) != 5 {
		t.Fatalf("the decision table gives verdict rows for %v, want the five verdicts", rowsOf)
	}
	words, ok := reportSectionOf(judge, "Report words")
	if !ok {
		t.Fatal("no Report words section")
	}
	readingOf := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\| *(Ahead|On plan|At risk|Late|Undecidable) *\| *[^|]+\| *([^|]+)\|$`).FindAllStringSubmatch(words, -1) {
		readingOf[m[1]] = m[2]
	}
	for verdict, rows := range rowsOf {
		reading, ok := readingOf[verdict]
		if !ok {
			t.Errorf("Report words has no reading for %s", verdict)
			continue
		}
		cited := map[string]bool{}
		for _, m := range regexp.MustCompile(`(?i)\brows? (\d+(?:(?:,| and) \d+)*)`).FindAllStringSubmatch(reading, -1) {
			for _, n := range regexp.MustCompile(`\d+`).FindAllString(m[1], -1) {
				cited[n] = true
			}
		}
		for _, row := range rows {
			if !cited[row] {
				t.Errorf("the reading of %s does not cite row %s, which gives that verdict", verdict, row)
			}
			delete(cited, row)
		}
		for extra := range cited {
			t.Errorf("the reading of %s cites row %s, which gives another verdict", verdict, extra)
		}
	}
	if strings.Contains(readingOf["Late"], "does not exist") {
		t.Error("the reading of Late requires the result to be absent, but row 4 is an achieved result that landed after the deadline")
	}
	unconfirmed := regexp.MustCompile(`(?s)Where a Schedule baseline is \x60unconfirmed\x60.*?(?:\n\n|$)`).FindString(words)
	if unconfirmed == "" {
		t.Fatal("Report words does not say what an unconfirmed baseline reports")
	}
	if !strings.Contains(unconfirmed, "판단 불가") || strings.Contains(unconfirmed, "survive") {
		t.Errorf("an unconfirmed baseline makes the baseline axis undecidable (Judge both axes); Report words says: %s", unconfirmed)
	}
}

// TestCheckActorSections: crw-check is divided by actor, the relay-store details are crw-run's, and the check's
// dispositions and the reviewer's VERDICT grammar correspond in one table.
func TestCheckActorSections(t *testing.T) {
	check := splitRead(t, checkSkillFile)
	headings := headingsOf(check)
	index := map[string]int{}
	for i, h := range headings {
		index[h] = i
	}
	order := []string{"Standalone audit", "In-workflow auditor", "Bounded helper"}
	last := -1
	for _, name := range order {
		i, ok := index[name]
		switch {
		case !ok:
			t.Errorf("%s has no section %q", checkSkillFile, name)
		case i <= last:
			t.Errorf("%s: the section %q is out of order", checkSkillFile, name)
		default:
			last = i
		}
	}
	// Contradictions are part of the check, not a skill of their own.
	if _, ok := index["Find contradictions"]; !ok {
		t.Errorf("%s has no section \"Find contradictions\"", checkSkillFile)
	}
	// The relay-store details moved to crw-run.
	const records = "plugins/crw/skills/crw-run/references/check-records.md"
	for _, store := range []string{"--expect-criteria-digest", "already_claimed", "QUEUES"} {
		if strings.Contains(check, store) {
			t.Errorf("%s still holds the relay-store detail %q", checkSkillFile, store)
		}
		if !strings.Contains(splitRead(t, records), store) {
			t.Errorf("%s does not hold the relay-store detail %q", records, store)
		}
	}
	if !strings.Contains(check, "](../crw-run/references/check-records.md") {
		t.Errorf("%s does not link %s", checkSkillFile, records)
	}
	// One table maps the check's dispositions to the reviewer grammar.
	table, ok := reportSectionOf(check, "Verdict words and the reviewer grammar")
	if !ok {
		t.Fatalf("%s has no section \"Verdict words and the reviewer grammar\"", checkSkillFile)
	}
	rows := tableRows(table, "Check disposition")
	if len(rows) != 3 {
		t.Fatalf("the correspondence table has %d rows, want 3: %q", len(rows), rows)
	}
	for i, disposition := range []string{"verified", "needs_changes", "unverified"} {
		if !strings.Contains(rows[i][0], disposition) {
			t.Errorf("row %d of the correspondence table is %q, want %s", i+1, rows[i][0], disposition)
		}
	}
	for _, grammar := range []string{"VERDICT: PASS", "VERDICT: GO-WITH-FIXES", "VERDICT: FAIL"} {
		if !strings.Contains(table, grammar) {
			t.Errorf("the correspondence table does not name %s", grammar)
		}
	}
}

// TestStatusReportsByLevel: crw-status reports manager, initiative, project, issue in that order, routes a gap
// one level down, chooses the next action and writes nothing.
func TestStatusReportsByLevel(t *testing.T) {
	status := splitRead(t, statusSkillFile)
	section, ok := reportSectionOf(status, "Report by level")
	if !ok {
		t.Fatalf("%s has no section \"Report by level\"", statusSkillFile)
	}
	last := -1
	for _, level := range []string{"manager", "initiative lead", "project orchestrator", "issue"} {
		i := strings.Index(section, level)
		if i < 0 || i <= last {
			t.Errorf("the level %q is missing or out of order in the report-by-level section", level)
		}
		last = i
	}
	if !strings.Contains(section, "one level down") {
		t.Error("the report-by-level section does not send a gap one level down")
	}
	if !strings.Contains(strings.ToLower(status), "read-only") {
		t.Errorf("%s does not say it is read-only", statusSkillFile)
	}
	if _, ok := reportSectionOf(status, "Choose one next action"); !ok {
		t.Errorf("%s has no section \"Choose one next action\"", statusSkillFile)
	}
	descriptions, err := skillDescriptions(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{"다음에 뭐", "어디까지"} {
		for name, description := range descriptions {
			if has := strings.Contains(description, trigger); has != (name == "crw-status") {
				t.Errorf("%s: the trigger %q in the description is %v, want it in crw-status alone", name, trigger, has)
			}
		}
	}
	if strings.Contains(splitRead(t, runSkillFile), "Status only") {
		t.Errorf("%s keeps a status-only mode", runSkillFile)
	}
}
