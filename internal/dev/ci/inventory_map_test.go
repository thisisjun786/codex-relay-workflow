//go:build dev

package ci

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The inventory map (docs/skills/inventory-map.json) has one row for each of the 83 overlap items of
// the skill inventory, keyed <category>[<index>]. This file is its checker: the identifier sets are equal, every row has a
// disposition, an owner and a check, and a done row has the evidence its disposition asks for. The
// completion mode (no open row left) is on since the issue that closes the series (S07, CRW-1222).

// inventoryCompletionMode is the completion mode switch: the final issue of the series (S07) set it, so a row that is open is an error.
const inventoryCompletionMode = true

// defaultTestPackage is the package of a check test the registry does not name.
const defaultTestPackage = "internal/dev/ci"

// inventoryCategories are the categories of the inventory's overlap section and their item counts.
var inventoryCategories = []struct {
	name  string
	count int
}{{"collisions", 14}, {"duplicateContent", 16}, {"roleMixing", 19}, {"triggerAmbiguity", 14}, {"sizeHotspots", 6}, {"deadOrStale", 14}}

var (
	inventoryOwner       = regexp.MustCompile(`^S0[1-7]$`)
	inventoryContributor = regexp.MustCompile(`^[SF][0-9]{2}$`)
	inventoryDescOwner   = regexp.MustCompile(`^S0[1-6]$`)
	inventoryTestName    = regexp.MustCompile(`^Test[A-Za-z0-9_]*[A-Za-z0-9]$`)
	inventoryRowID       = regexp.MustCompile(`^[a-zA-Z]+\[[0-9]+\]$`)
	// notEvidenceTests read the whole tree and prove no row.
	notEvidenceTests = []string{"TestSkillAnchorsResolve", "TestSkillInventoryMapComplete"}
)

// canonicalInventoryIDs is the identifier set of the inventory, built from the category counts.
func canonicalInventoryIDs() []string {
	var ids []string
	for _, category := range inventoryCategories {
		for i := 0; i < category.count; i++ {
			ids = append(ids, fmt.Sprintf("%s[%d]", category.name, i))
		}
	}
	return ids
}

// testFunc is a top-level test function found by parsing a package's test files.
type testFunc struct {
	file     string
	literals map[string]bool
}

// packageTests is the top-level `func Test...(t *testing.T)` of the *_test.go files in the package
// directory root/pkg, read with go/parser; the file declaring each also gives its Go string literals.
func packageTests(root, pkg string) (map[string]testFunc, error) {
	files, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pkg), "*_test.go"))
	if err != nil {
		return nil, err
	}
	tests := map[string]testFunc{}
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		var literals map[string]bool
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || !takesTestingT(fn.Type) {
				continue
			}
			if literals == nil {
				literals = stringLiterals(parsed)
			}
			tests[fn.Name.Name] = testFunc{file: file, literals: literals}
		}
	}
	return tests, nil
}

// takesTestingT reports whether the function type is func(t *testing.T) with nothing else.
func takesTestingT(fn *ast.FuncType) bool {
	if fn.Results != nil && len(fn.Results.List) > 0 || fn.Params == nil || len(fn.Params.List) != 1 || len(fn.Params.List[0].Names) > 1 {
		return false
	}
	star, ok := fn.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing"
}

// stringLiterals is every Go string literal of the file, unquoted.
func stringLiterals(file *ast.File) map[string]bool {
	literals := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if value, err := strconv.Unquote(lit.Value); err == nil {
				literals[value] = true
			}
		}
		return true
	})
	return literals
}

