package dagsched

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The release policy of a plan (CRW-411): local-optimistic release is switched off when the last landings show that optimism costs more than it saves, and on again after a run of clean
// landings (the additive-increase, multiplicative-decrease shape of a gate that shrinks its window on failure). The values it reads are recorded in dag_release_policy; a plan with no
// row has no policy and nothing here changes how it is read or digested. The state is a pure function of the landings in order, the results recorded for them and the settings in force,
// all read from stored rows, so two readings of one store state are equal. It is advice to the scheduler's own release judgement: it stops no child, revokes no release and changes no merge gate.

// The two states of local-optimistic release under a policy.
const (
	OptimismOn  = "on"
	OptimismOff = "off"
)

// The bounds of what a policy keeps and prints: a window is at most MaxPolicyWindow landings (so the rows in the reading, its digest and the pass record are bounded), and a reading names
// the last MaxPolicyTransitions switches.
const (
	MaxPolicyWindow      = 64
	MaxPolicyTransitions = 8
)

// ReleaseSettings are the values of one policy: Window landings are read; a landing is slow when the conflict it handled took more than HandlingSeconds; RedMerges red or reverted landings
// in the window, or one slow landing, switch local-optimistic release off; CleanRun clean landings in a row (neither slow, red nor reverted) switch it on again.
type ReleaseSettings struct {
	Seq                    int64
	Window                 int
	HandlingSeconds        int64
	RedMerges, CleanRun    int
	RecordedBy, RecordedAt string
}

// PolicyInput is what dag-release-policy-record is given.
type PolicyInput struct {
	Window          int64
	HandlingSeconds int64
	RedMerges       int64
	CleanRun        int64
}

// PolicyRecord is the answer of RecordReleasePolicy.
type PolicyRecord struct {
	PlanID   string
	Settings ReleaseSettings
	Replayed bool
}

func (p PolicyInput) validate() error {
	switch {
	case p.Window < 1 || p.Window > MaxPolicyWindow:
		return refuse(contract.RefusalMalformedReceipt, "the window is 1 to %d landings, not %d", MaxPolicyWindow, p.Window)
	case p.HandlingSeconds < 1:
		return refuse(contract.RefusalMalformedReceipt, "the handling time that makes a landing slow is 1 second or more, not %d", p.HandlingSeconds)
	case p.RedMerges < 1 || p.RedMerges > p.Window:
		return refuse(contract.RefusalMalformedReceipt, "the red landings that switch optimism off are 1 to the window (%d), not %d", p.Window, p.RedMerges)
	case p.CleanRun < 1 || p.CleanRun > p.Window:
		return refuse(contract.RefusalMalformedReceipt, "the clean landings that switch it on again are 1 to the window (%d), not %d", p.Window, p.CleanRun)
	}
	return nil
}

// loadSettings is the policy in force for a plan: the row with the highest policy_seq. A store whose zone predates the table has none, and a plan with no row has none.
func loadSettings(ctx context.Context, q store.Querier, plan string) (ReleaseSettings, bool, error) {
	if ok, err := tableExists(ctx, q, "dag_release_policy"); err != nil || !ok {
		return ReleaseSettings{}, false, err
	}
	var s ReleaseSettings
	found, err := queryOne(ctx, q, "SELECT policy_seq, window_size, handling_seconds, red_merges, clean_run, recorded_by, recorded_at FROM dag_release_policy WHERE plan_id = ? ORDER BY policy_seq DESC LIMIT 1",
		[]any{plan}, &s.Seq, &s.Window, &s.HandlingSeconds, &s.RedMerges, &s.CleanRun, &s.RecordedBy, &s.RecordedAt)
	return s, found, err
}

// RecordReleasePolicy records the values the release policy of a plan reads (dag-release-policy-record). The actor is the registered parent of the plan's project, the session holds the
// plan's epoch, the values are within their bounds, and the same values as the policy in force are a replay. A different set is the next policy_seq; the earlier rows stay as the ledger.
func (s *Scheduler) RecordReleasePolicy(ctx context.Context, plan, actor string, in PolicyInput) (PolicyRecord, error) {
	if err := in.validate(); err != nil {
		return PolicyRecord{}, err
	}
	out := PolicyRecord{PlanID: plan}
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		if err := s.fence(txCtx, q, plan, actor); err != nil {
			return err
		}
		snap, _, err := dag.SnapshotAt(txCtx, q, plan, 0)
		if err != nil {
			return err
		}
		if err := s.requireParent(txCtx, q, snap, actor); err != nil {
			return err
		}
		latest, found, err := loadSettings(txCtx, q, plan)
		if err != nil {
			return err
		}
		if found && int64(latest.Window) == in.Window && latest.HandlingSeconds == in.HandlingSeconds && int64(latest.RedMerges) == in.RedMerges && int64(latest.CleanRun) == in.CleanRun {
			out.Settings, out.Replayed = latest, true
			return nil
		}
		out.Settings = ReleaseSettings{Seq: latest.Seq + 1, Window: int(in.Window), HandlingSeconds: in.HandlingSeconds, RedMerges: int(in.RedMerges), CleanRun: int(in.CleanRun), RecordedBy: actor, RecordedAt: s.now()}
		_, err = q.ExecContext(txCtx, "INSERT INTO dag_release_policy (plan_id, policy_seq, window_size, handling_seconds, red_merges, clean_run, recorded_by, coordinator_epoch, recorded_at) VALUES (?,?,?,?,?,?,?,?,?)",
			plan, out.Settings.Seq, out.Settings.Window, out.Settings.HandlingSeconds, out.Settings.RedMerges, out.Settings.CleanRun, actor, s.ExpectedEpoch, out.Settings.RecordedAt)
		return err
	})
	return out, err
}

