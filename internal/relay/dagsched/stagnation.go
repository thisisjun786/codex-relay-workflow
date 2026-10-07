package dagsched

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A node's stagnation counter and the closed repair ladder (section 80 S-1). The reading answers the one question CRW-185 left open: a node that keeps failing the same way is counted, and the
// repair that comes next is named. It is derived like every other node state: the rows the store already keeps are the counter's only input, no writer is added for them, and the count clears by
// itself once the cause is repaired. Nothing here writes, refuses or reads a clock: two readings of one store state return equal objects, so the object is advisory like the merge-order
// constraint.

// The causes of a stagnant node, as its reading names them.
const (
	// CauseRepeatedFinding: the same finding was carried by consecutive corrections of the node.
	CauseRepeatedFinding = "repeated_finding"
	// CauseRepeatedCheckFailure: the same required checks failed again on the same head after the merge lane's one retry.
	CauseRepeatedCheckFailure = "repeated_check_failure"
)

// The rungs of the repair ladder, weakest first (the research report's ladder). The reading names one rung and stops naming one past the last.
const (
	RungRetrySamePacket = "retry_same_packet"
	RungEditPacket      = "edit_packet"
	RungSplitNode       = "split_node"
	RungNeighbourRepair = "neighbour_repair"
	RungFullReplan      = "full_replan"
)

// stagnationThreshold is the count a stagnation object first appears at: one correction is not repetition.
const stagnationThreshold = 2

// Stagnation is the reading of a node that keeps failing the same way: how many times the same cause has recurred (Count), which cause it is (Cause, one of the two constants above), the rung of
// the repair ladder the count has reached (Rung), what that rung asks of the parent (Next), the identity the repetition rests on (FindingDigest, the canonical-JSON sha256 of the finding entries
// that are not the restoration block; empty for a repeated check failure, which has no finding text) and the event the newest counted row belongs to (LastEventID).
type Stagnation struct {
	NodeID        string
	Count         int
	Cause         string
	Rung          string
	Next          string
	FindingDigest string
	LastEventID   string
}

// object is the reading as dag-ready prints it beside a node's release and merge_order.
func (st Stagnation) object() contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "node_id", Value: st.NodeID}, {Key: "count", Value: st.Count}, {Key: "cause", Value: st.Cause}, {Key: "rung", Value: st.Rung},
		{Key: "next", Value: optionalText(st.Next)}, {Key: "finding_digest", Value: optionalText(st.FindingDigest)}, {Key: "last_event_id", Value: optionalText(st.LastEventID)},
	}
}

// canonical is the object as the reading's input digest keeps it.
func (st Stagnation) canonical() map[string]any {
	return map[string]any{"node_id": st.NodeID, "count": st.Count, "cause": st.Cause, "rung": st.Rung, "next": st.Next, "finding_digest": st.FindingDigest, "last_event_id": st.LastEventID}
}