// registryErrors is what is wrong with the rowTestReads registry: a test that does not exist in
// its package, a read path that is not a Go string literal of the file declaring the test, a
// malformed entry or a test registered twice.
func registryErrors(root string, registry []rowTestRead) []string {
	var errs []string
	seen := map[string]bool{}
	packages := map[string]map[string]testFunc{}
	for _, entry := range registry {
		label := "rowTestReads " + entry.Test
		if seen[entry.Package+" "+entry.Test] {
			errs = append(errs, label+": registered twice")
		}
		seen[entry.Package+" "+entry.Test] = true
		if slices.Contains(notEvidenceTests, entry.Test) {
			errs = append(errs, label+": reads the whole tree and proves no row; it is not registered")
		}
		if len(entry.Reads) == 0 {
			errs = append(errs, label+": no read path")
		}
		tests, ok := packages[entry.Package]
		if !ok {
			var err error
			if tests, err = packageTests(root, entry.Package); err != nil {
				errs = append(errs, label+": "+err.Error())
				continue
			}
			packages[entry.Package] = tests
		}
		declared, ok := tests[entry.Test]
		if !ok {
			errs = append(errs, label+": no top-level func "+entry.Test+"(t *testing.T) in the *_test.go files of "+entry.Package)
			continue
		}
		for _, read := range entry.Reads {
			if !declared.literals[read] {
				errs = append(errs, label+": the read path "+strconv.Quote(read)+" is not a Go string literal of "+filepath.Base(declared.file))
			}
		}
		for _, row := range entry.Rows {
			if !declared.literals[row] {
				errs = append(errs, label+": the row "+strconv.Quote(row)+" is not a Go string literal of "+filepath.Base(declared.file))
			}
		}
	}
	return errs
}

// covers reports whether the registered read path covers the row path: they are the same, or the row
// path lies below the read path. Both are plain repository-relative paths (plainPath), so a path
// that leaves the read path through ".." is not below it.
func covers(read, path string) bool {
	if plainPath(read) != nil || plainPath(path) != nil {
		return false
	}
	read, path = strings.TrimSuffix(read, "/"), strings.TrimSuffix(path, "/")
	return read == path || strings.HasPrefix(path, read+"/")
}

