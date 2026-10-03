package dagsched

import (
	"context"
	"database/sql"
	"math"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The measurements of a plan (CRW-411), read from stored rows by one query (dag-measurements): parallelism, conflicts per pull request by grade, conflict handling time, base refresh, post-merge
// results and duplicated or discarded work. Every measure prints how many samples it rests on and either its values or the reason it has none: a value with no data is absent and never zero. A
// zero that is printed is a count over rows that exist (a landing with no stale-base judgement, a grade no measured pull request conflicted at).

// The reasons a measure is absent, a closed set.
const (
	AbsentNoRecordedPass = "no_recorded_pass" // no dag-ready --record pass of the plan
	AbsentNoLandings     = "no_landings"      // no node of the plan has landed
	AbsentNoObservations = "no_observations"  // landings or pull requests exist and nothing measured the thing (no conflict sweep, no merge judgement)
	AbsentNotRecorded    = "not_recorded"     // the store keeps no record of it in this build
	AbsentNoneRecorded   = "none_recorded"    // the parent has recorded no statement of the kind
	AbsentTableMissing   = "table_missing"    // a store whose zone predates the table that holds it
)

// Measurements is the answer of Measure.
type Measurements struct {
	PlanID       string
	PlanRevision int64
	Landings     []Landing
	Parallelism  contract.OrderedObject
	Conflicts    contract.OrderedObject
	Handling     contract.OrderedObject
	BaseRefresh  contract.OrderedObject
	PostMerge    contract.OrderedObject
	Duplicated   contract.OrderedObject
	Cancelled    contract.OrderedObject
}

// absentMetric is a measure with no data: the samples it rests on and the reason.
func absentMetric(samples int, reason string) contract.OrderedObject {
	return contract.OrderedObject{{Key: "samples", Value: samples}, {Key: "absent", Value: reason}}
}

// presentMetric is a measure with values: its samples, no absent reason, then the values.
func presentMetric(samples int, values ...contract.Field) contract.OrderedObject {
	return append(contract.OrderedObject{{Key: "samples", Value: samples}, {Key: "absent", Value: nil}}, values...)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// mean and median of whole numbers, rounded to two places.
func meanOf(values []int64) float64 {
	var sum int64
	for _, v := range values {
		sum += v
	}
	return round2(float64(sum) / float64(len(values)))
}

func medianOf(values []int64) float64 {
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if n := len(sorted); n%2 == 1 {
		return float64(sorted[n/2])
	}
	n := len(sorted)
	return round2(float64(sorted[n/2-1]+sorted[n/2]) / 2)
}

func maxOf(values []int64) int64 {
	best := values[0]
	for _, v := range values {
		best = max(best, v)
	}
	return best
}

func minOf(values []int64) int64 {
	best := values[0]
	for _, v := range values {
		best = min(best, v)
	}
	return best
}

func sumOf(values []int64) int64 {
	var sum int64
	for _, v := range values {
		sum += v
	}
	return sum
}

// Measure reads the measurements of a plan in the reader's transaction. It writes nothing and reads no clock.
func (s *Scheduler) Measure(ctx context.Context, q store.Querier, plan string) (Measurements, error) {
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return Measurements{}, err
	}
	out := Measurements{PlanID: plan, PlanRevision: snap.Revision}
	if out.Landings, err = s.landings(ctx, q, plan, snap); err != nil {
		return Measurements{}, err
	}
	if out.Parallelism, err = measureParallelism(ctx, q, plan); err != nil {
		return Measurements{}, err
	}
	if out.Conflicts, err = measureConflicts(ctx, q, plan); err != nil {
		return Measurements{}, err
	}
	out.Handling = measureHandling(out.Landings)
	out.BaseRefresh = measureBaseRefresh(out.Landings)
	if out.PostMerge, out.Duplicated, err = measureResults(ctx, q, plan, out.Landings); err != nil {
		return Measurements{}, err
	}
	if out.Cancelled, err = measureCancelled(ctx, q, plan, snap); err != nil {
		return Measurements{}, err
	}
	return out, nil
}

// Measurements is Measure inside one read transaction of the store, as Read is for the ready set.
func (s *Scheduler) Measurements(ctx context.Context, plan string) (Measurements, error) {
	var out Measurements
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		out, err = s.Measure(txCtx, s.Store.Q(txCtx), plan)
		return err
	})
	return out, err
}

