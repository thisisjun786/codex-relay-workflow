package manage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The relay store a review reads: its file name below the state directory and the busy timeout
// a read waits for a writer with.
const (
	dagReviewStoreFile  = "relay.sqlite3"
	dagReviewBusyMillis = 5000
)

// dagReviewStore is the read-only handle on the relay store. It opens mode=ro with query_only,
// so a review can neither create a table, migrate a schema nor write a row, and it holds no
// schema of its own: a table a store lacks is recorded as an unmeasured check rather than
// repaired. It is deliberately not store.Open, which is allowed to install the schema.
type dagReviewStore struct{ db *sql.DB }

// dagReviewOpenStore opens the store at state read-only. A missing file is an error, because a
// review of nothing is not a clean review.
func dagReviewOpenStore(ctx context.Context, state string) (*dagReviewStore, error) {
	if state == "" {
		return nil, errors.New("the relay state directory is not configured")
	}
	path := filepath.Join(state, dagReviewStoreFile)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("relay store: %w", err)
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_busy_timeout", fmt.Sprint(dagReviewBusyMillis))
	q.Set("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	connector, err := sqlite.NewConnector(u.String())
	if err != nil {
		return nil, fmt.Errorf("relay store: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("relay store: %w", err)
	}
	return &dagReviewStore{db: db}, nil
}

// Close releases the read-only handle.
func (s *dagReviewStore) Close() error { return s.db.Close() }

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
func dagReviewRows[T any](ctx context.Context, db *sql.DB, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
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

// The facts a review reads, one row per table. A node is one live node of the plan's head
// revision; an edge keeps the instant its revision was recorded, because an edge added after a
// node was released orders the merge rather than the release.
type (
	dagReviewPlanRef     struct{ planID, projectKey, createdAt string }
	dagReviewNode        struct{ nodeID, issueKey string }
	dagReviewEdge        struct{ from, to, kind, introducedAt string }
	dagReviewRelease     struct{ nodeID, decidedAt string }
	dagReviewAcceptance  struct{ acceptanceID, nodeID, relationshipID, acceptedAt string }
	dagReviewObservation struct {
		acceptanceID, observedAt, revertedBy string
		isAncestor                           bool
	}
	dagReviewRegion struct {
		nodeID, repository, path, kind, key, change string
		exclusive                                   bool
		grade, rule                                 string
	}
	dagReviewLaneTurn struct {
		turnID, targetKey, holder, state, relationshipID string
		prNumber                                         int
		closedAt, updatedAt, heldAt                      string
		waiters                                          int
	}
	dagReviewChild struct{ relationshipID, issueKey, createdAt string }
)

// dagReviewFacts is everything one plan's review needs.
type dagReviewFacts struct {
	plan         dagReviewPlanRef
	revision     int
	nodes        []dagReviewNode
	edges        []dagReviewEdge
	releases     []dagReviewRelease
	acceptances  []dagReviewAcceptance
	observations []dagReviewObservation
	regions      []dagReviewRegion
	children     []dagReviewChild
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

// dagReviewReadFacts reads one plan's facts. A table the zone predates is skipped and named in
// checks; a query that fails for any other reason is an error.
func (s *dagReviewStore) dagReviewReadFacts(ctx context.Context, plan dagReviewPlanRef, checks *[]Check) (dagReviewFacts, error) {
	facts := dagReviewFacts{plan: plan}
	var err error
	if facts.revision, err = s.dagReviewHead(ctx, plan.planID); err != nil {
		return facts, err
	}
	if facts.nodes, err = s.dagReviewNodes(ctx, plan.planID); err != nil {
		return facts, err
	}
	if facts.edges, err = s.dagReviewEdges(ctx, plan.planID); err != nil {
		return facts, err
	}
	if facts.releases, err = s.dagReviewReleases(ctx, plan.planID); err != nil {
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
	return facts, nil
}

func (s *dagReviewStore) dagReviewHead(ctx context.Context, planID string) (int, error) {
	var head sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(revision_no) FROM dag_plan_revisions WHERE plan_id = ?", planID).Scan(&head)
	if err != nil {
		return 0, err
	}
	return int(head.Int64), nil
}

func (s *dagReviewStore) dagReviewNodes(ctx context.Context, planID string) ([]dagReviewNode, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT node_id, issue_key FROM dag_nodes WHERE plan_id = ? AND retired_rev IS NULL ORDER BY node_id",
		[]any{planID}, func(rows *sql.Rows) (dagReviewNode, error) {
			var node dagReviewNode
			err := rows.Scan(&node.nodeID, &node.issueKey)
			return node, err
		})
}

func (s *dagReviewStore) dagReviewEdges(ctx context.Context, planID string) ([]dagReviewEdge, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT e.from_node_id, e.to_node_id, e.kind, COALESCE(r.recorded_at, '') FROM dag_edges e"+
			" LEFT JOIN dag_plan_revisions r ON r.plan_id = e.plan_id AND r.revision_no = e.introduced_rev"+
			" WHERE e.plan_id = ? AND e.retired_rev IS NULL ORDER BY e.edge_id",
		[]any{planID}, func(rows *sql.Rows) (dagReviewEdge, error) {
			var edge dagReviewEdge
			err := rows.Scan(&edge.from, &edge.to, &edge.kind, &edge.introducedAt)
			return edge, err
		})
}

func (s *dagReviewStore) dagReviewReleases(ctx context.Context, planID string) ([]dagReviewRelease, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT node_id, decided_at FROM dag_releases WHERE plan_id = ? ORDER BY decided_at",
		[]any{planID}, func(rows *sql.Rows) (dagReviewRelease, error) {
			var release dagReviewRelease
			err := rows.Scan(&release.nodeID, &release.decidedAt)
			return release, err
		})
}

func (s *dagReviewStore) dagReviewAcceptances(ctx context.Context, planID string) ([]dagReviewAcceptance, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT acceptance_id, node_id, relationship_id, accepted_at FROM dag_acceptances WHERE plan_id = ? AND state = 'active' ORDER BY acceptance_id",
		[]any{planID}, func(rows *sql.Rows) (dagReviewAcceptance, error) {
			var acceptance dagReviewAcceptance
			err := rows.Scan(&acceptance.acceptanceID, &acceptance.nodeID, &acceptance.relationshipID, &acceptance.acceptedAt)
			return acceptance, err
		})
}

func (s *dagReviewStore) dagReviewObservations(ctx context.Context, planID string) ([]dagReviewObservation, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT o.acceptance_id, o.is_ancestor, o.reverted_by, o.observed_at"+
			" FROM dag_integration_observations o JOIN dag_acceptances a ON a.acceptance_id = o.acceptance_id"+
			" WHERE a.plan_id = ? ORDER BY o.observed_at",
		[]any{planID}, func(rows *sql.Rows) (dagReviewObservation, error) {
			var observation dagReviewObservation
			var ancestor int
			var reverted sql.NullString
			if err := rows.Scan(&observation.acceptanceID, &ancestor, &reverted, &observation.observedAt); err != nil {
				return observation, err
			}
			observation.isAncestor = ancestor == 1
			observation.revertedBy = reverted.String
			return observation, nil
		})
}

