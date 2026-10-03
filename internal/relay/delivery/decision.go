package delivery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The decision reply (CRW-394). A child that stops with a blocked_needs_input receipt waits for the parent, and a
// verdict cannot answer it: the receipt carries no artifact, so it is never the head revision. A decision is the
// parent's own record on that receipt, kept apart from the verdict writer. docs/relay/README.md, "Replying to a
// blocked receipt", is the contract; this file is the rule and the message.
//
//	decision        generation       what the relay requires
//	answer          stays            a note
//	stop            stays            a note
//	split_approval  advances to g+1  a note and the digest of the criteria registered for the relationship
//	scope_change    advances to g+1  the same

// Decision kinds, and the outcome and generation reason a reply carries.
const (
	DecisionReply         = "decision_reply"
	DecisionAnswer        = "answer"
	DecisionStop          = "stop"
	DecisionSplitApproval = "split_approval"
	DecisionScopeChange   = "scope_change"
)

// MalformedReceipt is the reason a request that does not say what it must is refused with.
const MalformedReceipt = string(contract.RefusalMalformedReceipt)

// decisionKinds lists the kinds. advances says which of them open the next generation: the ones that change the
// criteria the child works under.
var decisionKinds = []string{DecisionAnswer, DecisionStop, DecisionSplitApproval, DecisionScopeChange}

func advances(decision string) bool {
	return decision == DecisionSplitApproval || decision == DecisionScopeChange
}

// DecisionRequest is what decision-reply is asked.
type DecisionRequest struct{ EventID, Decision, Turn, Note, CriteriaDigest string }

// keepsAnchor is whether the event is a decision reply that keeps its generation (answer, stop): the reply's turn is a later turn
// of a generation that has its anchor, so it is not the anchor and does not replace it, and offering it to the anchor binding would
// be refused (anchor_already_bound) or journalled as a conflict. A decision that opened its generation goes through the binding
// like a correction does, which binds the turn it opened or reports a conflict with the turn it was bound to by hand.
func keepsAnchor(event Row) bool {
	return event != nil && event.S("outcome") == DecisionReply && pyjson.Text(loadsObj(event.S("receipt")).Get("generationEffect")) == "stays"
}

// DecisionEventID is the id of the one decision a receipt can carry.
func DecisionEventID(relationship, answered string) string {
	sum := sha256.Sum256([]byte(relationship + "|" + answered + "|" + DecisionReply))
	return hex.EncodeToString(sum[:])[:32]
}

