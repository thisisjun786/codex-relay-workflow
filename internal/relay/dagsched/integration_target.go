package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The integration target that moved outside the integration path (CRW-1301, decision A + D(i)). dag-integrate-push already
// reads the remote branch before it pushes; that head is compared with the heads the relay recorded for the integration ref:
// the base of every recorded intent and every head a ref_moved stage moved the ref onto. When no target is recorded for the
// push's integration ref, remote and remote ref, the heads recorded for the store's other integration refs count as well,
// because nothing says which remote branch a ref is pushed to and their push to the same remote branch is the integration path
// too (evaluation d1). When the target is recorded (integration_target_record.go, CRW-1304), the ref's own heads are the whole
// set: another ref's head on the target's remote branch is not on its path. A remote head outside the set means something other
// than the relay moved the target (r2 F0-1: a child pushed its own commit to dev). The push records a broken
// integration_target_moved fault in the fault ledger, whose blocking notice the daemon carries to the supervisor, and names
// it to the calling parent in its output. What the push does is unchanged: nothing is reverted, and a remote the branch does
// not contain is still refused with merge_base_mismatch. The periodic read of the remote is the separate one-shot checker
// (integration_target_check.go); a remote that cannot be read, or a remote branch that does not exist yet, is compared with
// nothing and records nothing.

// ClassIntegrationTargetMoved is the fault class of a remote integration target that moved outside the integration path.
const ClassIntegrationTargetMoved = "integration_target_moved"

// targetProduct is the product the relay's own faults are recorded under (the daemon sweeps the same one).
const targetProduct = "crw"

// expectedShown bounds how many recorded heads a push names; ExpectedTotal says how many there are.
const expectedShown = 8

// TargetMoved is what a push found when the remote branch held a head the relay never recorded for an integration ref.
type TargetMoved struct {
	IntegrationRef string
	RemoteHead     string
	// Expected are the recorded heads of the integration ref, newest first, at most expectedShown of ExpectedTotal.
	Expected      []string
	ExpectedTotal int
	// Strict says the integration ref has a recorded target, so Expected is the ref's own heads and no other ref's head counts as on its path.
	Strict bool
	// Source is what read the remote head: SourcePush, SourceCheck or SourceReceive.
	Source string
	// FaultID is the fault the ledger recorded; FaultError is why it could not be recorded, when it could not.
	FaultID    string
	FaultError string
}

// The sources of an integration_target_moved observation, kept in the fault's evidence.
const (
	SourcePush    = "push"
	SourceCheck   = "check"
	SourceReceive = "receive"
)

// TargetCheck compares the remote head a push read with the heads the relay recorded for the integration refs. It answers nil
// when the head is one of them.
type TargetCheck func(ctx context.Context, checkout, remote, remoteRef, integrationRef, remoteHead string) (*TargetMoved, error)

// CheckIntegrationTarget is the TargetCheck dag-integrate-push runs: a head the relay recorded for no integration ref of the
// store is recorded as an integration_target_moved fault, identified by the checkout, the remote and the remote ref, one
// occurrence per remote head, scoped to the project of the plan that recorded the integration ref's newest head. An error means
// the recorded heads could not be read; a ledger that refuses the fault is named in FaultError, and neither stops the push.
func (s *Scheduler) CheckIntegrationTarget(ctx context.Context, checkout, remote, remoteRef, integrationRef, remoteHead string) (*TargetMoved, error) {
	expected, plan, recorded, err := s.recordedTargetHeads(ctx, integrationRef)
	if err != nil {
		return nil, err
	}
	strict, err := store.IntegrationTargetRecorded(ctx, s.Store, checkout, integrationRef, remote, remoteRef)
	if err != nil {
		return nil, err
	}
	if strict {
		recorded = headSet(expected)
	}
	if len(expected) == 0 || recorded[remoteHead] {
		return nil, nil
	}
	out := &TargetMoved{IntegrationRef: integrationRef, RemoteHead: remoteHead, Expected: expected[:min(len(expected), expectedShown)], ExpectedTotal: len(expected), Strict: strict, Source: SourcePush}
	signature := map[string]any{"checkout": checkout, "remote": remote, "remoteRef": remoteRef}
	if id, err := s.recordTargetMoved(ctx, out, signature, plan); err != nil {
		out.FaultError = err.Error()
	} else {
		out.FaultID = id
	}
	return out, nil
}

