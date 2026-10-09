package skill

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// `crw skill issue-ready check`: the release gate for one issue. A child that is given an issue with no
// decided answer designs while it implements, and the evaluations that follow find the same class of
// fault again (CRW-1039). The check reads the issue the way the Linear tools give it and answers whether
// the body carries what a child needs before it starts: the registered criteria, the edit regions, a
// decided answer with no design question left, the test cases to write first and see fail, and the end
// condition. An issue that is not ready is held with the name of every missing item and the heading that
// supplies it; an issue labelled design first, or one whose open decisions still list a question, is
// held as design_first until the design result stands in the decided answer.
//
// The answer holds the release (exit 1). The one way past it is a recorded exception: the management
// session's explicit approval, which the command cannot verify and only refuses to record without a name,
// an issue, a date and a statement, the way the size check records its exception. crw-run owns what a
// held answer does to a release; crw-plan owns the rule the items come from.

const readySchema = "crw-issue-ready-check/1"

// The items a ready issue carries, in the order the report lists them.
const (
	itemCriteria = "criteria"
	itemEdit     = "edit_region"
	itemDecided  = "decided_answer"
	itemRedTest  = "red_test"
	itemDone     = "done_condition"
)

var readyItems = []string{itemCriteria, itemEdit, itemDecided, itemRedTest, itemDone}

// readyOpen is the section kind of the open decisions; it is not an item, it is what holds the issue as
// design first while it lists a question. The other kinds are named by the item they supply (the scope
// section only adds to the edit regions).
const (
	readyOpen  = "open_decisions"
	readyScope = "scope"
)

// What supplies each item, shown in the reason: the Korean heading of the recorded issues first.
var readyHeadingHint = map[string]string{
	itemCriteria: "the acceptance criteria as a list under '## 기준' (or 'Acceptance criteria')",
	itemEdit:     "the files or directories the change edits, in backticks, under '## 편집 영역' (or 'Edit regions')",
	itemDecided:  "the decided answer under '## 정한 답' (or 'Decided answer'), with no design question left",
	itemRedTest:  "the test cases to write first and see fail under '## 먼저 실패하게 쓸 시험' (or 'Red tests')",
	itemDone:     "which command must give which result under '## 끝 조건' (or 'Done condition'), with the command in backticks or a code block",
}

// readyHeadings classifies a heading by words matched in the compact form (no case, spaces or emphasis).
// The first row that matches wins. A heading about what is out of scope is none of them.
var readyHeadings = []struct {
	kind            string
	words, prefixes []string
}{
	{readyOpen, []string{"열린결정", "열린질문", "남은설계질문", "opendecision", "openquestion", "opendesign"}, nil},
	{itemDecided, []string{"정한답", "확정한답", "확정된답", "decidedanswer", "settledanswer", "designanswer", "designresult"}, nil},
	{itemRedTest, []string{"빨간시험", "빨간테스트", "먼저실패", "실패하는시험", "실패시험", "redtest", "failingtest", "testfirst", "testsfirst"}, nil},
	{itemDone, []string{"끝조건", "종료조건", "donecondition", "donewhen", "finishcondition", "endcondition", "exitcondition"}, nil},
	{itemEdit, []string{"편집영역", "수정영역", "editregion", "editarea", "editsurface"}, nil},
	{itemCriteria, []string{"완료기준", "등록기준", "completioncriteria", "acceptancecriteria"}, []string{"기준", "criteria"}},
	{readyScope, []string{"범위", "scope"}, nil},
}

var readyOutOfScope = []string{"범위밖", "outofscope", "out-of-scope", "non-goal", "nongoal"}

func readyHeadingKind(title string) string {
	compact := compactHeading(title)
	for _, w := range readyOutOfScope {
		if strings.Contains(compact, w) {
			return ""
		}
	}
	for _, k := range readyHeadings {
		for _, w := range k.words {
			if strings.Contains(compact, w) {
				return k.kind
			}
		}
		for _, w := range k.prefixes {
			if strings.HasPrefix(compact, w) {
				return k.kind
			}
		}
	}
	return ""
}

// designFirstLabels are the label names (compact form) that hold an issue as design first.
var designFirstLabels = []string{"설계먼저", "designfirst", "design-first"}

// labelList is the labels of an issue: names, or objects with a name, as the Linear tools return them.
type labelList []string

