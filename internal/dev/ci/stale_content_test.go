//go:build dev

package ci

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// staleSkillsRoot is the tree the scans of the done rows read.
const staleSkillsRoot = "plugins/crw/skills/"

// scanPattern is one thing a content scan refuses.
type scanPattern struct {
	re   *regexp.Regexp
	what string
}

// staleCxcPatterns are the CodexClaw ("CXC") leftovers a CRW skill does not carry: its name in any
// form. A citation of the port oracle that must stay is listed in staleCxcAllowed.
var staleCxcPatterns = []scanPattern{
	{regexp.MustCompile(`(?i)codexclaw|\bcxc\b`), "a CodexClaw reference"},
}

// staleAllowance exempts the lines of the file under Path (a file or a directory) that contain Text
// (an entry without Text exempts nothing): a port-oracle citation the skill keeps on purpose. The list is
// named and short; an entry says why in Why.
type staleAllowance struct {
	Path, Text, Why string
}

// staleCxcAllowed is the named list of port-oracle citations the stale-reference scan lets through:
// the header of a ported test, which says which original test it replays and under which name
// table. A skill's instructions never cite the original.
var staleCxcAllowed = []staleAllowance{
	{Path: staleSkillsRoot + "crw-qa/tests/qa-validate-evidence.test.mjs", Text: "The oracle is CXC v0.2.40 plugins/codexclaw/test/qa-validate-evidence.test.mjs", Why: "the header names the original test this one is ported from"},
	{Path: staleSkillsRoot + "crw-qa/tests/qa-validate-evidence.test.mjs", Text: "(.codexclaw -> .crw)", Why: "the header names the name table the port applied"},
	{Path: staleSkillsRoot + "crw-repo-map/tests/repo-map-packaging.test.mjs", Text: "Ported by CRW-939 from CXC v0.2.40 plugins/codexclaw/test/repo-map-packaging.test.mjs", Why: "the header names the original test this one is ported from"},
	{Path: staleSkillsRoot + "crw-repo-map/tests/repo-map-packaging.test.mjs", Text: "name substitution of contract/schema/cxc/name-substitution.json: the skill is crw-repo-map", Why: "the header names the name table the port applied"},
	{Path: staleSkillsRoot + "crw-repo-map/tests/repo-map-packaging.test.mjs", Text: "(cxc map --help exits 0 without python deps", Why: "the header names the original dispatcher cases it does not replay"},
	{Path: staleSkillsRoot + "crw-repo-map/tests/repo-map-packaging.test.mjs", Text: "bin/codexclaw.mjs, the CXC Node dispatcher", Why: "the header names the original dispatcher this port replaced"},
	{Path: staleSkillsRoot + "crw-repo-map/tests/repo-map-packaging.test.mjs", Text: "The oracle spawned `cxc map --help`", Why: "the comment names the original spawn it does not replay"},
	{Path: staleSkillsRoot + "crw-repo-map/tests/repo-map-smoke.test.mjs", Text: "Ported by CRW-939 from CXC v0.2.40 plugins/codexclaw/test/repo-map-smoke.test.mjs", Why: "the header names the original test this one is ported from"},
	{Path: staleSkillsRoot + "crw-repo-map/tests/repo-map-smoke.test.mjs", Text: "name substitution of contract/schema/cxc/name-substitution.json. Three adaptations:", Why: "the header names the name table the port applied"},
	{Path: staleSkillsRoot + "crw-repo-map/tests/repo-map-smoke.test.mjs", Text: "plugins/codexclaw/test/fixtures/repo-map/", Why: "the header names the original fixture directory"},
	{Path: staleSkillsRoot + "crw-repo-map/tests/repo-map-smoke.test.mjs", Text: "CODEXCLAW_SKIP_REPOMAP_SMOKE=1", Why: "the header names the original escape variable"},
}

