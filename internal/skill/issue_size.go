package skill

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// `crw skill issue-size check`: a deterministic size check for one issue, made at design time so an
// issue that would outgrow one pull request is found before a child is given it.
//
// It reads the issue the way the Linear tools give it (id, title, description as markdown) or as
// fields, counts what the body states, and answers ok or split_recommended with its reasons. Nothing
// in it judges: the same input is the same bytes. crw-plan owns what a split_recommended answer
// leads to (the boundary rules) and crw-run owns what it does to dispatch.

const (
	sizeSchema = "crw-issue-size-check/1"

	// The limits were read from 33 recorded issues (P-CRW-101, 114, 115, 116 and P-CRW-64 M2). The 31
	// that finished small have at most 6 completion plus research criteria and no research row; the
	// two that grew past their design (CRW-183 and CRW-184) have 10 and 14 criteria and 5 and 7 research
	// rows. Each limit is the middle of the gap between the two groups, so a count of that value or less
	// is ok (a tie rounds up). The deliverables limit is provisional: it rests on the new non-test files the
	// pull requests created (2 or fewer for the small issues, 11 and 30 for the two) while the signal counts
	// the deliverables an issue declares, and no recorded body declares any. The body counts and outcomes are
	// held in TestIssueSizeOnTheRecordedIssues.
	limitCriteriaTotal = 8
	limitResearch      = 3
	limitDeliverables  = 7
)

type sizeLimits struct {
	CriteriaTotal int `json:"criteria_total"`
	Research      int `json:"research_criteria"`
	Deliverables  int `json:"deliverables"`
}

var appliedLimits = sizeLimits{CriteriaTotal: limitCriteriaTotal, Research: limitResearch, Deliverables: limitDeliverables}

// sizeSignalCounts holds what was counted. The criteria counts and the deliverables have a limit; the
// records show the other two do not separate the small issues from the oversized ones (the oversized
// bodies name no file, and the small CRW-260 has as many verification kinds as CRW-184), so they are
// reported. Deliverables are null when the issue declares none: not measured is not zero.
type sizeSignalCounts struct {
	Criteria          int  `json:"criteria"`
	Research          int  `json:"research_criteria"`
	CriteriaTotal     int  `json:"criteria_total"`
	Deliverables      *int `json:"deliverables"`
	EditRegions       int  `json:"edit_regions"`
	VerificationKinds int  `json:"verification_kinds"`
}

type sizeObserved struct {
	EditRegions       []string `json:"edit_regions"`
	VerificationKinds []string `json:"verification_kinds"`
	UnreadHeadings    []string `json:"unread_headings"` // headings of the description the command did not read
}

type sizeException struct {
	Issue      string `json:"issue"`
	ApprovedBy string `json:"approved_by"`
	ApprovedOn string `json:"approved_on"`
	Statement  string `json:"statement"`
}

type sizeReport struct {
	Schema          string           `json:"schema"`
	Issue           string           `json:"issue"`
	Title           string           `json:"title"`
	Decision        string           `json:"decision"`
	Assignable      bool             `json:"assignable"`
	Reasons         []string         `json:"reasons"`
	Signals         sizeSignalCounts `json:"signals"`
	Limits          sizeLimits       `json:"limits"`
	Observed        sizeObserved     `json:"observed"`
	Proposal        *splitProposal   `json:"proposal,omitempty"`
	Exception       *sizeException   `json:"exception,omitempty"`
	ExceptionRecord string           `json:"exception_record,omitempty"`
}

// textField is a field given as one string (list markers and blank lines are dropped) or a list of
// strings (blank entries are dropped). A field that is present replaces what the body gives for it.
type textField struct{ items []string }

func (f *textField) UnmarshalJSON(raw []byte) error {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		for _, line := range strings.Split(one, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				f.items = append(f.items, strings.TrimSpace(listMarker.ReplaceAllString(s, "")))
			}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return errors.New("must be a string or a list of strings")
	}
	for _, s := range many {
		if s = strings.TrimSpace(s); s != "" {
			f.items = append(f.items, s)
		}
	}
	return nil
}

type sizeInput struct {
	ID           string              `json:"id"`
	Title        string              `json:"title"`
	Description  string              `json:"description"`
	Criteria     *textField          `json:"criteria"`
	Research     *textField          `json:"research_criteria"`
	Scope        *textField          `json:"scope"`
	Verification *textField          `json:"verification"`
	Deliverables *textField          `json:"deliverables"`
	DependsOn    map[string][]string `json:"depends_on"`
	Exception    *sizeException      `json:"exception"`
}