// inventoryMapErrors is the format-mode check of the map m against the identifier list ids: the
// registry says which paths the check tests read; root is the checkout the asserts, the test files
// and the registry are read in. With completion set a row that is still open is an error too.
func inventoryMapErrors(root string, m inventoryMap, ids []string, registry []rowTestRead, completion bool) []string {
	var errs []string
	fail := func(key, format string, args ...any) { errs = append(errs, key+": "+fmt.Sprintf(format, args...)) }
	// The identifier sets.
	listed := map[string]bool{}
	for _, id := range ids {
		if !inventoryRowID.MatchString(id) {
			fail(id, "not a <category>[<index>] identifier")
		}
		if listed[id] {
			fail(id, "listed twice in the identifier copy")
		}
		listed[id] = true
	}
	for _, id := range sortedKeys(listed) {
		if _, ok := m.Rows[id]; !ok {
			fail(id, "has no row")
		}
	}
	for _, key := range sortedKeys(m.Rows) {
		if !listed[key] {
			fail(key, "is not one of the inventory's identifiers")
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(m.Base) {
		fail("base", "must be a full 40-hex commit sha")
	}
	errs = append(errs, registryErrors(root, registry)...)
	tests := map[string]map[string]testFunc{}
	testsOf := func(pkg string) map[string]testFunc {
		if found, ok := tests[pkg]; ok {
			return found
		}
		found, err := packageTests(root, pkg)
		if err != nil {
			errs = append(errs, "package "+pkg+": "+err.Error())
		}
		tests[pkg] = found
		return found
	}
	work := dirTree{root}
	for _, key := range sortedKeys(m.Rows) {
		row := m.Rows[key]
		switch row.Disposition {
		case "fix", "split", "delete":
		case "keep":
			if strings.TrimSpace(row.Reason) == "" {
				fail(key, "a keep row states its reason")
			}
		default:
			fail(key, "disposition %q is not fix, split, delete or keep", row.Disposition)
		}
		if !inventoryOwner.MatchString(row.Owner) {
			fail(key, "owner %q is not one issue S01-S07", row.Owner)
		}
		for _, contributor := range row.Contributors {
			if !inventoryContributor.MatchString(contributor) || contributor == row.Owner {
				fail(key, "contributor %q is not an issue other than the owner", contributor)
			}
		}
		if len(row.Check) == 0 {
			fail(key, "no check")
		}
		for _, name := range row.Check {
			if !inventoryTestName.MatchString(name) {
				fail(key, "check %q is not an exact test function name (a prefix is not a name)", name)
			}
		}
		if row.Status != "open" && row.Status != "done" {
			fail(key, "status %q is not open or done", row.Status)
		}
		for phrase, owner := range row.Triggers {
			if strings.TrimSpace(phrase) == "" || owner == "" {
				fail(key, "a trigger needs a phrase and an owning skill or none")
			}
		}
		for _, path := range row.Paths {
			if err := relativePath(path); err != nil {
				fail(key, "path %q: %v", path, err)
			}
		}
		for i, assert := range row.Assert {
			if err := assert.validate(); err != nil {
				fail(key, "assert %d: %v", i+1, err)
			}
		}
		for _, other := range row.SameAs {
			if _, ok := m.Rows[other]; !ok || other == key {
				fail(key, "sameAs %q is not another row", other)
			}
		}
		if row.Status == "open" && completion {
			fail(key, "is still open")
		}
		if row.Status != "done" {
			continue
		}
		// A done row: its named tests exist, its asserts hold, its sameAs rows are done, and a fix,
		// split or delete row has evidence.
		for _, name := range row.Check {
			pkg := defaultTestPackage
			for _, entry := range registry {
				if entry.Test == name {
					pkg = entry.Package
				}
			}
			if _, ok := testsOf(pkg)[name]; !ok {
				fail(key, "check %s is not a top-level func %s(t *testing.T) in the *_test.go files of %s", name, name, pkg)
			}
		}
		for i, assert := range row.Assert {
			if holds, err := assert.holds(work); err != nil {
				fail(key, "assert %d (%s): %v", i+1, assert.describe(), err)
			} else if !holds {
				fail(key, "assert %d (%s) does not hold", i+1, assert.describe())
			}
		}
		sameAsDone := len(row.SameAs) > 0
		for _, other := range row.SameAs {
			if m.Rows[other].Status != "done" {
				sameAsDone = false
				fail(key, "sameAs row %s is not done", other)
			}
		}
		sameAsDone = sameAsDone && sameAsEvidence(m, registry, key, map[string]bool{key: true})
		if row.Disposition == "keep" {
			continue
		}
		if !ownEvidence(key, row, registry) && !sameAsDone {
			fail(key, "a done %s row needs evidence: a check test registered in rowTestReads with one of its paths, "+
				"a non-guard assert, or sameAs rows that are all done and end in such evidence", row.Disposition)
		}
	}
	errs = append(errs, descriptionTableErrors(m)...)
	return errs
}

// ownEvidence reports whether the done fix, split or delete row key has evidence of its own: a check
// test registered in rowTestReads for one of its paths (or for the row), or a non-guard assert.
func ownEvidence(key string, row inventoryRow, registry []rowTestRead) bool {
	for _, assert := range row.Assert {
		if !assert.Guard {
			return true
		}
	}
	for _, entry := range registry {
		if !slices.Contains(row.Check, entry.Test) || slices.Contains(notEvidenceTests, entry.Test) {
			continue
		}
		if len(entry.Rows) > 0 && !slices.Contains(entry.Rows, key) {
			continue // the test proves other rows, not this one
		}
		if len(entry.Rows) > 0 && len(row.Paths) == 0 {
			return true
		}
		for _, read := range entry.Reads {
			for _, path := range row.Paths {
				if covers(read, path) {
					return true
				}
			}
		}
	}
	return false
}

// sameAsEvidence reports whether every sameAs row of the row key is done and ends, through its own
// sameAs rows, in a row with evidence of its own (ownEvidence) or a keep row: rows that name each
// other certify nothing. visiting holds the rows on the chain, so a cycle is no evidence.
func sameAsEvidence(m inventoryMap, registry []rowTestRead, key string, visiting map[string]bool) bool {
	for _, name := range m.Rows[key].SameAs {
		other, ok := m.Rows[name]
		if !ok || other.Status != "done" || visiting[name] {
			return false
		}
		if other.Disposition == "keep" || ownEvidence(name, other, registry) {
			continue
		}
		if len(other.SameAs) == 0 {
			return false
		}
		visiting[name] = true
		ok = sameAsEvidence(m, registry, name, visiting)
		delete(visiting, name)
		if !ok {
			return false
		}
	}
	return true
}

// relativePath is the error of a path that is not a plain repository-relative path.
func relativePath(path string) error { return plainPath(path) }

// descriptionTableErrors is the shape of the descriptions table.
func descriptionTableErrors(m inventoryMap) []string {
	var errs []string
	for _, name := range sortedKeys(m.Descriptions) {
		entry := m.Descriptions[name]
		if !skillName.MatchString(name) {
			errs = append(errs, "descriptions "+name+": not a skill name")
		}
		if !inventoryDescOwner.MatchString(entry.Owner) {
			errs = append(errs, fmt.Sprintf("descriptions %s: owner %q is not one issue S01-S06", name, entry.Owner))
		}
		if entry.Status != "open" && entry.Status != "done" {
			errs = append(errs, fmt.Sprintf("descriptions %s: status %q is not open or done", name, entry.Status))
		}
	}
	return errs
}

// inventoryFixture is a checkout in a temporary directory that holds a test file declaring TestKnown
// (reading the literal "plugins/crw/skills/") and the files the asserts read.
func inventoryFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name, text string) {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/dev/ci/known_test.go", "package ci\n\nimport \"testing\"\n\n// TestCommentOnly is named in a comment.\n\nfunc TestKnown(t *testing.T) { _ = \"plugins/crw/skills/\"; _ = \"collisions[1]\" }\n\n"+
		"func TestNotATest(t *testing.T, extra int) {}\n\nfunc (x X) TestMethod(t *testing.T) {}\n\nfunc TestKnownOther(t *testing.T) {}\n")
	write("plugins/crw/skills/a/SKILL.md", "# A\n\nnew text\n")
	return root
}

