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

// spawnPolicyUnitCheckProblems judges the spawn policy's account of the node's judgment: the rows of the old reviewer path (SP-REV-1,
// SP-REV-2 and SP-EXPL-3, retired by CRW-1429) are gone, and the executor's row SP-EXEC-1 says the run judges a node by its unit check.
func spawnPolicyUnitCheckProblems(page string) []string {
	var problems []string
	for _, id := range []string{"SP-REV-1", "SP-REV-2", "SP-EXPL-3"} {
		if lineWithPrefix(page, "| `"+id+"` |") != "" {
			problems = append(problems, "the spawn policy keeps the row of "+id+", a rule of the retired reviewer path")
		}
	}
	if row := lineWithPrefix(page, "| `SP-EXEC-1` |"); !regexp.MustCompile("(?i)unit check[^|]*`crw.unitCheck`|`crw.unitCheck`[^|]*unit check").MatchString(row) {
		problems = append(problems, "SP-EXEC-1 does not say the run judges a node by its unit check (crw.unitCheck)")
	}
	return problems
}

// flowPageUnitCheckProblems judges the canon's flow page: the ownership table's audit row and step 3 of the C4 procedure name the
// unit check, and step 3 puts it before the commit.
func flowPageUnitCheckProblems(section string) []string {
	var problems []string
	audit := lineWithPrefix(section, "| Audit |")
	if !regexp.MustCompile(`(?i)unit check`).MatchString(audit) {
		problems = append(problems, "the Audit row does not name the unit check: "+audit)
	}
	step := lineWithPrefix(section, "3. ")
	if !regexp.MustCompile(`(?i)unit check[^.]*before[^.]*commit`).MatchString(step) {
		problems = append(problems, "step 3 of the C4 procedure is not the unit check before the commit: "+step)
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

// CRW-1468: land-head judges the head it lands by one unit check in the committed mode (crw.unitCheck with opts.commit and opts.base:
// the review tool run in Go over base..head of the land checkout, no reviewer call). A head made outside a run has no executor, so the
// check records the receipt no_executor; a check that finds a blocking defect ends the land land_review_needs_changes with its finding
// ids carried, and a check the tool gave no judgment for (quota, no answer) holds with source unit_check and keeps the release. The
// pages a reader takes land-head from say so: script.md's "Landing a head made outside a run", crw-run's account of a change made
// another way, and the land-head entry of the flow command table.

const landHeadJS = "internal/flow/workflows/land-head.js"

// landHeadCallsTheCommittedCheck is whether land-head.js calls crw.unitCheck with a commit and a base, the form the pages describe.
func landHeadCallsTheCommittedCheck(script string) bool {
	return regexp.MustCompile(`crw\.unitCheck\([^)]*\bcommit:[^)]*\bbase:`).MatchString(script)
}

// landHeadSectionUnitCheckProblems judges script.md's section on landing a head made outside a run.
func landHeadSectionUnitCheckProblems(script string) []string {
	section := sectionOf(script, "Landing a head made outside a run")
	if section == "" {
		return []string{"script.md has no section \"Landing a head made outside a run\""}
	}
	flat := normalized(section)
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		"`crw.unitCheck\\(",
		`(?i)committed mode`,
		"`base\\.\\.head`",
		`(?i)no reviewer call`,
		"`no_executor`",
		"`land_review_needs_changes`[^.]*carried",
		"source `unit_check`",
		`(?i)resume[^.]*asks the tool again`,
	}) {
		problems = append(problems, "the land-head section lacks "+p)
	}
	for _, p := range []string{"`crw.review`", "fresh reviewer", "a reviewer the host cannot run"} {
		if strings.Contains(flat, p) {
			problems = append(problems, "the land-head section still names the reviewer path: "+p)
		}
	}
	return problems
}

// landHeadLineUnitCheckProblems judges one line that names what land-head runs: crw-run's account of a change made another way, or
// the land-head entry of the flow command table.
func landHeadLineUnitCheckProblems(line string) []string {
	var problems []string
	if line == "" {
		return []string{"there is no line naming land-head"}
	}
	if !regexp.MustCompile("(?i)unit check in the committed mode[^;)]*`?base\\.\\.head`?").MatchString(line) {
		problems = append(problems, "the land-head line does not name the committed-mode unit check of base..head")
	}
	if regexp.MustCompile(`(?i)(another|different) model family`).MatchString(line) {
		problems = append(problems, "the land-head line still requires a different model family")
	}
	if !regexp.MustCompile(`(?i)separate tool invocation`).MatchString(line) {
		problems = append(problems, "the land-head line does not identify the separate tool invocation")
	}
	return problems
}