func (l *labelList) UnmarshalJSON(raw []byte) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return errors.New("labels must be a list of names")
	}
	for _, item := range items {
		var name string
		if json.Unmarshal(item, &name) != nil {
			var object struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(item, &object) != nil || object.Name == "" {
				return errors.New("labels must be a list of names")
			}
			name = object.Name
		}
		*l = append(*l, name)
	}
	return nil
}

type readyInput struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Labels      labelList      `json:"labels"`
	Exception   *sizeException `json:"exception"`
}

type readyObserved struct {
	EditRegions    []string `json:"edit_regions"`
	UnreadHeadings []string `json:"unread_headings"`
}

type readyReport struct {
	Schema          string         `json:"schema"`
	Issue           string         `json:"issue"`
	Title           string         `json:"title"`
	Decision        string         `json:"decision"`
	DesignFirst     bool           `json:"design_first"`
	Present         []string       `json:"present"`
	Missing         []string       `json:"missing"`
	OpenQuestions   []string       `json:"open_questions"`
	Reasons         []string       `json:"reasons"`
	Observed        readyObserved  `json:"observed"`
	Exception       *sizeException `json:"exception,omitempty"`
	ExceptionRecord string         `json:"exception_record,omitempty"`
}

// punctuation dropped when an entry is compared with the placeholder and none words.
const readyPunctuation = " \t.。,;:!?*_`-()[]~…"

func compactEntry(s string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if strings.ContainsRune(readyPunctuation, r) {
			return -1
		}
		return r
	}, s))
}

// placeholderEntry is an entry that holds the place of an answer and states none.
func placeholderEntry(s string) bool {
	switch compactEntry(s) {
	case "", "tbd", "todo", "미정", "추후", "추후결정", "wip":
		return true
	}
	return false
}

// noneWords are the whole entries (compact form) that say nothing is open.
var noneWords = map[string]bool{
	"no": true, "nil": true, "na": true, "n/a": true, "none": true, "nothing": true,
	"noopendecisions": true, "noopenquestions": true, "noneopen": true,
	"없음": true, "없다": true, "없습니다": true, "해당없음": true, "해당사항없음": true, "열린결정없음": true, "남은질문없음": true,
}

// parenthetical is a note in brackets after the word, which the none check leaves out.
var parenthetical = regexp.MustCompile(`\([^()]*\)|（[^（）]*）`)

// noneEntry is an entry that says there is nothing (for the open decisions): the whole entry is a none
// word, with at most a note in brackets. An entry that only starts with one ("None of the providers
// supports CAS; which fallback?") is a question.
func noneEntry(s string) bool {
	c := compactEntry(parenthetical.ReplaceAllString(s, ""))
	return c == "" || noneWords[c]
}

// entries are what a section states: its list items and table rows, or, when it has none, its
// paragraphs.
func (s section) entries() []string {
	items := s.items()
	if len(items) == 0 {
		for _, p := range strings.Split(s.text(), "\n\n") {
			if p = strings.TrimSpace(p); p != "" {
				items = append(items, p)
			}
		}
	}
	return items
}

// fencedLines are the lines of a fenced block that carry content: not the fence lines, not blank. A
// done condition may be written as a block alone (the command and its expected result).
func (s section) fencedLines() int {
	n := 0
	for i, line := range s.lines {
		if s.fenced[i] && strings.TrimSpace(line) != "" {
			if mark, _, _ := fenceRun(line); mark == 0 {
				n++
			}
		}
	}
	return n
}

// hasCommand is whether the section names a command: a backticked token outside code, or a line of a
// fenced block that is not the fence itself.
func (s section) hasCommand() bool {
	for i, line := range s.lines {
		if s.fenced[i] {
			if mark, _, _ := fenceRun(line); mark == 0 && strings.TrimSpace(line) != "" {
				return true
			}
			continue
		}
		if pathToken.MatchString(line) || strings.Count(line, "`") >= 2 {
			return true
		}
	}
	return false
}

func questionLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 200 {
		line = string(r[:200])
	}
	return line
}

