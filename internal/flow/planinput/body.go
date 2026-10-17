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
	headingLine  = regexp.MustCompile(`^ {0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	bulletLine   = regexp.MustCompile(`^(\s*)(?:[-*+]|\d+[.)])(?:\s+(.*))?$`)
	checkbox     = regexp.MustCompile(`^\[[ xX]\]\s*`)
	blockedLine  = regexp.MustCompile(`(?i)^(?:\*\*)?blocked by:`)
	sizeFieldRe  = regexp.MustCompile("^(size_fit|size_reason|issue_size)\\s*:\\s*`?([A-Za-z0-9_]+)`?\\s*$")
	backtickSpan = regexp.MustCompile("`([^`]+)`")
)

// section is the non-fence lines under one heading.
type section struct {
	heading string
	lines   []string
}

// sections splits a body at its headings. Text before the first heading belongs to no section.
func sections(body string) []section {
	var out []section
	fence := ""
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		t := strings.TrimLeft(line, " ")
		marker := ""
		switch {
		case strings.HasPrefix(t, "```"):
			marker = "```"
		case strings.HasPrefix(t, "~~~"):
			marker = "~~~"
		}
		if marker != "" {
			switch fence {
			case "":
				fence = marker
			case marker:
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if m := headingLine.FindStringSubmatch(line); m != nil {
			out = append(out, section{heading: normalizeHeading(m[1])})
			continue
		}
		if len(out) > 0 {
			out[len(out)-1].lines = append(out[len(out)-1].lines, line)
		}
	}
	return out
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
	lines, _ := linesOf(secs, criteriaHeadings)
	return bullets(lines, true)
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
