package tracking

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	blockedByLine = regexp.MustCompile(`(?i)^\s*(?:[-*+]\s+)?(?:\*\*)?blocked by:(?:\*\*)?(.*)$`)
	carriesLine   = regexp.MustCompile(`(?i)^\s*(?:[-*+]\s+)?(?:\*\*)?carries:(?:\*\*)?(.*)$`)
	blockedByRef  = regexp.MustCompile(`(?:^|[\s,;(\[])([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#([0-9]+)\b`)
)

// ParseBlockedBy returns the issues named on the "Blocked by:" lines of an issue body, in order and
// once each. A line is "Blocked by:" at its start, after an optional list marker or bold, followed by
// references "#N" or "<owner>/<name>#N"; a bare "#N" means issue N of repo, the repository of the
// issue the body belongs to. Lines inside a code fence are not read.
func ParseBlockedBy(repo, body string) []Ref { return parseRefLines(blockedByLine, repo, body) }

// ParseCarries returns the issues named on the "Carries:" lines of a pull request body (the line OpenPullRequest writes), read as
// ParseBlockedBy reads its lines.
func ParseCarries(repo, body string) []Ref { return parseRefLines(carriesLine, repo, body) }

// parseRefLines returns the references on the lines of body that line matches, outside code fences, in order and once each.
func parseRefLines(line *regexp.Regexp, repo, body string) []Ref {
	var out []Ref
	seen := map[Ref]bool{}
	fence := ""
	for _, text := range strings.Split(body, "\n") {
		text = strings.TrimRight(text, "\r")
		if marker := fenceMarker(text); marker != "" {
			switch {
			case fence == "":
				fence = marker
			case fence == marker:
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		m := line.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		for _, r := range blockedByRef.FindAllStringSubmatch(m[1], -1) {
			n, err := strconv.Atoi(r[2])
			if err != nil || n <= 0 {
				continue
			}
			ref := Ref{Repo: repo, Number: n}
			if r[1] != "" && validRepo(r[1]) && !strings.EqualFold(r[1], repo) {
				ref.Repo = r[1]
			}
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	return out
}

// fenceMarker is "```" or "~~~" when line opens or closes a code fence, else "".
func fenceMarker(line string) string {
	t := strings.TrimLeft(line, " ")
	switch {
	case strings.HasPrefix(t, "```"):
		return "```"
	case strings.HasPrefix(t, "~~~"):
		return "~~~"
	}
	return ""
}
