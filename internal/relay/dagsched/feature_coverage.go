package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// SchemaFeatureCoverage names the document dag-feature-coverage answers (CRW-839).
const SchemaFeatureCoverage = "dag-feature-coverage/1"

// FeatureCoverage is the reading of one feature issue: the packets that deliver it, each packet's
// acceptance and integration, and the packet that owns each of the feature's declared criteria. A
// feature is complete only when every required criterion is covered by a packet that is integrated.
// Nothing here is written: the reading is the plan and the execution rows as they stand.
type FeatureCoverage struct {
	PlanID   string
	IssueKey string
	Packets  []CoveragePacket
	Criteria []CoverageCriterion
	Complete bool
}

// CoveragePacket is one packet of the feature: the plan node that carries it, the relationship it was
// released under, and what that relationship's output has reached.
type CoveragePacket struct {
	NodeID         string
	PacketID       string
	Covers         []string
	Owns           []string
	RelationshipID string
	Acceptance     *CoverageAcceptance
	Integration    *CoverageIntegration
}

// CoverageAcceptance is the packet's active acceptance: the head the parent accepted, and the pull
// request it was accepted on.
type CoverageAcceptance struct {
	AcceptanceID string
	HeadSHA      string
	PRNumber     *int64
	State        string
}

// CoverageIntegration is how the packet's output was integrated: CRW-768's member mapping (the bundle
// the pull request rode, its member head and the commit the bundle landed), or a landing the relay
// observed directly.
type CoverageIntegration struct {
	Method     string
	TrainID    string
	MemberHead string
	LandedSHA  string
	LandedAt   string
}

// CoverageCriterion is one criterion of the feature: who takes it, who owns it, and whether a packet
// that takes it is integrated.
type CoverageCriterion struct {
	ID         string
	Title      string
	Required   bool
	CoveredBy  []string
	Owner      string
	Integrated bool
}

// FeatureCoverage reads the feature issue of a plan (dag-feature-coverage). An issue whose plan declares
// no criteria is read from the criteria the relay registered for the node's relationship, which is what
// a plan without packets has always been judged by.
func (s *Scheduler) FeatureCoverage(ctx context.Context, plan, issue string) (out FeatureCoverage, err error) {
	out = FeatureCoverage{PlanID: plan, IssueKey: issue}
	err = s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		snap, _, err := dag.SnapshotAt(txCtx, q, plan, 0)
		if err != nil {
			return err
		}
		var nodes []dag.SnapNode
		for _, n := range snap.Nodes {
			if n.IssueKey == issue && n.Kind == dag.NodeImplementation && n.Lifecycle == "" {
				nodes = append(nodes, n)
			}
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
		if len(nodes) == 0 {
			return refuse(contract.RefusalUnregisteredScope, "plan %s holds no live implementation node for issue %s", plan, issue)
		}
		for _, n := range nodes {
			p := CoveragePacket{NodeID: n.NodeID, PacketID: n.PacketID, Covers: n.Covers, Owns: n.Owns}
			if p.RelationshipID, err = latestRelationshipOf(txCtx, q, plan, n.NodeID); err != nil {
				return err
			}
			if p.RelationshipID != "" {
				if p.Acceptance, err = activeAcceptanceOf(txCtx, q, p.RelationshipID); err != nil {
					return err
				}
				if p.Integration, err = integrationOf(txCtx, q, p.RelationshipID); err != nil {
					return err
				}
			}
			out.Packets = append(out.Packets, p)
		}
		criteria, err := coverageCriteria(txCtx, q, snap, issue, nodes, out.Packets)
		if err != nil {
			return err
		}
		out.Criteria = criteria
		out.Complete = true
		for _, c := range criteria {
			if c.Required && !c.Integrated {
				out.Complete = false
			}
		}
		return nil
	})
	return out, err
}