// RecordDecision records the parent's decision on a child's blocked_needs_input receipt and queues it to the child in
// one transaction. The same decision again (the same kind, note and criteria digest) is a replay (the record, marked _replay, and
// nothing written); any other decision on the same receipt is refused disposition_conflict. A refusal writes nothing.
func (a *Ack) RecordDecision(ctx context.Context, req DecisionRequest) (Obj, error) {
	note, turn, digest := strings.TrimSpace(req.Note), strings.TrimSpace(req.Turn), strings.TrimSpace(req.CriteriaDigest)
	switch {
	case !slices.Contains(decisionKinds, req.Decision):
		return nil, refuse(MalformedReceipt, "unknown decision %s: it is one of %s", strconv.Quote(req.Decision), strings.Join(decisionKinds, ", "))
	case note == "":
		return nil, refuse(MalformedReceipt, "a decision carries a note: what the child is told")
	case turn == "":
		return nil, refuse(MalformedReceipt, "a decision names the turn it was made in (--decision-turn)")
	case advances(req.Decision) && digest == "":
		return nil, refuse(MalformedReceipt, "a %s changes the child's criteria, so it names the digest of the set registered for the relationship (--criteria-digest; criteria-show prints it)", req.Decision)
	case !advances(req.Decision) && digest != "":
		return nil, refuse(MalformedReceipt, "only split_approval and scope_change name a criteria set; a %s leaves the child's criteria as they are", req.Decision)
	}
	now := a.Clock.ISO()
	var record Obj
	err := a.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		record = nil
		event, err := a.Delivery.eventRow(ctx, req.EventID)
		if err != nil {
			return err
		}
		if event == nil {
			return refuse(NotClaimable, "no event %s: a decision answers a receipt the relay holds", strconv.Quote(req.EventID))
		}
		rid := event.S("relationship_id")
		id := DecisionEventID(rid, req.EventID)
		prior, err := a.Delivery.eventRow(ctx, id)
		if err != nil {
			return err
		}
		if prior != nil {
			recorded := loadsObj(prior.S("receipt"))
			recordedDigest, _ := recorded.Lookup("criteriaDigest")
			if kind := pyjson.Text(recorded.Get("decision")); kind != req.Decision || pyjson.Text(recorded.Get("note")) != note || pyjson.Text(recordedDigest) != digest {
				return refuse(DispositionConflict, "%s already carries the decision %s (decision turn %s), and a decision of %s with another note or criteria set cannot replace it: a decision is final for its receipt, so a change of mind goes to the child as a decision on its next receipt", strconv.Quote(req.EventID), kind, strconv.Quote(pyjson.Text(recorded.Get("decisionTurnId"))), req.Decision)
			}
			record = append(recorded, F{Key: "_replay", Value: true})
			return nil
		}
		if event.S("producer") != "child" || event.S("outcome") != "blocked_needs_input" {
			return refuse(DispositionConflict, "%s is a %s receipt of the %s, and a decision answers a blocked_needs_input receipt of the child: a receipt that carries an artifact is ruled with a verdict", strconv.Quote(req.EventID), event.S("outcome"), event.S("producer"))
		}
		if event.S("stage") != "final" || truthy(event.Opt("suppressed_reason")) {
			return refuse(DispositionConflict, "%s is not final: its turn has not been seen to end, so the parent has not been told and there is nothing to answer yet", strconv.Quote(req.EventID))
		}
		if ruled, err := one(ctx, a.Store, "SELECT verdict FROM verdicts WHERE event_id = ?", req.EventID); err != nil {
			return err
		} else if ruled != nil {
			return refuse(DispositionConflict, "%s is already ruled %s, and a decision and a verdict never both answer one receipt", strconv.Quote(req.EventID), ruled.S("verdict"))
		}
		relationship, err := RequireActive(ctx, a.Store, rid)
		if err != nil {
			return err
		}
		current, err := one(ctx, a.Store, "SELECT execution_generation FROM relationships WHERE relationship_id = ?", rid)
		if err != nil {
			return err
		}
		generation := event.I("execution_generation")
		if generation != current.I("execution_generation") {
			return refuse(StaleGeneration, "%s is generation %d and the assignment is on generation %d: a decision answers the receipt the current generation stands on", strconv.Quote(req.EventID), generation, current.I("execution_generation"))
		}
		// later is read in the order the relay saw the receipts: when it first saw each one, and for receipts first seen at one
		// instant the order they were stored in (an event id is a hash of the receipt, not a sequence)
		later, err := one(ctx, a.Store, "SELECT event_id FROM events WHERE relationship_id = ? AND execution_generation = ? AND stage = 'final' AND suppressed_reason IS NULL AND producer = 'child' AND event_id != ? AND (first_seen_at > ? OR (first_seen_at = ? AND rowid > (SELECT rowid FROM events WHERE event_id = ?))) ORDER BY first_seen_at DESC, rowid DESC LIMIT 1",
			rid, generation, req.EventID, event.S("first_seen_at"), event.S("first_seen_at"), req.EventID)
		if err != nil {
			return err
		}
		if later != nil {
			return refuse(SupersededRevision, "the child reported again in generation %d (%s), so %s is no longer its newest word: answer that receipt", generation, later.S("event_id"), strconv.Quote(req.EventID))
		}
		child := relationship.Child.TaskID
		if !slices.Contains(relationship.AllowedRecipients, child) {
			return refuse(RecipientNotAuthorized, "child %s is not an allowed recipient, so a decision cannot be routed to it", strconv.Quote(child))
		}
		effect, anchor := "stays", any(nil)
		var criteria any
		if advances(req.Decision) {
			effect = "advances"
			registered, err := a.Criteria.Get(ctx, rid)
			if err != nil {
				return err
			}
			if registered == nil {
				return refuse(CriteriaUnregistered, "relationship %s has no criteria registered, so there is no set for the child to continue under: register it (criteria-register), then reply", strconv.Quote(rid))
			}
			if set := pyjson.Text(registered.Get("setDigest")); set != digest {
				return refuse(CriteriaSetChanged, "the criteria registered for %s have digest %s and the reply names %s: register the set the child continues under (criteria-register), then reply with its digest", strconv.Quote(rid), set, digest)
			}
			criteria, _ = registered.Lookup("criteria")
		} else {
			bound, err := one(ctx, a.Store, "SELECT dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", rid, generation)
			if err != nil {
				return err
			}
			if bound == nil || strings.TrimSpace(bound.S("dispatch_turn_id")) == "" {
				return refuse(string(contract.RefusalUnboundGeneration), "generation %d of %s has no bound anchor, so the child's next turn cannot be admitted to it", generation, strconv.Quote(rid))
			}
			anchor = bound.S("dispatch_turn_id")
		}
		number := generation
		if advances(req.Decision) {
			if number, err = OpenGenerationIn(ctx, a.Store, a.Clock, rid, "decision-"+id, DecisionReply, nil); err != nil {
				return err
			}
		}
		record = Obj{{Key: "eventId", Value: id}, {Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: number}, {Key: "kind", Value: DecisionReply}, {Key: "decision", Value: req.Decision},
			{Key: "answersEvent", Value: req.EventID}, {Key: "decisionTurnId", Value: turn}, {Key: "note", Value: note}, {Key: "generationEffect", Value: effect}}
		if advances(req.Decision) {
			// the set is kept as it was when the decision was made: the message prints it, so the child works from what the parent
			// approved even if the set is registered again before the message is sent
			record = append(record, F{Key: "nextExecutionGeneration", Value: number}, F{Key: "criteriaDigest", Value: digest}, F{Key: "criteria", Value: criteria})
		} else {
			record = append(record, F{Key: "anchorTurnId", Value: anchor})
		}
		record = append(record, F{Key: "childTaskId", Value: child}, F{Key: "decidedAt", Value: now})
		if err := a.queueToChild(ctx, id, rid, number, DecisionReply, relationship.Parent.TaskID, turn, child, record, now); err != nil {
			return err
		}
		return journal(ctx, a.Store, "decision_recorded", id, Obj{{Key: "relationship", Value: rid}, {Key: "answers", Value: req.EventID}, {Key: "decision", Value: req.Decision}, {Key: "generation", Value: number}}, now)
	})
	return record, err
}

