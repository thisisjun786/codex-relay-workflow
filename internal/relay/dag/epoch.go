package dag

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The coordinator epoch (contract 6.2). A parent session that is going to decide anything about a plan claims an epoch first: one row of
// dag_coordinator_claims, numbered one above the newest. Every DAG write that decides carries the epoch its session holds and checks it,
// inside the transaction that writes, against the newest claim (CheckCoordinatorEpoch). A session that was replaced, by a newer session of
// the same task or by another parent task, holds an epoch that is no longer the newest, and its writes are refused.

// Claim is one row of dag_coordinator_claims: who raised the plan to Epoch, under which parent binding, and in which session (SessionNonce
// is any text that names the session once and is never reused).
type Claim struct {
	PlanID          string
	Epoch           int64
	BindingID       string
	BindingRevision int64
	TaskID          string
	SessionNonce    string
	ClaimedAt       string
}

const claimColumns = "plan_id, epoch, binding_id, binding_revision, task_id, session_nonce, claimed_at"

func scanClaim(ctx context.Context, q Queryer, where string, args ...any) (Claim, bool, error) {
	var c Claim
	found, err := scanOne(ctx, q, "SELECT "+claimColumns+" FROM dag_coordinator_claims WHERE "+where, args, &c.PlanID, &c.Epoch, &c.BindingID, &c.BindingRevision, &c.TaskID, &c.SessionNonce, &c.ClaimedAt)
	if isMissingZone(err) {
		return Claim{}, false, nil
	}
	return c, found, err
}

// LatestClaim is the newest claim of the plan: the one that holds it now.
func LatestClaim(ctx context.Context, q Queryer, plan string) (Claim, bool, error) {
	return scanClaim(ctx, q, "plan_id = ? ORDER BY epoch DESC LIMIT 1", plan)
}

// ClaimByNonce is the claim a session made, whether or not it still holds the plan.
func ClaimByNonce(ctx context.Context, q Queryer, plan, nonce string) (Claim, bool, error) {
	return scanClaim(ctx, q, "plan_id = ? AND session_nonce = ?", plan, nonce)
}

// ClaimAt is the claim that raised the plan to epoch (found is false for an epoch nobody claimed, epoch 0 included).
func ClaimAt(ctx context.Context, q Queryer, plan string, epoch int64) (Claim, bool, error) {
	return scanClaim(ctx, q, "plan_id = ? AND epoch = ?", plan, epoch)
}

// liveParent is the parent binding of a project that a task holds and that is still the project's owner: active or paused, and not
// replaced. This is how the rest of the relay reads a live owner (the contract's SQL for the fence says status = 'active'; a paused binding
// is not a stale epoch, so it is not refused under that name).
const liveParent = "role = 'parent' AND scope_kind = 'project' AND scope_key = ? AND task_id = ? AND status IN ('active','paused') AND superseded_by IS NULL"

// LiveParentBinding is the live parent binding of project that task holds.
func LiveParentBinding(ctx context.Context, q Queryer, project, task string) (id string, revision int64, found bool, err error) {
	found, err = scanOne(ctx, q, "SELECT binding_id, revision FROM scope_bindings WHERE "+liveParent, []any{project, task}, &id, &revision)
	return
}

// StaleEpoch is the refusal of a write from a session that does not hold the plan's epoch (decision D-02: the second reason this feature
// adds to the relay's contract, beside plan_revision_conflict).
func StaleEpoch(format string, args ...any) error {
	return &store.RefusedError{Reason: string(contract.RefusalStaleCoordinatorEpoch), Detail: fmt.Sprintf(format, args...)}
}

// CheckCoordinatorEpoch is the fence. It is the first statement of every write that decides, inside the transaction that writes, so the
// answer and the write see one state of the store. A write names the epoch its session holds (expected) and who it is (actor):
//
//   - a plan nobody has claimed is unfenced, as every plan was before the fence existed: a write that names no epoch passes, and a write that
//     names one is refused (nobody holds it);
//   - once a claim exists the fence is strict: the epoch must be the newest, its claim must be the actor's, and the parent binding it was made
//     under must still be a live parent binding of the plan's project (the project of the plan's header or, for the first revision of a plan
//     that has no header yet, newProject).
//
// A refusal is stale_coordinator_epoch and writes nothing.
func CheckCoordinatorEpoch(ctx context.Context, q Queryer, plan, newProject, actor string, expected int64) error {
	latest, claimed, err := LatestClaim(ctx, q, plan)
	if err != nil {
		return err
	}
	if !claimed {
		if expected == 0 {
			return nil
		}
		return StaleEpoch("the request holds coordinator epoch %d and nobody has claimed an epoch of plan %s", expected, plan)
	}
	if expected != latest.Epoch {
		return StaleEpoch("the request holds coordinator epoch %d and plan %s is at epoch %d, claimed by task %s: a newer coordinator session holds the plan", expected, plan, latest.Epoch, latest.TaskID)
	}
	if latest.TaskID != actor {
		return StaleEpoch("epoch %d of plan %s is held by task %s, not by %s", latest.Epoch, plan, latest.TaskID, actor)
	}
	project, hasHeader, err := loadHeader(ctx, q, plan)
	if err != nil {
		return err
	}
	if !hasHeader {
		project = newProject
	}
	if project == "" {
		return nil
	}
	var one int
	live, err := scanOne(ctx, q, "SELECT 1 FROM scope_bindings WHERE binding_id = ? AND "+liveParent, []any{latest.BindingID, project, actor}, &one)
	if err != nil {
		return err
	}
	if !live {
		return StaleEpoch("task %s claimed epoch %d of plan %s under binding %s, which is not a live parent binding of project %s any more", actor, latest.Epoch, plan, latest.BindingID, project)
	}
	return nil
}
