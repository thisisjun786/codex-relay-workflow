//go:build dev

package ci

import (
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// The anchor check: a relative link's #fragment must name a heading slug or an explicit anchor of
// the file the link leads to. The slug follows GitHub's rule on the text the heading renders to
// (emphasis and link markup reduced to their text, entities decoded; then lowercase, spaces to '-',
// punctuation dropped, a repeated slug gets -1, -2, ...), and the id or name attribute of an HTML
// element counts as an anchor. The text is split into blocks as CommonMark splits it (parseMarkdown):
// block quotes and list items, code, raw HTML blocks, headings and paragraphs. Fenced code, indented
// code, code spans and the Markdown inside a raw HTML block contribute nothing, as anchors and as
// links. It covers plugins/crw/skills/**/*.md and docs/**/*.md, the links to the same file and to
// another file alike; there is no allow-list.

var (
	atxHeading      = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	setextUnderline = regexp.MustCompile(`^ {0,3}(?:=+|-+)[ \t]*$`)
	thematicBreak   = regexp.MustCompile(`^ {0,3}(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	htmlOpenTag     = regexp.MustCompile("<[A-Za-z][A-Za-z0-9-]*(?:[ \\t]+[^\\s\"'<>/=`]+(?:[ \\t]*=[ \\t]*(?:\"[^\"]*\"|'[^']*'|[^\\s\"'=<>`]+))?)*[ \\t]*/?>")
	htmlAttribute   = regexp.MustCompile("[ \\t]+([^\\s\"'<>/=`]+)(?:[ \\t]*=[ \\t]*(?:\"([^\"]*)\"|'([^']*)'|([^\\s\"'=<>`]+)))?")
	htmlMarkup      = regexp.MustCompile(`^(?:</?[A-Za-z][^<>]*>|<!--.*?-->)`)
	autolink        = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9+.-]{1,31}:[^\s<>]*)>`)
	htmlEntity      = regexp.MustCompile(`^&(?:#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});`)
)

// anchorScope reports whether the links of name, a repository-relative path, are anchor-checked.
func anchorScope(name string) bool {
	return filepath.Ext(name) == ".md" &&
		(strings.HasPrefix(name, "plugins/crw/skills/") || strings.HasPrefix(name, "docs/"))
}

// anchorChecked reports whether name is a document whose links the link and anchor checks read:
// the dispatch-cases data files and the refactor backlog fragments are generator inputs, checked
// through the document generated from them.
func anchorChecked(name string) bool {
	return filepath.Ext(name) == ".md" && !dispatchCasesDataFile(name) && !refactorBacklogFragment(name)
}

// anchorSlug is the GitHub slug of one heading's text, before a repeated slug gets its number.
func anchorSlug(heading string) string {
	var slug strings.Builder
	for _, r := range strings.ToLower(headingText(heading)) {
		switch {
		case r == ' ':
			slug.WriteByte('-')
		case r == '-' || unicode.Is(unicode.Pc, r) || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r):
			slug.WriteRune(r)
		}
	}
	return slug.String()
}

// headingText is the text a heading renders: link and image markup reduced to their text, code
// spans unquoted, HTML tags dropped, emphasis that CommonMark pairs unmarked, backslash escapes
// resolved and entities decoded.
func headingText(heading string) string {
	return strings.TrimSpace(renderInline(heading))
}

// inlineToken is a run of rendered text, or (char != 0) a run of emphasis delimiters not yet resolved.
type inlineToken struct {
	text              string
	char              rune
	count, original   int
	canOpen, canClose bool
}

func isMarkdownPunctuation(r rune) bool {
	return r < unicode.MaxASCII && strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", r)
}

func isFlankPunct(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }

// renderInline is the text of the inline Markdown s: CommonMark's inline rules for code spans,
// escapes, entities, links, images, raw HTML and emphasis, which is all a heading's slug depends on.
func renderInline(s string) string {
	tokens := inlineTokens([]rune(s))
	pairEmphasis(tokens)
	var out strings.Builder
	for _, token := range tokens {
		if token.char != 0 {
			out.WriteString(strings.Repeat(string(token.char), token.count))
		} else {
			out.WriteString(token.text)
		}
	}
	return out.String()
}

