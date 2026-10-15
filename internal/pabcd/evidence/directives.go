package evidence

import (
	"fmt"
	"strings"
)

// VerifierDirective is the reason of the block that sends an agent back when it stops without a valid receipt: the attempt it
// is on, what to record and where, and, at the first attempt, the way out of a read-only dispatch (verifierDirective). The texts
// of the three attempts are the oracle's with the state directory named .crw (testdata/directives.golden.json).
func VerifierDirective(attempt int) string {
	return strings.Join([]string{
		"Your completion is unverified — no evidence receipt was recorded.",
		fmt.Sprintf("This is attempt %d of %d.", attempt, MaxAttempts),
		"Actually run the relevant checks (build/tests/commands), write the output and your",
		"judgement to a file under `.crw/evidence/`, and make the LAST line of your reply",
		"exactly `EVIDENCE_RECORDED: <path>` pointing at that file. Do not claim done without it.",
		"If you were dispatched READ-ONLY and cannot write there, say so plainly in your final",
		"message: your parent must re-dispatch you as an `explorer` (read-only lanes are not",
		"evidence-gated). Do not keep repeating the same answer — this budget is bounded.",
	}, " ")
}

// VerifierDirectiveFor is the reason of the block that sends back a child with a flow contract (CRW-1482): the attempt it is on and
// the one absolute file its receipt goes to, which the engine already named in its spawn message and packet. The child's final
// message stays the one JSON value its packet names, so no marker line is asked for. The generic VerifierDirective is unchanged.
func VerifierDirectiveFor(attempt int, receiptPath string) string {
	return strings.Join([]string{
		"Your completion is unverified — no evidence receipt was recorded.",
		fmt.Sprintf("This is attempt %d of %d.", attempt, MaxAttempts),
		"Actually run the relevant checks (build/tests/commands) and write the commands, their output",
		"(red before, green after) and your judgement to the file `" + receiptPath + "`.",
		"That exact file is your receipt; a receipt anywhere else does not count.",
		"Your final message stays exactly the JSON object your packet names: no `EVIDENCE_RECORDED` line is needed after it.",
		"Do not claim done without the receipt. Do not keep repeating the same answer — this budget is bounded.",
	}, " ")
}

// EscalationDirective is the reason the oracle's gate gave when the budget was spent and it still blocked. Nothing calls it any
// more, because the budget became terminal (a tombstone and a release), and its text still says "fail-closed"; it is ported
// for completeness.
func EscalationDirective() string {
	return strings.Join([]string{
		fmt.Sprintf("Evidence verification failed %d times and is now fail-closed.", MaxAttempts),
		"Do not claim completion. Record the actual validation under `.crw/evidence/`",
		"and finish with `EVIDENCE_RECORDED: <path>`. If validation cannot run, record",
		"the blocker and diagnostics in that receipt so the parent can decide safely.",
	}, " ")
}