// DecisionsOf reads back the decisions recorded for a relationship, oldest first, each with where its delivery stands.
func (a *Ack) DecisionsOf(ctx context.Context, rid string) (Obj, error) {
	r, err := LoadRelationship(ctx, a.Store, rid)
	if err != nil {
		return nil, err
	}
	rows, err := all(ctx, a.Store, "SELECT e.receipt AS receipt, d.state AS state, d.hold_reason AS hold_reason, d.dispatch_turn_id AS dispatch_turn_id FROM events e LEFT JOIN deliveries d ON d.event_id = e.event_id WHERE e.relationship_id = ? AND e.outcome = ? ORDER BY e.first_seen_at, e.event_id", rid, DecisionReply)
	if err != nil {
		return nil, err
	}
	decisions := []any{}
	for _, row := range rows {
		delivery := Obj{{Key: "state", Value: row.Opt("state")}, {Key: "holdReason", Value: row.Opt("hold_reason")}, {Key: "dispatchTurnId", Value: row.Opt("dispatch_turn_id")}}
		decisions = append(decisions, append(loadsObj(row.S("receipt")), F{Key: "delivery", Value: delivery}))
	}
	return Obj{{Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: r.Generation}, {Key: "decisions", Value: decisions}}, nil
}

// criteriaLines is the criteria set a decision carries, one line per criterion, as many as the revision message shows of its findings.
func criteriaLines(set any, event string) []string {
	items, _ := set.([]any)
	var lines []string
	for _, item := range items[:min(len(items), manifestLines)] {
		o, _ := item.(Obj)
		id, _ := o.Lookup("id")
		title, _ := o.Lookup("title")
		lines = append(lines, "  "+unheaded(inline(id))+": "+inline(title))
	}
	if len(items) > manifestLines {
		lines = append(lines, fmt.Sprintf("  ... and %d more: codex-session-relay show --event %s prints the whole set", len(items)-manifestLines, event))
	}
	return lines
}