// matchingBracket is the index of the ']' that closes the '[' at runes[open], or -1.
func matchingBracket(runes []rune, open int) int {
	depth := 0
	for i := open; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			i++
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// matchingParenthesis is the index of the ')' that closes the '(' at runes[open], or -1.
func matchingParenthesis(runes []rune, open int) int {
	depth := 0
	for i := open; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// codeSpanEnd is the end (exclusive) of the code span whose opening backtick run starts at runes[i]
// and has length n, and its text; ok is false when no closing run of the same length follows.
func codeSpanEnd(runes []rune, i, n int) (end int, content string, ok bool) {
	for j := i + n; j < len(runes); j++ {
		if runes[j] != '`' {
			continue
		}
		run := 0
		for j+run < len(runes) && runes[j+run] == '`' {
			run++
		}
		if run == n {
			text := strings.ReplaceAll(string(runes[i+n:j]), "\n", " ")
			if len(text) >= 2 && text[0] == ' ' && text[len(text)-1] == ' ' && strings.TrimSpace(text) != "" {
				text = text[1 : len(text)-1]
			}
			return j + n, text, true
		}
		j += run - 1
	}
	return 0, "", false
}

func inlineTokens(runes []rune) []inlineToken {
	var tokens []inlineToken
	var literal strings.Builder
	flush := func() {
		if literal.Len() > 0 {
			tokens = append(tokens, inlineToken{text: literal.String()})
			literal.Reset()
		}
	}
	for i := 0; i < len(runes); {
		r := runes[i]
		rest := string(runes[i:])
		switch {
		case r == '\\' && i+1 < len(runes) && isMarkdownPunctuation(runes[i+1]):
			literal.WriteRune(runes[i+1])
			i += 2
		case r == '`':
			n := 1
			for i+n < len(runes) && runes[i+n] == '`' {
				n++
			}
			if end, content, ok := codeSpanEnd(runes, i, n); ok {
				literal.WriteString(content)
				i = end
			} else {
				literal.WriteString(strings.Repeat("`", n))
				i += n
			}
		case r == '&' && htmlEntity.MatchString(rest):
			match := htmlEntity.FindString(rest)
			literal.WriteString(html.UnescapeString(match))
			i += len([]rune(match))
		case r == '<':
			if match := autolink.FindStringSubmatch(rest); match != nil {
				literal.WriteString(match[1])
				i += len([]rune(match[0]))
			} else if match := htmlMarkup.FindString(rest); match != "" {
				i += len([]rune(match))
			} else {
				literal.WriteRune(r)
				i++
			}
		case r == '[' || (r == '!' && i+1 < len(runes) && runes[i+1] == '['):
			open := i
			if r == '!' {
				open++
			}
			if closing := matchingBracket(runes, open); closing > 0 && closing+1 < len(runes) && runes[closing+1] == '(' {
				if end := matchingParenthesis(runes, closing+1); end > 0 {
					literal.WriteString(renderInline(string(runes[open+1 : closing])))
					i = end + 1
					continue
				}
			}
			literal.WriteRune(r)
			i++
		case r == '*' || r == '_':
			n := 1
			for i+n < len(runes) && runes[i+n] == r {
				n++
			}
			before, after := ' ', ' '
			if i > 0 {
				before = runes[i-1]
			}
			if i+n < len(runes) {
				after = runes[i+n]
			}
			left := !unicode.IsSpace(after) && (!isFlankPunct(after) || unicode.IsSpace(before) || isFlankPunct(before))
			right := !unicode.IsSpace(before) && (!isFlankPunct(before) || unicode.IsSpace(after) || isFlankPunct(after))
			token := inlineToken{char: r, count: n, original: n, canOpen: left, canClose: right}
			if r == '_' {
				token.canOpen = left && (!right || isFlankPunct(before))
				token.canClose = right && (!left || isFlankPunct(after))
			}
			flush()
			tokens = append(tokens, token)
			i += n
		default:
			literal.WriteRune(r)
			i++
		}
	}
	flush()
	return tokens
}

// pairEmphasis consumes the delimiters of tokens that CommonMark matches into emphasis; the ones
// left over are text.
func pairEmphasis(tokens []inlineToken) {
	for closer := range tokens {
		for tokens[closer].char != 0 && tokens[closer].canClose && tokens[closer].count > 0 {
			opener := -1
			for k := closer - 1; k >= 0; k-- {
				o, c := tokens[k], tokens[closer]
				if o.char != c.char || !o.canOpen || o.count == 0 {
					continue
				}
				if (o.canClose || c.canOpen) && (o.original+c.original)%3 == 0 && !(o.original%3 == 0 && c.original%3 == 0) {
					continue
				}
				opener = k
				break
			}
			if opener < 0 {
				break
			}
			used := 1
			if tokens[opener].count >= 2 && tokens[closer].count >= 2 {
				used = 2
			}
			tokens[opener].count -= used
			tokens[closer].count -= used
			for k := opener + 1; k < closer; k++ {
				tokens[k].canOpen, tokens[k].canClose = false, false
			}
		}
	}
}

// maskCodeSpans is text (the lines of one paragraph or heading, joined with "\n") with the bytes of
// its code spans blanked out; line ends stay, so a byte offset keeps its line. A backslash-escaped
// backtick opens no span, and a span runs over the lines of its block as CommonMark reads it.
func maskCodeSpans(text string) string {
	b := []byte(text)
	runOf := func(i int) int {
		n := 0
		for i+n < len(b) && b[i+n] == '`' {
			n++
		}
		return n
	}
	for i := 0; i < len(b); {
		switch {
		case b[i] == '\\' && i+1 < len(b) && isMarkdownPunctuation(rune(b[i+1])):
			i += 2
		case b[i] == '`':
			n, end := runOf(i), -1
			for j := i + n; j < len(b); {
				if b[j] != '`' {
					j++
					continue
				}
				if run := runOf(j); run == n {
					end = j + n
					break
				} else {
					j += run
				}
			}
			if end < 0 {
				i += n
				continue
			}
			for k := i; k < end; k++ {
				if b[k] != '\n' {
					b[k] = ' '
				}
			}
			i = end
		default:
			i++
		}
	}
	return string(b)
}

// htmlAnchors is the id and name attribute values of the HTML elements opening in text, which holds no
// code (maskCodeSpans). An attribute whose name only ends in id (data-id) is not one.
func htmlAnchors(text string) []string {
	var found []string
	for _, tag := range htmlOpenTag.FindAllString(text, -1) {
		name := strings.IndexAny(tag, " \t/>")
		for _, attribute := range htmlAttribute.FindAllStringSubmatch(tag[name:], -1) {
			if key := strings.ToLower(attribute[1]); key != "id" && key != "name" {
				continue
			}
			if value := attribute[2] + attribute[3] + attribute[4]; value != "" {
				found = append(found, value)
			}
		}
	}
	return found
}

// leading is the width of the white space s starts with when s starts at column col (a tab reaches
// the next multiple of four), and s after it.
func leading(s string, col int) (int, string) {
	width := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ':
			width++
		case '\t':
			width += 4 - (col+width)%4
		default:
			return width, s[i:]
		}
	}
	return width, ""
}

