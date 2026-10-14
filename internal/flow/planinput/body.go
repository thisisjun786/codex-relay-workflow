package planinput

import (
	"path"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/skill"
)

// The headings of an issue body that carry an issue line's fields. A heading is matched without regard to case, level, bold marks and a
// trailing colon; a section runs to the next heading, and a heading inside a code fence is not a heading.
var (
	criteriaHeadings = []string{"끝 조건", "done when", "acceptance", "acceptance criteria"}
	regionHeadings   = []string{"수정 범위", "edit regions", "regions"}
	sizeHeadings     = []string{"크기", "size", "size fit"}
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

// linesOf joins the lines of every section whose heading is one of names.
func linesOf(secs []section, names []string) (lines []string, found bool) {
	for _, s := range secs {
		for _, n := range names {
			if s.heading == n {
				found = true
				lines = append(lines, s.lines...)
			}
		}
	}
	return lines, found
}

// bullets are the list items of lines, without marker and checkbox. An indented line that follows an item directly continues it; a
// "Blocked by:" item is an order and not an item.
func bullets(lines []string) []string {
	var out []string
	cur := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			cur = -1
			continue
		}
		if m := bulletLine.FindStringSubmatch(line); m != nil {
			text := strings.TrimSpace(checkbox.ReplaceAllString(strings.TrimSpace(m[2]), ""))
			if blockedLine.MatchString(text) {
				cur = -1
				continue
			}
			out = append(out, text)
			cur = len(out) - 1
			continue
		}
		if cur >= 0 && (line[0] == ' ' || line[0] == '\t') {
			out[cur] = strings.TrimSpace(out[cur] + " " + strings.TrimSpace(line))
			continue
		}
		cur = -1
	}
	kept := out[:0]
	for _, b := range out {
		if b != "" {
			kept = append(kept, b)
		}
	}
	return kept
}

// criteria are the bullets of the criteria sections.
func criteria(secs []section) []string {
	lines, _ := linesOf(secs, criteriaHeadings)
	return bullets(lines)
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
