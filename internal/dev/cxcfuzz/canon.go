//go:build dev

package cxcfuzz

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// decode reads JSON text as both sides exchange it. Surrogates keeps a lone surrogate escape as
// the three WTF-8 bytes a Go string holds it in, so a value reaches a target's Go function the
// way the oracle's str holds it.
func decode(text string) (any, error) {
	value, err := pyjson.Loads(text, pyjson.LoadOptions{Python: true, Constants: true, Surrogates: true, Deep: true})
	if err != nil {
		return nil, fmt.Errorf("reading the JSON value: %w", err)
	}
	return value, nil
}

// canonical rewrites a value in the one form both sides are compared in: object keys sorted, a
// lone surrogate as its \udXXX escape. Two answers that agree in this form are the same answer.
func canonical(value any) string {
	return pyjson.Dumps(value, pyjson.Options{SortKeys: true, Bytes: pyjson.SurrogateEscapes})
}

// compareJSON is the comparison most targets use: both answers canonicalised, compared as bytes.
func compareJSON(goOut, oracleOut any) Verdict {
	if canonical(goOut) == canonical(oracleOut) {
		return Verdict{Kind: Same}
	}
	return Verdict{Kind: Differ, Detail: "the canonical outputs differ"}
}

// errorValue is the Go side's failure as an answer, in the shape the worker's error reply takes,
// so a target's Compare sees both sides' failures the same way.
func errorValue(err error) any {
	return pyjson.Object{{Key: "error", Value: pyjson.Object{
		{Key: "name", Value: "GoError"},
		{Key: "message", Value: err.Error()},
	}}}
}
