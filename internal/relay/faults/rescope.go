package faults

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// rescope changes the owning scope and re-points the bounded set of unsent writes
// before the next observation is recorded.
func (l *Ledger) rescope(ctx context.Context, fault row, scope map[string]any, now string) error {
	_, err := l.rescopeCount(ctx, fault, scope, now)
	return err
}

func (l *Ledger) rescopeCount(ctx context.Context, fault row, scope map[string]any, now string) (int, error) {
	key := scopeKeyFor(text(fault, "product"), scope)
	other, err := l.one(ctx, "SELECT product FROM fault_ledger WHERE scope_key=? AND product!=? LIMIT 1", key, text(fault, "product"))
	if err != nil {
		return 0, err
	}
	if other == nil {
		other, err = l.one(ctx, "SELECT product FROM fault_target_projects WHERE scope_key=? AND product!=?", key, text(fault, "product"))
	}
	if err != nil {
		return 0, err
	}
	if other != nil {
		return 0, fmt.Errorf("fault_scope_conflict: scope key %s is already carried by product %s; one product's issues are never filed through another's key", pyvalue.Quote(key), pyvalue.Quote(text(other, "product")))
	}
	if _, err = l.exec(ctx, "UPDATE fault_ledger SET scope=?,scope_key=?,updated_at=? WHERE fault_id=?", dumps(scope, false), key, now, text(fault, "fault_id")); err != nil {
		return 0, err
	}
	id := text(fault, "fault_id")
	query, _, selected, _ := dRepointFaultQuery(id)
	query += " ORDER BY p.rowid LIMIT ?"
	selected = append(selected, 100)
	rows, err := l.Store.All(ctx, query, selected...)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		publication := text(r, "publication_id")
		if _, err = l.exec(ctx, "UPDATE fault_publications SET tracker_ref=?,updated_at=? WHERE publication_id=?", r.Get("team"), now, publication); err != nil {
			return 0, err
		}
		if _, err = l.exec(ctx, "UPDATE fault_publication_payloads SET project_ref=?,updated_at=? WHERE publication_id=?", r.Get("project"), now, publication); err != nil {
			return 0, err
		}
	}
	if text(fault, "external_ref") != "" {
		target, e := l.one(ctx, "SELECT project_ref FROM fault_target_projects WHERE scope_key=? AND product=?", key, text(fault, "product"))
		if e != nil {
			return 0, e
		}
		if target == nil || target.Get("project_ref") == nil {
			return len(rows), dUnlinkOne(ctx, l, id, now)
		}
		return len(rows), dRelinkOne(ctx, l, id, text(target, "project_ref"), now)
	}
	return len(rows), nil
}
