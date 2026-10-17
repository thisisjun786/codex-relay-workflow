//go:build dev

package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The orchestration skills name their actor and their one job (CRW-1219): crw-continue runs one task, and a milestone
// request goes to crw-run; the parent exclusion of crw-continue and crw-work is one pointer line to crw-canon/hierarchy.md;
// crw-work gives up the plan and requirements triggers to the skills that own those jobs; crw-refactor diagnoses and
// crw-tidy separates its diagnosis from its writes; the numbered plan documents of crw-work are workspace-local evidence
// that quotes the tracking canon. The inventory map rows of owner S04 point at these checks.

const (
	loopSkillFile      = "plugins/crw/skills/crw-continue/SKILL.md"
	pabcdSkillFile     = "plugins/crw/skills/crw-work/SKILL.md"
	refactorSkillFile  = "plugins/crw/skills/crw-refactor/SKILL.md"
	refactorRecordFile = "plugins/crw/skills/crw-refactor/references/candidate-record.md"
	tidySkillFile      = "plugins/crw/skills/crw-tidy/SKILL.md"
	tidySurfacesFile   = "plugins/crw/skills/crw-tidy/references/surfaces.md"
	uiuxSkillFile      = "plugins/crw/skills/crw-dev-uiux-design/SKILL.md"
	planSkillFile      = "plugins/crw/skills/crw-plan/SKILL.md"
	defineSkillFile    = "plugins/crw/skills/crw-define/SKILL.md"
	hierarchyLink      = "../crw-canon/references/hierarchy.md"
)

// flat is text with every run of white space made one space, so a phrase wrapped over lines is still found.
func flat(text string) string { return strings.Join(strings.Fields(text), " ") }

