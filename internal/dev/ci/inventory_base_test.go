//go:build dev

package ci

import (
	"strings"
	"testing"
)

func TestInventoryAssertMultiline(t *testing.T) {
	r := newRepo(t)
	r.write("skill/SKILL.md", "# Title\n\nintro\n\n## 10. Security Ownership Matrix\n\nthe tail\n")
	r.write("skill/dir/a.md", "x1\nx2\n")
	r.write("skill/dir/b.md", "x3\n")
	r.write("skill/dir/sub/c.md", "no match here\n")
	r.commit()
	head := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	trees := map[string]treeFiles{"working tree": dirTree{r.root}, "git tree": gitTree{root: r.root, commit: head}}
	for label, tree := range trees {
		for _, row := range []struct {
			name   string
			assert inventoryAssert
			holds  bool
		}{
			{"^ anchors at a line start in the middle of the file", inventoryAssert{Path: "skill/SKILL.md", Pattern: "^## .*10", Want: "present"}, true},
			{"$ anchors at a line end", inventoryAssert{Path: "skill/SKILL.md", Pattern: "^the tail$", Want: "present"}, true},
			{"a heading level is a line start too", inventoryAssert{Path: "skill/SKILL.md", Pattern: "^#+ Title$", Want: "count 1"}, true},
			{"absent holds when nothing matches", inventoryAssert{Path: "skill/SKILL.md", Pattern: "^## .*11", Want: "absent"}, true},
			{"absent fails on a match", inventoryAssert{Path: "skill/SKILL.md", Pattern: "^## .*10", Want: "absent"}, false},
			{"count N sums the files of a directory", inventoryAssert{Path: "skill/dir", Pattern: "^x[0-9]$", Want: "count 3"}, true},
			{"count N is exact", inventoryAssert{Path: "skill/dir/", Pattern: "^x[0-9]$", Want: "count 2"}, false},
			{"count 0 holds for no match", inventoryAssert{Path: "skill/dir", Pattern: "^y", Want: "count 0"}, true},
			{"a missing file has no match: present fails", inventoryAssert{Path: "skill/none.md", Pattern: ".", Want: "present"}, false},
			{"a missing file has no match: absent holds", inventoryAssert{Path: "skill/none.md", Pattern: ".", Want: "absent"}, true},
			{"a missing file has no match: count 0 holds", inventoryAssert{Path: "skill/none.md", Pattern: ".", Want: "count 0"}, true},
			{"exists sees a file", inventoryAssert{Path: "skill/SKILL.md", Want: "exists"}, true},
			{"exists sees a directory with files", inventoryAssert{Path: "skill/dir", Want: "exists"}, true},
			{"missing sees an absent file", inventoryAssert{Path: "skill/none.md", Want: "missing"}, true},
			{"missing fails on a file", inventoryAssert{Path: "skill/SKILL.md", Want: "missing"}, false},
			{"(?i) inside the pattern works behind the (?m) prefix", inventoryAssert{Path: "skill/SKILL.md", Pattern: "(?i)^## .*SECURITY", Want: "present"}, true},
		} {
			got, err := row.assert.holds(tree)
			if err != nil {
				t.Errorf("%s: %s: %v", label, row.name, err)
			} else if got != row.holds {
				t.Errorf("%s: %s: holds = %v, want %v", label, row.name, got, row.holds)
			}
		}
	}
	if _, err := (inventoryAssert{Path: "skill/SKILL.md", Pattern: "(", Want: "present"}).holds(dirTree{r.root}); err == nil {
		t.Error("an invalid pattern must be an error")
	}
	for _, bad := range []inventoryAssert{
		{Path: "p", Want: "present"}, {Path: "p", Pattern: "x", Want: "exists"}, {Path: "p", Pattern: "x", Want: "count"},
		{Path: "p", Pattern: "x", Want: "count -1"}, {Path: "p", Pattern: "x", Want: "sometimes"}, {Pattern: "x", Want: "present"},
		{Path: "../p", Pattern: "x", Want: "present"}, {Path: "/abs", Pattern: "x", Want: "present"}, {Path: "p", Pattern: "(", Want: "present"},
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("%+v must not validate", bad)
		}
	}
}

// baseRepo is a repository whose first commit is the base the inventory map names; the map is
// written after it, naming the commit.
func baseRepo(t *testing.T, rows string) (*fixtureRepo, string) {
	t.Helper()
	r := validateRepo(t)
	r.write("skill/a.md", "old text\n## Section\n")
	r.commit()
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	r.write(inventoryMapPath, `{"base": "`+base+`", "rows": {`+rows+`}, "descriptions": {}}`+"\n")
	return r, base
}

