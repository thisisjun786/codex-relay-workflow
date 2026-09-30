package faults

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Expected outputs.
//
// These tests compare Go's answers with goldens (internal/testsupport/golden) under
// testdata/golden, one file per top-level test. The goldens began as the answers the Python
// reference implementation gave for the same inputs (recorded until todo 44 and equal to Go's
// when the goldens were written); CRW_GOLDEN=update rewrites them from Go. A golden keeps
// run-specific paths as placeholders (runPaths) and the digest of evidence that names such a
// path as the occurrence it belongs to (evidencePlaceholders).

// goldenParents holds each running test whose subtests keep their goldens in its own file.
var goldenParents sync.Map

// goldenParent makes t's subtests keep their goldens in t's golden file, each under keys that
// start with its name below t, so a test with many subtests stays one file.
func goldenParent(t *testing.T) {
	goldenParents.Store(t.Name(), t)
	t.Cleanup(func() { goldenParents.Delete(t.Name()) })
}

// goldenSubtest is a subtest seen as its golden parent: the parent's name, so its goldens land
// in the parent's file, and the parent's cleanup, which writes that file after every subtest ran.
type goldenSubtest struct {
	testing.TB
	name   string
	parent *testing.T
}

func (g goldenSubtest) Name() string { return g.name }

func (g goldenSubtest) Cleanup(f func()) { g.parent.Cleanup(f) }

// goldenAt is the test whose golden file t writes, and the prefix t's keys take in it.
func goldenAt(t testing.TB) (testing.TB, string) {
	name := t.Name()
	for top := name; ; {
		i := strings.LastIndex(top, "/")
		if i < 0 {
			return t, ""
		}
		top = top[:i]
		if parent, ok := goldenParents.Load(top); ok {
			return goldenSubtest{TB: t, name: top, parent: parent.(*testing.T)}, name[len(top)+1:] + " "
		}
	}
}

// goldenQuestions counts the goldens each running test has checked.
var goldenQuestions = struct {
	sync.Mutex
	asked map[testing.TB]int
}{asked: map[testing.TB]int{}}

// goldenKey names the next golden t checks: its number in the test's fixed sequence, a label
// for the reader, and a digest of the question's words (a command line, with run paths as
// placeholders), so a question that changes finds no golden rather than another question's.
func goldenKey(t testing.TB, label string, words []string, paths runPaths) string {
	goldenQuestions.Lock()
	n, seen := goldenQuestions.asked[t]
	goldenQuestions.asked[t] = n + 1
	goldenQuestions.Unlock()
	if !seen {
		t.Cleanup(func() {
			goldenQuestions.Lock()
			delete(goldenQuestions.asked, t)
			goldenQuestions.Unlock()
		})
	}
	label = paths.neutral([]string{label})[0]
	if runes := []rune(label); len(runes) > 72 {
		label = string(runes[:72]) + "…"
	}
	digest := sha256.Sum256([]byte(strings.Join(paths.neutral(words), "\x00")))
	return fmt.Sprintf("%03d %s %x", n+1, label, digest[:4])
}

// checkGolden compares value, encoded as golden.CheckJSON encodes it, with the next golden t
// keeps for question.
func checkGolden(t testing.TB, label string, question []string, paths runPaths, value any) {
	t.Helper()
	at, prefix := goldenAt(t)
	golden.CheckJSON(at, prefix+goldenKey(t, label, question, paths), value, paths.options()...)
}

// checkGoldenEvidence is checkGolden for a value holding occurrence rows whose evidence names a
// run path: each evidence digest is compared as its occurrence's placeholder.
func checkGoldenEvidence(t testing.TB, label string, question []string, paths runPaths, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := evidencePlaceholders(raw)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeNumbers(normalized)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, label, question, paths, decoded)
}

// runPaths are the run-specific paths a question and its answer may carry, as pairs of the
// actual path and the placeholder a golden keeps instead. A longer path comes before a prefix
// of it.
type runPaths []string

// runPathsOf are the paths every command of t may name: its disposable home, the directory
// holding t's temporary directories (t.TempDir), the relay's installation location
// (relayPackageLocation) and this checkout.
func runPathsOf(t testing.TB, home string) runPaths {
	return runPaths{home, "<home>", testTemp(t), "<test-tmp>", relayPackageLocation, "<relay-package>", f1Root(), "<repo>"}
}

func (p runPaths) options() []golden.Option {
	var opts []golden.Option
	for i := 0; i+1 < len(p); i += 2 {
		opts = append(opts, golden.Substitute(p[i], p[i+1]))
	}
	return opts
}

func (p runPaths) neutral(words []string) []string {
	var pairs []string
	for i := 0; i+1 < len(p); i += 2 {
		if p[i] != "" {
			pairs = append(pairs, p[i], p[i+1])
		}
	}
	replacer := strings.NewReplacer(pairs...)
	out := make([]string, len(words))
	for i, word := range words {
		out[i] = replacer.Replace(word)
	}
	return out
}

// testTemps holds the directory each running test's t.TempDir directories are made in.
var testTemps = struct {
	sync.Mutex
	dir map[testing.TB]string
}{dir: map[testing.TB]string{}}

