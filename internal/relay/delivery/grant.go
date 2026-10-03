package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// renderGrant is DeliveryService._render_grant (delivery.py:757): the merge target is this
// parent's turn, and what it can actually do about it.
func renderGrant(event string, record Obj, request string, reading evidence.RequiredReading) string {
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
		"its project, so the turn is yours. It stays yours while you keep working it: record",
		"each step with merge-turn-progress, because a holding turn with no progress record for",
		fmt.Sprintf("%d seconds reads as stalled, and a waiting parent or the supervisor may then pass", mergeturn.HoldingLimitSeconds),
		"it on to the next waiter. Until then it is yours until you land it or give it back.",
		"",
		"To act on it, from inside your own turn:",
		"  merge-turn-acknowledge --turn " + turn + " --grant " + grant + " --actor <your task id> --evidence <what you read>",
		"  merge-turn-progress --turn " + turn + " --actor <your task id> --step " + strings.Join(mergeturn.ProgressSteps, "|") + " [--evidence <what>]",
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

// renderReturnRequest is the notice a holder gets when another parent asks for the target back
// (CRW-408): what was asked, by whom, and what the holder can do about it.
func renderReturnRequest(event string, record Obj, request string) string {
	turn := pyStr(fieldOf(record, "turnId"))
	lines := []string{
		"[codex-session-relay] merge turn return requested",
		"requestId: " + request,
		"eventId: " + event,
		"returnRequestId: " + pyStr(fieldOf(record, "requestId")),
		"turnId: " + turn,
		"target: " + pyStr(fieldOf(record, "repository")) + " " + pyStr(fieldOf(record, "baseRef")),
		"candidateHead: " + pyStr(fieldOf(record, "candidateHead")),
		"requestedBy: " + pyStr(fieldOf(record, "requestedBy")),
		"evidence: " + inline(fieldOf(record, "evidence")),
		"",
		"Another parent is waiting for this target and asked for it back. Nothing was taken",
		"from you: the turn is still yours while you keep working it.",
		"",
		"From inside your own turn, act on the state the turn is in.",
		"While it is holding (merge-turn-check has not succeeded for you):",
		"  keep working and record progress, so that the turn is not read as stalled:",
		"    merge-turn-progress --turn " + turn + " --actor <your task id> --step " + strings.Join(mergeturn.ProgressSteps, "|") + " [--evidence <what>]",
		"  or give it back now:",
		"    merge-turn-release --turn " + turn + " --actor <your task id> --disposition returned --reason <why>",
		"While it is merging (merge-turn-check succeeded), it cannot be released: record progress,",
		"and when the merge is on the base land it, or report that you cannot tell:",
		"    merge-turn-land --turn " + turn + " --actor <your task id> --landed-sha <the commit your merge put on the base> --evidence <what you observed>",
		"    merge-turn-unknown --turn " + turn + " --actor <your task id> --reason <why>",
		"If the outcome is already unknown, resolve it from an observation of the pull request and",
		"the base with merge-turn-resolve (you or the supervisor).",
		"",
		fmt.Sprintf("The holding limit is %d seconds: a holding turn with no progress record for that long", mergeturn.HoldingLimitSeconds),
		"reads as stalled in merge-turn-show, and a waiting parent or the supervisor may pass it on",
		"to the next waiter. You are then refused (merge_turn_not_held) and claim the target again",
		"if the candidate is still wanted. A merge already under way (merging) is never passed on:",
		"finish it with merge-turn-land, or report it with merge-turn-unknown.",
		"",
		"There is nothing to acknowledge on this message itself; the relay refuses an acknowledgement",
		"for this direction by kind.",
		"Full record: codex-session-relay merge-turn-show --turn " + turn,
	}
	return strings.Join(lines, "\n")
}

// requiredForNotice is delivery._required_for_notice.
func requiredForNotice(record Obj, reading evidence.RequiredReading) (string, string, []string) {
	if !reading.Known {
		return "requiredDeclared: not recorded (" + reading.Reason + ")",
			" --required=<each check the branch requires>",
			[]string{
				"No required-check reading is recorded for this candidate, so --required is",
				"yours to fill. Read the branch's required checks first:",
				"  codex-session-relay merge-evidence --repository " + quote.Shell(pyStr(fieldOf(record, "repository"))) + " --pull-request <its number>",
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
		flags.WriteString(" --required=" + quote.Shell(name))
	}
	return "requiredDeclared: " + pyjson.Dumps(reading.Required, pyjson.Options{Unicode: true}) + " (" + source + ")", flags.String(),
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
func grantRequired(ctx context.Context, s *store.Store, row Row, record Obj) (evidence.RequiredReading, error) {
	return evidence.RequiredForCandidate(ctx, s, row.S("relationship_id"), pyjson.Text(record.Get("repository")), pyjson.Text(record.Get("baseRef")), pyjson.Text(record.Get("candidateHead")))
}
