//go:build dev

package ci

import (
	"strings"
	"testing"
)

// An unknown check is a usage error that names the argument and the checks there are.
func TestAnUnknownCheckIsNamed(t *testing.T) {
	got := runCommand(t, repoRoot(), nil, crwDev, "ci", "nope\t")
	line := `crw-dev ci: error: invalid choice: "nope\t" (choose from contracts, operations, plugin, validate)` + "\n"
	if got.code != 2 || !strings.HasSuffix(got.stderr, line) {
		t.Errorf("crw-dev ci nope: exit %d\n%s\nwant the line %s", got.code, got.stderr, line)
	}
}

// A check's usage error exits 2 and its help, which lists every flag, exits 0.
func TestACheckListsItsFlags(t *testing.T) {
	got := runCommand(t, repoRoot(), nil, crwDev, "ci", "plugin", "--help")
	for _, flag := range []string{"-json", "-payload", "-record-version", "-revision"} {
		if got.code != 0 || !strings.Contains(got.stdout, flag) {
			t.Errorf("plugin --help lacks %s: %+v", flag, got)
		}
	}
	got = runCommand(t, repoRoot(), nil, crwDev, "ci", "plugin", "--bogus")
	if got.code != 2 || !strings.Contains(got.stderr, "flag provided but not defined: -bogus") {
		t.Errorf("plugin --bogus: %+v", got)
	}
	got = runCommand(t, repoRoot(), nil, crwDev, "ci", "validate", "extra")
	if got.code != 2 || !strings.Contains(got.stderr, "unexpected arguments: extra") {
		t.Errorf("validate extra: %+v", got)
	}
}
