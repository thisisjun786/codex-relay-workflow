package delivery

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Lineage evidence and currency reasons (currency.py).
const (
	Sole               = "sole_revision"
	Chain              = "declared_chain"
	NoRevision         = "no_revision"
	Fork               = "fork"
	Cycle              = "cycle"
	UnknownPredecessor = "unknown_predecessor"
	Disconnected       = "disconnected"

	StaleGeneration    = "stale_generation"
	SupersededRevision = "superseded_revision"
	RevisionAmbiguous  = "revision_ambiguous"
)

var ambiguousEvidence = []string{Fork, Cycle, UnknownPredecessor, Disconnected}

func ambiguous(evidence string, nodes []string, detail string) Obj {
	competitors := make([]any, len(nodes))
	sorted := slices.Clone(nodes)
	slices.Sort(sorted)
	for i, n := range sorted {
		competitors[i] = n
	}
	return Obj{{Key: "eventId", Value: nil}, {Key: "revisionHash", Value: nil}, {Key: "evidence", Value: evidence}, {Key: "competitors", Value: competitors}, {Key: "detail", Value: detail}}
}

// requestedPredecessors is currency._requested_predecessors: only the result whose ruling opened
// this correction is an external root.
func requestedPredecessors(ctx context.Context, s *store.Store, rid string, generation int64) (map[string][]string, error) {
	rows, err := all(ctx, s, "SELECT p.event_id, p.revision_hash, v.verdict_turn_id, r.event_id AS request_id, g.dispatch_request_id FROM generations g JOIN verdicts v ON v.next_generation = g.execution_generation JOIN events p ON p.event_id = v.event_id AND p.relationship_id = g.relationship_id JOIN events r ON r.relationship_id = g.relationship_id AND r.execution_generation = g.execution_generation WHERE g.relationship_id = ? AND g.execution_generation = ? AND g.reason = 'needs_changes_revision' AND v.verdict = 'needs_changes' AND p.execution_generation = g.execution_generation - 1 AND p.outcome = ? AND p.stage = 'final' AND p.suppressed_reason IS NULL AND r.outcome = 'revision_request' AND r.producer = 'relay' AND r.stage = 'final' AND r.suppressed_reason IS NULL", rid, generation, "ready_for_review")
	if err != nil {
		return nil, err
	}
	anchors := map[string][]string{}
	for _, row := range rows {
		request, err := store.RevisionRequestEventID(rid, row.S("event_id"), row.S("verdict_turn_id"))
		if err != nil {
			continue
		}
		if row.S("request_id") == request && row.S("dispatch_request_id") == "revision-"+request {
			anchors[row.S("revision_hash")] = append(anchors[row.S("revision_hash")], row.S("event_id"))
		}
	}
	return anchors, nil
}

