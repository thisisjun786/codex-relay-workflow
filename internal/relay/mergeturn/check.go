package mergeturn

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// pythonJSON is json.dumps(value) with Python's default separators and ensure_ascii.
func pythonJSON(v any) string { return pyjson.Dumps(v, pyjson.Options{}) }

// canonicalJSON is json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).
func canonicalJSON(v any) string {
	return pyjson.Dumps(v, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
}

// checksDigest is mergeturn.checks_digest: the declared required set is inside the digest.
func checksDigest(required []string, checks []any) string {
	lines := make([]string, 0, len(checks))
	for _, entry := range checks {
		o, _ := evidence.Object(entry)
		fields := make([]string, 0, 5)
		for _, name := range []string{"runId", "name", "headSha", "conclusion", "attempt"} {
			v, present := o.Lookup(name)
			if !present {
				fields = append(fields, "")
				continue
			}
			fields = append(fields, pyvalue.Str(v))
		}
		lines = append(lines, strings.Join(fields, "|"))
	}
	slices.Sort(lines)
	return pyvalue.SHA256Hex(canonicalJSON(contract.OrderedObject{{Key: "required", Value: required}, {Key: "checks", Value: lines}}))
}

func checkID(turn, head, base, digestChecks, digestReview string) string {
	return key("chk", turn, head, base, digestChecks, digestReview)
}

// Check is MergeTurn.begin_merge: restate exact head and base, the checks and the review,
// immediately before merging. checks and review are decoded JSON values (objects as
// contract.OrderedObject, integers as json.Number), review nil for Python's None.
func (s *Service) Check(ctx context.Context, turn, actor, head, base string, checkList any, review any, required []string, reader Reader) (map[string]any, error) {
	if review == nil {
		review = contract.OrderedObject{}
	}
	if problems := evidence.ReviewShapeProblems(review); len(problems) > 0 {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceMalformed), Detail: "the review restated for turn " + strconv.Quote(turn) + " is malformed, so the turn and its target were not read and nothing was recorded for this check: " + strings.Join(evidence.Details(problems), "; ")}
	}
	required = slices.Compact(slices.Sorted(slices.Values(required)))
	if required == nil {
		required = []string{}
	}
	checks, _ := evidence.List(checkList)
	if checks == nil {
		checks = []any{}
	}
	digestChecks := checksDigest(required, checks)
	digestReview := pyvalue.SHA256Hex(canonicalJSON(review))
	id := checkID(turn, head, base, digestChecks, digestReview)
	early, err := s.Store.MergeTurn(ctx, turn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var tip Tip
	unread := "the turn was not holding this ready candidate when the call began, and changed during it; call again"
	// moved is how the branch got from the last landing's recorded base to the tip, read here
	// like the tip, before the transaction; nil when there is no landing or nothing moved.
	var moved *moveReading
	if err == nil && early.HolderTaskID == actor && early.State == Holding && early.DeclaredReady == 1 && early.CandidateHead == head {
		tip, unread = readTarget(ctx, reader, early.Repository, early.BaseRef)
		if tip.SHA != "" && SameCommit(base, tip.SHA) {
			var readErr error
			if moved, readErr = s.readMoveSinceLanding(ctx, early, tip, reader); readErr != nil {
				return nil, readErr
			}
		}
	}
	at := s.now()
	var refusal *registry.CoordinationRefusal
	var verified any
	var restated map[string]any
	err = s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		row, e := s.row(tx, turn)
		if e != nil {
			return e
		}
		refuse := func(reason contract.RefusalReason, detail, incumbent, challenger string) {
			refusal = &registry.CoordinationRefusal{Reason: reason, Detail: detail, Domain: registry.DomainMergeTarget, Subject: row.TargetKey, Incumbent: incumbent, Challenger: challenger}
		}
		switch {
		case row.HolderTaskID != actor:
			refusal = notHolder(row, actor, "begin a merge on")
		case row.State != Holding:
			refusal = wrongState(row, actor, "beginning a merge")
		case row.DeclaredReady != 1:
			refuse(contract.RefusalMergeCandidateMoved, "turn "+pyvalue.StrRepr(turn)+" has not declared its candidate ready, so there is nothing saying "+pyvalue.StrRepr(row.CandidateHead)+" is the head it means to merge; "+notReadyStep(turn, actor, row.CandidateHead, head), row.CandidateHead, actor)
		}
		if refusal == nil {
			held, e := s.parents(tx, row.ProjectKey)
			if e != nil {
				return e
			}
			if len(held) != 1 || held[0] != actor {
				owner := ""
				if len(held) > 0 {
					owner = held[0]
				}
				refusal = staleOwner(row, owner, len(held) > 0, actor)
			}
		}
		if refusal == nil {
			status, e := s.ownerStatus(tx, row.ProjectKey)
			if e != nil {
				return e
			}
			if status != "active" {
				refusal = paused(row, actor, "begin a merge")
			}
		}
		if refusal == nil {
			if refusal, e = s.unansweredGrant(tx, row, actor); e != nil {
				return e
			}
		}
		if refusal == nil && head != row.CandidateHead {
			refuse(contract.RefusalMergeCandidateMoved, "the candidate head is "+pyvalue.StrRepr(row.CandidateHead)+" and the restated head is "+pyvalue.StrRepr(head)+"; the turn was granted for the first. Restate the head the turn holds, or declare the one you mean with "+restateCommand(turn, actor, head), row.CandidateHead, head)
		}
		if refusal == nil && tip.SHA == "" {
			refusal = unreadableTarget(row, actor, unread, "the restated base cannot be compared with it")
		}
		if refusal == nil && !SameCommit(base, tip.SHA) {
			refuse(contract.RefusalMergeCurrencyStale, "the base branch "+pyvalue.StrRepr(row.BaseRef)+" reads "+pyvalue.StrRepr(tip.SHA)+" and this restates "+pyvalue.StrRepr(base)+"; restate the base the branch points at now, in full", tip.SHA, base)
		}
		if refusal == nil {
			landing, e := s.latestLanding(tx, row.TargetKey)
			if e != nil {
				return e
			}
			if landing != nil {
				observed, _ := landing.Get("observed_base_sha").(string)
				if !SameCommit(observed, base) {
					// The base moved after the last landing. When the move is confirmed as merges made
					// outside the lane, the lane records the base again itself and the check goes on;
					// otherwise the refusal says what is known and how to repair it.
					done, detail, e := s.restateAfterOutOfLaneMerge(tx, row, landing, actor, base, tip, moved, id, at)
					if e != nil {
						return e
					}
					if done != nil {
						restated = done
					} else {
						refuse(contract.RefusalMergeCurrencyStale, detail, observed, base)
					}
				}
			}
		}
		if refusal == nil {
			if problems := evidence.ChecksProblems(head, required, checks); len(problems) > 0 {
				refuse(contract.RefusalMergeCurrencyStale, problems[0].Detail, problems[0].Incumbent, actor)
			}
		}
		if refusal == nil {
			if problems := evidence.ReviewProblems(review); len(problems) > 0 {
				refuse(contract.RefusalMergeReviewIncomplete, strings.Join(evidence.Details(problems), "; "), "", actor)
			}
		}
		if refusal == nil && row.RelationshipID.Valid && row.RelationshipID.String != "" {
			if verified, refusal, e = s.relationshipRefusal(tx, row, actor, head); e != nil {
				return e
			}
		}
		result := "current"
		reason := sql.NullString{}
		if refusal != nil {
			result = "refused"
			reason = nullable(string(refusal.Reason))
		}
		if e = s.Store.RecordMergeCheck(tx, store.MergeTurnChecksRow{CheckID: id, TurnID: turn, HeadSHA: head, BaseSHA: base, Required: pythonJSON(required), ChecksDigest: digestChecks, Checks: pythonJSON(checks), ReviewDigest: digestReview, Review: pythonJSON(review), Result: result, RefusalReason: reason, RecordedAt: at}); e != nil {
			return e
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		if _, e = s.Store.Querier(tx).ExecContext(tx, "UPDATE merge_turns SET state = ?, merging_at = ?, updated_at = ?, checked_base_sha = ? WHERE turn_id = ?", Merging, at, at, tip.SHA, turn); e != nil {
			return e
		}
		mark := contract.OrderedObject{{Key: "checkId", Value: id}, {Key: "baseRead", Value: tip.SHA}, {Key: "source", Value: tip.Source}}
		return s.ledger(tx, turn, "transition", Holding, Merging, "currency_confirmed", actor, canonicalJSON(mark), "merging:"+head, at)
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.Error()
	}
	answer, err := s.Turn(ctx, turn)
	if err != nil {
		return nil, err
	}
	answer["checkId"] = id
	answer["requiredDeclared"] = required
	answer["headVerifiedAgainst"] = verified
	if restated != nil {
		answer["landingBaseRestated"] = restated
	}
	return answer, nil
}

