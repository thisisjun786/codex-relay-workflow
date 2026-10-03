//go:build dev

package skillport

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(data), mode))
	must(t, os.Chmod(path, mode))
}

func slurp(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	return string(data)
}

// The original skill of the synthetic CXC tree: every kind of file the staging tool meets.
const (
	skillMD = `---
name: cxc-kwrite
description: "Demo skill for $cxc-pabcd"
---

# Demo

Run cxc orchestrate P after $codexclaw:cxc-loop.
Upstream: https://github.com/lidge-jun/codexclaw
Install record: .codexclaw-install.json
See [reference](references/a.md).
Last line
`
	openaiYAML = `interface:
  display_name: "cxc-kwrite"
  short_description: "Demo."
  default_prompt: "Use $cxc-kwrite to demo."
`
	blob = "\x00cxc\xff"
	// the staged SKILL.md as decided by hand: the names, the verb and the plugin form change, the upstream
	// address and the text of a rewrite rule keep their spelling
	wantSkillSHA = "238d45dde7c838743387edf23ea8adaaffe2b4fd1e4759e921d4795133fde7e9"
)

type fixture struct {
	root string
	src  Source
}

func sourceOf(t *testing.T, tree string) Source {
	t.Helper()
	listing, err := Listing(filepath.Join(tree, "plugins/codexclaw/skills"))
	must(t, err)
	return Source{Dir: tree, Origin: Origin{Tag: "test", Commit: "c0ffee", SkillsListing: listing}}
}

// newFixture is a repository root holding the real name table and a synthetic CXC tree.
func newFixture(t *testing.T) fixture {
	t.Helper()
	root, tree := t.TempDir(), t.TempDir()
	_, file, _, _ := runtime.Caller(0)
	table := slurp(t, filepath.Join(filepath.Dir(file), "../../../contract/schema/cxc/name-substitution.json"))
	put(t, filepath.Join(root, "contract/schema/cxc/name-substitution.json"), table, 0o644)
	skills := filepath.Join(tree, "plugins/codexclaw/skills")
	put(t, skills+"/kwrite/SKILL.md", skillMD, 0o644)
	put(t, skills+"/kwrite/agents/openai.yaml", openaiYAML, 0o644)
	put(t, skills+"/kwrite/references/a.md", "# A\n\nBack to [skill](../SKILL.md).\n", 0o644)
	put(t, skills+"/kwrite/scripts/run.sh", "#!/bin/sh\necho cxc\n", 0o755)
	put(t, skills+"/kwrite/assets/blob.bin", blob, 0o644)
	put(t, skills+"/goalplan/SKILL.md", "redirect stub\n", 0o644)
	return fixture{root, sourceOf(t, tree)}
}

func (f fixture) path(rel string) string {
	return filepath.Join(f.root, StagingRoot, "crw-kwrite", rel)
}

// rewrite changes the staged skill's record.
func (f fixture) rewrite(t *testing.T, change func(*Skill)) {
	t.Helper()
	skill, err := load(f.root, "crw-kwrite")
	must(t, err)
	change(skill)
	data, err := encode(skill)
	must(t, err)
	must(t, os.WriteFile(recordPath(f.root, "crw-kwrite"), data, 0o644))
}

func (f fixture) stage(t *testing.T) {
	t.Helper()
	if _, err := Stage(f.root, f.src, []string{"kwrite"}); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) problems(src *Source) string {
	_, problems := Check(f.root, src)
	return strings.Join(problems, "\n")
}

func (f fixture) expectClean(t *testing.T, label string, src *Source) {
	t.Helper()
	if n, problems := Check(f.root, src); n != 1 || len(problems) != 0 {
		t.Errorf("%s: %d skills, problems %q", label, n, problems)
	}
}

func expectProblem(t *testing.T, label, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s: problems %q lack %q", label, got, want)
	}
}

