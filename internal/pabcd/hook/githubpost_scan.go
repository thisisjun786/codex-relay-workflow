package hook

// CRW-783: the secret patterns the GitHub post guard looks for in the text bound for GitHub, and the
// name=value shape that marks a credential or an environment dump. Only the line is reported: the deny
// reason never carries the value or the pattern that matched.

import (
	"regexp"
	"strings"
)

// githubPostSecretLine is the 1-based number of the first line of the text that holds a secret pattern,
// and whether one does.
func githubPostSecretLine(text string) (int, bool) {
	patterns := githubPostSecretPatterns()
	lines := strings.Split(text, "\n")
	run, start := 0, 0
	for i, line := range lines {
		for _, pattern := range patterns {
			if pattern.MatchString(line) {
				return i + 1, true
			}
		}
		name, assignment := githubPostAssignment(line)
		if assignment && githubPostSecretName(name) {
			return i + 1, true
		}
		if assignment {
			if run == 0 {
				start = i
			}
			if run++; run >= 3 {
				return start + 1, true
			}
		} else {
			run = 0
		}
	}
	return 0, false
}

// githubPostSecretPatterns is the key prefixes and the private-key header, compiled where they are used
// rather than at package level. Each length is the shortest the pattern's own shape allows, so that a
// name in prose is not read as a key.
func githubPostSecretPatterns() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`AIza[A-Za-z0-9_-]{30,}`),
		regexp.MustCompile(`(gh[pousr]_|github_pat_)[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`xox[abposr]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	}
}

// githubPostAssignment is the name of a NAME=value line, an optional leading export allowed, and whether
// the line is one.
func githubPostAssignment(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if rest, ok := strings.CutPrefix(s, "export"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
		s = strings.TrimLeft(rest, " \t")
	}
	name, value, found := strings.Cut(s, "=")
	if !found || name == "" || value == "" {
		return "", false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return "", false
		}
	}
	return name, true
}

// githubPostSecretName is whether an assignment's name holds one of the words that mark a credential, in
// any case.
func githubPostSecretName(name string) bool {
	upper := strings.ToUpper(name)
	for _, word := range [...]string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD"} {
		if strings.Contains(upper, word) {
			return true
		}
	}
	return false
}