// The kinds of section the command reads, named by their heading. The headings of the recorded issues
// are Korean; the English words are the variant. A heading is matched without regard to case or spaces, by
// substring after emphasis marks are dropped (the English word research alone only as the start of the
// heading, so "Completion criteria (no research needed)" is not research, while "Research reinforcement"
// is research wherever it stands in the heading), in this order, and a heading deeper than a section already
// classified stays part of that section. A heading that is none of these is not read; the report names it.
const (
	kindResearch     = "research"
	kindCriteria     = "criteria"
	kindDeliverables = "deliverables"
	kindVerification = "verification"
	kindScope        = "scope"
)

var headingWords = []struct {
	kind            string
	words, prefixes []string
}{
	{kindResearch, []string{"연구보강", "researchreinforcement"}, []string{"research"}},
	{kindCriteria, []string{"완료기준", "completioncriteria", "acceptancecriteria"}, nil},
	{kindDeliverables, []string{"산출물", "deliverable"}, nil},
	{kindVerification, []string{"검증", "verification"}, nil},
	{kindScope, []string{"범위", "결과", "scope", "outcome", "result"}, nil},
}

var (
	linkTag        = regexp.MustCompile(`(?s)<(?:issue|project|pull-request|document)\b[^>]*>(.*?)</(?:issue|project|pull-request|document)>`)
	linkOpen       = regexp.MustCompile(`\[([^\]]*)\]\(`)
	markdownEscape = regexp.MustCompile(`\\([\\` + "`" + `*_{}\[\]()#+\-.!~|<>])`)
	headingLine    = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	setextRule     = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	listMarker     = regexp.MustCompile(`^(?:\d+[.)]|[-*+])[ \t]+`)
	listLine       = regexp.MustCompile(`^(\d{1,9}[.)]|[-*+])(?:([ \t]+)(.*))?$`)
	thematicBreak  = regexp.MustCompile(`^ {0,3}(?:(?:-[ \t]*){3,}|(?:\*[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	tableDelimiter = regexp.MustCompile(`^\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?$`)
)

// plainText removes what is only markup around the words: Linear's link tags, markdown links and markdown
// escapes. It is applied to a heading's or an item's words after the structure has been read, so an
// escaped marker is never a marker, and nothing is counted from an address.
func plainText(s string) string {
	s = linkTag.ReplaceAllString(s, "$1")
	s = stripMarkdownLinks(s)
	return markdownEscape.ReplaceAllString(s, "$1")
}

// stripMarkdownLinks reduces [text](address) to text. The address may hold balanced parentheses, or be
// written in angle brackets.
func stripMarkdownLinks(s string) string {
	var out strings.Builder
	for {
		loc := linkOpen.FindStringSubmatchIndex(s)
		if loc == nil {
			out.WriteString(s)
			return out.String()
		}
		end := linkAddressEnd(s, loc[1])
		if end < 0 {
			out.WriteString(s[:loc[1]])
			s = s[loc[1]:]
			continue
		}
		out.WriteString(s[:loc[0]])
		out.WriteString(s[loc[2]:loc[3]])
		s = s[end:]
	}
}

// linkAddressEnd is the index just after the parenthesis that closes a link address starting at i, or -1.
const linkAddressLimit = 2048 // bytes of an address read before giving up on it

func linkAddressEnd(s string, i int) int {
	if i < len(s) && s[i] == '<' {
		close := strings.IndexByte(s[i:], '>')
		if close < 0 {
			return -1
		}
		i += close + 1
	}
	for depth, limit := 1, i+linkAddressLimit; i < len(s) && i < limit; i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i + 1
			}
		case '\n':
			return -1
		}
	}
	return -1
}

// indentOf is the width in columns of a line's leading white space, a tab going to the next multiple of
// four, and the rest of the line.
func indentOf(line string) (int, string) {
	col := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			col++
		case '\t':
			col += 4 - col%4
		default:
			return col, line[i:]
		}
	}
	return col, ""
}

// fenceRun is the character and length of a code fence line (three or more backticks or tildes after at
// most three columns of indent) and what follows it on the line.
func fenceRun(line string) (mark byte, n int, rest string) {
	col, s := indentOf(line)
	if col > 3 || len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return 0, 0, ""
	}
	for n < len(s) && s[n] == s[0] {
		n++
	}
	if n < 3 {
		return 0, 0, ""
	}
	rest = strings.TrimSpace(s[n:])
	if s[0] == '`' && strings.Contains(rest, "`") {
		return 0, 0, "" // inline code that starts a line, not a fence
	}
	return s[0], n, rest
}