// coverageCriteria is the feature's criteria with who takes, owns and has integrated each one. The plan's
// declaration is the source when there is one; otherwise the criteria the relay registered for the
// issue's nodes are, which is how a plan without packets has always been judged.
func coverageCriteria(ctx context.Context, q store.Querier, snap dag.Snapshot, issue string, nodes []dag.SnapNode, packets []CoveragePacket) ([]CoverageCriterion, error) {
	type declared struct {
		title    string
		required bool
	}
	set := map[string]declared{}
	var order []string
	for _, row := range snap.FeatureCriteria {
		if row.IssueKey != issue {
			continue
		}
		for _, c := range row.Criteria {
			if _, seen := set[c.ID]; !seen {
				order = append(order, c.ID)
			}
			set[c.ID] = declared{title: c.Title, required: c.Required}
		}
	}
	registered := len(set) == 0
	if registered {
		for _, n := range nodes {
			relationship, err := latestRelationshipOf(ctx, q, snap.PlanID, n.NodeID)
			if err != nil {
				return nil, err
			}
			if relationship == "" {
				continue
			}
			rows, err := q.QueryContext(ctx, "SELECT criterion_id, title, required FROM canonical_criteria WHERE relationship_id = ? ORDER BY criterion_id", relationship)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id, title string
				var required int64
				if err := rows.Scan(&id, &title, &required); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if _, seen := set[id]; !seen {
					order = append(order, id)
				}
				set[id] = declared{title: title, required: required != 0}
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
		}
		sort.Strings(order)
	}
	byNode := map[string]CoveragePacket{}
	for _, p := range packets {
		byNode[p.NodeID] = p
	}
	out := make([]CoverageCriterion, 0, len(order))
	for _, id := range order {
		c := CoverageCriterion{ID: id, Title: set[id].title, Required: set[id].required}
		for _, n := range nodes {
			p := byNode[n.NodeID]
			covers := registered || containsID(p.Covers, id)
			if !covers {
				continue
			}
			c.CoveredBy = append(c.CoveredBy, n.NodeID)
			if containsID(p.Owns, id) {
				c.Owner = n.NodeID
			}
			if p.Integration != nil {
				c.Integrated = true
			}
		}
		if c.Owner == "" && len(c.CoveredBy) == 1 {
			c.Owner = c.CoveredBy[0]
		}
		out = append(out, c)
	}
	return out, nil
}

func containsID(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// latestRelationshipOf is the relationship of a node's newest execution generation: the child the node
// was last released to. Empty when the node was never released.
func latestRelationshipOf(ctx context.Context, q store.Querier, plan, node string) (string, error) {
	var relationship string
	found, err := queryOne(ctx, q, "SELECT relationship_id FROM dag_node_executions WHERE plan_id = ? AND node_id = ? ORDER BY execution_generation DESC LIMIT 1", []any{plan, node}, &relationship)
	if err != nil || !found {
		return "", err
	}
	return relationship, nil
}

// activeAcceptanceOf is the acceptance that stands for a relationship now.
func activeAcceptanceOf(ctx context.Context, q store.Querier, relationship string) (*CoverageAcceptance, error) {
	var a CoverageAcceptance
	var pr sql.NullInt64
	var head sql.NullString
	found, err := queryOne(ctx, q, "SELECT acceptance_id, head_sha, pr_number, state FROM dag_acceptances WHERE relationship_id = ? AND state = 'active' ORDER BY accepted_at DESC LIMIT 1",
		[]any{relationship}, &a.AcceptanceID, &head, &pr, &a.State)
	if err != nil || !found {
		return nil, err
	}
	a.HeadSHA = head.String
	if pr.Valid {
		n := pr.Int64
		a.PRNumber = &n
	}
	return &a, nil
}

// integrationOf is how a relationship's output was integrated: the bundle CRW-768 carried it in and the
// commit that bundle landed, or a landing the relay observed directly for the node's active acceptance.
// It is nil when nothing shows the output landed.
func integrationOf(ctx context.Context, q store.Querier, relationship string) (*CoverageIntegration, error) {
	var train, memberHead, detail, at string
	found, err := queryOne(ctx, q, "SELECT m.train_id, m.member_head, e.detail_json, e.recorded_at FROM merge_train_members m"+
		" JOIN merge_train_events e ON e.train_id = m.train_id AND e.kind = 'landed'"+
		" WHERE m.relationship_id = ? AND (SELECT kind FROM merge_train_events x WHERE x.train_id = m.train_id ORDER BY x.seq DESC LIMIT 1) <> 'abandoned'"+
		" ORDER BY e.recorded_at DESC LIMIT 1", []any{relationship}, &train, &memberHead, &detail, &at)
	if err != nil {
		return nil, err
	}
	if found {
		return &CoverageIntegration{Method: "merge_train", TrainID: train, MemberHead: memberHead, LandedSHA: detailString(detail, "landedSha"), LandedAt: at}, nil
	}
	var subject, observed string
	found, err = queryOne(ctx, q, "SELECT o.subject_sha, o.observed_at FROM dag_integration_observations o"+
		" JOIN dag_acceptances a ON a.acceptance_id = o.acceptance_id"+
		" WHERE a.relationship_id = ? AND a.state = 'active' AND o.is_ancestor = 1 AND o.reverted_by IS NULL"+
		" ORDER BY o.observed_seq DESC LIMIT 1", []any{relationship}, &subject, &observed)
	if err != nil || !found {
		return nil, err
	}
	return &CoverageIntegration{Method: "integration_observation", LandedSHA: subject, LandedAt: observed}, nil
}

// detailString is one string field of a stored event detail, empty when it is absent or not a string.
func detailString(detail, key string) string {
	var doc map[string]any
	if err := json.Unmarshal([]byte(detail), &doc); err != nil {
		return ""
	}
	s, _ := doc[key].(string)
	return s
}

func runFeatureCoverage(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services, args)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	cov, err := sched.FeatureCoverage(ctx, args.Text("plan"), args.Text("issue"))
	if err != nil {
		return nil, hostFailure(err)
	}
	return coverageAnswer(cov), nil
}