// Stagnation reads a node’s stagnation counter from the rows the store already keeps: its correction generations (dag_node_executions, kind correction) with the finding each ruling carried,
// its merge-check history (dag_merge_checks, the failed required checks of each judgement) and its revalidations (dag_acceptance_revalidations). It returns nil below the threshold of 2, and a
// node that landed reads none whatever the rows say (it is never run again, E-20). It reads no clock and writes nothing.
//
// What counts is the current state of the node: an acceptance is the repair, so the rows of the generations before it, and the check rows of a head other than the one it stands on now, are the
// history of a result that was already repaired. The head a node stands on is the one its acceptance stands on now (standOf): a recorded base refresh moves it, and the check rows of the head it
// stood on before the record are that head’s history. A node with no active acceptance counts every correction generation it has, and reads no repeated check failure at all: there is no head it
// stands on, and a check row is evidence about the commit it ran on.
//
// A revalidation (dag_acceptance_revalidations) is read as part of the acceptance it re-verifies and not as a row of its own: it re-verifies the SAME accepted output under the plan’s criteria and
// writes no correction generation, so it moves neither the floor nor the run. It matters here for the other direction: because the active acceptance is what the floor comes from, a node whose result
// was re-verified keeps counting only what happened after it, exactly as an acceptance does.
func (s *Scheduler) Stagnation(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) (*Stagnation, error) {
	// a node that landed is never run again, so the ladder has nothing to say about it
	since := int64(0)
	head := ""
	acc, has, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
	if err != nil {
		return nil, err
	}
	if has {
		landed, _, err := s.nodeIntegrated(ctx, q, plan, snap, acc)
		if err != nil {
			return nil, err
		}
		if landed {
			return nil, nil
		}
		// an acceptance is the repair: only what happened after the current result was accepted describes a node that is failing now. The generation counted from is the one the acceptance
		// STANDS on, not the one its row names: a recorded base refresh moves an acceptance to a later generation, and the generations before that are history.
		stand, err := s.standOf(ctx, q, acc)
		if err != nil {
			return nil, err
		}
		since = stand.Generation
		head = stand.Head
	}
	runs, err := s.correctionRuns(ctx, q, plan, n.NodeID, since)
	if err != nil {
		return nil, err
	}
	// the newest run of consecutive generations that carried the same finding identity decides the count; a different finding, and a generation opened by hand (which has no ruling and no
	// findings), breaks the run
	best, bestDigest := 0, ""
	if len(runs) > 0 {
		best = runs[len(runs)-1].count
		bestDigest = runs[len(runs)-1].digest
	}
	checks, err := s.repeatedCheckFailure(ctx, q, plan, n.NodeID, head)
	if err != nil {
		return nil, err
	}
	// the two causes are counted on different rows and never added: the longer run is the node’s count, and the finding run wins a tie because it carries the identity
	if best >= checks.count && best >= stagnationThreshold {
		return stagnationOf(n.NodeID, best, CauseRepeatedFinding, bestDigest, runs[len(runs)-1].event), nil
	}
	if checks.count >= stagnationThreshold {
		return stagnationOf(n.NodeID, checks.count, CauseRepeatedCheckFailure, "", checks.event), nil
	}
	return nil, nil
}

func stagnationOf(node string, count int, cause, digest, event string) *Stagnation {
	return &Stagnation{NodeID: node, Count: count, Cause: cause, Rung: stagnationRung(count), Next: stagnationNext(count), FindingDigest: digest, LastEventID: event}
}

// stagnationRung is the rung a count names: the first count is one attempt of the same packet (a retry that was recorded), the second says the packet itself has to change, and so on up the
// ladder. A count past the last rung keeps naming it.
func stagnationRung(count int) string {
	switch {
	case count <= 1:
		return RungRetrySamePacket
	case count == 2:
		return RungEditPacket
	case count == 3:
		return RungSplitNode
	case count == 4:
		return RungNeighbourRepair
	}
	return RungFullReplan
}

// stagnationNext is the rung the ladder names after this one, and empty at the last rung: the reading names a rung and stops naming one past full_replan, so nothing beyond the ladder is ever
// named. The rungs are the actions the parent already has: a retry of the same packet (dag-release), a correction of the packet (dag-correct), a plan revision with replace_node for a split or a
// neighbour repair, and a plan revision for a full replan.
func stagnationNext(count int) string {
	switch stagnationRung(count) {
	case RungRetrySamePacket:
		return RungEditPacket
	case RungEditPacket:
		return RungSplitNode
	case RungSplitNode:
		return RungNeighbourRepair
	case RungNeighbourRepair:
		return RungFullReplan
	}
	return ""
}

// correctionRun is a run of consecutive correction generations that carried the same finding identity.
type correctionRun struct {
	digest string
	count  int
	event  string
}

