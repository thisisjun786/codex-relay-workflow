package dagsched

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// What a command checks of the relationship and the parent it acts through. Three rules about the relationship stay in the commands that hold them, because the rules differ and a shared gate
// would have to take every difference as a parameter:
//
//   - a result is accepted, corrected or withdrawn only while its child's relationship is active and not superseded (accept.go, correction.go, withdraw.go); withdraw reads the relationship
//     after the replay lookup, which is its own order;
//   - a pull request is judged for merge, or asked a merge turn for, unless the relationship is paused, cancelled or superseded (mergeable in mergejudge.go): an archived relationship is
//     fine, the child's work ended and the pull request still has to land;
//   - an integration is observed unless the relationship is paused or cancelled (observableRelationship in integration.go). A superseded relationship is not refused there. That is how the
//     check read when the two landing rules were copies of each other, and folding duplicates does not change it; whether the two should agree is a decision for whoever owns the divergence.
//
// What every one of them shares is here.

// relationshipState is how a refusal names a relationship that is not usable: "superseded" when the registry replaced it, else its status.
func relationshipState(rel relRow) string {
	if rel.Superseded {
		return "superseded"
	}
	return rel.Status
}

// notParentHeldBy is the refusal of a task that is not the parent of a relationship, naming the task that holds it (accept, merge judgement, integration observation). The correction and the
// withdrawal say it shorter, with notParentOf in correction.go; the reason is the same.
func notParentHeldBy(actor string, rel relRow) error {
	return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the parent of relationship %s, which is held by %s", actor, rel.ID, rel.ParentTaskID)
}

// requireProjectParent refuses a task that is not the one registered parent of the project: a measurement, a decision and a plan's summary are the parent's, and a replaced parent is refused at once.
// capbasis.go (a declarer of the limit may also record), epoch.go (the live parent claims the epoch) and release_recovery.go (any registered parent that owns the slot closes a release) ask a
// different question and keep their own checks.
func requireProjectParent(ctx context.Context, q store.Querier, project, actor string) error {
	parents, err := projectParents(ctx, q, project)
	if err != nil {
		return err
	}
	if len(parents) != 1 || parents[0] != actor {
		return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the registered parent of project %s", actor, project)
	}
	return nil
}
