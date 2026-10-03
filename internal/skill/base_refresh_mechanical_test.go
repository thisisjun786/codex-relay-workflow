package skill

import (
	"fmt"
	"strings"
	"testing"
)

// These tests run the mechanical-resolution check (CRW-412) on real temporary repositories: a
// conflicted update of a pull request branch that the parent resolves by the declared rules, and
// the resolutions it may not accept. Every refusal names its code on the first line.

const (
	unionApplied = "applied: union path=backlog.md base_lines=3 previous_added=2 dev_added=1 result_lines=6\n"
	regenApplied = "applied: regenerate path=plugin.json command=\"sh regen.sh\" runs=2 identical=yes matches_head=yes\n"
)

func TestMechanicalResolutionPassesWhatItsRulesAllow(t *testing.T) {
	t.Run("both sides' lines are kept and the version is recorded again on the merged tree", func(t *testing.T) {
		f := newMechFixture(t)
		head := f.merged()
		f.wantMechPasses(f.run(head), head, unionApplied, regenApplied)
	})
	t.Run("the dev tip's lines may come first", func(t *testing.T) {
		f := newMechFixture(t)
		f.standard()
		f.startMerge()
		head := f.resolve(backlogBase + "- d1\n- p1\n- p2\n")
		f.wantMechPasses(f.run(head), head, unionApplied, regenApplied)
	})
	t.Run("the two sides' lines may interleave, each side keeping its own order", func(t *testing.T) {
		f := newMechFixture(t)
		f.standard()
		f.startMerge()
		head := f.resolve(backlogBase + "- p1\n- d1\n- p2\n")
		f.wantMechPasses(f.run(head), head, unionApplied, regenApplied)
	})
	t.Run("a line both sides added stands twice", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{"backlog.md": backlogBase + "- same\n- p1\n"}, false)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{"backlog.md": backlogBase + "- same\n- d1\n"}, false)
		f.startMerge()
		f.put("backlog.md", backlogBase+"- same\n- same\n- p1\n- d1\n")
		head := f.finish()
		f.wantMechPasses(f.run(head), head, "applied: union path=backlog.md base_lines=3 previous_added=2 dev_added=2 result_lines=7\n")
	})
	t.Run("a union region that merged cleanly is left alone", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{"backlog.md": "# Backlog\n- p0\n- e1\n- e2\n", "payload/one.txt": "one P\n"}, true)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{"backlog.md": backlogBase + "- d1\n", "payload/two.txt": "two D\n"}, true)
		f.startMerge()
		if got := f.read("backlog.md"); got != "# Backlog\n- p0\n- e1\n- e2\n- d1\n" {
			t.Fatalf("the fixture was meant to merge the backlog cleanly, got %q", got)
		}
		f.regen()
		head := f.finish()
		got := f.run(head)
		f.wantMechPasses(got, head, regenApplied)
		if strings.Contains(got.stdout, "applied: union") {
			t.Fatalf("a union was applied to a path that did not conflict: %+v", got)
		}
	})
	t.Run("an untracked build output outside the regions is ignored", func(t *testing.T) {
		f := newMechFixture(t)
		head := f.merged()
		regions := f.regionsFile(mechanical("backlog.md", "union"), mechanical("plugin.json", "regenerate:sh regen.sh; echo built > build.out"))
		f.wantMechPasses(f.run(head, regions), head, unionApplied, "applied: regenerate path=plugin.json command=\"sh regen.sh; echo built > build.out\" runs=2 identical=yes matches_head=yes\n")
	})
	t.Run("the regions of two nodes that name one rule", func(t *testing.T) {
		f := newMechFixture(t)
		head := f.merged()
		f.wantMechPasses(f.run(head, f.standardRegions(), f.standardRegions()), head, unionApplied, regenApplied)
	})
}

