//go:build dev

package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CRW-1344: the work is tracked in a GitHub repository, so the skills and their references name no other tracker. The
// one place a skill may name Linear is the line of crw-canon/references/tracking-model.md that says the tracking
// section's kind "linear" is a read-only archive setting for looking the old record up. The agents follow these
// documents, so a sentence that still named the old tracker would send them to a tool the product no longer uses.
//
// The word is also a CSS keyword (linear-gradient, the linear timing function), an adverb (maintenance burden scales
// linearly) and the name of a product whose design the design references describe (Linear's density); those files are
// not about tracking and are listed here with what they say, so a new file is held to the rule by default.

const (
	skillsDir        = "plugins/crw/skills"
	archiveLineFile  = "crw-canon/references/tracking-model.md"
	archiveLineWords = "read-only archive"
)

// notTracking are the skill directories and files whose "linear" is a CSS keyword, an adverb or a design product, not the tracker.
var notTracking = map[string]string{
	"crw-dev-frontend":    "CSS (linear-gradient, the linear timing function), the adverb linearly, and products named as design references",
	"crw-dev-uiux-design": "CSS (linear-gradient, the linear timing function) and products named as design references",
	"crw-dev-backend/references/core/api-lifecycle.md": "the adverb linearly in an API lifecycle table",
}

// pendingElsewhere are lines another running lane owns: the pull request and landing paragraph of delivery.md belongs to
// lane pr-landing-1355 (CRW-1355), which rewrites it; this lane leaves its one word and the integrator drops the entry
// when that lane lands.
var pendingElsewhere = map[string]string{
	"crw-canon/references/delivery.md": "The coordinator then checks the Linear criteria",
}

var linearWord = regexp.MustCompile(`(?i)linear`)

// skillTextExt are the files of a skill that agents read or that carry a prompt.
var skillTextExt = map[string]bool{".md": true, ".yaml": true, ".yml": true, ".json": true, ".txt": true}

func exemptFromTrackingWording(rel string) bool {
	for entry := range notTracking {
		if rel == entry || strings.HasPrefix(rel, entry+"/") {
			return true
		}
	}
	return false
}

// linearLineProblems are the lines of the skills under root that name Linear outside the archive line, and the problems
// with the archive line itself (exactly one, in tracking-model.md, saying it is a read-only archive setting).
func linearLineProblems(root string) []string {
	var problems []string
	archive := 0
	base := filepath.Join(root, filepath.FromSlash(skillsDir))
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return nil
		}
		if !skillTextExt[filepath.Ext(path)] || exemptFromTrackingWording(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !linearWord.MatchString(line) {
				continue
			}
			switch {
			case rel == archiveLineFile && strings.Contains(line, archiveLineWords):
				archive++
				if !strings.Contains(line, "`tracking.kind` `linear`") || !strings.Contains(line, "never written") {
					problems = append(problems, rel+":"+strconv.Itoa(i+1)+": the archive line does not say that tracking.kind linear is never written to: "+line)
				}
			case pendingElsewhere[rel] != "" && strings.Contains(line, pendingElsewhere[rel]):
			default:
				problems = append(problems, rel+":"+strconv.Itoa(i+1)+": names Linear: "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		return []string{err.Error()}
	}
	if archive != 1 {
		problems = append(problems, archiveLineFile+": "+strconv.Itoa(archive)+" archive lines, want exactly one")
	}
	return problems
}

func TestSkillsNameNoLinearTracker(t *testing.T) {
	for _, p := range linearLineProblems(repoRoot()) {
		t.Error(p)
	}
	for dir := range notTracking {
		if _, err := os.Stat(filepath.Join(repoRoot(), skillsDir, dir)); err != nil {
			t.Errorf("the exempt path %s does not exist: %v", dir, err)
		}
	}
	t.Run("fixture", func(t *testing.T) {
		archiveLine := "`tracking.kind` `linear` is a read-only archive setting for looking up the old record, never written to.\n"
		fixture := func(files map[string]string) string {
			root := t.TempDir()
			for rel, text := range files {
				path := filepath.Join(root, filepath.FromSlash(skillsDir), filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			return root
		}
		good := map[string]string{
			archiveLineFile:                "# T\n\n" + archiveLine,
			"crw-run/SKILL.md":             "Read the issue with gh.\n",
			"crw-dev-frontend/motion.md":   "animation: x linear both;\n",
			"crw-dev-uiux-design/trend.md": "background: linear-gradient(red, blue);\n",
		}
		if p := linearLineProblems(fixture(good)); len(p) != 0 {
			t.Errorf("good: %v", p)
		}
		for name, mutate := range map[string]func(map[string]string){
			"a skill names the tracker": func(f map[string]string) { f["crw-run/SKILL.md"] = "Use the Linear tools.\n" },
			"a prompt names the tracker": func(f map[string]string) {
				f["crw-plan/agents/openai.yaml"] = "default_prompt: \"write it in Linear\"\n"
			},
			"a reference link names it": func(f map[string]string) {
				f["crw-plan/references/a.md"] = "[x](../crw-canon/references/linear-model.md)\n"
			},
			"a second archive line": func(f map[string]string) {
				f["crw-check/SKILL.md"] = "`tracking.kind` `linear` is a read-only archive setting, never written to.\n"
			},
			"no archive line": func(f map[string]string) { f[archiveLineFile] = "# T\n" },
			"an archive line that writes": func(f map[string]string) {
				f[archiveLineFile] = "`tracking.kind` `linear` is a read-only archive setting that agents may update.\n"
			},
			"an unlisted design skill": func(f map[string]string) { f["crw-dev-testing/SKILL.md"] = "Compare with Linear.\n" },
		} {
			files := map[string]string{}
			for k, v := range good {
				files[k] = v
			}
			mutate(files)
			if p := linearLineProblems(fixture(files)); len(p) == 0 {
				t.Errorf("%s: no problem reported", name)
			}
		}
	})
}
