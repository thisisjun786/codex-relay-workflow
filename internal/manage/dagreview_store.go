package manage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay store a review reads. The file name is the relay-read helper's (CRW-836), so this
// package spells the store's location once.
const dagReviewStoreFile = relayReadStoreFile

// dagReviewStore is the read-only handle on the relay store. It opens through the relay-read
// helper's store.OpenInPlace, so a review creates neither the write-ahead log nor the
// shared-memory index beside a store whose log holds no frame, and it holds no schema of its own:
// a table a store lacks is recorded as an unmeasured check rather than repaired.
type dagReviewStore struct {
	handle *store.ReadOnly
	// db is where this review's list queries run: the read-only handle at rest, and the one
	// deferred snapshot's querier while dagReviewSnapshot runs, so every list reading sees the
	// same store state as the scheduler's own reading of the plan taken beside it.
	db store.Querier
}

// dagReviewOpenStore opens the store at state read-only. A missing file is an error, because a
// review of nothing is not a clean review. The open is the relay-read helper's, so this package
// holds one read-only open rule rather than a second copy of it.
func dagReviewOpenStore(ctx context.Context, state string) (*dagReviewStore, error) {
	handle, err := relayReadOpenStore(ctx, state)
	if err != nil {
		return nil, err
	}
	return &dagReviewStore{handle: handle, db: handle}, nil
}

// Close releases the read-only handle.
func (s *dagReviewStore) Close() error { return s.handle.Close() }

// dagReviewSnapshot runs one reading inside the store's single deferred snapshot and points this
// review's list queries at that snapshot's querier for the duration: the scheduler's own reading
// of a plan (dagsched.Scheduler.Progress) and the list queries around it then see one store state,
// so a plan cannot be read at two revisions. The store's path is set because the assignment view
// the scheduler asks derives the store directory from it, and a bare snapshot store carries none.
func (s *dagReviewStore) dagReviewSnapshot(ctx context.Context, state string, run func(context.Context, *store.Store) error) error {
	return s.handle.ReadSnapshot(ctx, func(ctx context.Context, st *store.Store) error {
		st.Path = relayReadStorePath(state)
		previous := s.db
		s.db = st.Q(ctx)
		defer func() { s.db = previous }()
		return run(ctx, st)
	})
}

