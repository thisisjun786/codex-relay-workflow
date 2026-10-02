package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// canonical is canonical JSON (keys sorted, no whitespace, UTF-8 as it is): the serialization every
// digest of the relay hashes (delivery.SetDigest, mergeturn's digests; contract 4.1).
func canonical(v any) string {
	return pyjson.Dumps(v, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
}

func sum(v any) string {
	h := sha256.Sum256([]byte(canonical(v)))
	return hex.EncodeToString(h[:])
}

// Canonical is the canonical JSON every digest of this package hashes, for the packages that build digests of
// their own over the same serialization (the scheduler's acceptance and evidence digests, contract 4.1).
func Canonical(v any) string { return canonical(v) }

func nodeObject(n Node) map[string]any {
	m := map[string]any{"node_id": n.NodeID, "issue_key": n.IssueKey, "kind": n.Kind, "criteria_set_digest": n.CriteriaSetDigest}
	if n.Title != "" {
		m["title"] = n.Title
	}
	return m
}

// edgeObject is an edge's spec with the fields that apply to it: an unset optional field is absent,
// so equal specs serialize equally.
func edgeObject(e Edge) map[string]any {
	m := map[string]any{"edge_id": e.EdgeID, "from_node_id": e.FromNodeID, "to_node_id": e.ToNodeID, "kind": e.Kind}
	if e.TargetRepository != "" {
		m["target_repository"] = e.TargetRepository
	}
	if e.TargetBaseRef != "" {
		m["target_base_ref"] = e.TargetBaseRef
	}
	if e.PinsCodeHead {
		m["pins_code_head"] = true
	}
	if e.DecisionSubject != "" {
		m["decision_subject"] = e.DecisionSubject
	}
	if e.DecisionDigest != "" {
		m["decision_digest"] = e.DecisionDigest
	}
	if len(e.RequiredAuthority) > 0 {
		m["required_authority"] = authorityList(e.RequiredAuthority)
	}
	return m
}

func authorityList(in []string) []any {
	sorted := append([]string(nil), in...)
	sort.Strings(sorted)
	out := make([]any, len(sorted))
	for i, s := range sorted {
		out[i] = s
	}
	return out
}

func changeObject(c Change) map[string]any {
	m := map[string]any{"op": c.Op}
	switch c.Op {
	case OpAddNode, OpUpdateNode:
		m["node"] = nodeObject(*c.Node)
	case OpReplaceNode:
		m["node"] = nodeObject(*c.Node)
		m["supersedes_node_id"] = c.SupersedesNodeID
	case OpRetireNode, OpPauseNode, OpResumeNode, OpCancelNode, OpArchiveNode:
		m["node_id"] = c.NodeID
	case OpAddEdge:
		m["edge"] = edgeObject(*c.Edge)
	case OpRetireEdge:
		m["edge_id"] = c.EdgeID
	}
	return m
}

// ChangesJSON is the canonical JSON of a change list, as a revision row stores it.
func ChangesJSON(changes []Change) string {
	list := make([]any, len(changes))
	for i, c := range changes {
		list[i] = changeObject(c)
	}
	return canonical(list)
}

// SliceDigest is the digest of a node's slice: its spec and its incoming edges (sorted by id), nothing
// else. It is not a hash of the plan and carries no revision number, so editing a node changes the
// digest of that node alone (and an edge's change moves only the node it points to).
func SliceDigest(n Node, incoming []Edge) string {
	sorted := append([]Edge(nil), incoming...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].EdgeID < sorted[j].EdgeID })
	edges := make([]any, len(sorted))
	for i, e := range sorted {
		edges[i] = edgeObject(e)
	}
	return sum(map[string]any{"schema": SchemaSlice, "node": nodeObject(n), "incoming": edges})
}

// stateDigest is the digest of a plan's live content: what two equal plans share whatever revisions
// produced them. The lifecycle (CRW-281) is part of it only when it is not the default, so a plan that was never
// paused, cancelled or archived digests exactly as it did before there were lifecycle changes.
func stateDigest(planID, project, planState string, nodes []SnapNode, edges []SnapEdge) string {
	ns := make([]any, len(nodes))
	for i, n := range nodes {
		m := nodeObject(n.Node)
		m["slice_digest"] = n.SliceDigest
		if n.SupersedesNodeID != "" {
			m["supersedes_node_id"] = n.SupersedesNodeID
		}
		if n.Lifecycle != "" {
			m["lifecycle"] = n.Lifecycle
		}
		ns[i] = m
	}
	es := make([]any, len(edges))
	for i, e := range edges {
		es[i] = edgeObject(e.Edge)
	}
	content := map[string]any{"schema": SchemaSnapshot, "plan_id": planID, "project_key": project, "nodes": ns, "edges": es}
	if planState != "" {
		content["plan_state"] = planState
	}
	return sum(content)
}

// RequestDigest identifies a request by what it asks, whatever its key order or whitespace: the
// repeated request that returns the stored result, and the reused request id that conflicts.
func RequestDigest(r Revision) string {
	changes := make([]any, len(r.Changes))
	for i, c := range r.Changes {
		changes[i] = changeObject(c)
	}
	return sum(map[string]any{"schema": SchemaRevision, "plan_id": r.PlanID, "project_key": r.ProjectKey, "request_id": r.RequestID,
		"expected_parent_revision": r.ExpectedParent, "coordinator_epoch": r.CoordinatorEpoch, "author_task_id": r.AuthorTaskID, "changes": changes})
}
