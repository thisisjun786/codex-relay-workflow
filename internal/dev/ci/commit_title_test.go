//go:build dev

package ci

import (
	"strings"
	"testing"
)

// The commit-title warning of `crw-dev ci validate` (CRW-1511, rule 4 of
// plugins/crw/skills/crw-canon/references/naming.md): a commit title in the range under judgment that
// does not start with CRW-NNNN or OWNER/NAME#N (and an optional /X mark) and then ": " is named on
// stdout of a passing run, one line each. The warning is a record, never a refusal: the exit code stays 0.

// commitTitleWarning is the line a title outside the rule is reported with.
func commitTitleWarning(short, title string) string {
	return "warning: commit " + short + ` title "` + title + `" is outside the title rule ` +
		"(CRW-NNNN: text or OWNER/NAME#N: text, see plugins/crw/skills/crw-canon/references/naming.md)\n"
}

func TestCommitTitleOutsideTheRuleWarnsAndRecordsButNeverRefuses(t *testing.T) {
	r, base := largeBlobTestRepo(t)
	var want string
	conforming := []string{
		"CRW-1511: a title with the issue key",
		"CRW-1511/b: a title with a unit mark",
		"thisisjun786/codex-relay-workflow#42: a title with a repository reference",
		"owner/name#7/x1: a title with a repository reference and a mark",
	}
	outside := []string{
		"fix the thing",
		"CRW-1511 missing colon",
		"CRW-1511:no space after the colon",
		"CRW-: no number",
		"crw-1511: lower case key",
		"CRW-1511:  ",
	}
	for i, title := range conforming {
		largeBlobTestWrite(t, r, "c"+string(rune('a'+i)), []byte(title))
		r.git("add", "-A")
		r.git("commit", "-qm", title)
	}
	for i, title := range outside {
		largeBlobTestWrite(t, r, "o"+string(rune('a'+i)), []byte(title))
		r.git("add", "-A")
		r.git("commit", "-qm", title)
		want += commitTitleWarning(largeBlobTestShort(r, "HEAD"), strings.TrimSpace(title))
	}
	// An integration commit's subject and a merge commit are exempt.
	largeBlobTestWrite(t, r, "integration", []byte("integration"))
	r.git("add", "-A")
	r.git("commit", "-qm", "integration cc-q10152155: re-derive the version line")
	trunk := strings.TrimSpace(r.git("branch", "--show-current"))
	r.git("switch", "-q", "-c", "side", base)
	largeBlobTestWrite(t, r, "side", []byte("side"))
	r.git("add", "-A")
	r.git("commit", "-qm", "CRW-1511: side work")
	r.git("switch", "-q", trunk)
	r.git("merge", "-q", "--no-ff", "-m", "Merge branch side", "side")

	got := largeBlobTestValidate(t, r.root, "pull_request", base)
	expectEqual(t, "pull request", got, result{0, validated + largeBlobTestSince(base) + want, ""})
	// The same range with no base is not judged, and the exit code stays 0 either way.
	expectEqual(t, "no base", validate(t, r), result{0, validated + largeBlobTestFull, ""})
}
