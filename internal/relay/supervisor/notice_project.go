package supervisor

import (
	"context"
	"database/sql"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

func (c *Channel) resolveNoticeProject(ctx context.Context, key string) (Resolution, error) {
	where := "project " + pyvalue.StrRepr(key)
	owners, err := c.Store.ScopeOwners(ctx, "project", key)
	if err != nil {
		return Resolution{}, Refusal{"relation_unreadable", "the linkage could not be read for " + where + " (OperationalError), so who the level above is is unknown; nothing is staged"}
	}
	if len(owners) == 0 {
		return Resolution{}, Refusal{"unregistered_scope", where + " has no live project owner, so there is nobody whose report this would be; the notification waits"}
	}
	if len(owners) > 1 {
		names := []string{}
		for _, o := range owners {
			names = append(names, pyvalue.StrRepr(o.TaskID))
		}
		return Resolution{}, Refusal{"duplicate_scope_owner", where + " has more than one live owner (" + strings.Join(names, ", ") + "); this sender will not choose between them"}
	}
	record := (&registry.Registry{Store: c.Store}).Up(ctx, registry.UpSelector{Task: sql.NullString{String: owners[0].TaskID, Valid: true}, Scope: sql.NullString{String: key, Valid: true}})
	reading := orderedMap(record)
	levels := evidence.Iter(reading["levels"])
	if reading["readable"] == true && (len(levels) == 0 || evidence.Item(levels[0], "scopeKind") != "project" || evidence.Item(levels[0], "scopeKey") != key) {
		return Resolution{}, Refusal{"unregistered_scope", "the linkage walk for " + where + " did not start at that project, so it names no level above it; the notification waits"}
	}
	return resolveReading(reading, where)
}