func TestMechanicalUnionRefusals(t *testing.T) {
	cases := []struct {
		name, backlog, code string
		says                []string
	}{
		{"it loses a line of the previous head", backlogBase + "- p1\n- d1\n", "union_line_lost", []string{"previous head", "- p2"}},
		{"it loses a line of the dev tip", backlogBase + "- p1\n- p2\n", "union_line_lost", []string{"dev tip", "- d1"}},
		{"it keeps the dev tip's side only", backlogBase + "- d1\n", "union_line_lost", []string{"previous head", "- p1"}},
		{"it empties the file", "", "union_line_lost", nil},
		{"it drops a line of the base", "# Backlog\n- e2\n- p1\n- p2\n- d1\n", "union_line_lost", []string{"- e1"}},
		{"it reorders the previous head's lines", backlogBase + "- p2\n- p1\n- d1\n", "union_line_lost", []string{"previous head"}},
		{"it adds a line neither side has", backlogBase + "- p1\n- p2\n- d1\n- x\n", "union_line_added", []string{"- x"}},
		{"it repeats a line of one side", backlogBase + "- p1\n- p1\n- p2\n- d1\n", "union_line_added", []string{"- p1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMechFixture(t)
			f.standard()
			f.startMerge()
			head := f.resolve(c.backlog)
			wantMechRefused(t, f.run(head), c.code, c.says...)
		})
	}
	t.Run("it keeps the conflict markers", func(t *testing.T) {
		f := newMechFixture(t)
		f.standard()
		f.startMerge()
		f.regen()
		head := f.finish()
		wantMechRefused(t, f.run(head), "union_line_added", "<<<<<<<")
	})
	t.Run("it keeps one copy of a line both sides added", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{"backlog.md": backlogBase + "- same\n- p1\n"}, false)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{"backlog.md": backlogBase + "- same\n- d1\n"}, false)
		f.startMerge()
		f.put("backlog.md", backlogBase+"- same\n- p1\n- d1\n")
		wantMechRefused(t, f.run(f.finish()), "union_line_lost", "- same")
	})
	t.Run("the previous head changed a line of the base", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{"backlog.md": "# Backlog\n- e1 edited\n- e2\n- p1\n"}, false)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{"backlog.md": backlogBase + "- d1\n"}, false)
		f.startMerge()
		f.put("backlog.md", "# Backlog\n- e1 edited\n- e2\n- p1\n- d1\n")
		wantMechRefused(t, f.run(f.finish()), "union_not_additions", "previous head")
	})
	t.Run("the dev tip deleted a line of the base", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{"backlog.md": backlogBase + "- p1\n"}, false)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{"backlog.md": "# Backlog\n- e2\n- d1\n"}, false)
		f.startMerge()
		f.put("backlog.md", "# Backlog\n- e2\n- p1\n- d1\n")
		wantMechRefused(t, f.run(f.finish()), "union_not_additions", "dev tip")
	})
	t.Run("a last line without a newline is a line of its own", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{"backlog.md": backlogBase + "- p1\n"}, false)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{"backlog.md": backlogBase + "- d1"}, false)
		f.startMerge()
		f.put("backlog.md", backlogBase+"- p1\n- d1")
		head := f.finish()
		f.wantMechPasses(f.run(head), head, "applied: union path=backlog.md base_lines=3 previous_added=1 dev_added=1 result_lines=5\n")
		f.r.git("checkout", "-q", "-B", "alternative", f.previous)
		f.r.gitFails("merge", "--no-ff", "-m", "Merge branch 'dev' into feature", "dev")
		f.put("backlog.md", backlogBase+"- p1\n- d1\n")
		wantMechRefused(t, f.run(f.finish()), "union_line_lost", "dev tip")
	})
	t.Run("a union path that merged cleanly and was changed", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{"backlog.md": "# Backlog\n- p0\n- e1\n- e2\n", "payload/one.txt": "one P\n"}, true)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{"backlog.md": backlogBase + "- d1\n", "payload/two.txt": "two D\n"}, true)
		f.startMerge()
		head := f.resolve("# Backlog\n- p0\n- e1\n- e2\n- d1\n- x\n")
		wantMechRefused(t, f.run(head), "union_not_conflicted", "backlog.md")
	})
}

