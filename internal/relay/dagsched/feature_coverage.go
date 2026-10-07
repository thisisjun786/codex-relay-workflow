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
	// RequiredRegistered is the criterion ids the relationship registered as REQUIRED. It is what tells
	// whether the packet's own verdicts actually judged a criterion the feature declares required (d4).
	RequiredRegistered map[string]bool
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

// coverageIntegrationOf is how a packet's output was integrated, judged by the scheduler's own integration
// predicate and not by a weaker one (CRW-839 d2). A packet counts as integrated only when nodeIntegrated
// says its accepted head landed everywhere it has to: an accepted code head, at least one target, the
// merged mark on the acceptance's current stand head, and every required target satisfied by a positive
// observation with no later negative. The landing commit is what the record names: the bundle's landed
// commit when a train carried the member (its own members mapping, so an excluded member is not credited,
// CRW-839 d3), else the stand head a direct observation proved contained in every target.
func (s *Scheduler) coverageIntegrationOf(ctx context.Context, q store.Querier, snap dag.Snapshot, a Acceptance) (*CoverageIntegration, error) {
	// The acceptance must still stand under the criteria the plan and the relationship hold now (CRW-839
	// pre-merge d2), the same test the scheduler's own edge judgement makes (standing, edges.go): the
	// effective criteria digest of the acceptance is the node's, and every registered criterion row
	// carries it. Without this a criterion strengthened to required after the landing - and re-registered
	// required - would be credited by a landing whose verdicts never judged it.
	node, ok := nodeOf(snap, a.NodeID)
	if !ok {
		return nil, nil
	}
	effective, err := effectiveCriteria(ctx, q, a)
	if err != nil {
		return nil, err
	}
	var rows, distinct int
	var canonical sql.NullString
	if _, err := queryOne(ctx, q, "SELECT COUNT(*), COUNT(DISTINCT set_digest), MIN(set_digest) FROM canonical_criteria WHERE relationship_id = ?", []any{a.RelationshipID}, &rows, &distinct, &canonical); err != nil {
		return nil, err
	}
	// The registered set must still be the one the acceptance was judged under. A node whose plan never
	// fixed a criteria set (an empty digest) is judged by the registered set alone, which is the legacy
	// meaning; a node whose plan fixed one must agree with the registration too.
	if rows == 0 || distinct != 1 || canonical.String != effective || (node.CriteriaSetDigest != "" && effective != node.CriteriaSetDigest) {
		return nil, nil
	}
	landed, targets, err := s.nodeIntegrated(ctx, q, a.PlanID, snap, a)
	if err != nil || !landed {
		return nil, err
	}
	at := ""
	for _, t := range targets {
		got, err := s.integratedAt(ctx, q, a.PlanID, a, t.Repository, t.BaseRef)
		if err != nil {
			return nil, err
		}
		if got.Since > at {
			at = got.Since
		}
	}
	stand, err := s.standOf(ctx, q, a)
	if err != nil {
		return nil, err
	}
	train, landedSHA, trainAt, found, err := landingRecordOf(ctx, q, a.RelationshipID, stand.Head)
	if err != nil {
		return nil, err
	}
	if found {
		return &CoverageIntegration{Method: "merge_train", TrainID: train, MemberHead: stand.Head, LandedSHA: landedSHA, LandedAt: trainAt}, nil
	}
	return &CoverageIntegration{Method: "integration_observation", LandedSHA: stand.Head, LandedAt: at}, nil
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
			// Every live node of the issue is one of its packets, whatever lifecycle word it carries
			// (CRW-839 pre-merge d3): pausing, cancelling or archiving a node leaves the node, its
			// criteria, its slice and any landing it reached intact, and the scheduler keeps crediting an
			// integrated edge out of an archived node (edges.go). Dropping a landed packet because of a
			// lifecycle word would erase its credit and read a delivered feature incomplete.
			if n.IssueKey == issue && n.Kind == dag.NodeImplementation {
				nodes = append(nodes, n)
			}
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
		if len(nodes) == 0 {
			return refuse(contract.RefusalUnregisteredScope, "plan %s holds no live implementation node for issue %s", plan, issue)
		}
		for _, n := range nodes {
			p := CoveragePacket{NodeID: n.NodeID, PacketID: n.PacketID, Covers: n.Covers, Owns: n.Owns}
			// The execution that stands for this node now. A node whose spec changed since it was released
			// has no current execution, so its packet reports none and credits nothing (CRW-839 review: a
			// packet whose covers moved must not be credited by the pull request released for the old
			// spec) - except that a revalidated acceptance stands under the criteria the plan holds now
			// even though its execution and manifest are the original ones (CRW-839 pre-merge d2).
			if p.RelationshipID, err = currentNodeExecution(txCtx, q, plan, n); err != nil {
				return err
			}
			if p.RelationshipID != "" {
				acc, found, err := loadActiveAcceptance(txCtx, q, plan, n.NodeID)
				if err != nil {
					return err
				}
				// The node's active acceptance is credited only while it is the acceptance of the
				// relationship the node's current execution stands on: an acceptance an older execution
				// left behind does not speak for the packet that replaced it.
				if found && acc.RelationshipID == p.RelationshipID {
					p.Acceptance = &CoverageAcceptance{AcceptanceID: acc.AcceptanceID, HeadSHA: acc.HeadSHA, State: acc.State}
					if acc.PRNumber > 0 {
						n := acc.PRNumber
						p.Acceptance.PRNumber = &n
					}
					if p.Integration, err = s.coverageIntegrationOf(txCtx, q, snap, acc); err != nil {
						return err
					}
				}
				if p.RequiredRegistered, err = registeredRequiredCriteria(txCtx, q, p.RelationshipID); err != nil {
					return err
				}
			}
			out.Packets = append(out.Packets, p)
		}
		// The legacy reading (the criteria the relay registered for the node's own relationship) is the
		// meaning of an issue with ONE node and no packet_id, and of nothing else (CRW-839 d5): a plan that
		// delivers an issue by packets must declare its criteria, and where such a plan holds no declaration
		// the reading cannot know the feature's requirements, so it never calls the feature complete.
		legacy := len(nodes) == 1 && nodes[0].PacketID == ""
		criteria, hasDeclaration, err := coverageCriteria(txCtx, q, snap, issue, nodes, out.Packets, legacy)
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
		if !hasDeclaration && !legacy {
			// A packet plan with no declaration: nothing says which criteria the feature requires, so the
			// reading never answers complete (fail closed; the plan validation refuses such a revision).
			out.Complete = false
		} else if !hasDeclaration {
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

// registeredRequiredCriteria is the criterion ids a relationship registered as required, the set the
// packet's own verified verdicts were actually judged against (CRW-839 d4).
func registeredRequiredCriteria(ctx context.Context, q store.Querier, relationship string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT criterion_id FROM canonical_criteria WHERE relationship_id = ? AND required <> 0", relationship)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// coverageCriteria is the feature's criteria with who takes, owns and has integrated each one. The plan's
// declaration is the source when there is one. The legacy fallback (the criteria the relay registered for
// the node's own relationship) applies only when legacy is set, which the caller sets for an issue with
// one node and no packet_id (CRW-839 d5).
func coverageCriteria(ctx context.Context, q store.Querier, snap dag.Snapshot, issue string, nodes []dag.SnapNode, packets []CoveragePacket, legacy bool) ([]CoverageCriterion, bool, error) {
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
	if !hasDeclaration && legacy {
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
			// A criterion counts as integrated only when a packet that takes it landed AND the packet's own
			// verified verdicts judged the criterion required (CRW-839 d4). A required feature criterion a
			// packet registered optional was never judged by its verdict, so its landing does not cover it.
			// The legacy reading has no packet criteria set to compare against: it keeps its meaning.
			if p.Integration != nil && (!hasDeclaration || !c.Required || legacy || p.RequiredRegistered[id]) {
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
// so the reading credits nothing for it.
//
// A relationship that was archived after its merge keeps its credit (CRW-839 d6): relationship-close-merged
// archives a SETTLED relationship, and settled (registry.settled.go) means the assignment read merged and
// nothing of it was still owed - including, for a plan node's execution, that the current head still holds
// an active acceptance under the current criteria. So an archived relationship that still holds an active
// acceptance is one closed while merged, and the packet it executed keeps its acceptance and integration;
// without this, ordinary cleanup would turn a complete feature incomplete. A relationship that was
// superseded (superseded_by set) or cancelled (abandoned) counts for nothing, whatever it reached, and a
// live execution is preferred over an archived one for the same node version.
func currentNodeExecution(ctx context.Context, q store.Querier, plan string, n dag.SnapNode) (string, error) {
	// The execution's active acceptance, with the criteria digest it stands on NOW: its newest
	// revalidation, else the digest it was accepted with (effectiveCriteria, edges.go). A revalidation
	// is how dag-accept re-judges an unchanged output under a plan whose criteria moved, and it keeps
	// the original execution and manifest, so the slice digest alone would not see it.
	//
	// The packet column is read only where the zone has the table (CRW-839 pre-merge d4): a store that
	// predates it has no dag_execution_packets, and a read-only open installs nothing (store.Open), so
	// an unconditional reference would turn the legacy single-node answer into a missing-table host
	// error. A store without the table answers as it always did.
	packetColumn := "''"
	if present, err := tableExists(ctx, q, "dag_execution_packets"); err != nil {
		return "", err
	} else if present {
		packetColumn = "COALESCE((SELECT x.packet_id FROM dag_execution_packets x WHERE x.relationship_id = e.relationship_id), '')"
	}
	rows, err := q.QueryContext(ctx, "SELECT e.relationship_id, COALESCE(m.body_json, ''),"+
		" COALESCE((SELECT v.criteria_set_digest FROM dag_acceptance_revalidations v"+
		"   WHERE v.acceptance_id = (SELECT a.acceptance_id FROM dag_acceptances a WHERE a.plan_id = e.plan_id AND a.node_id = e.node_id AND a.relationship_id = e.relationship_id AND a.state = 'active')"+
		"   ORDER BY v.reval_seq DESC LIMIT 1), ''),"+
		" "+packetColumn+
		" FROM dag_node_executions e"+
		" JOIN relationships r ON r.relationship_id = e.relationship_id"+
		" LEFT JOIN dag_input_manifests m ON m.manifest_digest = e.manifest_digest"+
		" WHERE e.plan_id = ? AND e.node_id = ? AND r.superseded_by IS NULL"+
		" AND (r.status IN ('active','paused')"+
		"   OR (r.status = 'archived' AND EXISTS (SELECT 1 FROM dag_acceptances a WHERE a.relationship_id = e.relationship_id AND a.state = 'active')))"+
		" ORDER BY CASE WHEN r.status IN ('active','paused') THEN 0 ELSE 1 END, e.execution_generation DESC", plan, n.NodeID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var relationship, body, revalidated, packet string
		if err := rows.Scan(&relationship, &body, &revalidated, &packet); err != nil {
			return "", err
		}
		if manifestSliceDigest(body) == n.SliceDigest {
			return relationship, nil
		}
		// A criteria-only revision moves the node's slice without moving the accepted output: an
		// acceptance REVALIDATED under the plan's criteria now is the packet's current execution. The
		// revalidation row alone does not say the output is still THIS packet's (CRW-839 pre-merge d2,
		// d3): the execution must also still execute the packet the node carries, and, because a packet
		// id is unique only WITHIN an issue, the execution's recorded issue must be the node's too - a
		// node whose issue_key was edited could otherwise be credited with another feature's revalidated
		// execution carrying the same packet id and criteria digest. Both checks bind only where the
		// execution records a packet; a node with no packet_id keeps the legacy meaning.
		if n.CriteriaSetDigest != "" && revalidated == n.CriteriaSetDigest && packet == n.PacketID && (packet == "" || executionIssueIs(ctx, q, relationship, n.IssueKey)) {
			return relationship, nil
		}
	}
	return "", rows.Err()
}

// executionIssueIs is whether the packet zone records this relationship as executing a node of issueKey.
// It is asked only where the zone has already named a packet for the relationship.
func executionIssueIs(ctx context.Context, q store.Querier, relationship, issueKey string) bool {
	var recorded string
	found, err := queryOne(ctx, q, "SELECT issue_key FROM dag_execution_packets WHERE relationship_id = ?", []any{relationship}, &recorded)
	return err == nil && found && recorded == issueKey
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

// landingRecordOf is the bundle a member rode and the commit that bundle landed, read from the LANDED
// event's OWN members mapping (CRW-839 d3): the mapping the event recorded is the immutable snapshot of
// which members the bundle carried, so a member the bundle excluded - withdrawn while the train waited -
// is not credited. The member is matched by its stand head, which is what the mapping's memberHead holds
// (the train records the stand head), so a member whose accepted stand head moved through a base refresh
// is still found. found is false when no landed bundle carried this relationship at this head.
func landingRecordOf(ctx context.Context, q store.Querier, relationship, standHead string) (train, landedSHA, at string, found bool, err error) {
	rows, err := q.QueryContext(ctx, "SELECT e.train_id, e.detail_json, e.recorded_at FROM merge_train_events e"+
		" WHERE e.kind = 'landed' ORDER BY e.recorded_at DESC", nil)
	if err != nil {
		return "", "", "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, detail, recorded string
		if err := rows.Scan(&id, &detail, &recorded); err != nil {
			return "", "", "", false, err
		}
		var body struct {
			LandedSHA string `json:"landedSha"`
			Members   []struct {
				RelationshipID string `json:"relationshipId"`
				MemberHead     string `json:"memberHead"`
			} `json:"members"`
		}
		if err := json.Unmarshal([]byte(detail), &body); err != nil {
			continue
		}
		for _, m := range body.Members {
			if m.RelationshipID == relationship && m.MemberHead == standHead {
				return id, body.LandedSHA, recorded, true, nil
			}
		}
	}
	return "", "", "", false, rows.Err()
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