// changeHistoryPatterns are notes about how a skill changed rather than what it says now: dropped
// products, candidate lists, dated verification stamps, renames and redirects.
var changeHistoryPatterns = []scanPattern{
	{regexp.MustCompile(`(?i)\bv2 candidates?\b`), "a v2-candidates note"},
	{regexp.MustCompile(`(?i)^#+\s*Runtime Status\b`), "a Runtime Status section"},
	{regexp.MustCompile(`(?i)\bskill-hub\b|\b(?:removed|dropped|retired)\b[^\n]{0,60}\bjaw\b|\bjaw\b[^\n]{0,60}\b(?:removed|dropped|retired)\b`), "a dropped skill-hub or jaw note"},
	{regexp.MustCompile(`(?i)\bverified 20[0-9]{2}-[0-9]{2}-[0-9]{2}\b|\blast-verified\b`), "a dated verification stamp"},
	{regexp.MustCompile(`(?i)\bformerly\b|\bredirects here\b|\brenamed from\b|\bwas renamed\b|\bused to (?:be|live|run)\b`), "a rename or history note"},
}

// scanFiles is the text files below root/name (the file itself when it is one).
func scanFiles(root, name string) ([]string, error) {
	files, err := dirTree{root}.list(name)
	if err != nil {
		return nil, err
	}
	var text []string
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(data[:min(len(data), 8192)], []byte{0}) {
			text = append(text, file)
		}
	}
	return text, nil
}

// rowScanErrors reads only the paths of the done rows whose check names test, and reports each line
// of those files that matches a pattern and is not allowed. A done row that names the test without
// paths, or whose paths hold no text file, is an error: a done row cannot pass by reading nothing.
func rowScanErrors(root string, m inventoryMap, test string, patterns []scanPattern, allowed []staleAllowance) []string {
	var errs []string
	for _, key := range sortedKeys(m.Rows) {
		row := m.Rows[key]
		named := false
		for _, name := range row.Check {
			named = named || name == test
		}
		if row.Status != "done" || !named {
			continue
		}
		if len(row.Paths) == 0 {
			errs = append(errs, key+": a done row that names "+test+" has no paths to scan")
		}
		for _, path := range row.Paths {
			files, err := scanFiles(root, path)
			if err != nil {
				errs = append(errs, key+": "+err.Error())
				continue
			}
			if len(files) == 0 {
				errs = append(errs, key+": "+path+" holds no file to scan")
			}
			for _, file := range files {
				data, err := os.ReadFile(filepath.Join(root, file))
				if err != nil {
					errs = append(errs, key+": "+err.Error())
					continue
				}
			nextLine:
				for number, line := range lines(string(data)) {
					// CRW-1526 permits this single old-name explanation in the
					// renamed skill's title. Extra prose and other files still fail.
					if test == "TestNoChangeHistoryNotes" && file == staleSkillsRoot+"crw-work/SKILL.md" && line == "# Task work (formerly PABCD)" {
						continue
					}
					for _, pattern := range patterns {
						if !pattern.re.MatchString(line) {
							continue
						}
						for _, allowance := range allowed {
							if covers(allowance.Path, file) && allowance.Text != "" && strings.Contains(line, allowance.Text) {
								continue nextLine
							}
						}
						errs = append(errs, key+": "+file+":"+strconv.Itoa(number+1)+": "+pattern.what+": "+strings.TrimSpace(line))
						continue nextLine
					}
				}
			}
		}
	}
	return errs
}

// scanRealTree runs a scan on the checked-in map.
func scanRealTree(t *testing.T, test string, patterns []scanPattern, allowed []staleAllowance) []string {
	t.Helper()
	root := repoRoot()
	data, err := os.ReadFile(filepath.Join(root, inventoryMapPath))
	if err != nil {
		t.Fatal(err)
	}
	m, err := loadInventoryMap(data)
	if err != nil {
		t.Fatal(err)
	}
	return rowScanErrors(root, m, test, patterns, allowed)
}