func TestMechanicalVersionRegeneration(t *testing.T) {
	staleManifest := func(f *mechFixture, from string) string {
		f.standard()
		f.startMerge()
		f.put("backlog.md", backlogBase+"- p1\n- p2\n- d1\n")
		f.put("plugin.json", f.r.git("show", from+":plugin.json")+"\n")
		return f.finish()
	}
	t.Run("the suffix is left at the previous head's value", func(t *testing.T) {
		f := newMechFixture(t)
		head := staleManifest(f, "feature")
		wantMechRefused(t, f.run(head), "regeneration_differs", "plugin.json")
	})
	t.Run("the suffix is left at the dev tip's value", func(t *testing.T) {
		f := newMechFixture(t)
		head := staleManifest(f, "dev")
		wantMechRefused(t, f.run(head), "regeneration_differs", "plugin.json")
	})
	t.Run("the conflict markers are left in the manifest", func(t *testing.T) {
		f := newMechFixture(t)
		f.standard()
		f.startMerge()
		f.put("backlog.md", backlogBase+"- p1\n- p2\n- d1\n")
		wantMechRefused(t, f.run(f.finish()), "regeneration_differs", "plugin.json")
	})
	t.Run("the version is recorded again on the merged tree", func(t *testing.T) {
		f := newMechFixture(t)
		head := f.merged()
		got := f.run(head)
		f.wantMechPasses(got, head, regenApplied)
		if f.read("plugin.json") == f.r.git("show", f.previous+":plugin.json")+"\n" || f.read("plugin.json") == f.r.git("show", f.devTip+":plugin.json")+"\n" {
			t.Fatal("the fixture was meant to record a version that is neither side's")
		}
	})
}

func TestMechanicalOutsideTheRegions(t *testing.T) {
	t.Run("an edit rides in the merge", func(t *testing.T) {
		f := newMechFixture(t)
		f.standard()
		f.startMerge()
		f.put("other.txt", "smuggled\n")
		wantMechRefused(t, f.run(f.resolve(backlogBase+"- p1\n- p2\n- d1\n")), "differs_outside_mechanical", "other.txt")
	})
	t.Run("a new file rides in the merge", func(t *testing.T) {
		f := newMechFixture(t)
		f.standard()
		f.startMerge()
		f.put("smuggled.txt", "not from either side\n")
		wantMechRefused(t, f.run(f.resolve(backlogBase+"- p1\n- p2\n- d1\n")), "differs_outside_mechanical", "smuggled.txt")
	})
	t.Run("the payload is edited after the version was recorded", func(t *testing.T) {
		f := newMechFixture(t)
		f.standard()
		f.startMerge()
		f.put("backlog.md", backlogBase+"- p1\n- p2\n- d1\n")
		f.regen()
		f.put("payload/one.txt", "tampered\n")
		wantMechRefused(t, f.run(f.finish()), "differs_outside_mechanical", "payload/one.txt")
	})
	t.Run("a conflict in a file no region names", func(t *testing.T) {
		f := newMechFixture(t)
		f.standard()
		f.previous = f.commitOn("feature", "previous notes", map[string]string{"notes.md": "p note\n"}, false)
		f.devTip = f.commitOn("dev", "dev notes", map[string]string{"notes.md": "d note\n"}, false)
		f.startMerge()
		f.put("notes.md", "p note\nd note\n")
		wantMechRefused(t, f.run(f.resolve(backlogBase+"- p1\n- p2\n- d1\n")), "conflict_outside_mechanical", "notes.md")
	})
	t.Run("renumber has no check", func(t *testing.T) {
		f := newMechFixture(t)
		head := f.merged()
		regions := f.regionsFile(mechanical("backlog.md", "renumber"), mechanical("plugin.json", "regenerate:sh regen.sh"))
		wantMechRefused(t, f.run(head, regions), "rule_unchecked", "renumber", "backlog.md")
	})
}