// strip is s, starting at column col, without n columns of leading white space; a tab that reaches
// past them leaves the rest of its width as spaces.
func strip(s string, col, n int) string {
	width := 0
	for i := 0; i < len(s); i++ {
		if width >= n {
			return s[i:]
		}
		switch s[i] {
		case ' ':
			width++
		case '\t':
			width += 4 - (col+width)%4
			if width > n {
				return strings.Repeat(" ", width-n) + s[i+1:]
			}
		default:
			return s[i:]
		}
	}
	return ""
}

// afterQuoteMarker is s, the text after a block quote's '>' at column col, without the one optional
// space that follows the marker, and the column it then starts at.
func afterQuoteMarker(s string, col int) (string, int) {
	if s == "" || (s[0] != ' ' && s[0] != '\t') {
		return s, col
	}
	return strip(s, col, 1), col + 1
}

var (
	listMarker     = regexp.MustCompile(`^([-*+]|([0-9]{1,9})[.)])(?:[ \t]|$)`)
	fenceOpen      = regexp.MustCompile("^(`{3,}|~{3,})(.*)$")
	fenceClose     = regexp.MustCompile("^(`{3,}|~{3,})[ \\t]*$")
	htmlBlockTypes = []struct {
		start, end *regexp.Regexp // end nil: the block ends at a blank line
		anchors    bool           // the block's elements are rendered, so their id and name count
		interrupts bool           // the block can interrupt a paragraph
	}{
		{regexp.MustCompile(`(?i)^<pre(?:[ \t>]|$)`), regexp.MustCompile(`(?i)</pre>`), true, true},
		{regexp.MustCompile(`(?i)^<(?:script|style|textarea)(?:[ \t>]|$)`), regexp.MustCompile(`(?i)</(?:script|style|textarea)>`), false, true},
		{regexp.MustCompile(`^<!--`), regexp.MustCompile(`-->`), false, true},
		{regexp.MustCompile(`^<\?`), regexp.MustCompile(`\?>`), false, true},
		{regexp.MustCompile(`^<![A-Za-z]`), regexp.MustCompile(`>`), false, true},
		{regexp.MustCompile(`^<!\[CDATA\[`), regexp.MustCompile(`\]\]>`), false, true},
		{regexp.MustCompile(`(?i)^</?(?:address|article|aside|base|basefont|blockquote|body|caption|center|col|colgroup|dd|details|dialog|dir|div|dl|dt|fieldset|figcaption|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|iframe|legend|li|link|main|menu|menuitem|nav|noframes|ol|optgroup|option|p|param|search|section|summary|table|tbody|td|tfoot|th|thead|title|tr|track|ul)(?:[ \t]|/?>|$)`), nil, true, true},
		{regexp.MustCompile("^(?:<[A-Za-z][A-Za-z0-9-]*(?:[ \\t]+[^\\s\"'<>/=`]+(?:[ \\t]*=[ \\t]*(?:\"[^\"]*\"|'[^']*'|[^\\s\"'=<>`]+))?)*[ \\t]*/?>|</[A-Za-z][A-Za-z0-9-]*[ \\t]*>)[ \\t]*$"), nil, true, false},
	}
)