func TestStageCleanSkill(t *testing.T) {
	f := newFixture(t)
	names, err := Stage(f.root, f.src, []string{"kwrite"})
	if err != nil || !reflect.DeepEqual(names, []string{"crw-kwrite"}) {
		t.Fatalf("Stage = %v, %v", names, err)
	}
	staged := slurp(t, f.path("SKILL.md"))
	if got := sum([]byte(staged)); got != wantSkillSHA {
		t.Errorf("the staged SKILL.md is not the expected text:\n%s", staged)
	}
	if got := slurp(t, f.path("assets/blob.bin")); got != blob {
		t.Errorf("the binary file was changed: %q", got)
	}
	if info, err := os.Stat(f.path("scripts/run.sh")); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("the executable bit was lost: %v %v", info, err)
	}
	f.expectClean(t, "offline", nil)
	f.expectClean(t, "with the source", &f.src)
}

func TestCheckNamesUnrecordedDifferences(t *testing.T) {
	for _, row := range []struct {
		name, want string
		mutate     func(t *testing.T, f fixture)
	}{
		{"edited file", "port/cxc/skills/crw-kwrite/SKILL.md: differs from the substituted original",
			func(t *testing.T, f fixture) { put(t, f.path("SKILL.md"), "changed\n", 0o644) }},
		{"extra file", "crw-kwrite/extra.md: is not in the substituted original",
			func(t *testing.T, f fixture) { put(t, f.path("extra.md"), "x\n", 0o644) }},
		{"deleted file", "crw-kwrite/references/a.md: missing from the staged copy",
			func(t *testing.T, f fixture) { must(t, os.Remove(f.path("references/a.md"))) }},
		{"executable bit", "crw-kwrite/scripts/run.sh: executable bit differs from the substituted original",
			func(t *testing.T, f fixture) { must(t, os.Chmod(f.path("scripts/run.sh"), 0o644)) }},
		{"symlink", "not a regular file",
			func(t *testing.T, f fixture) { must(t, os.Symlink("SKILL.md", f.path("link.md"))) }},
		{"record of another folder", "crw-kwrite.json: record name",
			func(t *testing.T, f fixture) { f.rewrite(t, func(s *Skill) { s.From = "other" }) }},
		{"trailing data in the record", "crw-kwrite.json: trailing data", func(t *testing.T, f fixture) {
			p := recordPath(f.root, "crw-kwrite")
			put(t, p, slurp(t, p)+"]", 0o644)
		}},
		{"skill the table no longer ports", "the name table no longer ports kwrite", func(t *testing.T, f fixture) {
			p := filepath.Join(f.root, "contract/schema/cxc/name-substitution.json")
			put(t, p, strings.Replace(slurp(t, p), `"crw": "crw-kwrite",`, "", 1), 0o644)
		}},
		{"tampered record", "crw-kwrite/SKILL.md: differs from the substituted original",
			func(t *testing.T, f fixture) {
				f.rewrite(t, func(s *Skill) { s.Files["SKILL.md"] = FileEntry{Original: "00"} })
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			f.stage(t)
			row.mutate(t, f)
			expectProblem(t, row.name, f.problems(nil), row.want)
		})
	}
}

func TestMissingAndUnrecordedSkills(t *testing.T) {
	f := newFixture(t)
	if n, problems := Check(f.root, nil); n != 0 || len(problems) != 0 {
		t.Errorf("nothing staged: %d, %q", n, problems)
	}
	f.stage(t)
	must(t, os.RemoveAll(filepath.Dir(f.path("SKILL.md"))))
	expectProblem(t, "directory gone", f.problems(nil), "port/cxc/skills/crw-kwrite: staged skill is missing")
	g := newFixture(t)
	g.stage(t)
	must(t, os.Remove(filepath.Join(g.root, RecordDir, "crw-kwrite.json")))
	expectProblem(t, "record gone", g.problems(nil), "port/cxc/skills/crw-kwrite: staged skill has no record")
	put(t, filepath.Join(g.root, StagingRoot, "crw-other/SKILL.md"), "x\n", 0o644)
	expectProblem(t, "unrecorded directory", g.problems(nil), "port/cxc/skills/crw-other: staged skill has no record")
}

