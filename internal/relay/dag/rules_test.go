package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// matrixRow is one rule of the rejection matrix: the revision it feeds, the plan it is applied to, and
// the violation it must produce (rule and where in the document).
type matrixRow struct {
	name string
	// base is a revision committed first (nil: the request is the plan's first revision).
	base *doc
	req  func() doc
	rule string
	path string
	// detail is a fragment the violation's explanation must hold: a clear, specific error.
	detail string
}

func manyNodes(n int) []doc {
	var cs []doc
	for i := 0; i < n; i++ {
		cs = append(cs, addNode(fmt.Sprintf("n%03d", i), NodeNonPR))
	}
	return cs
}

func chainEdges(nodes, edges int) []doc {
	var cs []doc
	for i := 0; i < nodes && len(cs) < edges; i++ {
		for j := i + 1; j < nodes && len(cs) < edges; j++ {
			cs = append(cs, addEdge(fmt.Sprintf("e%03d-%03d", i, j), fmt.Sprintf("n%03d", i), fmt.Sprintf("n%03d", j), EdgeArtifactVerified))
		}
	}
	return cs
}

func base() *doc {
	d := forkJoin("plan", "base")
	return &d
}

func with(d doc, key string, value any) doc { d[key] = value; return d }

func without(d doc, key string) doc { delete(d, key); return d }

func edgeWith(edge doc, k string, v any) doc {
	e := doc{}
	for key, value := range edge {
		e[key] = value
	}
	if v == nil {
		delete(e, k)
	} else {
		e[k] = v
	}
	return e
}

func change(op string, fields doc) doc {
	c := doc{"op": op}
	for k, v := range fields {
		c[k] = v
	}
	return c
}

