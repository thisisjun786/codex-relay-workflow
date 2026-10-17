//go:build dev

package ci

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CRW-1418 (REFAC1015 B1-4): the `child` session path is retired. A node the architect sized `child` goes up as a `size_split`
// escalation with detail `child_retired`; the issue-lane workflow, `crw flow apply` and `crw flow start --child-host` are gone.
// The six flow documents that described that path no longer name it, and neither do the two skill pages that sent a node to it: crw-plan,
// whose size fields told planners and architects to give an oversized bundle `size_fit` `child`, and crw-run's task packet, which said a
// node a run releases runs the issue lane (PR #889 round 1). Old records (a crw.releaseChild row, an issue-lane run) stay readable, and
// the pages may say so without the words below. The rest of the crw-run skill, other skills, the known-defects records, the refactor
// backlog and the inventory map are not scanned here: they keep their own history or belong to another issue.

// issueChildPathDocs is the documents that may not name the `child` session path.
var issueChildPathDocs = []string{
	"docs/flow/script.md",
	"docs/flow/reporting.md",
	"docs/flow/journal.md",
	canonRefDir + "flow.md",
	canonRefDir + "hierarchy.md",
	canonRefDir + "spawn-policy.md",
	planSkillFile,
	"plugins/crw/skills/crw-run/references/task-packet.md",
}

// issueChildPathWord is a word of the retired `child` session path ("flow apply" as a word of its own: "the workflow apply to" is not it).
var issueChildPathWord = regexp.MustCompile(`SP-CHILD-[12]|issue-lane|\bflow apply|--child-host|child_host|child-host`)

// issueChildPathProblems is each line of text that names the retired `child` session path, with its line number.
func issueChildPathProblems(text string) []string {
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		for _, word := range issueChildPathWord.FindAllString(line, -1) {
			problems = append(problems, "line "+strconv.Itoa(i+1)+" names "+word)
		}
	}
	return problems
}

func TestFlowDocsHaveNoIssueChildPath(t *testing.T) {
	for _, file := range issueChildPathDocs {
		if problems := issueChildPathProblems(readFlowReference(t, file)); len(problems) > 0 {
			t.Errorf("%s still names the retired `child` session path:\n  %s", file, strings.Join(problems, "\n  "))
		}
	}

	t.Run("fixture: a clean text passes and each word alone is reported", func(t *testing.T) {
		clean := "A node sized `child` goes up as a `size_split` escalation with detail `child_retired`.\n" +
			"An old `crw.releaseChild` row is read as recorded; the `child` role id names an old record kind.\n" +
			"The rules of the workflow apply to both formats.\n"
		if p := issueChildPathProblems(clean); len(p) > 0 {
			t.Errorf("a clean text is reported: %v", p)
		}
		for _, word := range []string{"SP-CHILD-1", "SP-CHILD-2", "issue-lane", "flow apply", "--child-host", "child_host", "child-host"} {
			p := issueChildPathProblems("clean line\nsee " + word + " here\n")
			if len(p) != 1 || !strings.Contains(p[0], word) || !strings.HasPrefix(p[0], "line 2 ") {
				t.Errorf("%q alone: got %v, want one problem on line 2", word, p)
			}
		}
	})
}

// CRW-1426 (N3-1): the skills carry no id of the retired path either. The SP-CHILD-1 tag of the exception path (a `child` session of a plan
// with no flow run) is gone from the five skill pages that quoted it; the path itself stays and stays pinned
// (TestIssueChildDispatchIsOnlyForAPlanWithNoFlowRun). `--child-host` alone is the live `codex-session-relay register` flag, so only the
// `flow start --child-host` form is the retired word; old port records that quote an id are history and are not scanned.

// skillIssueChildPathWord is a word of the retired `child` session path in a skill page.
var skillIssueChildPathWord = regexp.MustCompile(`SP-CHILD-[12]|issue-lane|\bflow apply|flow start\s+--child-host|child_host`)