// paragraphLine is a line a setext underline can turn into a heading: text that is not a list item, a
// table row, a quote, an ATX heading or an underline itself.
func paragraphLine(line string) bool {
	col, s := indentOf(line)
	return col <= 3 && s != "" && !strings.HasPrefix(s, "|") && !strings.HasPrefix(s, ">") && !strings.HasPrefix(s, "#") &&
		!listLine.MatchString(s) && !setextRule.MatchString(line)
}

// section is the lines under one classified heading, up to the next heading of the same or a
// shallower level, with the lines that sit inside a fenced code block marked.
type section struct {
	kind   string
	lines  []string
	fenced []bool
}

func headingKind(title string) string {
	plain := strings.Map(func(r rune) rune {
		if r == '*' || r == '_' || r == '`' {
			return -1
		}
		return r
	}, strings.ToLower(title))
	compact := strings.Join(strings.Fields(plain), "")
	for _, k := range headingWords {
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

func readSections(body string) (sections []section, unread []string, err error) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	fenced := make([]bool, len(lines))
	type heading struct {
		at, skip, level int
		title           string
	}
	var headings []heading
	var openMark byte
	openLen := 0
	listParagraph := false // the lines since the last blank one continue a list item
	for i := 0; i < len(lines); i++ {
		switch mark, n, rest := fenceRun(lines[i]); {
		case openMark != 0:
			fenced[i] = true
			if mark == openMark && n >= openLen && rest == "" {
				openMark = 0
				listParagraph = false // a fence is no paragraph, so the line after it cannot continue an item's text
			}
		case mark != 0:
			fenced[i], openMark, openLen = true, mark, n
		default:
			col, text := indentOf(lines[i])
			switch m := headingLine.FindStringSubmatch(lines[i]); {
			case text == "":
				listParagraph = false
			case m != nil:
				headings = append(headings, heading{i, 1, len(m[1]), plainText(m[2])})
				listParagraph = false
			case !listParagraph && i+1 < len(lines) && paragraphLine(lines[i]) && setextRule.MatchString(lines[i+1]):
				level := 1
				if strings.HasPrefix(strings.TrimSpace(lines[i+1]), "-") {
					level = 2
				}
				headings = append(headings, heading{i, 2, level, plainText(strings.TrimSpace(lines[i]))})
				i++
				listParagraph = false
			case col <= 3 && thematicBreak.MatchString(text):
				listParagraph = false // a rule ends the item, and what follows it starts a new paragraph
			case col <= 3 && listLine.MatchString(text):
				listParagraph = true
			}
		}
	}
	if openMark != 0 {
		return nil, nil, errors.New("the description has a code fence that is never closed, so everything after it would be skipped; close it")
	}
	activeLevel := 0
	for h, head := range headings {
		if activeLevel != 0 && head.level > activeLevel {
			continue
		}
		activeLevel = 0
		kind := headingKind(head.title)
		if kind == "" {
			unread = append(unread, head.title)
			continue
		}
		activeLevel = head.level
		end := len(lines)
		for _, next := range headings[h+1:] {
			if next.level <= head.level {
				end = next.at
				break
			}
		}
		from := head.at + head.skip
		sections = append(sections, section{kind, lines[from:end], fenced[from:end]})
	}
	return sections, unread, nil
}

// markerGap is the columns between a list marker and its text; five or more is an indented code block, so one.
func markerGap(white string) int {
	n := 0
	for _, r := range white {
		if r == '\t' {
			n += 4 - n%4
		} else {
			n++
		}
	}
	if n > 4 {
		return 1
	}
	return n
}

// items are a section's criteria: its top-level list items and the data rows of its top-level pipe
// tables. A line belongs to the item above it when it is indented to that item's text (a nested list, a
// table, a paragraph) or continues its paragraph without a blank line; a sibling is indented less than
// the item's text, as in CommonMark.
func (s section) items() []string {
	var items, rows []string
	cur, offset := -1, 0
	afterBlank := false
	flush := func() {
		if len(rows) >= 2 && tableDelimiter.MatchString(rows[1]) {
			for _, row := range rows[2:] {
				items = append(items, strings.TrimSpace(strings.Trim(row, "|")))
			}
		}
		rows = nil
	}
	for i, line := range s.lines {
		indent, rest := indentOf(line)
		item := listLine.FindStringSubmatch(rest)
		if thematicBreak.MatchString(rest) {
			item = nil
		}
		switch {
		case s.fenced[i]:
			flush()
		case rest == "":
			flush()
			afterBlank = true
			continue
		case cur >= 0 && indent >= offset:
			items[cur] += "\n" + rest
		case item != nil && indent <= 3:
			flush()
			items = append(items, strings.TrimSpace(item[3]))
			cur, offset = len(items)-1, indent+len(item[1])+markerGap(item[2])
		case strings.HasPrefix(rest, "|") && indent <= 3:
			rows = append(rows, strings.TrimSpace(rest))
			cur = -1
		case cur >= 0 && !afterBlank && !setextRule.MatchString(line) && !thematicBreak.MatchString(rest):
			items[cur] += "\n" + rest
		default:
			flush()
			cur = -1
		}
		afterBlank = false
	}
	flush()
	for i := range items {
		items[i] = plainText(items[i])
	}
	return items
}

// text is a section's words outside fenced code.
func (s section) text() string {
	var kept []string
	for i, line := range s.lines {
		if !s.fenced[i] {
			kept = append(kept, line)
		}
	}
	return plainText(strings.Join(kept, "\n"))
}

// sizeIssue is what the command counts: the issue's four kinds of text.
type sizeIssue struct {
	criteria, research, deliverables, scope, verification, unread []string
	declaresDeliverables                                          bool
}

func readSizeIssue(in sizeInput) (sizeIssue, error) {
	var issue sizeIssue
	sections, unread, err := readSections(in.Description)
	if err != nil {
		return sizeIssue{}, err
	}
	issue.unread = unread
	for _, s := range sections {
		switch s.kind {
		case kindCriteria:
			issue.criteria = append(issue.criteria, s.items()...)
		case kindResearch:
			issue.research = append(issue.research, s.items()...)
		case kindDeliverables:
			issue.deliverables = append(issue.deliverables, s.items()...)
			issue.declaresDeliverables = true
		case kindScope:
			issue.scope = append(issue.scope, s.text())
		case kindVerification:
			issue.verification = append(issue.verification, s.text())
		}
	}
	for _, o := range []struct {
		field *textField
		into  *[]string
	}{{in.Criteria, &issue.criteria}, {in.Research, &issue.research}, {in.Deliverables, &issue.deliverables}, {in.Scope, &issue.scope}, {in.Verification, &issue.verification}} {
		if o.field != nil {
			*o.into = o.field.items
		}
	}
	issue.declaresDeliverables = issue.declaresDeliverables || in.Deliverables != nil
	return issue, nil
}

var (
	pathToken  = regexp.MustCompile("`([^`\\s]+)`")
	lineSuffix = regexp.MustCompile(`:\d+(-\d+)?$`)
	pathExt    = regexp.MustCompile(`\.(go|md|json|ya?ml|toml|sh|py|sql|txt)$`)
	fileExt    = regexp.MustCompile(`\.[A-Za-z0-9]+$`)
)

// pathRegions are the repository regions the text names: every backticked path, cut to its directory
// and to at most three segments, sorted and without repeats. Absolute, home, variable and URL tokens
// are not repository paths.
func pathRegions(text string) []string {
	seen := map[string]bool{}
	for _, m := range pathToken.FindAllStringSubmatch(text, -1) {
		t := lineSuffix.ReplaceAllString(m[1], "")
		if strings.HasPrefix(t, "/") || strings.HasPrefix(t, "~") || strings.HasPrefix(t, "$") || strings.Contains(t, "://") || strings.ContainsAny(t, "*{}<>=(),;?") {
			continue
		}
		if !strings.Contains(t, "/") && !pathExt.MatchString(t) {
			continue
		}
		clean := path.Clean(t)
		if clean == ".." || strings.HasPrefix(clean, "../") {
			continue
		}
		parts := strings.Split(clean, "/")
		if fileExt.MatchString(parts[len(parts)-1]) {
			parts = parts[:len(parts)-1]
		}
		if len(parts) > 3 {
			parts = parts[:3]
		}
		region := strings.Join(parts, "/")
		if region == "" {
			region = "."
		}
		seen[region] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// verificationKinds are the kinds of check the criteria and the verification text name, from a fixed
// word list, in the list's order.
var verificationKinds = []struct {
	name string
	re   *regexp.Regexp
}{
	{"test", regexp.MustCompile(`(?i)테스트|\btests?\b|\btesting\b`)},
	{"ci_gate", regexp.MustCompile(`(?i)make test|make lint|gofmt|go vet|git diff --check|\bci\b`)},
	{"review", regexp.MustCompile(`(?i)devin|리뷰|\breview`)},
	{"red_first", regexp.MustCompile(`(?i)수정 전|실패하|failing|red-first|before the fix`)},
	{"integration", regexp.MustCompile(`(?i)통합|integration|end-to-end|\be2e\b|실제 결합|\bfake\b|격리 서버|round-trip|왕복`)},
	{"concurrency", regexp.MustCompile(`(?i)경합|동시|concurren|\brace\b|writer`)},
	{"crash_recovery", regexp.MustCompile(`(?i)재시작|restart|crash|종료|복구|recover`)},
	{"golden", regexp.MustCompile(`(?i)golden|픽스처|fixture|replay`)},
	{"live", regexp.MustCompile(`(?i)설치본|installed|\blive\b|실사용|real-use|무개입`)},
	{"measurement", regexp.MustCompile(`(?i)측정|지표|metric|latency|\bp50\b|\bp90\b`)},
}

func kindsNamed(text string) []string {
	out := []string{}
	for _, k := range verificationKinds {
		if k.re.MatchString(text) {
			out = append(out, k.name)
		}
	}
	return out
}

func joined(parts ...[]string) string {
	var all []string
	for _, p := range parts {
		all = append(all, p...)
	}
	return strings.Join(all, "\n")
}

func (i sizeIssue) decide() (signals sizeSignalCounts, observed sizeObserved, reasons []string) {
	regions := pathRegions(joined(i.criteria, i.research, i.deliverables, i.scope, i.verification))
	kinds := kindsNamed(joined(i.criteria, i.research, i.deliverables, i.verification))
	signals = sizeSignalCounts{Criteria: len(i.criteria), Research: len(i.research), CriteriaTotal: len(i.criteria) + len(i.research), EditRegions: len(regions), VerificationKinds: len(kinds)}
	if i.declaresDeliverables {
		n := len(i.deliverables)
		signals.Deliverables = &n
	}
	observed = sizeObserved{regions, kinds, append([]string{}, i.unread...)}
	reasons = []string{}
	if signals.CriteriaTotal > appliedLimits.CriteriaTotal {
		reasons = append(reasons, fmt.Sprintf("criteria_total %d exceeds the limit %d (%d completion + %d research-reinforcement criteria)", signals.CriteriaTotal, appliedLimits.CriteriaTotal, signals.Criteria, signals.Research))
	}
	if signals.Research > appliedLimits.Research {
		reasons = append(reasons, fmt.Sprintf("research_criteria %d exceeds the limit %d", signals.Research, appliedLimits.Research))
	}
	if signals.Deliverables != nil && *signals.Deliverables > appliedLimits.Deliverables {
		reasons = append(reasons, fmt.Sprintf("deliverables %d exceeds the limit %d", *signals.Deliverables, appliedLimits.Deliverables))
	}
	return signals, observed, reasons
}

// check validates an exception against the issue it is for: shape only. Whether it is the user's own
// approval is what crw-run's text says; the command can only refuse a record that names no one.
func (e sizeException) check(id string) error {
	switch {
	case strings.TrimSpace(e.ApprovedBy) == "":
		return errors.New("exception.approved_by is empty")
	case strings.TrimSpace(e.Statement) == "":
		return errors.New("exception.statement is empty")
	case id == "":
		return errors.New("exception: the issue has no id to bind it to")
	case e.Issue != id:
		return fmt.Errorf("exception.issue %q is not the issue %q", e.Issue, id)
	}
	if day, err := time.Parse("2006-01-02", e.ApprovedOn); err != nil || day.Format("2006-01-02") != e.ApprovedOn {
		return fmt.Errorf("exception.approved_on %q is not a date as YYYY-MM-DD", e.ApprovedOn)
	}
	return nil
}

var itemID = regexp.MustCompile(`^([CR])([1-9][0-9]*)$`)

// itemNumber is the n of an id C<n> or R<n> (as kind says) that names one of count items, or 0.
func itemNumber(id, kind string, count int) int {
	m := itemID.FindStringSubmatch(id)
	if m == nil || m[1] != kind {
		return 0
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n > count {
		return 0
	}
	return n
}

// checkDepends reads depends_on against the items the issue has. Ids are C<n> for the n-th completion
// criterion and R<n> for the n-th research row; a need is a completion criterion, earlier than the item
// when that is a completion criterion too, so a draft can always order a need before what needs it.
func checkDepends(depends map[string][]string, completion, research int) error {
	keys := make([]string, 0, len(depends))
	for key := range depends {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		c, r := itemNumber(key, "C", completion), itemNumber(key, "R", research)
		if c == 0 && r == 0 {
			return fmt.Errorf("depends_on: %q is not an item of this issue (C1..C%d, R1..R%d)", key, completion, research)
		}
		for _, need := range depends[key] {
			switch n := itemNumber(need, "C", completion); {
			case n == 0:
				return fmt.Errorf("depends_on: %q needs %q, which is not a completion criterion of this issue", key, need)
			case c != 0 && n >= c:
				return fmt.Errorf("depends_on: %q needs %q, which is not earlier; put the criteria in dependency order", key, need)
			}
		}
	}
	return nil
}

func sizeReportFor(in sizeInput) (sizeReport, error) {
	issue, err := readSizeIssue(in)
	if err != nil {
		return sizeReport{}, err
	}
	if len(issue.criteria) == 0 {
		return sizeReport{}, errors.New("the issue has no completion criteria: give criteria, or a description with a '완료 기준' or 'Completion criteria' section")
	}
	if err := checkDepends(in.DependsOn, len(issue.criteria), len(issue.research)); err != nil {
		return sizeReport{}, err
	}
	if in.Exception != nil {
		if err := in.Exception.check(in.ID); err != nil {
			return sizeReport{}, err
		}
	}
	signals, observed, reasons := issue.decide()
	report := sizeReport{Schema: sizeSchema, Issue: in.ID, Title: in.Title, Decision: "ok", Assignable: true, Reasons: reasons, Signals: signals, Limits: appliedLimits, Observed: observed}
	if len(reasons) == 0 {
		return report, nil
	}
	report.Decision, report.Assignable = "split_recommended", false
	report.Proposal = proposeSplit(issue.criteria, issue.research, in.DependsOn)
	if e := in.Exception; e != nil {
		by, statement := strings.TrimSpace(e.ApprovedBy), strings.Join(strings.Fields(e.Statement), " ")
		report.Assignable = true
		report.Exception = &sizeException{e.Issue, by, e.ApprovedOn, statement}
		report.ExceptionRecord = fmt.Sprintf("size-check exception: %s is split_recommended (%s); approved by %s on %s: %s", in.ID, strings.Join(reasons, "; "), by, e.ApprovedOn, statement)
	}
	return report, nil
}

func runIssueSize(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	name, code, ok := issueSize.command(args, stdout, stderr)
	if !ok {
		return code
	}
	line := newCommandLine("issue-size", name, "Read one issue as JSON, from the file named or stdin, and print the size answer.").takes("file", 0, 1)
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
		fmt.Fprintf(stderr, "Issue size check failed: %s. Nothing was decided.\n", err)
		return 3
	}
	// Exit 1 means split_recommended and nothing else, so a caller that reads only the status stays
	// fail-closed; an input that cannot be read is 2 whatever is wrong with it (the sibling commands
	// answer 1 for text that is not UTF-8).
	if !utf8.Valid(raw) {
		fmt.Fprintln(stderr, "Unreadable issue: the input is "+errNotUTF8.Error())
		return 2
	}
	var in sizeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		fmt.Fprintln(stderr, "Unreadable issue: "+err.Error())
		return 2
	}
	report, err := sizeReportFor(in)
	if err != nil {
		fmt.Fprintln(stderr, "Unreadable issue: "+err.Error())
		return 2
	}
	_ = emit(stdout, report)
	if report.Assignable {
		return 0
	}
	return 1
}
