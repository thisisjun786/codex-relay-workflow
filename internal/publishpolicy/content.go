// Package publishpolicy owns content checks on the exact final bytes sent to a forge.
package publishpolicy

// CRW-783: the secret patterns the guard looks for, and the name=value shape marking a credential or dump.

import (
	"encoding/json"
	"regexp"
	"strings"
)

// SecretLine is the 1-based number of the first line holding a secret pattern, and whether one does.
func SecretLine(text string) (int, bool) {
	patterns := secretPatterns()
	lines := strings.Split(text, "\n")
	run, start := 0, 0
	for i, line := range lines {
		for _, pattern := range patterns {
			if pattern.MatchString(line) {
				return i + 1, true
			}
		}
		name, assignment := assignment(line)
		if assignment && secretName(name) {
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

// secretPatterns is the key prefixes and the private-key header, compiled where they are used.
func secretPatterns() []*regexp.Regexp {
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

// assignment is the name of a NAME=value line, an optional leading export allowed.
func assignment(line string) (string, bool) {
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

// secretName is whether an assignment's name holds one of the words that mark a credential, in any case.
func secretName(name string) bool {
	upper := strings.ToUpper(name)
	for _, word := range [...]string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD"} {
		if strings.Contains(upper, word) {
			return true
		}
	}
	return false
}

// RawSecretLine checks raw final bytes, including assignment shapes in JSON strings.
func RawSecretLine(content string) (int, bool) {
	if line, found := SecretLine(content); found {
		return line, true
	}
	if !json.Valid([]byte(content)) {
		return 0, false
	}
	split := strings.NewReplacer("\"", "\n", "{", "\n", "}", "\n", "[", "\n", "]", "\n", ",", "\n", "\\n", "\n")
	for i, line := range strings.Split(content, "\n") {
		for _, part := range strings.Split(split.Replace(line), "\n") {
			if name, ok := assignment(part); ok && secretName(name) {
				return i + 1, true
			}
		}
	}
	return 0, false
}
