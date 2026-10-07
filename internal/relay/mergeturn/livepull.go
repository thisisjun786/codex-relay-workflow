package mergeturn

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A parent holds one live turn per target. merge-turn-request used to answer a request for another pull request with
// the live turn (CRW-538): a caller that took the answer for the turn of the pull request it asked about declared that
// pull request's head on the live one. The request is now refused, saying which turn it is.

// compareClaim reads what a request names against what the holder's live claim records, one kind of identity at a time
// (the pull request number, the relationship id). contradicted lists the kinds the request names and the claim records as
// something else; unrecorded lists the kinds the request names and the claim records none of. A kind the request does not
// name is in neither.
func compareClaim(live store.MergeTurnsRow, asked ClaimOptions) (contradicted, unrecorded []string) {
	if asked.PR.Valid {
		switch {
		case !live.PRNumber.Valid:
			unrecorded = append(unrecorded, "pull request")
		case live.PRNumber.Int64 != asked.PR.Int64:
			contradicted = append(contradicted, "pull request")
		}
	}
	if asked.Relationship.Valid && asked.Relationship.String != "" {
		switch {
		case !live.RelationshipID.Valid || live.RelationshipID.String == "":
			unrecorded = append(unrecorded, "relationship")
		case live.RelationshipID.String != asked.Relationship.String:
			contradicted = append(contradicted, "relationship")
		}
	}
	return contradicted, unrecorded
}

// requestIsLiveClaim is whether a request is the repeat of the holder's live claim: it names no pull request and no
// relationship, or every one it names is recorded on the claim and equal. The commit a request states is not an identity
// (CRW-587): two pull requests can point at one commit, so a request that names an identity the claim did not record
// cannot be told from a request for another pull request, whatever head it states. dag-merge-request reads the answer it
// gets with the same suspicion.
func requestIsLiveClaim(live store.MergeTurnsRow, asked ClaimOptions) bool {
	contradicted, unrecorded := compareClaim(live, asked)
	return len(contradicted) == 0 && len(unrecorded) == 0
}

// livePlace is where a live claim stands in the order the lane serves its target in: the claims that hold the target
// first, then the waiting ones by the time they were made (the order a released target promotes them in). A claim that
// closed is not counted.
func (s *Service) livePlace(ctx context.Context, live store.MergeTurnsRow) (place, of int, err error) {
	rows, err := s.Store.MergeTurnsForTarget(ctx, live.TargetKey)
	if err != nil {
		return 0, 0, err
	}
	var holding, waiting []string
	for _, row := range rows {
		switch row.State {
		case Holding, Merging, Unknown:
			holding = append(holding, row.TurnID)
		case Waiting, MemberWaiting:
			waiting = append(waiting, row.TurnID)
		}
	}
	order := append(holding, waiting...)
	for i, id := range order {
		if id == live.TurnID {
			return i + 1, len(order), nil
		}
	}
	return 0, len(order), nil
}

// identityText says which pull request and relationship a claim or a request names, with any detail beside it.
func identityText(pr sql.NullInt64, relationship sql.NullString, extra string) string {
	var details []string
	hasRelationship := relationship.Valid && relationship.String != ""
	if hasRelationship {
		details = append(details, "relationship "+pyvalue.StrRepr(relationship.String))
	}
	if extra != "" {
		details = append(details, extra)
	}
	var subject string
	switch {
	case pr.Valid:
		subject = fmt.Sprintf("pull request %d", pr.Int64)
	case hasRelationship:
		subject, details = details[0], details[1:]
	default:
		subject = "no pull request or relationship"
	}
	if len(details) > 0 {
		return subject + " (" + strings.Join(details, ", ") + ")"
	}
	return subject
}

// claimedWith says which identity arguments the live claim was made with, which are the ones a repeat of it states.
func claimedWith(live store.MergeTurnsRow) string {
	hasPR := live.PRNumber.Valid
	hasRelationship := live.RelationshipID.Valid && live.RelationshipID.String != ""
	pr := fmt.Sprintf("--pr %d", live.PRNumber.Int64)
	relationship := "--relationship " + pyvalue.StrRepr(live.RelationshipID.String)
	switch {
	case hasPR && hasRelationship:
		return pr + " " + relationship
	case hasPR:
		return pr + " and no --relationship"
	case hasRelationship:
		return relationship + " and no --pr"
	}
	return "neither --pr nor --relationship"
}

// otherPullRequestRefusal is the refusal of a request that is not the repeat of the holder's live turn. It names the
// turn, what it is bound to, its place in the order and the step that frees the target, says whether the request
// contradicts the turn or only names an identity the turn does not record, and tells the caller how to repeat the claim.
func (s *Service) otherPullRequestRefusal(ctx context.Context, live store.MergeTurnsRow, asked ClaimOptions, actor string) (*registry.CoordinationRefusal, error) {
	place, of, err := s.livePlace(ctx, live)
	if err != nil {
		return nil, err
	}
	next := "return it with merge-turn-release"
	switch live.State {
	case Waiting, MemberWaiting:
		next = "withdraw it with merge-turn-withdraw"
	case Merging:
		next = "land it with merge-turn-land"
	case Unknown:
		next = "resolve it with merge-turn-resolve"
	}
	bound := identityText(live.PRNumber, live.RelationshipID, "candidate head "+pyvalue.StrRepr(live.CandidateHead))
	named := identityText(asked.PR, asked.Relationship, "")
	because := "A parent holds one turn per target and a request for another pull request is not answered with the turn it already has"
	if contradicted, unrecorded := compareClaim(live, asked); len(contradicted) == 0 {
		because = "The turn does not record the " + strings.Join(unrecorded, " and the ") + " this request names, so the request cannot be confirmed as a repeat of it, and a matching commit does not settle that because two pull requests can point at one commit; it is not answered with the turn"
	}
	detail := fmt.Sprintf("task %s already has the live merge turn %s on this target: it is %s at place %d of %d (the claims that hold the target first, then the waiting ones in the order they were made), bound to %s, and this request names %s. %s. Nothing was recorded for this request. To repeat this claim, repeat the request with the arguments the claim was made with (its identity arguments: %s); that does not add or change the identities it records. To request a turn with other or additional identities, %s, then request it with those identities.",
		pyvalue.StrRepr(actor), pyvalue.StrRepr(live.TurnID), live.State, place, of, bound, named, because, claimedWith(live), next)
	return coordination(live, contract.RefusalDispositionConflict, detail, live.TurnID, identityText(asked.PR, asked.Relationship, "")), nil
}