// correctionRuns walks a node’s recorded correction generations in order and returns the run each generation belongs to. A generation’s finding identity is the canonical-JSON sha256 of the
// findings of the needs_changes ruling that opened it (the same join RecordCorrection reads and restorationDigest parses), minus the restoration entry, whose note carries this generation’s
// manifest digest and so differs every round. A generation the coordinator opened by hand has no ruling at all, and one whose ruling carried no findings reads the same: neither carries an
// identity, and both break a run without counting.
//
// The ruling is looked up the way RecordCorrection looks it up: the event of the generation BEFORE this one, in this relationship, that was ruled needs_changes and named this generation next. The
// relationship and the event are what scope it; the verdict alone would let a correction of one node read another relationship’s ruling when the generation numbers coincide. A generation that several
// events of that relationship name (a report superseded by a later one) is read once, from the newest ruling.
//
// The generation before is the nearest one below it that was not withdrawn (store.LiveGenerationBefore), the same lookup RecordCorrection makes: a withdrawn generation is neither counted nor a
// break, it is simply not there, and the ruling that opened the correction sits on the live generation below it.
func (s *Scheduler) correctionRuns(ctx context.Context, q store.Querier, plan, node string, since int64) ([]correctionRun, error) {
	rows, err := q.QueryContext(ctx, "SELECT e.execution_generation, e.relationship_id FROM dag_node_executions e"+
		" WHERE e.plan_id = ? AND e.node_id = ? AND e.kind = 'correction' AND e.execution_generation > ?"+
		" ORDER BY e.execution_generation, e.relationship_id", plan, node, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// The rows are read to the end and CLOSED before anything else is queried on this querier. The store has one connection
	// (store.Open: SetMaxOpenConns(1)) and the release path reads Ready outside a transaction, so a second query issued while
	// these rows are open would wait for the connection they hold, until the context deadline: one node with a correction
	// generation stopped every release of its plan.
	type correctionGeneration struct {
		generation   int64
		relationship string
	}
	var collected []correctionGeneration
	var seen int64
	for rows.Next() {
		var generation int64
		var rid string
		if err := rows.Scan(&generation, &rid); err != nil {
			return nil, err
		}
		if generation == seen {
			continue
		}
		seen = generation
		collected = append(collected, correctionGeneration{generation: generation, relationship: rid})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Close's own error is not read, as the deferred Close above never was: iterating the rows is
	// what rows.Err reports, and the answer this function returns is what it returned before.
	_ = rows.Close()
	var runs []correctionRun
	for _, g := range collected {
		// the generation this one follows: the nearest one below it that was not withdrawn
		before, err := store.LiveGenerationBefore(ctx, q, g.relationship, g.generation)
		if err != nil {
			return nil, err
		}
		var eventID, findings *string
		found, err := queryOne(ctx, q, "SELECT v.event_id, c.findings FROM verdicts v"+
			" JOIN events e ON e.event_id = v.event_id"+
			" JOIN verdict_context c ON c.event_id = v.event_id"+
			" WHERE e.relationship_id = ? AND e.execution_generation = ? AND v.verdict = 'needs_changes' AND v.next_generation = ?"+
			" ORDER BY v.decided_at DESC LIMIT 1", []any{g.relationship, before, g.generation}, &eventID, &findings)
		if err != nil {
			return nil, err
		}
		digest := ""
		if found && findings != nil {
			digest = correctionFindingDigest(*findings)
		}
		if digest == "" {
			// no ruling, or a ruling that carried no findings: this generation is counted as a correction generation and carries no identity, so it breaks the run
			runs = append(runs, correctionRun{})
			continue
		}
		if len(runs) > 0 && runs[len(runs)-1].digest == digest {
			runs[len(runs)-1].count++
			runs[len(runs)-1].event = eventOf(eventID)
			continue
		}
		runs = append(runs, correctionRun{digest: digest, count: 1, event: eventOf(eventID)})
	}
	return runs, nil
}

func eventOf(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}

// correctionFindingDigest is a correction generation's finding identity: the canonical-JSON sha256 of the ruling's finding entries that are not the restoration block, each entry as the relay
// stored it. The restoration entry is excluded because its note carries this generation's manifest digest, which differs every round. A text that is not a JSON list of objects, or a list with
// nothing left once the restoration entry is out, carries no identity.
func correctionFindingDigest(findings string) string {
	if strings.TrimSpace(findings) == "" {
		return ""
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(findings), &list); err != nil {
		return ""
	}
	kept := make([]any, 0, len(list))
	for _, f := range list {
		if flag, _ := f["restoration"].(bool); flag {
			continue
		}
		kept = append(kept, f)
	}
	if len(kept) == 0 {
		return ""
	}
	return dag.Digest(kept)
}

// repeatedCheckFailure is a node’s run of repeated failed required checks on one head: the newest merge-check row that is not a re-run in progress, and the consecutive rows before it that failed
// the same non-empty set of required check names, newest first (the merge lane’s retry round and its eviction, D-12). The rows come from EVERY acceptance of the node that observed this head
// (whatever its state: the checks ran on the commit, so a head accepted again keeps the failures the earlier acceptance recorded), and the head is the one the node’s acceptance stands on now — the
// accepted head, or after a recorded base refresh the head the record names.
//
// The count is the number of DISTINCT failed attempts, not the number of rows: an attempt is the (name, run, attempt) of every failed required check of one judgement, so the same attempt observed
// again (another evidence digest, or the eviction copied onto a fresh acceptance of the same head) counts once. A checks_pending row is neither counted nor a break: it is a re-run in progress, not a
// result. A row that failed nothing, or failed another set of names, ends the run. The round is carried in the history but is not what ends the run, because a head accepted again restarts it. The
// digest is empty: a check failure has no finding text, and the failed check names are its identity.
func (s *Scheduler) repeatedCheckFailure(ctx context.Context, q store.Querier, plan, node, head string) (correctionRun, error) {
	if head == "" {
		return correctionRun{}, nil
	}
	rows, err := q.QueryContext(ctx, "SELECT c.failed_required_json, c.outcome FROM dag_merge_checks c"+
		" JOIN dag_acceptances a ON a.acceptance_id = c.acceptance_id"+
		" WHERE a.plan_id = ? AND a.node_id = ? AND c.head_sha = ? ORDER BY c.rowid DESC", plan, node, head)
	if err != nil {
		return correctionRun{}, err
	}
	defer rows.Close()
	var list []struct {
		failed, outcome string
	}
	for rows.Next() {
		var failed, outcome string
		if err := rows.Scan(&failed, &outcome); err != nil {
			return correctionRun{}, err
		}
		list = append(list, struct {
			failed, outcome string
		}{failed, outcome})
	}
	if err := rows.Err(); err != nil {
		return correctionRun{}, err
	}
	// a re-run in progress says nothing about the failure: the newest row that is not one names the failure the run is about
	start := 0
	for start < len(list) && list[start].outcome == OutcomeChecksPending {
		start++
	}
	if start == len(list) {
		return correctionRun{}, nil
	}
	first, err := stagnationAttemptOf(list[start].failed)
	if err != nil {
		return correctionRun{}, err
	}
	if len(first.names) == 0 {
		return correctionRun{}, nil
	}
	seen := map[string]bool{first.identity: true}
	count := 1
	for _, row := range list[start+1:] {
		if row.outcome == OutcomeChecksPending {
			continue
		}
		next, err := stagnationAttemptOf(row.failed)
		if err != nil {
			return correctionRun{}, err
		}
		// the same names failing again is the same failure; a different set, or a row that failed nothing, ends the run
		if !stagnationSameNames(next.names, first.names) {
			break
		}
		if seen[next.identity] {
			continue
		}
		seen[next.identity] = true
		count++
	}
	if count < stagnationThreshold {
		return correctionRun{}, nil
	}
	return correctionRun{count: count}, nil
}

// stagnationAttempt is one judgement’s failed required checks: the set of names that failed (the failure’s identity) and the (name, run, attempt) of every one of them (the attempt’s identity). Two
// judgements are the same attempt exactly when their attempt identities are equal.
type stagnationAttempt struct {
	names    []string // sorted, deduplicated
	identity string
}

// stagnationAttemptOf reads the failed required checks a row records: the names that failed and the (name, run, attempt) of each. The relay’s canonical form is a list of objects (name, run, attempt);
// a row written before that shape holds the names alone, and then the name is the whole attempt (there is no run or attempt to tell two observations apart, so they read as one). The identity is the
// canonical JSON of the sorted (name, run, attempt) tuples, an unambiguous encoding of a set.
func stagnationAttemptOf(text string) (stagnationAttempt, error) {
	if strings.TrimSpace(text) == "" {
		return stagnationAttempt{}, nil
	}
	var list []any
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		return stagnationAttempt{}, err
	}
	names := map[string]bool{}
	tuples := make([]any, 0, len(list))
	for _, entry := range list {
		switch f := entry.(type) {
		case string:
			if f == "" {
				continue
			}
			names[f] = true
			tuples = append(tuples, []any{f, "", int64(0)})
		case map[string]any:
			name, _ := f["name"].(string)
			if name == "" {
				continue
			}
			run, _ := f["run"].(string)
			attempt, _ := f["attempt"].(float64)
			names[name] = true
			tuples = append(tuples, []any{name, run, int64(attempt)})
		}
	}
	out := stagnationAttempt{names: make([]string, 0, len(names))}
	for name := range names {
		out.names = append(out.names, name)
	}
	sort.Strings(out.names)
	slices.SortFunc(tuples, func(a, b any) int { return strings.Compare(dag.Canonical(a), dag.Canonical(b)) })
	out.identity = dag.Canonical(tuples)
	return out, nil
}

// stagnationSameNames is whether two sorted name lists are equal.
func stagnationSameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