func TestMechanicalRegionCoverage(t *testing.T) {
	cases := []struct {
		name    string
		regions func(f *mechFixture) []string
		says    string
	}{
		{"a region of another grade", func(f *mechFixture) []string {
			local := mechanical("backlog.md", "")
			local.Grade = "local"
			return []string{f.regionsFile(local, mechanical("plugin.json", "regenerate:sh regen.sh"))}
		}, "backlog.md"},
		{"a region that is independent", func(f *mechFixture) []string {
			independent := mechanical("backlog.md", "")
			independent.Grade = "independent"
			return []string{f.regionsFile(mechanical("backlog.md", "union"), independent, mechanical("plugin.json", "regenerate:sh regen.sh"))}
		}, "backlog.md"},
		{"a symbol region", func(f *mechFixture) []string {
			symbol := mechanical("backlog.md", "union")
			symbol.Kind, symbol.Key = "symbol", "Entries"
			return []string{f.regionsFile(symbol, mechanical("plugin.json", "regenerate:sh regen.sh"))}
		}, "backlog.md"},
		{"a region that deletes the file", func(f *mechFixture) []string {
			deleted := mechanical("backlog.md", "union")
			deleted.Change = "delete"
			return []string{f.regionsFile(deleted, mechanical("plugin.json", "regenerate:sh regen.sh"))}
		}, "backlog.md"},
		{"two rules for one file", func(f *mechFixture) []string {
			return []string{f.regionsFile(mechanical("backlog.md", "union"), mechanical("backlog.md", "regenerate:sh regen.sh"), mechanical("plugin.json", "regenerate:sh regen.sh"))}
		}, "backlog.md"},
		{"a second node that names another rule", func(f *mechFixture) []string {
			return []string{f.standardRegions(), f.regionsFile(mechanical("backlog.md", "union"), mechanical("plugin.json", "regenerate:sh other.sh"))}
		}, "plugin.json"},
		{"a second node that does not name the file", func(f *mechFixture) []string {
			return []string{f.standardRegions(), f.regionsFile(mechanical("backlog.md", "union"))}
		}, "plugin.json"},
		{"a region of another repository", func(f *mechFixture) []string {
			elsewhere := mechanical("backlog.md", "union")
			elsewhere.Repository = "other/repo"
			return []string{f.regionsFile(elsewhere, mechanical("plugin.json", "regenerate:sh regen.sh"))}
		}, "backlog.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMechFixture(t)
			head := f.merged()
			wantMechRefused(t, f.run(head, c.regions(f)...), "conflict_outside_mechanical", c.says)
		})
	}
	// a tree region covers the files under it, a file region of another grade inside it takes one back, and two trees that
	// hold a shared contract surface are one exclusive overlap whatever rule they name
	tree := func(path, rule string) mechRegion {
		r := mechanical(path, rule)
		r.Kind = "tree"
		return r
	}
	listFixture := func(t *testing.T, file string) (*mechFixture, string) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{file: "# List\n- a\n- p\n"}, false)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{file: "# List\n- a\n- d\n"}, false)
		f.startMerge()
		f.put(file, "# List\n- a\n- p\n- d\n")
		return f, f.finish()
	}
	t.Run("a tree region covers the files under it", func(t *testing.T) {
		f, head := listFixture(t, "docs/list.md")
		f.wantMechPasses(f.run(head, f.regionsFile(tree("docs", "union"))), head, "applied: union path=docs/list.md base_lines=2 previous_added=1 dev_added=1 result_lines=4\n")
	})
	t.Run("a file region of another grade inside a tree region", func(t *testing.T) {
		f, head := listFixture(t, "docs/list.md")
		local := mechanical("docs/list.md", "")
		local.Grade = "local"
		wantMechRefused(t, f.run(head, f.regionsFile(tree("docs", "union"), local)), "conflict_outside_mechanical", "docs/list.md")
	})
	t.Run("a shared contract surface is never covered", func(t *testing.T) {
		f, head := listFixture(t, "contract/golden/list.txt")
		wantMechRefused(t, f.run(head, f.regionsFile(mechanical("contract/golden/list.txt", "union"))), "conflict_outside_mechanical", "contract/golden/list.txt")
	})
	t.Run("two trees that hold a shared contract surface", func(t *testing.T) {
		f, head := listFixture(t, "internal/relay/list.md")
		one := f.regionsFile(tree("internal/relay", "union"))
		wantMechRefused(t, f.run(head, one, f.regionsFile(tree("internal/relay", "union"))), "conflict_outside_mechanical", "internal/relay/list.md")
	})
}