// hasTable reports whether the store carries a table. The zone is additive, so a store written
// before a later CRW lacks tables this review would otherwise read.
func (s *dagReviewStore) hasTable(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// dagReviewRows runs one query and scans every row through scan.
func dagReviewRows[T any](ctx context.Context, db store.Querier, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// The facts a review reads beside the scheduler's own reading of the plan. A plan's nodes, their
// stages, their executions and their integration are NOT read here: those are Progress's. What is
// read here is the list material the six checks need that the scheduler does not project (the plan
// list, the live edges, the region declarations, the plan's children, the merge lanes) plus the two
// row sets a check asks about by target: which acceptance belongs to which relationship, and where
// and when a landing was observed.
type (
	dagReviewPlanRef    struct{ planID, projectKey, createdAt string }
	dagReviewEdge       struct{ from, to, kind string }
	dagReviewAcceptance struct {
		acceptanceID, nodeID, relationshipID string
	}
	dagReviewObservation struct {
		acceptanceID, observedAt, repository, baseRef string
	}
	dagReviewRegion struct {
		nodeID, repository, path, kind, key, change string
		exclusive                                   bool
		grade, rule                                 string
	}
	dagReviewLaneTurn struct {
		turnID, targetKey, holder, state, relationshipID string
		repository, baseRef                              string
		prNumber                                         int
		closedAt, updatedAt, heldAt                      string
		waiters                                          int
	}
	dagReviewChild     struct{ relationshipID, issueKey, createdAt string }
	dagReviewExecution struct{ nodeID, relationshipID string }
)

// dagReviewFacts is everything one plan's review needs.
type dagReviewFacts struct {
	plan dagReviewPlanRef
	// progress is the relay's own reading of the plan. Every judgement of execution, integration
	// and release ownership is taken from it, so this review holds no second state engine.
	progress     dagsched.Progress
	edges        []dagReviewEdge
	acceptances  []dagReviewAcceptance
	observations []dagReviewObservation
	regions      []dagReviewRegion
	children     []dagReviewChild
	executions   []dagReviewExecution
}

// dagReviewReadPlans reads the plan refs the review covers: the configured ids, or every plan.
func (s *dagReviewStore) dagReviewReadPlans(ctx context.Context, wanted []string) ([]dagReviewPlanRef, error) {
	query := "SELECT plan_id, project_key, created_at FROM dag_plans"
	var args []any
	if len(wanted) > 0 {
		query += " WHERE plan_id IN (" + dagReviewPlaceholders(len(wanted)) + ")"
		for _, id := range wanted {
			args = append(args, id)
		}
	}
	query += " ORDER BY plan_id"
	return dagReviewRows(ctx, s.db, query, args, func(rows *sql.Rows) (dagReviewPlanRef, error) {
		var ref dagReviewPlanRef
		err := rows.Scan(&ref.planID, &ref.projectKey, &ref.createdAt)
		return ref, err
	})
}

// dagReviewReadFacts reads one plan: the scheduler's own reading of it first, then the list
// material the checks need. A table the zone predates is skipped and named in checks; a query that
// fails for any other reason is an error.
func (s *dagReviewStore) dagReviewReadFacts(ctx context.Context, st *store.Store, plan dagReviewPlanRef, checks *[]Check) (dagReviewFacts, error) {
	facts := dagReviewFacts{plan: plan}
	var err error
	if facts.progress, err = (&dagsched.Scheduler{Store: st}).Progress(ctx, st.Q(ctx), plan.planID); err != nil {
		return facts, err
	}
	if facts.edges, err = s.dagReviewEdges(ctx, plan.planID); err != nil {
		return facts, err
	}
	if facts.acceptances, err = s.dagReviewAcceptances(ctx, plan.planID); err != nil {
		return facts, err
	}
	if facts.observations, err = s.dagReviewObservations(ctx, plan.planID); err != nil {
		return facts, err
	}
	if facts.regions, err = s.dagReviewRegions(ctx, plan.planID, checks); err != nil {
		return facts, err
	}
	if facts.children, err = s.dagReviewChildren(ctx, plan); err != nil {
		return facts, err
	}
	if facts.executions, err = s.dagReviewExecutions(ctx, plan.planID); err != nil {
		return facts, err
	}
	return facts, nil
}

// dagReviewExecutions reads which relationship each node of the plan was released to. It is the
// link the host reading uses to decide whether an active child belongs to this plan.
func (s *dagReviewStore) dagReviewExecutions(ctx context.Context, planID string) ([]dagReviewExecution, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT node_id, relationship_id FROM dag_node_executions WHERE plan_id = ? ORDER BY node_id, relationship_id",
		[]any{planID}, func(rows *sql.Rows) (dagReviewExecution, error) {
			var execution dagReviewExecution
			err := rows.Scan(&execution.nodeID, &execution.relationshipID)
			return execution, err
		})
}

// dagReviewEdges reads the live edges of the plan's head revision, which is the plan shape's edge
// count. No edge is judged here: whether one is satisfied, and when it became so, is the
// scheduler's reading, and it exports no reading of that time yet.
func (s *dagReviewStore) dagReviewEdges(ctx context.Context, planID string) ([]dagReviewEdge, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT from_node_id, to_node_id, kind FROM dag_edges WHERE plan_id = ? AND retired_rev IS NULL ORDER BY edge_id",
		[]any{planID}, func(rows *sql.Rows) (dagReviewEdge, error) {
			var edge dagReviewEdge
			err := rows.Scan(&edge.from, &edge.to, &edge.kind)
			return edge, err
		})
}

