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
// element counts as an anchor. Fenced code, indented code and code spans contribute nothing. It
// covers plugins/crw/skills/**/*.md and docs/**/*.md, the links to the same file and to another
// file alike; there is no allow-list.

var (
	atxHeading      = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	setextUnderline = regexp.MustCompile(`^ {0,3}(?:=+|-+)[ \t]*$`)
	thematicBreak   = regexp.MustCompile(`^ {0,3}(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	listItemStart   = regexp.MustCompile(`^([-*+]|[0-9]{1,9}[.)])( *)(?:\t|$|\S)`)
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

// maskCodeSpans is line with its code spans (backtick runs that close on the line) blanked out.
func maskCodeSpans(line string) string {
	runes := []rune(line)
	for i := 0; i < len(runes); {
		if runes[i] != '`' {
			i++
			continue
		}
		n := 1
		for i+n < len(runes) && runes[i+n] == '`' {
			n++
		}
		if end, _, ok := codeSpanEnd(runes, i, n); ok {
			for k := i; k < end; k++ {
				runes[k] = ' '
			}
			i = end
		} else {
			i += n
		}
	}
	return string(runes)
}

// htmlAnchors is the id and name attribute values of the HTML elements opening on line, outside
// code spans. An attribute whose name only ends in id (data-id) is not one.
func htmlAnchors(line string) []string {
	var found []string
	for _, tag := range htmlOpenTag.FindAllString(maskCodeSpans(line), -1) {
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

// indentation is the width of the leading white space of line (a tab reaches the next multiple of
// four) and the rest of the line.
func indentation(line string) (int, string) {
	width := 0
	for i, r := range line {
		switch r {
		case ' ':
			width++
		case '\t':
			width += 4 - width%4
		default:
			return width, line[i:]
		}
	}
	return width, ""
}

// markdownAnchors is every fragment a link to the Markdown text can name: the heading slugs (a
// setext heading takes the whole paragraph above its underline), a repeated one numbered as GitHub
// numbers it, and the id and name attributes of HTML elements. Fenced code, indented code, code
// spans and a frontmatter block contribute nothing.
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
		fence       string   // the open fence's delimiter
		listColumn  = -1     // the column the content of the open list item starts at
		blank       = true   // the previous line was blank
		inParagraph bool     // the previous line is a paragraph line, which an indented line continues
		headable    bool     // that paragraph can be the text of a setext heading
		paragraph   []string // its lines
	)
	endBlock := func() { inParagraph, headable, paragraph = false, false, nil }
	for i := start; i < len(all); i++ {
		line := all[i]
		if fence != "" {
			if marker := fenceMarker.FindStringSubmatch(strings.TrimLeft(line, " \t")); marker != nil &&
				marker[1][0] == fence[0] && len(marker[1]) >= len(fence) {
				fence = ""
			}
			continue
		}
		wasBlank := blank
		blank = strings.TrimSpace(line) == ""
		if blank {
			endBlock()
			continue
		}
		indent, rest := indentation(line)
		if listColumn >= 0 && indent < listColumn && wasBlank && !listItemStart.MatchString(rest) {
			listColumn = -1
		}
		relative := indent
		if listColumn >= 0 && indent >= listColumn {
			relative = indent - listColumn
		}
		if relative >= 4 && !inParagraph {
			continue // indented code
		}
		if relative >= 4 {
			if headable {
				paragraph = append(paragraph, strings.TrimSpace(rest))
			}
			for _, name := range htmlAnchors(rest) {
				anchors[name] = true
			}
			continue
		}
		if marker := fenceMarker.FindStringSubmatch(rest); marker != nil {
			fence = marker[1]
			if indent < listColumn {
				listColumn = -1
			}
			endBlock()
			continue
		}
		for _, name := range htmlAnchors(rest) {
			anchors[name] = true
		}
		quoted := rest
		for strings.HasPrefix(quoted, ">") {
			quoted = strings.TrimLeft(strings.TrimPrefix(quoted, ">"), " ")
		}
		if quoted != rest {
			if match := atxHeading.FindStringSubmatch(quoted); match != nil {
				addHeading(match[2])
				endBlock()
			} else {
				inParagraph, headable, paragraph = true, false, nil
			}
			continue
		}
		if match := atxHeading.FindStringSubmatch(rest); match != nil {
			addHeading(match[2])
			if indent < listColumn {
				listColumn = -1
			}
			endBlock()
			continue
		}
		if setextUnderline.MatchString(rest) && inParagraph && headable && len(paragraph) > 0 {
			addHeading(strings.Join(paragraph, " "))
			endBlock()
			continue
		}
		if thematicBreak.MatchString(rest) {
			endBlock()
			continue
		}
		if item := listItemStart.FindStringSubmatch(rest); item != nil {
			gap := len(item[2])
			if gap == 0 || gap > 4 || strings.TrimSpace(rest[len(item[1]):]) == "" {
				gap = 1
			}
			listColumn = indent + len(item[1]) + gap
			inParagraph, headable, paragraph = true, false, nil
			continue
		}
		if !inParagraph {
			inParagraph, headable, paragraph = true, !strings.HasPrefix(rest, "|"), nil
		}
		if headable {
			paragraph = append(paragraph, strings.TrimSpace(rest))
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
