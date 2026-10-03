package mergeturn

import "strings"

// headReset is a candidate head that a merge-turn-ready call restated. Restating a head resets
// readiness and, on a holding turn, issues a grant that has to be acknowledged (Service.Ready), so the
// answer to the call says what was reset and what the holder does next: a candidate_restated grant
// wakes nobody, and when readiness was already off the ledger records nothing about a --ready the call
// dropped, so without this the caller would learn of either only when merge-turn-check refuses it.
type headReset struct {
	from, to   string
	readyAsked bool
}

// answer is the readinessReset object of the call's answer. held is the turn the call answers with:
// the grant it reports as current is the one owed, and only a holding turn has one.
func (h headReset) answer(turn, actor string, held map[string]any) map[string]any {
	var owed any
	if grant, _ := held["grant"].(map[string]any); grant != nil && held["state"] == Holding {
		owed = grant["grantId"]
	}
	id, _ := owed.(string)
	return map[string]any{"previousHead": h.from, "candidateHead": h.to, "readyRequested": h.readyAsked, "grantId": owed, "detail": h.detail(turn, actor, id)}
}

// detail names the steps that follow a restated head, in the order merge-turn-check needs them: the
// grant is acknowledged, the new head's checks finish, readiness is declared on the new head.
func (h headReset) detail(turn, actor, grant string) string {
	var b strings.Builder
	b.WriteString("restating the candidate head from " + h.from + " to " + h.to + " resets readiness")
	if h.readyAsked {
		b.WriteString(", so the --ready given with it was not recorded")
	}
	b.WriteString(". Next: ")
	if grant != "" {
		b.WriteString("acknowledge grant " + grant + " with merge-turn-acknowledge --turn " + turn + " --actor " + actor + " --grant " + grant + " --evidence <what you read>; ")
	}
	b.WriteString("once " + h.to + "'s checks have finished, declare readiness with merge-turn-ready --turn " + turn + " --actor " + actor + " --head " + h.to + " --ready")
	return b.String()
}