func TestMechanicalRegeneration(t *testing.T) {
	cases := []struct {
		name, command, code string
		says                []string
	}{
		{"a command that writes its process id", "sh flaky.sh", "regeneration_not_deterministic", []string{"plugin.json"}},
		{"a command that depends on what the file held", "sh append.sh", "regeneration_not_deterministic", []string{"plugin.json"}},
		{"a command that leaves the file as it finds it", "true", "regeneration_not_deterministic", []string{"plugin.json"}},
		{"a command that fails", "exit 3", "regeneration_failed", []string{"exit status 3"}},
		{"a command that changes a tracked file outside the regions", "sh regen.sh; echo x >> other.txt", "regeneration_touches_outside", []string{"other.txt"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMechFixture(t)
			head := f.merged()
			regions := f.regionsFile(mechanical("backlog.md", "union"), mechanical("plugin.json", "regenerate:"+c.command))
			wantMechRefused(t, f.run(head, regions), c.code, c.says...)
		})
	}
	t.Run("a command that takes longer than the timeout is not an answer", func(t *testing.T) {
		f := newMechFixture(t)
		head := f.merged()
		regions := f.regionsFile(mechanical("backlog.md", "union"), mechanical("plugin.json", "regenerate:sleep 5"))
		got := f.runWith(head, []string{regions}, "--regenerate-timeout", "1s")
		if got.exit != 2 || !strings.Contains(got.stderr, "timed out") || strings.Contains(got.stdout, "ok:") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("the command runs in a checkout of the head", func(t *testing.T) {
		f := newMechFixture(t)
		head := f.merged()
		command := fmt.Sprintf("test \"$(git rev-parse HEAD)\" = %s && sh regen.sh", head)
		regions := f.regionsFile(mechanical("backlog.md", "union"), mechanical("plugin.json", "regenerate:"+command))
		got := f.run(head, regions)
		f.wantMechPasses(got, head, unionApplied)
	})
}

func TestMechanicalConflictsThatAreNotPlainText(t *testing.T) {
	union := func(f *mechFixture, paths ...string) string {
		var regions []mechRegion
		for _, p := range paths {
			regions = append(regions, mechanical(p, "union"))
		}
		return f.regionsFile(regions...)
	}
	t.Run("a file one side deleted and the other changed", func(t *testing.T) {
		f := newMechFixture(t)
		f.r.git("checkout", "-q", "dev")
		f.put("gone.md", "# Gone\n- a\n")
		f.r.git("add", "-A")
		f.r.git("commit", "-q", "-m", "gone")
		f.r.git("checkout", "-q", "-B", "feature", "dev")
		f.r.git("rm", "-q", "gone.md")
		f.r.git("commit", "-q", "-m", "previous deletes it")
		f.previous = f.r.git("rev-parse", "HEAD")
		f.devTip = f.commitOn("dev", "dev appends", map[string]string{"gone.md": "# Gone\n- a\n- d\n"}, false)
		f.startMerge()
		f.put("gone.md", "# Gone\n- a\n- d\n")
		wantMechRefused(t, f.run(f.finish(), union(f, "gone.md")), "conflict_not_content", "gone.md")
	})
	t.Run("a binary file both sides added", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous binary", map[string]string{"data.bin": "a\x00b\n"}, false)
		f.devTip = f.commitOn("dev", "dev binary", map[string]string{"data.bin": "c\x00d\n"}, false)
		f.startMerge()
		f.put("data.bin", "a\x00b\nc\x00d\n")
		wantMechRefused(t, f.run(f.finish(), union(f, "data.bin")), "conflict_not_content", "data.bin")
	})
	renamed := func(t *testing.T, bulk int) (*mechFixture, string) {
		f := newMechFixture(t)
		f.r.git("checkout", "-q", "dev")
		f.put("old.md", "# Old\n- a\n- b\n- c\n- d\n- e\n- f\n- g\n- h\n")
		for i := 0; i < bulk; i++ {
			f.put(fmt.Sprintf("bulk/f%04d.txt", i), fmt.Sprintf("file %d\n", i))
		}
		f.r.git("add", "-A")
		f.r.git("commit", "-q", "-m", "old file and bulk")
		f.r.git("checkout", "-q", "-B", "feature", "dev")
		f.r.git("mv", "old.md", "new.md")
		if bulk > 0 {
			f.r.git("mv", "bulk", "bulk2")
		}
		f.r.git("commit", "-q", "-m", "previous renames")
		f.previous = f.r.git("rev-parse", "HEAD")
		f.devTip = f.commitOn("dev", "dev adds the new name", map[string]string{"new.md": "# New\n- d\n"}, false)
		f.startMerge()
		f.put("new.md", "# Old\n- a\n- b\n- c\n- d\n- e\n- f\n- g\n- h\n# New\n- d\n")
		return f, f.finish()
	}
	t.Run("one side renamed the file the other side created", func(t *testing.T) {
		f, head := renamed(t, 0)
		wantMechRefused(t, f.run(head, union(f, "new.md")), "conflict_not_content", "new.md")
	})
	t.Run("the same where rename detection of a plain diff is skipped", func(t *testing.T) {
		f, head := renamed(t, 1001)
		wantMechRefused(t, f.run(head, union(f, "new.md")), "conflict_not_content", "new.md")
	})
	t.Run("a criss-cross has more than one merge base", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous side file", map[string]string{"f1.txt": "f1\n"}, false)
		first := f.previous
		f.devTip = f.commitOn("dev", "dev side file", map[string]string{"d1.txt": "d1\n"}, false)
		devFirst := f.devTip
		f.r.git("checkout", "-q", "feature")
		f.r.git("merge", "-q", "--no-ff", "-m", "feature takes dev", devFirst)
		f.r.git("checkout", "-q", "-B", "dev", devFirst)
		f.r.git("merge", "-q", "--no-ff", "-m", "dev takes feature", first)
		f.previous = f.commitOn("feature", "previous appends", map[string]string{"backlog.md": backlogBase + "- p1\n"}, false)
		f.devTip = f.commitOn("dev", "dev appends", map[string]string{"backlog.md": backlogBase + "- d1\n"}, false)
		f.startMerge()
		f.put("backlog.md", backlogBase+"- p1\n- d1\n")
		wantMechRefused(t, f.run(f.finish(), union(f, "backlog.md")), "ambiguous_base")
	})
}