var matrix = []matrixRow{
	// the document itself
	{name: "empty change list", req: func() doc { return revDoc("plan", "r", 0) }, rule: RuleEmptyChanges, path: "changes", detail: "at least one change"},
	{name: "inline nodes are not read (OMON define(nodes) lesson)", req: func() doc {
		return with(revDoc("plan", "r", 0, addNode("a", NodeNonPR)), "nodes", []any{nodeDoc("b", NodeNonPR)})
	}, rule: RuleUnknownField, path: "$.nodes", detail: "add_node"},
	{name: "unknown field of a change", req: func() doc {
		return revDoc("plan", "r", 0, with(addNode("a", NodeNonPR), "after", "x"))
	}, rule: RuleUnknownField, path: "changes[0].after"},
	{name: "unknown field of a node", req: func() doc {
		n := nodeDoc("a", NodeNonPR)
		n["depends_on"] = []any{"x"}
		return revDoc("plan", "r", 0, doc{"op": OpAddNode, "node": n})
	}, rule: RuleUnknownField, path: "changes[0].node.depends_on"},
	{name: "missing plan_id", req: func() doc { return without(revDoc("plan", "r", 0, addNode("a", NodeNonPR)), "plan_id") }, rule: RuleMissingField, path: "$.plan_id"},
	{name: "wrong schema", req: func() doc {
		return with(revDoc("plan", "r", 0, addNode("a", NodeNonPR)), "schema", "dag-plan-revision/2")
	}, rule: RuleBadSchema, path: "$.schema"},
	{name: "parent revision is a float", req: func() doc {
		return with(revDoc("plan", "r", 0, addNode("a", NodeNonPR)), "expected_parent_revision", 1.5)
	}, rule: RuleWrongType, path: "$.expected_parent_revision"},
	{name: "empty string is a missing value", req: func() doc {
		n := nodeDoc("a", NodeNonPR)
		n["issue_key"] = ""
		return revDoc("plan", "r", 0, doc{"op": OpAddNode, "node": n})
	}, rule: RuleEmptyValue, path: "changes[0].node.issue_key", detail: "missing"},
	{name: "bad identifier", req: func() doc { return revDoc("plan", "r", 0, addNode("a b", NodeNonPR)) }, rule: RuleBadIdentifier, path: "changes[0].node.node_id"},
	{name: "bad digest", req: func() doc {
		n := nodeDoc("a", NodeNonPR)
		n["criteria_set_digest"] = "ABC"
		return revDoc("plan", "r", 0, doc{"op": OpAddNode, "node": n})
	}, rule: RuleBadDigest, path: "changes[0].node.criteria_set_digest"},
	{name: "unknown op", req: func() doc { return revDoc("plan", "r", 0, doc{"op": "rename_node"}) }, rule: RuleUnknownOp, path: "changes[0].op"},
	{name: "undefined node kind", req: func() doc {
		n := nodeDoc("a", NodeNonPR)
		n["kind"] = "research"
		return revDoc("plan", "r", 0, doc{"op": OpAddNode, "node": n})
	}, rule: RuleUnknownNodeKind, path: "changes[0].node.kind"},
	{name: "undefined edge kind", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR), addEdge("e", "a", "b", "blocks"))
	}, rule: RuleUnknownEdgeKind, path: "changes[2].edge.kind"},
	{name: "empty authority list", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR),
			doc{"op": OpAddEdge, "edge": edgeWith(edgeDoc("e", "a", "b", EdgeDecision), "required_authority", []any{})})
	}, rule: RuleEmptyValue, path: "required_authority"},
	{name: "authority named twice", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR),
			doc{"op": OpAddEdge, "edge": edgeWith(edgeDoc("e", "a", "b", EdgeDecision), "required_authority", []any{"user", "user"})})
	}, rule: RuleDuplicateValue, path: "required_authority[1]"},

	// the graph the revision would produce
	{name: "empty graph after the revision", base: base(), req: func() doc {
		var cs []doc
		for _, id := range []string{"design", "impl-a", "impl-b", "join", "ship"} {
			cs = append(cs, change(OpRetireNode, doc{"node_id": id}))
		}
		for _, id := range []string{"e1", "e2", "e3", "e4", "e5"} {
			cs = append(cs, change(OpRetireEdge, doc{"edge_id": id}))
		}
		return revDoc("plan", "r", 1, cs...)
	}, rule: RuleEmptyGraph, path: "plan", detail: "no node"},
	{name: "edges without any node", req: func() doc { return revDoc("plan", "r", 0, addEdge("e", "a", "b", EdgeArtifactVerified)) }, rule: RuleEmptyGraph, path: "plan"},
	{name: "duplicate node id in the revision", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("a", NodeNonPR))
	}, rule: RuleDuplicateNodeID, path: "changes[1].node.node_id", detail: "changes[0]"},
	{name: "duplicate node id already in the plan", base: base(), req: func() doc { return revDoc("plan", "r", 1, addNode("join", NodeNonPR)) },
		rule: RuleDuplicateNodeID, path: "changes[0].node.node_id", detail: "is in the plan"},
	{name: "duplicate edge id in the revision", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR), addEdge("e", "a", "b", EdgeArtifactVerified), addEdge("e", "a", "b", EdgeArtifactVerified))
	}, rule: RuleDuplicateEdgeID, path: "changes[3].edge.edge_id", detail: "changes[2]"},
	{name: "duplicate edge id already in the plan", base: base(), req: func() doc {
		return revDoc("plan", "r", 1, addEdge("e1", "join", "ship", EdgeDecision))
	}, rule: RuleDuplicateEdgeID, path: "changes[0].edge.edge_id"},
	{name: "missing predecessor", req: func() doc {
		return revDoc("plan", "r", 0, addNode("b", NodeNonPR), addEdge("e", "ghost", "b", EdgeArtifactVerified))
	}, rule: RuleMissingPredecessor, path: "changes[1].edge", detail: "ghost"},
	{name: "missing successor", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addEdge("e", "a", "ghost", EdgeArtifactVerified))
	}, rule: RuleMissingSuccessor, path: "changes[1].edge", detail: "ghost"},
	{name: "retiring a node leaves its edge without a predecessor", base: base(), req: func() doc {
		return revDoc("plan", "r", 1, change(OpRetireNode, doc{"node_id": "design"}))
	}, rule: RuleMissingPredecessor, path: "edges.e1", detail: "design"},
	{name: "self reference", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addEdge("e", "a", "a", EdgeArtifactVerified))
	}, rule: RuleSelfReference, path: "changes[1].edge", detail: "itself"},
	{name: "cycle", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR), addNode("c", NodeNonPR),
			addEdge("e1", "a", "b", EdgeArtifactVerified), addEdge("e2", "b", "c", EdgeArtifactVerified), addEdge("e3", "c", "a", EdgeArtifactVerified))
	}, rule: RuleCycle, path: "plan", detail: "a -> b -> c -> a"},
	{name: "a revision that closes a cycle in an existing plan", base: base(), req: func() doc {
		return revDoc("plan", "r", 1, addEdge("back", "ship", "design", EdgeArtifactVerified))
	}, rule: RuleCycle, path: "plan", detail: "design"},
	{name: "update of a node that is not in the plan", base: base(), req: func() doc {
		return revDoc("plan", "r", 1, doc{"op": OpUpdateNode, "node": nodeDoc("ghost", NodeNonPR)})
	}, rule: RuleUnknownNode, path: "changes[0].node.node_id"},
	{name: "retire of an edge that is not in the plan", base: base(), req: func() doc {
		return revDoc("plan", "r", 1, change(OpRetireEdge, doc{"edge_id": "ghost"}))
	}, rule: RuleUnknownEdge, path: "changes[0].edge_id"},
	{name: "two changes to one node", base: base(), req: func() doc {
		return revDoc("plan", "r", 1, doc{"op": OpUpdateNode, "node": nodeDoc("join", NodeNonPR)}, change(OpRetireNode, doc{"node_id": "join"}))
	}, rule: RuleConflictingChanges, path: "changes[1].node_id", detail: "changes[0]"},
	{name: "a project cannot be switched", base: base(), req: func() doc {
		return with(revDoc("plan", "r", 1, addNode("extra", NodeNonPR)), "project_key", "P-OTHER")
	}, rule: RuleProjectMismatch, path: "$.project_key", detail: "P-TEST"},

	// edge integrity (contract 2.4) and the output conditions of the node kinds (1.2)
	{name: "integrated without a target repository and base ref", req: func() doc {
		e := edgeDoc("e", "a", "b", EdgeIntegrated)
		delete(e, "target_repository")
		delete(e, "target_base_ref")
		return revDoc("plan", "r", 0, addNode("a", NodeImplementation), addNode("b", NodeNonPR), doc{"op": OpAddEdge, "edge": e})
	}, rule: RuleEdgeFieldMissing, path: "changes[2].edge", detail: "target_repository, target_base_ref"},
	{name: "integrated leaving a non-implementation node", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR), addEdge("e", "a", "b", EdgeIntegrated))
	}, rule: RuleOutputIntegratedNeedsImpl, path: "changes[2].edge", detail: "implementation node"},
	{name: "code artifact_verified without a pinned head, target repository and base ref", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeImplementation), addNode("b", NodeNonPR), addEdge("e", "a", "b", EdgeArtifactVerified))
	}, rule: RuleEdgeFieldMissing, path: "changes[2].edge", detail: "pins_code_head, target_repository, target_base_ref"},
	{name: "code artifact_verified pinning a head but naming no base ref", req: func() doc {
		e := codeEdge("e", "a", "b")
		return revDoc("plan", "r", 0, addNode("a", NodeImplementation), addNode("b", NodeNonPR),
			doc{"op": OpAddEdge, "edge": edgeWith(e["edge"].(doc), "target_base_ref", nil)})
	}, rule: RuleEdgeFieldMissing, path: "changes[2].edge", detail: "target_base_ref"},
	{name: "non-code artifact_verified cannot pin a head", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR), codeEdge("e", "a", "b"))
	}, rule: RuleEdgeFieldNotApplicable, path: "changes[2].edge", detail: "pins_code_head"},
	{name: "decision without subject, digest and authority", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR),
			doc{"op": OpAddEdge, "edge": doc{"edge_id": "e", "from_node_id": "a", "to_node_id": "b", "kind": EdgeDecision}})
	}, rule: RuleEdgeFieldMissing, path: "changes[2].edge", detail: "decision_subject, decision_digest, required_authority"},
	{name: "decision leaving an implementation node", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeImplementation), addNode("b", NodeNonPR), addEdge("e", "a", "b", EdgeDecision))
	}, rule: RuleOutputDecisionNeedsNonPR, path: "changes[2].edge", detail: "non_pr"},
	{name: "a decision edge cannot name a target", req: func() doc {
		return revDoc("plan", "r", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR),
			doc{"op": OpAddEdge, "edge": edgeWith(edgeDoc("e", "a", "b", EdgeDecision), "target_repository", "owner/repo")})
	}, rule: RuleEdgeFieldNotApplicable, path: "changes[2].edge", detail: "target_repository"},

	// limits
	{name: "65 nodes", req: func() doc { return revDoc("plan", "r", 0, toDocs(manyNodes(MaxNodes+1))...) }, rule: RuleLimitExceeded, path: "plan", detail: "65 nodes"},
	{name: "257 edges", req: func() doc {
		cs := append(manyNodes(MaxNodes), chainEdges(MaxNodes, MaxEdges+1)...)
		return revDoc("plan", "r", 0, cs...)
	}, rule: RuleLimitExceeded, path: "$.changes", detail: "limit is 256"},
	{name: "257 changes", req: func() doc {
		var cs []doc
		for i := 0; i < MaxChanges+1; i++ {
			cs = append(cs, addNode(fmt.Sprintf("n%03d", i), NodeNonPR))
		}
		return revDoc("plan", "r", 0, cs...)
	}, rule: RuleLimitExceeded, path: "$.changes", detail: "257 changes"},
	{name: "id longer than 128", req: func() doc { return revDoc("plan", "r", 0, addNode(strings.Repeat("a", MaxIDLength+1), NodeNonPR)) }, rule: RuleValueTooLong, path: "changes[0].node.node_id"},
	{name: "title longer than 256", req: func() doc {
		n := nodeDoc("a", NodeNonPR)
		n["title"] = strings.Repeat("t", MaxTitleLength+1)
		return revDoc("plan", "r", 0, doc{"op": OpAddNode, "node": n})
	}, rule: RuleValueTooLong, path: "changes[0].node.title"},
}

