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

// requestIsLiveClaim is whether a request is the replay of the holder's live claim. A request that states no pull
// request and no relationship is, as it always was. One that states some is the replay only when no stated identity
// contradicts what the claim records (the same kind, recorded and different) and at least one agrees: identities the
// claim does not record at all cannot be told from another pull request's, which is how dag-merge-request already
// reads the answer it gets.
func requestIsLiveClaim(live store.MergeTurnsRow, asked ClaimOptions) bool {
	stated, agreed := false, false
	if asked.PR.Valid {
		stated = true
		if live.PRNumber.Valid {
			if live.PRNumber.Int64 != asked.PR.Int64 {
				return false
			}
			agreed = true
		}
	}
	if asked.Relationship.Valid && asked.Relationship.String != "" {
		stated = true
		if live.RelationshipID.Valid && live.RelationshipID.String != "" {
			if live.RelationshipID.String != asked.Relationship.String {
				return false
			}
			agreed = true
		}
	}
	return !stated || agreed
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

// identityText says which pull request and relationship a claim or a request names.
func identityText(pr sql.NullInt64, relationship sql.NullString, extra string) string {
	var parts []string
	if relationship.Valid && relationship.String != "" {
		parts = append(parts, "relationship "+pyvalue.StrRepr(relationship.String))
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	switch {
	case pr.Valid && len(parts) > 0:
		return fmt.Sprintf("pull request %d (%s)", pr.Int64, strings.Join(parts, ", "))
	case pr.Valid:
		return fmt.Sprintf("pull request %d", pr.Int64)
	case len(parts) > 0:
		return strings.Join(parts, ", ")
	}
	return "no pull request or relationship"
}

// otherPullRequestRefusal is the refusal of a request for another pull request than the one the holder's live turn is
// bound to. It names the turn, what it is bound to, its place in the order and the step that frees the target.
func (s *Service) otherPullRequestRefusal(ctx context.Context, live store.MergeTurnsRow, asked ClaimOptions, actor string) (*registry.CoordinationRefusal, error) {
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
	detail := fmt.Sprintf("task %s already has the live merge turn %s on this target: it is %s at place %d of %d (the claims that hold the target first, then the waiting ones in the order they were made), bound to %s, and this request names %s. A parent holds one turn per target and a request for another pull request is not answered with the turn it already has; nothing was recorded for this request. %s, then request the turn for the other pull request.",
		pyvalue.StrRepr(actor), pyvalue.StrRepr(live.TurnID), live.State, place, of, bound, named, strings.ToUpper(next[:1])+next[1:])
	return coordination(live, contract.RefusalDispositionConflict, detail, live.TurnID, identityText(asked.PR, asked.Relationship, "")), nil
}
