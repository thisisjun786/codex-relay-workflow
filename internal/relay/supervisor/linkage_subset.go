package supervisor

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Subset ported for todo 24; todo 26 owns and extends the linkage walk.
// This walk follows the live execution edges from the relationship's issue through
// its project to its initiative, rather than consulting frozen task ids.
type StoreLinkage struct{ Store *store.Store }

func (l StoreLinkage) Up(ctx context.Context, relationshipID string) (map[string]any, error) {
	reading := map[string]any{"state": "resolved", "readable": true, "levels": []any{}, "gaps": []any{}, "contention": []any{}}
	_, err := l.Store.RelationshipScope(ctx, relationshipID)
	if errors.Is(err, sql.ErrNoRows) {
		reading["state"] = "unregistered"
		return reading, nil
	}
	if err != nil {
		return map[string]any{"state": "unreadable", "readable": false, "levels": []any{}, "gaps": []any{}, "contention": []any{}, "detail": err.Error()}, nil
	}
	relation, err := l.Store.Relationship(ctx, relationshipID)
	if err != nil {
		return nil, err
	}
	kind, key := "issue", relation.IssueKey
	levels := make([]any, 0, 2)
	gaps := make([]any, 0)
	contention := make([]any, 0)
	seen := map[string]bool{}
	for key != "" && !seen[kind+":"+key] {
		seen[kind+":"+key] = true
		owners, err := l.Store.ScopeOwners(ctx, kind, key)
		if err != nil {
			return map[string]any{"state": "unreadable", "readable": false, "levels": []any{}, "gaps": []any{}, "contention": []any{}, "detail": err.Error()}, nil
		}
		var owner any
		if len(owners) == 1 {
			owner = map[string]any{"taskId": owners[0].TaskID}
		} else if len(owners) > 1 {
			candidates := make([]any, len(owners))
			for i, o := range owners {
				candidates[i] = o.TaskID
			}
			contention = append(contention, map[string]any{"contention": "competing_owners", "scopeKind": kind, "scopeKey": key, "candidates": candidates})
		}
		levels = append(levels, map[string]any{"scopeKind": kind, "scopeKey": key, "owner": owner, "depth": len(levels)})
		if owner == nil && len(owners) == 0 {
			role := map[string]string{"issue": "child", "project": "parent", "initiative": "supervisor"}[kind]
			gaps = append(gaps, map[string]any{"gap": kind + "_without_" + role, "scopeKind": kind, "scopeKey": key})
		}
		conflicts, err := l.Store.LinkageConflicts(ctx, kind, key)
		if err != nil {
			return nil, err
		}
		for _, c := range conflicts {
			contention = append(contention, map[string]any{"reason": c.Reason, "scopeKind": kind, "scopeKey": key})
		}
		directives, err := (&registry.Registry{Store: l.Store}).ContestedDirectives(ctx, kind, key)
		if err != nil {
			return nil, err
		}
		for _, directive := range directives {
			conflict := map[string]any{"contention": "instruction_conflict", "scopeKind": kind, "scopeKey": key}
			for _, field := range directive {
				if field.Key == "directiveId" || field.Key == "fromScopeKey" || field.Key == "digest" {
					conflict[field.Key] = field.Value
				}
			}
			contention = append(contention, conflict)
		}
		edges, err := l.Store.ExecutionLinksAbove(ctx, kind, key)
		if err != nil {
			return nil, err
		}
		if len(edges) > 1 {
			ids := make([]string, len(edges))
			for i, edge := range edges {
				ids[i] = edge.LinkID
			}
			contention = append(contention, map[string]any{"contention": "competing_parents", "scopeKind": kind, "scopeKey": key, "candidates": ids})
			reading["state"] = "ambiguous"
			break
		}
		if len(edges) == 0 {
			if kind == "project" {
				gaps = append(gaps, map[string]any{"gap": "no_supervisor", "scopeKind": kind, "scopeKey": key})
			}
			break
		}
		edge := edges[0]
		if len(owners) == 1 && owners[0].TaskID != edge.LowerTaskID {
			contention = append(contention, map[string]any{"contention": "owner_drift", "linkId": edge.LinkID, "recorded": edge.LowerTaskID, "live": owners[0].TaskID})
		}
		upper, err := l.Store.ScopeOwners(ctx, edge.UpperKind, edge.UpperKey)
		if err != nil {
			return nil, err
		}
		if len(upper) == 1 && upper[0].TaskID != edge.UpperTaskID {
			contention = append(contention, map[string]any{"contention": "owner_drift", "linkId": edge.LinkID, "recorded": edge.UpperTaskID, "live": upper[0].TaskID})
		}
		kind, key = edge.UpperKind, edge.UpperKey
	}
	reading["levels"] = levels
	reading["gaps"] = gaps
	reading["contention"] = contention
	for _, c := range contention {
		if c.(map[string]any)["contention"] == "competing_owners" {
			reading["state"] = "ambiguous"
		}
	}
	return reading, nil
}