func toDocs(in []doc) []doc { return in }

func asRejected(err error, target **PlanRejected) bool { return errors.As(err, target) }

func asUnreadable(err error, target **UnreadableError) bool { return errors.As(err, target) }

func countingRows(t *testing.T, r *Repo) map[string][]map[string]any {
	t.Helper()
	return zoneRows(t, r.Store.DB)
}

// Every row of the rejection matrix is refused with its rule, at its place, with an explanation that says why,
// and the refusal leaves every dag_ table exactly as it was.
func TestRejectionMatrix(t *testing.T) {
	for _, row := range matrix {
		if row.rule == "" {
			continue
		}
		t.Run(row.name, func(t *testing.T) {
			r, _, _ := newRepo(t)
			ctx := context.Background()
			if row.base != nil {
				if _, err := r.Put(ctx, decode(t, *row.base)); err != nil {
					t.Fatalf("the base revision is rejected: %v", err)
				}
			}
			before := countingRows(t, r)
			body, err := json.Marshal(row.req())
			if err != nil {
				t.Fatal(err)
			}
			rev, err := DecodeRevision(body)
			if err == nil {
				_, err = r.Put(ctx, rev)
			}
			p := rejected(t, err)
			if !hasRule(p, row.rule, row.path) {
				t.Fatalf("want rule %s at %q, got %v", row.rule, row.path, p.Violations)
			}
			if row.detail != "" {
				found := false
				for _, v := range p.Violations {
					found = found || (v.Rule == row.rule && strings.Contains(v.Detail, row.detail))
				}
				if !found {
					t.Fatalf("no %s violation explains itself with %q: %v", row.rule, row.detail, p.Violations)
				}
			}
			if after := countingRows(t, r); !reflect.DeepEqual(before, after) {
				t.Fatalf("a rejected revision changed the store:\n before %v\n after  %v", before, after)
			}
			// the same input is answered the same way: the same violations in the same order
			_, again := func() (Revision, error) {
				rev, err := DecodeRevision(body)
				if err == nil {
					_, err = r.Put(ctx, rev)
				}
				return rev, err
			}()
			if again == nil || again.Error() != err.Error() {
				t.Fatalf("the same input answered differently:\n first  %v\n second %v", err, again)
			}
		})
	}
}