// dagReviewRegions reads the latest declaration of every node, with its grade and its stated
// hold where those tables exist. The hold is mirrored the way the scheduler reads it: a stated
// row decides; without one the stored exclusive flag holds the whole repository only for a
// rename, because a delete or a hotspot protected its own place alone.
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

// dagReviewChildren reads the plan project's relationships that were opened after the plan was
// created. A relationship opened before the plan was released by hand on purpose, so it is not
// a bypass of this plan.
func (s *dagReviewStore) dagReviewChildren(ctx context.Context, plan dagReviewPlanRef) ([]dagReviewChild, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT r.relationship_id, r.issue_key, r.created_at FROM relationships r"+
			" JOIN relationship_scope s ON s.relationship_id = r.relationship_id"+
			" WHERE s.project_key = ? AND r.status = 'active' AND r.created_at > ? ORDER BY r.issue_key",
		[]any{plan.projectKey, plan.createdAt}, func(rows *sql.Rows) (dagReviewChild, error) {
			var child dagReviewChild
			err := rows.Scan(&child.relationshipID, &child.issueKey, &child.createdAt)
			return child, err
		})
}

// dagReviewReadLanes reads every merge turn and the number of turns waiting on the same target.
func (s *dagReviewStore) dagReviewReadLanes(ctx context.Context) ([]dagReviewLaneTurn, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT t.turn_id, t.target_key, t.holder_task_id, t.state, COALESCE(t.pr_number, 0), COALESCE(t.relationship_id, ''),"+
			" COALESCE(t.closed_at, ''), t.updated_at, COALESCE(t.held_at, ''),"+
			" (SELECT COUNT(*) FROM merge_turns w WHERE w.target_key = t.target_key AND w.state = 'waiting')"+
			" FROM merge_turns t ORDER BY t.turn_id",
		nil, func(rows *sql.Rows) (dagReviewLaneTurn, error) {
			var turn dagReviewLaneTurn
			err := rows.Scan(&turn.turnID, &turn.targetKey, &turn.holder, &turn.state, &turn.prNumber,
				&turn.relationshipID, &turn.closedAt, &turn.updatedAt, &turn.heldAt, &turn.waiters)
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
