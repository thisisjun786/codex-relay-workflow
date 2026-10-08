//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"strings"
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
func caseIsADivergence(target Target, c Case) (problem string) {
	input, err := decode(c.Input)
	if err != nil {
		return "the input is not JSON: " + err.Error()
	}
	root, err := os.MkdirTemp("", "cxcfuzz-pin-")
	if err != nil {
		return err.Error()
	}
	defer func() { problem = joinRemoval(problem, root) }()
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

// TestPinnedOracleAnswersMatchTheOracle replays every pinned case through the real oracle worker and
// checks its answer still equals the case's stored oracle field. TestPinnedDivergencesAreRealDivergences
// compares the Go side to that stored field, so a corrupted or hand-edited oracle field would leave a
// case that pins the wrong thing while still looking like a divergence; this closes that gap. It needs
// Node, so it skips on a host without it, exactly as the campaign tests do. Each target is its own
// subtest so the state and goalplan parts skip on a host without the oracle tree while the pyjson
// part, which imports no oracle module, still runs.
func TestPinnedOracleAnswersMatchTheOracle(t *testing.T) {
	requireNode(t)
	for _, name := range shimTargets() {
		t.Run(name, func(t *testing.T) {
			requireOracleModule(t, name)
			requireOracleCommands(t, name)
			target, ok := Lookup(name)
			if !ok {
				t.Fatalf("%s is not registered", name)
			}
			cases, err := LoadCases(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			pool, err := NewPool(target.Oracle, 1, DefaultTimeout, DefaultStartupTimeout, os.Environ())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pool.Close() }()
			for _, c := range cases {
				if problem := caseOracleMatches(target, pool, c); problem != "" {
					t.Errorf("%s/%s: %s", name, c.Name, problem)
				}
			}
		})
	}
}

// caseOracleMatches replays one case through the real oracle worker and reports whether its answer
// still equals the case's stored oracle field. It builds the case root and calls the pool the way a
// campaign does, and removes the root however it returns, so a failure inside it leaks nothing.
func caseOracleMatches(target Target, pool *Pool, c Case) (problem string) {
	input, err := decode(c.Input)
	if err != nil {
		return "the input is not JSON: " + err.Error()
	}
	root, err := os.MkdirTemp("", "cxcfuzz-oracle-pin-")
	if err != nil {
		return err.Error()
	}
	defer func() { problem = joinRemoval(problem, root) }()
	if err := PrepareRoot(root); err != nil {
		return "the case root was not prepared: " + err.Error()
	}
	if _, err := Scenarios(root, input); err != nil {
		return "the scenario is refused: " + err.Error()
	}
	answer, err := pool.Call(canonical(input), root)
	if err != nil {
		return "the oracle did not answer: " + err.Error()
	}
	value, err := decode(answer)
	if err != nil {
		return "the oracle answer is not JSON: " + err.Error()
	}
	live := stripRoot(value, root)
	if got := canonical(live); got != canonicalText(c.Oracle) {
		if recursionPinMatches(live, c.Oracle) {
			return ""
		}
		return "the oracle answers " + got + ", pinned " + canonicalText(c.Oracle)
	}
	return ""
}

// recursionPinMatches reports whether a live oracle answer matches a pinned one whose stderr is a
// RecursionError traceback. The traceback names the interpreter's own install path, source line
// numbers and repeat counts, which differ between Python builds, so such a pin compares the exit
// status and stdout exactly and requires the live stderr to name the same error (CRW-708 generation 5,
// c10 d4). Any other pinned answer is compared whole, as before.
func recursionPinMatches(live any, pinned string) bool {
	stored, err := decode(pinned)
	if err != nil {
		return false
	}
	storedErr, _ := field(stored, "stderr")
	if text, _ := storedErr.(string); !strings.Contains(text, "RecursionError") {
		return false
	}
	liveErr, _ := field(live, "stderr")
	if text, _ := liveErr.(string); !strings.Contains(text, "RecursionError") {
		return false
	}
	for _, key := range []string{"exit", "stdout"} {
		a, _ := field(live, key)
		b, _ := field(stored, key)
		if canonical(a) != canonical(b) {
			return false
		}
	}
	return true
}

// joinRemoval reports a pin's case root that could not be removed. A removal failure is never dropped: it
// stays on the problem already found, or stands alone when the pin had none (CRW-978 c3b).
func joinRemoval(problem, root string) string {
	err := os.RemoveAll(root)
	if err == nil {
		return problem
	}
	if problem == "" {
		return "the case root " + root + " was not removed: " + err.Error()
	}
	return problem + "; the case root " + root + " was not removed: " + err.Error()
}