// Landing is one merge of the plan, read from stored rows only: a node whose accepted head the scheduler reads as landed (nodeIntegrated: contained in every target, marked merged, its merge
// turn landed), at the latest time any of its targets became integrated. The facts beside it are what the store says of the work around the merge.
type Landing struct {
	NodeID string
	// At is the stored text of the landing time and When its instant; landings are ordered by (When, NodeID).
	At   string
	When time.Time
	// HandlingSeconds is the time from the first conflict sweep after the node's first acceptance that measured its head against a tip with a conflict, to the landing; nil when no such sweep
	// exists (nothing conflicted that anyone measured after the hand-over). A landing without it is not slow.
	HandlingSeconds *int64
	// StaleBase is the merge judgements of the node that found the base moved (a count of stored rows, not of round trips); Judged is whether the node has any merge judgement; Returns is the
	// correction generations dag-correct recorded for the node.
	StaleBase, Returns int
	Judged             bool
	// Results are the kinds the parent recorded for the node in dag_landing_results.
	Results map[string]bool
}

func (l Landing) red() bool      { return l.Results[ResultDevRed] }
func (l Landing) reverted() bool { return l.Results[ResultReverted] }

func (l Landing) slow(s ReleaseSettings) bool {
	return l.HandlingSeconds != nil && *l.HandlingSeconds > s.HandlingSeconds
}

func (l Landing) clean(s ReleaseSettings) bool { return !l.slow(s) && !l.red() && !l.reverted() }

// storedTime reads a stored instant; stored times are RFC 3339 with an offset, and are compared as instants and never as text.
func storedTime(text string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, text)
	return t, err == nil
}

