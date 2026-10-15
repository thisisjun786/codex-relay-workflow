//go:build dev

package ci

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CRW-1465: the node lane of the project lane judges an executor's work by a unit check before the run commits it
// (crw.unitCheck: the review tool run in Go, no reviewer agent), sends its findings back to the same executor thread for at most
// UNIT_CHECK_ROUNDS round trips and carries what is left; the reviewer path (crw.review, crw.reviewHalt, crw.exploreEvidence) stays
// only for the journals of old runs. The pages a reader takes the lane from say so: script.md's project lane and its lint table,
// the canon's flow page, the spawn policy and crw-run's account of how the child works.

const (
	scriptDocPath   = "docs/flow/script.md"
	projectLaneJS   = "internal/flow/workflows/project-lane.js"
	unitCheckRounds = "UNIT_CHECK_ROUNDS"
)

var numberWords = map[int]string{1: "one", 2: "two", 3: "three", 4: "four"}

// unitCheckRoundsOf is the UNIT_CHECK_ROUNDS constant of project-lane.js, the cap the pages state.
func unitCheckRoundsOf(t *testing.T) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^const ` + unitCheckRounds + ` = (\d+)$`).FindStringSubmatch(readRepoText(t, projectLaneJS))
	if m == nil {
		t.Fatalf("%s has no const %s", projectLaneJS, unitCheckRounds)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// lineWithPrefix is the first line of text that starts with prefix after its indentation.
func lineWithPrefix(text, prefix string) string {
	for _, line := range lines(text) {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// boldParagraph is the paragraph of text that starts with the bold lead **lead**, up to the next blank line.
func boldParagraph(text, lead string) string {
	var out []string
	in := false
	for _, line := range lines(text) {
		trimmed := strings.TrimSpace(line)
		if !in {
			if strings.HasPrefix(trimmed, "**"+lead+"**") {
				in = true
				out = append(out, trimmed)
			}
			continue
		}
		if trimmed == "" {
			break
		}
		out = append(out, trimmed)
	}
	return strings.Join(out, " ")
}

// rounds is the cap as the pages may write it: the digit or the word.
func roundsPattern(n int) string {
	if w, ok := numberWords[n]; ok {
		return "(" + strconv.Itoa(n) + "|" + w + ")"
	}
	return strconv.Itoa(n)
}

// nodeLaneUnitCheckProblems judges script.md: the project lane's transition table, its unit-check paragraph, its review paragraph
// and the lint table's land rule.
func nodeLaneUnitCheckProblems(script string, rounds int) []string {
	var problems []string
	lane := sectionOf(script, "The project lane")
	if lane == "" {
		return []string{"script.md has no section \"The project lane\""}
	}
	ready := lineWithPrefix(lane, "| `ready` |")
	if !regexp.MustCompile(`(?i)unit check[^|]*commit[^|]*verify[^|]*land`).MatchString(ready) || strings.Contains(ready, "review") {
		problems = append(problems, "the ready row does not run the unit check before commit, verify and land (and no review): "+ready)
	}
	if failed := lineWithPrefix(lane, "| `self_failed`"); !regexp.MustCompile(`(?i)unit check`).MatchString(failed) {
		problems = append(problems, "the self_failed row does not name the unit check: "+failed)
	}
	uc := boldParagraph(lane, "Unit check.")
	if uc == "" {
		problems = append(problems, "the project lane has no **Unit check.** paragraph")
	} else {
		for _, p := range wordingMissing(uc, []string{
			"`crw.unitCheck\\(",
			"`crw.unitCheck\\([^`]*`[^`]*before[^.]*`crw.commit`",
			`(?i)no reviewer (agent|call|thread)`,
			`(?i)same executor thread`,
			`(?i)at most ` + roundsPattern(rounds) + ` round trips`,
			"`" + unitCheckRounds + "`",
			`(?i)carried`,
			"`unit_check_carried`",
			"source `unit_check`",
			`(?i)resume asks the tool again`,
		}) {
			problems = append(problems, "the unit check paragraph lacks "+p)
		}
	}
	if review := boldParagraph(lane, "Review."); !regexp.MustCompile(`(?i)old reviewer path`).MatchString(review) || !regexp.MustCompile(`(?i)old journals|old runs`).MatchString(review) {
		problems = append(problems, "the review paragraph does not say crw.review is the old reviewer path kept for old journals")
	}
	if ops := lineWithPrefix(lane, "| `unitCheck("); ops == "" {
		problems = append(problems, "the operations table has no unitCheck row")
	}
	lint := sectionOf(script, "crw flow lint")
	row := lineWithPrefix(lint, "| `lifecycle_land_twice`")
	for _, p := range wordingMissing(row, []string{
		"`crw.unitCheck`",
		`(?i)land at most once[^|]*two ways[^|]*completed review[^|]*unit check`,
	}) {
		problems = append(problems, "the lint row of lifecycle_land_without_review lacks "+p)
	}
	return problems
}

// runSkillUnitCheckProblems judges crw-run's account of the node lane in "How the child works".
func runSkillUnitCheckProblems(section string) []string {
	flat := normalized(section)
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		`(?i)executors? in parallel[^.]*unit check[^.]*same executor thread[^.]*verification[^.]*land`,
		`(?i)no reviewer agent`,
		`(?i)no acceptance before the unit check and the verification`,
	}) {
		problems = append(problems, "How the child works lacks "+p)
	}
	return problems
}

