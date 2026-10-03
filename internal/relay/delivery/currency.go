package delivery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
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
func requestedPredecessors(ctx context.Context, q store.Querier, rid string, generation int64) (map[string][]string, error) {
	// the generation a ruling's correction follows: the one before it, or the nearest one that was not withdrawn (CRW-446)
	before, err := store.LiveGenerationBefore(ctx, q, rid, generation)
	if err != nil {
		return nil, err
	}
	rows, err := allFrom(ctx, q, "SELECT p.event_id, p.revision_hash, v.verdict_turn_id, r.event_id AS request_id, g.dispatch_request_id FROM generations g JOIN verdicts v ON v.next_generation = g.execution_generation JOIN events p ON p.event_id = v.event_id AND p.relationship_id = g.relationship_id JOIN events r ON r.relationship_id = g.relationship_id AND r.execution_generation = g.execution_generation WHERE g.relationship_id = ? AND g.execution_generation = ? AND g.reason = 'needs_changes_revision' AND v.verdict = 'needs_changes' AND p.execution_generation = ? AND p.outcome = ? AND p.stage = 'final' AND p.suppressed_reason IS NULL AND r.outcome = 'revision_request' AND r.producer = 'relay' AND r.stage = 'final' AND r.suppressed_reason IS NULL", rid, generation, before, "ready_for_review")
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
	return HeadRevisionFrom(ctx, s.Q(ctx), rid, generation)
}

// headRevisionSQL reads the reviewable receipts of one generation, each with the predecessor it
// declares and whether the relay holds it as suppressed, in one statement: a suppressed receipt is
// no revision, but what it declared is read when a revision names it (CRW-470), and one statement
// is one moment's answer for both. The lineage row is found by its primary key (relationship,
// generation, event): joined on event_id alone, SQLite scans the whole of revision_lineage once for
// every event row, so one judgment cost the revisions of the generation times the lineage rows of
// the store (CRW-416). The product stores an event and its lineage row together under one
// relationship and generation, so this reads the rows the event_id join read.
const headRevisionSQL = "SELECT e.event_id, e.revision_hash, l.supersedes_hash, e.suppressed_reason IS NOT NULL FROM events e LEFT JOIN revision_lineage l ON l.relationship_id = e.relationship_id AND l.execution_generation = e.execution_generation AND l.event_id = e.event_id WHERE e.relationship_id = ? AND e.execution_generation = ? AND e.outcome = ? ORDER BY e.event_id"

// revision is one reviewable revision of a generation as the head judgment reads it: its event, its
// revision hash and the predecessor hash it declares ("" for none).
type revision struct{ id, hash, declared string }

