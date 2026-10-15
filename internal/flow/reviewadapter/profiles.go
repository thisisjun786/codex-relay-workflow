package reviewadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// Profile is one review tool: how to run it, which model and effort it runs, the texts that mean its quota is spent, and the
// reader that turns its stdout into a document of AnswerSchema. Family is the engine family when the model id does not name it
// (empty: execution.ModelFamily of Model); the record names it in canonical form (execution.CanonicalFamily), and Check refuses a
// profile whose family is blank after the trim or no family name.
type Profile struct {
	Name       string
	Argv       []string
	Model      string
	Effort     string
	Family     string
	QuotaTexts []string
	// Answer reads the tool's stdout for the request it answers: a reader of a committed range (Ranged) reads the range from the
	// request (Request.Base, Request.Head) and takes only an answer about it.
	Answer func(stdout []byte, req Request) ([]byte, error)
	// Ranged is set for a profile that judges a committed range: Check refuses a request that names no range (Base and Head).
	Ranged bool
}

// JSONProfile is a tool that prints the answer document itself.
func JSONProfile(name string, argv []string, model, effort string) Profile {
	return Profile{Name: name, Argv: argv, Model: model, Effort: effort, Answer: func(stdout []byte, _ Request) ([]byte, error) { return stdout, nil }}
}

// The model Bugbot runs and its effort word (the effort is the one the model id carries; an assumption until the tool reports it).
const (
	bugbotModel  = "grok-4.7-medium"
	bugbotEffort = "medium"
)

// BugbotProfile is Cursor's Bugbot through cursor-agent in ask mode, which prints one JSON envelope whose result is prose. An
// envelope with is_error set is the tool's own report of a failure (a *ToolError: quota when a quota text is in it, else
// tool_failed). The answer reader accepts two shapes of the prose: one or more markdown tables "Severity | Location (file:line) |
// Finding", whose rows together are the findings (verdict needs_changes when a finding blocks under premerge.Blocks once
// normalize has given it its impact: a P0-P2 grade, or a security finding; else verified), or no table and a last line that ends
// with the sentence "Bugbot found no bugs." or that sentence with a short scope phrase ("... in the uncommitted changes.")
// (verified, no findings). Prose that says the review covered only part of the change or
// dropped a finding (bugbotPartial: files skipped, unchecked or not reviewed, a review that did not, could not or only reviewed
// part, a partial, truncated or incomplete review, a change too large, a review that timed out, a finding removed or dropped, a
// review not clean), read in the prose outside the tables in any case, prose that only mentions "found no bugs" (negated, hedged,
// or followed by more), and prose of any other shape, is not an answer.
//
// The quota texts are UNCONFIRMED: no real quota sample of the tool has been captured, and the wording is the server's. They are
// matched only on a failed call or a blank answer, so a finding that quotes one is unaffected. Replace them once a sample exists.
//
// BugbotProfile is the uncommitted mode: its clean sentence takes the scope phrases of bugbotNoBugsScope.uncommitted.
func BugbotProfile() Profile {
	p := bugbotCall("bugbot")
	p.Answer = bugbotAnswer(bugbotNoBugsScope.uncommitted)
	return p
}

// BugbotCommittedProfile is the committed mode of the unit check (CRW-1468: land-head's check of base..head): the same cursor-agent
// call as BugbotProfile, whose prompt names the range, with an answer reader that takes the range's scope phrases
// (bugbotNoBugsScope.committed: "in the changes between <base> and <head>", "in <base>..<head>") and refuses a result whose prose
// outside the findings tables names the uncommitted, staged or unstaged changes or the working tree: the range is checked in a clean
// checkout of its head, so a review of any of those reviewed nothing of the range. The profile is Ranged: its reader is given the
// range the request asks about and takes an answer only when it names that range by its commit ids (a clean sentence whose scope
// phrase names it, or findings whose prose names it) and names no other range anywhere outside the tables; a clean answer that names
// no range, or names another (a narrower B..C of the requested A..C), is no answer. Whether cursor-agent reviews a range, and names
// it, is UNCONFIRMED (no real committed-mode sample yet): until it does, a real answer fails closed as invalid. The reader is tested on
// synthetic envelopes of the uncommitted mode's shape.
func BugbotCommittedProfile() Profile {
	p := bugbotCall("bugbot-committed")
	p.Ranged = true
	p.Answer = bugbotAnswer(bugbotNoBugsScope.committed)
	return p
}