// lineContaining is the first line of text that contains every one of the substrings.
func lineContaining(text string, subs ...string) string {
	for _, line := range lines(text) {
		ok := true
		for _, s := range subs {
			if !strings.Contains(line, s) {
				ok = false
				break
			}
		}
		if ok {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func TestUnitCheckDocsNameTheLandHeadCommittedCheck(t *testing.T) {
	if !landHeadCallsTheCommittedCheck(readRepoText(t, landHeadJS)) {
		t.Errorf("%s does not call crw.unitCheck with a commit and a base", landHeadJS)
	}
	if p := landHeadSectionUnitCheckProblems(readRepoText(t, scriptDocPath)); len(p) > 0 {
		t.Errorf("%s:\n  %s", scriptDocPath, strings.Join(p, "\n  "))
	}
	if p := landHeadLineUnitCheckProblems(lineContaining(readRepoText(t, runSkillFile), "A change made another way lands", "land-head")); len(p) > 0 {
		t.Errorf("%s:\n  %s", runSkillFile, strings.Join(p, "\n  "))
	}
	if p := landHeadLineUnitCheckProblems(lineContaining(readRepoText(t, flowRef), "| `crw flow start WORKFLOW", "`land-head` lands")); len(p) > 0 {
		t.Errorf("%s:\n  %s", flowRef, strings.Join(p, "\n  "))
	}

	t.Run("fixture: the reviewer-path wording is reported", func(t *testing.T) {
		old := "## Landing a head made outside a run\n\n- The gates are the lane's: `crw.verify` of that head, `crw.review` with a fresh reviewer whose model family\n" +
			"  has to differ from every author, and `crw.land`. A hold (a verifier or a reviewer the host cannot run now) keeps the release.\n\n## Next\n"
		got := strings.Join(landHeadSectionUnitCheckProblems(old), "\n")
		for _, want := range []string{"crw.unitCheck", "committed mode", "no reviewer call", "no_executor", "land_review_needs_changes", "source `unit_check`", "asks the tool again", "still names the reviewer path: `crw.review`", "fresh reviewer", "a reviewer the host cannot run"} {
			if !strings.Contains(got, want) {
				t.Errorf("no problem names %q:\n%s", want, got)
			}
		}
		if p := landHeadLineUnitCheckProblems("`land-head` lands a head (verification, a review by another model family than the authors, acceptance and integration)"); len(p) != 3 {
			t.Errorf("the old land-head line gives %d problems, want 3: %v", len(p), p)
		}
		if p := landHeadLineUnitCheckProblems("`land-head` lands a head (verification, a unit check in the committed mode over `base..head` in a separate tool invocation, acceptance and integration)"); len(p) != 0 {
			t.Errorf("the new land-head line gives problems: %v", p)
		}
		if landHeadCallsTheCommittedCheck("await crw.review(node, head, digest, call)") || !landHeadCallsTheCommittedCheck("crw.unitCheck(node, null, { commit: head, base: head.base })") {
			t.Error("landHeadCallsTheCommittedCheck does not tell the committed call from the review")
		}
	})
}

// CRW-1517: the unit check is a Codex review by default (`codex review --uncommitted` over the executor's uncommitted work,
// `codex review --base <base>` in the committed mode of land-head), with Bugbot as the fallback when the Codex quota is spent; the
// model and effort are the lane's settings CRW_UNIT_CHECK_MODEL and CRW_UNIT_CHECK_EFFORT. Codex is given no prompt, so the executor
// receipt is read and kept but not handed to it (the row's receipt `handed`). The pages that describe the unit check say so, and none
// says the unit check is Bugbot's.

var (
	codexByDefault = `(?i)codex review[^.;]*by default|by default[^.;]*codex review`
	bugbotOnQuota  = `(?i)bugbot[^.;]*codex quota|codex quota[^.;]*bugbot`
	// bugbotIsTheCheck is a wording that makes Bugbot the unit check itself.
	bugbotIsTheCheck = regexp.MustCompile(`(?i)bugbot unit check|unit check (is|by|of|through) bugbot|runs bugbot|bugbot 단위 점검|the bugbot (check|review) of (each|the) node`)
)

// codexDefaultProblems is what text (one region of a page, named where) lacks of the Codex default and the Bugbot fallback, and
// any wording in it that makes Bugbot the unit check.
func codexDefaultProblems(where, text string, extra ...string) []string {
	if strings.TrimSpace(text) == "" {
		return []string{where + " is missing"}
	}
	flat := normalized(text)
	var problems []string
	for _, p := range wordingMissing(flat, append([]string{codexByDefault, bugbotOnQuota}, extra...)) {
		problems = append(problems, where+" lacks "+p)
	}
	if m := bugbotIsTheCheck.FindString(flat); m != "" {
		problems = append(problems, where+" makes Bugbot the unit check: "+m)
	}
	return problems
}

// scriptCodexDefaultProblems judges script.md: the project lane's unit-check paragraph and the land-head section.
func scriptCodexDefaultProblems(script string) []string {
	problems := codexDefaultProblems("the project lane's unit check paragraph", boldParagraph(sectionOf(script, "The project lane"), "Unit check."),
		"`codex review --uncommitted`", "`CRW_UNIT_CHECK_MODEL`", "`CRW_UNIT_CHECK_EFFORT`", `(?i)given no prompt`, "`handed`")
	land := sectionOf(script, "Landing a head made outside a run")
	problems = append(problems, codexDefaultProblems("the land-head section", land, "`codex review --base( <base>)?`")...)
	if strings.Contains(normalized(land), "until that profile is replaced") {
		problems = append(problems, "the land-head section still says the committed profile is to be replaced")
	}
	return problems
}

// runSkillCodexDefaultProblems judges crw-run: its account of how the child works and its land-head line.
func runSkillCodexDefaultProblems(skill string) []string {
	problems := codexDefaultProblems("How the child works", sectionOf(skill, "How the child works"))
	return append(problems, codexDefaultProblems("the land-head line", lineContaining(skill, "A change made another way lands", "land-head"))...)
}

// flowPageCodexDefaultProblems judges the canon's flow page: the Audit row, step 3 of the C4 procedure and the land-head entry of
// the flow command table.
func flowPageCodexDefaultProblems(page string) []string {
	own := ownershipSection(page)
	problems := codexDefaultProblems("the Audit row", lineWithPrefix(own, "| Audit |"))
	problems = append(problems, codexDefaultProblems("step 3 of the C4 procedure", lineWithPrefix(own, "3. "))...)
	return append(problems, codexDefaultProblems("the land-head entry", lineContaining(page, "| `crw flow start WORKFLOW", "`land-head` lands"))...)
}

func TestUnitCheckDocsNameCodexReviewAsTheDefault(t *testing.T) {
	script := readRepoText(t, scriptDocPath)
	skill := readRepoText(t, runSkillFile)
	flow := readRepoText(t, flowRef)
	if p := scriptCodexDefaultProblems(script); len(p) > 0 {
		t.Errorf("%s:\n  %s", scriptDocPath, strings.Join(p, "\n  "))
	}
	if p := runSkillCodexDefaultProblems(skill); len(p) > 0 {
		t.Errorf("%s:\n  %s", runSkillFile, strings.Join(p, "\n  "))
	}
	if p := flowPageCodexDefaultProblems(flow); len(p) > 0 {
		t.Errorf("%s:\n  %s", flowRef, strings.Join(p, "\n  "))
	}
	for path, text := range map[string]string{scriptDocPath: script, runSkillFile: skill, flowRef: flow} {
		if m := bugbotIsTheCheck.FindString(normalized(text)); m != "" {
			t.Errorf("%s makes Bugbot the unit check: %q", path, m)
		}
	}

	t.Run("fixture: the review-tool wording of CRW-1436 is reported", func(t *testing.T) {
		old := "| Audit | the run | runs the unit check of each node's work itself (`crw.unitCheck`, a review tool of another model family than the node's authors, no reviewer agent) |"
		if p := codexDefaultProblems("the Audit row", old); len(p) != 2 {
			t.Errorf("the CRW-1436 Audit row gives %d problems, want 2: %v", len(p), p)
		}
		land := "## Landing a head made outside a run\n\n- one unit check in the committed mode, the review tool run in Go over `base..head`; until that profile is replaced a clean answer fails closed.\n"
		if p := scriptCodexDefaultProblems("## The project lane\n\n**Unit check.** the run runs the review tool itself.\n\n" + land); len(p) != 11 {
			t.Errorf("the CRW-1436 script.md gives %d problems, want 11: %v", len(p), p)
		}
	})
	t.Run("fixture: Bugbot as the unit check is reported", func(t *testing.T) {
		for _, text := range []string{
			"the run runs Bugbot over the worktree, a Codex review by default and Bugbot on a Codex quota",
			"the Bugbot unit check, a Codex review by default and Bugbot on a Codex quota",
			"단위 점검(기본 Codex 리뷰): Bugbot 단위 점검, a Codex review by default and Bugbot on a Codex quota",
		} {
			if p := codexDefaultProblems("x", text); len(p) != 1 || !strings.Contains(p[0], "makes Bugbot the unit check") {
				t.Errorf("%q gives %v, want one Bugbot problem", text, p)
			}
		}
	})
	t.Run("fixture: the Codex wording passes", func(t *testing.T) {
		row := "| Audit | the run | runs the unit check of each node's work itself (`crw.unitCheck`: a Codex review by default, Bugbot on a Codex quota, in a separate tool invocation) |"
		if p := codexDefaultProblems("the Audit row", row); len(p) != 0 {
			t.Errorf("the Codex Audit row gives problems: %v", p)
		}
	})
}
