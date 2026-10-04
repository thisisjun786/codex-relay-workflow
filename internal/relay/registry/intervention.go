package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A direct parent intervention is a message the relationship's registered parent sent to the child outside the routes the relay carries (a decision reply, a ruling), and then told the relay about by
// admitting the turn that received it (admit-turn). It is a journal row of the kind below, written in the transaction of the admission and read back by intervention-show. It records a statement: the
// relay does not authenticate --actor (the same holds for a continuation claim) and cannot see a message that was sent over the thread bridge and never admitted, which leaves no trace here.

// InterventionKind is the journal kind of a direct parent intervention and the kind its read-back prints.
const InterventionKind = "direct_parent_intervention"

// recordParentIntervention writes the intervention an admission of turn by actor states, when actor is the parent registered for the relationship. Any other actor is only an admission. The same
// statement again (relationship, generation, turn, anchor, actor and reason) is not written twice; a different reason is another statement.
func (r *Registry) recordParentIntervention(ctx context.Context, rid string, generation int64, turn, anchor, actor, reason, at string) error {
	q := r.Store.Querier(ctx)
	var parent string
	if err := q.QueryRowContext(ctx, "SELECT parent_task_id FROM relationships WHERE relationship_id = ?", rid).Scan(&parent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if actor != parent {
		return nil
	}
	detail := pyjson.Dumps(contract.OrderedObject{{Key: "relationship", Value: rid}, {Key: "generation", Value: generation}, {Key: "turn", Value: turn}, {Key: "anchor", Value: anchor},
		{Key: "actor", Value: actor}, {Key: "reason", Value: nullText(reason)}}, pyjson.Options{})
	var one int
	switch err := q.QueryRowContext(ctx, "SELECT 1 FROM journal WHERE kind = ? AND subject = ? AND detail = ? LIMIT 1", InterventionKind, rid, detail).Scan(&one); {
	case err == nil:
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	return journal(ctx, r.Store, InterventionKind, rid, detail, at)
}

// InterventionsOf is intervention-show: the direct parent interventions recorded for a relationship, oldest first. A read: it records and sends nothing.
func (r *Registry) InterventionsOf(ctx context.Context, rid string) (any, error) {
	relationship, err := r.Get(ctx, rid)
	if err != nil {
		return nil, err
	}
	rows, err := r.Store.All(ctx, "SELECT at, detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq", InterventionKind, rid)
	if err != nil {
		return nil, err
	}
	list := []any{}
	for _, row := range rows {
		var d struct {
			Generation int64   `json:"generation"`
			Turn       string  `json:"turn"`
			Anchor     string  `json:"anchor"`
			Actor      string  `json:"actor"`
			Reason     *string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(row.Text("detail")), &d); err != nil {
			return nil, err
		}
		var reason any
		if d.Reason != nil {
			reason = *d.Reason
		}
		list = append(list, contract.OrderedObject{{Key: "kind", Value: InterventionKind}, {Key: "generation", Value: d.Generation}, {Key: "turn", Value: d.Turn}, {Key: "anchorTurnId", Value: d.Anchor},
			{Key: "actor", Value: d.Actor}, {Key: "reason", Value: reason}, {Key: "recordedAt", Value: row.Text("at")}})
	}
	return contract.OrderedObject{{Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: relationship.Generation}, {Key: "parentTaskId", Value: relationship.Parent.TaskID},
		{Key: "interventions", Value: list}}, nil
}