func readyReportFor(in readyInput) (readyReport, error) {
	sections, unread, err := readSectionsBy(in.Description, readyHeadingKind)
	if err != nil {
		return readyReport{}, err
	}
	if in.Exception != nil {
		if err := in.Exception.check(in.ID); err != nil {
			return readyReport{}, err
		}
	}
	has := map[string]bool{}
	var open []string
	var regionText []string
	for _, s := range sections {
		var real []string
		for _, e := range s.entries() {
			if !placeholderEntry(e) {
				real = append(real, e)
			}
		}
		switch s.kind {
		case readyOpen:
			for _, e := range real {
				if !noneEntry(e) {
					open = append(open, questionLine(e))
				}
			}
			continue
		case itemDone:
			if (len(real) > 0 || s.fencedLines() > 0) && s.hasCommand() {
				has[itemDone] = true
			}
		case itemEdit, readyScope:
			// These supply the edit region only through the paths they name (below).
		default:
			if len(real) > 0 {
				has[s.kind] = true
			}
		}
		regionText = append(regionText, s.text())
	}
	regions := pathRegions(strings.Join(regionText, "\n"))
	if len(regions) > 0 {
		has[itemEdit] = true
	}

	report := readyReport{Schema: readySchema, Issue: in.ID, Title: in.Title, Present: []string{}, Missing: []string{}, OpenQuestions: []string{}, Reasons: []string{},
		Observed: readyObserved{EditRegions: regions, UnreadHeadings: append([]string{}, unread...)}}
	if report.Observed.EditRegions == nil {
		report.Observed.EditRegions = []string{}
	}
	for _, item := range readyItems {
		if has[item] {
			report.Present = append(report.Present, item)
			continue
		}
		report.Missing = append(report.Missing, item)
		report.Reasons = append(report.Reasons, fmt.Sprintf("missing %s: state %s", item, readyHeadingHint[item]))
	}
	labelled := ""
	for _, l := range in.Labels {
		for _, name := range designFirstLabels {
			if compactHeading(l) == name {
				labelled = l
			}
		}
	}
	report.OpenQuestions = append(report.OpenQuestions, open...)
	report.DesignFirst = labelled != "" || len(open) > 0
	if labelled != "" {
		report.Reasons = append(report.Reasons, fmt.Sprintf("design first: the issue carries the label %q; put the design result (the architect's proposal and the parent's decision) in the decided answer section and remove the label before release", labelled))
	}
	if len(open) > 0 {
		report.Reasons = append(report.Reasons, fmt.Sprintf("design first: the open decisions section still lists %d question(s); settle them in a design, record the result in the decided answer section and leave the open decisions empty before release", len(open)))
	}

	held := report.DesignFirst || len(report.Missing) > 0
	switch {
	case !held:
		report.Decision = "ready"
		return report, nil
	case report.DesignFirst:
		report.Decision = "design_first"
	default:
		report.Decision = "not_ready"
	}
	if e := in.Exception; e != nil {
		by, statement := strings.TrimSpace(e.ApprovedBy), strings.Join(strings.Fields(e.Statement), " ")
		report.Exception = &sizeException{e.Issue, by, e.ApprovedOn, statement}
		without := append([]string{}, report.Missing...)
		if report.DesignFirst {
			without = append(without, "a settled design")
		}
		report.ExceptionRecord = fmt.Sprintf("release-gate exception: %s released without %s; approved by %s on %s: %s", in.ID, strings.Join(without, ", "), by, e.ApprovedOn, statement)
		report.Decision = "bypassed"
	}
	return report, nil
}

func runIssueReady(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	name, code, ok := issueReady.command(args, stdout, stderr)
	if !ok {
		return code
	}
	line := newCommandLine("issue-ready", name, "Read one issue as JSON, from the file named or stdin, and print whether it may be released.").takes("file", 0, 1)
	positionals, code := line.parse(args[1:], stdout, stderr)
	if code >= 0 {
		return code
	}
	path := ""
	if len(positionals) == 1 {
		path = positionals[0]
	}
	raw, err := readFileOrStdin(path, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Issue ready check failed: %s. Nothing was decided.\n", err)
		return 3
	}
	if !utf8.Valid(raw) {
		fmt.Fprintln(stderr, "Unreadable issue: the input is "+errNotUTF8.Error())
		return 2
	}
	var in readyInput
	if err := json.Unmarshal(raw, &in); err != nil {
		fmt.Fprintln(stderr, "Unreadable issue: "+err.Error())
		return 2
	}
	report, err := readyReportFor(in)
	if err != nil {
		fmt.Fprintln(stderr, "Unreadable issue: "+err.Error())
		return 2
	}
	_ = emit(stdout, report)
	if report.Decision == "ready" || report.Decision == "bypassed" {
		return 0
	}
	return 1
}
