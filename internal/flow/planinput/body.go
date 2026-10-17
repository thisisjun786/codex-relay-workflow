package planinput

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/skill"
)

// headingSet selects release-gate kinds using issue-ready's single alias list,
// plus exact legacy headings that plan-input has always supported.
type headingSet struct {
	kinds, legacy []string
}

// The compile input carries criteria, the decided answer, red tests and the done
// condition in its criteria list. Regions remain separate. A section runs to the
// next heading; headings and bullets inside code fences are not read.
var (
	criteriaHeadings = headingSet{kinds: []string{"criteria", "decided_answer", "red_test", "done_condition"}, legacy: []string{"acceptance"}}
	regionHeadings   = headingSet{kinds: []string{"edit_region"}, legacy: []string{"수정 범위", "regions"}}
	sizeHeadings     = headingSet{legacy: []string{"크기", "size", "size fit"}}
)

// sizeFields are the "name: value" lines of a size section and the closed set of each value; compile judges the same sets.
var sizeFields = map[string][]string{
	"size_fit":    skill.SizeFits,
	"size_reason": skill.SizeReasons,
	"issue_size":  skill.IssueSizeAnswers,
}

var (
	headingLine     = regexp.MustCompile(`^ {0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	setextLine      = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	setextListLine  = regexp.MustCompile(`^(\d{1,9}[.)]|[-*+])(?:([ \t]+)(.*))?$`)
	setextBreakLine = regexp.MustCompile(`^ {0,3}(?:(?:-[ \t]*){3,}|(?:\*[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	bulletLine      = regexp.MustCompile(`^(\s*)(?:[-*+]|\d+[.)])(?:\s+(.*))?$`)
	checkbox        = regexp.MustCompile(`^\[[ xX]\]\s*`)
	blockedLine     = regexp.MustCompile(`(?i)^(?:\*\*)?blocked by:`)
	sizeFieldRe     = regexp.MustCompile("^(size_fit|size_reason|issue_size)\\s*:\\s*`?([A-Za-z0-9_]+)`?\\s*$")
	backtickSpan    = regexp.MustCompile("`([^`]+)`")
)

// section is the non-fence lines under one heading.
type section struct {
	heading string
	lines   []string
	depth   int
}

// headingBlockLine is the shared classification of a physical line. Paragraph
// readers and reference-definition readers must agree on these boundaries.
type headingBlockLine struct {
	text                       string
	indent                     int
	fence                      string
	html                       int
	atx, list                  []string
	blank, setext, rule, quote bool
}

func readHeadingBlock(line string, inParagraph bool) headingBlockLine {
	text := strings.TrimLeft(line, " \t")
	b := headingBlockLine{
		text: text, indent: indentOf(line[:len(line)-len(text)]),
		atx: headingLine.FindStringSubmatch(line), blank: text == "",
	}
	t := strings.TrimLeft(line, " ")
	switch {
	case strings.HasPrefix(t, "```"):
		b.fence = "```"
	case strings.HasPrefix(t, "~~~"):
		b.fence = "~~~"
	}
	if b.indent <= 3 {
		b.html = headingHTMLStart(text, inParagraph)
		b.setext = setextLine.MatchString(line)
		b.rule = setextBreakLine.MatchString(text)
		b.list = setextListLine.FindStringSubmatch(text)
		b.quote = strings.HasPrefix(text, ">")
	}
	return b
}

func (b headingBlockLine) interruptsParagraph() bool {
	return b.blank || b.fence != "" || b.html != 0 || b.atx != nil ||
		b.setext || b.rule || b.list != nil || b.quote
}

func headingReferenceBoundary(s string) bool {
	line, _, _ := strings.Cut(s, "\n")
	return readHeadingBlock(strings.TrimRight(line, "\r"), true).interruptsParagraph()
}

// sections splits a body at its ATX or Setext headings, omitting an out-of-scope heading and
// its descendants through the next peer or shallower heading. Text before the
// first heading belongs to no section.
func sections(body string) []section {
	const (
		noParagraph = iota
		rootParagraph
		listParagraph
		quoteParagraph
	)
	var out []section
	excludedDepth := 0
	state := struct {
		paragraph    []string
		kind         int
		itemText     int
		fence        string
		html         int // zero: none; otherwise index + 1 in headingHTMLBlocks
		quoteDepth   int
		htmlItemText int // zero: root HTML; otherwise its owning list's text column
	}{}
	emit := func(lines ...string) {
		if excludedDepth == 0 && len(out) > 0 {
			out[len(out)-1].lines = append(out[len(out)-1].lines, lines...)
		}
	}
	flush := func() {
		emit(state.paragraph...)
		state.paragraph, state.kind, state.quoteDepth = nil, noParagraph, 0
	}
	heading := func(title string, depth int) {
		state.itemText = 0
		if excludedDepth > 0 && depth > excludedDepth {
			return
		}
		excludedDepth = 0
		if skill.ReadyOutOfScopeHeading(title) {
			excludedDepth = depth
			return
		}
		out = append(out, section{heading: normalizeHeading(title), depth: depth})
	}
	for remaining := strings.ReplaceAll(body, "\r\n", "\n"); remaining != ""; {
		source := remaining
		var raw string
		raw, remaining, _ = strings.Cut(remaining, "\n")
		line := strings.TrimRight(raw, "\r")
		block := readHeadingBlock(line, state.kind != noParagraph)
		rest, col := block.text, block.indent
		inItem := state.itemText > 0 && col >= state.itemText
		if state.html != 0 {
			if state.htmlItemText > 0 && !block.blank && col < state.htmlItemText {
				// A container exit ends its HTML block. Re-read this line as a
				// normal block; it may itself be a heading or a new container.
				state.html, state.htmlItemText, state.itemText = 0, 0, 0
				inItem = false
			} else {
				if headingHTMLEnd(state.html, line, true) {
					state.html, state.htmlItemText = 0, 0
				}
				continue
			}
		}
		marker := block.fence
		if marker != "" {
			flush()
			switch state.fence {
			case "":
				state.fence = marker
				if col < state.itemText {
					state.itemText = 0
				}
			case marker:
				state.fence = ""
			}
			continue
		}
		if state.fence != "" {
			continue
		}
		if html := block.html; html != 0 {
			flush()
			state.htmlItemText = 0
			if !inItem {
				state.itemText = 0
			}
			if !headingHTMLEnd(html, rest, false) {
				state.html = html
				if inItem {
					state.htmlItemText = state.itemText
				}
			}
			// An omitted block must also end bullet continuation in the section.
			emit("")
			continue
		}
		if col <= 3 && state.kind == noParagraph && !inItem && strings.HasPrefix(rest, "[") {
			if n := headingReference(strings.TrimLeft(source, " \t")); n > 0 {
				remaining = strings.TrimLeft(source, " \t")[n:]
				state.itemText = 0
				emit("")
				continue
			}
		}
		if m := block.atx; m != nil {
			flush()
			heading(m[2], len(m[1]))
			continue
		}
		if state.kind == rootParagraph && block.setext {
			parts := make([]string, len(state.paragraph))
			for i, part := range state.paragraph {
				parts[i] = strings.TrimSpace(part)
			}
			title := strings.Join(parts, " ")
			depth := 1
			if strings.HasPrefix(strings.TrimSpace(line), "-") {
				depth = 2
			}
			// The complete paragraph and underline are the heading, not body data.
			state.paragraph, state.kind, state.quoteDepth = nil, noParagraph, 0
			heading(title, depth)
			continue
		}
		switch m := block.list; {
		case block.blank:
			flush()
			emit(line)
		case block.rule:
			flush()
			if !inItem {
				state.itemText = 0
			}
			emit(line)
		case m != nil:
			flush()
			if !inItem {
				gap := 0
				for _, r := range m[2] {
					if r == '\t' {
						gap += 4 - gap%4
					} else {
						gap++
					}
				}
				if gap == 0 || gap > 4 {
					gap = 1
				}
				state.itemText = col + len(m[1]) + gap
			}
			if strings.TrimSpace(m[3]) == "" {
				emit(line)
			} else {
				state.kind = listParagraph
				state.paragraph = append(state.paragraph, line)
			}
		case block.quote:
			text, depth := headingQuoteText(line)
			continued := state.kind == quoteParagraph && state.quoteDepth == depth &&
				(state.itemText == 0 || inItem)
			flush()
			if !inItem {
				state.itemText = 0
			}
			content := strings.TrimLeft(text, " ")
			paragraph := false
			if len(text)-len(content) >= 4 {
				// Indented text in this same open paragraph cannot start a
				// nested HTML block, fence or heading.
				paragraph = continued && content != ""
			} else {
				paragraph = setextParagraph(content) && !strings.HasPrefix(content, "```") &&
					!strings.HasPrefix(content, "~~~") && headingHTMLStart(content, continued) == 0
			}
			if paragraph {
				state.kind, state.quoteDepth = quoteParagraph, depth
				state.paragraph = append(state.paragraph, line)
			} else {
				emit(line)
			}
		case setextParagraph(rest):
			if state.kind == noParagraph {
				switch {
				case inItem:
					state.kind = listParagraph
				case col <= 3:
					state.kind = rootParagraph
					state.itemText = 0
				default:
					emit(line)
					continue
				}
			}
			// Unmarked continuations keep the paragraph's origin, including a
			// list paragraph that began after a blank line.
			state.paragraph = append(state.paragraph, line)
		default:
			flush()
			if !inItem {
				state.itemText = 0
			}
			emit(line)
		}
	}
	flush()
	return out
}

// Block starts and reference syntax follow the document checker's rules in
// internal/dev/ci/anchors.go. That dev-only package is not a runtime dependency.
var headingHTMLBlocks = []struct {
	start, end *regexp.Regexp // nil end: a blank line terminates the block
	interrupts bool
}{
	{regexp.MustCompile(`^<!--`), nil, true}, // headingCommentOpen also handles reopening
	{regexp.MustCompile(`(?i)^<(?:pre|script|style|textarea)(?:[ \t>]|$)`), regexp.MustCompile(`(?i)</(?:pre|script|style|textarea)>`), true},
	{regexp.MustCompile(`^<\?`), regexp.MustCompile(`\?>`), true},
	{regexp.MustCompile(`^<![A-Za-z]`), regexp.MustCompile(`>`), true},
	{regexp.MustCompile(`^<!\[CDATA\[`), regexp.MustCompile(`\]\]>`), true},
	{regexp.MustCompile(`(?i)^</?(?:address|article|aside|base|basefont|blockquote|body|caption|center|col|colgroup|dd|details|dialog|dir|div|dl|dt|fieldset|figcaption|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|iframe|legend|li|link|main|menu|menuitem|nav|noframes|ol|optgroup|option|p|param|search|section|summary|table|tbody|td|tfoot|th|thead|title|tr|track|ul)(?:[ \t]|/?>|$)`), nil, true},
	{regexp.MustCompile("^(?:<[A-Za-z][A-Za-z0-9-]*(?:[ \\t]+[^\\s\"'<>/=`]+(?:[ \\t]*=[ \\t]*(?:\"[^\"]*\"|'[^']*'|[^\\s\"'=<>`]+))?)*[ \\t]*/?>|</[A-Za-z][A-Za-z0-9-]*[ \\t]*>)[ \\t]*$"), nil, false},
}

func headingHTMLStart(line string, inParagraph bool) int {
	for i, block := range headingHTMLBlocks {
		if (block.interrupts || !inParagraph) && block.start.MatchString(line) {
			return i + 1
		}
	}
	return 0
}

func headingHTMLEnd(kind int, line string, open bool) bool {
	if kind == 1 {
		return !headingCommentOpen(line, open)
	}
	end := headingHTMLBlocks[kind-1].end
	if end == nil {
		return strings.TrimSpace(line) == ""
	}
	return end.MatchString(line)
}

// headingQuoteText removes quote markers and their one optional space, keeping
// code indentation, and returns the text and quote depth. Expand tabs at their
// original columns before stripping.
func headingQuoteText(line string) (string, int) {
	var expanded strings.Builder
	col := 0
	for _, r := range line {
		if r == '\t' {
			n := 4 - col%4
			expanded.WriteString(strings.Repeat(" ", n))
			col += n
		} else {
			expanded.WriteRune(r)
			col++
		}
	}
	text := expanded.String()
	depth := 0
	for {
		rest := strings.TrimLeft(text, " ")
		if len(text)-len(rest) > 3 || !strings.HasPrefix(rest, ">") {
			return text, depth
		}
		text = strings.TrimPrefix(rest[1:], " ")
		depth++
	}
}

// headingReference reads the link reference definition s starts with, as CommonMark reads one: a
// label in brackets (some non-space text of at most 999 characters, no unescaped bracket), ':', white space holding at most one
// line end, a destination (in <> without a line end or an unescaped <, or plain text without spaces
// or control characters whose parentheses balance), and, after white space, an optional title in
// double quotes, single quotes or parentheses; nothing but spaces may follow on the line. When the
// title is not well formed or text follows it, the definition ends with its destination if nothing
// follows that on its line. It
// returns the length of s the definition takes, up to its line end, or zero.
func headingReference(s string) int {
	if !strings.HasPrefix(s, "[") {
		return 0
	}
	i, characters := 1, 0 // the label holds at most 999 characters (not UTF-8 bytes)
	for ; ; i++ {
		if i >= len(s) || s[i] == '[' || characters > 999 {
			return 0
		}
		if s[i] == '\n' && headingReferenceBoundary(s[i+1:]) {
			return 0
		}
		if s[i] == '\\' && i+1 < len(s) && headingPunctuation(rune(s[i+1])) {
			i++
			characters += 2
			continue
		}
		if s[i] == ']' {
			break
		}
		if utf8.RuneStart(s[i]) {
			characters++
		}
	}
	label := strings.TrimSpace(s[1:i])
	if label == "" || i+1 >= len(s) || s[i+1] != ':' {
		return 0
	}
	i = headingDefinitionSpace(s, i+2)
	destination := i
	if i < len(s) && s[i] == '<' {
		for i++; ; i++ {
			if i >= len(s) || s[i] == '\n' || s[i] == '<' {
				return 0
			}
			if s[i] == '\\' && i+1 < len(s) && headingPunctuation(rune(s[i+1])) {
				i++
				continue
			}
			if s[i] == '>' {
				i++
				break
			}
		}
	} else {
		depth := 0
	raw:
		for ; i < len(s) && s[i] > ' ' && s[i] != 0x7f; i++ {
			switch {
			case s[i] == '\\' && i+1 < len(s) && headingPunctuation(rune(s[i+1])):
				i++
			case s[i] == '(':
				depth++
			case s[i] == ')':
				if depth == 0 {
					break raw
				}
				depth--
			}
		}
		if i == destination || depth != 0 {
			return 0
		}
	}
	if title := headingDefinitionSpace(s, i); title > i && title < len(s) && strings.ContainsRune("\"'(", rune(s[title])) {
		if end, ok := headingTitleEnd(s, title); ok {
			if line, ok := headingLineEnd(s, end); ok {
				return line
			}
		}
	}
	if line, ok := headingLineEnd(s, i); ok {
		return line
	}
	return 0
}

// headingDefinitionSpace skips horizontal space and at most one non-boundary line end.
func headingDefinitionSpace(s string, i int) int {
	lineEnd := false
	for ; i < len(s); i++ {
		if s[i] == '\n' {
			if lineEnd || headingReferenceBoundary(s[i+1:]) {
				break
			}
			lineEnd = true
		} else if s[i] != ' ' && s[i] != '\t' {
			break
		}
	}
	return i
}

// headingTitleEnd is the index after the title that opens at s[open] (in double quotes, single
// quotes or parentheses), and whether it is closed; in a () title an unescaped '(' is not allowed.
func headingTitleEnd(s string, open int) (int, bool) {
	closing := s[open]
	if closing == '(' {
		closing = ')'
	}
	for i := open + 1; i < len(s); i++ {
		switch {
		case s[i] == '\n' && headingReferenceBoundary(s[i+1:]):
			return 0, false
		case s[i] == '\\' && i+1 < len(s) && headingPunctuation(rune(s[i+1])):
			i++
		case s[i] == closing:
			return i + 1, true
		case s[open] == '(' && s[i] == '(':
			return 0, false
		}
	}
	return 0, false
}

// headingLineEnd is the index after the line end that ends s[i:] (or len(s)), and whether only
// spaces and tabs come before it.
func headingLineEnd(s string, i int) (int, bool) {
	for ; i < len(s); i++ {
		switch s[i] {
		case '\n':
			return i + 1, true
		case ' ', '\t':
		default:
			return 0, false
		}
	}
	return i, true
}

func headingPunctuation(r rune) bool {
	return strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", r)
}

// headingCommentOpen carries an HTML comment block across lines. A comment may
// close and another open on the same line; its contents never become headings.
func headingCommentOpen(line string, open bool) bool {
	for {
		marker := "<!--"
		if open {
			marker = "-->"
		}
		at := strings.Index(line, marker)
		if at < 0 {
			return open
		}
		line = line[at+len(marker):]
		open = !open
	}
}

// setextParagraph rejects block markers that an underline cannot turn into a
// section title. List-continuation and indentation checks belong to sections.
func setextParagraph(text string) bool {
	return text != "" && !strings.HasPrefix(text, "|") && !strings.HasPrefix(text, ">") &&
		!strings.HasPrefix(text, "#") && !setextListLine.MatchString(text) &&
		!setextLine.MatchString(text) && !setextBreakLine.MatchString(text)
}

func normalizeHeading(s string) string {
	s = strings.NewReplacer("*", "", "_", "").Replace(s)
	s = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(s), ":："))
	return strings.ToLower(s)
}

// linesOf joins the lines of every section whose heading belongs to names.
func linesOf(secs []section, names headingSet) (lines []string, found bool) {
	for _, s := range secs {
		if contains(names.legacy, s.heading) || contains(names.kinds, skill.ReadyHeadingKind(s.heading)) {
			found = true
			lines = append(lines, s.lines...)
		}
	}
	return lines, found
}

// bullets are the list items of lines, without marker and checkbox. An indented line that follows an item directly continues it; a
// "Blocked by:" item is an order and not an item, at any indent (tracking.ParseBlockedBy reads it so). With nested, a bullet indented
// deeper than the item above it is a sub-bullet and part of that item (at any depth, after a blank line too): it joins the item's text.
// A "Blocked by:" bullet is never joined: under an item it is left out and the item stays the parent of the sub-bullets after it, and a
// sub-bullet of a "Blocked by:" item is left out with it. An indented line that is not a bullet leaves that state alone: it extends the
// item above it, or is left out with a "Blocked by:" item, and the sub-bullets after it are read as they were before it. An indented
// "Blocked by:" line that is not a bullet is read as the bullet is, with or without an item above it, and never joins an item. Without
// nested every bullet is an item of its own, the reading of a list of paths.
func bullets(lines []string, nested bool) []string {
	var out []string
	cur := -1    // the item a continuation line extends
	parent := -1 // the item whose sub-bullets join it (nested only)
	parentIndent := 0
	dropped := false // the sub-bullets deeper than dropIndent belong to a "Blocked by:" item that was left out (nested only)
	dropIndent := 0
	follows := false // the line above was a bullet or an indented line of one, not a blank line
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			cur, follows = -1, false
			continue
		}
		wasFollowing := follows
		follows = true
		if m := bulletLine.FindStringSubmatch(line); m != nil {
			indent := indentOf(m[1])
			text := strings.TrimSpace(checkbox.ReplaceAllString(strings.TrimSpace(m[2]), ""))
			if dropped && indent > dropIndent {
				cur = -1
				continue
			}
			dropped = false
			blocked := blockedLine.MatchString(text)
			if nested && parent >= 0 && indent > parentIndent {
				if blocked {
					// an order under an item: not part of its text, and the item stays the parent of what follows
					dropped, dropIndent, cur = true, indent, -1
				} else {
					out[parent] = strings.TrimSpace(out[parent] + " " + text)
					cur = parent
				}
				continue
			}
			parent, parentIndent = -1, indent
			if blocked {
				cur = -1
				dropped, dropIndent = nested, indent
				continue
			}
			out = append(out, text)
			cur = len(out) - 1
			parent = cur
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if blockedLine.MatchString(strings.TrimSpace(line)) {
				// an order in a plain line: not part of any item, whose state it leaves as it is; what is deeper than it leaves with it
				if at := indentOf(line[:len(line)-len(strings.TrimLeft(line, " \t"))]); nested && (!dropped || at < dropIndent) {
					dropped, dropIndent = true, at
				}
				cur = -1
				continue
			}
			if cur >= 0 {
				out[cur] = strings.TrimSpace(out[cur] + " " + strings.TrimSpace(line))
				continue
			}
			if wasFollowing && (parent >= 0 || dropped) {
				// the continuation of a "Blocked by:" item that was left out
				continue
			}
		}
		cur, parent, dropped, follows = -1, -1, false, false
	}
	kept := out[:0]
	for _, b := range out {
		if b != "" {
			kept = append(kept, b)
		}
	}
	return kept
}

