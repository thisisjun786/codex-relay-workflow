package faults

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

// SetTarget records the tracker and project for a product's scope. Existing
// unissued writes are repointed within the same transaction as the target.
func (l *Ledger) SetTarget(ctx context.Context, product, project, team, projectRef string) (map[string]any, error) {
	return l.SetWorkspaceTarget(ctx, product, "", project, team, projectRef)
}

func (l *Ledger) SetWorkspaceTarget(ctx context.Context, product, workspace, project, team, projectRef string) (map[string]any, error) {
	if !productName.MatchString(product) {
		return nil, fmt.Errorf("fault_observation_malformed: product %s is not a plain identifier (letters, digits, '.', '_', '-'); a ':' '@' or '|' would let one product's key read as another's", quote.Value(product))
	}
	for _, field := range []struct{ name, value string }{{"workspace", workspace}, {"project", project}, {"project_ref", projectRef}, {"team", team}} {
		if (field.value != "" || field.name == "team") && strings.TrimSpace(field.value) == "" {
			return nil, fmt.Errorf("fault_observation_malformed: %s is a non-blank string", field.name)
		}
	}
	key := scopeKeyFor(product, map[string]any{"projectKey": project})
	if workspace != "" {
		key = scopeKeyFor(product, map[string]any{"workspace": workspace, "projectKey": project})
	}
	changed := false
	stamp := l.Clock.ISO()
	var work map[string]any
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		other, err := l.one(ctx, "SELECT product FROM fault_ledger WHERE scope_key=? AND product!=? LIMIT 1", key, product)
		if err == nil && other == nil {
			other, err = l.one(ctx, "SELECT product FROM fault_target_projects WHERE scope_key=? AND product!=?", key, product)
		}
		if err != nil {
			return err
		}
		if other != nil {
			return fmt.Errorf("fault_scope_conflict: scope key %s is already carried by product %s; one product's issues are never filed through another's key", quote.Value(key), quote.Value(text(other, "product")))
		}
		current, err := l.one(ctx, "SELECT t.tracker_ref,p.product,p.project_ref FROM fault_targets t LEFT JOIN fault_target_projects p ON p.scope_key = t.scope_key WHERE t.scope_key = ?", key)
		if err != nil {
			return err
		}
		changed = current == nil || text(current, "tracker_ref") != team || text(current, "product") != product || text(current, "project_ref") != projectRef
		if changed {
			if _, err = l.exec(ctx, "INSERT INTO fault_targets(scope_key,tracker_ref,recorded_at) VALUES(?,?,?) ON CONFLICT(scope_key) DO UPDATE SET tracker_ref = excluded.tracker_ref, recorded_at = excluded.recorded_at", key, team, stamp); err != nil {
				return err
			}
			if _, err = l.exec(ctx, "INSERT INTO fault_target_projects(scope_key,product,project_ref,recorded_at) VALUES(?,?,?,?) ON CONFLICT(scope_key) DO UPDATE SET product = excluded.product, project_ref = excluded.project_ref, recorded_at = excluded.recorded_at", key, product, nilIfEmpty(projectRef), stamp); err != nil {
				return err
			}
		}
		limit := 0
		if changed {
			limit = 100
		}
		work, err = dRelinkWork(ctx, l, key, limit, stamp)
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"scopeKey": key, "product": product, "team": team, "projectRef": nilIfEmpty(projectRef), "changed": changed, "backfilled": int64(work["backfilled"].(int)), "backfillPending": work["backfillPending"], "relinked": work["relinked"], "relinkPending": work["relinkPending"]}, nil
}
