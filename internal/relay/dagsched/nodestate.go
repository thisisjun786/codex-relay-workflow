package dagsched

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Derived node states (contract 3.1). A state is read from the rows, never stored.
const (
	StatePlanned         = "planned"
	StateReleasing       = "releasing"
	StateCreationUnknown = "creation_unknown"
	StateRunning         = "running"
	StateReported        = "reported"
	StateVerifying       = "verifying"
	StateCorrecting      = "correcting"
	StateAccepted        = "accepted"
	StateIntegrated      = "integrated"
	StatePausedNode      = "paused"
	StateCancelled       = "cancelled"
	StateClosedNode      = "closed"
	StateAmbiguousNode   = "ambiguous"
)

// nodeState is what the execution records say of one node. An owned node has a release or an execution: it is not a candidate, and Disp/Reason
// say what the reading reports for it.
type nodeState struct {
	State  string
	Owned  bool
	Disp   string
	Reason string
	Detail string
	Acc    Acceptance
	HasAcc bool
	// Holds is whether the node's edit regions are in use: an implementation node with a live execution, or an accepted head that has not landed.
	Holds bool
}

func ownedAs(state, reason, detail string, holds bool) nodeState {
	return nodeState{State: state, Owned: true, Disp: dispositionOf(reason), Reason: reason, Detail: detail, Holds: holds}
}

// dispositionOf is the class of a closed reason: the text before its first colon.
func dispositionOf(reason string) string {
	class, _, _ := strings.Cut(reason, ":")
	return class
}

// stateOf reads a node's execution records: nothing (unowned), a release whose child is not yet bound, or a bound execution.
func (s *Scheduler) stateOf(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) (nodeState, error) {
	var bound int
	if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM dag_node_executions WHERE plan_id = ? AND node_id = ?", []any{plan, n.NodeID}, &bound); err != nil {
		return nodeState{}, err
	}
	if bound == 0 {
		return s.unbound(ctx, q, plan, n)
	}
	return s.bound(ctx, q, plan, snap, n)
}

