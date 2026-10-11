package risktier

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// protectedTestdata are existing testdata files that a test replays as expected answers of a security
// reader (the dispatch ledger's symlink and workspace containment refusals, the C>D receipt session,
// phase and epoch guards) or runs as commands (the session oracle's scenarios execute world, setup and
// script strings with sh -ec). Their names carry no reserved word and their extension is a data
// extension, so only a testdata tree that is not on the low list keeps them high (CRW-1041 correction
// round 3, defects d1 and d2).
var protectedTestdata = []string{
	"internal/role/testdata/dispatch/safety.json",
	"internal/pabcd/cli/testdata/boundary.json",
	"internal/pabcd/source/session/testdata/scenarios.json",
}

func TestProtectedTestdataIsHighAtEveryLevel(t *testing.T) {
	root := repoRoot(t)
	for _, p := range protectedTestdata {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			t.Fatalf("%s is gone: update this list: %v", p, err)
		}
		if v := Classify(p); v.Tier != High {
			t.Errorf("Classify(%q) = %+v, want high", p, v)
		}
		for _, r := range []Region{{Path: p, Kind: "file"}, {Path: p, Kind: "symbol", Key: "case"}, {Path: path.Dir(p), Kind: "tree"}} {
			if v := ClassifyRegion(r); v.Tier != High {
				t.Errorf("region %+v = %+v, want high", r, v)
			}
		}
		// the whole decision, with and without the author's low declaration and a matching region
		in := input(edit(p))
		if rec := must(t, in); rec.ChangedTier != High || rec.Tier != High || rec.ExecutionTier != High || rec.Exception != nil {
			t.Errorf("a diff of only %s: changed %s, tier %s, execution %s", p, rec.ChangedTier, rec.Tier, rec.ExecutionTier)
		}
		in.Declared = Low
		in.Regions = []Region{{Path: p, Kind: "file"}, {Path: path.Dir(p), Kind: "tree"}}
		rec := must(t, in)
		if rec.RegionTier == nil || *rec.RegionTier != High || rec.Tier != High || rec.ExecutionTier != High {
			t.Errorf("a declared low diff of only %s inside its own regions: %+v", p, rec)
		}
		// a stored record that says low for these paths is not the record Decide gives
		if _, err := Decode(encode(t, rec)); err != nil {
			t.Errorf("the decided record of %s does not decode: %v", p, err)
		}
	}
}

// lowTestdataFiles is the expected set, written out by hand and independent of the classifier: the only
// testdata files in the repository that are plain data consumed by tests that decide nothing about a
// gate, a reader or a command. Every other testdata file is high.
var lowTestdataFiles = []string{
	"internal/skill/search/testdata/boundaries.json",
	"internal/skill/search/testdata/catalogs.json",
	"internal/skill/search/testdata/hermes-lines.json",
	"internal/skill/search/testdata/ranking.json",
}

// The listed trees stay plain data: no test of the package that owns one runs a command, and no
// non-test file of that package reads the tree (a production reader of a data file makes the file a
// behaviour input, not test data).
func TestLowTestdataTreesAreOnlyFedToPureTests(t *testing.T) {
	root := repoRoot(t)
	for _, entry := range lowTestdata {
		dir := path.Dir(entry)
		if i := strings.Index(entry, "/testdata"); i >= 0 {
			dir = entry[:i]
		}
		files, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(dir), "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("package %s of %s has no Go files (%v): update the list", dir, entry, err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			src := string(b)
			if strings.HasSuffix(f, "_test.go") {
				if strings.Contains(src, `"os/exec"`) || strings.Contains(src, "exec.Command") {
					t.Errorf("%s runs a command, so %s is not plain test data", f, entry)
				}
			} else if strings.Contains(src, "testdata") {
				t.Errorf("%s reads testdata in production code, so %s is a behaviour input", f, entry)
			}
		}
	}
}

// Every testdata file of the repository is high unless it is on the low list: the file name and the
// extension say nothing about whether a test replays the file against a reader or runs it.
func TestEveryTestdataFileIsHighUnlessListedLow(t *testing.T) {
	root := repoRoot(t)
	want := map[string]bool{}
	for _, p := range lowTestdataFiles {
		want[p] = true
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			t.Fatalf("%s is gone: update this list and the classifier's: %v", p, err)
		}
	}
	var lowSeen []string
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !strings.Contains("/"+rel, "/testdata/") {
			return nil
		}
		n++
		v := Classify(rel)
		if v.Tier == Low {
			lowSeen = append(lowSeen, rel)
		}
		if (v.Tier == Low) != want[rel] {
			t.Errorf("Classify(%q) = %+v, on the expected low list: %v", rel, v, want[rel])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lowSeen)
	if n < 100 {
		t.Fatalf("only %d testdata files found under %s: the walk is wrong", n, root)
	}
	if len(lowSeen) != len(lowTestdataFiles) {
		t.Errorf("low testdata files = %v, want %v", lowSeen, lowTestdataFiles)
	}
}

// A testdata tree that is not on the low list is high as a region, and a new testdata tree is high before
// anyone lists it.
func TestUnlistedTestdataTreeIsHigh(t *testing.T) {
	for _, p := range []string{"internal/role/testdata", "internal/role/testdata/dispatch", "internal/new/testdata", "internal/new/testdata/a.json",
		"internal/skill/testdata", "internal/skill/search/testdata/cli-oracle.json", "internal/skill/search/testdata/golden/a.json"} {
		if v := Classify(p); v.Tier != High {
			t.Errorf("Classify(%q) = %+v, want high", p, v)
		}
	}
	for _, p := range []string{"internal/role/testdata", "internal/new/testdata", "internal/skill/testdata", "internal/skill", "internal/skill/search"} {
		if v := ClassifyRegion(Region{Path: p, Kind: "tree"}); v.Tier != High {
			t.Errorf("tree region %q = %+v, want high", p, v)
		}
	}
	if v := ClassifyRegion(Region{Path: "internal/skill/search/testdata", Kind: "tree"}); v.Tier != Low {
		t.Errorf("the listed tree is %+v, want low", v)
	}
}
