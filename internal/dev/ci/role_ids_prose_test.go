//go:build dev

package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// CRW-1428 (bundle CRW-1419, N3-2): the relay role ids `supervisor`, `parent` and `child` are names the code keeps
// (a stored row, a host's role policy, a flag, a CLI word). Text a person reads names the layers 부모 and 자식 and
// their English nouns, not the role ids, so the phrase "relay role" (the way the old prose introduced an id) stays
// out of the prose. hierarchy.md keeps the ids in one place, its level-2 section "Names used only in code".
//
// CRW-1431 (bundle CRW-1419, N3-3) widens the check to every .md under plugins/crw/skills, one subtest per skill
// directory. A directory that still holds the phrase is listed in a pending marker, one file per unit that rewrites
// it (testdata/role_ids_pending/<unit>.txt, one directory name per line, `#` starts a comment). The test is shrink
// only: a listed directory must still hold the phrase (a stale marker fails, so a marker cannot hide a clean
// directory) and an unlisted one must hold none. The unit that rewrites a directory first deletes its marker (red:
// the test names the file and the line), then edits the text (green). The directory is a name, so the subtest name
// is the same whether the directory is pending or enforced.

// relayRolePhrase is the phrase arm: "relay role" or "relay roles", with any whitespace (a line wrap) between the
// two words. It is matched on the text as written, so a wrapped "relay\nrole" is caught and the line is exact.
var relayRolePhrase = regexp.MustCompile(`(?i)relay\s+roles?\b`)

// codeOnlySection is the heading of the section of hierarchy.md that may hold the phrase: the code-name table.
// headingSection compares its prefix with the whole level-2 heading line, so the "## " belongs to the argument.
const codeOnlySection = "## Names used only in code"

// roleIdProseProblems reports each "relay role" phrase in text, as "rel:line: phrase", outside the sections that
// allow names (each a level-2 heading line as headingSection takes it). An allowed section that is missing or empty
// is a problem too: a section that cannot be found would leave its text in scope or hide a deleted section. The cut
// keeps the line count, so a line number reported is a line of the file.
func roleIdProseProblems(rel, text string, allow []string) []string {
	var problems []string
	for _, heading := range allow {
		section := headingSection(text, heading)
		if strings.TrimSpace(section) == "" {
			problems = append(problems, fmt.Sprintf("%s: section %q is missing or empty", rel, heading))
			continue
		}
		text = strings.Replace(text, section, strings.Repeat("\n", strings.Count(section, "\n")), 1)
	}
	for _, loc := range relayRolePhrase.FindAllStringIndex(text, -1) {
		line := 1 + strings.Count(text[:loc[0]], "\n")
		problems = append(problems, fmt.Sprintf("%s:%d: %q is prose, the role ids live in %q", rel, line, normalized(text[loc[0]:loc[1]]), codeOnlySection))
	}
	return problems
}

// roleIdAllowedFiles are the files under the skills root that may hold the phrase as a whole, with why. hierarchy.md
// is not here: it is allowed only inside its code-only section (codeOnlySection).
var roleIdAllowedFiles = map[string]string{
	"crw-canon/references/execution-roles.md":      "the host's role policy keys are named relay roles there; they are stored names, not prose about layers",
	"crw-run/references/initiative-supervision.md": "the old initiative supervisor: out of scope of CRW-1431 (bundle exclusion), the file is not rewritten",
}

const (
	roleIdSkillsRoot   = "plugins/crw/skills"
	roleIdPendingDir   = "testdata/role_ids_pending"
	roleIdHierarchyRel = "crw-canon/references/hierarchy.md"
)

// roleIdDirProblems is the verdict on one skill directory: hits are the phrase uses found in it (already outside the
// allowed sections and files) and pending says a marker lists it. An enforced directory fails on any hit; a pending
// one fails when it has none, since a marker for a clean directory hides nothing but is stale.
func roleIdDirProblems(dir string, hits []string, pending bool) []string {
	switch {
	case pending && len(hits) == 0:
		return []string{fmt.Sprintf("%s: listed in %s but holds no %q: delete the entry", dir, roleIdPendingDir, "relay role")}
	case !pending && len(hits) > 0:
		return append([]string{fmt.Sprintf("%s: %d use(s) of %q in prose and no pending marker lists the directory", dir, len(hits), "relay role")}, hits...)
	}
	return nil
}

