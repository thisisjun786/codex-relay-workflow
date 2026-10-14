package dagsched

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The record of the integration target (CRW-1304). The relay records which remote branch an integration ref is pushed to, in the
// append-only table dag_integration_targets, so the push and the checker compare a remote head with the heads of that ref alone, and
// so the heads read on the remote branch are kept for the receive and acknowledgement steps, which read the store and never the
// remote. A target is recorded by dag-integration-target-record, by the --remote and --remote-ref flags of dag-integrate, and by
// dag-integrate-push once it has read the remote head. A repeat of the newest record is a no-op and a change is a new row.
// Recording a target changes nothing the push or the batch does: a record that cannot be written is reported, never a refusal.

// PushActor and CheckActor are the actors the push and the checker record under; the batch and the record command record the actor they were given.
const (
	PushActor  = "dag-integrate-push"
	CheckActor = "dag-integration-target-check"
)

// TargetInput names the remote branch an integration ref of a checkout is pushed to.
type TargetInput struct {
	Checkout, IntegrationRef, Remote, RemoteRef string
	// Plan is the plan that records the target; empty names the plan of the newest head recorded for the integration ref.
	Plan string
	// Actor is who records it; Source is one of the store.TargetSource constants.
	Actor, Source string
}

// RefuseTargetInput is the refusal of an input the relay cannot read as a target: an empty name, a name git would read as an option,
// a checkout that is not an absolute path.
func RefuseTargetInput(in TargetInput) error {
	for what, value := range map[string]string{"checkout": in.Checkout, "integration ref": in.IntegrationRef, "remote": in.Remote, "remote ref": in.RemoteRef, "actor": in.Actor} {
		if strings.TrimSpace(value) == "" {
			return refuse(contract.RefusalMergeTargetUnreadable, "the %s of an integration target must not be empty", what)
		}
	}
	for what, value := range map[string]string{"checkout": in.Checkout, "integration ref": in.IntegrationRef, "remote": in.Remote, "remote ref": in.RemoteRef} {
		if strings.HasPrefix(value, "-") {
			return refuse(contract.RefusalMergeTargetUnreadable, "the %s %q must not start with a dash", what, value)
		}
	}
	if !filepath.IsAbs(in.Checkout) {
		return refuse(contract.RefusalMergeTargetUnreadable, "the checkout %q must be an absolute path", in.Checkout)
	}
	return nil
}

// RecordTarget records the target of an integration ref and answers the integration ref's current target row and whether the call appended one. The integration
// ref must be one the relay recorded a batch for: a name no intent carries is refused, because a record of a mistyped ref would stand for
// a branch nothing ever merges onto.
func (s *Scheduler) RecordTarget(ctx context.Context, in TargetInput) (store.IntegrationTargetRow, bool, error) {
	if err := RefuseTargetInput(in); err != nil {
		return store.IntegrationTargetRow{}, false, err
	}
	heads, plan, _, err := s.recordedTargetHeads(ctx, in.IntegrationRef)
	if err != nil {
		return store.IntegrationTargetRow{}, false, err
	}
	if len(heads) == 0 {
		return store.IntegrationTargetRow{}, false, refuse(contract.RefusalMergeTargetUnreadable, "%s is not an integration ref the relay recorded a batch for, so it has no target to record", in.IntegrationRef)
	}
	if in.Plan == "" {
		in.Plan = plan
	}
	row := store.IntegrationTargetRow{Kind: store.TargetKindTarget, Checkout: in.Checkout, IntegrationRef: in.IntegrationRef, Remote: in.Remote, RemoteRef: in.RemoteRef,
		PlanID: in.Plan, Source: in.Source, RecordedBy: in.Actor, RecordedAt: registry.SystemISO()}
	appended, err := store.RecordIntegrationTarget(ctx, s.Store, row)
	if err != nil {
		return row, appended, err
	}
	current, err := s.currentTarget(ctx, in.Checkout, in.IntegrationRef)
	if err != nil {
		return row, appended, err
	}
	return current, appended, nil
}

// currentTarget is the newest target row of an integration ref of a checkout.
func (s *Scheduler) currentTarget(ctx context.Context, checkout, integrationRef string) (store.IntegrationTargetRow, error) {
	targets, err := store.IntegrationTargets(ctx, s.Store)
	if err != nil {
		return store.IntegrationTargetRow{}, err
	}
	for _, t := range targets {
		if t.Checkout == checkout && t.IntegrationRef == integrationRef {
			return t, nil
		}
	}
	return store.IntegrationTargetRow{}, refuse(contract.RefusalMergeTargetUnreadable, "no target is recorded for %s in %s", integrationRef, checkout)
}

// ObserveTarget records the head read on a target's remote branch. It is a no-op while that head is the newest one recorded for the branch.
func (s *Scheduler) ObserveTarget(ctx context.Context, target store.IntegrationTargetRow, head, source, actor string) (bool, error) {
	target.Kind, target.ObservedHead, target.Source, target.RecordedBy, target.RecordedAt = store.TargetKindObserved, head, source, actor, registry.SystemISO()
	return store.RecordIntegrationTarget(ctx, s.Store, target)
}

// RecordPushRead is what dag-integrate-push records once it has read the remote head: the target of the integration ref and the head the
// remote branch holds after the push. It runs after the push compared the head, so the comparison of the first push of a ref is the one
// made without a record. An error is why the record could not be written; the push is what it was.
func (s *Scheduler) RecordPushRead(ctx context.Context, checkout, remote, remoteRef, integrationRef, head string) error {
	row, _, err := s.RecordTarget(ctx, TargetInput{Checkout: checkout, IntegrationRef: integrationRef, Remote: remote, RemoteRef: remoteRef, Actor: PushActor, Source: store.TargetSourcePush})
	if err != nil {
		return err
	}
	if head == "" {
		return nil
	}
	_, err = s.ObserveTarget(ctx, row, head, store.TargetSourcePush, PushActor)
	return err
}