// measureParallelism reads the passes the parent recorded: the slots held and free at each, and the limit that decided each. It measures recorded passes, not the children running between them.
func measureParallelism(ctx context.Context, q store.Querier, plan string) (contract.OrderedObject, error) {
	rows, err := q.QueryContext(ctx, "SELECT held, free_slots, ceiling, deciding_limit FROM dag_passes WHERE plan_id = ? ORDER BY pass_seq", plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var held, free []int64
	var ceiling int64
	limits := map[string]int{}
	for rows.Next() {
		var h, f, c int64
		var limit string
		if err := rows.Scan(&h, &f, &c, &limit); err != nil {
			return nil, err
		}
		held, free, ceiling = append(held, h), append(free, f), c
		limits[limit]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(held) == 0 {
		return absentMetric(0, AbsentNoRecordedPass), nil
	}
	limitedBy := contract.OrderedObject{{Key: LimitNone, Value: limits[LimitNone]}, {Key: LimitNoCapacity, Value: limits[LimitNoCapacity]}, {Key: LimitEditOverlap, Value: limits[LimitEditOverlap]},
		{Key: LimitCapacityUnmeasured, Value: limits[LimitCapacityUnmeasured]}}
	return presentMetric(len(held),
		contract.Field{Key: "held_slots", Value: contract.OrderedObject{{Key: "latest", Value: held[len(held)-1]}, {Key: "max", Value: maxOf(held)}, {Key: "mean", Value: meanOf(held)}}},
		contract.Field{Key: "free_slots", Value: contract.OrderedObject{{Key: "latest", Value: free[len(free)-1]}, {Key: "min", Value: minOf(free)}, {Key: "mean", Value: meanOf(free)}}},
		contract.Field{Key: "ceiling_latest", Value: ceiling}, contract.Field{Key: "limited_by", Value: limitedBy}), nil
}

// gradeTally is, for one grade, the pull requests that conflicted at it and the files they conflicted in.
type gradeTally struct {
	nodes map[string]bool
	files map[[2]string]bool
}

func newTally() *gradeTally {
	return &gradeTally{nodes: map[string]bool{}, files: map[[2]string]bool{}}
}

func (g *gradeTally) add(node string, files []string) {
	g.nodes[node] = true
	for _, f := range files {
		g.files[[2]string{node, f}] = true
	}
}

func (g *gradeTally) object() contract.OrderedObject {
	var perPR any
	if len(g.nodes) > 0 {
		perPR = round2(float64(len(g.files)) / float64(len(g.nodes)))
	}
	return contract.OrderedObject{{Key: "prs", Value: len(g.nodes)}, {Key: "files", Value: len(g.files)}, {Key: "files_per_pr", Value: perPR}}
}

// measureConflicts counts, per pull request, the files that conflicted, by the grade of the conflict. The measured pull requests are the nodes a conflict sweep (or dag-conflict-observe) measured at
// all; one that no observation found a conflict for has its zero. An observation of two heads is graded as a whole by the declarations as they are now (classify: the worst grade over its files, a
// conflict every file of which a rule settles is mechanical) and each of its files counts at that grade for both nodes; an observation of a head against a tip has no declaration on the other side and
// is counted apart. Hunks are not recorded: an observation keeps the names and the number of the files git could not merge.
func measureConflicts(ctx context.Context, q store.Querier, plan string) (contract.OrderedObject, error) {
	for _, table := range []string{"dag_conflict_sweep_members", "dag_conflict_observations", "dag_tip_conflict_observations"} {
		if ok, err := tableExists(ctx, q, table); err != nil || !ok {
			if err != nil {
				return nil, err
			}
			return absentMetric(0, AbsentTableMissing), nil
		}
	}
	measured := map[string]bool{}
	rows, err := q.QueryContext(ctx, "SELECT left_node_id, right_node_id FROM dag_conflict_sweep_members WHERE plan_id = ? AND status <> 'unmeasured'", plan)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var left, right string
		if err := rows.Scan(&left, &right); err != nil {
			rows.Close()
			return nil, err
		}
		measured[left] = true
		if right != "" {
			measured[right] = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(measured) == 0 {
		return absentMetric(0, AbsentNoObservations), nil
	}
	declarations, err := loadDeclarations(ctx, q, plan)
	if err != nil {
		return nil, err
	}
	tallies := map[string]*gradeTally{GradeMechanical: newTally(), GradeLocal: newTally(), GradeExclusive: newTally()}
	tip := newTally()
	unattributed := 0

	type pairRow struct{ id, left, right string }
	var pairs []pairRow
	prow, err := q.QueryContext(ctx, "SELECT observation_id, left_node_id, right_node_id FROM dag_conflict_observations WHERE plan_id = ? AND conflict_count > 0 ORDER BY observation_id", plan)
	if err != nil {
		return nil, err
	}
	for prow.Next() {
		var p pairRow
		if err := prow.Scan(&p.id, &p.left, &p.right); err != nil {
			prow.Close()
			return nil, err
		}
		pairs = append(pairs, p)
	}
	if err := prow.Err(); err != nil {
		prow.Close()
		return nil, err
	}
	prow.Close()
	for _, p := range pairs {
		names, files, err := observedFiles(ctx, q, false, p.id)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			unattributed++
			continue
		}
		grade, _ := classify(p.left, p.right, declarations, names, files)
		if grade == "" {
			grade = GradeMechanical
		}
		tallies[grade].add(p.left, files)
		tallies[grade].add(p.right, files)
	}

	type tipRow struct{ id, node string }
	var tips []tipRow
	trow, err := q.QueryContext(ctx, "SELECT observation_id, node_id FROM dag_tip_conflict_observations WHERE plan_id = ? AND conflict_count > 0 ORDER BY observation_id", plan)
	if err != nil {
		return nil, err
	}
	for trow.Next() {
		var t tipRow
		if err := trow.Scan(&t.id, &t.node); err != nil {
			trow.Close()
			return nil, err
		}
		tips = append(tips, t)
	}
	if err := trow.Err(); err != nil {
		trow.Close()
		return nil, err
	}
	trow.Close()
	for _, t := range tips {
		_, files, err := observedFiles(ctx, q, true, t.id)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			unattributed++
			continue
		}
		tip.add(t.node, files)
	}
	grades := contract.OrderedObject{{Key: GradeMechanical, Value: tallies[GradeMechanical].object()}, {Key: GradeLocal, Value: tallies[GradeLocal].object()}, {Key: GradeExclusive, Value: tallies[GradeExclusive].object()}}
	return presentMetric(len(measured),
		contract.Field{Key: "by_grade", Value: grades}, contract.Field{Key: "against_tip", Value: tip.object()}, contract.Field{Key: "unattributed_observations", Value: unattributed},
		contract.Field{Key: "hunks", Value: absentMetric(0, AbsentNotRecorded)}), nil
}

// measureHandling is the conflict handling time of the landings that have one: from the first conflict sweep after the node's first acceptance to the landing.
func measureHandling(landings []Landing) contract.OrderedObject {
	if len(landings) == 0 {
		return absentMetric(0, AbsentNoLandings)
	}
	var seconds []int64
	for _, l := range landings {
		if l.HandlingSeconds != nil {
			seconds = append(seconds, *l.HandlingSeconds)
		}
	}
	if len(seconds) == 0 {
		return absentMetric(0, AbsentNoObservations)
	}
	return presentMetric(len(seconds), contract.Field{Key: "median_seconds", Value: medianOf(seconds)}, contract.Field{Key: "mean_seconds", Value: meanOf(seconds)},
		contract.Field{Key: "max_seconds", Value: maxOf(seconds)})
}

// measureBaseRefresh prints the two counts the store keeps of base refresh work per landing: the merge judgements that found the base moved, and the correction generations dag-correct recorded. The
// store records neither why a node was sent back nor when a parent's own refresh began and ended, so the round trips themselves are absent.
func measureBaseRefresh(landings []Landing) contract.OrderedObject {
	counts := func(pick func(Landing) (int64, bool)) contract.OrderedObject {
		if len(landings) == 0 {
			return absentMetric(0, AbsentNoLandings)
		}
		var values []int64
		for _, l := range landings {
			if v, ok := pick(l); ok {
				values = append(values, v)
			}
		}
		if len(values) == 0 {
			return absentMetric(0, AbsentNoObservations)
		}
		return presentMetric(len(values), contract.Field{Key: "total", Value: sumOf(values)}, contract.Field{Key: "mean_per_landing", Value: meanOf(values)}, contract.Field{Key: "max", Value: maxOf(values)})
	}
	stale := counts(func(l Landing) (int64, bool) { return int64(l.StaleBase), l.Judged })
	returns := counts(func(l Landing) (int64, bool) { return int64(l.Returns), true })
	return contract.OrderedObject{{Key: "stale_base_judgements", Value: stale}, {Key: "returns_to_child", Value: returns}, {Key: "round_trips", Value: absentMetric(0, AbsentNotRecorded)}}
}

// measureResults reads what the parent recorded: after the landings (dev green, dev red, reverted) and about the work (duplicate, discarded, counted as distinct nodes per kind).
func measureResults(ctx context.Context, q store.Querier, plan string, landings []Landing) (postMerge, duplicated contract.OrderedObject, err error) {
	has, err := tableExists(ctx, q, "dag_landing_results")
	if err != nil {
		return nil, nil, err
	}
	kinds := map[string]map[string]bool{}
	if has {
		rows, err := q.QueryContext(ctx, "SELECT DISTINCT kind, node_id FROM dag_landing_results WHERE plan_id = ?", plan)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, node string
			if err := rows.Scan(&kind, &node); err != nil {
				return nil, nil, err
			}
			if kinds[kind] == nil {
				kinds[kind] = map[string]bool{}
			}
			kinds[kind][node] = true
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	switch {
	case len(landings) == 0:
		postMerge = absentMetric(0, AbsentNoLandings)
	case !has:
		postMerge = absentMetric(len(landings), AbsentTableMissing)
	default:
		recorded, red, reverted, green := 0, 0, 0, 0
		for _, l := range landings {
			if l.Results[ResultDevGreen] || l.red() || l.reverted() {
				recorded++
			}
			if l.red() {
				red++
			}
			if l.reverted() {
				reverted++
			}
			if l.Results[ResultDevGreen] {
				green++
			}
		}
		if recorded == 0 {
			postMerge = absentMetric(len(landings), AbsentNoneRecorded)
		} else {
			postMerge = presentMetric(len(landings), contract.Field{Key: "with_a_result", Value: recorded}, contract.Field{Key: "dev_green", Value: green},
				contract.Field{Key: "dev_red", Value: red}, contract.Field{Key: "reverted", Value: reverted})
		}
	}
	switch {
	case !has:
		duplicated = absentMetric(0, AbsentTableMissing)
	case len(kinds[ResultDuplicate]) == 0 && len(kinds[ResultDiscarded]) == 0:
		duplicated = absentMetric(0, AbsentNoneRecorded)
	default:
		duplicated = presentMetric(len(kinds[ResultDuplicate])+len(kinds[ResultDiscarded]),
			contract.Field{Key: "duplicate_nodes", Value: len(kinds[ResultDuplicate])}, contract.Field{Key: "discarded_nodes", Value: len(kinds[ResultDiscarded])})
	}
	return postMerge, duplicated, nil
}

// measureCancelled counts the nodes the plan cancelled after a child was created for them: work that was started and then abandoned. It is the plan's own record and not a statement about a pull
// request, so it is kept apart from the duplicated and discarded work the parent states.
func measureCancelled(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot) (contract.OrderedObject, error) {
	cancelled := 0
	for _, n := range snap.Nodes {
		if n.Lifecycle != dag.LifeCancelled {
			continue
		}
		var one int
		started, err := queryOne(ctx, q, "SELECT 1 FROM dag_node_executions WHERE plan_id = ? AND node_id = ? LIMIT 1", []any{plan, n.NodeID}, &one)
		if err != nil {
			return nil, err
		}
		if started {
			cancelled++
		}
	}
	return presentMetric(len(snap.Nodes), contract.Field{Key: "nodes", Value: cancelled}), nil
}

// Object is the measurements as dag-measurements prints them.
func (m Measurements) Object() contract.OrderedObject {
	rows := m.Landings
	if len(rows) > MaxPolicyWindow {
		rows = rows[len(rows)-MaxPolicyWindow:]
	}
	landings := make([]any, len(rows))
	for i, l := range rows {
		var stale any
		if l.Judged {
			stale = l.StaleBase
		}
		results := []any{}
		for _, kind := range ResultKinds {
			if l.Results[kind] {
				results = append(results, kind)
			}
		}
		landings[i] = contract.OrderedObject{{Key: "node_id", Value: l.NodeID}, {Key: "landed_at", Value: l.At}, {Key: "conflict_handling_seconds", Value: optionalSeconds(l.HandlingSeconds)},
			{Key: "stale_base_judgements", Value: stale}, {Key: "returns_to_child", Value: l.Returns}, {Key: "results", Value: results}}
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: SchemaMeasurements}, {Key: "plan_id", Value: m.PlanID}, {Key: "plan_revision", Value: m.PlanRevision},
		{Key: "landed_nodes", Value: len(m.Landings)}, {Key: "parallelism", Value: m.Parallelism}, {Key: "conflicts_by_grade", Value: m.Conflicts}, {Key: "conflict_handling", Value: m.Handling},
		{Key: "base_refresh", Value: m.BaseRefresh}, {Key: "post_merge", Value: m.PostMerge}, {Key: "duplicated_or_discarded", Value: m.Duplicated}, {Key: "cancelled_after_release", Value: m.Cancelled},
		{Key: "landings", Value: landings}}
}

// SchemaMeasurements names the document dag-measurements prints.
const SchemaMeasurements = "dag-measurements/1"