// skillIssueChildPathProblems is each line of text that names the retired `child` session path, with its line number.
func skillIssueChildPathProblems(text string) []string {
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		for _, word := range skillIssueChildPathWord.FindAllString(line, -1) {
			problems = append(problems, "line "+strconv.Itoa(i+1)+" names "+word)
		}
	}
	return problems
}

// TestSkillsHaveNoIssueChildPath scans every SKILL.md and every references/*.md under plugins/crw/skills.
func TestSkillsHaveNoIssueChildPath(t *testing.T) {
	root := filepath.Join(repoRoot(), "plugins/crw/skills")
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		isSkill := len(parts) == 2 && parts[1] == "SKILL.md"
		isRef := len(parts) >= 3 && parts[1] == "references" && strings.HasSuffix(path, ".md")
		if !isSkill && !isRef {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if problems := skillIssueChildPathProblems(string(data)); len(problems) > 0 {
			t.Errorf("%s still names the retired `child` session path:\n  %s", filepath.ToSlash(rel), strings.Join(problems, "\n  "))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 20 {
		t.Errorf("only %d skill pages scanned; the walk lost the skills tree", scanned)
	}

	t.Run("fixture: a clean text passes and each retired word is reported", func(t *testing.T) {
		clean := "A `child` session of a plan with no flow run is the exception path.\n" +
			"codex-session-relay register --child-task T --child-host H\n" +
			"The rules of the workflow apply to both formats.\n"
		if p := skillIssueChildPathProblems(clean); len(p) > 0 {
			t.Errorf("a clean text is reported: %v", p)
		}
		for _, word := range []string{"SP-CHILD-1", "SP-CHILD-2", "issue-lane", "flow apply", "flow start --child-host", "child_host"} {
			p := skillIssueChildPathProblems("clean line\nsee " + word + " here\n")
			if len(p) != 1 || !strings.Contains(p[0], word) || !strings.HasPrefix(p[0], "line 2 ") {
				t.Errorf("%q alone: got %v, want one problem on line 2", word, p)
			}
		}
	})
}

// CRW-1418 (PR #889 round 2): the `child` session is dispatched only for a plan with no flow run. The places of crw-run and crw-pabcd that
// tell the child to dispatch, reuse, observe or correct a `child` session say so in the same sentence: crw-run's execution request and its
// project-run loop, and the managed lanes of crw-pabcd's dispatch surfaces. A sentence that says no `child` session is started or that one
// never does something needs no condition.
var issueChildDispatchPlaces = []struct{ file, heading string }{
	{runSkillFile, "Determine the requested operation"},
	{runSkillFile, "Keep a project run moving"},
	{"plugins/crw/skills/crw-pabcd/references/dispatch-surfaces.md", "DISPATCH-MANAGED-01 (STRICT) — start from the binding, not from the mechanism"},
}

var (
	// issueChildDispatch is a sentence that sends work to a `child` session, or waits for or reads one: a verb before the `child` session, or
	// the `child` session before a word of its dispatch, observation, result or correction.
	issueChildDispatch = regexp.MustCompile(`(?i)\b(reuse|create|dispatch|launch|start|resume|observe)\w*\b[^.]*\x60child\x60 sessions?\b|\x60child\x60 sessions?\b[^.]*\b(dispatch|observ|run|event|result|correct|start|resum)\w*`)
	// issueChildNegated is a sentence that says no `child` session is started, or that one does not or never does something.
	issueChildNegated = regexp.MustCompile(`(?i)\b(do not|does not|never|no \x60child\x60 sessions?|starts? no \x60child\x60 sessions?)\b`)
	// noFlowRunCondition is the condition of the `child` session's dispatch.
	noFlowRunCondition = regexp.MustCompile(`(?i)\bplan with no flow run\b`)
	// sentenceEnd ends a sentence of normalized text: a period, an exclamation or a question mark followed by a space (a `crw.release`
	// stays whole).
	sentenceEnd = regexp.MustCompile(`[.!?]\s+`)
)

// issueChildDispatchProblems is each sentence of text (a section of a skill page) that dispatches or observes a `child` session and does
// not say it is only for a plan with no flow run.
func issueChildDispatchProblems(text string) []string {
	var problems []string
	for _, paragraph := range regexp.MustCompile(`\n\s*\n|\n\s*- `).Split(text, -1) {
		for _, sentence := range sentenceEnd.Split(normalized(paragraph), -1) {
			if issueChildDispatch.MatchString(sentence) && !issueChildNegated.MatchString(sentence) && !noFlowRunCondition.MatchString(sentence) {
				problems = append(problems, sentence)
			}
		}
	}
	return problems
}

func TestIssueChildDispatchIsOnlyForAPlanWithNoFlowRun(t *testing.T) {
	for _, place := range issueChildDispatchPlaces {
		section := sectionOf(readFlowReference(t, place.file), place.heading)
		if strings.TrimSpace(section) == "" {
			t.Errorf("%s has no section %q", place.file, place.heading)
			continue
		}
		if problems := issueChildDispatchProblems(section); len(problems) > 0 {
			t.Errorf("%s, %q: a `child` session is dispatched without the condition of a plan with no flow run:\n  %s", place.file, place.heading, strings.Join(problems, "\n  "))
		}
	}

	t.Run("fixture: an unqualified dispatch is reported, a qualified or a negated one is not", func(t *testing.T) {
		for _, bad := range []string{
			"Reconcile existing work, reuse/create responsible `child` sessions, verify and integrate their results.",
			"Before any `child` session dispatch, record its observation path.",
			"- a `child` session (a code id, not a layer), started and resumed through `crw-run`: the exception path.",
		} {
			if p := issueChildDispatchProblems("clean line.\n\n" + bad + "\n"); len(p) != 1 {
				t.Errorf("%q: got %v, want one problem", bad, p)
			}
		}
		for _, good := range []string{
			"Only for a plan with no flow run, reuse/create responsible `child` sessions and verify their results.",
			"Bind only; do not dispatch `child` sessions or create a goal.",
			"A node sized for a child starts no `child` session: the lane escalates it.",
			"Return the outcome from here: a `child` session never answers another project's child.",
		} {
			if p := issueChildDispatchProblems(good); len(p) != 0 {
				t.Errorf("%q is reported: %v", good, p)
			}
		}
	})
}

// CRW-1464 (S-4, bundle CRW-1425): the code of the managed `child` session start is gone from the flow, not only from its docs. No
// non-test Go file under internal/flow holds SchedRelay.ReleaseChild, the host settings document of cli/childhost.go (ChildHost and
// its schema and refusal), or the ops.ChildRequest and ops.ChildResult types: the lane releases no `child` session since CRW-1415, and a
// node a DESIGN recorded before S-1 sized for a child is an unreleased sized row (crw.release) that the lane escalates as size_split.
//
// What is read. Each Go token of a non-test file under internal/flow (testdata directories skipped): an identifier holds a word when
// the word is part of its name (ErrChildHostUnset holds ChildHost), and a comment or a string literal holds a word on word boundaries.
// The word "childhost" is matched in any case, so a file name or a lower-case spelling counts too.
//
// What stays. The op name of old journal rows is read as recorded: ops.OpReleaseChild (ops/lane.go, read by cli/runs.go and the
// legacy readers) and engine.OpLegacyReleaseChild (engine/legacy.go, the detection of a legacy run). Those two identifiers are the
// allowlist (goChildPathKept); an allowed identifier that no file names any more is stale and fails, so the list only shrinks.

// goChildPathWord is a word of the deleted managed start in an identifier (part of its name).
var goChildPathWord = regexp.MustCompile(`ReleaseChild|ChildHost|ChildRequest|ChildResult|(?i:childhost)`)

// goChildPathTextWord is a word of the deleted managed start in a comment or a string literal (on word boundaries).
var goChildPathTextWord = regexp.MustCompile(`\b(?:ReleaseChild|ChildHost|ChildRequest|ChildResult|(?i:childhost))\b`)

// goChildPathKept are the identifiers that keep a word, each with its reason.
var goChildPathKept = map[string]string{
	"OpReleaseChild":       "the op name of an old crw.releaseChild row (ops/lane.go), which cli/runs.go and the legacy readers read as recorded",
	"OpLegacyReleaseChild": "the engine's name of that op (engine/legacy.go), by which a legacy run is detected and refused",
}

// goChildPathHits is each place of src (a Go source file) that holds a word, as "line N: kind text"; kept collects the allowed
// identifiers it names.
func goChildPathHits(name string, src []byte, kept map[string]bool) []string {
	fset := token.NewFileSet()
	file := fset.AddFile(name, fset.Base(), len(src))
	var s scanner.Scanner
	var problems []string
	s.Init(file, src, func(pos token.Position, msg string) { problems = append(problems, pos.String()+": "+msg) }, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		line := fset.Position(pos).Line
		switch {
		case tok == token.IDENT && goChildPathWord.MatchString(lit):
			if _, ok := goChildPathKept[lit]; ok {
				kept[lit] = true
				continue
			}
			problems = append(problems, "line "+strconv.Itoa(line)+": identifier "+lit)
		case tok == token.COMMENT || tok == token.STRING:
			for _, word := range goChildPathTextWord.FindAllString(lit, -1) {
				problems = append(problems, "line "+strconv.Itoa(line)+": "+strings.ToLower(tok.String())+" names "+word)
			}
		}
	}
	return problems
}

func TestNoIssueChildGoSourceRemains(t *testing.T) {
	root := repoRoot()
	kept := map[string]bool{}
	scanned := 0
	var report []string
	err := filepath.WalkDir(filepath.Join(root, "internal", "flow"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.Contains(strings.ToLower(filepath.Base(rel)), "childhost") {
			report = append(report, rel+": the file of the deleted managed start")
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, p := range goChildPathHits(rel, src, kept) {
			report = append(report, rel+" "+p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("only %d Go files scanned under internal/flow; the walk lost the tree", scanned)
	}
	if len(report) > 0 {
		t.Errorf("the managed `child` session start is still in the flow's Go source:\n  %s", strings.Join(report, "\n  "))
	}
	for id, reason := range goChildPathKept {
		if !kept[id] {
			t.Errorf("the kept identifier %s (%s) is named by no file under internal/flow any more: drop it from goChildPathKept", id, reason)
		}
	}

	t.Run("fixture: kept identifiers pass and each word is reported", func(t *testing.T) {
		clean := "package p\n\n// an old crw.releaseChild row is read as recorded; a `child` session is a relay role id\n" +
			"const OpReleaseChild = OpLegacyReleaseChild\n\nvar childTitle = \"crw child\"\n"
		k := map[string]bool{}
		if p := goChildPathHits("clean.go", []byte(clean), k); len(p) > 0 {
			t.Errorf("a clean file is reported: %v", p)
		}
		if !k["OpReleaseChild"] || !k["OpLegacyReleaseChild"] {
			t.Errorf("the kept identifiers were not seen: %v", k)
		}
		for _, src := range []string{
			"package p\n\nfunc ReleaseChild() {}\n",
			"package p\n\nvar ErrChildHostUnset = 1\n",
			"package p\n\ntype ChildRequest struct{}\n",
			"package p\n\ntype ChildResult struct{}\n",
			"package p\n\n// see ReleaseChild\n",
			"package p\n\n// kept in childhost.go\n",
			"package p\n\nvar s = \"the ChildHost document\"\n",
		} {
			if p := goChildPathHits("bad.go", []byte(src), map[string]bool{}); len(p) != 1 || !strings.HasPrefix(p[0], "line 3: ") {
				t.Errorf("%q: got %v, want one problem on line 3", src, p)
			}
		}
	})
}
