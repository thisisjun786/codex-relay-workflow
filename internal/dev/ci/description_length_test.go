//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// descriptionSkillsRoot is the skills directory whose SKILL.md descriptions are measured.
const descriptionSkillsRoot = "plugins/crw/skills/"

// maxDescriptionRunes is the longest frontmatter description a finished skill has.
const maxDescriptionRunes = 300

// descriptionLengthErrors checks the descriptions table of m against root's skills: a done entry and
// a skill with no entry (one created after the table was frozen) have a description of at most 300
// runes, an entry names an existing directory, triggerAmbiguity[13] done means the entries of its
// paths are done, and sizeHotspots[4] done means every entry is.
func descriptionLengthErrors(root string, m inventoryMap) []string {
	descriptions, err := skillDescriptions(root)
	if err != nil {
		return []string{err.Error()}
	}
	var errs []string
	for _, name := range sortedKeys(descriptions) {
		entry, listed := m.Descriptions[name]
		if length := utf8.RuneCountInString(descriptions[name]); (!listed || entry.Status == "done") && length > maxDescriptionRunes {
			which := "has no entry in the descriptions table"
			if listed {
				which = "is done in the descriptions table"
			}
			errs = append(errs, name+": the description has "+strconv.Itoa(length)+" characters, over "+strconv.Itoa(maxDescriptionRunes)+", and "+which)
		}
	}
	for _, name := range sortedKeys(m.Descriptions) {
		if _, ok := descriptions[name]; !ok {
			errs = append(errs, "descriptions "+name+": names a directory with no SKILL.md under "+descriptionSkillsRoot)
		}
	}
	if m.Rows["triggerAmbiguity[13]"].Status == "done" {
		for _, path := range m.Rows["triggerAmbiguity[13]"].Paths {
			name, ok := descriptionSkillOf(path)
			if !ok {
				errs = append(errs, "triggerAmbiguity[13] path "+path+": is not a path below "+descriptionSkillsRoot)
				continue
			}
			entry, listed := m.Descriptions[name]
			if _, exists := descriptions[name]; !listed && !exists {
				errs = append(errs, "triggerAmbiguity[13] path "+path+": names no skill, neither a SKILL.md under "+descriptionSkillsRoot+" nor a descriptions entry")
			} else if listed && entry.Status != "done" {
				errs = append(errs, "triggerAmbiguity[13] is done but the descriptions entry "+name+" is open")
			}
		}
	}
	if m.Rows["sizeHotspots[4]"].Status == "done" {
		for _, name := range sortedKeys(m.Descriptions) {
			if m.Descriptions[name].Status != "done" {
				errs = append(errs, "sizeHotspots[4] is done but the descriptions entry "+name+" is open")
			}
		}
	}
	return errs
}

// descriptionSkillOf is the skill a path below the skills directory belongs to, whether the path
// names the skill's directory, a file or a directory in it.
func descriptionSkillOf(path string) (string, bool) {
	rest, ok := strings.CutPrefix(filepath.ToSlash(filepath.Clean(path)), descriptionSkillsRoot)
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, "/")
	return name, name != "" && name != ".."
}

