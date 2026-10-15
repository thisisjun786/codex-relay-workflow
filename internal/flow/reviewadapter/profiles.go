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
	Answer     func(stdout []byte) ([]byte, error)
}

// JSONProfile is a tool that prints the answer document itself.
func JSONProfile(name string, argv []string, model, effort string) Profile {
	return Profile{Name: name, Argv: argv, Model: model, Effort: effort, Answer: func(stdout []byte) ([]byte, error) { return stdout, nil }}
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
// with the sentence "Bugbot found no bugs." (verified, no findings). Prose that says the review covered only part of the change or
// dropped a finding (bugbotPartial: files skipped, unchecked or not reviewed, a review that did not, could not or only reviewed
// part, a partial, truncated or incomplete review, a change too large, a review that timed out, a finding removed or dropped, a
// review not clean), read in the prose outside the tables in any case, prose that only mentions "found no bugs" (negated, hedged,
// or followed by more), and prose of any other shape, is not an answer.
//
// The quota texts are UNCONFIRMED: no real quota sample of the tool has been captured, and the wording is the server's. They are
// matched only on a failed call or a blank answer, so a finding that quotes one is unaffected. Replace them once a sample exists.
func BugbotProfile() Profile {
	return Profile{
		Name:       "bugbot",
		Argv:       []string{"cursor-agent", "-p", "--mode", "ask", "--trust", "--model", bugbotModel, "--output-format", "json"},
		Model:      bugbotModel,
		Effort:     bugbotEffort,
		QuotaTexts: []string{"usage limit", "rate limit", "resource_exhausted", "too many requests", "429", "spend limit", "upgrade"},
		Answer:     bugbotAnswer,
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

// bugbotAnswer reads cursor-agent's envelope and turns its result prose into a document of AnswerSchema. A blank result is a
// blank answer.
func bugbotAnswer(stdout []byte) ([]byte, error) {
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
	case saysNoBugs(prose):
	default:
		return nil, fmt.Errorf("the result has neither a findings table nor a last sentence %q: %s", "Bugbot found no bugs.", firstLine([]byte(text)))
	}
	return json.Marshal(doc)
}

// bugbotNoBugs is the sentence of a clean Bugbot review, lower-cased and without its full stop.
const bugbotNoBugs = "bugbot found no bugs"

// saysNoBugs reports whether prose affirms a clean review: its last non-empty line ends with the sentence "Bugbot found no bugs"
// (any case, then only '.' or '!'), and that sentence starts the line or follows the end of an earlier sentence. cursor-agent glues
// its sentences without a space ("...launching the reviewer.Bugbot found no bugs."), so the line may carry earlier sentences;
// "whether Bugbot found no bugs", "Bugbot found no bugs?" or the sentence followed by more prose are not affirmations.
func saysNoBugs(prose string) bool {
	lines := strings.Split(strings.TrimSpace(prose), "\n")
	last := strings.TrimRight(strings.ToLower(strings.TrimSpace(lines[len(lines)-1])), ".!")
	before, ok := strings.CutSuffix(last, bugbotNoBugs)
	if !ok {
		return false
	}
	before = strings.TrimRight(before, " \t")
	return before == "" || strings.HasSuffix(before, ".") || strings.HasSuffix(before, "!") || strings.HasSuffix(before, "?")
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
