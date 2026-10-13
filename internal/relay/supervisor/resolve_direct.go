package supervisor

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// The recipients of the direct messages (CRW-1333, design CRW-1314 §2).

// scopeKindManagement is a management session's own seat: the level a lead's message goes to.
const scopeKindManagement = "management"

// scopeKindRun is the kind a run's owner is read from: flow_runs.actor_task_id, not a linkage level.
const scopeKindRun = "run"

// resolveInitiative is 'initiative:KEY': the sender is the initiative's live owner (its lead), and
// the recipient is the owner of the management seat whose execution edge stands above the
// initiative. An initiative with no management edge falls back to the single store seat, as a
// project with no initiative supervisor does; a management edge whose seat has no live owner is
// refused unregistered_scope rather than falling back, because the seat that should hear it exists.
func (c *Channel) resolveInitiative(ctx context.Context, key string) (Resolution, error) {
	where := "initiative " + pyvalue.StrRepr(key)
	owners, err := c.Store.ScopeOwners(ctx, scopeKindInitiative, key)
	if err != nil {
		return Resolution{}, Refusal{"relation_unreadable", "the linkage could not be read for " + where + " (" + err.Error() + "), so who the level above is is unknown; nothing is staged"}
	}
	if len(owners) == 0 {
		return Resolution{}, Refusal{"unregistered_scope", where + " has no live owner, so there is nobody whose message this would be; the message waits"}
	}
	if len(owners) > 1 {
		return Resolution{}, Refusal{"duplicate_scope_owner", where + " has more than one live owner; this sender will not choose between them"}
	}
	lead := owners[0].TaskID
	record := (&registry.Registry{Store: c.Store}).Up(ctx, registry.UpSelector{Task: sql.NullString{String: lead, Valid: true}, Scope: sql.NullString{String: key, Valid: true}})
	reading := orderedMap(record)
	if err := settledReading(reading, where); err != nil {
		return Resolution{}, err
	}
	levels := evidence.Iter(reading["levels"])
	if len(levels) == 0 || evidence.Item(levels[0], "scopeKind") != scopeKindInitiative || evidence.Item(levels[0], "scopeKey") != key {
		return Resolution{}, Refusal{"unregistered_scope", "the linkage walk for " + where + " did not start at that initiative, so it names no level above it; the message waits"}
	}
	result := Resolution{Sender: lead, InitiativeKey: key, Source: "linkage"}
	if len(levels) > 1 {
		level, _ := levels[1].(map[string]any)
		if level["scopeKind"] == scopeKindManagement {
			owner := levelOwner(level)
			if owner == "" {
				return Resolution{}, Refusal{"unregistered_scope", "the management seat " + pyvalue.StrRepr(pyvalue.Str(level["scopeKey"])) + " above " + where +
					" has no live owner, so there is nobody to tell; the message waits for the seat's handover"}
			}
			result.Recipient, result.RecipientScopeKind = owner, scopeKindManagement
			return result, nil
		}
	}
	held, err := (&registry.Registry{Store: c.Store}).StoreScopeSupervisor(ctx)
	if err != nil {
		return Resolution{}, Refusal{"relation_unreadable", "the store seat could not be read for " + where + ", so who the level above is is unknown; nothing is staged"}
	}
	if len(held) > 1 {
		candidates := make([]any, len(held))
		for i, one := range held {
			candidates[i] = one.Get("taskId")
		}
		return Resolution{}, Refusal{"duplicate_scope_owner", "the store scope has more than one live supervisor above " + where + "; this sender will not choose between them: " + pythonRepr(candidates)}
	}
	if len(held) == 0 {
		return Resolution{}, Refusal{"unregistered_scope", "no management seat stands above " + where + " and the store has no seat, so there is nobody to tell; the message waits"}
	}
	result.Recipient, result.RecipientScopeKind = pyvalue.Str(held[0].Get("taskId")), scopeKindStore
	return result, nil
}

// resolveRun is 'run:ID': the sender is the run, and the recipient is the thread that owns it
// (flow_runs.actor_task_id, the orchestrator that started it).
func (c *Channel) resolveRun(ctx context.Context, id string) (Resolution, error) {
	where := "run " + pyvalue.StrRepr(id)
	var actor string
	err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT actor_task_id FROM flow_runs WHERE run_id = ?", id).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return Resolution{}, Refusal{"unregistered_scope", where + " is not in this store, so it has no owner to tell; the message waits"}
	}
	if err != nil {
		return Resolution{}, Refusal{"relation_unreadable", "the run record could not be read for " + where + " (" + err.Error() + "); nothing is staged"}
	}
	return Resolution{Sender: id, Recipient: actor, RecipientScopeKind: scopeKindRun, Source: "flow_runs"}, nil
}