// scanFixture is a skills tree with the text of two skills, and a map whose rows are given.
func scanFixture(t *testing.T, test string, status string, paths ...string) (string, inventoryMap) {
	t.Helper()
	root := t.TempDir()
	for name, text := range map[string]string{
		"crw-a/SKILL.md":            "# A\n\nPorted from CodexClaw.\nThe cxc oracle says so.\nVerified 2026-09-09.\n",
		"crw-a/references/notes.md": "## Runtime Status\n\nA skill-hub note, formerly a v2 candidate.\n",
		"crw-b/SKILL.md":            "# B\n\nclean text\n",
		"crw-c/SKILL.md":            "# C\n\nUses the cxc name and the jaw note.\n",
		"crw-a/assets/pic.bin":      "\x00codexclaw\x00",
	} {
		path := filepath.Join(root, staleSkillsRoot, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	row := inventoryRow{Disposition: "fix", Owner: "S06", Check: []string{test}, Status: status, Paths: paths}
	return root, inventoryMap{Rows: map[string]inventoryRow{"deadOrStale[11]": row}}
}

func TestNoStaleCxcReferences(t *testing.T) {
	t.Run("the checked-in table", func(t *testing.T) {
		if errs := scanRealTree(t, "TestNoStaleCxcReferences", staleCxcPatterns, staleCxcAllowed); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	t.Run("fixture: only the paths of done rows that name the test are read", func(t *testing.T) {
		root, m := scanFixture(t, "TestNoStaleCxcReferences", "done", staleSkillsRoot+"crw-a/")
		expectEqual(t, "errors", rowScanErrors(root, m, "TestNoStaleCxcReferences", staleCxcPatterns, nil), []string{
			"deadOrStale[11]: plugins/crw/skills/crw-a/SKILL.md:3: a CodexClaw reference: Ported from CodexClaw.",
			"deadOrStale[11]: plugins/crw/skills/crw-a/SKILL.md:4: a CodexClaw reference: The cxc oracle says so.",
		})
		expectEqual(t, "another test", rowScanErrors(root, m, "TestNoChangeHistoryNotes", changeHistoryPatterns, nil), []string(nil))
		row := m.Rows["deadOrStale[11]"]
		row.Status = "open"
		expectEqual(t, "open row", rowScanErrors(root, inventoryMap{Rows: map[string]inventoryRow{"deadOrStale[11]": row}}, "TestNoStaleCxcReferences", staleCxcPatterns, nil), []string(nil))
		clean, m2 := scanFixture(t, "TestNoStaleCxcReferences", "done", staleSkillsRoot+"crw-b/SKILL.md")
		expectEqual(t, "clean path", rowScanErrors(clean, m2, "TestNoStaleCxcReferences", staleCxcPatterns, nil), []string(nil))
	})
	t.Run("fixture: a done row cannot pass by reading nothing", func(t *testing.T) {
		root, m := scanFixture(t, "TestNoStaleCxcReferences", "done")
		expectEqual(t, "no paths", rowScanErrors(root, m, "TestNoStaleCxcReferences", staleCxcPatterns, nil),
			[]string{"deadOrStale[11]: a done row that names TestNoStaleCxcReferences has no paths to scan"})
		root, m = scanFixture(t, "TestNoStaleCxcReferences", "done", staleSkillsRoot+"crw-none/")
		expectEqual(t, "empty path", rowScanErrors(root, m, "TestNoStaleCxcReferences", staleCxcPatterns, nil),
			[]string{"deadOrStale[11]: plugins/crw/skills/crw-none/ holds no file to scan"})
	})
	t.Run("fixture: an entry without text lets nothing through", func(t *testing.T) {
		root, m := scanFixture(t, "TestNoStaleCxcReferences", "done", staleSkillsRoot)
		allowed := []staleAllowance{{Path: staleSkillsRoot + "crw-a/SKILL.md", Why: "a ported test header"}, {Path: staleSkillsRoot + "crw-c/", Why: "x"}}
		if got := rowScanErrors(root, m, "TestNoStaleCxcReferences", staleCxcPatterns, allowed); len(got) == 0 {
			t.Errorf("entries without Text let the scan skip lines: %v", got)
		}
	})
	t.Run("the named list is short, says why and names ported test files", func(t *testing.T) {
		for _, allowance := range staleCxcAllowed {
			if allowance.Text == "" || allowance.Why == "" || !strings.HasPrefix(allowance.Path, staleSkillsRoot) || !strings.HasSuffix(allowance.Path, ".test.mjs") {
				t.Errorf("allowance %+v: needs the citation text, a reason and a ported test file below %s", allowance, staleSkillsRoot)
			}
		}
		if len(staleCxcAllowed) > 12 {
			t.Errorf("%d allowances; the list stays short", len(staleCxcAllowed))
		}
	})
	t.Run("fixture: the named list lets a kept citation through", func(t *testing.T) {
		root, m := scanFixture(t, "TestNoStaleCxcReferences", "done", staleSkillsRoot)
		allowed := []staleAllowance{{Path: staleSkillsRoot + "crw-a/", Text: "cxc oracle", Why: "port-oracle citation"}, {Path: staleSkillsRoot + "crw-c/SKILL.md", Text: "cxc name", Why: "x"}}
		expectEqual(t, "errors", rowScanErrors(root, m, "TestNoStaleCxcReferences", staleCxcPatterns, allowed), []string{
			"deadOrStale[11]: plugins/crw/skills/crw-a/SKILL.md:3: a CodexClaw reference: Ported from CodexClaw."})
	})
}

func TestNoChangeHistoryNotes(t *testing.T) {
	t.Run("only the approved task work rename heading is allowed", func(t *testing.T) {
		for _, row := range []struct {
			path, line string
			wantOK     bool
		}{
			{"crw-work/SKILL.md", "# Task work (formerly PABCD)", true},
			{"crw-work/SKILL.md", "# Task work (formerly PABCD), renamed from another skill", false},
			{"crw-work/SKILL.md", "Task work (formerly PABCD)", false},
			{"crw-work/references/notes.md", "# Task work (formerly PABCD)", false},
			{"crw-continue/SKILL.md", "# Task work (formerly PABCD)", false},
		} {
			root, m := scanFixture(t, "TestNoChangeHistoryNotes", "done", staleSkillsRoot+row.path)
			p := filepath.Join(root, staleSkillsRoot+row.path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(row.line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			errs := rowScanErrors(root, m, "TestNoChangeHistoryNotes", changeHistoryPatterns, nil)
			if (len(errs) == 0) != row.wantOK {
				t.Errorf("%s: %q: errors %v, want clean %v", row.path, row.line, errs, row.wantOK)
			}
		}
	})
	t.Run("a removal note for the jaw registry fails, its name as a live source does not", func(t *testing.T) {
		re := changeHistoryPatterns[2].re
		for line, want := range map[string]bool{
			"### Removed cli-jaw tiers (non-goals)":         true,
			"The jaw registry was dropped.":                 true,
			"skill-hub is gone":                             true,
			"| 1st | **jaw** (registry) | `--source jaw` |": false,
			"Always try jaw first, then clawhub.":           false,
			"crw skill search <query> (jaw first)":          false,
		} {
			if re.MatchString(line) != want {
				t.Errorf("%q: match = %v, want %v", line, !want, want)
			}
		}
	})
	t.Run("the checked-in table", func(t *testing.T) {
		if errs := scanRealTree(t, "TestNoChangeHistoryNotes", changeHistoryPatterns, nil); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	t.Run("fixture: only the paths of done rows that name the test are read", func(t *testing.T) {
		root, m := scanFixture(t, "TestNoChangeHistoryNotes", "done", staleSkillsRoot+"crw-a/")
		expectEqual(t, "errors", rowScanErrors(root, m, "TestNoChangeHistoryNotes", changeHistoryPatterns, nil), []string{
			"deadOrStale[11]: plugins/crw/skills/crw-a/SKILL.md:5: a dated verification stamp: Verified 2026-09-09.",
			"deadOrStale[11]: plugins/crw/skills/crw-a/references/notes.md:1: a Runtime Status section: ## Runtime Status",
			"deadOrStale[11]: plugins/crw/skills/crw-a/references/notes.md:3: a v2-candidates note: A skill-hub note, formerly a v2 candidate.",
		})
		expectEqual(t, "another test", rowScanErrors(root, m, "TestNoStaleCxcReferences", staleCxcPatterns, nil), []string(nil))
		root, m = scanFixture(t, "TestNoChangeHistoryNotes", "done", staleSkillsRoot+"crw-b/")
		expectEqual(t, "clean skill", rowScanErrors(root, m, "TestNoChangeHistoryNotes", changeHistoryPatterns, nil), []string(nil))
	})
	t.Run("fixture: a done row cannot pass by reading nothing", func(t *testing.T) {
		root, m := scanFixture(t, "TestNoChangeHistoryNotes", "done")
		expectEqual(t, "no paths", rowScanErrors(root, m, "TestNoChangeHistoryNotes", changeHistoryPatterns, nil),
			[]string{"deadOrStale[11]: a done row that names TestNoChangeHistoryNotes has no paths to scan"})
	})
}
