package routing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// checkout is the repository root, which fixtures and goldens name <repo>.
func checkout() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

// runPaths are the run-specific paths a scenario names, most specific first: extra (such as a
// state directory) as given, then the checkout as <repo> and TMPDIR as <tmpdir>. The Python
// scenarios' temporary directories are named <tmpdir>/python-temporary-N in the fixtures.
func runPaths(extra ...[2]string) [][2]string {
	return append(extra, [2]string{checkout(), "<repo>"}, [2]string{os.Getenv("TMPDIR"), "<tmpdir>"})
}

// scenarioInputs reads the named fixture, the calls a Python scenario made, with its paths put back
// for this run and decoded into out (numbers as json.Number).
func scenarioInputs(t *testing.T, name string, out any, extra ...[2]string) {
	t.Helper()
	data := golden.Fixture(t, name)
	paths := runPaths(extra...)
	for i := len(paths) - 1; i >= 0; i-- {
		if paths[i][0] != "" {
			data = bytes.ReplaceAll(data, []byte(paths[i][1]), []byte(paths[i][0]))
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
}

// goldenPaths keeps the run's paths out of a golden (see runPaths).
func goldenPaths(extra ...[2]string) []golden.Option {
	var opts []golden.Option
	for _, path := range runPaths(extra...) {
		opts = append(opts, golden.Substitute(path[0], path[1]))
	}
	return opts
}

// tableTrail checks the whole-table dumps a test takes after its steps. The golden holds each dump
// as the rows that changed since the test's previous dump (tableDelta): a test stops at its first
// differing dump, so the previous dump is the expected one and the delta determines the whole dump,
// while gzip could not fold the same rows repeated in dumps far apart.
type tableTrail struct {
	previous map[string][]json.RawMessage
}

// check compares tables, a dump as tablesJSON writes it, with the golden under key.
func (trail *tableTrail) check(t *testing.T, key, tables string, opts ...golden.Option) {
	t.Helper()
	current, err := splitTables(tables)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	golden.Check(t, key, tableDelta(trail.previous, current), opts...)
	trail.previous = current
}

func splitTables(tables string) (map[string][]json.RawMessage, error) {
	var split map[string][]json.RawMessage
	if err := json.Unmarshal([]byte(tables), &split); err != nil {
		return nil, fmt.Errorf("table dump: %v", err)
	}
	return split, nil
}

// tableDelta writes current as its difference from previous, one line per changed item: "<table>
// rows <n>" for each table whose rows differ, followed by "<index> <row>" for each row that is new
// or changed at that index, and "<table> gone" for each table current no longer has.
func tableDelta(previous, current map[string][]json.RawMessage) []byte {
	names := map[string]bool{}
	for name := range previous {
		names[name] = true
	}
	for name := range current {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var out bytes.Buffer
	for _, name := range sorted {
		before, had := previous[name]
		after, has := current[name]
		if !has {
			fmt.Fprintf(&out, "%s gone\n", name)
			continue
		}
		var changed []int
		for i, row := range after {
			if i >= len(before) || !bytes.Equal(before[i], row) {
				changed = append(changed, i)
			}
		}
		if had && len(before) == len(after) && len(changed) == 0 {
			continue
		}
		fmt.Fprintf(&out, "%s rows %d\n", name, len(after))
		for _, i := range changed {
			fmt.Fprintf(&out, "%d %s\n", i, after[i])
		}
	}
	return out.Bytes()
}
