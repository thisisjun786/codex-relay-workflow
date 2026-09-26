package registry

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// currency.py's head_revision, as the assignment view reads it: which revision a generation
// currently stands on, decided from declared lineage, never from arrival order.

// Lineage evidence words (currency.py).
const (
	evidenceSole               = "sole_revision"
	evidenceChain              = "declared_chain"
	evidenceNone               = "no_revision"
	evidenceFork               = "fork"
	evidenceCycle              = "cycle"
	evidenceUnknownPredecessor = "unknown_predecessor"
	evidenceDisconnected       = "disconnected"
	reviewable                 = "ready_for_review"
)

// ambiguousEvidence is currency.AMBIGUOUS.
var ambiguousEvidence = []string{evidenceFork, evidenceCycle, evidenceUnknownPredecessor, evidenceDisconnected}

// Head is head_revision's answer; EventID "" is None.
type Head struct {
	EventID, RevisionHash, Evidence, Detail string
	Competitors                             []string
}

// Ambiguous reports whether the head's evidence is one of currency.AMBIGUOUS.
func (h Head) Ambiguous() bool { return slices.Contains(ambiguousEvidence, h.Evidence) }

func (h Head) record() contract.OrderedObject {
	competitors := anyStrings(h.Competitors)
	return contract.OrderedObject{{Key: "eventId", Value: nullText(h.EventID)}, {Key: "revisionHash", Value: nullText(h.RevisionHash)},
		{Key: "evidence", Value: h.Evidence}, {Key: "competitors", Value: competitors}, {Key: "detail", Value: h.Detail}}
}

func ambiguousHead(evidence string, nodes []string, detail string) Head {
	sorted := slices.Clone(nodes)
	slices.Sort(sorted)
	return Head{Evidence: evidence, Competitors: sorted, Detail: detail}
}

func colString(row store.Row, name string) string {
	s, _ := row.Get(name).(string)
	return s
}

// requestedPredecessors is currency._requested_predecessors.
func requestedPredecessors(ctx context.Context, s *store.Store, rid string, generation int64) (map[string][]string, error) {
	rows, err := s.All(ctx, "SELECT p.event_id, p.revision_hash, v.verdict_turn_id, r.event_id AS request_id,"+
		" g.dispatch_request_id"+
		" FROM generations g"+
		" JOIN verdicts v ON v.next_generation = g.execution_generation"+
		" JOIN events p ON p.event_id = v.event_id AND p.relationship_id = g.relationship_id"+
		" JOIN events r ON r.relationship_id = g.relationship_id"+
		" AND r.execution_generation = g.execution_generation"+
		" WHERE g.relationship_id = ? AND g.execution_generation = ?"+
		" AND g.reason = 'needs_changes_revision' AND v.verdict = 'needs_changes'"+
		" AND p.execution_generation = g.execution_generation - 1"+
		" AND p.outcome = ? AND p.stage = 'final' AND p.suppressed_reason IS NULL"+
		" AND r.outcome = 'revision_request' AND r.producer = 'relay'"+
		" AND r.stage = 'final' AND r.suppressed_reason IS NULL", rid, generation, reviewable)
	if err != nil {
		return nil, err
	}
	anchors := map[string][]string{}
	for _, row := range rows {
		request, err := store.RevisionRequestEventID(rid, colString(row, "event_id"), colString(row, "verdict_turn_id"))
		if err != nil {
			return nil, &HostError{Class: "ValueError", Detail: err.Error()}
		}
		if colString(row, "request_id") == request && colString(row, "dispatch_request_id") == "revision-"+request {
			hash := colString(row, "revision_hash")
			anchors[hash] = append(anchors[hash], colString(row, "event_id"))
		}
	}
	return anchors, nil
}

// HeadRevision is currency.head_revision.
func HeadRevision(ctx context.Context, s *store.Store, rid string, generation int64) (Head, error) {
	rows, err := s.All(ctx, "SELECT e.event_id, e.revision_hash, l.supersedes_hash"+
		"  FROM events e"+
		"  LEFT JOIN revision_lineage l ON l.event_id = e.event_id"+
		" WHERE e.relationship_id = ? AND e.execution_generation = ?"+
		"   AND e.outcome = ? AND e.suppressed_reason IS NULL"+
		" ORDER BY e.event_id", rid, generation, reviewable)
	if err != nil {
		return Head{}, err
	}
	if len(rows) == 0 {
		return Head{Evidence: evidenceNone, Competitors: []string{}, Detail: "no reviewable revision in this generation"}, nil
	}
	var nodes []string
	hashOf, declaredOf := map[string]string{}, map[string]string{}
	for _, row := range rows {
		id := colString(row, "event_id")
		if _, seen := hashOf[id]; !seen {
			nodes = append(nodes, id)
		}
		hashOf[id] = colString(row, "revision_hash")
		declaredOf[id] = colString(row, "supersedes_hash")
	}
	byHash := map[string][]string{}
	for _, id := range nodes {
		byHash[hashOf[id]] = append(byHash[hashOf[id]], id)
	}
	anchors, err := requestedPredecessors(ctx, s, rid, generation)
	if err != nil {
		return Head{}, err
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
		return ambiguousHead(evidenceUnknownPredecessor, nodes, "a declared predecessor is neither a unique revision of this generation "+
			"nor its requested correction predecessor: "+strings.Join(unresolved, ", ")), nil
	}
	for _, start := range nodes {
		seen := map[string]bool{start: true}
		for current := start; ; {
			next, ok := edges[current]
			if !ok {
				break
			}
			current = next
			if seen[current] {
				return ambiguousHead(evidenceCycle, nodes, fmt.Sprintf("the declared chain from %s returns to %s", start, current)), nil
			}
			seen[current] = true
		}
	}
	predecessors := map[string]int{}
	for _, target := range edges {
		predecessors[target]++
	}
	var forked []string
	for target, count := range predecessors {
		if count > 1 {
			forked = append(forked, target)
		}
	}
	if len(forked) > 0 {
		slices.Sort(forked)
		return ambiguousHead(evidenceFork, nodes, "more than one revision declares the same predecessor: "+strings.Join(forked, ", ")), nil
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
		return ambiguousHead(evidenceFork, nodes, fmt.Sprintf("%d revisions in this generation are unsuperseded, so none of them is "+
			"the head; a revision that replaces another says so when it is emitted", len(tips))), nil
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
			return ambiguousHead(evidenceDisconnected, nodes, "the declared chain from the tip does not reach every revision in this generation"), nil
		}
	}
	evidence := evidenceChain
	if len(nodes) == 1 && len(edges) == 0 {
		evidence = evidenceSole
	}
	return Head{EventID: tip, RevisionHash: hashOf[tip], Evidence: evidence, Competitors: []string{}}, nil
}