// recordedTargetHeads reads the heads the relay recorded for an integration ref, newest first and each once, and the plan of
// the newest: the base of every intent of the ref and every head a ref_moved stage of its batches names. The ref is matched
// by name, as VerifiedMoveOnto matches it. recorded holds the same heads of every integration ref of the store.
func (s *Scheduler) recordedTargetHeads(ctx context.Context, integrationRef string) (heads []string, plan string, recorded map[string]bool, err error) {
	rows, err := s.Store.Q(ctx).QueryContext(ctx, "SELECT stage, batch_id, plan_id, detail FROM dag_integration_stages WHERE node_id = '' AND stage IN ('intent', 'ref_moved') ORDER BY recorded_at, rowid")
	if err != nil {
		return nil, "", nil, err
	}
	defer rows.Close()
	type stageRow struct{ stage, batch, plan, detail string }
	var all []stageRow
	batchRef := map[string]string{}
	for rows.Next() {
		var r stageRow
		if err := rows.Scan(&r.stage, &r.batch, &r.plan, &r.detail); err != nil {
			return nil, "", nil, err
		}
		if r.stage == "intent" {
			var intent map[string]string
			if json.Unmarshal([]byte(r.detail), &intent) == nil && intent["integration_ref"] != "" {
				batchRef[r.batch] = intent["integration_ref"]
				r.detail = intent["base"]
			} else {
				r.detail = ""
			}
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", nil, err
	}
	recorded = map[string]bool{}
	for i := len(all) - 1; i >= 0; i-- {
		r := all[i]
		if batchRef[r.batch] == "" || r.detail == "" {
			continue
		}
		recorded[r.detail] = true
		if batchRef[r.batch] != integrationRef {
			continue
		}
		if plan == "" {
			plan = r.plan
		}
		if !slices.Contains(heads, r.detail) {
			heads = append(heads, r.detail)
		}
	}
	return heads, plan, recorded, nil
}

// recordTargetMoved records one occurrence of the fault through the ledger, which raises its blocking notice when it opens, and
// answers the fault's id.
func (s *Scheduler) recordTargetMoved(ctx context.Context, m *TargetMoved, signature map[string]any, plan string) (string, error) {
	if err := faults.RegisterClass(ClassIntegrationTargetMoved, 0); err != nil {
		return "", err
	}
	scope := map[string]any{}
	var project string
	if has, err := queryOne(ctx, s.Store.Q(ctx), "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &project); err != nil {
		return "", err
	} else if has {
		scope["projectKey"] = project
	}
	detail := fmt.Sprintf("%s on %s holds %s, which is not a head the relay recorded for %s%s (newest recorded for %s: %s); something outside the integration path moved it", signature["remoteRef"], signature["remote"], m.RemoteHead, m.IntegrationRef, m.otherRefs(), m.IntegrationRef, strings.Join(m.Expected, ", "))
	shown := make([]any, len(m.Expected))
	for i, head := range m.Expected {
		shown[i] = head
	}
	ledger := &faults.Ledger{Store: s.Store, Clock: delivery.SystemClock{}}
	_, err := ledger.Record(ctx, faults.Observation{Product: targetProduct, FaultClass: ClassIntegrationTargetMoved, Severity: faults.Broken, Signature: signature,
		OccurrenceKey: "head:" + m.RemoteHead, Scope: scope, Detail: detail,
		Evidence: []any{map[string]any{"integrationRef": m.IntegrationRef, "remoteHead": m.RemoteHead, "expected": shown, "expectedTotal": int64(m.ExpectedTotal), "plan": plan, "source": m.Source}}})
	if err != nil {
		return "", err
	}
	return ledger.CanonicalID(ctx, targetProduct, ClassIntegrationTargetMoved, signature, "")
}

// nameTargetMoved adds the moved target to the detail of a refusal the push answers after it found one, so the parent that
// reads only the refusal still sees it.
func nameTargetMoved(err error, m *TargetMoved, remote, remoteRef string) error {
	var refused *store.RefusedError
	if m == nil || !errors.As(err, &refused) {
		return err
	}
	recorded := "fault " + m.FaultID + " recorded"
	if m.FaultID == "" {
		recorded = "the fault could not be recorded: " + m.FaultError
	}
	refused.Detail += fmt.Sprintf("; %s: %s on %s held %s, not a head the relay recorded for %s%s (%s)", ClassIntegrationTargetMoved, remoteRef, remote, m.RemoteHead, m.IntegrationRef, m.otherRefs(), recorded)
	return err
}

// otherRefs is the words that say the heads of the store's other integration refs count as on the path too: they do until the target is recorded.
func (m *TargetMoved) otherRefs() string {
	if m.Strict {
		return ""
	}
	return " or any other integration ref"
}

// headSet is the set of the heads in a list.
func headSet(heads []string) map[string]bool {
	set := make(map[string]bool, len(heads))
	for _, h := range heads {
		set[h] = true
	}
	return set
}
