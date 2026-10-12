package report

import (
	"context"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// The states of a report's recipient (the recipient_state member of a report row).
const (
	// RecipientInitiative: the live supervisor of the initiative above the project's live parent; Thread names it.
	RecipientInitiative = "initiative"
	// RecipientStore: the store seat; the recipient of a decision only Jun can make. No bridge delivery is made to it.
	RecipientStore = "store"
	// RecipientUnregistered: no initiative lead supervises the project (the walk found none, or folded in the store seat, which is not an
	// initiative lead and is never sent a report through the bridge).
	RecipientUnregistered = "unregistered_scope"
	// RecipientUnreadable: the linkage could not be read or is contested; Reason names which.
	RecipientUnreadable = "unreadable"
)

// Recipient is who a report goes to, as the linkage walk answers when the report is made.
type Recipient struct {
	State string
	// Thread is the initiative lead's thread (the task of its supervision binding) for RecipientInitiative, the store seat's for RecipientStore
	// when exactly one is registered.
	Thread        string
	InitiativeKey string
	ProjectKey    string
	// Reason is the refusal of the walk that left the report standing.
	Reason, Detail string
}

// Deliverable says whether the report goes to a thread through the bridge.
func (r Recipient) Deliverable() bool { return r.State == RecipientInitiative && r.Thread != "" }

// Resolver finds the recipient of a report of a run.
type Resolver interface {
	// Recipient answers for the project of the run's plan; kind is the report kind (a jun_decision_request goes to the store seat).
	Recipient(ctx context.Context, plan, kind string) (Recipient, error)
}

// StoreResolver resolves through the supervisor package's linkage walk, read-only: the project's live parent, the initiative above it and
// the initiative's live supervisor, as the supervisor channel finds the level above a notice for a project. It writes nothing to the relay
// store and reads the hierarchy as it is now, so a handover between two reports sends the second to the new supervisor.
type StoreResolver struct{ Store *store.Store }

// Recipient implements Resolver.
func (r StoreResolver) Recipient(ctx context.Context, plan, kind string) (Recipient, error) {
	if kind == KindJunDecisionRequest {
		rec := Recipient{State: RecipientStore}
		held, err := (&registry.Registry{Store: r.Store}).StoreScopeSupervisor(ctx)
		if err != nil {
			return rec, err
		}
		if len(held) == 1 {
			rec.Thread = fmt.Sprint(held[0].Get("taskId"))
		}
		return rec, nil
	}
	project, err := ProjectOf(ctx, r.Store, plan)
	if err != nil {
		return Recipient{State: RecipientUnreadable, Reason: "plan_unknown", Detail: err.Error()}, nil
	}
	res, err := (&supervisor.Channel{Store: r.Store}).Resolve(ctx, "project:"+project)
	if err != nil {
		var refusal supervisor.Refusal
		if errors.As(err, &refusal) {
			state := RecipientUnreadable
			if refusal.Reason == "unregistered_scope" {
				state = RecipientUnregistered
			}
			return Recipient{State: state, ProjectKey: project, Reason: refusal.Reason, Detail: refusal.Detail}, nil
		}
		return Recipient{}, err
	}
	if res.RecipientScopeKind != "initiative" {
		// the walk folded the store seat in: it is not the initiative lead, and no bridge delivery goes to it
		return Recipient{State: RecipientUnregistered, ProjectKey: project, Reason: "unregistered_scope",
			Detail: "no initiative supervises project " + project + "; the walk named the " + res.RecipientScopeKind + " seat, which is not an initiative lead"}, nil
	}
	return Recipient{State: RecipientInitiative, Thread: res.Recipient, InitiativeKey: res.InitiativeKey, ProjectKey: project}, nil
}

// ProjectOf is the project key of a plan.
func ProjectOf(ctx context.Context, st *store.Store, plan string) (string, error) {
	if plan == "" {
		return "", errors.New("the run pinned no plan")
	}
	snap, _, err := (&dag.Repo{Store: st}).Snapshot(ctx, plan, 0)
	if err != nil {
		return "", fmt.Errorf("plan %s: %w", plan, err)
	}
	if snap.ProjectKey == "" {
		return "", fmt.Errorf("plan %s names no project", plan)
	}
	return snap.ProjectKey, nil
}
