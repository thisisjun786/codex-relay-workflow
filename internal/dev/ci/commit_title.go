//go:build dev

package ci

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// The commit-title warning of `crw-dev ci validate` (CRW-1511, rule 4 of
// plugins/crw/skills/crw-canon/references/naming.md). A commit title starts with the work it belongs to:
// a CRW-NNNN key or an OWNER/NAME#N reference, an optional /X mark, then ": " and text. The check reads
// the titles of the commits the large-blob range adds, so it judges a range only when that range has a
// base (BLOB_RANGE_BASE, or the base a local run substitutes); with no base nothing is scanned. It only
// warns: a title outside the rule is named on stdout of a passing run and never refuses the run. Branch
// names and pull request titles are not read.

// commitTitleRule is the title a commit starts with.
var commitTitleRule = regexp.MustCompile(`^(CRW-\d+|[\w.-]+/[\w.-]+#\d+)(/[A-Za-z0-9]+)?: \S`)

// commitTitleExemptPrefix starts the subject of an integration commit, which the integrator writes.
const commitTitleExemptPrefix = "integration cc-q"

// commitTitleLimit is the longest title a warning line quotes, in runes.
const commitTitleLimit = 120

// commitTitleOutside is whether a non-merge commit's subject is outside the rule.
func commitTitleOutside(subject string) bool {
	return !commitTitleRule.MatchString(subject) && !strings.HasPrefix(subject, commitTitleExemptPrefix)
}

// commitTitleQuote is a subject made safe for one line of stdout: control characters become spaces and a
// long title is cut.
func commitTitleQuote(subject string) string {
	subject = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, subject)
	subject = strings.TrimSpace(strings.ToValidUTF8(subject, "�"))
	if runes := []rune(subject); len(runes) > commitTitleLimit {
		subject = string(runes[:commitTitleLimit]) + "..."
	}
	return subject
}

// commitTitleWarnings are the warning lines for the non-merge commits base..HEAD adds, oldest first, one
// per title outside the rule. A git failure is one more warning line: the check never refuses.
func commitTitleWarnings(root, base string) []string {
	out, err := runGit(root, "log", "--no-merges", "--reverse", "--format=%h%x00%s", base+"..HEAD")
	if err != nil {
		return []string{"warning: commit titles were not checked: " + largeBlobOneLine(err.Error())}
	}
	var warnings []string
	for _, line := range lines(string(out)) {
		short, subject, found := strings.Cut(line, "\x00")
		if !found || !commitTitleOutside(subject) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("warning: commit %s title %q is outside the title rule "+
			"(CRW-NNNN: text or OWNER/NAME#N: text, see plugins/crw/skills/crw-canon/references/naming.md)", short, commitTitleQuote(subject)))
	}
	return warnings
}
