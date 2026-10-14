//go:build dev

package ci

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CRW-1228: the four operator documents describe CRW as the only workflow plugin, in the present tense.
// CXC v0.2.40 is where the port came from (NOTICE, docs/port-cxc/provenance.md) and CRW runs without it:
// no sentence offers CXC beside CRW, a way back to it or the switch's `cxc` value as anything but the
// ported hooks turned off (Jun 10-11: CXC removed, fix forward). Restoring a file from its backup and
// moving the runtime pointer back to an earlier CRW runtime stay allowed.

// crwOnlyDocs are the documents CRW-1228 holds to that wording.
var crwOnlyDocs = []string{"README.md", "AGENTS.md", "docs/runtime-install.md", "docs/plugin-packaging.md"}

// cxcCoexistence is the wording of the time CRW and CXC were installed together: the grep set of the
// issue and its 10-10 comment, and the dependency and way-back sentences the issue names.
var cxcCoexistence = regexp.MustCompile(`(?i)side by side|no hook reads|not in the installer yet|installed CXC|CXC-owned|return to CXC|` +
	`back to CXC|CXC's own plugin serves|until the port is switched on|designed to work with CXC|CXC remains a separate dependency|` +
	`separately installed CXC|referenced CXC skills|codexclaw installation guide`)

// cxcCoexistenceProblems are the lines of text that still read as CXC beside CRW.
func cxcCoexistenceProblems(name, text string) []string {
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		if m := cxcCoexistence.FindString(line); m != "" {
			problems = append(problems, name+":"+strconv.Itoa(i+1)+": "+m)
		}
	}
	return problems
}

func TestCRWOnlyDocsNameNoCoexistence(t *testing.T) {
	for _, doc := range crwOnlyDocs {
		for _, p := range cxcCoexistenceProblems(doc, readRepoText(t, doc)) {
			t.Error(p)
		}
	}
	t.Run("fixture: the grep set is caught and the origin sentence is not", func(t *testing.T) {
		if p := cxcCoexistenceProblems("x.md", "CRW and CXC stay installed side by side.\nreplaces the CXC-owned file\n"); len(p) != 2 {
			t.Errorf("problems = %v, want two", p)
		}
		if p := cxcCoexistenceProblems("x.md", "CXC v0.2.40 is the origin of the port and CRW runs without it.\n"); len(p) != 0 {
			t.Errorf("the origin sentence is reported: %v", p)
		}
	})
}

// TestCRWOnlyDocsNameTheOrigin: README and AGENTS say what CXC is now, the origin of the port, and that
// CRW runs without it.
func TestCRWOnlyDocsNameTheOrigin(t *testing.T) {
	origin := regexp.MustCompile(`CXC v0\.2\.40.{0,60}origin of the port.{0,200}NOTICE.{0,200}provenance\.md.{0,120}CRW runs without it`)
	for _, doc := range []string{"README.md", "AGENTS.md"} {
		if !origin.MatchString(normalized(readRepoText(t, doc))) {
			t.Errorf("%s does not say that CXC v0.2.40 is the origin of the port (NOTICE, provenance) and CRW runs without it", doc)
		}
	}
}

// markdownTables are the tables of a document, each as its rows of trimmed cells (the separator row left out).
func markdownTables(text string) [][][]string {
	var tables [][][]string
	var current [][]string
	flush := func() {
		if len(current) > 0 {
			tables = append(tables, current)
			current = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			flush()
			continue
		}
		var cells []string
		for _, cell := range strings.Split(strings.Trim(line, "|"), "|") {
			cells = append(cells, strings.TrimSpace(cell))
		}
		if regexp.MustCompile(`^[\s|:-]+$`).MatchString(line) {
			continue
		}
		current = append(current, cells)
	}
	flush()
	return tables
}

// installationStates are the six states CRW-1228 keeps apart in one table of runtime-install.md.
var installationStates = []string{"source", "runtime install", "plugin cache", "trust", "activation", "firing"}

// stateTables counts the tables whose first column names every one of the six states.
func stateTables(text string) int {
	count := 0
	for _, table := range markdownTables(text) {
		first := map[string]bool{}
		for _, row := range table {
			first[strings.ToLower(strings.Trim(row[0], "`* "))] = true
		}
		all := true
		for _, state := range installationStates {
			all = all && first[state]
		}
		if all {
			count++
		}
	}
	return count
}

func TestRuntimeInstallHasOneStateTable(t *testing.T) {
	if n := stateTables(readRepoText(t, "docs/runtime-install.md")); n != 1 {
		t.Errorf("docs/runtime-install.md has %d tables whose first column names %v, want one", n, installationStates)
	}
	t.Run("fixture: six rows make the table, five do not", func(t *testing.T) {
		six := "| State | Means |\n| --- | --- |\n| Source | a |\n| Runtime install | b |\n| Plugin cache | c |\n| Trust | d |\n| Activation | e |\n| Firing | f |\n"
		if n := stateTables(six); n != 1 {
			t.Errorf("six = %d", n)
		}
		if n := stateTables(strings.Replace(six, "| Firing | f |\n", "", 1)); n != 0 {
			t.Errorf("five = %d", n)
		}
	})
}

// TestSwitchCXCIsTheHooksOff: the switch's `cxc` value is the ported hooks turned off and nothing else,
// in both documents, and the packaging page names the writer that exists.
func TestSwitchCXCIsTheHooksOff(t *testing.T) {
	install := readRepoText(t, "docs/runtime-install.md")
	if !strings.Contains(install, "\n## The hook switch\n") {
		t.Error("docs/runtime-install.md has no section \"The hook switch\"")
	}
	section := normalized(headingSection(install, "## The hook switch"))
	for _, want := range []string{"the only workflow plugin", "emergency stop", "`cmd/crw/hook_switch.go`", "`internal/runtime/install/switch_cmd.go`", "The completion Stop is not behind the switch"} {
		if !strings.Contains(section, want) {
			t.Errorf("the hook switch section lacks %q", want)
		}
	}
	packaging := readRepoText(t, "docs/plugin-packaging.md")
	if !strings.Contains(normalized(packaging), "The writer is `crw install switch`") {
		t.Error("docs/plugin-packaging.md does not name `crw install switch` as the writer")
	}
	if strings.Contains(packaging, "written by hand") {
		t.Error("docs/plugin-packaging.md still has the switch file written by hand")
	}
	for _, table := range markdownTables(packaging) {
		for _, row := range table {
			if row[0] == "`cxc`" && (len(row) < 2 || !strings.Contains(row[1], "emergency stop") || strings.Contains(row[1], "CXC")) {
				t.Errorf("docs/plugin-packaging.md: the `cxc` row is not the ported hooks off: %v", row)
			}
		}
	}
}