// dagReviewAcceptances reads the plan's active acceptances, which is how a merge turn's
// relationship is tied to the node it landed. The acceptance's own integration is not judged here.
func (s *dagReviewStore) dagReviewAcceptances(ctx context.Context, planID string) ([]dagReviewAcceptance, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT acceptance_id, node_id, relationship_id FROM dag_acceptances WHERE plan_id = ? AND state = 'active' ORDER BY acceptance_id",
		[]any{planID}, func(rows *sql.Rows) (dagReviewAcceptance, error) {
			var acceptance dagReviewAcceptance
			err := rows.Scan(&acceptance.acceptanceID, &acceptance.nodeID, &acceptance.relationshipID)
			return acceptance, err
		})
}

// dagReviewObservations reads the target and the instant of every integration observation the
// plan's acceptances carry. What an observation says about containment is not read: this review
// asks only whether a landing was observed in the target the turn landed on.
func (s *dagReviewStore) dagReviewObservations(ctx context.Context, planID string) ([]dagReviewObservation, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT o.acceptance_id, o.repository, o.base_ref, o.observed_at"+
			" FROM dag_integration_observations o JOIN dag_acceptances a ON a.acceptance_id = o.acceptance_id"+
			" WHERE a.plan_id = ? ORDER BY o.acceptance_id, o.observed_seq",
		[]any{planID}, func(rows *sql.Rows) (dagReviewObservation, error) {
			var observation dagReviewObservation
			err := rows.Scan(&observation.acceptanceID, &observation.repository, &observation.baseRef, &observation.observedAt)
			return observation, err
		})
}

// dagReviewRegions reads the latest declaration of every node, with its grade and its stated hold
// where those tables exist. The hold is mirrored the way the scheduler reads it: a stated row
// decides; without one the stored exclusive flag holds the whole repository only for a rename,
// because a delete or a hotspot protected its own place alone. The grade of a pair is not derived
// here: dagReviewExclusivePair asks the scheduler's own PairGrade.
func (s *dagReviewStore) dagReviewRegions(ctx context.Context, planID string, checks *[]Check) ([]dagReviewRegion, error) {
	columns, join := ", '', ''", ""
	graded, err := s.hasTable(ctx, "dag_node_region_grades")
	if err != nil {
		return nil, err
	}
	if !graded {
		*checks = append(*checks, Check{Name: "dag_node_region_grades", State: dagReviewUnmeasured, Detail: "the store predates the region grade table"})
	} else {
		columns = ", COALESCE(g.grade, ''), COALESCE(g.rule, '')"
		join = " LEFT JOIN dag_node_region_grades g ON g.plan_id = r.plan_id AND g.node_id = r.node_id AND g.declaration_seq = r.declaration_seq AND g.repository = r.repository AND g.path = r.path AND g.region_kind = r.region_kind AND g.region_key = r.region_key"
	}
	stated := ", -1"
	held, err := s.hasTable(ctx, "dag_node_region_holds")
	if err != nil {
		return nil, err
	}
	if !held {
		*checks = append(*checks, Check{Name: "dag_node_region_holds", State: dagReviewUnmeasured, Detail: "the store predates the region hold table"})
	} else {
		stated = ", COALESCE(h.stated, -1)"
		join += " LEFT JOIN dag_node_region_holds h ON h.plan_id = r.plan_id AND h.node_id = r.node_id AND h.declaration_seq = r.declaration_seq AND h.repository = r.repository AND h.path = r.path AND h.region_kind = r.region_kind AND h.region_key = r.region_key"
	}
	query := "SELECT r.node_id, r.repository, r.path, r.region_kind, r.region_key, r.change, r.exclusive" + columns + stated +
		" FROM dag_node_regions r" + join +
		" WHERE r.plan_id = ? AND r.declaration_seq = (SELECT MAX(declaration_seq) FROM dag_node_regions m WHERE m.plan_id = r.plan_id AND m.node_id = r.node_id)" +
		" ORDER BY r.node_id, r.repository, r.path, r.region_kind, r.region_key"
	return dagReviewRows(ctx, s.db, query, []any{planID}, func(rows *sql.Rows) (dagReviewRegion, error) {
		var region dagReviewRegion
		var exclusive, statedHold int
		if err := rows.Scan(&region.nodeID, &region.repository, &region.path, &region.kind, &region.key, &region.change, &exclusive, &region.grade, &region.rule, &statedHold); err != nil {
			return region, err
		}
		if statedHold >= 0 {
			region.exclusive = statedHold == 1
		} else {
			place, _ := dagsched.Classify(region.path, region.change)
			region.exclusive = exclusive == 1 && (region.change == "rename" || !place)
		}
		return region, nil
	})
}

