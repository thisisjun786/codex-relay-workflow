package supervisor

import (
	"context"
	"database/sql"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A recipient's queue read whole (CRW-1486/S4, finding 9). List only reads.

// ListRequest is which of a recipient's messages List reads (store.SupervisorListFilter's words:
// Project empty, none or a project key; State unsent, pending or all; Hold empty, none or a hold
// word).
type ListRequest struct {
	Recipient, Project, State, Hold string
}

// List is a recipient's messages, in sending order, with their counts by state, hold, project and
// obligation kind. A message with no project counts under none, the set --project none lists.
// claimable on a row is the channel's own test at now (store.SupervisorClaimableSQL
// for a row already read): unsent, no hold, due.
func (c *Channel) List(ctx context.Context, req ListRequest, now float64) (map[string]any, error) {
	if req.State == "" {
		req.State = store.SupervisorListUnsent
	}
	rows, err := c.Store.ListSupervisorMessages(ctx, store.SupervisorListFilter{Recipient: req.Recipient, Project: req.Project, State: req.State, Hold: req.Hold})
	if err != nil {
		return nil, err
	}
	listed := make([]any, 0, len(rows))
	byState, byHold, byProject, byKind := map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{}
	count := func(into map[string]any, key string) {
		n, _ := into[key].(int)
		into[key] = n + 1
	}
	for _, row := range rows {
		listed = append(listed, map[string]any{
			"messageId":      row.MessageID,
			"relationshipId": row.RelationshipID,
			"projectKey":     nullText(row.ProjectKey),
			"kind":           row.Kind,
			"obligationKind": row.ObligationKind,
			"subject":        row.Subject,
			"state":          row.State,
			"holdReason":     nullText(row.HoldReason),
			"stagedAt":       row.StagedAt,
			"nextEligibleAt": nullNumber(row.NextEligibleAt),
			"claimable":      row.ClaimableAt(now),
		})
		count(byState, row.State)
		count(byHold, textOr(row.HoldReason, store.SupervisorListHoldNone))
		count(byProject, textOr(row.ProjectKey, store.SupervisorListProjectNone))
		count(byKind, row.ObligationKind)
	}
	answer := map[string]any{
		"schema":    "supervisor-list/1",
		"recipient": req.Recipient,
		"project":   emptyNull(req.Project),
		"state":     req.State,
		"hold":      emptyNull(req.Hold),
		"rows":      listed,
		"counts":    map[string]any{"total": len(rows), "byState": byState, "byHold": byHold, "byProject": byProject, "byObligationKind": byKind},
	}
	return answer, nil
}

func nullText(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func textOr(value sql.NullString, otherwise string) string {
	if !value.Valid {
		return otherwise
	}
	return value.String
}

func nullNumber(value sql.NullFloat64) any {
	if !value.Valid {
		return nil
	}
	return value.Float64
}

func emptyNull(value string) any {
	if value == "" {
		return nil
	}
	return value
}