func TestMechanicalRefusesAHeadThatIsNotAnUpdate(t *testing.T) {
	t.Run("the head is the previous head", func(t *testing.T) {
		f := newMechFixture(t)
		f.merged()
		wantMechRefused(t, f.run(f.previous), "no_update")
	})
	t.Run("a commit of its own sits on top of the update", func(t *testing.T) {
		f := newMechFixture(t)
		f.merged()
		head := f.commitOn("feature", "fix after the update", map[string]string{"other.txt": "changed\n"}, false)
		wantMechRefused(t, f.run(head), "not_a_merge")
	})
	t.Run("the merge went the other way", func(t *testing.T) {
		f := newMechFixture(t)
		f.standard()
		f.r.branchFrom("reverse", "dev")
		f.r.gitFails("merge", "--no-ff", "-m", "Merge feature into dev", "feature")
		head := f.resolve(backlogBase + "- p1\n- p2\n- d1\n")
		wantMechRefused(t, f.run(head), "parents_swapped")
	})
	t.Run("the base moved after the update", func(t *testing.T) {
		f := newMechFixture(t)
		head := f.merged()
		f.commitOn("dev", "dev moves again", map[string]string{"later.txt": "later\n"}, false)
		wantMechRefused(t, f.run(head), "not_the_dev_tip")
	})
	t.Run("a merge without a conflict settles nothing", func(t *testing.T) {
		f := newMechFixture(t)
		f.previous = f.commitOn("feature", "previous work", map[string]string{"feature.txt": "feature work\n"}, false)
		f.devTip = f.commitOn("dev", "dev work", map[string]string{"dev.txt": "dev work\n"}, false)
		f.r.git("checkout", "-q", "feature")
		f.r.git("merge", "-q", "--no-ff", "-m", "Merge branch 'dev' into feature", "dev")
		wantMechRefused(t, f.run(f.r.git("rev-parse", "HEAD")), "nothing_resolved", "check")
	})
}

