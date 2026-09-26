package capacity

import (
	"context"
	"database/sql"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const (
	scopeProject    = "project"
	scopeInitiative = "initiative"
	roleParent      = "parent"
	roleSupervisor  = "supervisor"
)

func isLive(status string) bool { return status == "active" || status == "paused" }
func owners(ctx context.Context, s *store.Store, kind, key, role string) ([]string, error) {
	rows, err := (&registry.Registry{Store: s}).Owners(ctx, kind, key)
	if err != nil {
		return nil, err
	}
	tasks := []string{}
	for _, row := range rows {
		if capacityField(row, "role") == role {
			tasks = append(tasks, capacityField(row, "taskId"))
		}
	}
	return tasks, nil
}
func capacityField(row contract.OrderedObject, key string) string {
	for _, field := range row {
		if field.Key == key {
			value, _ := field.Value.(string)
			return value
		}
	}
	return ""
}

type level struct {
	kind, key, owner string
	owned            bool
}

func above(ctx context.Context, s *store.Store, project string) (*level, error) {
	parents, err := owners(ctx, s, scopeProject, project, roleParent)
	if err != nil || len(parents) != 1 {
		return nil, err
	}
	result := (&registry.Registry{Store: s}).Up(ctx, registry.UpSelector{Task: sql.NullString{String: parents[0], Valid: true}, Scope: sql.NullString{String: project, Valid: true}})
	if capacityField(result, "state") != "resolved" {
		return nil, nil
	}
	for _, field := range result {
		if field.Key != "levels" {
			continue
		}
		for _, item := range field.Value.([]any) {
			entry := item.(contract.OrderedObject)
			if capacityField(entry, "scopeKind") != scopeInitiative {
				continue
			}
			l := &level{kind: scopeInitiative, key: capacityField(entry, "scopeKey")}
			for _, f := range entry {
				if f.Key == "owner" && f.Value != nil {
					l.owned = true
					l.owner = capacityField(f.Value.(contract.OrderedObject), "taskId")
				}
			}
			return l, nil
		}
	}
	return nil, nil
}