// textOf reads a column as Row.S does: text, or "" for NULL and for any other type.
func textOf(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// reviewableRevisions is every reviewable revision of the generation in event id order, and the
// suppressed receipts apart: the predecessors they declared by revision hash. Reading the revisions
// all is what a head is: a fork, a cycle or an unknown predecessor anywhere among them makes the
// whole generation ambiguous and names every revision as a competitor.
func reviewableRevisions(ctx context.Context, q store.Querier, rid string, generation int64) (_ []revision, suppressed map[string][]string, err error) {
	rows, err := q.QueryContext(ctx, headRevisionSQL, rid, generation, "ready_for_review")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var out []revision
	for rows.Next() {
		var id, hash, declared any
		var held bool
		if err := rows.Scan(&id, &hash, &declared, &held); err != nil {
			return nil, nil, err
		}
		if held {
			if suppressed == nil {
				suppressed = map[string][]string{}
			}
			suppressed[textOf(hash)] = append(suppressed[textOf(hash)], textOf(declared))
			continue
		}
		out = append(out, revision{id: textOf(id), hash: textOf(hash), declared: textOf(declared)})
	}
	return out, suppressed, rows.Err()
}

// HeadRevisionFrom reads through the caller's snapshot, including a read-only guard connection.
func HeadRevisionFrom(ctx context.Context, q store.Querier, rid string, generation int64) (Obj, error) {
	revisions, suppressed, err := reviewableRevisions(ctx, q, rid, generation)
	if err != nil {
		return nil, err
	}
	if len(revisions) == 0 {
		return Obj{{Key: "eventId", Value: nil}, {Key: "revisionHash", Value: nil}, {Key: "evidence", Value: NoRevision}, {Key: "competitors", Value: []any{}}, {Key: "detail", Value: "no reviewable revision in this generation"}}, nil
	}
	anchors, err := requestedPredecessors(ctx, q, rid, generation)
	if err != nil {
		return nil, err
	}
	return judgeHead(readThroughSuppressed(revisions, suppressed, anchors), anchors), nil
}

// readThroughSuppressed returns the revisions with each declared predecessor that names a
// suppressed receipt of the generation read as naming what that receipt replaced (CRW-470; the
// rule is registry.ReadThrough's). A suppressed receipt is not a revision, and a child that
// restarted cannot know its earlier receipt was one. The revisions come back as they were when no
// receipt is suppressed, which is nearly every judgment.
func readThroughSuppressed(revisions []revision, suppressed map[string][]string, anchors map[string][]string) []revision {
	if len(suppressed) == 0 {
		return revisions
	}
	held := make(map[string]int, len(revisions))
	seen := make(map[string]bool, len(revisions))
	named := make([]string, 0, len(revisions))
	// An event id is the events primary key, so each revision is listed once (as in judgeHead).
	for _, r := range revisions {
		if !seen[r.id] {
			seen[r.id] = true
			held[r.hash]++
		}
		named = append(named, r.declared)
	}
	through := registry.ReadThrough(named, func(hash string) int { return held[hash] + len(anchors[hash]) }, suppressed)
	if len(through) == 0 {
		return revisions
	}
	out := slices.Clone(revisions)
	for i := range out {
		if effective, ok := through[out[i].declared]; ok {
			out[i].declared = effective
		}
	}
	return out
}

// judgeHead is currency.head_revision over the generation's reviewable revisions (at least one, in
// event id order) and its requested correction predecessors.
func judgeHead(revisions []revision, anchors map[string][]string) Obj {
	nodes := make([]string, 0, len(revisions))
	hashOf := make(map[string]string, len(revisions))
	declaredOf := make(map[string]string, len(revisions))
	for _, r := range revisions {
		// An event id is the events primary key and the lineage row is found by its own key, so it
		// is listed once; were it repeated, it would keep its first place and its last values.
		if _, seen := hashOf[r.id]; !seen {
			nodes = append(nodes, r.id)
		}
		hashOf[r.id] = r.hash
		declaredOf[r.id] = r.declared
	}
	byHash := make(map[string][]string, len(nodes))
	for _, id := range nodes {
		byHash[hashOf[id]] = append(byHash[hashOf[id]], id)
	}
	edges := make(map[string]string, len(nodes))
	var unresolved []string
	for _, id := range nodes {
		declared := declaredOf[id]
		if declared == "" {
			continue
		}
		held, requested := byHash[declared], anchors[declared]
		if len(held)+len(requested) != 1 {
			unresolved = append(unresolved, id+" -> "+declared)
			continue
		}
		if len(held) == 1 {
			edges[id] = held[0]
		} else {
			edges[id] = requested[0]
		}
	}
	if len(unresolved) > 0 {
		return ambiguous(UnknownPredecessor, nodes, "a declared predecessor is neither a unique revision of this generation nor its requested correction predecessor: "+strings.Join(unresolved, ", "))
	}
	// A revision declares at most one predecessor, so a walk along the declared chain ends or
	// returns to a revision it has passed. Starts are taken in event id order and the first walk that
	// returns reports where it started and the revision it returned to. A walk that reaches a
	// revision an earlier walk finished without returning stops there: what follows was walked then,
	// and none of it is on the walk so far. Each revision is walked once.
	const (
		walking = iota + 1
		cleared
	)
	passed := make(map[string]uint8, len(nodes))
	for _, start := range nodes {
		if passed[start] == cleared {
			continue
		}
		walk := []string{start}
		passed[start] = walking
		for current := start; ; {
			next, ok := edges[current]
			if !ok {
				break
			}
			current = next
			if passed[current] == walking {
				return ambiguous(Cycle, nodes, fmt.Sprintf("the declared chain from %s returns to %s", start, current))
			}
			if passed[current] == cleared {
				break
			}
			passed[current] = walking
			walk = append(walk, current)
		}
		for _, id := range walk {
			passed[id] = cleared
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
		return ambiguous(Fork, nodes, "more than one revision declares the same predecessor: "+strings.Join(forked, ", "))
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
		return ambiguous(Fork, nodes, fmt.Sprintf("%d revisions in this generation are unsuperseded, so none of them is the head; a revision that replaces another says so when it is emitted", len(tips)))
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
			return ambiguous(Disconnected, nodes, "the declared chain from the tip does not reach every revision in this generation")
		}
	}
	evidence := Chain
	if len(nodes) == 1 && len(edges) == 0 {
		evidence = Sole
	}
	return Obj{{Key: "eventId", Value: tip}, {Key: "revisionHash", Value: hashOf[tip]}, {Key: "evidence", Value: evidence}, {Key: "competitors", Value: []any{}}, {Key: "detail", Value: ""}}
}

// Currency is currency.currency_of: is this event the thing the assignment stands on now?
func Currency(ctx context.Context, s *store.Store, relationship, event Row) (Obj, error) {
	rid := event.S("relationship_id")
	if relationship.S("status") != "active" || truthy(relationship.Opt("superseded_by")) {
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: RelationshipNotActive}, {Key: "evidence", Value: nil}, {Key: "headEventId", Value: nil}, {Key: "headRevisionHash", Value: nil}, {Key: "detail", Value: fmt.Sprintf("relationship %q is %q", rid, relationship.S("status"))}}, nil
	}
	generation := relationship.I("execution_generation")
	if event.I("execution_generation") != generation {
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: StaleGeneration}, {Key: "evidence", Value: nil}, {Key: "headEventId", Value: nil}, {Key: "headRevisionHash", Value: nil}, {Key: "detail", Value: fmt.Sprintf("this event is generation %d and the assignment is on generation %d", event.I("execution_generation"), generation)}}, nil
	}
	head, err := HeadRevision(ctx, s, rid, generation)
	if err != nil {
		return nil, err
	}
	if slices.Contains(ambiguousEvidence, pyjson.Text(head.Get("evidence"))) {
		competitors, _ := head.Lookup("competitors")
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: RevisionAmbiguous}, {Key: "evidence", Value: pyjson.Text(head.Get("evidence"))}, {Key: "headEventId", Value: nil}, {Key: "headRevisionHash", Value: nil}, {Key: "detail", Value: pyjson.Text(head.Get("detail"))}, {Key: "competitors", Value: competitors}}, nil
	}
	headID, _ := head.Lookup("eventId")
	headHash, _ := head.Lookup("revisionHash")
	if headID != event.S("event_id") {
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: SupersededRevision}, {Key: "evidence", Value: pyjson.Text(head.Get("evidence"))}, {Key: "headEventId", Value: headID}, {Key: "headRevisionHash", Value: headHash}, {Key: "detail", Value: fmt.Sprintf("the current revision of generation %d is %v", generation, headID)}}, nil
	}
	return Obj{{Key: "current", Value: true}, {Key: "reason", Value: nil}, {Key: "evidence", Value: pyjson.Text(head.Get("evidence"))}, {Key: "headEventId", Value: headID}, {Key: "headRevisionHash", Value: headHash}, {Key: "detail", Value: ""}}, nil
}