// renderDecision is the message the child receives: the decision, what it changes and the claim that admits its next turn.
func renderDecision(row Row, record Obj, request string) string {
	g := func(k string) any { v, _ := record.Lookup(k); return v }
	rid, event, decision := row.S("relationship_id"), row.S("event_id"), pyjson.Text(g("decision"))
	generation := known(g("executionGeneration"))
	moves := advances(decision)
	lines := []string{
		"[codex-session-relay] parent decision",
		"requestId: " + request,
		"eventId: " + event,
		"relationshipId: " + rid,
		"decision: " + decision,
		"answersEvent: " + known(g("answersEvent")),
		"executionGeneration: " + generation + map[bool]string{true: "  (new)", false: "  (unchanged)"}[moves],
	}
	if moves {
		lines = append(lines, "criteriaDigest: "+known(g("criteriaDigest")))
	}
	lines = append(lines, "", "DECISION: "+unheaded(inline(g("note"))), "")
	switch decision {
	case DecisionAnswer:
		lines = append(lines, "This answers the question your blocked_needs_input receipt asked. Your generation and criteria are unchanged: continue the work from where you stopped.")
	case DecisionStop:
		lines = append(lines, "Stop the work now and change nothing more in your checkout. End this turn recording the disposition interrupted and emit an interrupted receipt.")
	default:
		lines = append(lines,
			"Your criteria changed: continue on the same node under the set below, which is the set registered for this relationship when the decision was made (criteria-show --relationship "+rid+" prints the set registered now; the criteriaDigest above names this one).")
		lines = append(lines, criteriaLines(g("criteria"), event)...)
		lines = append(lines,
			"This turn is the anchor of generation "+generation+": your next receipts name that generation, and the first one passes no --supersedes-revision (this generation holds no earlier revision to replace).")
	}
	lines = append(lines, "", "Nothing is acknowledged: this direction defines no acknowledgement and the relay refuses one by kind.")
	if !moves {
		outcome := "ready_for_review"
		if decision == DecisionStop {
			outcome = "interrupted"
		}
		lines = append(lines, "Your next receipt comes from a later turn of generation "+generation+", so it states the claim that admits that turn:",
			fmt.Sprintf("  emit --relationship %s --generation %s --attempt <n>", rid, generation),
			"       --outcome "+outcome+" --turn-thread "+pyjson.Text(g("childTaskId"))+" --turn-id <your turn>",
			"       --continues-anchor "+pyjson.Text(g("anchorTurnId"))+" --continuation-actor "+pyjson.Text(g("childTaskId"))+" --continuation-reason 'after the parent decision "+event+"'")
	}
	return strings.Join(append(lines, "", "Full record: codex-session-relay show --event "+event), "\n")
}
