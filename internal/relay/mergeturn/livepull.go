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

// compareClaim reads what a request states against what the holder's live claim records, one kind of identity at a
// time (the pull request number, the relationship id): whether the request states any, whether a stated one agrees with
// the recorded one of its kind, and whether one contradicts it (recorded and different).
func compareClaim(live store.MergeTurnsRow, asked ClaimOptions) (stated, agreed, contradicted bool) {
	if asked.PR.Valid {
		stated = true
		if live.PRNumber.Valid {
			if live.PRNumber.Int64 != asked.PR.Int64 {
				contradicted = true
			} else {
				agreed = true
			}
		}
	}
	if asked.Relationship.Valid && asked.Relationship.String != "" {
		stated = true
		if live.RelationshipID.Valid && live.RelationshipID.String != "" {
			if live.RelationshipID.String != asked.Relationship.String {
				contradicted = true
			} else {
				agreed = true
			}
		}
	}
	return stated, agreed, contradicted
}

// requestIsLiveClaim is whether a request is the replay of the holder's live claim. A request that states no pull
// request and no relationship is, as it always was. One that states some is the replay when none contradicts what the
// claim records and at least one agrees. Identities the claim does not record at all cannot be told from another pull
// request's, so they are the replay only when the request names the claim's own candidate head: two pull requests do
// not share a head, and a request for another one names its own. dag-merge-request reads the answer it gets with the
// same suspicion.
func requestIsLiveClaim(live store.MergeTurnsRow, asked ClaimOptions, head string) bool {
	stated, agreed, contradicted := compareClaim(live, asked)
	return !contradicted && (!stated || agreed || SameCommit(head, live.CandidateHead))
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
		case Waiting:
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

// otherPullRequestRefusal is the refusal of a request that is not the replay of the holder's live turn. It names the
// turn, what it is bound to, its place in the order and the step that frees the target, and says whether the request
// contradicts the turn or only names an identity the turn does not record.
func (s *Service) otherPullRequestRefusal(ctx context.Context, live store.MergeTurnsRow, asked ClaimOptions, head, actor string) (*registry.CoordinationRefusal, error) {
	place, of, err := s.livePlace(ctx, live)
	if err != nil {
		return nil, err
	}
	next := "return it with merge-turn-release"
	switch live.State {
	case Waiting:
		next = "withdraw it with merge-turn-withdraw"
	case Merging:
		next = "land it with merge-turn-land"
	case Unknown:
		next = "resolve it with merge-turn-resolve"
	}
	bound := identityText(live.PRNumber, live.RelationshipID, "candidate head "+pyvalue.StrRepr(live.CandidateHead))
	named := identityText(asked.PR, asked.Relationship, "")
	because := "A parent holds one turn per target and a request for another pull request is not answered with the turn it already has"
	if _, _, contradicted := compareClaim(live, asked); !contradicted {
		because = "The turn does not record what this request names and the request states head " + pyvalue.StrRepr(head) + " instead of the turn's candidate head, so it cannot be told from a request for another pull request and is not answered with the turn; repeat the request with the arguments the claim was made with"
	}
	detail := fmt.Sprintf("task %s already has the live merge turn %s on this target: it is %s at place %d of %d (the claims that hold the target first, then the waiting ones in the order they were made), bound to %s, and this request names %s. %s. Nothing was recorded for this request. To get the turn of another pull request, %s, then request it.",
		pyvalue.StrRepr(actor), pyvalue.StrRepr(live.TurnID), live.State, place, of, bound, named, because, next)
	return coordination(live, contract.RefusalDispositionConflict, detail, live.TurnID, identityText(asked.PR, asked.Relationship, "")), nil
}
