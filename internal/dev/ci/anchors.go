//go:build dev

package ci

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// The anchor check: a relative link's #fragment must name a heading slug or an explicit anchor of
// the file the link leads to. The slug follows GitHub's rule (lowercase, spaces to '-', punctuation
// dropped, a repeated slug gets -1, -2, ...), and an HTML id or name attribute counts as an anchor.
// It covers plugins/crw/skills/**/*.md and docs/**/*.md, the links to the same file and to another
// file alike; there is no allow-list.

var (
	atxHeading      = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	setextUnderline = regexp.MustCompile(`^ {0,3}(?:=+|-+)[ \t]*$`)
	htmlAnchor      = regexp.MustCompile(`<[A-Za-z][^<>]*?\b(?:id|name)[ \t]*=[ \t]*(?:"([^"]+)"|'([^']+)')`)
	inlineLink      = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	htmlTag         = regexp.MustCompile(`</?[A-Za-z][^<>]*>`)
	listOrQuote     = regexp.MustCompile(`^ {0,3}(?:[-*+]|[0-9]+[.)])[ \t]`)
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
	text := headingText(heading)
	var slug strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == ' ':
			slug.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r):
			slug.WriteRune(r)
		}
	}
	return slug.String()
}

// headingText is the text a heading renders: link and image markup reduced to their text, code
// spans unquoted, HTML tags, emphasis markers and backslash escapes dropped.
func headingText(heading string) string {
	text := inlineLink.ReplaceAllString(heading, "$1")
	text = htmlTag.ReplaceAllString(text, "")
	text = strings.NewReplacer("`", "", "*", "").Replace(text)
	runes := []rune(text)
	var out []rune
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\\' && i+1 < len(runes) && strings.ContainsRune("\\`*_{}[]()#+-.!|<>~\"'&", runes[i+1]):
			i++
			out = append(out, runes[i])
		case r == '_' && underscoreEmphasis(runes, i):
		default:
			out = append(out, r)
		}
	}
	return strings.TrimSpace(string(out))
}

// underscoreEmphasis reports whether the underscore at runes[i] opens or closes emphasis: it sits at
// a word edge. An underscore inside a word (snake_case) is text.
func underscoreEmphasis(runes []rune, i int) bool {
	word := func(j int) bool {
		return j >= 0 && j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsNumber(runes[j]))
	}
	return word(i-1) != word(i+1)
}

// markdownAnchors is every fragment a link to the Markdown text can name: the heading slugs, a
// repeated one numbered as GitHub numbers it, and the id and name attributes of HTML tags. Fenced
// code, and a frontmatter block, contribute nothing.
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
	fence := ""
	for i := start; i < len(all); i++ {
		line := all[i]
		if marker := fenceMarker.FindStringSubmatch(strings.TrimLeft(line, " \t")); marker != nil {
			delimiter := marker[1]
			if fence == "" {
				fence = delimiter
			} else if delimiter[0] == fence[0] && len(delimiter) >= len(fence) {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		for _, match := range htmlAnchor.FindAllStringSubmatch(line, -1) {
			anchors[match[1]+match[2]] = true
		}
		quoted := strings.TrimLeft(line, " ")
		for strings.HasPrefix(quoted, ">") {
			quoted = strings.TrimLeft(strings.TrimPrefix(quoted, ">"), " ")
		}
		if match := atxHeading.FindStringSubmatch(quoted); match != nil {
			addHeading(match[2])
			continue
		}
		if setextUnderline.MatchString(line) && i > start {
			previous := all[i-1]
			if strings.TrimSpace(previous) != "" && !listOrQuote.MatchString(previous) &&
				!strings.ContainsAny(previous[:1], "|>") && !atxHeading.MatchString(previous) &&
				!setextUnderline.MatchString(previous) && !fenceMarker.MatchString(strings.TrimLeft(previous, " \t")) {
				addHeading(strings.TrimSpace(previous))
			}
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
			if strings.Contains(message, ": invalid anchor ") {
				errs = append(errs, message)
			}
		}
	}
	return errs, nil
}