// coverageAnswer is the relay's JSON envelope for dag-feature-coverage.
func coverageAnswer(cov FeatureCoverage) contract.OrderedObject {
	packets := make([]any, len(cov.Packets))
	for i, p := range cov.Packets {
		var acceptance, integration any
		if p.Acceptance != nil {
			acceptance = contract.OrderedObject{
				{Key: "acceptance_id", Value: p.Acceptance.AcceptanceID}, {Key: "head_sha", Value: nullableText(p.Acceptance.HeadSHA)},
				{Key: "pr_number", Value: p.Acceptance.PRNumber}, {Key: "state", Value: p.Acceptance.State},
			}
		}
		if p.Integration != nil {
			integration = contract.OrderedObject{
				{Key: "method", Value: p.Integration.Method}, {Key: "train_id", Value: nullableText(p.Integration.TrainID)},
				{Key: "member_head", Value: nullableText(p.Integration.MemberHead)}, {Key: "landed_sha", Value: nullableText(p.Integration.LandedSHA)},
				{Key: "landed_at", Value: nullableText(p.Integration.LandedAt)},
			}
		}
		packets[i] = contract.OrderedObject{
			{Key: "node_id", Value: p.NodeID}, {Key: "packet_id", Value: nullableText(p.PacketID)},
			{Key: "covers", Value: idList(p.Covers)}, {Key: "owns", Value: idList(p.Owns)},
			{Key: "relationship_id", Value: nullableText(p.RelationshipID)},
			{Key: "acceptance", Value: acceptance}, {Key: "integration", Value: integration},
		}
	}
	criteria := make([]any, len(cov.Criteria))
	for i, c := range cov.Criteria {
		criteria[i] = contract.OrderedObject{
			{Key: "id", Value: c.ID}, {Key: "title", Value: nullableText(c.Title)}, {Key: "required", Value: c.Required},
			{Key: "covered_by", Value: idList(c.CoveredBy)}, {Key: "owner", Value: nullableText(c.Owner)}, {Key: "integrated", Value: c.Integrated},
		}
	}
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: SchemaFeatureCoverage},
		{Key: "plan_id", Value: cov.PlanID}, {Key: "issue_key", Value: cov.IssueKey},
		{Key: "complete", Value: cov.Complete},
		{Key: "packets", Value: packets}, {Key: "criteria", Value: criteria},
	}
}

func idList(list []string) any {
	if len(list) == 0 {
		return []any{}
	}
	sorted := append([]string(nil), list...)
	sort.Strings(sorted)
	out := make([]any, len(sorted))
	for i, s := range sorted {
		out[i] = s
	}
	return out
}

func nullableText(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
