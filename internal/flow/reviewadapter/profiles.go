package reviewadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// Profile is one review tool: how to run it, which model and effort it runs, the texts that mean its quota is spent, and the
// reader that turns its stdout into a document of AnswerSchema. Family is the engine family when the model id does not name it
// (empty: execution.ModelFamily of Model).
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

// BugbotProfile is Cursor's Bugbot through cursor-agent in ask mode, which prints a one-line JSON envelope whose result is prose.
// The answer reader accepts two shapes of that prose: a markdown table "Severity | Location (file:line) | Finding" (verdict
// needs_changes when a finding grades P0-P2, else verified), or no table and "found no bugs" (verified, no findings). Prose that
// says the review covered only part of the change or dropped a finding ("unchecked", "not clean", "removed"), and prose of any
// other shape, is not an answer.
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

// bugbotPartial are the words of prose that says the review was not whole.
var bugbotPartial = []string{"unchecked", "not clean", "removed"}

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
	if dec.More() {
		return nil, errors.New("the output is more than one JSON envelope")
	}
	if env.Result == nil {
		return nil, errors.New("the envelope has no result")
	}
	text := *env.Result
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	if env.IsError {
		return nil, fmt.Errorf("the envelope reports an error: %s", firstLine([]byte(text)))
	}
	prose, rows, found, err := findingsTable(text)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(prose)
	for _, w := range bugbotPartial {
		if strings.Contains(lower, w) {
			return nil, fmt.Errorf("the review was not whole (%q): %s", w, firstLine([]byte(prose)))
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
		for _, r := range rows {
			file, line := splitLocation(r[1])
			doc.Findings = append(doc.Findings, finding{Severity: r[0], File: file, Line: line, Text: r[2]})
			if g, _, _ := review.NormalizeGrade(r[0]); g != "" && g != review.P3 {
				doc.Verdict = VerdictNeedsChanges
			}
		}
	case strings.Contains(lower, "found no bugs"):
	default:
		return nil, fmt.Errorf("the result has neither a findings table nor %q: %s", "found no bugs", firstLine([]byte(text)))
	}
	return json.Marshal(doc)
}

// findingsTable finds the findings table in text. It returns the prose outside the table, the rows as [severity, location,
// finding] cells, and whether a table was there; a table without rows, or with a row of fewer cells, is an error.
func findingsTable(text string) (string, [][3]string, bool, error) {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		cells, ok := tableCells(l, 3)
		if !ok || !equalFold(cells, bugbotTableHeader) {
			continue
		}
		j := i + 1
		if j < len(lines) {
			if sep, ok := tableCells(lines[j], 3); ok && strings.Trim(strings.Join(sep, ""), "-: ") == "" {
				j++
			}
		}
		var rows [][3]string
		for ; j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "|"); j++ {
			cells, ok := tableCells(lines[j], 3)
			if !ok {
				return "", nil, true, fmt.Errorf("a row of the findings table has fewer than three cells: %s", strings.TrimSpace(lines[j]))
			}
			rows = append(rows, [3]string{cells[0], cells[1], cells[2]})
		}
		if len(rows) == 0 {
			return "", nil, true, errors.New("the findings table has no row")
		}
		prose := strings.Join(append(append([]string{}, lines[:i]...), lines[j:]...), "\n")
		return prose, rows, true, nil
	}
	return text, nil, false, nil
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
