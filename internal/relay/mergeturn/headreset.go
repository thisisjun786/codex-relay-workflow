package mergeturn

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// headReset is a candidate head that a merge-turn-ready call restated. Restating a head resets
// readiness and, on a holding turn, issues a grant that has to be acknowledged (Service.Ready), so the
// answer to the call says what was reset and what the holder does next: a candidate_restated grant
// wakes nobody, and when readiness was already off the ledger records nothing about a --ready the call
// dropped, so without this the caller would learn of either only when merge-turn-check refuses it.
type headReset struct {
	from, to   string
	readyAsked bool
	// grant is the grant this call issued, read in its own transaction; "" when it issued none (a
	// waiting claim holds nothing to acknowledge).
	grant string
}

// answer is the readinessReset object of the call's answer.
func (h headReset) answer(turn, actor string) map[string]any {
	var owed any
	if h.grant != "" {
		owed = h.grant
	}
	return map[string]any{"previousHead": h.from, "candidateHead": h.to, "readyRequested": h.readyAsked, "grantId": owed, "detail": h.detail(turn, actor)}
}

// detail names the steps that follow a restated head: the grant is acknowledged, the new head's
// checks finish, readiness is declared on the new head.
func (h headReset) detail(turn, actor string) string {
	var b strings.Builder
	b.WriteString("restating the candidate head from " + h.from + " to " + h.to + " resets readiness")
	if h.readyAsked {
		b.WriteString(", so the --ready given with it was not recorded")
	}
	b.WriteString(". Next: ")
	if h.grant != "" {
		b.WriteString("acknowledge grant " + h.grant + " with merge-turn-acknowledge --turn " + turn + " --actor " + actor + " --grant " + h.grant + " --evidence <what you read>; ")
	}
	b.WriteString("once " + h.to + "'s checks have finished, declare readiness with merge-turn-ready --turn " + turn + " --actor " + actor + " --head " + h.to + " --ready")
	return b.String()
}

// restateCommand is the command that declares head as the candidate of a turn. merge-turn-ready
// requires one of --ready and --not-ready, and a restated head cannot be ready yet, so the first
// declaration is --not-ready.
func restateCommand(turn, actor, head string) string {
	return "merge-turn-ready --turn " + turn + " --actor " + actor + " --head " + head + " --not-ready (it resets readiness and, on a holding turn, issues a grant to acknowledge)"
}

// notReadyStep is what merge-turn-check names when the turn has not declared its candidate ready:
// declare that candidate once its checks have finished, or, when the check states another head,
// declare the stated one first, because declaring the stored one ready would mark a head the
// caller no longer means.
func notReadyStep(turn, actor, candidate, stated string) string {
	if stated == candidate {
		return "once its checks have finished, declare it with merge-turn-ready --turn " + turn + " --actor " + actor + " --head " + candidate + " --ready (restating a head resets readiness)"
	}
	return "this check states " + pyvalue.StrRepr(stated) + ", which is not that head: declare the head you mean with " + restateCommand(turn, actor, stated) + ", then follow its answer"
}