func TestSkillDescriptionLength(t *testing.T) {
	t.Run("the checked-in table", func(t *testing.T) {
		root := repoRoot()
		data, err := os.ReadFile(filepath.Join(root, inventoryMapPath))
		if err != nil {
			t.Fatal(err)
		}
		m, err := loadInventoryMap(data)
		if err != nil {
			t.Fatal(err)
		}
		if errs := descriptionLengthErrors(root, m); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
		// The table is frozen for the 34 skills of the base, less the two that CRW-1218 merged into crw-status and crw-check and the Luna search skill that CRW-1220 merged into crw-search: seven are already short enough and done.
		done := 0
		for _, entry := range m.Descriptions {
			if entry.Status == "done" {
				done++
			}
		}
		if len(m.Descriptions) != 31 || done < 7 {
			t.Errorf("%d entries, %d done; want 31 entries and at least 7 done", len(m.Descriptions), done)
		}
	})
	long := strings.Repeat("x", 301)
	short := strings.Repeat("y", 300)
	wide := strings.Repeat("한", 300) // 900 bytes, 300 runes
	root := skillsFixture(t, map[string]string{"s-long": long, "s-short": short, "s-wide": wide, "s-new-long": long, "s-new-short": "short", "s-open-long": long, "s-done-long": long})
	table := func() inventoryMap {
		m := inventoryMap{Rows: map[string]inventoryRow{}, Descriptions: map[string]inventoryDescription{
			"s-long": {Owner: "S02", Status: "open"}, "s-short": {Owner: "S01", Status: "done"}, "s-wide": {Owner: "S01", Status: "done"},
			"s-open-long": {Owner: "S02", Status: "open"}, "s-done-long": {Owner: "S02", Status: "open"}}}
		return m
	}
	t.Run("fixture: open entries are not read, done entries and unlisted skills are", func(t *testing.T) {
		m := table()
		expectEqual(t, "errors", descriptionLengthErrors(root, m), []string{"s-new-long: the description has 301 characters, over 300, and has no entry in the descriptions table"})
		m.Descriptions["s-done-long"] = inventoryDescription{Owner: "S02", Status: "done"}
		expectEqual(t, "done long", descriptionLengthErrors(root, m), []string{
			"s-done-long: the description has 301 characters, over 300, and is done in the descriptions table",
			"s-new-long: the description has 301 characters, over 300, and has no entry in the descriptions table"})
	})
	t.Run("fixture: an entry that names a missing directory fails", func(t *testing.T) {
		m := table()
		m.Descriptions["s-gone"] = inventoryDescription{Owner: "S03", Status: "open"}
		errs := descriptionLengthErrors(root, m)
		if len(errs) != 2 || !strings.Contains(errs[1], "descriptions s-gone: names a directory with no SKILL.md") {
			t.Errorf("errors: %q", errs)
		}
	})
	t.Run("fixture: triggerAmbiguity[13] done needs the entries of its paths done", func(t *testing.T) {
		m := table()
		m.Rows["triggerAmbiguity[13]"] = inventoryRow{Status: "done", Paths: []string{"plugins/crw/skills/s-long/", "plugins/crw/skills/s-short/", "plugins/crw/skills/s-no-entry/"}}
		errs := descriptionLengthErrors(root, m)
		expectEqual(t, "errors", errs[len(errs)-2:], []string{
			"triggerAmbiguity[13] is done but the descriptions entry s-long is open",
			"triggerAmbiguity[13] path plugins/crw/skills/s-no-entry/: names no skill, neither a SKILL.md under plugins/crw/skills/ nor a descriptions entry"})
		// A path to a file or a deeper directory belongs to its skill; a path outside the skills fails.
		m.Rows["triggerAmbiguity[13]"] = inventoryRow{Status: "done", Paths: []string{"plugins/crw/skills/s-long/SKILL.md"}}
		expectEqual(t, "file path", descriptionLengthErrors(root, m)[1:], []string{"triggerAmbiguity[13] is done but the descriptions entry s-long is open"})
		m.Rows["triggerAmbiguity[13]"] = inventoryRow{Status: "done", Paths: []string{"plugins/crw/skills/s-long/references/x.md"}}
		expectEqual(t, "deeper path", descriptionLengthErrors(root, m)[1:], []string{"triggerAmbiguity[13] is done but the descriptions entry s-long is open"})
		m.Rows["triggerAmbiguity[13]"] = inventoryRow{Status: "done", Paths: []string{"docs/skills/", "plugins/crw/skills/", "plugins/crw/skills/s-short/"}}
		expectEqual(t, "outside", descriptionLengthErrors(root, m)[1:], []string{
			"triggerAmbiguity[13] path docs/skills/: is not a path below plugins/crw/skills/",
			"triggerAmbiguity[13] path plugins/crw/skills/: is not a path below plugins/crw/skills/"})
		m.Rows["triggerAmbiguity[13]"] = inventoryRow{Status: "open", Paths: []string{"plugins/crw/skills/s-long/"}}
		if got := descriptionLengthErrors(root, m); len(got) != 1 {
			t.Errorf("an open row reads nothing: %q", got)
		}
	})
	t.Run("fixture: sizeHotspots[4] done needs every entry done", func(t *testing.T) {
		m := table()
		m.Rows["sizeHotspots[4]"] = inventoryRow{Status: "done"}
		errs := descriptionLengthErrors(root, m)
		expectEqual(t, "errors", errs[1:], []string{
			"sizeHotspots[4] is done but the descriptions entry s-done-long is open",
			"sizeHotspots[4] is done but the descriptions entry s-long is open",
			"sizeHotspots[4] is done but the descriptions entry s-open-long is open"})
	})
}