// landings are the plan's landings in order, with the facts the policy and the measurements read.
func (s *Scheduler) landings(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot) ([]Landing, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+acceptanceColumns+" FROM dag_acceptances WHERE plan_id = ? AND state = 'active' AND head_sha IS NOT NULL AND head_sha <> '' ORDER BY node_id", plan)
	if err != nil {
		return nil, err
	}
	var accepted []Acceptance
	for rows.Next() {
		a, err := scanAcceptance(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		accepted = append(accepted, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	sweeps, err := tableExists(ctx, q, "dag_conflict_sweep_members")
	if err != nil {
		return nil, err
	}
	results, err := tableExists(ctx, q, "dag_landing_results")
	if err != nil {
		return nil, err
	}
	var out []Landing
	for _, a := range accepted {
		landed, targets, err := s.nodeIntegrated(ctx, q, plan, snap, a)
		if err != nil {
			return nil, err
		}
		if !landed {
			continue
		}
		l := Landing{NodeID: a.NodeID, Results: map[string]bool{}}
		for _, t := range targets {
			at, err := s.integratedAt(ctx, q, plan, a, t.Repository, t.BaseRef)
			if err != nil {
				return nil, err
			}
			if when, ok := storedTime(at.Since); ok && (l.At == "" || when.After(l.When)) {
				l.At, l.When = at.Since, when
			}
		}
		if l.At == "" {
			continue
		}
		if sweeps {
			if l.HandlingSeconds, err = handlingSeconds(ctx, q, plan, a.NodeID, l.When); err != nil {
				return nil, err
			}
		}
		var judgements int
		if _, err := queryOne(ctx, q, "SELECT COUNT(*), COALESCE(SUM(c.outcome = 'stale_base'), 0) FROM dag_merge_checks c JOIN dag_acceptances x ON x.acceptance_id = c.acceptance_id WHERE x.plan_id = ? AND x.node_id = ?",
			[]any{plan, a.NodeID}, &judgements, &l.StaleBase); err != nil {
			return nil, err
		}
		l.Judged = judgements > 0
		if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND kind = 'correction'", []any{plan, a.NodeID}, &l.Returns); err != nil {
			return nil, err
		}
		if results {
			if l.Results, err = nodeResults(ctx, q, plan, a.NodeID); err != nil {
				return nil, err
			}
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].When.Equal(out[j].When) {
			return out[i].When.Before(out[j].When)
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out, nil
}

// handlingSeconds is the conflict handling time of a landing: from the first conflict sweep at or after the node's first acceptance to the landing. A sweep counts when a member of it measured the
// node's head against a tip (observed or replayed) and found a conflict; a replay is a measurement made at the sweep's time, whatever the age of the observation row it points to. Nil when there is none.
func handlingSeconds(ctx context.Context, q store.Querier, plan, node string, landed time.Time) (*int64, error) {
	var floor time.Time
	accepted, err := q.QueryContext(ctx, "SELECT accepted_at FROM dag_acceptances WHERE plan_id = ? AND node_id = ?", plan, node)
	if err != nil {
		return nil, err
	}
	for accepted.Next() {
		var text string
		if err := accepted.Scan(&text); err != nil {
			accepted.Close()
			return nil, err
		}
		if t, ok := storedTime(text); ok && (floor.IsZero() || t.Before(floor)) {
			floor = t
		}
	}
	if err := accepted.Err(); err != nil {
		accepted.Close()
		return nil, err
	}
	accepted.Close()
	if floor.IsZero() {
		return nil, nil
	}
	measured, err := q.QueryContext(ctx, "SELECT s.observed_at FROM dag_conflict_sweep_members m JOIN dag_conflict_sweeps s ON s.plan_id = m.plan_id AND s.sweep_seq = m.sweep_seq"+
		" WHERE m.plan_id = ? AND m.kind = 'tip' AND m.left_node_id = ? AND m.status <> 'unmeasured' AND m.conflicts > 0", plan, node)
	if err != nil {
		return nil, err
	}
	defer measured.Close()
	var first time.Time
	for measured.Next() {
		var text string
		if err := measured.Scan(&text); err != nil {
			return nil, err
		}
		if t, ok := storedTime(text); ok && !t.Before(floor) && !t.After(landed) && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	if err := measured.Err(); err != nil {
		return nil, err
	}
	if first.IsZero() {
		return nil, nil
	}
	seconds := int64(landed.Sub(first) / time.Second)
	return &seconds, nil
}

// Transition is one switch of local-optimistic release: the landing at which the state read after it differs from the one before, and why.
type Transition struct {
	NodeID, At, From, To, Because string
}

// OptimismState is what the policy says of a plan now: the settings in force, whether local-optimistic release is on, why, the landings of the window (oldest first) and the last switches. It is
// what the reading prints as release_policy and a recorded pass keeps.
type OptimismState struct {
	Settings           ReleaseSettings
	On                 bool
	Reason             string
	Landings           int
	Window             []Landing
	Transitions        []Transition
	TransitionsOmitted int
}

// optimismAfter is the state after the landings it is given (in order): off when a landing in the last Window of them is slow or RedMerges of them are red or reverted, unless the last CleanRun
// are all clean. With no landing it is on. The reason is the sentence that says which.
func optimismAfter(s ReleaseSettings, landings []Landing) (on bool, reason string) {
	if len(landings) == 0 {
		return true, "no landing has been read yet"
	}
	window := landings
	if len(window) > s.Window {
		window = window[len(window)-s.Window:]
	}
	var slow *Landing
	troubled := 0
	for i := range window {
		if slow == nil && window[i].slow(s) {
			slow = &window[i]
		}
		if window[i].red() || window[i].reverted() {
			troubled++
		}
	}
	trigger := ""
	switch {
	case slow != nil:
		trigger = fmt.Sprintf("landing %s took %d seconds to settle a conflict, over the %d seconds the policy allows", slow.NodeID, *slow.HandlingSeconds, s.HandlingSeconds)
	case troubled >= s.RedMerges:
		trigger = fmt.Sprintf("%d of the last %d landings are red or reverted (the policy switches off at %d)", troubled, len(window), s.RedMerges)
	}
	if trigger == "" {
		return true, fmt.Sprintf("none of the last %d landings is slow, red or reverted", len(window))
	}
	if len(landings) >= s.CleanRun {
		clean := true
		for _, l := range landings[len(landings)-s.CleanRun:] {
			clean = clean && l.clean(s)
		}
		if clean {
			return true, fmt.Sprintf("the last %d landings were clean (the window still holds: %s)", s.CleanRun, trigger)
		}
	}
	return false, trigger
}

// evaluateOptimism is the state under a policy: the state after all the landings, and the switches found by reading the state after each prefix of them.
func evaluateOptimism(settings ReleaseSettings, landings []Landing) *OptimismState {
	state := &OptimismState{Settings: settings, Landings: len(landings)}
	state.On, state.Reason = optimismAfter(settings, landings)
	previous := true
	var switches []Transition
	for i := 1; i <= len(landings); i++ {
		on, because := optimismAfter(settings, landings[:i])
		if on != previous {
			t := Transition{NodeID: landings[i-1].NodeID, At: landings[i-1].At, From: optimismWord(previous), To: optimismWord(on), Because: because}
			switches = append(switches, t)
			previous = on
		}
	}
	if len(switches) > MaxPolicyTransitions {
		state.TransitionsOmitted = len(switches) - MaxPolicyTransitions
		switches = switches[len(switches)-MaxPolicyTransitions:]
	}
	state.Transitions = switches
	window := landings
	if len(window) > settings.Window {
		window = window[len(window)-settings.Window:]
	}
	state.Window = window
	return state
}

func optimismWord(on bool) string {
	if on {
		return OptimismOn
	}
	return OptimismOff
}

// releasePolicy is the policy state of a plan for a reading: nil when the plan has no policy, and then nothing about the reading differs from a build without it.
func (s *Scheduler) releasePolicy(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot) (*OptimismState, error) {
	settings, armed, err := loadSettings(ctx, q, plan)
	if err != nil || !armed {
		return nil, err
	}
	landings, err := s.landings(ctx, q, plan, snap)
	if err != nil {
		return nil, err
	}
	return evaluateOptimism(settings, landings), nil
}

func optionalSeconds(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func (l Landing) object(s ReleaseSettings) contract.OrderedObject {
	return contract.OrderedObject{{Key: "node_id", Value: l.NodeID}, {Key: "landed_at", Value: l.At}, {Key: "conflict_handling_seconds", Value: optionalSeconds(l.HandlingSeconds)},
		{Key: "slow", Value: l.slow(s)}, {Key: "post_merge_red", Value: l.red()}, {Key: "reverted", Value: l.reverted()}}
}

func (l Landing) canonical(s ReleaseSettings) map[string]any {
	m := map[string]any{"node_id": l.NodeID, "landed_at": l.At, "slow": l.slow(s), "post_merge_red": l.red(), "reverted": l.reverted()}
	if l.HandlingSeconds != nil {
		m["conflict_handling_seconds"] = *l.HandlingSeconds
	}
	return m
}

func (t Transition) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "node_id", Value: t.NodeID}, {Key: "landed_at", Value: t.At}, {Key: "from", Value: t.From}, {Key: "to", Value: t.To}, {Key: "because", Value: t.Because}}
}

func (t Transition) canonical() map[string]any {
	return map[string]any{"node_id": t.NodeID, "landed_at": t.At, "from": t.From, "to": t.To, "because": t.Because}
}

func (s ReleaseSettings) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "policy_seq", Value: s.Seq}, {Key: "window", Value: s.Window}, {Key: "handling_seconds", Value: s.HandlingSeconds},
		{Key: "red_merges", Value: s.RedMerges}, {Key: "clean_run", Value: s.CleanRun}}
}

