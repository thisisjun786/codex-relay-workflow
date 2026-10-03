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
			return nil, err
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
	// One statement reads the generation's reviewable receipts, the suppressed ones with the live,
	// so what a suppressed receipt declared and which receipts are live are one moment's answer.
	rows, err := s.All(ctx, "SELECT e.event_id, e.revision_hash, l.supersedes_hash,"+
		"       e.suppressed_reason IS NOT NULL AS suppressed"+
		"  FROM events e"+
		"  LEFT JOIN revision_lineage l ON l.event_id = e.event_id"+
		" WHERE e.relationship_id = ? AND e.execution_generation = ?"+
		"   AND e.outcome = ?"+
		" ORDER BY e.event_id", rid, generation, reviewable)
	if err != nil {
		return Head{}, err
	}
	var nodes []string
	hashOf, declaredOf := map[string]string{}, map[string]string{}
	suppressed := map[string][]string{}
	for _, row := range rows {
		if flag, _ := row.Get("suppressed").(int64); flag != 0 {
			hash := colString(row, "revision_hash")
			suppressed[hash] = append(suppressed[hash], colString(row, "supersedes_hash"))
			continue
		}
		id := colString(row, "event_id")
		if _, seen := hashOf[id]; !seen {
			nodes = append(nodes, id)
		}
		hashOf[id] = colString(row, "revision_hash")
		declaredOf[id] = colString(row, "supersedes_hash")
	}
	if len(nodes) == 0 {
		return Head{Evidence: evidenceNone, Competitors: []string{}, Detail: "no reviewable revision in this generation"}, nil
	}
	byHash := map[string][]string{}
	for _, id := range nodes {
		byHash[hashOf[id]] = append(byHash[hashOf[id]], id)
	}
	anchors, err := requestedPredecessors(ctx, s, rid, generation)
	if err != nil {
		return Head{}, err
	}
	if len(suppressed) > 0 {
		declared := make([]string, 0, len(nodes))
		for _, id := range nodes {
			declared = append(declared, declaredOf[id])
		}
		through := ReadThrough(declared, func(hash string) int { return len(byHash[hash]) + len(anchors[hash]) }, suppressed)
		for _, id := range nodes {
			if effective, ok := through[declaredOf[id]]; ok {
				declaredOf[id] = effective
			}
		}
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

// ReadThrough answers, for each revision hash in named that no live revision of the generation
// and no requested correction predecessor holds (held counts those), what a revision naming it is
// read as naming instead. declaredBy lists, for each revision hash, the predecessor hash of every
// suppressed receipt of the generation holding it ("" for none).
//
// A suppressed receipt (the turn that staged it ended failed or interrupted, so the relay does
// not promote it) is not a revision, and a child that restarted cannot know its earlier receipt
// was one; so the naming is read through it, to the predecessor that receipt itself declared. ""
// is no predecessor: the revision stands alone. Through two or more suppressed receipts in a row
// is read the same way.
//
// Only a hash held by exactly one suppressed receipt is read through, and only to an end that is
// "" or a hash held by exactly one live revision or requested predecessor. A hash nothing holds
// (never emitted, or another generation's: declaredBy is this generation's), a hash several
// suppressed receipts hold, and a chain of suppressed receipts that ends at a hash nothing
// resolves are left out of the answer: the naming stays what it was, and the head reads it as it
// always has. Each hash is followed once, however many revisions name it.
func ReadThrough(named []string, held func(hash string) int, declaredBy map[string][]string) map[string]string {
	if len(declaredBy) == 0 {
		return nil
	}
	memo := map[string]readThroughEnd{}
	through := map[string]string{}
	for _, hash := range named {
		if hash == "" || held(hash) != 0 {
			continue
		}
		if end := readThrough(hash, held, declaredBy, memo); end.ok {
			through[hash] = end.effective
		}
	}
	return through
}

// readThroughEnd is where a naming ends once read through: ok is false when it cannot be.
type readThroughEnd struct {
	effective string
	ok        bool
}

// readThrough follows start through the suppressed receipts that hold it and remembers the end for
// every hash it passed, which is the end of any walk that reaches one of them.
func readThrough(start string, held func(hash string) int, declaredBy map[string][]string, memo map[string]readThroughEnd) readThroughEnd {
	var path []string
	var onPath map[string]bool
	var end readThroughEnd
	for hash := start; ; {
		if known, ok := memo[hash]; ok {
			end = known
			break
		}
		holders := declaredBy[hash]
		if held(hash) != 0 || len(holders) != 1 || onPath[hash] {
			break
		}
		path = append(path, hash)
		if onPath == nil {
			onPath = map[string]bool{}
		}
		onPath[hash] = true
		hash = holders[0]
		if hash == "" || held(hash) == 1 {
			end = readThroughEnd{hash, true}
			break
		}
	}
	for _, passed := range path {
		memo[passed] = end
	}
	return end
}