func fixtureRow(status string, mutate func(*inventoryRow)) inventoryRow {
	row := inventoryRow{Disposition: "fix", Owner: "S02", Contributors: []string{}, Check: []string{"TestKnown"}, Status: status}
	if mutate != nil {
		mutate(&row)
	}
	return row
}

func TestSkillInventoryMapComplete(t *testing.T) {
	t.Run("the checked-in table", func(t *testing.T) {
		root := repoRoot()
		mapData, err := os.ReadFile(filepath.Join(root, inventoryMapPath))
		if err != nil {
			t.Fatal(err)
		}
		m, err := loadInventoryMap(mapData)
		if err != nil {
			t.Fatal(err)
		}
		idData, err := os.ReadFile(filepath.Join(root, inventoryIDsPath))
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		if err := json.Unmarshal(idData, &ids); err != nil {
			t.Fatal(err)
		}
		// The pinned copy is the inventory's 83 identifiers, in category order.
		expectEqual(t, "the pinned identifier copy", ids, canonicalInventoryIDs())
		if len(m.Rows) != 83 || len(m.Descriptions) != 31 {
			t.Errorf("%d rows and %d description entries, want 83 and 31", len(m.Rows), len(m.Descriptions))
		}
		if errs := inventoryMapErrors(root, m, ids, rowTestReads, inventoryCompletionMode); len(errs) > 0 {
			t.Errorf("%d problems:\n%s", len(errs), strings.Join(errs, "\n"))
		}
		// An open row needs nothing more than a disposition, an owner and a check; at S01 only the
		// rows S01 owns are done.
		for _, key := range sortedKeys(m.Rows) {
			if row := m.Rows[key]; row.Owner == "S01" && row.Status != "done" {
				t.Errorf("%s: S01 owns it, so it starts done", key)
			}
		}
		// Each row is one line of the file, so an issue changes only the lines of its own rows.
		for _, key := range sortedKeys(m.Rows) {
			if !strings.Contains(string(mapData), "\n    "+strconv.Quote(key)+": {") {
				t.Errorf("%s: not on a line of its own", key)
			}
		}
	})

	ids := []string{"collisions[0]", "collisions[1]"}
	check := func(label string, rows map[string]inventoryRow, ids []string, registry []rowTestRead, completion bool, want ...string) {
		t.Helper()
		root := inventoryFixture(t)
		m := inventoryMap{Base: strings.Repeat("a", 40), Rows: rows}
		errs := inventoryMapErrors(root, m, ids, registry, completion)
		if len(want) == 0 && len(errs) > 0 {
			t.Errorf("%s: unexpected errors:\n%s", label, strings.Join(errs, "\n"))
		}
		for _, text := range want {
			found := false
			for _, err := range errs {
				found = found || strings.Contains(err, text)
			}
			if !found {
				t.Errorf("%s: no error contains %q:\n%s", label, text, strings.Join(errs, "\n"))
			}
		}
		if len(want) > 0 && len(errs) == 0 {
			t.Errorf("%s: want errors containing %q, got none", label, want)
		}
	}
	registry := []rowTestRead{{"TestKnown", defaultTestPackage, []string{"plugins/crw/skills/"}, nil}}
	open := func(mutate func(*inventoryRow)) inventoryRow { return fixtureRow("open", mutate) }

	t.Run("fixture: the identifier sets must be equal", func(t *testing.T) {
		both := map[string]inventoryRow{"collisions[0]": open(nil), "collisions[1]": open(nil)}
		check("equal", both, ids, nil, false)
		check("missing row", map[string]inventoryRow{"collisions[0]": open(nil)}, ids, nil, false, "collisions[1]: has no row")
		check("extra row", map[string]inventoryRow{"collisions[0]": open(nil), "collisions[1]": open(nil), "collisions[2]": open(nil)}, ids, nil, false,
			"collisions[2]: is not one of the inventory's identifiers")
		check("duplicate id", both, []string{"collisions[0]", "collisions[1]", "collisions[1]"}, nil, false, "collisions[1]: listed twice")
	})
	t.Run("fixture: an open row needs only a disposition, an owner and a check", func(t *testing.T) {
		bare := inventoryRow{Disposition: "fix", Owner: "S05", Check: []string{"TestNotWrittenYet"}, Status: "open"}
		check("open and bare", map[string]inventoryRow{"collisions[0]": bare, "collisions[1]": bare}, ids, nil, false)
		for label, mutate := range map[string]func(*inventoryRow){
			"disposition": func(r *inventoryRow) { r.Disposition = "" },
			"owner":       func(r *inventoryRow) { r.Owner = "" },
			"two owners":  func(r *inventoryRow) { r.Owner = "S02, S03" },
			"check":       func(r *inventoryRow) { r.Check = nil },
			"prefix":      func(r *inventoryRow) { r.Check = []string{"TestDeliveryAuthority_"} },
			"keep reason": func(r *inventoryRow) { r.Disposition = "keep" },
		} {
			check(label, map[string]inventoryRow{"collisions[0]": open(mutate), "collisions[1]": open(nil)}, ids, nil, false, "collisions[0]:")
		}
	})
	t.Run("fixture: a done fix row without evidence fails", func(t *testing.T) {
		for _, disposition := range []string{"fix", "split", "delete"} {
			row := fixtureRow("done", func(r *inventoryRow) { r.Disposition = disposition })
			check(disposition, map[string]inventoryRow{"collisions[0]": row, "collisions[1]": open(nil)}, ids, registry, false, "needs evidence")
		}
		// A guard assert is not evidence; the two excluded tests do not count as registered evidence.
		guardOnly := fixtureRow("done", func(r *inventoryRow) {
			r.Assert = []inventoryAssert{{Path: "plugins/crw/skills/a/SKILL.md", Pattern: "new text", Want: "present", Guard: true}}
		})
		check("guard only", map[string]inventoryRow{"collisions[0]": guardOnly, "collisions[1]": open(nil)}, ids, registry, false, "needs evidence")
		pathOnly := fixtureRow("done", func(r *inventoryRow) {
			r.Check = []string{"TestSkillInventoryMapComplete"}
			r.Paths = []string{"plugins/crw/skills/a/"}
		})
		check("excluded test", map[string]inventoryRow{"collisions[0]": pathOnly, "collisions[1]": open(nil)}, ids,
			[]rowTestRead{{"TestSkillInventoryMapComplete", defaultTestPackage, []string{"plugins/crw/skills/"}, nil}}, false, "needs evidence", "reads the whole tree")
	})
	t.Run("fixture: each kind of evidence is enough", func(t *testing.T) {
		registered := fixtureRow("done", func(r *inventoryRow) { r.Paths = []string{"plugins/crw/skills/a/SKILL.md"} })
		check("registered path", map[string]inventoryRow{"collisions[0]": registered, "collisions[1]": open(nil)}, ids, registry, false)
		asserted := fixtureRow("done", func(r *inventoryRow) {
			r.Assert = []inventoryAssert{{Path: "plugins/crw/skills/a/SKILL.md", Pattern: "new text", Want: "present"}}
		})
		check("assert", map[string]inventoryRow{"collisions[0]": asserted, "collisions[1]": open(nil)}, ids, registry, false)
		same := fixtureRow("done", func(r *inventoryRow) { r.SameAs = []string{"collisions[1]"} })
		check("sameAs done", map[string]inventoryRow{"collisions[0]": same, "collisions[1]": asserted}, ids, registry, false)
		check("sameAs open", map[string]inventoryRow{"collisions[0]": same, "collisions[1]": open(nil)}, ids, registry, false, "sameAs row collisions[1] is not done")
		// Rows that name each other certify nothing: a sameAs chain ends in a row with evidence of its own.
		other := fixtureRow("done", func(r *inventoryRow) { r.SameAs = []string{"collisions[0]"} })
		check("sameAs cycle", map[string]inventoryRow{"collisions[0]": same, "collisions[1]": other}, ids, registry, false,
			"collisions[0]: a done fix row needs evidence", "collisions[1]: a done fix row needs evidence")
		keep := fixtureRow("done", func(r *inventoryRow) { r.Disposition, r.Reason = "keep", "nothing to do" })
		check("keep", map[string]inventoryRow{"collisions[0]": keep, "collisions[1]": open(nil)}, ids, registry, false)
	})
	t.Run("fixture: a done row asserts what holds and names tests that exist", func(t *testing.T) {
		wrong := fixtureRow("done", func(r *inventoryRow) {
			r.Assert = []inventoryAssert{{Path: "plugins/crw/skills/a/SKILL.md", Pattern: "old text", Want: "present"}}
		})
		check("assert does not hold", map[string]inventoryRow{"collisions[0]": wrong, "collisions[1]": open(nil)}, ids, registry, false, "does not hold")
		wrongGuard := fixtureRow("done", func(r *inventoryRow) {
			r.Assert = []inventoryAssert{{Path: "plugins/crw/skills/a/SKILL.md", Pattern: "old text", Want: "present", Guard: true},
				{Path: "plugins/crw/skills/a/SKILL.md", Pattern: "new text", Want: "present"}}
		})
		check("guard does not hold", map[string]inventoryRow{"collisions[0]": wrongGuard, "collisions[1]": open(nil)}, ids, registry, false, "does not hold")
		for label, name := range map[string]string{"missing test": "TestNoSuchTest", "prefix": "TestKnown_", "comment only": "TestCommentOnly",
			"wrong signature": "TestNotATest", "method": "TestMethod", "name prefix": "TestKnownO"} {
			row := fixtureRow("done", func(r *inventoryRow) {
				r.Check = []string{name}
				r.Assert = []inventoryAssert{{Path: "plugins/crw/skills/a/SKILL.md", Pattern: "new text", Want: "present"}}
			})
			check(label, map[string]inventoryRow{"collisions[0]": row, "collisions[1]": open(nil)}, ids, registry, false, "collisions[0]: check "+name)
		}
		exists := fixtureRow("done", func(r *inventoryRow) {
			r.Check = []string{"TestKnownOther"}
			r.Assert = []inventoryAssert{{Path: "plugins/crw/skills/a/SKILL.md", Pattern: "new text", Want: "present"}}
		})
		check("exists", map[string]inventoryRow{"collisions[0]": exists, "collisions[1]": open(nil)}, ids, registry, false)
	})
	t.Run("fixture: a registration narrowed to rows proves those rows alone", func(t *testing.T) {
		narrowed := []rowTestRead{{"TestKnown", defaultTestPackage, []string{"plugins/crw/skills/"}, []string{"collisions[1]"}}}
		withPath := fixtureRow("done", func(r *inventoryRow) { r.Paths = []string{"plugins/crw/skills/a/SKILL.md"} })
		check("a row the test does not prove", map[string]inventoryRow{"collisions[0]": withPath, "collisions[1]": open(nil)}, ids, narrowed, false,
			"collisions[0]: a done fix row needs evidence")
		check("a row it proves", map[string]inventoryRow{"collisions[0]": open(nil), "collisions[1]": withPath}, ids, narrowed, false)
		noPaths := fixtureRow("done", nil)
		check("a proven row without paths", map[string]inventoryRow{"collisions[0]": open(nil), "collisions[1]": noPaths}, ids, narrowed, false)
		check("an unproven row without paths", map[string]inventoryRow{"collisions[0]": noPaths, "collisions[1]": open(nil)}, ids, narrowed, false, "needs evidence")
		unknown := []rowTestRead{{"TestKnown", defaultTestPackage, []string{"plugins/crw/skills/"}, []string{"collisions[7]"}}}
		check("a row that is not a literal", map[string]inventoryRow{"collisions[0]": open(nil), "collisions[1]": open(nil)}, ids, unknown, false,
			`the row "collisions[7]" is not a Go string literal of known_test.go`)
	})
	t.Run("fixture: a registered path must be a string literal of the test's file", func(t *testing.T) {
		notRead := []rowTestRead{{"TestKnown", defaultTestPackage, []string{"plugins/crw/skills/", "docs/never-read/"}, nil}}
		row := fixtureRow("done", func(r *inventoryRow) { r.Paths = []string{"docs/never-read/x.md"} })
		check("literal missing", map[string]inventoryRow{"collisions[0]": row, "collisions[1]": open(nil)}, ids, notRead, false,
			`the read path "docs/never-read/" is not a Go string literal of known_test.go`)
		missingTest := []rowTestRead{{"TestNoSuchTest", defaultTestPackage, []string{"plugins/crw/skills/"}, nil}}
		check("registered test missing", map[string]inventoryRow{"collisions[0]": open(nil), "collisions[1]": open(nil)}, ids, missingTest, false,
			"rowTestReads TestNoSuchTest: no top-level func")
		check("registered twice", map[string]inventoryRow{"collisions[0]": open(nil), "collisions[1]": open(nil)}, ids, append(slicesClone(registry), registry...), false, "registered twice")
	})
	t.Run("fixture: a path not written plainly is refused and proves nothing", func(t *testing.T) {
		for _, path := range []string{"plugins/crw/skills/../../../README.md", "plugins/crw/skills/./a/", "plugins//crw/skills/a/", "./plugins/crw/skills/a/"} {
			row := fixtureRow("done", func(r *inventoryRow) { r.Paths = []string{path} })
			check(path, map[string]inventoryRow{"collisions[0]": row, "collisions[1]": open(nil)}, ids, registry, false,
				"collisions[0]: path "+strconv.Quote(path), "collisions[0]: a done fix row needs evidence")
			asserted := fixtureRow("done", func(r *inventoryRow) {
				r.Assert = []inventoryAssert{{Path: path, Pattern: "text", Want: "present"}}
			})
			check(path+" assert", map[string]inventoryRow{"collisions[0]": asserted, "collisions[1]": open(nil)}, ids, registry, false,
				"collisions[0]: assert 1: path "+strconv.Quote(path))
		}
		if covers("plugins/crw/skills/", "plugins/crw/skills/../../../README.md") {
			t.Error("a path that leaves the registered directory through .. is covered by it")
		}
	})
	t.Run("fixture: the completion mode wants no open row", func(t *testing.T) {
		done := fixtureRow("done", func(r *inventoryRow) {
			r.Assert = []inventoryAssert{{Path: "plugins/crw/skills/a/SKILL.md", Pattern: "new text", Want: "present"}}
		})
		check("format mode", map[string]inventoryRow{"collisions[0]": done, "collisions[1]": open(nil)}, ids, registry, false)
		check("completion mode", map[string]inventoryRow{"collisions[0]": done, "collisions[1]": open(nil)}, ids, registry, true, "collisions[1]: is still open")
	})
}

func slicesClone[T any](items []T) []T { return append([]T(nil), items...) }
