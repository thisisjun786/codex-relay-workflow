//go:build dev

package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CRW-1536: the pinned Paperthin tree and manifest at
// 7d5dc6235990230e45a24a25c527d81701da5458 contain 28 skills, not the brief's 29.
// This freezes that inventory and checks the map's CRW file destinations, not
// whether a future instruction change implements the mapped principle.
const paperthinMapPath = "docs/rebrand/paperthin-map.md"

var paperthinCatalog = strings.Fields(`
	depth/re0 depth/readchk depth/aim depth/modelchk depth/hate depth/macrothink
	depth/feynman depth/autobahn depth/reorder depth/detool depth/dedash depth/debloat
	depth/shower depth/factchk depth/mandela depth/sip depth/re0-git depth/re0-release
	depth/re0-merge breadth/ssotize breadth/re0-upgrade coil/re0-plan coil/re0-loop
	coil/re0-memo coil/re0-work coil/catchup coil/nba mesh/prism
`)

var paperthinCodeSpan = regexp.MustCompile("`([^`]+)`")

func paperthinMapProblems(root, text string) []string {
	rows := tableRows(text, "paperthin skill")
	var problems []string
	if len(rows) != len(paperthinCatalog) {
		problems = append(problems, fmt.Sprintf("map has %d rows, want the %d skills in the pinned source", len(rows), len(paperthinCatalog)))
	}
	expected := map[string]string{}
	for _, entry := range paperthinCatalog {
		expected[filepath.Base(entry)] = "skills/" + entry + "/SKILL.md"
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) != 7 {
			problems = append(problems, fmt.Sprintf("row %v has %d cells, want 7", row, len(row)))
			continue
		}
		name := strings.Trim(row[0], "`")
		source, known := expected[name]
		if !known || seen[name] {
			problems = append(problems, "unknown or repeated skill: "+name)
		}
		seen[name] = true
		if strings.Trim(row[1], "`") != source {
			problems = append(problems, name+": source path does not match the pinned catalog")
		}
		if row[2] == "" || row[5] == "" {
			problems = append(problems, name+": stage and adaptation or exclusion reason are required")
		}
		switch row[3] {
		case "adapt":
			for _, owner := range strings.Split(row[6], ",") {
				switch strings.TrimSpace(owner) {
				case "CRW-1537", "CRW-1538", "CRW-1539", "CRW-1540", "CRW-1541", "CRW-1542":
				default:
					problems = append(problems, name+": no valid follow-up owner: "+owner)
				}
			}
		case "not adopted":
			if row[6] != "none" {
				problems = append(problems, name+": an excluded skill must say none for its follow-up")
			}
		default:
			problems = append(problems, name+": disposition must be adapt or not adopted")
		}
		paths := paperthinCodeSpan.FindAllStringSubmatch(row[4], -1)
		if len(paths) == 0 {
			problems = append(problems, name+": no CRW file path")
		}
		for _, match := range paths {
			rel := match[1]
			if !filepath.IsLocal(rel) || strings.Contains(rel, "\\") {
				problems = append(problems, name+": CRW path is not repository-relative: "+rel)
				continue
			}
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil || !info.Mode().IsRegular() {
				problems = append(problems, name+": CRW path is not an existing file: "+rel)
			}
		}
	}
	for _, entry := range paperthinCatalog {
		if !seen[filepath.Base(entry)] {
			problems = append(problems, "missing skill: "+filepath.Base(entry))
		}
	}
	return problems
}

func TestPaperthinMapCoversPinnedSkillsWithExistingCRWPaths(t *testing.T) {
	if problems := paperthinMapProblems(repoRoot(), readRepoText(t, paperthinMapPath)); len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

func TestPaperthinMapRejectsMissingTargetsAndEmptyInventory(t *testing.T) {
	text := readRepoText(t, paperthinMapPath)
	rows := tableRows(text, "paperthin skill")
	if len(rows) == 0 || len(rows[0]) != 7 {
		t.Fatal("map fixture has no complete row")
	}
	paths := paperthinCodeSpan.FindAllStringSubmatch(rows[0][4], -1)
	if len(paths) == 0 {
		t.Fatal("map fixture has no destination")
	}
	const missing = "missing/paperthin-map-target.md"
	broken := strings.ReplaceAll(text, "`"+paths[0][1]+"`", "`"+missing+"`")
	problems := strings.Join(paperthinMapProblems(repoRoot(), broken), "\n")
	if !strings.Contains(problems, "CRW path is not an existing file: "+missing) {
		t.Fatalf("missing target was not caught: %s", problems)
	}
	if problems := paperthinMapProblems(repoRoot(), "# No map\n"); len(problems) == 0 {
		t.Fatal("an absent table was accepted")
	}
}