const baseRows = `
"collisions[0]": {"disposition": "fix", "owner": "S02", "contributors": [], "check": ["TestX"], "status": "open", "assert": [{"path": "skill/a.md", "pattern": "old text", "want": "present"}]},
"collisions[1]": {"disposition": "split", "owner": "S02", "contributors": [], "check": ["TestX"], "status": "done", "assert": [{"path": "skill/a.md", "pattern": "old text", "want": "present", "guard": true}, {"path": "skill/new.md", "want": "exists"}]},
"collisions[2]": {"disposition": "delete", "owner": "S02", "contributors": [], "check": ["TestX"], "status": "open", "assert": [{"path": "skill/a.md", "pattern": "^## Gone$", "want": "absent"}]},
"collisions[3]": {"disposition": "keep", "owner": "S02", "contributors": [], "check": ["TestX"], "status": "open", "reason": "kept", "assert": [{"path": "skill/a.md", "pattern": "old text", "want": "present"}]},
"collisions[4]": {"disposition": "fix", "owner": "S02", "contributors": [], "check": ["TestX"], "status": "open", "assert": [{"path": "skill/a.md", "pattern": "new text", "want": "present"}, {"path": "skill/gone.md", "want": "missing"}]}
`

func TestInventoryBaseRule(t *testing.T) {
	r, base := baseRepo(t, baseRows)
	errs := InventoryBaseErrors(r.root)
	prefix := inventoryMapPath + ": "
	expectEqual(t, "errors", errs, []string{
		prefix + "collisions[0]: assert 1 (skill/a.md: /old text/ present) already holds at base " + base[:12],
		prefix + "collisions[2]: assert 1 (skill/a.md: /^## Gone$/ absent) already holds at base " + base[:12],
		prefix + "collisions[4]: assert 2 (skill/gone.md missing) already holds at base " + base[:12],
	})
	// A guard assert is exempt, a keep row is not read, and an open row is read like a done one
	// (collisions[0] above is open).
	// The CLI runs the rule: validate fails with the same errors.
	got := validate(t, r)
	if got.code != 1 || !strings.Contains(got.stderr, errs[0]) || !strings.Contains(got.stderr, errs[2]) {
		t.Errorf("validate: %+v", got)
	}
	// Asserts that fail at base pass the rule.
	clean := `"collisions[0]": {"disposition": "fix", "owner": "S02", "contributors": [], "check": ["TestX"], "status": "open", "assert": [{"path": "skill/a.md", "pattern": "new text", "want": "present"}]}`
	r.write(inventoryMapPath, `{"base": "`+base+`", "rows": {`+clean+`}, "descriptions": {}}`+"\n")
	expectEqual(t, "clean", InventoryBaseErrors(r.root), []string(nil))
	if got := validate(t, r); got.code != 0 || !strings.HasPrefix(got.stdout, validated) || got.stderr != "" {
		t.Errorf("clean validate: %+v", got)
	}
	// A base commit the repository does not hold is an error, never a skip.
	missing := strings.Repeat("ab", 20)
	r.write(inventoryMapPath, `{"base": "`+missing+`", "rows": {`+clean+`}, "descriptions": {}}`+"\n")
	errs = InventoryBaseErrors(r.root)
	if len(errs) != 1 || !strings.Contains(errs[0], "base commit "+missing+" is not in the repository") {
		t.Errorf("missing base: %q", errs)
	}
	if got := validate(t, r); got.code != 1 || !strings.Contains(got.stderr, "is not in the repository") {
		t.Errorf("validate with a missing base: %+v", got)
	}
	// A base that is not a full sha is an error too (a branch name moves).
	r.write(inventoryMapPath, `{"base": "HEAD", "rows": {`+clean+`}, "descriptions": {}}`+"\n")
	errs = InventoryBaseErrors(r.root)
	if len(errs) != 1 || !strings.Contains(errs[0], "base must be a full 40-hex commit sha") {
		t.Errorf("short base: %q", errs)
	}
	// A repository without the table has nothing to evaluate.
	r2 := validateRepo(t)
	expectEqual(t, "no table", InventoryBaseErrors(r2.root), []string(nil))
	if got := validate(t, r2); got.code != 0 || !strings.HasPrefix(got.stdout, validated) || got.stderr != "" {
		t.Errorf("no table validate: %+v", got)
	}
}