func TestRecordsShareAnOrigin(t *testing.T) {
	f := newFixture(t)
	f.stage(t)
	put(t, filepath.Join(f.src.Dir, "plugins/codexclaw/skills/second/SKILL.md"), "---\nname: cxc-second\ndescription: \"Second\"\n---\n", 0o644)
	_, err := Stage(f.root, sourceOf(t, f.src.Dir), []string{"second"})
	must(t, err)
	expectProblem(t, "two origins", f.problems(nil), "origin differs from the other records")
}

func TestSourceModeAndPinning(t *testing.T) {
	f := newFixture(t)
	f.stage(t)
	f.rewrite(t, func(s *Skill) { s.Files["SKILL.md"] = FileEntry{Original: "00"} })
	expectProblem(t, "stale record", f.problems(&f.src), "record is stale against the source")
	g := newFixture(t)
	g.stage(t)
	put(t, filepath.Join(g.src.Dir, "plugins/codexclaw/skills/kwrite/SKILL.md"), skillMD+"more\n", 0o644)
	expectProblem(t, "another tree", g.problems(&g.src), "source skills tree does not match the record origin")
	h := newFixture(t)
	h.stage(t)
	tablePath := filepath.Join(h.root, "contract/schema/cxc/name-substitution.json")
	table := slurp(t, tablePath)
	put(t, tablePath, strings.Replace(table, "pabcd-state/src/hook.ts PHASE_DIRECTIVES", "prose only", 1), 0o644)
	h.expectClean(t, "a prose-only table change", nil)
	put(t, tablePath, strings.Replace(table, "[CRW-DISPATCH:", "[crw-DISPATCH:", 1), 0o644)
	expectProblem(t, "table drift", h.problems(nil), "name-substitution table changed since this skill was staged")
}

func TestStageRefusals(t *testing.T) {
	f := newFixture(t)
	skills := filepath.Join(f.src.Dir, "plugins/codexclaw/skills")
	put(t, skills+"/ph/SKILL.md", "node \"${PLUGIN_ROOT}/bin/cxc.mjs\" run\n", 0o644)
	src := sourceOf(t, f.src.Dir)
	bad := src
	bad.Origin.SkillsListing = "00"
	for _, row := range []struct {
		src            Source
		folder, expect string
	}{
		{src, "goalplan", "redirect stub"}, {src, "nope", "no such skill"}, {src, "../kwrite", "not a skill folder name"},
		{src, "ph", "{CRW}"}, {bad, "kwrite", "does not match the pinned origin"},
	} {
		if _, err := Stage(f.root, row.src, []string{row.folder}); err == nil || !strings.Contains(err.Error(), row.expect) {
			t.Errorf("Stage(%q) error = %v, want %q", row.folder, err, row.expect)
		}
	}
	must(t, os.Chmod(skills+"/kwrite/scripts/run.sh", 0o644)) // after src was measured: same bytes, another mode
	if _, err := Stage(f.root, src, []string{"kwrite"}); err == nil || !strings.Contains(err.Error(), "does not match the pinned origin") {
		t.Errorf("a source with another mode was staged: %v", err)
	}
	if left := leftovers(t, f.root); len(left) != 0 {
		t.Errorf("refusals left %v", left)
	}
}

// A file and its mode must not read like another file's name.
func TestListingTellsAnExecutableFromAnOddName(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	put(t, a+"/run.sh", "x\n", 0o755)
	put(t, b+"/run.sh x", "x\n", 0o644)
	la, _ := Listing(a)
	if lb, _ := Listing(b); la == lb {
		t.Error("an executable run.sh and a plain file named \"run.sh x\" list alike")
	}
}

func TestListingRefusesANewlineInAName(t *testing.T) {
	dir := t.TempDir()
	put(t, dir+"/a\nb", "x\n", 0o644)
	if _, err := Listing(dir); err == nil {
		t.Error("a file name with a newline was listed")
	}
}