// htmlBlockStart is the index in htmlBlockTypes of the raw HTML block line (indentation removed)
// starts, or -1; inParagraph says whether a paragraph is open, which only some kinds can interrupt.
func htmlBlockStart(line string, inParagraph bool) int {
	for kind, block := range htmlBlockTypes {
		if block.start.MatchString(line) && (block.interrupts || !inParagraph) {
			return kind
		}
	}
	return -1
}

// listItem reads a list marker at the start of line (indentation removed): the marker's width, the
// gap of white space after it as the content column counts it, and the content. A marker needs a
// space, a tab or the line end after it; one that interrupts a paragraph needs content, and an
// ordered one there must be 1.
func listItem(line string, col int, interrupts bool) (marker, gap int, content string, ok bool) {
	match := listMarker.FindStringSubmatch(line)
	if match == nil {
		return 0, 0, "", false
	}
	marker = len(match[1])
	width, content := leading(line[marker:], col+marker)
	if interrupts && (content == "" || (match[2] != "" && match[2] != "1")) {
		return 0, 0, "", false
	}
	switch {
	case content == "":
		return marker, 1, "", true
	case width > 4: // the content is indented code; the item's column is one past the marker
		return marker, 1, strip(line[marker:], col+marker, 1), true
	}
	return marker, width, content, true
}

// startsBlock reports whether line, read where a paragraph is open, starts a block other than a
// paragraph continuation: then it is not a lazy continuation of that paragraph.
func startsBlock(line string, col int) bool {
	indent, rest := leading(line, col)
	if indent > 3 || rest == "" {
		return false
	}
	_, _, _, item := listItem(rest, col+indent, true)
	return rest[0] == '>' || item || thematicBreak.MatchString(rest) || fenceOpen.MatchString(rest) ||
		atxHeading.MatchString(rest) || htmlBlockStart(rest, true) >= 0
}

// sourceLine is one line of a Markdown block: its 1-based number in the file and its text without
// the container prefixes and indentation.
type sourceLine struct {
	number int
	text   string
}

// inlineBlock is a block whose text is inline Markdown: a paragraph, or a heading (heading set).
type inlineBlock struct {
	isHeading bool
	heading   string
	lines     []sourceLine
}

// joined is the block's text, its lines joined with "\n".
func (b inlineBlock) joined() string {
	texts := make([]string, len(b.lines))
	for i, line := range b.lines {
		texts[i] = line.text
	}
	return strings.Join(texts, "\n")
}