// An id that was retired is never introduced again: the retirement of a node and the later reuse of its id.
func TestRetiredIDsAreNeverReused(t *testing.T) {
	r, _, _ := newRepo(t)
	ctx := context.Background()
	mustPut(t, r, forkJoin("plan", "r1"))
	mustPut(t, r, revDoc("plan", "r2", 1, change(OpRetireEdge, doc{"edge_id": "e5"}), change(OpRetireNode, doc{"node_id": "ship"})))
	_, err := r.Put(ctx, decode(t, revDoc("plan", "r3", 2, addNode("ship", NodeNonPR))))
	p := rejected(t, err)
	if !hasRule(p, RuleDuplicateNodeID, "changes[0].node.node_id") || !strings.Contains(p.Violations[0].Detail, "was retired at revision 2") {
		t.Fatalf("reusing a retired node id: %v", p.Violations)
	}
	_, err = r.Put(ctx, decode(t, revDoc("plan", "r4", 2, addEdge("e5", "join", "design", EdgeDecision))))
	p = rejected(t, err)
	if !hasRule(p, RuleDuplicateEdgeID, "changes[0].edge.edge_id") {
		t.Fatalf("reusing a retired edge id: %v", p.Violations)
	}
	// the log still replays: VerifyLog judges history, and found nothing reused
	if err := r.VerifyLog(ctx, "plan"); err != nil {
		t.Fatal(err)
	}
}