func (s ReleaseSettings) canonical() map[string]any {
	return map[string]any{"policy_seq": s.Seq, "window": s.Window, "handling_seconds": s.HandlingSeconds, "red_merges": s.RedMerges, "clean_run": s.CleanRun}
}

// object is the policy state as the reading prints it (release_policy).
func (o OptimismState) object() contract.OrderedObject {
	window := make([]any, len(o.Window))
	for i, l := range o.Window {
		window[i] = l.object(o.Settings)
	}
	switches := make([]any, len(o.Transitions))
	for i, t := range o.Transitions {
		switches[i] = t.object()
	}
	return contract.OrderedObject{{Key: "settings", Value: o.Settings.object()}, {Key: "local_optimistic", Value: optimismWord(o.On)}, {Key: "reason", Value: o.Reason},
		{Key: "landings", Value: o.Landings}, {Key: "window", Value: window}, {Key: "transitions", Value: switches}, {Key: "transitions_omitted", Value: o.TransitionsOmitted}}
}

// canonical is the policy state as a recorded pass and the reading's digest keep it.
func (o OptimismState) canonical() map[string]any {
	window := make([]any, len(o.Window))
	for i, l := range o.Window {
		window[i] = l.canonical(o.Settings)
	}
	switches := make([]any, len(o.Transitions))
	for i, t := range o.Transitions {
		switches[i] = t.canonical()
	}
	return map[string]any{"settings": o.Settings.canonical(), "local_optimistic": optimismWord(o.On), "reason": o.Reason, "landings": o.Landings, "window": window,
		"transitions": switches, "transitions_omitted": o.TransitionsOmitted}
}
