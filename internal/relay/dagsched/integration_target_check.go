package dagsched

import (
	"context"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The one-shot check of the recorded integration targets (CRW-1304). dag-integration-target-check reads, for every integration ref
// that has a recorded target, the head of the target's remote branch with the same git calls the push uses (remoteURLForPush and
// lsRemoteHead), and compares it with the heads recorded for that integration ref alone: the base of every recorded intent and every
// head a ref_moved stage moved the ref onto. A head outside that set records the broken integration_target_moved fault of CRW-1301
// (one occurrence per remote head, so a head seen again adds nothing and raises no second blocking notice) and names it in the
// reading. Every head that was read is stored as an observed row of the target, which the receive and acknowledgement steps read
// without a network call. The command runs when cron, the coordinator or the integrator runs it; the daemon's fault sweep makes no
// network call and never runs it (docs/relay/faults.md).
//
// The check fails open. It never reverts the remote, never pushes and blocks nothing: a remote that cannot be reached, a remote
// branch that does not exist and a remote git cannot read are deferred (a reading with that status, no fault), and a fault or an
// observation that cannot be written is named in the reading. Only an input error is a refusal: a name that starts with a dash and a
// filter that names no recorded target.

// The statuses of one reading.
const (
	// TargetOnPath: the remote head is one of the heads recorded for the integration ref.
	TargetOnPath = "on_path"
	// TargetMovedOutside: the remote head is none of them; the fault is recorded.
	TargetMovedOutside = "moved"
	// TargetUnreachable: the remote could not be reached; nothing is compared and nothing is recorded.
	TargetUnreachable = "unreachable"
	// TargetAbsent: the remote answered and holds no such branch; nothing is compared.
	TargetAbsent = "absent"
	// TargetUnreadable: git answered with a refusal (a missing checkout, an unknown remote); nothing is compared.
	TargetUnreadable = "unreadable"
	// TargetNoHeads: the relay holds no recorded head for the integration ref, so the remote head has nothing to be compared with.
	TargetNoHeads = "no_recorded_heads"
)

// targetReadTimeout bounds one read of a remote, so a remote that does not answer is deferred like one that cannot be reached.
const targetReadTimeout = 60 * time.Second

// TargetReading is what the check found for one recorded target.
type TargetReading struct {
	Target     store.IntegrationTargetRow
	Status     string
	RemoteHead string
	// Detail is why the remote was not compared, when it was not.
	Detail string
	// Moved is set when Status is TargetMovedOutside; it carries the fault's id, or why the fault could not be recorded.
	Moved *TargetMoved
	// Observed says the head was stored as a new observed row; ObservationError is why it could not be.
	Observed         bool
	ObservationError string
}

// CheckRecordedTargets reads the remote branch of every recorded target, or of the targets of one integration ref and one checkout
// when those are given, and answers one reading per target in the order the targets were first recorded. A filter that names no
// recorded target is refused, so is a name that starts with a dash.
func (s *Scheduler) CheckRecordedTargets(ctx context.Context, checkout, integrationRef string) ([]TargetReading, error) {
	for what, value := range map[string]string{"checkout": checkout, "integration ref": integrationRef} {
		if strings.HasPrefix(value, "-") {
			return nil, refuse(contract.RefusalMergeTargetUnreadable, "the %s %q must not start with a dash", what, value)
		}
	}
	all, err := store.IntegrationTargets(ctx, s.Store)
	if err != nil {
		return nil, err
	}
	var targets []store.IntegrationTargetRow
	for _, t := range all {
		if (checkout == "" || t.Checkout == checkout) && (integrationRef == "" || t.IntegrationRef == integrationRef) {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 && (checkout != "" || integrationRef != "") {
		return nil, refuse(contract.RefusalMergeTargetUnreadable, "no integration target is recorded for the integration ref %q in the checkout %q", integrationRef, checkout)
	}
	readings := make([]TargetReading, 0, len(targets))
	for _, t := range targets {
		readings = append(readings, s.readTarget(ctx, t))
	}
	return readings, nil
}

// readTarget reads one target's remote branch and records what it found.
func (s *Scheduler) readTarget(ctx context.Context, t store.IntegrationTargetRow) TargetReading {
	out := TargetReading{Target: t}
	readCtx, cancel := context.WithTimeout(ctx, targetReadTimeout)
	defer cancel()
	head, unreachable, err := lsRemoteHead(readCtx, t.Checkout, remoteURLForPush(readCtx, t.Checkout, t.Remote), t.RemoteRef)
	if err != nil {
		out.Status, out.Detail = TargetUnreadable, err.Error()
		if unreachable {
			out.Status = TargetUnreachable
		}
		return out
	}
	if head == "" {
		out.Status, out.Detail = TargetAbsent, t.RemoteRef+" is not a branch of "+t.Remote
		return out
	}
	out.RemoteHead = head
	expected, plan, _, err := s.recordedTargetHeads(ctx, t.IntegrationRef)
	switch {
	case err != nil:
		out.Status, out.Detail = TargetUnreadable, "the recorded heads could not be read: "+err.Error()
	case len(expected) == 0:
		out.Status = TargetNoHeads
	case headSet(expected)[head]:
		out.Status = TargetOnPath
	default:
		out.Status = TargetMovedOutside
		out.Moved = &TargetMoved{IntegrationRef: t.IntegrationRef, RemoteHead: head, Expected: expected[:min(len(expected), expectedShown)], ExpectedTotal: len(expected), Strict: true, Source: SourceCheck}
		if t.PlanID != "" {
			plan = t.PlanID
		}
		signature := map[string]any{"checkout": t.Checkout, "remote": t.Remote, "remoteRef": t.RemoteRef}
		if id, err := s.recordTargetMoved(ctx, out.Moved, signature, plan); err != nil {
			out.Moved.FaultError = err.Error()
		} else {
			out.Moved.FaultID = id
		}
	}
	if out.Observed, err = s.ObserveTarget(ctx, t, head, store.TargetSourceCheck, CheckActor); err != nil {
		out.ObservationError = err.Error()
	}
	return out
}