// markdownDocument is what a Markdown text renders that the link and anchor checks read: its
// paragraphs and headings in order, and the lines of its raw HTML blocks whose elements are rendered.
type markdownDocument struct {
	blocks []inlineBlock
	html   []sourceLine
}

// blockContainer is an open block quote or list item; a list item's content starts width columns
// after the start of the line's text the container sees.
type blockContainer struct {
	quote bool
	width int
}

// parseMarkdown reads text the way CommonMark splits it into blocks: block quotes and list items
// (their content read as blocks of its own, a lazy line continuing a paragraph), fenced and indented
// code, raw HTML blocks, ATX and setext headings, thematic breaks and paragraphs. A frontmatter block
// at the top is skipped. Code and HTML block lines hold no inline Markdown, so they are in no block.
func parseMarkdown(text string) markdownDocument {
	var doc markdownDocument
	all := lines(text)
	start := 0
	if len(all) > 0 && strings.TrimRight(all[0], " \t") == "---" {
		for i := 1; i < len(all); i++ {
			if strings.TrimRight(all[i], " \t") == "---" {
				start = i + 1
				break
			}
		}
	}
	var (
		stack     []blockContainer
		fence     string // the open fence's delimiter
		html      = -1   // the kind of the open raw HTML block
		paragraph = -1   // the index in doc.blocks of the open paragraph
		headable  bool   // that paragraph can be the text of a setext heading
	)
	closeParagraph := func() { paragraph, headable = -1, false }
	addLine := func(number int, text string) {
		doc.blocks[paragraph].lines = append(doc.blocks[paragraph].lines, sourceLine{number, strings.TrimSpace(text)})
	}
	for i := start; i < len(all); i++ {
		number, rest, col := i+1, all[i], 0
		// The open containers the line continues.
		matched := 0
		for _, container := range stack {
			if strings.TrimSpace(rest) == "" && !container.quote {
				matched++
				continue
			}
			indent, after := leading(rest, col)
			if container.quote {
				if indent > 3 || !strings.HasPrefix(after, ">") {
					break
				}
				col += indent + 1
				rest, col = afterQuoteMarker(after[1:], col)
			} else {
				if indent < container.width {
					break
				}
				rest, col = strip(rest, col, container.width), col+container.width
			}
			matched++
		}
		blank := strings.TrimSpace(rest) == ""
		if matched < len(stack) {
			if paragraph >= 0 && !blank && !startsBlock(rest, col) {
				addLine(number, rest) // a lazy continuation line
				continue
			}
			stack, fence, html = stack[:matched], "", -1
			closeParagraph()
		}
		if fence != "" {
			if indent, after := leading(rest, col); indent <= 3 {
				if marker := fenceClose.FindStringSubmatch(after); marker != nil && marker[1][0] == fence[0] && len(marker[1]) >= len(fence) {
					fence = ""
				}
			}
			continue
		}
		if html >= 0 {
			block := htmlBlockTypes[html]
			if block.end == nil && blank {
				html = -1
				continue
			}
			if block.anchors {
				doc.html = append(doc.html, sourceLine{number, rest})
			}
			if block.end != nil && block.end.MatchString(rest) {
				html = -1
			}
			continue
		}
		// The containers the line opens.
		opened := false
		for {
			indent, after := leading(rest, col)
			if indent > 3 || after == "" {
				break
			}
			if after[0] == '>' {
				col += indent + 1
				rest, col = afterQuoteMarker(after[1:], col)
				stack, opened = append(stack, blockContainer{quote: true}), true
				continue
			}
			if thematicBreak.MatchString(after) {
				break
			}
			marker, gap, content, ok := listItem(after, col+indent, paragraph >= 0 && !opened)
			if !ok {
				break
			}
			width := indent + marker + gap
			stack, opened = append(stack, blockContainer{width: width}), true
			rest, col = content, col+width
		}
		if opened {
			closeParagraph()
		}
		indent, line := leading(rest, col)
		switch {
		case line == "":
			closeParagraph()
		case indent >= 4 && paragraph >= 0:
			addLine(number, line)
		case indent >= 4: // indented code
		case fenceOpen.MatchString(line) && !(line[0] == '`' && strings.Contains(fenceOpen.FindStringSubmatch(line)[2], "`")):
			fence = fenceOpen.FindStringSubmatch(line)[1]
			closeParagraph()
		case atxHeading.MatchString(line):
			closeParagraph()
			doc.blocks = append(doc.blocks, inlineBlock{isHeading: true, heading: atxHeading.FindStringSubmatch(line)[2], lines: []sourceLine{{number, line}}})
		case paragraph >= 0 && headable && setextUnderline.MatchString(line):
			block := &doc.blocks[paragraph]
			texts := make([]string, len(block.lines))
			for k, source := range block.lines {
				texts[k] = source.text
			}
			block.isHeading, block.heading = true, strings.Join(texts, " ")
			closeParagraph()
		case thematicBreak.MatchString(line):
			closeParagraph()
		case htmlBlockStart(line, paragraph >= 0) >= 0:
			closeParagraph()
			html = htmlBlockStart(line, false)
			block := htmlBlockTypes[html]
			if block.anchors {
				doc.html = append(doc.html, sourceLine{number, line})
			}
			if block.end != nil && block.end.MatchString(line) {
				html = -1
			}
		default:
			if paragraph < 0 {
				doc.blocks = append(doc.blocks, inlineBlock{})
				paragraph, headable = len(doc.blocks)-1, !strings.HasPrefix(line, "|")
			}
			addLine(number, line)
		}
	}
	return doc
}