// indentOf is the width of a bullet's leading white space, a tab counting four columns.
func indentOf(ws string) int {
	n := 0
	for _, r := range ws {
		if r == '\t' {
			n += 4
		} else {
			n++
		}
	}
	return n
}

// criteria are the bullets of the criteria sections.
func criteria(secs []section) []string {
	// Existing inputs using only the old headings keep their bytes and source
	// order. A new release-gate heading used by this reader selects kind order.
	legacy := true
	for _, s := range secs {
		kind := skill.ReadyHeadingKind(s.heading)
		if !contains(criteriaHeadings.kinds, kind) && !contains(regionHeadings.kinds, kind) {
			continue
		}
		switch s.heading {
		case "끝 조건", "done when", "acceptance", "acceptance criteria", "edit regions":
		default:
			legacy = false
		}
	}
	if legacy {
		lines, _ := linesOf(secs, criteriaHeadings)
		return bullets(lines, true)
	}
	var out []string
	for _, kind := range criteriaHeadings.kinds {
		names := headingSet{kinds: []string{kind}}
		if kind == "done_condition" {
			names.legacy = criteriaHeadings.legacy
		}
		lines, _ := linesOf(secs, names)
		out = append(out, bullets(lines, true)...)
	}
	return out
}

// regionOf reads the path of one region item: the first backticked span, else the first word. ok is false for a path the scheduler
// cannot take (compile refuses it): an absolute path, one that leaves the repository, the root of the repository.
func regionOf(item string) (r compile.Region, ok bool) {
	p := ""
	if m := backtickSpan.FindStringSubmatch(item); m != nil {
		p = strings.TrimSpace(m[1])
	} else if f := strings.Fields(item); len(f) > 0 {
		p = strings.TrimRight(f[0], ",;:")
	}
	trimmed := strings.TrimSuffix(p, "/")
	if p == "" || trimmed == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\\") {
		return compile.Region{Path: p}, false
	}
	if clean := path.Clean(trimmed); clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return compile.Region{Path: p}, false
	}
	kind := "file"
	if strings.HasSuffix(p, "/") {
		kind = "tree"
	}
	return compile.Region{Path: p, Kind: kind}, true
}

// sizeOf reads the "name: value" lines of the size sections. A value outside the closed set is returned in bad and not in fields.
func sizeOf(secs []section) (fields map[string]string, bad []string) {
	fields = map[string]string{}
	lines, _ := linesOf(secs, sizeHeadings)
	for _, line := range lines {
		text := strings.TrimSpace(line)
		if m := bulletLine.FindStringSubmatch(line); m != nil {
			text = strings.TrimSpace(m[2])
		}
		m := sizeFieldRe.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		name, value := m[1], m[2]
		if !contains(sizeFields[name], value) {
			bad = append(bad, name+": "+value)
			continue
		}
		fields[name] = value
	}
	return fields, bad
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
