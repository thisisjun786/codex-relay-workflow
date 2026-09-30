package faults

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Expected outputs.
//
// These tests compare Go's answers with goldens (internal/testsupport/golden) under
// testdata/golden, one file per top-level test. The goldens began as the answers the Python
// reference implementation gave for the same inputs; CRW_GOLDEN=update rewrites them from Go.
// A golden keeps run-specific paths as placeholders (runPaths) and the digest of evidence that
// names such a path as the occurrence it belongs to (evidencePlaceholders).

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
	decoded, err := pyDecode(normalized)
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

// evidencePlaceholders is an answer with each evidence digest its occurrence row's evidence
// yields replaced by that occurrence's placeholder, wherever the answer names it: evidence a
// sweep derives names the installation and the host record by path, so its digest changes from
// run to run as those paths do, which a path placeholder cannot follow. The digest compared is
// faults.evidence_digest: SHA-256 over the stored evidence text with its whitespace removed.
func evidencePlaceholders(raw []byte) ([]byte, error) {
	return recordEvidenceDigests(raw)
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