// bugbotCall is the cursor-agent call both Bugbot profiles make, under the profile's name and without its answer reader.
func bugbotCall(name string) Profile {
	return Profile{
		Name:       name,
		Argv:       []string{"cursor-agent", "-p", "--mode", "ask", "--trust", "--model", bugbotModel, "--output-format", "json"},
		Model:      bugbotModel,
		Effort:     bugbotEffort,
		QuotaTexts: []string{"usage limit", "rate limit", "resource_exhausted", "too many requests", "429", "spend limit", "upgrade"},
	}
}

// bugbotTableHeader is the header of Bugbot's findings table, its cells lower-cased and trimmed.
var bugbotTableHeader = []string{"severity", "location (file:line)", "finding"}

// bugbotPartial are the wordings of prose that says the review was not whole: it covered only part of the change, or a finding was
// dropped. They are matched in lower case, on word boundaries, in the prose outside the findings tables only, so a finding whose
// text uses one of the words is finding text. Every real complete Bugbot result of the R1, B1 and N1 lanes reads the same with
// them (none of these wordings is in one).
var bugbotPartial = regexp.MustCompile(`\b(` + strings.Join([]string{
	`skip(s|ped|ping)?`,
	`unchecked`, `unreviewed`, `not (been )?(fully )?(reviewed|checked|covered)`,
	`(did not|didn['’]t|could not|couldn['’]t|cannot|can['’]t|unable to|failed to) (fully )?(review|check|cover|finish|complete)`,
	`only (reviewed|checked|covered|looked at)`,
	`partial(ly)?`, `incomplete`, `truncat(ed|ion)`, `too (large|big|long)`, `timed out`, `cut short`,
	`\d+ of (the )?\d+ (changed |uncommitted |modified )?files`,
	`not clean`, `removed`, `dropped`, `omitted`,
}, "|") + `)\b`)

// bugbotAnswer is the answer reader of a Bugbot profile whose scope is scope: it reads cursor-agent's envelope and turns its result
// prose into a document of AnswerSchema. A blank result is a blank answer. Prose outside the findings tables that names a scope the
// profile refuses (scope.wrong) is not an answer, and a clean sentence takes only the profile's scope phrases (scope.phrase). A ranged
// scope takes only an answer about the request's range (bugbotRangeOf).
func bugbotAnswer(scope bugbotScope) func(stdout []byte, req Request) ([]byte, error) {
	return func(stdout []byte, req Request) ([]byte, error) { return readBugbot(stdout, scope, req) }
}