// spawnPolicyUnitCheckProblems judges the reviewer rows of the spawn policy: SP-REV-1, SP-REV-2 and SP-EXPL-3 are the old reviewer
// path, and SP-REV-1 says what the project lane does instead.
func spawnPolicyUnitCheckProblems(page string) []string {
	var problems []string
	for _, id := range []string{"SP-REV-1", "SP-REV-2", "SP-EXPL-3"} {
		row := lineWithPrefix(page, "| `"+id+"` |")
		if row == "" {
			problems = append(problems, "the spawn policy has no row "+id)
			continue
		}
		if !regexp.MustCompile(`(?i)old reviewer path`).MatchString(row) {
			problems = append(problems, id+" does not name the old reviewer path")
		}
	}
	if row := lineWithPrefix(page, "| `SP-REV-1` |"); !regexp.MustCompile("(?i)unit check[^|]*`crw.unitCheck`|`crw.unitCheck`[^|]*unit check").MatchString(row) {
		problems = append(problems, "SP-REV-1 does not say the project lane judges a node by its unit check (crw.unitCheck)")
	}
	return problems
}

// flowPageUnitCheckProblems judges the canon's flow page: the ownership table's audit row and step 3 of the C4 procedure name the
// unit check (step 3 keeps the words "candidate review", which TestFlowControlOwnershipTableWording reads).
func flowPageUnitCheckProblems(section string) []string {
	var problems []string
	audit := lineWithPrefix(section, "| Audit |")
	if !regexp.MustCompile(`(?i)unit check`).MatchString(audit) {
		problems = append(problems, "the Audit row does not name the unit check: "+audit)
	}
	step := lineWithPrefix(section, "3. ")
	if !strings.Contains(step, "candidate review") || !regexp.MustCompile(`(?i)unit check[^.]*before[^.]*commit`).MatchString(step) {
		problems = append(problems, "step 3 of the C4 procedure is not the candidate review by the unit check before the commit: "+step)
	}
	return problems
}

func TestUnitCheckDocsNameTheNodeLaneUnitCheck(t *testing.T) {
	rounds := unitCheckRoundsOf(t)
	if p := nodeLaneUnitCheckProblems(readRepoText(t, scriptDocPath), rounds); len(p) > 0 {
		t.Errorf("%s:\n  %s", scriptDocPath, strings.Join(p, "\n  "))
	}
	if p := runSkillUnitCheckProblems(sectionOf(readRepoText(t, runSkillFile), "How the child works")); len(p) > 0 {
		t.Errorf("%s:\n  %s", runSkillFile, strings.Join(p, "\n  "))
	}
	if p := spawnPolicyUnitCheckProblems(readRepoText(t, spawnPolicyRef)); len(p) > 0 {
		t.Errorf("%s:\n  %s", spawnPolicyRef, strings.Join(p, "\n  "))
	}
	if p := flowPageUnitCheckProblems(ownershipSection(readRepoText(t, flowRef))); len(p) > 0 {
		t.Errorf("%s:\n  %s", flowRef, strings.Join(p, "\n  "))
	}

	t.Run("fixture: the reviewer-path wording is reported", func(t *testing.T) {
		old := "## The project lane\n\n| `crw.classify` state | The one transition |\n| --- | --- |\n| `ready` | commit, verify, review, land |\n" +
			"| `self_failed` | the run commits and verifies the worktree anyway and reviews it |\n\n" +
			"**Review.** `crw.review(node, plan, head, ci, opts)` is a fresh reviewer thread over the committed head (SP-REV-1).\n\n" +
			"## crw flow lint\n\n| Rule | What it finds |\n| --- | --- |\n| `lifecycle_land_twice`, `lifecycle_land_without_review` | then land at most once, from a completed review. |\n"
		got := strings.Join(nodeLaneUnitCheckProblems(old, 2), "\n")
		for _, want := range []string{"the ready row", "the self_failed row", "no **Unit check.** paragraph", "the review paragraph", "no unitCheck row", "lifecycle_land_without_review lacks"} {
			if !strings.Contains(got, want) {
				t.Errorf("no problem names %q:\n%s", want, got)
			}
		}
		if p := runSkillUnitCheckProblems("a reviewer of another family reviews each head, a fix round goes back to the same worktree. No acceptance before the verification and the review."); len(p) != 3 {
			t.Errorf("the old crw-run wording gives %d problems, want 3: %v", len(p), p)
		}
		if p := spawnPolicyUnitCheckProblems("| `SP-REV-1` | a reviewer | every head | x |\n| `SP-REV-2` | a batch | y | z |\n| `SP-EXPL-3` | evidence_missing | a | b |\n"); len(p) != 4 {
			t.Errorf("the old spawn policy rows give %d problems, want 4: %v", len(p), p)
		}
		if p := flowPageUnitCheckProblems("| Audit | the run | a reviewer of another model family for each head |\n\n3. The candidate review of each issue: a fresh reviewer.\n"); len(p) != 2 {
			t.Errorf("the old flow page gives %d problems, want 2: %v", len(p), p)
		}
	})
	t.Run("fixture: the cap is the constant", func(t *testing.T) {
		if roundsPattern(2) != "(2|two)" || roundsPattern(7) != "7" {
			t.Errorf("roundsPattern: %q %q", roundsPattern(2), roundsPattern(7))
		}
	})
}
