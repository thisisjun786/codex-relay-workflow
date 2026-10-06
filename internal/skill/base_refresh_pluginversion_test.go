package skill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
)

// The plugin manifest's version line, settled without a declaration (CRW-664). The fixture is a real
// repository whose plugins/crw package records the version its payload derives: two branches change
// different skill files and each records its own version, so the merge conflicts on that one line.

const pluginRepository = "owner/repo"

// pluginManifestText is the fixture manifest with the version and description given.
func pluginManifestText(version, description string) string {
	return "{\n  \"name\": \"crw\",\n  \"version\": \"" + version + "\",\n  \"description\": \"" + description + "\"\n}\n"
}

func pluginSkill(name string) string {
	return "---\nname: " + name + "\ndescription: d\n---\n"
}

type pluginVersionFixture struct {
	t        *testing.T
	r        *refreshRepo
	previous string // the verified head: the tip of feature before the update
	devTip   string // the tip of dev the update merges
	recorded string // the version the head records
}

func newPluginVersionFixture(t *testing.T) *pluginVersionFixture {
	t.Helper()
	r := newRefreshRepoFormat(t, "sha1")
	f := &pluginVersionFixture{t: t, r: r}
	f.put(pluginversion.ManifestRepoPath, pluginManifestText("0.4.0", "d"))
	f.put(pluginversion.PluginRelative+"/LICENSE", "MIT\n")
	f.put(pluginversion.PluginRelative+"/skills/crw-run/SKILL.md", pluginSkill("crw-run"))
	f.put(pluginversion.PluginRelative+"/skills/crw-plan/SKILL.md", pluginSkill("crw-plan"))
	f.record()
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "plugin package")
	r.branchFrom("feature", "dev")
	return f
}

func (f *pluginVersionFixture) put(file, content string) {
	f.t.Helper()
	path := filepath.Join(f.r.path, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// record is what crw-dev ci plugin --record-version does: it writes the version that names the
// payload of the working tree into the manifest, and answers it.
func (f *pluginVersionFixture) record() string {
	f.t.Helper()
	p, errs := pluginversion.DirectoryPayload(filepath.Join(f.r.path, filepath.FromSlash(pluginversion.PluginRelative)))
	if len(errs) > 0 {
		f.t.Fatalf("the fixture payload: %v", errs)
	}
	current, err := pluginversion.ManifestVersion(p)
	if err != nil {
		f.t.Fatal(err)
	}
	next, err := pluginversion.PayloadVersion(p, current)
	if err != nil {
		f.t.Fatal(err)
	}
	f.put(pluginversion.ManifestRepoPath, pluginManifestText(next, "d"))
	f.recorded = next
	return next
}

func (f *pluginVersionFixture) commitOn(branch, message string, files map[string]string, record bool) string {
	f.t.Helper()
	f.r.git("checkout", "-q", branch)
	for file, content := range files {
		f.put(file, content)
	}
	if record {
		f.record()
	}
	f.r.git("add", "-A")
	f.r.git("commit", "-q", "-m", message)
	return f.r.git("rev-parse", "HEAD")
}

// standard is the picture this rule is for: the two branches change different skill files and each
// records its own version, so the merge conflicts on the manifest's version line and nowhere else.
func (f *pluginVersionFixture) standard() {
	f.t.Helper()
	f.previous = f.commitOn("feature", "previous work", map[string]string{
		pluginversion.PluginRelative + "/skills/crw-plan/SKILL.md": pluginSkill("crw-plan") + "previous paragraph.\n"}, true)
	f.devTip = f.commitOn("dev", "dev work", map[string]string{
		pluginversion.PluginRelative + "/skills/crw-run/SKILL.md": pluginSkill("crw-run") + "dev paragraph.\n"}, true)
}

// startMerge is the parent's update: merge dev into feature, which stops on the manifest.
func (f *pluginVersionFixture) startMerge() {
	f.t.Helper()
	f.r.git("checkout", "-q", "feature")
	f.r.gitFails("merge", "--no-ff", "-m", "Merge branch 'dev' into feature", "dev")
}

// finish commits the work tree as the merge and returns the new head.
func (f *pluginVersionFixture) finish() string {
	f.t.Helper()
	f.r.git("add", "-A")
	f.r.git("commit", "-q", "-m", "Merge branch 'dev' into feature")
	return f.r.git("rev-parse", "HEAD")
}

// resolve is the parent's resolution: take either side's manifest and record the version again on
// the merged tree.
func (f *pluginVersionFixture) resolve() string {
	f.t.Helper()
	return f.resolveWith("0.4.0")
}

// resolveWith is the same resolution with the release the head records chosen by the resolver.
func (f *pluginVersionFixture) resolveWith(release string) string {
	f.t.Helper()
	f.put(pluginversion.ManifestRepoPath, pluginManifestText(release, "d"))
	f.record()
	return f.finish()
}

// regions is a declaration that covers nothing: no declared region names the manifest.
func (f *pluginVersionFixture) regions() string {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "regions.json")
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// regionsFile is a declaration holding the regions given.
func (f *pluginVersionFixture) regionsFile(regions ...mechRegion) string {
	f.t.Helper()
	data, err := json.Marshal(regions)
	if err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(f.t.TempDir(), "regions.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *pluginVersionFixture) run(head string, regions ...string) skillProcessResult {
	f.t.Helper()
	if len(regions) == 0 {
		regions = []string{f.regions()}
	}
	args := []string{"base-refresh", "mechanical", "--repo", f.r.path, "--previous", f.previous, "--head", head, "--base", "dev", "--repository", pluginRepository}
	for _, file := range regions {
		args = append(args, "--regions", file)
	}
	return runSkillInProcess(args...)
}

// wantPass is the pass the built-in rule prints: the usual evidence, and the applied line naming the
// rule and the version the head derives.
func (f *pluginVersionFixture) wantPass(t *testing.T, got skillProcessResult, head string) {
	t.Helper()
	tree := f.r.git("rev-parse", head+"^{tree}")
	for _, line := range []string{
		"ok: " + head + " is " + f.previous + " plus the tip of dev (" + f.devTip + ")",
		"evidence: previous=" + f.previous + " dev_tip=" + f.devTip + " head=" + head + " tree=" + tree + " rule=mechanical_resolution\n",
		"parents: (" + f.previous + ", " + f.devTip + ") in that order\n",
		"applied: regenerate path=" + pluginversion.ManifestRepoPath + " rule=regenerate:plugin-version version=" + f.recorded + "\n",
	} {
		if !strings.Contains(got.stdout, line) {
			t.Fatalf("the pass lacks %q: %+v", line, got)
		}
	}
	if got.exit != 0 || strings.Contains(got.stdout, "refused") {
		t.Fatalf("want a pass, got %+v", got)
	}
}

func TestMechanicalPluginVersionLine(t *testing.T) {
	t.Run("the version line alone is settled without a declaration", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.standard()
		f.startMerge()
		head := f.resolve()
		f.wantPass(t, f.run(head), head)
	})
	t.Run("a manifest that merged cleanly and was recorded again is settled", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{
			pluginversion.PluginRelative + "/skills/crw-plan/SKILL.md": pluginSkill("crw-plan") + "previous paragraph.\n"}, false)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{
			pluginversion.PluginRelative + "/skills/crw-run/SKILL.md": pluginSkill("crw-run") + "dev paragraph.\n"}, true)
		f.r.git("checkout", "-q", "feature")
		f.r.git("merge", "-q", "--no-commit", "--no-ff", "-m", "Merge branch 'dev' into feature", "dev")
		f.put(pluginversion.ManifestRepoPath, pluginManifestText("0.4.0", "d"))
		f.record()
		f.wantPass(t, f.run(f.finish()), f.r.git("rev-parse", "HEAD"))
	})
}