// testTemp is the directory t.TempDir makes t's temporary directories in.
func testTemp(t testing.TB) string {
	testTemps.Lock()
	dir, ok := testTemps.dir[t]
	testTemps.Unlock()
	if ok {
		return dir
	}
	dir = filepath.Dir(t.TempDir())
	testTemps.Lock()
	testTemps.dir[t] = dir
	testTemps.Unlock()
	t.Cleanup(func() {
		testTemps.Lock()
		delete(testTemps.dir, t)
		testTemps.Unlock()
	})
	return dir
}

// Evidence digests.
//
// An occurrence's evidence_digest is the SHA-256 of its evidence as stored compactly (sorted
// keys, no ASCII escaping, no whitespace). Evidence a sweep derives names the installation and
// the host record by path, so its digest changes from run to run as those paths do, which a path
// placeholder cannot follow. A golden therefore holds each digest that is its row's evidence's
// as "<evidence-sha256:OCCURRENCE>", and a digest that is not that of its row's evidence as it is.

const evidencePlaceholder = "<evidence-sha256:"

// evidencePlaceholders is an answer with each evidence digest its occurrence row's evidence
// yields replaced by that occurrence's placeholder, wherever the answer names it.
func evidencePlaceholders(raw []byte) ([]byte, error) {
	value, err := decodeNumbers(raw)
	if err != nil {
		return nil, err
	}
	owner := map[string]string{}
	occurrenceEvidence(value, func(row map[string]any, id, evidence string) {
		digest, _ := row["evidence_digest"].(string)
		if computed, ok := storedEvidenceDigest(evidence); ok && digest != "" && computed == digest {
			if current, seen := owner[digest]; !seen || id < current {
				owner[digest] = id
			}
		}
	})
	if len(owner) == 0 {
		return raw, nil
	}
	var pairs []string
	for digest, id := range owner {
		pairs = append(pairs, digest, evidencePlaceholder+id+">")
	}
	replacer := strings.NewReplacer(pairs...)
	return encodeNumbers(mapStrings(value, replacer.Replace))
}

// occurrenceEvidence walks value for occurrence rows, objects with a string occurrence_id and a
// string evidence, and calls visit with each row.
func occurrenceEvidence(value any, visit func(row map[string]any, id, evidence string)) {
	switch v := value.(type) {
	case map[string]any:
		id, idOK := v["occurrence_id"].(string)
		evidence, evidenceOK := v["evidence"].(string)
		if idOK && evidenceOK {
			visit(v, id, evidence)
		}
		for _, item := range v {
			occurrenceEvidence(item, visit)
		}
	case []any:
		for _, item := range v {
			occurrenceEvidence(item, visit)
		}
	}
}

// storedEvidenceDigest is faults.evidence_digest of evidence as a store holds it.
func storedEvidenceDigest(evidence string) (string, bool) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(evidence)); err != nil {
		return "", false
	}
	sum := sha256.Sum256(compact.Bytes())
	return fmt.Sprintf("%x", sum), true
}

// mapStrings is value with f applied to every string in it.
func mapStrings(value any, f func(string) string) any {
	switch v := value.(type) {
	case string:
		return f(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = mapStrings(item, f)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = mapStrings(item, f)
		}
		return out
	}
	return value
}

// decodeNumbers decodes JSON keeping every number as written.
func decodeNumbers(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%v: %s", err, raw)
	}
	return value, nil
}

// encodeNumbers encodes a value compactly: numbers as written, markup unescaped.
func encodeNumbers(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// seedCLI runs a command line on the Go store in dir, as cliCall does, at the fixed time now
// (seconds since the epoch), so the times the later answers echo are the same in every run.
func seedCLI(t *testing.T, now float64, dir string, args ...string) (int, map[string]any) {
	t.Helper()
	ctx := context.WithValue(context.Background(), f1InputsKey{}, f1Inputs{clock: &testClock{now: now}})
	var out, stderr bytes.Buffer
	code, handled := executeAsCLI(ctx, append([]string{"--state", dir}, args...), &out, &stderr)
	if !handled {
		t.Fatalf("not handled: %v", args)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("%s (stderr: %s): %v", out.String(), stderr.String(), err)
	}
	return code, payload
}

// noticeDifference reports the first differing field of a whole-value comparison.
func noticeDifference(path string, want, got any) string {
	if reflect.DeepEqual(want, got) {
		return ""
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			break
		}
		keys := make([]string, 0, len(w))
		for key := range w {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if diff := noticeDifference(path+"."+key, w[key], g[key]); diff != "" {
				return diff
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			break
		}
		if len(w) != len(g) {
			return fmt.Sprintf("%s.length: got %d want %d", path, len(g), len(w))
		}
		for i := range w {
			if diff := noticeDifference(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); diff != "" {
				return diff
			}
		}
	}
	return fmt.Sprintf("%s: got %v want %v", path, got, want)
}

// cliGolden is what a command line answered: its exit status, its stdout, its stderr where a
// test compares it, and, where a test asks, whether its --state directory existed after it ran.
type cliGolden struct {
	Code    int    `json:"code"`
	Stdout  string `json:"stdout"`
	Stderr  string `json:"stderr,omitempty"`
	Created *bool  `json:"created,omitempty"`
}

// created reports whether dir exists, for cliGolden.Created.
func created(t testing.TB, dir string) *bool {
	t.Helper()
	exists := !stateAbsent(t, dir)
	return &exists
}
