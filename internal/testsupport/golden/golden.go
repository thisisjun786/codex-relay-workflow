// Package golden keeps the expected outputs of the Go tests as files the tests can regenerate.
//
// A golden is one gzip-compressed JSON file per test under the calling package's testdata/golden
// directory, holding each expected value the test checks under a key the test names. By default
// Check compares the value the test produced with the stored one and fails with the first
// difference. With CRW_GOLDEN=update it stores the produced value instead, so an intended change of
// output is one run with the variable set followed by a review of the goldens' diff:
//
//	CRW_GOLDEN=update go test ./internal/relay/cli/
//	git diff --stat internal/relay/cli/testdata/golden
//
// Run an update over whole packages: a golden file holds the keys its test asked for in the run
// that wrote it, and a key a test no longer asks for leaves the file with it.
//
// Fixtures are the other half. A fixture is an input a test starts from (a store, a directory
// tree, a transcript) that the test cannot produce itself; it lives under testdata/fixtures,
// named by the test author, and is read with Fixture. No mode rewrites a fixture.
//
// The goldens began as the answers the Python reference implementation gave (recorded through
// the former internal/testsupport/pyoracle); after todo 44 they are simply what the Go product
// is expected to answer.
package golden

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// ModeEnv names the variable that selects the mode: unset compares, "update" rewrites.
const ModeEnv = "CRW_GOLDEN"

// Directory is where a package keeps its goldens, relative to the package directory.
const Directory = "testdata/golden"

// FixtureDirectory is where a package keeps its fixtures, relative to the package directory.
const FixtureDirectory = "testdata/fixtures"

// Updating reports whether this run rewrites goldens.
func Updating() bool {
	switch mode := os.Getenv(ModeEnv); mode {
	case "":
		return false
	case "update":
		return true
	default:
		panic(fmt.Sprintf("golden: %s=%q is not \"update\" or unset", ModeEnv, mode))
	}
}

// Option adjusts one Check.
type Option func(*options)

type options struct {
	substitutions [][2]string
	compare       func(want, got []byte) error
}

// Substitute stores actual as placeholder in the golden and reads the placeholder back as actual.
// Use it for every run-specific string a value carries (a temporary directory, a port, a pid).
// Substitutions apply in the order given, so name a longer path before a prefix of it.
func Substitute(actual, placeholder string) Option {
	return func(o *options) {
		if actual != "" {
			o.substitutions = append(o.substitutions, [2]string{actual, placeholder})
		}
	}
}

// Compare replaces byte equality, for values that carry what a rerun changes (a masked time, an
// order the test does not fix). It returns nil when got matches want.
func Compare(compare func(want, got []byte) error) Option {
	return func(o *options) { o.compare = compare }
}

// Check compares got with the golden stored under key for the calling test, or stores got when
// updating. The key must be unique within the test and the same on every run.
func Check(t testing.TB, key string, got []byte, opts ...Option) {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	file := fileFor(t)
	if Updating() {
		file.put(key, apply(got, o.substitutions, false))
		return
	}
	stored, ok := file.get(key)
	if !ok {
		t.Fatalf("golden: %s has no value for %q; write it with %s=update", file.path, key, ModeEnv)
	}
	want := apply(stored, o.substitutions, true)
	if o.compare != nil {
		if err := o.compare(want, got); err != nil {
			t.Fatalf("golden: %q differs from %s: %v", key, file.path, err)
		}
		return
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden: %q differs from %s\n%s", key, file.path, firstDifference(want, got))
	}
}

// CheckJSON is Check for a value, encoded as indented JSON with sorted object keys.
func CheckJSON(t testing.TB, key string, got any, opts ...Option) {
	t.Helper()
	encoded, err := Encode(got)
	if err != nil {
		t.Fatalf("golden: encode %q: %v", key, err)
	}
	Check(t, key, encoded, opts...)
}

// Want returns the golden stored under key for the calling test, for a test that compares by its
// own means. When updating it stores and returns produce(), so the test compares the value with
// itself; produce is called only then.
func Want(t testing.TB, key string, produce func() []byte, opts ...Option) []byte {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	file := fileFor(t)
	if Updating() {
		got := produce()
		file.put(key, apply(got, o.substitutions, false))
		return got
	}
	stored, ok := file.get(key)
	if !ok {
		t.Fatalf("golden: %s has no value for %q; write it with %s=update", file.path, key, ModeEnv)
	}
	return apply(stored, o.substitutions, true)
}

