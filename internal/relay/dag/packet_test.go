package dag

import (
	"context"
	"strings"
	"testing"
)

// The packet identity of a feature issue (CRW-839): a plan node may carry an optional packet_id, several
// nodes may share an issue_key only with distinct packet_ids, and the plan validation refuses a duplicate
// (issue, packet), a required criterion no packet covers, and a criterion two packets take without one
// owner. These tests are the red tests of the issue: each one is refused for its own reason, and the
// accepted plans are the ones the rules allow.

func packetNode(id, issue, packet string, covers, owns []string) doc {
	n := nodeDoc(id, NodeImplementation)
	n["issue_key"] = issue
	if packet != "" {
		n["packet_id"] = packet
	}
	if covers != nil {
		n["covers"] = strList(covers)
	}
	if owns != nil {
		n["owns"] = strList(owns)
	}
	return n
}

func strList(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func featureCriteria(issue string, criteria ...doc) doc {
	cs := make([]any, len(criteria))
	for i, c := range criteria {
		cs[i] = c
	}
	return doc{"issue_key": issue, "criteria": cs}
}

func criterion(id string, required bool) doc { return doc{"id": id, "required": required} }

// revWith declares feature criteria beside its changes.
func revWith(plan, request string, parent int, criteria []doc, changes ...doc) doc {
	d := revDoc(plan, request, parent, changes...)
	if len(criteria) > 0 {
		cs := make([]any, len(criteria))
		for i, c := range criteria {
			cs[i] = c
		}
		d["feature_criteria"] = cs
	}
	return d
}

func addPacketNode(id, issue, packet string, covers, owns []string) doc {
	return doc{"op": OpAddNode, "node": packetNode(id, issue, packet, covers, owns)}
}

// TestPacketRulesRefuse is the red test of the plan-validation refusals: each plan below is the plan the
// issue says must be rejected, and each is rejected for its own rule.
func TestPacketRulesRefuse(t *testing.T) {
	criteria := []doc{featureCriteria("CRW-F", criterion("c1", true), criterion("c2", true), criterion("c3", false))}
	cases := []struct {
		name   string
		doc    doc
		rule   string
		detail string
	}{
		{
			name:   "two live nodes of one issue share a packet_id",
			doc:    revWith("plan", "r", 0, criteria, addPacketNode("n1", "CRW-F", "p1", []string{"c1", "c2"}, []string{"c1", "c2"}), addPacketNode("n2", "CRW-F", "p1", []string{"c3"}, []string{"c3"})),
			rule:   RuleDuplicatePacket,
			detail: "(issue_key, packet_id) is unique",
		},
		{
			name:   "two live nodes of one issue and one carries no packet_id",
			doc:    revWith("plan", "r", 0, criteria, addPacketNode("n1", "CRW-F", "p1", []string{"c1", "c2"}, []string{"c1", "c2"}), addPacketNode("n2", "CRW-F", "", []string{"c3"}, []string{"c3"})),
			rule:   RulePacketRequired,
			detail: "carries no packet_id",
		},
		{
			name:   "a required declared criterion no packet covers",
			doc:    revWith("plan", "r", 0, criteria, addPacketNode("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"})),
			rule:   RuleCriterionUncovered,
			detail: "no live node of issue CRW-F covers the required criterion c2",
		},
		{
			name:   "a criterion two packets take and neither owns",
			doc:    revWith("plan", "r", 0, criteria, addPacketNode("n1", "CRW-F", "p1", []string{"c1", "c2"}, nil), addPacketNode("n2", "CRW-F", "p2", []string{"c2", "c3"}, []string{"c3"})),
			rule:   RuleCriterionOwnerMissing,
			detail: "none of them names it in owns",
		},
		{
			name:   "a criterion two packets take and both own",
			doc:    revWith("plan", "r", 0, criteria, addPacketNode("n1", "CRW-F", "p1", []string{"c1", "c2"}, []string{"c1", "c2"}), addPacketNode("n2", "CRW-F", "p2", []string{"c2", "c3"}, []string{"c2", "c3"})),
			rule:   RuleCriterionOwnerConflict,
			detail: "claim to own it",
		},
		{
			name:   "covers names a criterion the issue does not declare",
			doc:    revWith("plan", "r", 0, criteria, addPacketNode("n1", "CRW-F", "p1", []string{"c1", "c2", "c9"}, []string{"c1", "c2", "c9"})),
			rule:   RuleCoversUnknownCriterion,
			detail: "which issue CRW-F does not declare",
		},
		{
			name:   "owns names a criterion the node does not cover",
			doc:    revWith("plan", "r", 0, criteria, addPacketNode("n1", "CRW-F", "p1", []string{"c1", "c2"}, []string{"c1", "c3"})),
			rule:   RuleOwnsNotCovered,
			detail: "which it does not cover",
		},
		{
			name:   "covers without a packet_id",
			doc:    revWith("plan", "r", 0, criteria, addPacketNode("n1", "CRW-F", "", []string{"c1", "c2", "c3"}, []string{"c1", "c2", "c3"})),
			rule:   RulePacketRequired,
			detail: "declares covers or owns without a packet_id",
		},
		{
			name:   "a non_pr node carries a packet identity",
			doc:    revWith("plan", "r", 0, nil, doc{"op": OpAddNode, "node": with(nodeDoc("n1", NodeNonPR), "packet_id", "p1")}),
			rule:   RulePacketFieldNotApplicable,
			detail: "a packet is an implementation node",
		},
		{
			// d5: without a declaration the fold has no required criteria to reject as uncovered, so the
			// legacy fallback would let a required criterion assigned to no packet disappear.
			name:   "a packet issue declares no feature criteria",
			doc:    revWith("plan", "r", 0, nil, addPacketNode("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"})),
			rule:   RuleFeatureCriteriaRequired,
			detail: "declares no feature_criteria",
		},
		{
			// d5: a packet that declares no covers takes nothing, so a required criterion could be
			// assigned to no packet while the plan still validates.
			name:   "a packet declares no covers",
			doc:    revWith("plan", "r", 0, criteria, addPacketNode("n1", "CRW-F", "p1", nil, nil)),
			rule:   RulePacketCoversRequired,
			detail: "declares no covers",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _, _ := newRepo(t)
			ctx := context.Background()
			_, err := r.Put(ctx, decode(t, c.doc))
			p := rejected(t, err)
			if !hasRule(p, c.rule, "") {
				t.Fatalf("want rule %s, got %v", c.rule, p.Violations)
			}
			found := false
			for _, v := range p.Violations {
				found = found || (v.Rule == c.rule && strings.Contains(v.Detail, c.detail))
			}
			if !found {
				t.Fatalf("no %s violation explains itself with %q: %v", c.rule, c.detail, p.Violations)
			}
		})
	}
}