// relationshipRefusal is MergeTurn._relationship_refusal: when the claim named a relay
// assignment, its current work reports' head must agree.
func (s *Service) relationshipRefusal(ctx context.Context, row store.MergeTurnsRow, actor, head string) (any, *registry.CoordinationRefusal, error) {
	rid := row.RelationshipID.String
	attachment, err := s.Registry.Attachment(ctx, rid)
	if err != nil {
		return nil, nil, err
	}
	if o, ok := attachment.(contract.OrderedObject); ok {
		if project := o.Get("projectKey"); project != nil && project != row.ProjectKey {
			return nil, &registry.CoordinationRefusal{Reason: contract.RefusalForeignScope, Detail: "relationship " + pyvalue.StrRepr(rid) + " belongs to project " + pyvalue.Repr(project) + ", not " + pyvalue.StrRepr(row.ProjectKey), Domain: registry.DomainMergeTarget, Subject: row.TargetKey, Challenger: actor}, nil
		}
	}
	current, err := evidence.CurrentReportHeads(ctx, s.Store, rid)
	if err != nil {
		return nil, nil, err
	}
	heads := slices.Compact(slices.Sorted(slices.Values(current)))
	if len(heads) == 0 {
		return nil, nil, nil
	}
	if len(heads) > 1 {
		return nil, &registry.CoordinationRefusal{Reason: contract.RefusalRevisionAmbiguous, Detail: "relationship " + pyvalue.StrRepr(rid) + " has work reports naming " + pyvalue.Repr(heads) + "; which one this candidate is cannot be read off them", Domain: registry.DomainMergeTarget, Subject: row.TargetKey, Incumbent: heads[0], Challenger: head}, nil
	}
	if heads[0] != head {
		return nil, &registry.CoordinationRefusal{Reason: contract.RefusalMergeCandidateMoved, Detail: "the work report for " + pyvalue.StrRepr(rid) + " names head " + pyvalue.StrRepr(heads[0]) + " and this restates " + pyvalue.StrRepr(head), Domain: registry.DomainMergeTarget, Subject: row.TargetKey, Incumbent: heads[0], Challenger: head}, nil
	}
	return heads[0], nil, nil
}
