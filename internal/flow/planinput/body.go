package planinput

import (
	"path"
	"regexp"
	"strings"

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
		paragraph   []string
		kind        int
		itemText    int
		fence       string
		htmlComment bool
	}{}
	emit := func(lines ...string) {
		if excludedDepth == 0 && len(out) > 0 {
			out[len(out)-1].lines = append(out[len(out)-1].lines, lines...)
		}
	}
	flush := func() {
		emit(state.paragraph...)
		state.paragraph, state.kind = nil, noParagraph
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
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimRight(raw, "\r")
		rest := strings.TrimLeft(line, " \t")
		col := indentOf(line[:len(line)-len(rest)])
		inItem := state.itemText > 0 && col >= state.itemText
		if state.htmlComment {
			state.htmlComment = headingCommentOpen(line, true)
			emit(line)
			continue
		}
		t := strings.TrimLeft(line, " ")
		marker := ""
		switch {
		case strings.HasPrefix(t, "```"):
			marker = "```"
		case strings.HasPrefix(t, "~~~"):
			marker = "~~~"
		}
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
		if col <= 3 && strings.HasPrefix(rest, "<!--") {
			flush()
			if !inItem {
				state.itemText = 0
			}
			state.htmlComment = headingCommentOpen(rest, false)
			emit(line)
			continue
		}
		if m := headingLine.FindStringSubmatch(line); m != nil {
			flush()
			heading(m[2], len(m[1]))
			continue
		}
		if state.kind == rootParagraph && setextLine.MatchString(line) {
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
			state.paragraph, state.kind = nil, noParagraph
			heading(title, depth)
			continue
		}
		switch m := setextListLine.FindStringSubmatch(rest); {
		case rest == "":
			flush()
			emit(line)
		case col <= 3 && setextBreakLine.MatchString(rest):
			flush()
			if !inItem {
				state.itemText = 0
			}
			emit(line)
		case col <= 3 && m != nil:
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
		case col <= 3 && strings.HasPrefix(rest, ">"):
			flush()
			if !inItem {
				state.itemText = 0
			}
			text := strings.TrimSpace(rest)
			for strings.HasPrefix(text, ">") {
				text = strings.TrimSpace(strings.TrimPrefix(text, ">"))
			}
			if setextParagraph(text) && !strings.HasPrefix(text, "```") &&
				!strings.HasPrefix(text, "~~~") && !strings.HasPrefix(text, "<!--") {
				state.kind = quoteParagraph
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
