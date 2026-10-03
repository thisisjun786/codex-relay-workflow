//go:build dev

package skillport

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
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
See [reference](references/a.md).
Last line
`
	openaiYAML = `interface:
  display_name: "cxc-kwrite"
  short_description: "Demo."
  default_prompt: "Use $cxc-kwrite to demo."
`
	blob = "\x00cxc\xff"
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
	for _, want := range []string{"name: crw-kwrite", "$crw-pabcd", "crw pabcd orchestrate P", "$crw:crw-loop", "https://github.com/lidge-jun/codexclaw"} {
		if !strings.Contains(staged, want) {
			t.Errorf("staged SKILL.md lacks %q:\n%s", want, staged)
		}
	}
	if strings.Contains(staged, "cxc") {
		t.Errorf("a CXC name is left:\n%s", staged)
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
	if left := leftovers(t, f.root); len(left) != 0 {
		t.Errorf("refusals left %v", left)
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
	if _, err := Stage(f.root, f.src, []string{"kwrite"}); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("Stage over a staged skill: %v", err)
	}
	if got := slurp(t, f.path("SKILL.md")); got != "hand work\n" {
		t.Errorf("hand work was changed: %q", got)
	}
	for failing, name := range []string{"directory rename", "record rename"} {
		g, calls := newFixture(t), 0
		_, err := stage(g.root, g.src, []string{"kwrite"}, func(from, to string) error {
			if calls++; calls == failing+1 {
				return errors.New("injected")
			}
			return os.Rename(from, to)
		})
		if err == nil || !strings.Contains(err.Error(), "injected") {
			t.Errorf("%s: error %v", name, err)
		}
		if left := leftovers(t, g.root); len(left) != 0 {
			t.Errorf("%s left %v", name, left)
		}
	}
}

// The pinned listing digest is the real one when CRW_CXC_V0240_TREE names an extracted CXC v0.2.40 tree.
func TestDefaultOriginMatchesTheOracle(t *testing.T) {
	tree := os.Getenv("CRW_CXC_V0240_TREE")
	if tree == "" {
		t.Skip("CRW_CXC_V0240_TREE is not set")
	}
	if got, err := Listing(Source{Dir: tree}.skills()); err != nil || got != DefaultOrigin().SkillsListing {
		t.Errorf("Listing = %q, %v; DefaultOrigin has %q", got, err, DefaultOrigin().SkillsListing)
	}
}

func TestRun(t *testing.T) {
	f := newFixture(t)
	do := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run(args, &out, &errb, f.src.Origin)
		return code, out.String(), errb.String()
	}
	for _, row := range [][]string{{}, {"nope"}, {"edits"}, {"stage", "--root", f.root, "kwrite"}, {"check", "extra"}} {
		if code, _, _ := do(row...); code != 2 {
			t.Errorf("run(%q) exit %d, want 2", row, code)
		}
	}
	if code, out, errs := do("stage", "--root", f.root, "--source", f.src.Dir, "kwrite"); code != 0 || !strings.Contains(out, "staged crw-kwrite") {
		t.Fatalf("stage: %d %q %q", code, out, errs)
	}
	if code, out, _ := do("check", "--root", f.root, "--source", f.src.Dir); code != 0 || !strings.Contains(out, "Checked 1 staged skills") || !strings.Contains(out, "fidelity only") {
		t.Errorf("check: %d %q", code, out)
	}
	put(t, f.path("SKILL.md"), "changed\n", 0o644)
	if code, _, errs := do("check", "--root", f.root); code != 1 || !strings.Contains(errs, "SKILL.md: differs") {
		t.Errorf("check after a change: %d %q", code, errs)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"stage", "--root", f.root, "--source", f.src.Dir, "kwrite"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "does not match the pinned origin") {
		t.Errorf("Run with a synthetic tree: %d %q", code, errb.String())
	}
}