// HeadRevision is currency.head_revision: the one revision this generation stands on, or why not.
func HeadRevision(ctx context.Context, s *store.Store, rid string, generation int64) (Obj, error) {
	rows, err := all(ctx, s, "SELECT e.event_id, e.revision_hash, l.supersedes_hash FROM events e LEFT JOIN revision_lineage l ON l.event_id = e.event_id WHERE e.relationship_id = ? AND e.execution_generation = ? AND e.outcome = ? AND e.suppressed_reason IS NULL ORDER BY e.event_id", rid, generation, "ready_for_review")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return Obj{{Key: "eventId", Value: nil}, {Key: "revisionHash", Value: nil}, {Key: "evidence", Value: NoRevision}, {Key: "competitors", Value: []any{}}, {Key: "detail", Value: "no reviewable revision in this generation"}}, nil
	}
	var nodes []string
	hashOf := map[string]string{}
	declaredOf := map[string]string{}
	byHash := map[string][]string{}
	for _, row := range rows {
		id := row.S("event_id")
		if _, seen := hashOf[id]; !seen {
			nodes = append(nodes, id)
		}
		hashOf[id] = row.S("revision_hash")
		declaredOf[id] = row.S("supersedes_hash")
	}
	for _, id := range nodes {
		byHash[hashOf[id]] = append(byHash[hashOf[id]], id)
	}
	anchors, err := requestedPredecessors(ctx, s, rid, generation)
	if err != nil {
		return nil, err
	}
	edges := map[string]string{}
	var unresolved []string
	for _, id := range nodes {
		declared := declaredOf[id]
		if declared == "" {
			continue
		}
		targets := append(slices.Clone(byHash[declared]), anchors[declared]...)
		if len(targets) != 1 {
			unresolved = append(unresolved, id+" -> "+declared)
			continue
		}
		edges[id] = targets[0]
	}
	if len(unresolved) > 0 {
		return ambiguous(UnknownPredecessor, nodes, "a declared predecessor is neither a unique revision of this generation nor its requested correction predecessor: "+strings.Join(unresolved, ", ")), nil
	}
	for _, start := range nodes {
		seen := map[string]bool{start: true}
		current := start
		for {
			next, ok := edges[current]
			if !ok {
				break
			}
			current = next
			if seen[current] {
				return ambiguous(Cycle, nodes, fmt.Sprintf("the declared chain from %s returns to %s", start, current)), nil
			}
			seen[current] = true
		}
	}
	predecessors := map[string]int{}
	for _, id := range nodes {
		if target, ok := edges[id]; ok {
			predecessors[target]++
		}
	}
	var forked []string
	for target, count := range predecessors {
		if count > 1 {
			forked = append(forked, target)
		}
	}
	if len(forked) > 0 {
		slices.Sort(forked)
		return ambiguous(Fork, nodes, "more than one revision declares the same predecessor: "+strings.Join(forked, ", ")), nil
	}
	targets := map[string]bool{}
	for _, t := range edges {
		targets[t] = true
	}
	var tips []string
	for _, id := range nodes {
		if !targets[id] {
			tips = append(tips, id)
		}
	}
	slices.Sort(tips)
	if len(tips) != 1 {
		return ambiguous(Fork, nodes, fmt.Sprintf("%d revisions in this generation are unsuperseded, so none of them is the head; a revision that replaces another says so when it is emitted", len(tips))), nil
	}
	tip := tips[0]
	covered := map[string]bool{tip: true}
	for current := tip; ; {
		next, ok := edges[current]
		if !ok {
			break
		}
		current = next
		covered[current] = true
	}
	for _, id := range nodes {
		if !covered[id] {
			return ambiguous(Disconnected, nodes, "the declared chain from the tip does not reach every revision in this generation"), nil
		}
	}
	evidence := Chain
	if len(nodes) == 1 && len(edges) == 0 {
		evidence = Sole
	}
	return Obj{{Key: "eventId", Value: tip}, {Key: "revisionHash", Value: hashOf[tip]}, {Key: "evidence", Value: evidence}, {Key: "competitors", Value: []any{}}, {Key: "detail", Value: ""}}, nil
}

// Currency is currency.currency_of: is this event the thing the assignment stands on now?
func Currency(ctx context.Context, s *store.Store, relationship, event Row) (Obj, error) {
	rid := event.S("relationship_id")
	if relationship.S("status") != "active" || truthy(relationship.Opt("superseded_by")) {
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: RelationshipNotActive}, {Key: "evidence", Value: nil}, {Key: "headEventId", Value: nil}, {Key: "headRevisionHash", Value: nil}, {Key: "detail", Value: fmt.Sprintf("relationship %s is %s", store.PyRepr(rid), store.PyRepr(relationship.S("status")))}}, nil
	}
	generation := relationship.I("execution_generation")
	if event.I("execution_generation") != generation {
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: StaleGeneration}, {Key: "evidence", Value: nil}, {Key: "headEventId", Value: nil}, {Key: "headRevisionHash", Value: nil}, {Key: "detail", Value: fmt.Sprintf("this event is generation %d and the assignment is on generation %d", event.I("execution_generation"), generation)}}, nil
	}
	head, err := HeadRevision(ctx, s, rid, generation)
	if err != nil {
		return nil, err
	}
	if slices.Contains(ambiguousEvidence, str(head, "evidence")) {
		competitors, _ := get(head, "competitors")
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: RevisionAmbiguous}, {Key: "evidence", Value: str(head, "evidence")}, {Key: "headEventId", Value: nil}, {Key: "headRevisionHash", Value: nil}, {Key: "detail", Value: str(head, "detail")}, {Key: "competitors", Value: competitors}}, nil
	}
	headID, _ := get(head, "eventId")
	headHash, _ := get(head, "revisionHash")
	if headID != event.S("event_id") {
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: SupersededRevision}, {Key: "evidence", Value: str(head, "evidence")}, {Key: "headEventId", Value: headID}, {Key: "headRevisionHash", Value: headHash}, {Key: "detail", Value: fmt.Sprintf("the current revision of generation %d is %v", generation, headID)}}, nil
	}
	return Obj{{Key: "current", Value: true}, {Key: "reason", Value: nil}, {Key: "evidence", Value: str(head, "evidence")}, {Key: "headEventId", Value: headID}, {Key: "headRevisionHash", Value: headHash}, {Key: "detail", Value: ""}}, nil
}