// Encode is CheckJSON's encoding: two-space indentation, sorted object keys, no HTML escaping,
// numbers as they were given (json.Number stays verbatim), and a final newline.
func Encode(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Fixture reads the named fixture of the calling package: testdata/fixtures/<name>, or <name>.gz
// decompressed.
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	path := filepath.Join(FixtureDirectory, name)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data, err = readGzip(path + ".gz")
	}
	if err != nil {
		t.Fatalf("golden: fixture %s: %v", path, err)
	}
	return data
}

func apply(data []byte, substitutions [][2]string, back bool) []byte {
	if back {
		for i := len(substitutions) - 1; i >= 0; i-- {
			data = bytes.ReplaceAll(data, []byte(substitutions[i][1]), []byte(substitutions[i][0]))
		}
		return data
	}
	for _, s := range substitutions {
		data = bytes.ReplaceAll(data, []byte(s[0]), []byte(s[1]))
	}
	return data
}

// firstDifference shows the first line where want and got part, with a line of context.
func firstDifference(want, got []byte) string {
	wantLines, gotLines := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g || i >= len(wantLines) || i >= len(gotLines) {
			context := ""
			if i > 0 && i-1 < len(wantLines) {
				context = fmt.Sprintf("  line %d: %s\n", i, clip(wantLines[i-1]))
			}
			return fmt.Sprintf("%s  line %d want: %s\n  line %d got:  %s", context, i+1, clip(w), i+1, clip(g))
		}
	}
	return "  (equal lines; the bytes differ in line endings)"
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// file is one test's goldens. Values are held as stored: UTF-8 text under a "txt:" prefix,
// anything else as base64 under "b64:".
type file struct {
	mu     sync.Mutex
	path   string
	values map[string]string
	dirty  bool
}

var (
	filesMu sync.Mutex
	files   = map[string]*file{}
)

func fileFor(t testing.TB) *file {
	t.Helper()
	path := filepath.Join(Directory, fileName(t.Name())+".json.gz")
	filesMu.Lock()
	defer filesMu.Unlock()
	if f, ok := files[path]; ok {
		return f
	}
	f := &file{path: path, values: map[string]string{}}
	if !Updating() {
		if err := f.load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("golden: %v", err)
		}
	} else {
		t.Cleanup(func() {
			if err := f.save(); err != nil {
				t.Errorf("golden: %v", err)
			}
		})
	}
	files[path] = f
	return f
}

// fileName turns a test name into a file name. A name made only of letters, digits, '-', '_' and
// '.' is used as it is; any other name, a subtest's included, keeps a readable prefix (subtest
// separators as "__", other characters as "_") and gains a digest of the whole name, so two names
// never share a file.
func fileName(name string) string {
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

func (f *file) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.values[key]
	if !ok {
		return nil, false
	}
	if rest, found := strings.CutPrefix(stored, "b64:"); found {
		data, err := base64.StdEncoding.DecodeString(rest)
		return data, err == nil
	}
	if rest, found := strings.CutPrefix(stored, "txt:"); found {
		return []byte(rest), true
	}
	return nil, false
}

func (f *file) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if utf8.Valid(data) {
		f.values[key] = "txt:" + string(data)
	} else {
		f.values[key] = "b64:" + base64.StdEncoding.EncodeToString(data)
	}
	f.dirty = true
}

type document struct {
	Note   string            `json:"note"`
	Values map[string]string `json:"values"`
}

const note = "Expected values of this test, written by internal/testsupport/golden (CRW_GOLDEN=update)."

func (f *file) load() error {
	data, err := readGzip(f.path)
	if err != nil {
		return err
	}
	var d document
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("%s: %w", f.path, err)
	}
	if d.Values != nil {
		f.values = d.Values
	}
	return nil
}

func (f *file) save() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirty {
		return nil
	}
	encoded, err := Encode(document{Note: note, Values: f.values})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := writer.Write(encoded); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(f.path, buf.Bytes(), 0o644)
}

func readGzip(path string) ([]byte, error) {
	compressed, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return data, nil
}