// CRW-1526 renames the instructional surface, not hook registrations, stored
// paths, or source identifiers. Separately owned documents keep their spellings
// until their owner migrates them.
func TestTaskWorkSkillNames1526(t *testing.T) {
	root := filepath.Join(repoRoot(), "plugins", "crw", "skills")
	retired := regexp.MustCompile(`(?i)\b(?:ipabcd|pabcd|crw-loop)\b`)
	codePath := regexp.MustCompile(`(?:internal/pabcd|(?:components/)?pabcd-state)/[A-Za-z0-9_./-]+`)
	hookNames := strings.NewReplacer(
		"session-start-bootstrapping-pabcd-state", "HOOK",
		"user-prompt-submit-checking-pabcd-trigger", "HOOK",
		"stop-checking-pabcd-continuation", "HOOK",
		"bootstrapping-pabcd-state", "HOOK",
		"checking-pabcd-trigger", "HOOK",
		"checking-pabcd-continuation", "HOOK",
		"pabcd-state", "HOOK",
	)
	// These exact existing storage/hook sentences are frozen by the task brief
	// pending CRW-1529/1532. Strip only their bytes, not an entire surrounding
	// line: an additional retired name on that line must still fail.
	preserved := map[string]string{
		"crw-work/SKILL.md":                    "State lives in `.crw/sessions/<sessionId>.json` plus `.crw/ledger.jsonl`; transitions are driven by the `pabcd-state` hook component, the chat-side `orchestrate` command (human free-pass), and the live `crw pabcd orchestrate` terminal CLI (agent-gated).",
		"crw-run/references/bridge.md":         "A task's PABCD state lives in the cwd the host reports for it (`<cwd>/.crw/sessions/<thread id>.json`), so a resume at another cwd would open an empty IDLE state machine beside the work it left.",
		"crw-work/references/phase-control.md": "`PABCD: <phase> (<LABEL>)`. Injected footer values are prompt-time snapshots.",
	}
	formerly := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == "crw-status" || rel == "crw-check" {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "crw-canon/references/naming.md" ||
			rel == "crw-canon/references/flow/preview.md" ||
			rel == "crw-run/references/merge-readiness.md" {
			return nil
		}
		if retired.MatchString(rel) {
			t.Errorf("%s: retired skill directory/name", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
			return nil
		}
		parentMerge := false
		recordedCases := false
		for n, line := range strings.Split(string(data), "\n") {
			if rel == "crw-run/references/operations/scenarios.md" && strings.HasPrefix(line, "## ") {
				parentMerge = line == "## S24 Parent coordination finishes without a local implementation diff"
			}
			if rel == "crw-run/references/dispatch-verification.md" && strings.HasPrefix(line, "## ") {
				// Generated historical cases retain the names they were
				// recorded under; migrate their data, never this rendering.
				recordedCases = line == "## Recorded cases"
			}
			if parentMerge || recordedCases {
				continue
			}
			formerly += strings.Count(line, "(formerly PABCD)")
			line = strings.ReplaceAll(line, "(formerly PABCD)", "")
			if sentence, ok := preserved[rel]; ok {
				line = strings.ReplaceAll(line, sentence, "")
			}
			line = codePath.ReplaceAllString(line, "SOURCE")
			line = hookNames.Replace(line)
			if retired.MatchString(line) {
				t.Errorf("%s:%d: retired task-work name: %s", rel, n+1, line)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if formerly > 1 {
		t.Errorf("found %d '(formerly PABCD)' explanations, want at most one", formerly)
	}
}

// skillBody is the text of a SKILL.md after its frontmatter.
func skillBody(t *testing.T, rel string) string {
	t.Helper()
	text := splitRead(t, rel)
	end := strings.Index(text[4:], "\n---\n")
	if !strings.HasPrefix(text, "---\n") || end < 0 {
		t.Fatalf("%s has no frontmatter", rel)
	}
	return text[end+4+5:]
}

// descriptionOf is the frontmatter description of the SKILL.md at rel.
func descriptionOf(t *testing.T, rel string) string {
	t.Helper()
	fields, err := frontmatterFields(splitRead(t, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return fields["description"]
}

func repoDescriptions(t *testing.T) map[string]string {
	t.Helper()
	descriptions, err := skillDescriptions(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	return descriptions
}

// TestLoopAndPabcdParentExclusionIsOnePointer: the paragraphs that kept the project parent out of the crw-continue and
// crw-work procedure are one line each that points to the hierarchy; crw-continue's description is one task and sends a milestone
// request to crw-run.
func TestLoopAndPabcdParentExclusionIsOnePointer(t *testing.T) {
	for _, file := range []string{loopSkillFile, pabcdSkillFile} {
		body := skillBody(t, file)
		for _, gone := range []string{"parent never follows", "never follows this procedure", "creates no implementation"} {
			if strings.Contains(flat(body), gone) {
				t.Errorf("%s still states the parent exclusion in its own words (%q)", file, gone)
			}
		}
		var pointer []int
		for i, line := range strings.Split(body, "\n") {
			if strings.Contains(line, hierarchyLink) {
				pointer = append(pointer, i+1)
			}
		}
		if len(pointer) != 1 {
			t.Errorf("%s links %s on lines %v of the body, want exactly one pointer line", file, hierarchyLink, pointer)
		}
	}
	description := repoDescriptions(t)["crw-continue"]
	if !strings.Contains(description, "one task") || !strings.Contains(description, "crw-run") {
		t.Errorf("the crw-continue description does not say it runs one task and that a milestone request goes to crw-run: %q", description)
	}
	if prompt := splitRead(t, "plugins/crw/skills/crw-continue/agents/openai.yaml"); strings.Contains(prompt, "goal mode") {
		t.Error("the crw-continue default prompt names crw-run goal mode, which crw-run owns")
	}
}

// TestPabcdHasNoPlanTriggers: crw-work's description no longer lists the plan and requirements triggers; the skill that owns
// each job lists it.
func TestPabcdHasNoPlanTriggers(t *testing.T) {
	pabcd := descriptionOf(t, pabcdSkillFile)
	for _, phrase := range []string{"plan this", "기획", "요구사항 정리", "requirements"} {
		if strings.Contains(strings.ToLower(pabcd), phrase) {
			t.Errorf("the crw-work description still lists the trigger %q: %q", phrase, pabcd)
		}
	}
	for file, phrases := range map[string][]string{
		planSkillFile:   {"기획", "roadmap"},
		defineSkillFile: {"마일스톤 정의", "요구사항 정리"},
	} {
		description := descriptionOf(t, file)
		for _, phrase := range phrases {
			if !strings.Contains(strings.ToLower(description), phrase) {
				t.Errorf("the description in %s lacks the trigger %q: %q", file, phrase, description)
			}
		}
	}
}

// TestPabcdPlanDocsAreLocalEvidence: implementation-units.md and phase-control.md write a numbered plan document as
// workspace-local execution evidence that quotes the tracking canon, and say nowhere that devlog is the canon of a product
// decision. The plan-document format has one link target, implementation-units.md.
func TestPabcdPlanDocsAreLocalEvidence(t *testing.T) {
	issueFree := regexp.MustCompile(`(?i)\b(no|without|has no|have no|lacks?)\b[^.]{0,30}\btracking (issue|record)`)
	canonical := regexp.MustCompile(`(?i)devlog[^.]{0,120}\b(canonical|source of truth|SoT)\b|\b(canonical|source of truth|SoT)\b[^.]{0,120}devlog`)
	for _, file := range []string{
		"plugins/crw/skills/crw-work/references/implementation-units.md",
		"plugins/crw/skills/crw-work/references/phase-control.md",
	} {
		text := flat(splitRead(t, file))
		if !strings.Contains(text, "workspace-local execution evidence") {
			t.Errorf("%s does not write the numbered plan documents as workspace-local execution evidence", file)
		}
		if !strings.Contains(text, "quotes the tracking") {
			t.Errorf("%s does not say that a plan document quotes the tracking canon", file)
		}
		if m := canonical.FindString(text); m != "" {
			t.Errorf("%s makes devlog a canon: %q", file, m)
		}
	}
	// Standalone task work has no tracking issue and no CRW execution binding (crw-continue says so); a plan document of that work
	// cannot quote an issue, so each file gives it another source and creates no issue for the citation.
	if loop := flat(splitRead(t, loopSkillFile)); !issueFree.MatchString(loop) {
		t.Fatalf("%s no longer allows standalone task work without a tracking issue; revisit this check", loopSkillFile)
	}
	for _, file := range []string{
		"plugins/crw/skills/crw-work/references/implementation-units.md",
		"plugins/crw/skills/crw-work/references/phase-control.md",
	} {
		text := flat(splitRead(t, file))
		sentence := ""
		for _, s := range strings.SplitAfter(text, ". ") {
			if issueFree.MatchString(s) {
				sentence = s
				break
			}
		}
		if sentence == "" {
			t.Errorf("%s requires the tracking citation of every plan document and has no rule for work with no tracking issue", file)
			continue
		}
		if !regexp.MustCompile(`(?i)request|repository`).MatchString(sentence) {
			t.Errorf("%s: the rule for work with no tracking issue names no other source: %q", file, sentence)
		}
		if regexp.MustCompile(`(?i)\b(create|open|invent)s?\b[^.]{0,40}\bissue`).MatchString(sentence) && !regexp.MustCompile(`(?i)\b(no|not|never|without)\b[^.]{0,30}\b(create|open|invent)|\b(create|open|invent)s?\s+(no|not)\b`).MatchString(sentence) {
			t.Errorf("%s asks work with no tracking issue to create one: %q", file, sentence)
		}
	}
	if !strings.Contains(flat(splitRead(t, "plugins/crw/skills/crw-work/references/phase-control.md")), "tracking-holds-canonical-documents") {
		t.Error("phase-control.md does not link the rule that the tracking repository holds the canonical documents")
	}
	for _, file := range []string{pabcdSkillFile, loopSkillFile, "plugins/crw/skills/crw-work/references/phase-control.md"} {
		text := splitRead(t, file)
		if strings.Contains(text, "implementation-log.md") {
			t.Errorf("%s links the implementation log as a plan-document format; implementation-units.md is the one target", file)
		}
	}
	for _, file := range []string{pabcdSkillFile, loopSkillFile} {
		if !strings.Contains(splitRead(t, file), "implementation-units.md") {
			t.Errorf("%s does not link implementation-units.md", file)
		}
	}
}

// TestRefactorHasNoWriteProcedure: crw-refactor diagnoses, read-only, for the parent and the top session; a selected
// repair becomes a tracking issue through crw-add-issue and runs on the ordinary project path. The skill holds no execution
// procedure and no crw-continue run.
func TestRefactorHasNoWriteProcedure(t *testing.T) {
	skill := splitRead(t, refactorSkillFile)
	record := splitRead(t, refactorRecordFile)
	text := flat(skill)
	for _, want := range []string{
		"read-only",
		"the parent",
		"the top session",
		"](../crw-add-issue/SKILL.md)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s lacks %q", refactorSkillFile, want)
		}
	}
	for _, gone := range []string{
		"crw-continue",
		"Implement the smallest agreed repair",
		"direct implementation",
		"Implement in this task",
		"execute selected candidates",
		"A new task requires authorization",
		"preserving the issue/PR mapping",
	} {
		if strings.Contains(text, gone) {
			t.Errorf("%s still carries the execution procedure (%q)", refactorSkillFile, gone)
		}
	}
	for _, heading := range headingsOf(skill) {
		if lower := strings.ToLower(heading); strings.Contains(lower, "into execution") || strings.Contains(lower, "implement") {
			t.Errorf("%s keeps the heading %q: an execution procedure is not this skill's", refactorSkillFile, heading)
		}
	}
	if handoff := flat(sectionOf(skill, "Hand a selected candidate to the project path")); !strings.Contains(handoff, "](../crw-add-issue/SKILL.md)") || !strings.Contains(handoff, "crw-run") {
		t.Errorf("%s has no section that hands a selected candidate to crw-add-issue and the project path through crw-run", refactorSkillFile)
	}
	// The callers of crw-add-issue are the child, which registers, the parent, which hands a drafted body to it, and the
	// user. The parent selecting a repair is therefore not a registrant: the handoff must route the drafted issue to the
	// child of the project that owns the code.
	callers := flat(sectionOf(splitRead(t, "plugins/crw/skills/crw-add-issue/SKILL.md"), "Who calls"))
	if !regexp.MustCompile(`(?i)child[^.]*regist`).MatchString(callers) {
		t.Fatal("crw-add-issue no longer names the child as its registering caller; revisit this check")
	}
	handoff := flat(sectionOf(skill, "Hand a selected candidate to the project path"))
	routed := false
	for _, s := range strings.SplitAfter(handoff, ". ") {
		lower := strings.ToLower(s)
		if !strings.Contains(lower, "the parent") {
			continue
		}
		if !strings.Contains(lower, "child") || !regexp.MustCompile(`regist|hand`).MatchString(lower) {
			t.Errorf("%s: a sentence puts the parent on crw-add-issue without the child that registers the issue: %q", refactorSkillFile, s)
			continue
		}
		routed = true
	}
	if !routed {
		t.Errorf("%s does not hand the drafted issue of a parent-origin repair to the project's child", refactorSkillFile)
	}
	for _, gone := range []string{"direct implementation owner", "explicitly chosen by the user"} {
		if strings.Contains(flat(record), gone) {
			t.Errorf("%s still names an in-skill execution owner (%q)", refactorRecordFile, gone)
		}
	}
	if description := repoDescriptions(t)["crw-refactor"]; strings.Contains(strings.ToLower(description), "execute") {
		t.Errorf("the crw-refactor description offers execution: %q", description)
	}
}

// TestTidyJobsSeparated: crw-tidy has two jobs under two headings. Diagnose reads and is open to any actor; Supplement writes,
// and only the owner of the body written does: the parent for a milestone description it opened, the child for a project or issue body, and
// the parent's store-wide sweep sends each supplement to its owner.
func TestTidyJobsSeparated(t *testing.T) {
	skill := splitRead(t, tidySkillFile)
	count := map[string]int{}
	for _, heading := range headingsOf(skill) {
		count[heading]++
	}
	for _, heading := range []string{"Diagnose", "Supplement"} {
		if count[heading] != 1 {
			t.Errorf("%s has %d headings %q, want exactly one", tidySkillFile, count[heading], heading)
		}
	}
	diagnose := flat(sectionOf(skill, "Diagnose"))
	supplement := flat(sectionOf(skill, "Supplement"))
	for _, want := range []string{"read-only", "any actor"} {
		if !strings.Contains(diagnose, want) {
			t.Errorf("the Diagnose section lacks %q", want)
		}
	}
	// Diagnose names the surfaces it judges and the dispositions; it never describes a write step.
	for _, gone := range []string{"read it back", "Read the record again immediately before the write", "write authority"} {
		if strings.Contains(diagnose, gone) {
			t.Errorf("the Diagnose section carries write procedure (%q)", gone)
		}
	}
	for _, want := range []string{
		"the parent for a milestone description it opened",
		"the child for a project or issue body",
		"store-wide sweep",
		"Read the record again immediately before the write",
	} {
		if !strings.Contains(supplement, want) {
			t.Errorf("the Supplement section lacks %q", want)
		}
	}
	// hierarchy.md gives each tracking body to one layer and the top session none, so the parent's store-wide sweep is requested
	// through crw manage and sends every supplement to the owner of its body; it writes no other layer's record itself.
	hierarchy := flat(splitRead(t, "plugins/crw/skills/crw-canon/references/hierarchy.md"))
	if !strings.Contains(hierarchy, "the initiative, project and issue records belong to the layers below it") {
		t.Fatal("hierarchy.md no longer keeps the initiative, project and issue records below the top session; revisit this check")
	}
	sweepBullet := ""
	for _, line := range strings.Split(sectionOf(skill, "Supplement"), "\n") {
		if strings.HasPrefix(line, "- ") && strings.Contains(line, "store-wide sweep") {
			sweepBullet = flat(line)
		}
	}
	if sweepBullet == "" {
		t.Error("the Supplement section does not say what the parent's store-wide sweep does")
	} else if !strings.Contains(sweepBullet, "crw manage") || !strings.Contains(sweepBullet, "owner") || !regexp.MustCompile(`send|hand|route`).MatchString(sweepBullet) || !strings.Contains(sweepBullet, "writes none itself") {
		t.Errorf("the parent's store-wide sweep is not routed through crw manage to the owner of each body: %q", sweepBullet)
	}
	// The write order, the read-back and the authority rule are Supplement's, so they appear once, there.
	if got := strings.Count(flat(skill), "Read the record again immediately before the write"); got != 1 {
		t.Errorf("the pre-write read is stated %d times, want once (in Supplement)", got)
	}
	if !strings.Contains(flat(splitRead(t, tidySurfacesFile)), "Supplement") {
		t.Errorf("%s does not say that its last column belongs to the Supplement job", tidySurfacesFile)
	}
	description := repoDescriptions(t)["crw-tidy"]
	if !strings.Contains(description, "Diagnose") || !strings.Contains(description, "Supplement") {
		t.Errorf("the crw-tidy description does not name its two jobs: %q", description)
	}
}

// TestQuestionEntryPoints: no skill owns an interview or a catalog discovery since CRW-1340 removed crw-interview;
// crw-define settles a milestone and its issues before registration and says where a run's ambiguity goes, and the
// uiux-design intent protocol asks its caller, or records an assumption when no user is present.
func TestQuestionEntryPoints(t *testing.T) {
	descriptions := repoDescriptions(t)
	if _, ok := descriptions["crw-interview"]; ok {
		t.Error("crw-interview still has a SKILL.md")
	}
	for name, description := range descriptions {
		for _, phrase := range []string{"catalog discovery", "clarifying"} {
			if strings.Contains(strings.ToLower(description), phrase) {
				t.Errorf("the %s description still lists %q: %q", name, phrase, description)
			}
		}
	}
	define := flat(skillBody(t, defineSkillFile))
	if !strings.Contains(define, "decision request") || !strings.Contains(define, "crw-run") {
		t.Error("crw-define does not send a run's ambiguity to the run's decision request")
	}
	uiux := splitRead(t, uiuxSkillFile)
	if !strings.Contains(uiux, "ask the caller, or record an assumption when no user is present") {
		t.Errorf("%s does not state the intent protocol as ask the caller, or record an assumption when no user is present", uiuxSkillFile)
	}
	if strings.Contains(uiux, "ask ONE best clarifying fork") {
		t.Errorf("%s still has the intent protocol ask the user directly", uiuxSkillFile)
	}
}

// TestOrchestrationDescriptionsAreOneJob: the six descriptions of triggerAmbiguity[13] are at most 300 characters and name one
// job; the length itself is TestSkillDescriptionLength's, and this keeps the neighbours' pointers.
func TestOrchestrationDescriptionsAreOneJob(t *testing.T) {
	descriptions := repoDescriptions(t)
	for skill, neighbours := range map[string][]string{
		"crw-plan":     {"crw-define", "crw-run"},
		"crw-define":   {"crw-plan"},
		"crw-check":    {"crw-run"},
		"crw-tidy":     {"crw-check", "crw-plan"},
		"crw-refactor": {"crw-add-issue"},
	} {
		for _, neighbour := range neighbours {
			if !strings.Contains(descriptions[skill], neighbour) {
				t.Errorf("the %s description does not send the neighbouring job to %s: %q", skill, neighbour, descriptions[skill])
			}
		}
	}
}