func readBugbot(stdout []byte, scope bugbotScope, req Request) ([]byte, error) {
	var env struct {
		Result  *string `json:"result"`
		IsError bool    `json:"is_error"`
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("the output is not cursor-agent's JSON envelope: %v", err)
	}
	if err := onlyValue(dec); err != nil {
		return nil, fmt.Errorf("the output is more than one JSON envelope: %v", err)
	}
	if env.IsError {
		text := ""
		if env.Result != nil {
			text = *env.Result
		}
		return nil, &ToolError{Text: text}
	}
	if env.Result == nil {
		return nil, errors.New("the envelope has no result")
	}
	text := *env.Result
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	prose, rows, found, err := findingsTable(text)
	if err != nil {
		return nil, err
	}
	if w := bugbotPartial.FindString(strings.ToLower(prose)); w != "" {
		return nil, fmt.Errorf("the review was not whole (%q): %s", w, firstLine([]byte(prose)))
	}
	if scope.wrong != nil {
		if w := scope.wrong.FindString(strings.ToLower(prose)); w != "" {
			return nil, fmt.Errorf("the review names another scope (%q): %s", w, firstLine([]byte(prose)))
		}
	}
	if scope.ranged {
		named, err := bugbotRangeOf(prose, req.Base, req.Head)
		if err != nil {
			return nil, err
		}
		if found && !named {
			return nil, fmt.Errorf("the review does not name the range %s..%s it was asked about: %s", req.Base, req.Head, firstLine([]byte(prose)))
		}
	}
	type finding struct {
		Severity string `json:"severity"`
		File     string `json:"file"`
		Line     int    `json:"line"`
		Text     string `json:"text"`
	}
	doc := struct {
		Verdict  string    `json:"verdict"`
		Findings []finding `json:"findings"`
	}{Verdict: VerdictVerified, Findings: []finding{}}
	switch {
	case found:
		var graded []Finding
		var security []bool
		for _, r := range rows {
			file, line := splitLocation(r[1])
			doc.Findings = append(doc.Findings, finding{Severity: r[0], File: file, Line: line, Text: r[2]})
			if g, sec, ok := review.NormalizeGrade(r[0]); ok {
				graded = append(graded, Finding{ID: FindingID(file, r[2]), Severity: string(g), File: file, Line: line, Text: r[2]})
				security = append(security, sec)
			}
		}
		// The verdict follows the rule decodeAnswer applies to this document: needs_changes when a finding blocks once normalize
		// has given it its impact (a P0-P2 grade, or a security finding's safety_or_data). A row no grade word reads is left to
		// decodeAnswer, which refuses the document.
		if norm, err := normalize(VerdictNeedsChanges, graded, security); err == nil {
			if _, blocks := firstBlocking(norm); blocks {
				doc.Verdict = VerdictNeedsChanges
			}
		}
	case saysNoBugs(prose, scope):
	default:
		return nil, fmt.Errorf("the result has neither a findings table nor a last sentence %q: %s", "Bugbot found no bugs.", firstLine([]byte(text)))
	}
	return json.Marshal(doc)
}

// bugbotNoBugs is the sentence of a clean Bugbot review, lower-cased and without its full stop.
const bugbotNoBugs = "bugbot found no bugs"

// bugbotScope is what a Bugbot profile's reader takes as the scope of its request: phrase is the short scope phrase Bugbot may append
// to its clean sentence ("Bugbot found no bugs in the uncommitted changes."), matched lower-cased at the end of the sentence without
// its full stop; wrong, when set, is a scope named anywhere in the prose outside the findings tables that makes the result no answer;
// ranged is set for the committed range, whose answer must name the request's range (bugbotRangeOf): its clean sentence's scope phrase
// names it.
type bugbotScope struct {
	phrase, wrong *regexp.Regexp
	ranged        bool
}

// bugbotRev is a revision a committed scope phrase names, lower-cased: a commit id of 7 to 64 hex digits (optionally in backticks),
// or "the base", "the head"; bugbotRange is a range of two of them: "between <a> and <b>", "from <a> to <b>", or "<a>..<b>" /
// "<a>...<b>". A symbolic ref (HEAD, HEAD~1, a branch) is not taken: the request names its range by commit ids, and "HEAD~1..HEAD"
// names a range that may be narrower than the one asked for. Which commit ids the phrase names is read against the request by
// bugbotRangeOf: "the base" and "the head" name no commit of the request, so a range of them ties the answer to none.
const (
	bugbotRev   = "(the (base|head)|`?[0-9a-f]{7,64}`?)"
	bugbotDots  = bugbotRev + `\.\.\.?` + bugbotRev
	bugbotRange = "((between|from) " + bugbotRev + " (and|to) " + bugbotRev + "|(in |of |for )?" + bugbotDots + ")"
)