// roleIdPendingEntries reads every marker file in dir: entry -> the marker file that lists it. A name listed twice,
// or a line that is not a directory name, is a problem.
func roleIdPendingEntries(dir string) (map[string]string, []string) {
	entries := map[string]string{}
	var problems []string
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return entries, nil
		}
		return entries, []string{err.Error()}
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".txt") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for number, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.ContainsAny(line, "/ \t") {
				problems = append(problems, fmt.Sprintf("%s:%d: %q is not a skill directory name", f.Name(), number+1, line))
				continue
			}
			if prior, dup := entries[line]; dup {
				problems = append(problems, fmt.Sprintf("%s:%d: %q is already listed in %s", f.Name(), number+1, line, prior))
				continue
			}
			entries[line] = f.Name()
		}
	}
	return entries, problems
}

// roleIdSkillDirProblems reads every .md under one skill directory and returns the phrase uses outside the allowed
// files and sections, as "path:line: ..." (path relative to the skills root).
func roleIdSkillDirProblems(t *testing.T, skillsRoot, dir string) []string {
	t.Helper()
	var hits []string
	walkRoot := filepath.Join(skillsRoot, dir)
	err := filepath.WalkDir(walkRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, err := filepath.Rel(skillsRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, allowed := roleIdAllowedFiles[rel]; allowed {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var allow []string
		if rel == roleIdHierarchyRel {
			allow = []string{codeOnlySection}
		}
		hits = append(hits, roleIdProseProblems(roleIdSkillsRoot+"/"+rel, string(data), allow)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestRoleIdsStayOutOfProse(t *testing.T) {
	t.Run("hierarchy", func(t *testing.T) {
		text := readRepoText(t, hierarchyFile)
		if problems := roleIdProseProblems(hierarchyFile, text, []string{codeOnlySection}); len(problems) > 0 {
			t.Errorf("hierarchy.md names relay roles outside its code-only section:\n%s", strings.Join(problems, "\n"))
		}
	})

	t.Run("execution-roles", func(t *testing.T) {
		for rel, why := range roleIdAllowedFiles {
			if _, err := os.Stat(filepath.Join(repoRoot(), roleIdSkillsRoot, rel)); err != nil {
				t.Errorf("%s is allowed (%s) but cannot be read: %v", rel, why, err)
			}
		}
	})

	skillsRoot := filepath.Join(repoRoot(), roleIdSkillsRoot)
	pending, markerProblems := roleIdPendingEntries(filepath.Join(repoRoot(), "internal/dev/ci", roleIdPendingDir))
	for _, p := range markerProblems {
		t.Error(p)
	}
	dirs, err := os.ReadDir(skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dirs {
		if d.IsDir() {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	known := map[string]bool{}
	for _, name := range names {
		known[name] = true
		t.Run(name, func(t *testing.T) {
			_, isPending := pending[name]
			hits := roleIdSkillDirProblems(t, skillsRoot, name)
			if problems := roleIdDirProblems(name, hits, isPending); len(problems) > 0 {
				t.Errorf("%s", strings.Join(problems, "\n"))
			}
		})
	}
	t.Run("markers", func(t *testing.T) {
		for entry, file := range pending {
			if !known[entry] {
				t.Errorf("%s lists %q, which is no directory under %s", file, entry, roleIdSkillsRoot)
			}
		}
	})

	t.Run("fixtures", func(t *testing.T) {
		allow := []string{codeOnlySection}
		doc := func(prose, code string) string {
			return "# T\n\n## The levels\n\n" + prose + "\n\n" + codeOnlySection + "\n\n" + code + "\n\n## Tail\n\nend\n"
		}
		const phrase = "The relay role id `parent` names the child."
		for name, c := range map[string]struct {
			text string
			want []string // substrings of the problems; none means the text passes
		}{
			"phrase inside the section passes":  {doc("No ids here.", phrase), nil},
			"phrase outside the section fails":  {doc(phrase, "A table."), []string{"x.md:5:"}},
			"wrapped phrase is caught":          {doc("the relay\nrole id `child`", "A table."), []string{"x.md:5:"}},
			"plural and case are caught":        {doc("Relay Roles are listed.", "A table."), []string{"x.md:5:"}},
			"a phrase after the section fails":  {doc("No ids here.", "A table.") + "\nAnother relay role.\n", []string{"x.md:15:"}},
			"no section fails as missing":       {"# T\n\n## The levels\n\nNo ids here.\n", []string{`section "## Names used only in code" is missing or empty`}},
			"empty section fails as empty":      {doc("No ids here.", ""), []string{"is missing or empty"}},
			"an h3 inside the section is in it": {doc("No ids here.", "### Table\n\n"+phrase), nil},
		} {
			t.Run(name, func(t *testing.T) {
				problems := roleIdProseProblems("x.md", c.text, allow)
				if len(c.want) == 0 {
					if len(problems) > 0 {
						t.Fatalf("want a pass, got %v", problems)
					}
					return
				}
				joined := strings.Join(problems, "\n")
				for _, want := range c.want {
					if !strings.Contains(joined, want) {
						t.Errorf("no problem contains %q: %v", want, problems)
					}
				}
			})
		}

		t.Run("the heading without its marks finds no section", func(t *testing.T) {
			// headingSection takes the whole heading line: "Names used only in code" alone is the bug the check
			// above exists to catch, and it must read as a missing section rather than leave the text in scope.
			problems := roleIdProseProblems("x.md", doc("No ids here.", phrase), []string{"Names used only in code"})
			if len(problems) == 0 || !strings.Contains(problems[0], "is missing or empty") {
				t.Fatalf("want the missing-section problem first, got %v", problems)
			}
		})

		t.Run("a marker for a clean directory fails", func(t *testing.T) {
			if p := roleIdDirProblems("crw-x", nil, true); len(p) != 1 || !strings.Contains(p[0], "holds no") {
				t.Errorf("a stale marker: %v", p)
			}
		})
		t.Run("a directory with hits and no marker fails", func(t *testing.T) {
			if p := roleIdDirProblems("crw-x", []string{"a.md:3: hit"}, false); len(p) != 2 || !strings.Contains(p[0], "no pending marker") {
				t.Errorf("an unlisted directory: %v", p)
			}
		})
		t.Run("a pending directory with hits and a clean enforced one pass", func(t *testing.T) {
			if p := roleIdDirProblems("crw-x", []string{"a.md:3: hit"}, true); len(p) != 0 {
				t.Errorf("a listed directory with hits: %v", p)
			}
			if p := roleIdDirProblems("crw-x", nil, false); len(p) != 0 {
				t.Errorf("a clean directory: %v", p)
			}
		})
		t.Run("marker files: comments, blanks, duplicates and paths", func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("a.txt", "# why\ncrw-x\n\ncrw-y\n")
			write("b.txt", "crw-y\ncrw-z/refs\n")
			write("c.md", "ignored\n")
			entries, problems := roleIdPendingEntries(dir)
			if len(entries) != 2 || entries["crw-x"] != "a.txt" || entries["crw-y"] != "a.txt" {
				t.Errorf("entries: %v", entries)
			}
			joined := strings.Join(problems, "\n")
			if len(problems) != 2 || !strings.Contains(joined, "b.txt:1:") || !strings.Contains(joined, "b.txt:2:") {
				t.Errorf("problems: %v", problems)
			}
			if entries, problems := roleIdPendingEntries(filepath.Join(dir, "absent")); len(entries) != 0 || len(problems) != 0 {
				t.Errorf("an absent directory is no marker: %v %v", entries, problems)
			}
		})
	})
}
