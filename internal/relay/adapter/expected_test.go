package adapter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The values this package's tests expect are goldens (internal/testsupport/golden) under
// testdata/golden: CRW_GOLDEN=update rewrites them from what the Go side answers. They began as
// the answers the Python reference implementation gave, which the tests compared Go's with.

var goldenTests struct {
	sync.Mutex
	shared   map[string]testing.TB
	parents  map[testing.TB]string
	counters map[testing.TB]map[string]int
}

// shareGoldens keeps the goldens of t's subtests in t's own golden file, each key led by the
// subtest's name, instead of in one file per subtest.
func shareGoldens(t *testing.T) {
	t.Helper()
	goldenTests.Lock()
	defer goldenTests.Unlock()
	if goldenTests.shared == nil {
		goldenTests.shared = map[string]testing.TB{}
	}
	goldenTests.shared[t.Name()] = t
	t.Cleanup(func() {
		goldenTests.Lock()
		defer goldenTests.Unlock()
		delete(goldenTests.shared, t.Name())
	})
}

// sharedGolden is a subtest seen by the golden package as the test that shares its file: the
// file is named after owner and saved when owner ends; everything else is the subtest's.
type sharedGolden struct {
	testing.TB
	owner testing.TB
}

func (s sharedGolden) Name() string     { return s.owner.Name() }
func (s sharedGolden) Cleanup(f func()) { s.owner.Cleanup(f) }

// goldenFor is the test the golden package files t's goldens under, and the prefix t's keys take
// in that file.
func goldenFor(t testing.TB) (testing.TB, string) {
	goldenTests.Lock()
	defer goldenTests.Unlock()
	name := t.Name()
	for owner, shared := range goldenTests.shared {
		if rest, ok := strings.CutPrefix(name, owner+"/"); ok {
			return sharedGolden{TB: t, owner: shared}, rest + ": "
		}
	}
	return t, ""
}

// goldenKey names the next value the calling test checks under name: name and how many times
// the test checked one under it before, since a test checks its values in a fixed order.
func goldenKey(t testing.TB, name string) string {
	t.Helper()
	goldenTests.Lock()
	defer goldenTests.Unlock()
	if goldenTests.counters == nil {
		goldenTests.counters = map[testing.TB]map[string]int{}
	}
	asked, ok := goldenTests.counters[t]
	if !ok {
		asked = map[string]int{}
		goldenTests.counters[t] = asked
		t.Cleanup(func() {
			goldenTests.Lock()
			defer goldenTests.Unlock()
			delete(goldenTests.counters, t)
		})
	}
	asked[name]++
	return fmt.Sprintf("%s #%d", name, asked[name])
}

// testTempParent is the directory every t.TempDir of the calling test lies in: the one path
// each of the test's own paths starts with, on every run.
func testTempParent(t testing.TB) string {
	t.Helper()
	goldenTests.Lock()
	defer goldenTests.Unlock()
	if parent, ok := goldenTests.parents[t]; ok {
		return parent
	}
	if goldenTests.parents == nil {
		goldenTests.parents = map[testing.TB]string{}
	}
	parent := filepath.Dir(t.TempDir())
	goldenTests.parents[t] = parent
	t.Cleanup(func() {
		goldenTests.Lock()
		defer goldenTests.Unlock()
		delete(goldenTests.parents, t)
	})
	return parent
}

// repoRoot is the repository root.
func repoRoot(t testing.TB) string {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// goldenOptions stores the run's own paths as placeholders: the caller's first (a longer path
// before any prefix of it), then the test's temporary directories, the suite directory (TMPDIR,
// the fake hosts' sockets and the built binaries) and the repository.
func goldenOptions(t testing.TB, extra ...golden.Option) []golden.Option {
	t.Helper()
	options := append([]golden.Option{}, extra...)
	for _, path := range []struct{ actual, placeholder string }{{testTempParent(t), "<test-tmp>"}, {suiteDirectory, "<suite>"}, {repoRoot(t), "<repo>"}} {
		options = append(options, golden.Substitute(path.actual, path.placeholder))
		if real, err := filepath.EvalSymlinks(path.actual); err == nil && real != path.actual {
			options = append(options, golden.Substitute(real, path.placeholder))
		}
	}
	return options
}

// expectJSON checks got, as CheckJSON encodes it, with the golden under the next key named name.
func expectJSON(t testing.TB, name string, got any, extra ...golden.Option) {
	t.Helper()
	owner, prefix := goldenFor(t)
	golden.CheckJSON(owner, prefix+goldenKey(t, name), got, goldenOptions(t, extra...)...)
}

// expectBytes checks got with the golden under the next key named name.
func expectBytes(t testing.TB, name string, got []byte, extra ...golden.Option) {
	t.Helper()
	owner, prefix := goldenFor(t)
	golden.Check(owner, prefix+goldenKey(t, name), got, goldenOptions(t, extra...)...)
}

// wantBytes is the golden under the next key named name, for a test that compares by its own
// means; when updating it is produce().
func wantBytes(t testing.TB, name string, produce func() []byte, extra ...golden.Option) []byte {
	t.Helper()
	owner, prefix := goldenFor(t)
	return golden.Want(owner, prefix+goldenKey(t, name), produce, goldenOptions(t, extra...)...)
}

// processExit is how a process ended: its exit code and what it wrote.
type processExit struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr,omitempty"`
}

// sqliteArgument turns a table dump value decoded with json.Number back into what SQLite stores:
// an integer or a text is plain JSON, a real and a blob are tagged.
func sqliteArgument(value any) (any, error) {
	switch v := value.(type) {
	case nil, string:
		return v, nil
	case json.Number:
		return v.Int64()
	case map[string]any:
		if real, ok := v["real"].(json.Number); ok {
			return real.Float64()
		}
		if blob, ok := v["blob"].(string); ok {
			return base64.StdEncoding.DecodeString(blob)
		}
	}
	return nil, fmt.Errorf("not a table dump value: %#v", value)
}

// withoutWallClock is value as JSON reads it back, with every startedAt and updatedAt number in
// it, which a ledger stamps from the wall clock, as <wall-clock>.
func withoutWallClock(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var answer any
	if err := decodeNumbers(raw, &answer); err != nil {
		return nil, err
	}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				if _, ok := item.(json.Number); ok && (key == "startedAt" || key == "updatedAt") {
					v[key] = "<wall-clock>"
					continue
				}
				walk(item)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(answer)
	return answer, nil
}

// decodeNumbers decodes raw keeping numbers as json.Number.
func decodeNumbers(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(out)
}

// quoted is a SQL identifier.
func quoted(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