// TestPacketPlanIsAccepted is the other half: the plan the rules allow is stored, reads back with its
// packet identity, and replays.
func TestPacketPlanIsAccepted(t *testing.T) {
	r, _, _ := newRepo(t)
	ctx := context.Background()
	plan := revWith("plan", "r1", 0, []doc{featureCriteria("CRW-F", criterion("c1", true), criterion("c2", true), criterion("c3", false))},
		addPacketNode("n1", "CRW-F", "p1", []string{"c1", "c2"}, []string{"c1", "c2"}),
		addPacketNode("n2", "CRW-F", "p2", []string{"c2", "c3"}, []string{"c3"}),
		addNode("other", NodeNonPR))
	if _, err := r.Put(ctx, decode(t, plan)); err != nil {
		t.Fatalf("the two-packet plan is rejected: %v", err)
	}
	snap, _, err := r.Snapshot(ctx, "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]SnapNode{}
	for _, n := range snap.Nodes {
		byID[n.NodeID] = n
	}
	if got := byID["n1"]; got.PacketID != "p1" || len(got.Covers) != 2 || len(got.Owns) != 2 {
		t.Fatalf("n1 = %+v, want packet p1 with its covers and owns", got)
	}
	if got := byID["n2"]; got.PacketID != "p2" || len(got.Owns) != 1 {
		t.Fatalf("n2 = %+v, want packet p2", got)
	}
	if got := byID["other"]; got.PacketID != "" || len(got.Covers) != 0 {
		t.Fatalf("a node without a packet reads one: %+v", got)
	}
	if len(snap.FeatureCriteria) != 1 || snap.FeatureCriteria[0].IssueKey != "CRW-F" || len(snap.FeatureCriteria[0].Criteria) != 3 {
		t.Fatalf("the declaration did not read back: %+v", snap.FeatureCriteria)
	}
	if err := r.VerifyLog(ctx, "plan"); err != nil {
		t.Fatalf("the packet plan does not replay: %v", err)
	}
}

// TestSingleNodePlanKeepsItsMeaning: one node of an issue with no packet_id is the single packet it always
// was, and it is accepted beside a declaration that requires nothing of it.
func TestSingleNodePlanKeepsItsMeaning(t *testing.T) {
	r, _, _ := newRepo(t)
	ctx := context.Background()
	if _, err := r.Put(ctx, decode(t, revDoc("plan", "r1", 0, addNode("solo", NodeImplementation)))); err != nil {
		t.Fatalf("a single node of an issue is rejected: %v", err)
	}
	if _, err := r.Put(ctx, decode(t, revWith("plan", "r2", 1, nil, addNode("solo", NodeImplementation)))); err == nil {
		t.Fatal("a node id was reused and accepted")
	}
	snap, _, err := r.Snapshot(ctx, "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.FeatureCriteria) != 0 {
		t.Fatalf("a plan that declares no criteria carries some: %+v", snap.FeatureCriteria)
	}
	if err := r.VerifyLog(ctx, "plan"); err != nil {
		t.Fatalf("a plan without packets does not replay: %v", err)
	}
}

// TestFeatureCriteriaReplaceFoldedLikeAChange: a later revision replaces an issue's declaration, and the
// plan as of an earlier revision still reads the earlier declaration.
func TestFeatureCriteriaReplaceFoldedLikeAChange(t *testing.T) {
	r, _, _ := newRepo(t)
	ctx := context.Background()
	first := revWith("plan", "r1", 0, []doc{featureCriteria("CRW-F", criterion("c1", true))}, addPacketNode("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	if _, err := r.Put(ctx, decode(t, first)); err != nil {
		t.Fatal(err)
	}
	second := revWith("plan", "r2", 1, []doc{featureCriteria("CRW-F", criterion("c1", true), criterion("c2", true))},
		doc{"op": OpUpdateNode, "node": packetNode("n1", "CRW-F", "p1", []string{"c1", "c2"}, []string{"c1", "c2"})})
	if _, err := r.Put(ctx, decode(t, second)); err != nil {
		t.Fatal(err)
	}
	at1, _, err := r.Snapshot(ctx, "plan", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(at1.FeatureCriteria) != 1 || len(at1.FeatureCriteria[0].Criteria) != 1 {
		t.Fatalf("revision 1 reads %+v, want the first declaration", at1.FeatureCriteria)
	}
	head, _, err := r.Snapshot(ctx, "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(head.FeatureCriteria) != 1 || len(head.FeatureCriteria[0].Criteria) != 2 {
		t.Fatalf("the head reads %+v, want the replaced declaration", head.FeatureCriteria)
	}
	if at1.StateDigest == head.StateDigest {
		t.Fatal("the two revisions digest the same, so the declaration is not in the state digest")
	}
	if err := r.VerifyLog(ctx, "plan"); err != nil {
		t.Fatalf("the replaced declaration does not replay: %v", err)
	}
}

// TestPacketFieldsAreReadStrictly: the reader refuses the shapes a packet field cannot have.
// TestFeatureCriteriaIdentityIsUnambiguous: a criterion id may hold ':' (the identifier grammar allows
// it), so the digest and the row comparison must not join ids and titles with delimiters a value can
// contain. Two declarations that differ only inside such a value must digest differently.
func TestFeatureCriteriaIdentityIsUnambiguous(t *testing.T) {
	digestOf := func(t *testing.T, plan, title string) string {
		t.Helper()
		r, _, _ := newRepo(t)
		ctx := context.Background()
		crit := doc{"id": "a:b", "title": title, "required": true}
		d := revWith(plan, "r1", 0, []doc{featureCriteria("CRW-F", crit)}, addPacketNode("n1", "CRW-F", "p1", []string{"a:b"}, []string{"a:b"}))
		if _, err := r.Put(ctx, decode(t, d)); err != nil {
			t.Fatal(err)
		}
		snap, _, err := r.Snapshot(ctx, plan, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.FeatureCriteria) != 1 || snap.FeatureCriteria[0].Criteria[0].ID != "a:b" {
			t.Fatalf("the criterion did not read back: %+v", snap.FeatureCriteria)
		}
		if err := r.VerifyLog(ctx, plan); err != nil {
			t.Fatalf("the plan does not replay: %v", err)
		}
		return snap.StateDigest
	}
	one := digestOf(t, "plan-one", "x:y;z=1")
	two := digestOf(t, "plan-two", "x:y;z=2")
	if one == two {
		t.Fatal("two different declared criteria digest the same")
	}
}

// TestPacketFieldsAreReadStrictly: the reader refuses the shapes a packet field cannot have.
func TestPacketFieldsAreReadStrictly(t *testing.T) {
	base := func(n doc) doc { return revWith("plan", "r", 0, nil, doc{"op": OpAddNode, "node": n}) }
	cases := []struct {
		name string
		node doc
	}{
		{"covers is not a list", with(packetNode("n1", "CRW-F", "p1", nil, nil), "covers", "c1")},
		{"covers names nothing", with(packetNode("n1", "CRW-F", "p1", nil, nil), "covers", []any{})},
		{"covers repeats an id", with(packetNode("n1", "CRW-F", "p1", nil, nil), "covers", []any{"c1", "c1"})},
		{"packet_id is empty", with(packetNode("n1", "CRW-F", "p1", nil, nil), "packet_id", "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeRevision(raw(t, base(c.node))); err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
		})
	}
}
