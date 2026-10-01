package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The answers these tests expect are goldens (internal/testsupport/golden): a test checks the
// part of an answer it compares - after the masks the comparison applies - under a key of its
// own. The goldens began as the answers the Python reference implementation gave (todo 44)
// and are regenerated from this build with CRW_GOLDEN=update. What the Python relay built for
// a test to start from (a populated store, a directory tree, a sweep's cases) is a fixture
// under testdata/fixtures, which no run rewrites.

var goldenCalls sync.Map // test name + label -> *atomic.Int64

// goldenKey names the next value a test checks against its golden: a readable label (never a
// temporary path or a generated identity) and, for a label the test checks again, how many
// times it checked before. Values with different labels may be checked in any order.
func goldenKey(t testing.TB, label string) string {
	counter, _ := goldenCalls.LoadOrStore(t.Name()+"\x00"+label, new(atomic.Int64))
	if n := counter.(*atomic.Int64).Add(1); n > 1 {
		return fmt.Sprintf("%s #%d", label, n)
	}
	return label
}

// goldenOptions stores every run-specific directory and identity the anchors name as a
// placeholder, and puts the run's own back when the golden is read: the test's temporary homes
// (tempHome) as <home>, <home2>, ..., the scenario directory as <scenarios>, then every other
// directory and identity as placeholderPairs names it.
func goldenOptions(t testing.TB, anchors ...string) []golden.Option {
	var options []golden.Option
	for name := t.Name(); ; {
		if homes, ok := testHomes.Load(name); ok {
			for i, home := range homes.([]string) {
				placeholder := "<home>"
				if i > 0 {
					placeholder = fmt.Sprintf("<home%d>", i+1)
				}
				options = append(options, golden.Substitute(home, placeholder))
			}
			break
		}
		parent, _, found := strings.Cut(name, "/")
		if !found {
			break
		}
		name = parent
	}
	if scenarioSetDir != "" {
		options = append(options, golden.Substitute(scenarioSetDir, "<scenarios>"))
	}
	for _, pair := range placeholderPairs(anchors...) {
		options = append(options, golden.Substitute(pair[0], pair[1]))
	}
	return options
}

// testHomes are the temporary homes each test made (tempHome), in the order it made them.
var testHomes sync.Map // test name -> []string

// tempHome is fixedHome in a temporary directory, for a test whose goldens name the home's
// paths but hash none of them: they spell it <home> (a second one <home2>).
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	homes, _ := testHomes.Load(t.Name())
	list, _ := homes.([]string)
	testHomes.Store(t.Name(), append(list, home))
	t.Cleanup(func() { testHomes.Delete(t.Name()) })
	return isolateHome(t, home)
}

// expectJSON checks value, the part of an answer a test compares, against the golden stored
// under key, with the run's directories and identities the anchors name as placeholders.
func expectJSON(t testing.TB, key string, value any, anchors ...string) {
	t.Helper()
	options := goldenOptions(t, anchors...)
	inPackageDirectory(t, func() { golden.CheckJSON(t, key, value, options...) })
}

// inPackageDirectory runs check, a golden check, in the package directory, for a test that
// changed its working directory (t.Chdir): a golden file is read and written relative to the
// working directory, and an update writes it when the test ends, so the directory is the
// package's then too.
func inPackageDirectory(t testing.TB, check func()) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if wd == packageDir {
		check()
		return
	}
	if golden.Updating() {
		// Runs after the golden file is written (see below).
		t.Cleanup(func() { _ = os.Chdir(wd) })
	}
	if err = os.Chdir(packageDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatal(err)
		}
	}()
	check()
	if golden.Updating() {
		// The golden file is written when the test ends, relative to the working directory then:
		// this cleanup runs before that write, and the one above after it.
		t.Cleanup(func() { _ = os.Chdir(packageDir) })
	}
}

// expectRun checks an answer's exit status and the text of it the test compares (already
// masked as the comparison masks it) against the golden under key.
func expectRun(t testing.TB, key string, code int, stdout string, anchors ...string) {
	t.Helper()
	expectJSON(t, key, map[string]any{"code": code, "stdout": stdout}, anchors...)
}

// expectRunErr is expectRun for a comparison that reads stderr too.
func expectRunErr(t testing.TB, key string, code int, stdout, stderr string, anchors ...string) {
	t.Helper()
	expectJSON(t, key, map[string]any{"code": code, "stdout": stdout, "stderr": stderr}, anchors...)
}

// fixtureJSON decodes the named fixture (testdata/fixtures/<name>, or <name>.gz) into out, with
// each placeholder it holds replaced by the run's value (placeholder -> actual pairs, applied
// in order).
func fixtureJSON(t testing.TB, name string, out any, replacements ...[2]string) {
	t.Helper()
	raw := golden.Fixture(t, name)
	for _, r := range replacements {
		raw = bytes.ReplaceAll(raw, []byte(r[0]), []byte(r[1]))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
}
