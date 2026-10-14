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
	// RecipientManagement: the owner of the management seat whose execution edge stands directly above the project (CRW-1314: the
	// management session bound the project owner); Thread names it. It hears every report kind of the project.
	RecipientManagement = "management"
	// RecipientStore: the store seat; the recipient of a decision only Jun can make when no management seat stands above the project, and of
	// the project's other reports when neither a management seat nor an initiative lead does (the walk folded the seat in). RelaySender
	// delivers to it through the supervisor channel like any other recipient (CRW-1380, D1c d2); ManageSender leaves it to the manager's
	// pump.
	RecipientStore = "store"
	// RecipientUnregistered: nobody stands above the project (no management seat, no initiative lead and no store seat), or the
	// management seat above it has no live owner.
	RecipientUnregistered = "unregistered_scope"
	// RecipientUnreadable: the linkage could not be read or is contested; Reason names which.
	RecipientUnreadable = "unreadable"
)

// Recipient is who a report goes to, as the linkage walk answers when the report is made.
type Recipient struct {
	State string
	// Thread is the management seat owner's thread for RecipientManagement, the initiative lead's thread (the task of its supervision
	// binding) for RecipientInitiative, the store seat's for RecipientStore when exactly one is registered.
	Thread        string
	InitiativeKey string
	ProjectKey    string
	// Reason is the refusal of the walk that left the report standing.
	Reason, Detail string
}

// Deliverable says whether the report goes to a thread through crw manage send-parent: the management session above the project, or a
// legacy initiative lead. A sender that answers Delivers (RelaySender) decides for itself.
func (r Recipient) Deliverable() bool {
	return (r.State == RecipientManagement || r.State == RecipientInitiative) && r.Thread != ""
}

// Resolver finds the recipient of a report of a run.
type Resolver interface {
	// Recipient answers for the project of the run's plan; kind is the report kind (a jun_decision_request goes to the management seat
	// above the project, else to the store seat).
	Recipient(ctx context.Context, plan, kind string) (Recipient, error)
}

// StoreResolver resolves through the supervisor package's linkage walk, read-only, as the supervisor channel finds the level above a
// notice for a project: the owner of the management seat directly above the project's live parent (CRW-1314: the management session bound
// the project owner), else the live supervisor of the initiative above it (the legacy lead), else the single store seat. It writes nothing
// to the relay store and reads the hierarchy as it is now, so a handover between two reports sends the second to the new owner.
//
// A decision only Jun can make goes to the same management seat when one stands above the project; otherwise it stays the store seat's,
// as it always was, because a lead does not decide for Jun (design CRW-1314 §5). Its resolution fails open: a plan or a walk that cannot be
// read leaves it with the store seat.
type StoreResolver struct{ Store *store.Store }

// Recipient implements Resolver.
func (r StoreResolver) Recipient(ctx context.Context, plan, kind string) (Recipient, error) {
	project, planErr := ProjectOf(ctx, r.Store, plan)
	if kind == KindJunDecisionRequest {
		if planErr == nil {
			if res, err := (&supervisor.Channel{Store: r.Store}).Resolve(ctx, "project:"+project); err == nil && res.RecipientScopeKind == RecipientManagement {
				return Recipient{State: RecipientManagement, Thread: res.Recipient, ProjectKey: project}, nil
			}
		}
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
	if planErr != nil {
		return Recipient{State: RecipientUnreadable, Reason: "plan_unknown", Detail: planErr.Error()}, nil
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
	switch res.RecipientScopeKind {
	case RecipientManagement:
		return Recipient{State: RecipientManagement, Thread: res.Recipient, ProjectKey: project}, nil
	case RecipientInitiative:
		return Recipient{State: RecipientInitiative, Thread: res.Recipient, InitiativeKey: res.InitiativeKey, ProjectKey: project}, nil
	case RecipientStore:
		// the walk folded the store seat in: the report is the seat's, and no bridge delivery goes to it
		return Recipient{State: RecipientStore, Thread: res.Recipient, ProjectKey: project}, nil
	}
	return Recipient{State: RecipientUnregistered, ProjectKey: project, Reason: "unregistered_scope",
		Detail: "nobody supervises project " + project + "; the walk named the " + res.RecipientScopeKind + " level, which takes no report"}, nil
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
