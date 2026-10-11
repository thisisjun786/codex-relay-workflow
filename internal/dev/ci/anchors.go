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
	"unicode/utf8"
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

// anchorSlug is the GitHub slug of one heading's text, before a repeated slug gets its number; no
// link reference definition is known to it.
func anchorSlug(heading string) string { return slugOf(heading, nil) }

// slugOf is anchorSlug with the labels of the link reference definitions of the document (see
// referenceLabels): a reference link to one of them renders as its text.
func slugOf(heading string, definitions map[string]bool) string {
	var slug strings.Builder
	for _, r := range strings.ToLower(headingText(heading, definitions)) {
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
func headingText(heading string, definitions map[string]bool) string {
	return strings.TrimSpace(renderInline(heading, definitions))
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
func renderInline(s string, definitions map[string]bool) string {
	tokens := inlineTokens([]rune(s), definitions)
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

// normalizeLabel is a link label as CommonMark matches it: white space runs collapsed, case folded.
func normalizeLabel(label string) string {
	return strings.ToLower(strings.Join(strings.Fields(label), " "))
}

// referenceLabels is the normalized labels of the link reference definitions that open the
// paragraphs of the document (see linkDefinition).
func referenceLabels(blocks []inlineBlock) map[string]bool {
	labels := map[string]bool{}
	for _, block := range blocks {
		if block.isHeading {
			continue
		}
		for rest := block.joined(); ; {
			label, n, ok := linkDefinition(rest)
			if !ok {
				break
			}
			labels[label] = true
			rest = rest[n:]
		}
	}
	return labels
}

// linkDefinition reads the link reference definition s starts with, as CommonMark reads one: a
// label in brackets (some non-space text of at most 999 characters, no unescaped bracket), ':', white space holding at most one
// line end, a destination (in <> without a line end or an unescaped <, or plain text without spaces
// or control characters whose parentheses balance), and, after white space, an optional title in
// double quotes, single quotes or parentheses; nothing but spaces may follow on the line. When the
// title is not well formed or text follows it, the definition ends with its destination if nothing
// follows that on its line. It
// returns the normalized label and the length of s the definition takes, up to its line end.
func linkDefinition(s string) (label string, n int, ok bool) {
	if !strings.HasPrefix(s, "[") {
		return "", 0, false
	}
	i, characters := 1, 0 // the label holds at most 999 characters (not UTF-8 bytes)
	for ; ; i++ {
		if i >= len(s) || s[i] == '[' || characters > 999 {
			return "", 0, false
		}
		if s[i] == '\\' && i+1 < len(s) && isMarkdownPunctuation(rune(s[i+1])) {
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
	label = normalizeLabel(s[1:i])
	if label == "" || i+1 >= len(s) || s[i+1] != ':' {
		return "", 0, false
	}
	i = skipDefinitionSpace(s, i+2)
	destination := i
	if i < len(s) && s[i] == '<' {
		for i++; ; i++ {
			if i >= len(s) || s[i] == '\n' || s[i] == '<' {
				return "", 0, false
			}
			if s[i] == '\\' && i+1 < len(s) && isMarkdownPunctuation(rune(s[i+1])) {
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
			case s[i] == '\\' && i+1 < len(s) && isMarkdownPunctuation(rune(s[i+1])):
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
			return "", 0, false
		}
	}
	if title := skipDefinitionSpace(s, i); title > i && title < len(s) && strings.ContainsRune("\"'(", rune(s[title])) {
		if end, ok := definitionTitleEnd(s, title); ok {
			if line, ok := lineRestBlank(s, end); ok {
				return label, line, true
			}
		}
	}
	if line, ok := lineRestBlank(s, i); ok {
		return label, line, true
	}
	return "", 0, false
}

// skipDefinitionSpace is the index after the spaces and tabs at s[i:], with at most one line end.
func skipDefinitionSpace(s string, i int) int {
	lineEnd := false
	for ; i < len(s); i++ {
		if s[i] == '\n' {
			if lineEnd {
				break
			}
			lineEnd = true
		} else if s[i] != ' ' && s[i] != '\t' {
			break
		}
	}
	return i
}

// definitionTitleEnd is the index after the title that opens at s[open] (in double quotes, single
// quotes or parentheses), and whether it is closed; in a () title an unescaped '(' is not allowed.
func definitionTitleEnd(s string, open int) (int, bool) {
	closing := s[open]
	if closing == '(' {
		closing = ')'
	}
	for i := open + 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && isMarkdownPunctuation(rune(s[i+1])):
			i++
		case s[i] == closing:
			return i + 1, true
		case s[open] == '(' && s[i] == '(':
			return 0, false
		}
	}
	return 0, false
}

// lineRestBlank is the index after the line end that ends s[i:] (or len(s)), and whether only
// spaces and tabs come before it.
func lineRestBlank(s string, i int) (int, bool) {
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

func inlineTokens(runes []rune, definitions map[string]bool) []inlineToken {
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
					literal.WriteString(renderInline(string(runes[open+1:closing]), definitions))
					i = end + 1
					continue
				}
			} else if closing > 0 && closing+1 < len(runes) && runes[closing+1] == '[' {
				// A full or collapsed reference link: [text][label] or [text][] with a definition of
				// the label; the label is no part of the rendered text. As in a definition, the label
				// holds at most 999 characters as written, before its white space is collapsed.
				if end := matchingBracket(runes, closing+1); end > 0 {
					label := runes[closing+2 : end]
					if len(label) == 0 {
						label = runes[open+1 : closing]
					}
					if len(label) <= 999 && definitions[normalizeLabel(string(label))] {
						literal.WriteString(renderInline(string(runes[open+1:closing]), definitions))
						i = end + 1
						continue
					}
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

var htmlOpenTagAt = regexp.MustCompile(`^(?:` + htmlOpenTag.String() + `)`)

// htmlAnchors is the id and name attribute values (character references decoded, as the browser reads
// them) of the HTML elements opening in text, which holds no code (maskCodeSpans). An attribute whose
// name only ends in id (data-id) is not one. The text is read from left to right: an element's tag,
// quoted attribute values and all, is taken whole, so '<!--' or '-->' inside a value is part of the
// value; an HTML comment opens only where markup can open and holds no element. In a raw HTML block
// (block set) a comment that is never closed runs to the end of the block; in a paragraph it is
// text, and so is a backslash-escaped '<'.
func htmlAnchors(text string, block bool) []string {
	var found []string
	for i := 0; i < len(text); {
		switch {
		case !block && text[i] == '\\' && i+1 < len(text) && isMarkdownPunctuation(rune(text[i+1])):
			i += 2
			continue
		case text[i] != '<':
			i++
			continue
		case strings.HasPrefix(text[i:], "<!--"):
			if end := strings.Index(text[i+2:], "-->"); end >= 0 {
				i += 2 + end + len("-->")
				continue
			}
			if block {
				return found
			}
			i++
			continue
		}
		loc := htmlOpenTagAt.FindStringIndex(text[i:])
		if loc == nil {
			i++
			continue
		}
		tag := text[i : i+loc[1]]
		i += loc[1]
		name := strings.IndexAny(tag, " \t/>")
		for _, attribute := range htmlAttribute.FindAllStringSubmatch(tag[name:], -1) {
			if key := strings.ToLower(attribute[1]); key != "id" && key != "name" {
				continue
			}
			if value := html.UnescapeString(attribute[2] + attribute[3] + attribute[4]); value != "" {
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
	html   [][]string // the lines of each raw HTML block whose elements are rendered
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
				doc.html[len(doc.html)-1] = append(doc.html[len(doc.html)-1], rest)
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
				doc.html = append(doc.html, []string{line})
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
	doc := parseMarkdown(text)
	definitions := referenceLabels(doc.blocks)
	addHeading := func(heading string) {
		original := slugOf(heading, definitions)
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
	for _, block := range doc.blocks {
		if block.isHeading {
			addHeading(block.heading)
		}
		for _, name := range htmlAnchors(maskCodeSpans(block.joined()), false) {
			anchors[name] = true
		}
	}
	for _, lines := range doc.html {
		for _, name := range htmlAnchors(strings.Join(lines, "\n"), true) {
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
