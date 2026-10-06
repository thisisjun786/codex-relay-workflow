//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"testing"
)

// A case tagged open or intentionally-changed pins a divergence: the two sides' answers must really
// differ. A stored input that evaluates Same pins nothing, and the class it names goes unpinned while
// the record still points at it (this is how the two lone-surrogate cases were wrong: their input held
// a literal backslash rather than a surrogate escape, so both sides read the same text). The check
// runs the Go side only, against each case's own stored oracle answer, so it needs no Node.
//
// It covers the three targets this issue owns. A sibling issue's target carries its own check, so a
// bogus case there fails that issue's work rather than this one's (this package is shared, but the
// targets and their cases are not).
func TestPinnedDivergencesAreRealDivergences(t *testing.T) {
	for _, name := range shimTargets() {
		target, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		cases, err := LoadCases(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			if c.Tag != TagOpen && c.Tag != TagIntentionallyChanged {
				continue
			}
			if problem := caseIsADivergence(target, c); problem != "" {
				t.Errorf("%s/%s (tag %s): %s", name, c.Name, c.Tag, problem)
			}
		}
	}
}

// caseIsADivergence runs one case's Go side and reports whether it still answers something other than
// the case's stored oracle answer. A case whose Go answer equals the oracle's is not a divergence.
func caseIsADivergence(target Target, c Case) string {
	input, err := decode(c.Input)
	if err != nil {
		return "the input is not JSON: " + err.Error()
	}
	root, err := os.MkdirTemp("", "cxcfuzz-pin-")
	if err != nil {
		return err.Error()
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := PrepareRoot(root); err != nil {
		return "the case root was not prepared: " + err.Error()
	}
	if _, err := Scenarios(root, input); err != nil {
		return "the scenario is refused: " + err.Error()
	}
	value, err := target.Go(input, RootEnv(root))
	if err != nil {
		value = errorValue(err)
	}
	if canonical(stripRoot(value, root)) == canonicalText(c.Oracle) {
		return "the Go answer equals the oracle answer, so the case pins no divergence"
	}
	return ""
}