// bugbotNoBugsScope is the scope reader of each Bugbot profile (one regexp per profile).
//
// uncommitted: the phrase must name the scope the request asks for, all uncommitted changes of the worktree: in/on/for/across,
// optionally "the", optionally "uncommitted" or "working tree" ("working-tree"), then changes, diff, code or files; or "the working
// tree". A phrase with any other qualifier (staged, committed, changed, modified, a number, "first N") names another or a narrower
// scope, and one that says more ("in the files it could open") is not taken either.
//
// committed: the phrase must name the range the request asks for: in/on/for/across, optionally "the", optionally "committed", then
// changes, diff, code, files, commits, range or commit range, optionally followed by the range (bugbotRange) and the request's own
// "(git diff <base>..<head>)"; or the range alone ("in <base>..<head>"). "uncommitted", "working tree", "staged" or any other
// qualifier is not taken, and prose outside the tables that names the uncommitted changes, the working tree or the staged (or
// unstaged) changes is no answer: the range is checked in a clean checkout, where each of those is empty.
var bugbotNoBugsScope = struct {
	uncommitted, committed bugbotScope
}{
	uncommitted: bugbotScope{phrase: regexp.MustCompile(`[ \t]+(in|on|for|across)(( the)?( (uncommitted|working[- ]tree))? (changes|diff|code|files)| the working tree)$`)},
	committed: bugbotScope{
		ranged: true,
		phrase: regexp.MustCompile(`[ \t]+(in|on|for|across)(( the)?( committed)? (changes|diff|code|files|commits|range|commit range)( ` + bugbotRange + `)?| ` + bugbotDots + `)( \(git diff ` + bugbotDots + `\))?$`),
		wrong:  regexp.MustCompile(`\b(uncommitted|working[- ]tree|(un)?staged|staging area)\b`),
	},
}

// saysNoBugs reports whether prose affirms a clean review: its last non-empty line ends with the sentence "Bugbot found no bugs"
// (any case), optionally followed by the profile's scope phrase (bugbotScope.phrase), then only '.' or '!', and that sentence starts the line or
// follows the end of an earlier sentence. cursor-agent glues its sentences without a space ("...launching the reviewer.Bugbot found
// no bugs."), so the line may carry earlier sentences; "whether Bugbot found no bugs", "Bugbot found no bugs?" or the sentence
// followed by more prose are not affirmations. The partial-review words are read before this, so they still win.
func saysNoBugs(prose string, scope bugbotScope) bool {
	lines := strings.Split(strings.TrimSpace(prose), "\n")
	last := strings.TrimRight(strings.ToLower(strings.TrimSpace(lines[len(lines)-1])), ".!")
	phrase := scope.phrase.FindString(last)
	last = strings.TrimSuffix(last, phrase)
	before, ok := strings.CutSuffix(last, bugbotNoBugs)
	if !ok {
		return false
	}
	// a committed clean sentence names the range it judged in its own scope phrase; bugbotRangeOf has refused any other range already
	if scope.ranged && !bugbotRangeExpr.MatchString(phrase) {
		return false
	}
	before = strings.TrimRight(before, " \t")
	return before == "" || strings.HasSuffix(before, ".") || strings.HasSuffix(before, "!") || strings.HasSuffix(before, "?")
}

// bugbotRangeExpr finds the ranges committed prose names, lower-cased: "between <a> and <b>", "from <a> to <b>", "<a>..<b>" or
// "<a>...<b>", each revision a commit id of 7 to 64 hex digits (optionally in backticks) or "the base" / "the head". The submatches
// are the two revisions of the first form (1, 2) or of the second (3, 4).
var bugbotRangeExpr = regexp.MustCompile("(?:between|from) " + bugbotRangeRev + " (?:and|to) " + bugbotRangeRev + "|" + bugbotRangeRev + `\.\.\.?` + bugbotRangeRev)

