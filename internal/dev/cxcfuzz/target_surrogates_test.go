//go:build dev

package cxcfuzz

import "testing"

// A rewrite that keeps some unpaired surrogates of the oracle's string and replaces the rest with U+FFFD
// has lost text, so the verdict is a data-loss in both targets, not a plain difference (CRW-978 c2).
func TestPartialSurrogateReplacementIsALossInBothTargets(t *testing.T) {
	oracle := `{"slug":"\ud800A\udfff"}`
	goText := `{"slug":"\ufffdA\udfff"}`
	requireDataLoss(t, stateCompare(writtenAnswer(goText), writtenAnswer(oracle)), "slug")
	requireDataLoss(t, goalplanCompare(writtenAnswer(goText), writtenAnswer(oracle)), "slug")
}

// The partial replacement is found at any depth and at any number of positions, and the path it names is
// the one holding the kept and replaced surrogates (CRW-978 c2).
func TestPartialSurrogateReplacementIsALossAtAnyDepthAndPosition(t *testing.T) {
	oracle := `{"a":{"b":["\ud800x\udc00\udfff", "\ud801\ud801"]}}`
	goText := `{"a":{"b":["\ufffdx\udc00\ufffd", "\ud801\ud801"]}}`
	requireDataLoss(t, goalplanCompare(writtenAnswer(goText), writtenAnswer(oracle)), "a.b[0]")
}

// Controls: a value with no surrogate difference stays Same, and a surrogate replaced by a different
// character, not U+FFFD, is a plain difference rather than a loss (CRW-978 c2).
func TestSurrogateControlsStaySameAndPlain(t *testing.T) {
	same := `{"slug":"\ud800A"}`
	if verdict := goalplanCompare(writtenAnswer(same), writtenAnswer(same)); verdict.Kind != Same {
		t.Fatalf("an identical surrogate value compared %v (%s), want Same", verdict.Kind, verdict.Detail)
	}
	plain := `{"slug":"\u0041A"}`
	verdict := stateCompare(writtenAnswer(plain), writtenAnswer(`{"slug":"\ud800A"}`))
	if verdict.Kind != Differ || containsLoss(verdict.Detail) {
		t.Fatalf("a surrogate replaced by a plain character compared %v (%s), want a plain difference", verdict.Kind, verdict.Detail)
	}
}

func containsLoss(detail string) bool {
	return len(detail) >= len("data-loss") && detail[:len("data-loss")] == "data-loss"
}
