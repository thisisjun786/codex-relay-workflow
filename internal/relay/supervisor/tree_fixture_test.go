package supervisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Tree fixtures. Many of this package's tests start from files the Python reference
// implementation's test fixtures left, which Go cannot make: a store a fixture built, snapshots of
// it taken mid-test, the operations a capture driver listed. They are fixtures under
// testdata/fixtures/trees, one file per test holding each tree under the key the test names, and
// treeFixture restores one: its directories, files and links, and each SQLite store rebuilt from
// its rows, table by table, on the schema Go creates. A tree names the directory it is restored in
// as <root> and the repository root as <repo>. What the Python run answered is not in the
// fixtures: the tests compare Go's answers with goldens (internal/testsupport/golden).

// treeFixtureFile is the stored form of a test's tree fixture.
type treeFixtureFile struct {
	Note  string                     `json:"note"`
	Trees map[string]json.RawMessage `json:"trees"`
}

var treeFixtureFiles sync.Map

// treeFixtureText is the tree the calling test's fixture holds under key, as stored.
func treeFixtureText(t testing.TB, key string) []byte {
	t.Helper()
	name := "trees/" + testFileName(t.Name()) + ".json"
	trees, ok := treeFixtureFiles.Load(name)
	if !ok {
		var file treeFixtureFile
		if err := json.Unmarshal(golden.Fixture(t, name), &file); err != nil {
			t.Fatalf("tree fixture %s: %v", name, err)
		}
		trees, _ = treeFixtureFiles.LoadOrStore(name, file.Trees)
	}
	raw, ok := trees.(map[string]json.RawMessage)[key]
	if !ok {
		t.Fatalf("tree fixture %s holds no tree %q", name, key)
	}
	return append([]byte(nil), raw...)
}

// treeFixture empties root and restores in it the tree the calling test's fixture holds under
// key. Each placeholder pair names a value the tree holds as placeholder[1], put back as
// placeholder[0]; <root> and <repo> are always put back.
func treeFixture(t testing.TB, key, root string, placeholders ...[2]string) {
	t.Helper()
	restoreTreeFixture(t, key, root, false, placeholders)
}

func restoreTreeFixture(t testing.TB, key, root string, revisions bool, placeholders [][2]string) []golden.Option {
	t.Helper()
	text := treeFixtureText(t, key)
	text = bytes.ReplaceAll(text, []byte("<repo>"), []byte(repoRoot(t)))
	for i := len(placeholders) - 1; i >= 0; i-- {
		text = bytes.ReplaceAll(text, []byte(placeholders[i][1]), []byte(placeholders[i][0]))
	}
	text = bytes.ReplaceAll(text, []byte("<root>"), []byte(root))
	var options []golden.Option
	if revisions {
		var hashes map[string]string
		var err error
		if text, hashes, err = derivedRevisions(text); err != nil {
			t.Fatalf("tree fixture %q: %v", key, err)
		}
		events := make([]string, 0, len(hashes))
		for event := range hashes {
			events = append(events, event)
		}
		sort.Strings(events)
		for _, event := range events {
			options = append(options, golden.Substitute(hashes[event], "<revision of "+event+">"))
		}
		for _, event := range events {
			options = append(options, golden.Substitute(hashes[event][:12], "<revision of "+event+"/12>"))
		}
	}
	var record treeRecord
	if err := json.Unmarshal(text, &record); err != nil {
		t.Fatalf("tree fixture %q: %v", key, err)
	}
	if err := writeTree(t, root, record); err != nil {
		t.Fatalf("tree fixture %q: %v", key, err)
	}
	return options
}

// derivedRevisions puts back each revision placeholder a tree holds: the hash of the manifest the
// event's receipt carries, which names the files where this run's tree has them. It returns the
// hash of each event.
func derivedRevisions(text []byte) ([]byte, map[string]string, error) {
	var record treeRecord
	if err := json.Unmarshal(text, &record); err != nil {
		return nil, nil, err
	}
	hashes := map[string]string{}
	err := receiptRevisions(record, func(event, claimed, derived string) {
		if claimed == "<revision of "+event+">" {
			hashes[event] = derived
		}
	})
	for event, hash := range hashes {
		text = bytes.ReplaceAll(text, []byte("<revision of "+event+">"), []byte(hash))
		text = bytes.ReplaceAll(text, []byte("<revision of "+event+"/12>"), []byte(hash[:12]))
	}
	return text, hashes, err
}

// testFileName is the file name a test's golden, recording or tree fixture takes from the test's
// name, without its extension (as internal/testsupport/golden names it).
func testFileName(name string) string {
	var b strings.Builder
	plain := len(name) <= 120 && !strings.Contains(name, "__")
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '/':
			b.WriteString("__")
			plain = false
		default:
			b.WriteByte('_')
			plain = false
		}
	}
	base := b.String()
	if !plain {
		sum := sha256.Sum256([]byte(name))
		if len(base) > 100 {
			base = base[:100]
		}
		base += "-" + hex.EncodeToString(sum[:])[:12]
	}
	return base
}

// goldenKeys numbers the goldens a test checks under the same name, for a test that checks one
// in a fixed sequence.
var goldenKeys sync.Map

// goldenKey is a key unique within the running test: what, numbered by the order it is asked in.
func goldenKey(t testing.TB, what string) string {
	counter, _ := goldenKeys.LoadOrStore(t, new(int))
	n := counter.(*int)
	*n++
	return fmt.Sprintf("%d %s", *n, what)
}

// treeGolden is the golden options for a value that names a restored tree's directory or the
// repository root.
func treeGolden(t testing.TB, root string) []golden.Option {
	t.Helper()
	return []golden.Option{golden.Substitute(root, "<root>"), golden.Substitute(repoRoot(t), "<repo>")}
}

// asJSON is value as JSON decodes it: objects as maps, numbers as float64.
func asJSON(t testing.TB, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// The tree fixture helpers, for this package's external tests (package supervisor_test).
var (
	TreeFixture = treeFixture
	TreeGolden  = treeGolden
)