const bugbotRangeRev = "(the base|the head|`?\\b[0-9a-f]{7,64}\\b`?)"

// bugbotRangeOf reads the ranges the prose of a committed answer names (outside the findings tables) against the request's range
// base..head: each must be that range, its first revision a commit id that base starts with and its second one that head starts
// with (case aside); a range of any other commits, of the two the other way round, or of "the base" and "the head" (which name no
// commit) is an error, since the answer may then be about a narrower or another range. It answers whether the prose names the
// request's range at all.
func bugbotRangeOf(prose, base, head string) (bool, error) {
	base, head = strings.ToLower(base), strings.ToLower(head)
	named := false
	for _, m := range bugbotRangeExpr.FindAllStringSubmatch(strings.ToLower(prose), -1) {
		from, to := m[1], m[2]
		if from == "" {
			from, to = m[3], m[4]
		}
		if !bugbotRevIs(from, base) || !bugbotRevIs(to, head) {
			return false, fmt.Errorf("the review names the range %q, not the range %s..%s it was asked about", m[0], base, head)
		}
		named = true
	}
	return named, nil
}

// bugbotRevIs reports whether a revision a committed answer names is the commit id want: a commit id of at least 7 hex digits that want
// starts with. "the base" and "the head" are not.
func bugbotRevIs(rev, want string) bool {
	rev = strings.Trim(rev, "`")
	return len(rev) >= 7 && want != "" && strings.HasPrefix(want, rev)
}

// findingsTable finds every findings table in text. It returns the prose outside the tables, the rows of all of them in order as
// [severity, location, finding] cells, and whether a table was there; a table without rows, or with a row of fewer cells, is an
// error.
func findingsTable(text string) (string, [][3]string, bool, error) {
	lines := strings.Split(text, "\n")
	var prose []string
	var rows [][3]string
	found := false
	for i := 0; i < len(lines); {
		cells, ok := tableCells(lines[i], 3)
		if !ok || !equalFold(cells, bugbotTableHeader) {
			prose = append(prose, lines[i])
			i++
			continue
		}
		found = true
		j := i + 1
		if j < len(lines) {
			if sep, ok := tableCells(lines[j], 3); ok && strings.Trim(strings.Join(sep, ""), "-: ") == "" {
				j++
			}
		}
		n := len(rows)
		for ; j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "|"); j++ {
			cells, ok := tableCells(lines[j], 3)
			if !ok {
				return "", nil, true, fmt.Errorf("a row of a findings table has fewer than three cells: %s", strings.TrimSpace(lines[j]))
			}
			rows = append(rows, [3]string{cells[0], cells[1], cells[2]})
		}
		if len(rows) == n {
			return "", nil, true, errors.New("a findings table has no row")
		}
		i = j
	}
	if !found {
		return text, nil, false, nil
	}
	return strings.Join(prose, "\n"), rows, true, nil
}

// tableCells splits a markdown table line into n cells; the last cell keeps any further '|'.
func tableCells(line string, n int) ([]string, bool) {
	l := strings.TrimSpace(line)
	if !strings.HasPrefix(l, "|") {
		return nil, false
	}
	l = strings.TrimSuffix(strings.TrimPrefix(l, "|"), "|")
	cells := strings.SplitN(l, "|", n)
	if len(cells) != n {
		return nil, false
	}
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells, true
}

func equalFold(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// splitLocation splits "file:line" at the last ':' (backticks dropped); a location without a line, or with one that is not a
// number, is the file at line 0. A range "25-30" gives its first line.
func splitLocation(loc string) (string, int) {
	loc = strings.Trim(strings.TrimSpace(loc), "`")
	i := strings.LastIndexByte(loc, ':')
	if i < 0 {
		return loc, 0
	}
	first, _, _ := strings.Cut(loc[i+1:], "-")
	n, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil || n < 0 {
		return loc, 0
	}
	return loc[:i], n
}
