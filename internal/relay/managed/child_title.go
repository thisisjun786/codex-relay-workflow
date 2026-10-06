package managed

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// createTitleByteLimit is the byte length the bridge accepts for a create's title: its own limit,
// checked in internal/bridge/create.go before the host is asked, where nonempty("title", 500) is
// measured in bytes. childTitle reads the same number so the prefix it adds can never be the reason
// a creation is refused. It is a copy, not a shared symbol: the bridge owns the check.
const createTitleByteLimit = 500

// childTitle is the name the managed engine sends the host for a child: the issue key, then the
// task title, so the Codex app list tells one child from another (crw-run's task-packet rule).
//
// A title that already names the issue key is sent as the parent wrote it, so a request that
// arrived prefixed is not prefixed twice. "Already names the key" means the title begins with
// the key and what follows it is the end of the title or a character that is neither a letter
// nor a digit: "CRW-697 · x", "CRW-697: x" and "CRW-697 x" are kept, while "CRW-6977 x"
// and "CRW-69 · x" are not this key and take the prefix.
//
// The prefix is added only when the prefixed value fits the bridge's title limit; a title whose
// prefixed value would be longer is sent unchanged. That is exactly the value the parent wrote,
// so the prefix never turns a request the bridge accepted into one it refuses, and a title that
// the bridge already refused stays refused - the rune/byte mismatch between this request's bound
// and the bridge's is not this function's to fix.
//
// An empty title sends no name at all, as it did before this rule existed. A managed-start
// request cannot carry one (child.title is required and non-blank), so this is a guard on the
// function's own contract rather than a case a caller reaches.
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
	prefixed := issueKey + " · " + title
	if len(prefixed) > createTitleByteLimit {
		return title
	}
	return prefixed
}
