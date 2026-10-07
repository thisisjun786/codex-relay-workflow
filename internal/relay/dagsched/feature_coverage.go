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
			// The execution that stands for this node now: the live relationship whose manifest was built
			// for the node version the plan holds. A node the plan has since edited has no current
			// execution, so its packet reports none and credits nothing (CRW-839 review: a packet whose
			// covers moved must not be credited by the pull request that was released for the old spec).
			if p.RelationshipID, err = currentNodeExecution(txCtx, q, plan, n.NodeID, n.SliceDigest); err != nil {
				return err
			}
			if p.RelationshipID != "" {
				if p.Acceptance, err = activeAcceptanceOf(txCtx, q, p.RelationshipID); err != nil {
					return err
				}
				if p.Acceptance != nil {
					if p.Integration, err = integrationOf(txCtx, q, p.RelationshipID, p.Acceptance.HeadSHA); err != nil {
						return err
					}
				}
			}
			out.Packets = append(out.Packets, p)
		}
		criteria, hasDeclaration, err := coverageCriteria(txCtx, q, snap, issue, nodes, out.Packets)
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
		if !hasDeclaration {
			// Nothing says which packet owns which criterion, so the feature is complete only when every
			// packet of the issue is integrated: an unknown ownership is never read as covered.
			for _, p := range out.Packets {
				if p.Integration == nil {
					out.Complete = false
				}
			}
		}
		return nil
	})
	return out, err
}

// coverageCriteria is the feature's criteria with who takes, owns and has integrated each one. The plan's
// declaration is the source when there is one; otherwise the criteria the relay registered for the
// issue's nodes are, which is how a plan without packets has always been judged.
func coverageCriteria(ctx context.Context, q store.Querier, snap dag.Snapshot, issue string, nodes []dag.SnapNode, packets []CoveragePacket) ([]CoverageCriterion, bool, error) {
	type declared struct {
		title    string
		required bool
	}
	set := map[string]declared{}
	registered := map[string][]string{} // criterion id -> the node whose own relationship registered it
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
	hasDeclaration := len(set) > 0
	if !hasDeclaration {
		// The plan declares nothing for this issue, so the criteria are the ones the relay registered for
		// the issue's nodes. Each criterion stays with the packet whose relationship registered it: one
		// packet's landing never stands for another's criteria.
		for _, n := range nodes {
			for _, p := range packets {
				if p.NodeID != n.NodeID || p.RelationshipID == "" {
					continue
				}
				rows, err := q.QueryContext(ctx, "SELECT criterion_id, title, required FROM canonical_criteria WHERE relationship_id = ? ORDER BY criterion_id", p.RelationshipID)
				if err != nil {
					return nil, false, err
				}
				for rows.Next() {
					var id, title string
					var required int64
					if err := rows.Scan(&id, &title, &required); err != nil {
						_ = rows.Close()
						return nil, false, err
					}
					if _, seen := set[id]; !seen {
						order = append(order, id)
					}
					set[id] = declared{title: title, required: required != 0}
					registered[id] = append(registered[id], p.NodeID)
				}
				if err := errors.Join(rows.Err(), rows.Close()); err != nil {
					return nil, false, err
				}
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
			// With no declaration a criterion belongs to the node whose own relationship registered it;
			// with one, to the packets whose covers name it.
			covers := containsID(p.Covers, id)
			if !hasDeclaration {
				covers = containsID(registered[id], n.NodeID)
			}
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
	return out, hasDeclaration, nil
}

func containsID(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// currentNodeExecution is the relationship a node's output stands on now: the live relationship whose
// execution consumed a manifest built for the node version the plan holds. A node whose spec changed since
// it was released has no current execution (its slice digest is not the one the manifest was built for),
// so the reading credits nothing for it. An archived relationship is never current, whatever generation it
// reached: generations are relationship-local, so the largest one alone does not name the live child.
func currentNodeExecution(ctx context.Context, q store.Querier, plan, node, sliceDigest string) (string, error) {
	rows, err := q.QueryContext(ctx, "SELECT e.relationship_id, COALESCE(m.body_json, '') FROM dag_node_executions e"+
		" JOIN relationships r ON r.relationship_id = e.relationship_id"+
		" LEFT JOIN dag_input_manifests m ON m.manifest_digest = e.manifest_digest"+
		" WHERE e.plan_id = ? AND e.node_id = ? AND r.status IN ('active','paused') AND r.superseded_by IS NULL"+
		" ORDER BY e.execution_generation DESC", plan, node)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var relationship, body string
		if err := rows.Scan(&relationship, &body); err != nil {
			return "", err
		}
		if manifestSliceDigest(body) == sliceDigest {
			return relationship, nil
		}
	}
	return "", rows.Err()
}

// manifestSliceDigest is the node slice digest a stored manifest was built for, empty when the manifest
// cannot be read (a manifest that is not there credits nothing).
func manifestSliceDigest(body string) string {
	if body == "" {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return ""
	}
	s, _ := doc["node_slice_digest"].(string)
	return s
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
func integrationOf(ctx context.Context, q store.Querier, relationship, acceptedHead string) (*CoverageIntegration, error) {
	var train, memberHead, detail, at string
	// The bundle that carried THIS accepted head. The member head is required to be the active
	// acceptance's head, so a landing of an earlier head never credits a later acceptance.
	found, err := queryOne(ctx, q, "SELECT m.train_id, m.member_head, e.detail_json, e.recorded_at FROM merge_train_members m"+
		" JOIN merge_train_events e ON e.train_id = m.train_id AND e.kind = 'landed'"+
		" WHERE m.relationship_id = ? AND m.member_head = ?"+
		" AND (SELECT kind FROM merge_train_events x WHERE x.train_id = m.train_id ORDER BY x.seq DESC LIMIT 1) <> 'abandoned'"+
		" ORDER BY e.recorded_at DESC LIMIT 1", []any{relationship, acceptedHead}, &train, &memberHead, &detail, &at)
	if err != nil {
		return nil, err
	}
	if found {
		return &CoverageIntegration{Method: "merge_train", TrainID: train, MemberHead: memberHead, LandedSHA: detailString(detail, "landedSha"), LandedAt: at}, nil
	}
	// A landing the relay observed directly, under the same predicate the scheduler's own edge judgement
	// uses: a positive observation of the active acceptance with no later negative for that target.
	var subject, observed string
	found, err = queryOne(ctx, q, "SELECT o.subject_sha, o.observed_at FROM dag_integration_observations o"+
		" WHERE o.acceptance_id = (SELECT acceptance_id FROM dag_acceptances WHERE relationship_id = ? AND state = 'active')"+
		" AND o.is_ancestor = 1 AND o.reverted_by IS NULL"+
		" AND NOT EXISTS (SELECT 1 FROM dag_integration_observations o2 WHERE o2.acceptance_id = o.acceptance_id AND o2.repository = o.repository"+
		"   AND o2.base_ref = o.base_ref AND o2.observed_seq > o.observed_seq AND o2.is_ancestor = 0)"+
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
			var pr any
			if p.Acceptance.PRNumber != nil {
				pr = *p.Acceptance.PRNumber
			}
			acceptance = contract.OrderedObject{
				{Key: "acceptance_id", Value: p.Acceptance.AcceptanceID}, {Key: "head_sha", Value: nullableText(p.Acceptance.HeadSHA)},
				{Key: "pr_number", Value: pr}, {Key: "state", Value: p.Acceptance.State},
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