// unbound is a node with no execution: either nobody has touched it, or a release was decided and its child is not yet bound to it.
func (s *Scheduler) unbound(ctx context.Context, q store.Querier, plan string, n dag.SnapNode) (nodeState, error) {
	// the node's open intent: a release whose managed request was not closed explicitly (dag-release-close); a closed one no longer owns the node
	open, found, err := latestRelease(ctx, q, plan, n.NodeID)
	if err != nil {
		return nodeState{}, err
	}
	if !found {
		return nodeState{State: StatePlanned}, nil
	}
	digest, request := open.Digest, open.Request
	var state string
	var receipt *string
	managed, err := queryOne(ctx, q, "SELECT state, receipt_status FROM managed_start_requests WHERE request_id = ?", []any{request}, &state, &receipt)
	if err != nil {
		return nodeState{}, err
	}
	switch {
	case !managed:
		return ownedAs(StateReleasing, SkipAlreadyOwned, "a release is decided (manifest "+short(digest)+") and the managed start has not begun", true), nil
	case state == "released":
		return ownedAs(StateReleasing, BlockedReleaseAbandoned, "the managed start "+request+" was released before it created a child; dag-release-close --request-id "+request+" ends the intent and returns its slot, and the node can then be released again", false), nil
	case state == "create_armed" && (receipt == nil || *receipt != "accepted"):
		return ownedAs(StateCreationUnknown, BlockedCreationUnknown, "the creation of the child for "+request+" was armed and its outcome is not known; a repeat of the release reconciles it", true), nil
	}
	return ownedAs(StateReleasing, SkipAlreadyOwned, "a release is decided (manifest "+short(digest)+") and its child is not yet bound to the node", true), nil
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// bound is a node with at least one execution: the relationship it stands on, the relay's state word for it and the acceptance, when there is one.
func (s *Scheduler) bound(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) (nodeState, error) {
	rel, found, err := currentRelationshipOf(ctx, q, plan, n.NodeID)
	if err != nil {
		return nodeState{}, err
	}
	if !found {
		return ownedAs(StateAmbiguousNode, BlockedAmbiguousHead, "an execution is recorded for a relationship the store does not hold", false), nil
	}
	implementation := n.Kind == dag.NodeImplementation
	acc, hasAcc, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
	if err != nil {
		return nodeState{}, err
	}
	if hasAcc {
		return s.accepted(ctx, q, plan, snap, n, acc, rel, implementation)
	}
	// The relay's state word for the relationship. The view is told the time is always zero: the word does not depend on it, and the reading, which
	// reads no clock, throws away the time-based projection the view also builds.
	view := registry.NewAssignmentView(&registry.Registry{Store: s.Store})
	view.Clock = func() float64 { return 0 }
	answer, err := view.State(ctx, rel.ID)
	if err != nil {
		return nodeState{}, err
	}
	word := orderedString(answer, "state")
	live := implementation
	switch word {
	case registry.StateRequested, registry.StateReceived, registry.StateVerifying, registry.StateCorrected, registry.StateVerified, registry.StateRereview,
		registry.StateMerged, registry.StateNeedsChanges:
		// the child's own latest report decides first: a blocked_needs_input that arrived after a report under review is the newest word.
		if blocked, detail, err := s.blockedByChild(ctx, q, rel); err != nil {
			return nodeState{}, err
		} else if blocked {
			return ownedAs(StateReported, BlockedInputUnverifiedAtUse, detail, live), nil
		}
	}
	switch word {
	case registry.StateRequested:
		return ownedAs(StateRunning, SkipAlreadyOwned, "relay state "+word, live), nil
	case registry.StateReceived:
		return ownedAs(StateReported, SkipAlreadyOwned, "relay state "+word, live), nil
	case registry.StateVerifying, registry.StateCorrected, registry.StateVerified, registry.StateRereview, registry.StateMerged:
		return ownedAs(StateVerifying, SkipAlreadyOwned, "relay state "+word, live), nil
	case registry.StateNeedsChanges:
		return ownedAs(StateCorrecting, SkipAlreadyOwned, "relay state "+word, live), nil
	case registry.StatePaused:
		return ownedAs(StatePausedNode, SkipAlreadyOwned, "the relationship is paused", live), nil
	case registry.StateAbandoned:
		return ownedAs(StateCancelled, SkipAlreadyOwned, "the relationship was cancelled", false), nil
	case registry.StateClosed:
		return ownedAs(StateClosedNode, SkipAlreadyOwned, "the relationship is closed", false), nil
	case registry.StateAmbiguous:
		return ownedAs(StateAmbiguousNode, BlockedAmbiguousHead, "the relationship has more than one head revision", live), nil
	}
	return ownedAs(StateRunning, SkipAlreadyOwned, "relay state "+word, live), nil
}

// blockedByChild is whether the latest final event of the current generation is the child's own blocked_needs_input receipt (a later report
// supersedes it), and its text. The text is the child's word; it is shown, never acted on.
func (s *Scheduler) blockedByChild(ctx context.Context, q store.Querier, rel relRow) (bool, string, error) {
	var outcome, receipt string
	found, err := queryOne(ctx, q, "SELECT outcome, receipt FROM events WHERE relationship_id = ? AND execution_generation = ? AND stage = 'final' AND suppressed_reason IS NULL"+
		" AND producer = 'child' ORDER BY first_seen_at DESC, event_id DESC LIMIT 1", []any{rel.ID, rel.Generation}, &outcome, &receipt)
	if err != nil || !found || outcome != "blocked_needs_input" {
		return false, "", err
	}
	if len(receipt) > 300 {
		receipt = receipt[:300]
	}
	return true, "the child reported blocked_needs_input: " + receipt, nil
}

// accepted is a node the parent accepted. An integration that is proven stays proven whatever happened to the relationship afterwards. Otherwise the
// relationship's own status comes first: a cancelled one no longer holds its regions, a paused one still does. Then the merge lane's negative facts (an
// eviction, a landing whose effect is unknown) are shown as blocked, and a node with none is accepted.
func (s *Scheduler) accepted(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, acc Acceptance, rel relRow, implementation bool) (nodeState, error) {
	out := nodeState{Owned: true, Acc: acc, HasAcc: true}
	// what landed is judged from the accepted head, whatever kind a later revision gave the node (revalidation.go: a node that landed is never run again)
	if implementation || acc.HeadSHA != "" {
		landed, _, err := s.nodeIntegrated(ctx, q, plan, snap, acc)
		if err != nil {
			return nodeState{}, err
		}
		if landed {
			out.State, out.Disp, out.Reason = StateIntegrated, DispDone, DoneIntegrated
			out.Detail = "the accepted head " + short(acc.HeadSHA) + " is contained in every target and marked merged"
			return out, nil
		}
	}
	switch rel.Status {
	case "cancelled":
		out.State, out.Disp, out.Reason = StateCancelled, DispSkip, SkipAlreadyOwned
		out.Detail = "accepted as " + short(acc.AcceptanceID) + " and then the relationship was cancelled; nothing it accepted has landed"
		return out, nil
	case "paused":
		out.State, out.Disp, out.Reason, out.Holds = StatePausedNode, DispSkip, SkipAlreadyOwned, implementation
		out.Detail = "accepted as " + short(acc.AcceptanceID) + " and the relationship is paused"
		return out, nil
	}
	if implementation {
		var outcome string
		evicted, err := queryOne(ctx, q, "SELECT outcome FROM dag_merge_checks WHERE acceptance_id = ? ORDER BY check_seq DESC LIMIT 1", []any{acc.AcceptanceID}, &outcome)
		if err != nil {
			return nodeState{}, err
		}
		out.State, out.Holds = StateAccepted, true
		if evicted && outcome == "evicted" {
			out.Disp, out.Reason, out.Detail = DispBlocked, BlockedEvicted, "a required check failed again on the same head after its retry; the node left the merge lane"
			return out, nil
		}
		var one int
		unknown, err := queryOne(ctx, q, "SELECT 1 FROM merge_turns WHERE candidate_head = ? AND state = 'unknown' LIMIT 1", []any{acc.HeadSHA}, &one)
		if err != nil {
			return nodeState{}, err
		}
		if unknown && acc.HeadSHA != "" {
			out.Disp, out.Reason, out.Detail = DispBlocked, BlockedEffectUnknown, "a merge turn for the accepted head ended with an unknown effect; whether it landed is not known"
			return out, nil
		}
	}
	// the plan moved under what the accepted result consumed: it no longer counts as current (invalidation.go). The relationship's own facts above come first in the reading; the edges built on the
	// result ask the same judgement and do not depend on this order.
	stale, err := s.staleOf(ctx, q, plan, snap, n)
	if err != nil {
		return nodeState{}, err
	}
	if stale != nil {
		out.State, out.Disp, out.Reason, out.Detail = StateStale, DispStale, stale.Reason(), stale.Text
		return out, nil
	}
	out.State, out.Disp, out.Reason = StateAccepted, DispDone, DoneAccepted
	out.Detail = "accepted as " + short(acc.AcceptanceID)
	return out, nil
}

// errNoAcceptance is returned by loadAcceptanceByID for an id the store does not hold.
var errNoAcceptance = errors.New("no such acceptance")

// loadAcceptanceByID reads one acceptance of any state.
func loadAcceptanceByID(ctx context.Context, q store.Querier, id string) (Acceptance, error) {
	a, err := scanAcceptance(q.QueryRowContext(ctx, "SELECT "+acceptanceColumns+" FROM dag_acceptances WHERE acceptance_id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Acceptance{}, errNoAcceptance
	}
	return a, err
}
