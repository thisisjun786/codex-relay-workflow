package quote

import "strings"

// Shell is shlex.quote: s as one word a POSIX shell reads back unchanged. A string made only of
// ASCII letters, digits and @%+=:,./-_ is its own word, the empty string is ”, and anything else
// is single-quoted with each ' written '"'"'. A recovery line a message shows a person is built
// from it, so the person can paste the line.
func Shell(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !shellSafe(r) {
			return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
		}
	}
	return s
}

func shellSafe(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)
}