func TestUnsafeLayoutsAreRefused(t *testing.T) {
	for _, row := range []struct {
		name, rel string
		link      bool
	}{{"linked port", "port", true}, {"linked staging root", StagingRoot, true}, {"linked records root", RecordDir, true}, {"records root is a file", RecordDir, false}} {
		t.Run(row.name, func(t *testing.T) {
			f, outside := newFixture(t), t.TempDir()
			path := filepath.Join(f.root, row.rel)
			must(t, os.MkdirAll(filepath.Dir(path), 0o755))
			if row.link {
				must(t, os.Symlink(outside, path))
			} else {
				put(t, path, "x", 0o644)
			}
			if _, err := Stage(f.root, f.src, []string{"kwrite"}); err == nil || !strings.Contains(err.Error(), "not a plain directory") {
				t.Errorf("Stage: %v", err)
			}
			expectProblem(t, "Check", f.problems(nil), "not a plain directory")
			if left, _ := os.ReadDir(outside); len(left) != 0 {
				t.Errorf("wrote through the link: %v", left)
			}
		})
	}
	f := newFixture(t)
	f.stage(t)
	record := recordPath(f.root, "crw-kwrite")
	outside := filepath.Join(t.TempDir(), "record.json")
	put(t, outside, slurp(t, record), 0o644)
	must(t, os.Remove(record))
	must(t, os.Symlink(outside, record))
	expectProblem(t, "linked record", f.problems(nil), "not a regular file")
}

func TestUnreadableRootsAreNotEmpty(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	f := newFixture(t)
	dir := filepath.Join(f.root, StagingRoot)
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.Chmod(dir, 0))
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	expectProblem(t, "unreadable staging root", f.problems(nil), "permission denied")
}

// Two runs that race for one skill: exactly one gets it. The injected rename holds each run until
// both are there, the interleaving in which an empty skill let both publish.
func TestConcurrentStagesPublishOnce(t *testing.T) {
	f := newFixture(t)
	must(t, os.MkdirAll(filepath.Join(f.src.Dir, "plugins/codexclaw/skills/empty"), 0o755))
	var mu sync.Mutex
	waiting, together := 0, make(chan struct{})
	rename := func(from, to string) error {
		mu.Lock()
		if waiting++; waiting == 2 {
			close(together)
		}
		mu.Unlock()
		select {
		case <-together:
		case <-time.After(300 * time.Millisecond):
		}
		return syscall.Rename(from, to) // the kernel's rename, which replaces an empty directory (os.Rename refuses)
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := stage(f.root, f.src, []string{"empty"}, rename)
			errs <- err
		}()
	}
	if a, b := <-errs, <-errs; (a == nil) == (b == nil) {
		t.Fatalf("both or neither staged: %v, %v", a, b)
	}
}
func leftovers(t *testing.T, root string) []string {
	t.Helper()
	var left []string
	for _, pattern := range []string{StagingRoot + "/*", RecordDir + "/*"} {
		found, err := filepath.Glob(filepath.Join(root, pattern))
		must(t, err)
		left = append(left, found...)
	}
	return left
}

func TestStageNeverOverwritesAndRollsBack(t *testing.T) {
	f := newFixture(t)
	f.stage(t)
	put(t, f.path("SKILL.md"), "hand work\n", 0o644)
	if _, err := Stage(f.root, f.src, []string{"kwrite"}); err == nil || !strings.Contains(err.Error(), "stage never overwrites") {
		t.Errorf("Stage over a staged skill: %v", err)
	}
	if got := slurp(t, f.path("SKILL.md")); got != "hand work\n" {
		t.Errorf("hand work was changed: %q", got)
	}
	g := newFixture(t) // the directory rename fails after the record was published
	_, err := stage(g.root, g.src, []string{"kwrite"}, func(string, string) error { return errors.New("injected") })
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("error %v", err)
	}
	if left := leftovers(t, g.root); len(left) != 0 {
		t.Errorf("a failed stage left %v", left)
	}
	h := newFixture(t) // another writer replaces the record after the link and before the failed rename
	_, err = stage(h.root, h.src, []string{"kwrite"}, func(string, string) error {
		p := recordPath(h.root, "crw-kwrite")
		must(t, os.Remove(p))
		put(t, p, "theirs", 0o644)
		return errors.New("injected")
	})
	if got := slurp(t, recordPath(h.root, "crw-kwrite")); err == nil || got != "theirs" {
		t.Errorf("another writer's record was removed: %v %q", err, got)
	}
}