// markdownAnchors is every fragment a link to the Markdown text can name: the heading slugs (a
// setext heading takes the whole paragraph above its underline), a repeated one numbered as GitHub
// numbers it, and the id and name attributes of the HTML elements it renders, inline or in raw HTML
// blocks. Code (fenced, indented, code spans), HTML comments and a frontmatter block contribute
// nothing.
func markdownAnchors(text string) map[string]bool {
	anchors := map[string]bool{}
	occurrences := map[string]int{}
	addHeading := func(heading string) {
		original := anchorSlug(heading)
		slug := original
		for {
			if _, used := occurrences[slug]; !used {
				break
			}
			occurrences[original]++
			slug = original + "-" + strconv.Itoa(occurrences[original])
		}
		occurrences[slug] = 0
		anchors[slug] = true
	}
	doc := parseMarkdown(text)
	for _, block := range doc.blocks {
		if block.isHeading {
			addHeading(block.heading)
		}
		for _, name := range htmlAnchors(maskCodeSpans(block.joined())) {
			anchors[name] = true
		}
	}
	for _, line := range doc.html {
		for _, name := range htmlAnchors(line.text) {
			anchors[name] = true
		}
	}
	return anchors
}

// fragmentOf is the decoded #fragment of a link target, and whether the target has one.
func fragmentOf(target string) (string, bool) {
	_, fragment, found := strings.Cut(target, "#")
	if !found || fragment == "" {
		return "", false
	}
	if unescaped, err := url.PathUnescape(fragment); err == nil {
		fragment = unescaped
	}
	return fragment, true
}

// anchorsOf reads the anchors of the Markdown file at path; the cache keeps a run to one read per file.
func anchorsOf(cache map[string]map[string]bool, path string) (map[string]bool, error) {
	if anchors, ok := cache[path]; ok {
		return anchors, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	anchors := markdownAnchors(string(data))
	cache[path] = anchors
	return anchors, nil
}

// repositoryFiles is every file of the checkout at root git lists, tracked or not yet tracked and
// not ignored, as repository-relative paths in order.
func repositoryFiles(root string) ([]string, error) {
	out, err := runGit(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	return sortedSet(strings.Split(string(out), "\x00")), nil
}

// The two messages the anchor check reports: a fragment the target does not name, and a target
// whose anchors could not be read.
const (
	invalidAnchorMessage    = ": invalid anchor "
	unreadableAnchorMessage = ": unreadable anchor target "
)

// anchorErrors is the invalid anchors among the links of the documents in names that the anchor
// check covers (anchorScope) and the link check reads (anchorChecked).
func anchorErrors(root string, names []string) ([]string, error) {
	var errs []string
	cache := map[string]map[string]bool{}
	for _, name := range names {
		if !anchorScope(name) || !anchorChecked(name) {
			continue
		}
		found, err := linkErrors(root, name, cache)
		if err != nil {
			return nil, err
		}
		for _, message := range found {
			if strings.Contains(message, invalidAnchorMessage) || strings.Contains(message, unreadableAnchorMessage) {
				errs = append(errs, message)
			}
		}
	}
	return errs, nil
}