// The limits are exact: the limit itself is accepted and one more is not.
func TestLimitsAreExact(t *testing.T) {
	r, _, _ := newRepo(t)
	ctx := context.Background()
	cs := append(manyNodes(MaxNodes), chainEdges(MaxNodes, MaxEdges)...)
	if len(cs) != MaxNodes+MaxEdges || len(cs) > MaxChanges+MaxNodes {
		t.Fatalf("test setup: %d changes", len(cs))
	}
	// 64 nodes and 256 edges do not fit one revision of 256 changes: two revisions build the plan
	first := revDoc("plan", "r1", 0, cs[:MaxNodes+(MaxChanges-MaxNodes)]...)
	if _, err := r.Put(ctx, decode(t, first)); err != nil {
		t.Fatalf("64 nodes and %d edges: %v", MaxChanges-MaxNodes, err)
	}
	if _, err := r.Put(ctx, decode(t, revDoc("plan", "r2", 1, cs[MaxNodes+(MaxChanges-MaxNodes):]...))); err != nil {
		t.Fatalf("256 edges in all: %v", err)
	}
	snap, _, err := r.Snapshot(ctx, "plan", 0)
	if err != nil || len(snap.Nodes) != MaxNodes || len(snap.Edges) != MaxEdges {
		t.Fatalf("the plan at its limits: %d nodes, %d edges (%v)", len(snap.Nodes), len(snap.Edges), err)
	}
	_, err = r.Put(ctx, decode(t, revDoc("plan", "r3", 2, addEdge("one-more", "n000", "n063", EdgeArtifactVerified))))
	p := rejected(t, err)
	if !hasRule(p, RuleLimitExceeded, "plan") || !strings.Contains(p.Violations[0].Detail, "257 edges") {
		t.Fatalf("the 257th edge: %v", p.Violations)
	}
	_, err = r.Put(ctx, decode(t, revDoc("plan", "r4", 2, addNode("one-more", NodeNonPR))))
	p = rejected(t, err)
	if !hasRule(p, RuleLimitExceeded, "plan") || !strings.Contains(p.Violations[0].Detail, "65 nodes") {
		t.Fatalf("the 65th node: %v", p.Violations)
	}
	// the document size limit is read before the document is
	big := append([]byte(`{"pad":"`), append([]byte(strings.Repeat("x", MaxDocumentBytes)), []byte(`"}`)...)...)
	_, err = DecodeRevision(big)
	p = rejected(t, err)
	if !hasRule(p, RuleLimitExceeded, "$") {
		t.Fatalf("a document over %d bytes: %v", MaxDocumentBytes, p.Violations)
	}
}

func mustPut(t testing.TB, r *Repo, d doc) Result {
	t.Helper()
	res, err := r.Put(context.Background(), decode(t, d))
	if err != nil {
		t.Fatalf("put %v: %v", d["request_id"], err)
	}
	return res
}

// Not JSON at all is not a rejected plan: it is a document nobody can read.
func TestDocumentThatIsNotJSON(t *testing.T) {
	for _, in := range []string{"", "not json", "{", `{"a":1,"a":2}`} {
		_, err := DecodeRevision([]byte(in))
		var unreadable *UnreadableError
		var p *PlanRejected
		switch {
		case in == `{"a":1,"a":2}`:
			if !asRejected(err, &p) || !hasRule(p, RuleDuplicateKey, "$") {
				t.Errorf("a repeated key: %v", err)
			}
		case !asUnreadable(err, &unreadable):
			t.Errorf("%q: %v", in, err)
		}
	}
}