func TestMechanicalPluginVersionLineRefusals(t *testing.T) {
	t.Run("a version that is not the one the head derives", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.standard()
		f.startMerge()
		f.put(pluginversion.ManifestRepoPath, pluginManifestText("0.4.0+000000000000", "d"))
		wantMechRefused(t, f.run(f.finish()), "conflict_outside_mechanical", pluginversion.ManifestRepoPath)
	})
	t.Run("another line of the manifest differs from both parents", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.standard()
		f.startMerge()
		f.put(pluginversion.ManifestRepoPath, pluginManifestText("0.4.0", "d"))
		f.record()
		f.put(pluginversion.ManifestRepoPath, pluginManifestText(f.recorded, "another description"))
		wantMechRefused(t, f.run(f.finish()), "conflict_outside_mechanical", pluginversion.ManifestRepoPath)
	})
	t.Run("a conflict in another file", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.standard()
		f.previous = f.commitOn("feature", "previous notes", map[string]string{"notes.md": "p\n"}, false)
		f.devTip = f.commitOn("dev", "dev notes", map[string]string{"notes.md": "d\n"}, false)
		f.startMerge()
		f.put("notes.md", "p\nd\n")
		wantMechRefused(t, f.run(f.resolve()), "conflict_outside_mechanical", "notes.md")
	})
	t.Run("a declared region still decides the file", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.standard()
		f.startMerge()
		head := f.resolve()
		regions := f.regionsFile(mechanical(pluginversion.ManifestRepoPath, "regenerate:false"))
		wantMechRefused(t, f.run(head, regions), "regeneration_failed")
	})
	t.Run("a declaration that covers the file without a mechanical rule", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.standard()
		f.startMerge()
		head := f.resolve()
		local := mechanical(pluginversion.ManifestRepoPath, "")
		local.Grade = "local"
		wantMechRefused(t, f.run(head, f.regionsFile(local)), "conflict_outside_mechanical", pluginversion.ManifestRepoPath)
	})
	t.Run("the parents record two releases", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{
			pluginversion.PluginRelative + "/skills/crw-plan/SKILL.md": pluginSkill("crw-plan") + "previous paragraph.\n"}, true)
		f.r.git("checkout", "-q", "dev")
		f.put(pluginversion.ManifestRepoPath, pluginManifestText("0.5.0", "d"))
		f.devTip = f.commitOn("dev", "dev work", map[string]string{
			pluginversion.PluginRelative + "/skills/crw-run/SKILL.md": pluginSkill("crw-run") + "dev paragraph.\n"}, true)
		f.startMerge()
		// the head keeps the previous head's release and records the payload it has, so only the
		// release the base chose stands in the way
		wantMechRefused(t, f.run(f.resolveWith("0.4.0")), "conflict_outside_mechanical", pluginversion.ManifestRepoPath)
	})
	t.Run("the head records a release neither parent has", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.standard()
		f.startMerge()
		wantMechRefused(t, f.run(f.resolveWith("1.0.0")), "conflict_outside_mechanical", pluginversion.ManifestRepoPath)
	})
	t.Run("the head makes the manifest executable", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.standard()
		f.startMerge()
		// the version is recorded for the executable payload, so the mode is the only difference
		f.put(pluginversion.ManifestRepoPath, pluginManifestText("0.4.0", "d"))
		if err := os.Chmod(filepath.Join(f.r.path, filepath.FromSlash(pluginversion.ManifestRepoPath)), 0o755); err != nil {
			t.Fatal(err)
		}
		f.record()
		wantMechRefused(t, f.run(f.finish()), "conflict_outside_mechanical", pluginversion.ManifestRepoPath)
	})
}