// dagReviewChildren reads the plan project's live relationships that were opened after the plan
// was created and that no plan ever executed. A relationship opened before the plan was released
// by hand on purpose, so it is not a bypass of this plan; one another plan of the same project
// executed is that plan's child, not an unreleased one here; and a paused relationship is still
// live (registry.isLive), so it is included.
func (s *dagReviewStore) dagReviewChildren(ctx context.Context, plan dagReviewPlanRef) ([]dagReviewChild, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT r.relationship_id, r.issue_key, r.created_at FROM relationships r"+
			" JOIN relationship_scope s ON s.relationship_id = r.relationship_id"+
			" WHERE s.project_key = ? AND r.status IN ('active', 'paused') AND r.created_at > ?"+
			" AND NOT EXISTS (SELECT 1 FROM dag_node_executions e WHERE e.relationship_id = r.relationship_id)"+
			" ORDER BY r.issue_key",
		[]any{plan.projectKey, plan.createdAt}, func(rows *sql.Rows) (dagReviewChild, error) {
			var child dagReviewChild
			err := rows.Scan(&child.relationshipID, &child.issueKey, &child.createdAt)
			return child, err
		})
}

// dagReviewReadLanes reads the merge turns and the number of turns waiting on the same target.
// The turn's own repository and base ref are read with it, because a landing is observed in the
// branch it landed on and an observation of another target does not resolve the turn. When the
// section names plans, a turn is kept only when its relationship executes a node of one of them,
// so a narrowed review cannot report another plan's lane.
func (s *dagReviewStore) dagReviewReadLanes(ctx context.Context, plans []string) ([]dagReviewLaneTurn, error) {
	query := "SELECT t.turn_id, t.target_key, t.holder_task_id, t.state, COALESCE(t.pr_number, 0), COALESCE(t.relationship_id, '')," +
		" t.repository, t.base_ref, COALESCE(t.closed_at, ''), t.updated_at, COALESCE(t.held_at, '')," +
		" (SELECT COUNT(*) FROM merge_turns w WHERE w.target_key = t.target_key AND w.state = 'waiting')" +
		" FROM merge_turns t"
	var args []any
	if len(plans) > 0 {
		query += " WHERE EXISTS (SELECT 1 FROM dag_node_executions e WHERE e.relationship_id = t.relationship_id AND e.plan_id IN (" + dagReviewPlaceholders(len(plans)) + "))"
		for _, plan := range plans {
			args = append(args, plan)
		}
	}
	query += " ORDER BY t.turn_id"
	return dagReviewRows(ctx, s.db, query, args, func(rows *sql.Rows) (dagReviewLaneTurn, error) {
		var turn dagReviewLaneTurn
		err := rows.Scan(&turn.turnID, &turn.targetKey, &turn.holder, &turn.state, &turn.prNumber,
			&turn.relationshipID, &turn.repository, &turn.baseRef, &turn.closedAt, &turn.updatedAt, &turn.heldAt, &turn.waiters)
		return turn, err
	})
}

// dagReviewExecutionExists reports whether the plan recorded a DAG execution for a relationship.
func (s *dagReviewStore) dagReviewExecutionExists(ctx context.Context, planID, relationshipID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM dag_node_executions WHERE plan_id = ? AND relationship_id = ? LIMIT 1", planID, relationshipID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// dagReviewPlaceholders is n "?"s for an IN list.
func dagReviewPlaceholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ", "
		}
		out += "?"
	}
	return out
}

// dagReviewInstant parses a stored relay timestamp; the second result is false when the column
// is empty or in a form this build does not know, which is not a failure to read the store.
func dagReviewInstant(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000000+00:00", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
