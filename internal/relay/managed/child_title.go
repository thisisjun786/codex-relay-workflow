package managed

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// childTitle is the name the managed engine sends the host for a child: the issue key, then the
// task title, so the Codex app list tells one child from another (crw-run's task-packet rule).
//
// A title that already names the issue key is sent as the parent wrote it, so a request that
// arrived prefixed is not prefixed twice. "Already names the key" means the title begins with
// the key and what follows it is the end of the title or a character that is neither a letter
// nor a digit: "CRW-697 · x", "CRW-697: x" and "CRW-697 x" are kept, while "CRW-6977 x"
// and "CRW-69 · x" are not this key and take the prefix. An empty title sends no name at all,
// as it did before this rule existed.
//
// The stored managed request and its fingerprint are never rewritten: only the value handed to
// the host is normalized, so a repeat of the same request stays a replay.
func childTitle(issueKey, title string) string {
	if title == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(title, issueKey); ok {
		if rest == "" {
			return title
		}
		first, _ := utf8.DecodeRuneInString(rest)
		if !unicode.IsLetter(first) && !unicode.IsDigit(first) {
			return title
		}
	}
	return issueKey + " · " + title
}