func TestMechanicalCommandLine(t *testing.T) {
	f := newMechFixture(t)
	head := f.merged()
	regions := f.standardRegions()
	base := []string{"base-refresh", "mechanical", "--repo", f.r.path, "--previous", f.previous, "--head", head, "--base", "dev", "--repository", mechRepository, "--regions", regions}
	without := func(flag string) []string {
		var out []string
		for i := 0; i < len(base); i++ {
			if base[i] == flag {
				i++
				continue
			}
			out = append(out, base[i])
		}
		return out
	}
	for _, flag := range []string{"--previous", "--head", "--base", "--repository", "--regions"} {
		got := runSkillInProcess(without(flag)...)
		if got.exit != 2 || !strings.Contains(got.stderr, flag+" is required") {
			t.Fatalf("without %s: %+v", flag, got)
		}
	}
	for name, text := range map[string]string{
		"not JSON":                         "not json",
		"an unknown field":                 "[{\"repository\":\"owner/repo\",\"path\":\"backlog.md\",\"kind\":\"file\",\"grade\":\"mechanical\",\"rule\":\"union\",\"extra\":1}]",
		"a mechanical region with no rule": "[{\"repository\":\"owner/repo\",\"path\":\"backlog.md\",\"kind\":\"file\",\"grade\":\"mechanical\"}]",
		"a rule that is not one":           "[{\"repository\":\"owner/repo\",\"path\":\"backlog.md\",\"kind\":\"file\",\"grade\":\"mechanical\",\"rule\":\"merge it\"}]",
		"a path outside the repository":    "[{\"repository\":\"owner/repo\",\"path\":\"../x\",\"kind\":\"file\",\"grade\":\"independent\"}]",
	} {
		got := f.run(head, f.regionsText(text))
		if got.exit != 2 || !strings.Contains(got.stderr, "regions") || strings.Contains(got.stdout, "ok:") {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	if got := runSkillInProcess(append(without("--head"), "--head", "no-such-revision")...); got.exit != 2 {
		t.Fatalf("an unknown head: %+v", got)
	}
}

func TestMechanicalCheckLeavesTheCheckoutAlone(t *testing.T) {
	f := newMechFixture(t)
	head := f.merged()
	state := func() string {
		return f.r.git("count-objects", "-v") + f.r.git("for-each-ref") + f.r.git("status", "--porcelain") + f.r.git("rev-parse", "HEAD")
	}
	before := state()
	f.wantMechPasses(f.run(head), head, unionApplied, regenApplied)
	if after := state(); after != before {
		t.Fatalf("the check changed the checkout:\n%s\n---\n%s", before, after)
	}
}
