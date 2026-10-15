//go:build dev

package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CRW-1340 (Jun 10-14, Sion relaying; 04:3x correction): the synchronous Interview that came over from CXC
// is gone from CRW. The crw-interview skill and every route to it are removed; crw-define stays and is the
// step that registers a milestone and its issues, used by the top session (today Sion) and the parent
// (repository session); ambiguity during a run goes up as a decision request; the uiux-design intent
// ladder stays an optional path of that skill which the default flow never calls.

// interviewRouteScanned is whether rel is a file whose text may not route to crw-interview: the plugin,
// the documents and the README. The known-defects records are history and stay as written.
func interviewRouteScanned(rel string) bool {
	switch {
	case strings.HasPrefix(rel, "docs/port-cxc/known-defects"):
		return false
	case rel == "README.md", strings.HasPrefix(rel, "plugins/"), strings.HasPrefix(rel, "docs/"):
		return true
	}
	return false
}

func TestInterviewSkillRetired(t *testing.T) {
	root := repoRoot()
	if _, err := os.Stat(filepath.Join(root, "plugins/crw/skills/crw-interview")); !os.IsNotExist(err) {
		t.Errorf("plugins/crw/skills/crw-interview still exists (%v)", err)
	}
	for _, dir := range []string{"plugins", "docs"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if !interviewRouteScanned(filepath.ToSlash(rel)) {
				return nil
			}
			data, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(data), "crw-interview") {
				t.Errorf("%s still names crw-interview", rel)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if readme := readRepoText(t, "README.md"); strings.Contains(readme, "crw-interview") {
		t.Error("README.md still names crw-interview")
	}
}

func TestDefineRegistersMilestonesAndIssues(t *testing.T) {
	description := descriptionOf(t, defineSkillFile)
	for _, want := range []string{"milestone", "issues", "crw-plan"} {
		if !strings.Contains(description, want) {
			t.Errorf("the crw-define description lacks %q: %q", want, description)
		}
	}
	if strings.Contains(strings.ToLower(description), "initiative") {
		t.Errorf("the crw-define description still defines an initiative: %q", description)
	}
	body := flat(skillBody(t, defineSkillFile))
	for _, want := range []string{
		"registers a milestone and its issues",
		"the top session (today Sion)",
		"purpose", "desired change", "success evidence", "scope", "open decisions",
		"one question at a time", "no ritual interview",
		"](../crw-canon/references/tracking-model.md)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("crw-define lacks %q", want)
		}
	}
	// CRW-1431: the parent's label gives the relay role id (the old form) or stands without it (the new form); the unit
	// that rewrites crw-define (1431-e) tightens this to the new form.
	if !strings.Contains(body, "(repository session, relay role `supervisor`)") && !strings.Contains(body, "(repository session)") {
		t.Error("crw-define does not label the parent as the repository session")
	}
	if !regexp.MustCompile(`the top session[^.]*(holds|has) (that|the) conversation with Jun`).MatchString(body) {
		t.Error("crw-define does not say that the top session holds a needed question's conversation with Jun")
	}
	if strings.Contains(body, "crw-interview") {
		t.Error("crw-define still links crw-interview")
	}
	// Post-evaluation d1: the step runs before the milestone exists, so the skill says where the
	// definition lives until then and who creates the milestone.
	for _, want := range []string{
		"the definition is a draft returned to the caller",
		"crw-plan creates the milestone with it as its description",
		"A milestone that does not exist yet is not created here",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("crw-define does not say where a not yet registered milestone's definition lives: lacks %q", want)
		}
	}
	// Post-evaluation d2: no ownership row points at a separate Interview owner.
	if owners := flat(splitRead(t, "plugins/crw/skills/crw-dev/references/skill-ownership.md")); strings.Contains(owners, "Interview remains separate") {
		t.Error("skill-ownership.md still points at a separate Interview owner")
	}
}

func TestDefaultFlowRaisesDecisionRequests(t *testing.T) {
	for _, file := range []string{pabcdSkillFile, "plugins/crw/skills/crw-dev/references/async-questions.md"} {
		text := flat(splitRead(t, file))
		for _, want := range []string{"decision request", "the session above the project session", "`crw flow report-up`"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", file, want)
			}
		}
	}
	if dev := flat(skillBody(t, "plugins/crw/skills/crw-dev/SKILL.md")); strings.Contains(dev, "Interview-first") || strings.Contains(dev, "until Interview resolves") {
		t.Error("crw-dev still routes unclear requirements to the Interview")
	}
	ladder := "references/intent-discovery-ladder.md"
	if _, err := os.Stat(filepath.Join(repoRoot(), "plugins/crw/skills/crw-dev-uiux-design", ladder)); err != nil {
		t.Errorf("the uiux-design intent ladder is gone: %v", err)
	}
	if uiux := splitRead(t, uiuxSkillFile); !strings.Contains(uiux, ladder) || !strings.Contains(uiux, "Optional deepening") {
		t.Errorf("%s does not keep the intent ladder as its optional path", uiuxSkillFile)
	}
	for _, skill := range []string{"crw-run", "crw-pabcd", "crw-dev"} {
		err := filepath.WalkDir(filepath.Join(repoRoot(), "plugins/crw/skills", skill), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(data), "intent-discovery-ladder") {
				t.Errorf("%s calls the uiux-design intent ladder", path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
