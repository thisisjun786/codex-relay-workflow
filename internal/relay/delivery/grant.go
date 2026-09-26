package delivery

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// renderGrant is DeliveryService._render_grant (delivery.py:757): the merge target is this
// parent's turn, and what it can actually do about it.
func renderGrant(event string, record Obj, request string, reading supervisor.RequiredReading) string {
	turn, grant := pyStr(fieldOf(record, "turnId")), pyStr(fieldOf(record, "grantId"))
	declared, flags, guidance := requiredForNotice(record, reading)
	lines := []string{
		"[codex-session-relay] merge turn granted",
		"requestId: " + request,
		"eventId: " + event,
		"grantId: " + grant,
		"turnId: " + turn,
		"target: " + pyStr(fieldOf(record, "repository")) + " " + pyStr(fieldOf(record, "baseRef")),
		"candidateHead: " + pyStr(fieldOf(record, "candidateHead")),
		"grantedFrom: " + pyStr(fieldOf(record, "grantedFrom")),
		declared,
		"",
		"The target was released and this claim was the oldest ready one that still owns",
		"its project, so the turn is yours. Nothing here expires: the target stays yours",
		"until you land it or give it back.",
		"",
		"To act on it, from inside your own turn:",
		"  merge-turn-acknowledge --turn " + turn + " --grant " + grant + " --actor <your task id> --evidence <what you read>",
		"  merge-turn-check --turn " + turn + " --actor <your task id> --head-sha <head> --base-sha <base branch tip now> --checks <json> --review <json>" + flags,
		"  merge-turn-land --turn " + turn + " --actor <your task id> --landed-sha <the commit your merge put on the base> --observed-base-sha <base branch tip you read after the merge> --evidence <what you observed>",
		"",
		"The relay reads the base branch itself at the check and at the landing and records",
		"its own reading: a --base-sha or --observed-base-sha that disagrees is refused, and",
		"so is a landing while the branch still reads the base it was checked against.",
		"",
	}
	lines = append(lines, guidance...)
	lines = append(lines,
		"",
		"Or hand it on without merging:",
		"  merge-turn-release --turn "+turn+" --actor <your task id> --disposition returned --reason <why>",
		"",
		"There is nothing to acknowledge on this message itself. Contract v1 defines no",
		"acknowledgement for this direction and the relay refuses one by kind; the",
		"acknowledgement above is the merge turn's, and merging without it is refused.",
		"Full record: codex-session-relay merge-turn-show --turn "+turn,
	)
	return strings.Join(lines, "\n")
}

// requiredForNotice is delivery._required_for_notice.
func requiredForNotice(record Obj, reading supervisor.RequiredReading) (string, string, []string) {
	if !reading.Known {
		return "requiredDeclared: not recorded (" + reading.Reason + ")",
			" --required=<each check the branch requires>",
			[]string{
				"No required-check reading is recorded for this candidate, so --required is",
				"yours to fill. Read the branch's required checks first:",
				"  codex-session-relay merge-evidence --repository " + shellQuote(pyStr(fieldOf(record, "repository"))) + " --pull-request <its number>",
				"and pass each name in its requiredDeclared as its own --required=<name>.",
				"Leaving --required out declares that nothing is required, and a failing",
				"required check would then not stop the merge.",
			}
	}
	source := "work report " + reading.EventID + " submission " + pyStr(submission(reading.SubmissionNo))
	if len(reading.Required) == 0 {
		return "requiredDeclared: none (" + source + " found no required check)", "",
			[]string{
				"The candidate's merge-evidence reading found no required check on this branch,",
				"so the check above declares none. Read the rules again with merge-evidence if",
				"they may have changed since.",
			}
	}
	var flags strings.Builder
	for _, name := range reading.Required {
		flags.WriteString(" --required=" + shellQuote(name))
	}
	return "requiredDeclared: " + supervisor.Dumps(reading.Required, false, false, false) + " (" + source + ")", flags.String(),
		[]string{
			"--required restates what the candidate's merge-evidence reading found the branch",
			"rules require. The names are yours to declare; read them again with merge-evidence",
			"if the rules may have changed since.",
		}
}

func submission(v any) any {
	if n, ok := v.(int64); ok {
		return json.Number(strconv.FormatInt(n, 10))
	}
	return v
}

// grantRequired is DeliveryService._grant_required for a grant row.
func grantRequired(ctx context.Context, s *store.Store, row Row, record Obj) (supervisor.RequiredReading, error) {
	return supervisor.RequiredForCandidate(ctx, s, row.S("relationship_id"), str(record, "repository"), str(record, "baseRef"), str(record, "candidateHead"))
}
