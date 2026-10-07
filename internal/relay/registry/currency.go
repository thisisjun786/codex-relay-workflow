package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// currency.py's head_revision, as the assignment view reads it: which revision a generation
// currently stands on, decided from declared lineage, never from arrival order.
//
// CRW-827: this is the one judgment. delivery.HeadRevision, delivery.HeadRevisionFrom,
// delivery.Currency and delivery.RefuseNewFork are adapters over it, so the assignment view
// (dag-ready, assignment-show) and the verdict path (dag-accept) cannot read different heads.

// Lineage evidence words (currency.py). One declaration, because both readers print them.
const (
	EvidenceSole               = "sole_revision"
	EvidenceChain              = "declared_chain"
	EvidenceNone               = "no_revision"
	EvidenceFork               = "fork"
	EvidenceCycle              = "cycle"
	EvidenceUnknownPredecessor = "unknown_predecessor"
	EvidenceDisconnected       = "disconnected"
	reviewable                 = "ready_for_review"
)

// AmbiguousEvidence is currency.AMBIGUOUS: the evidence kinds that name no single head.
var AmbiguousEvidence = []string{EvidenceFork, EvidenceCycle, EvidenceUnknownPredecessor, EvidenceDisconnected}

// Head is head_revision's answer; EventID "" is None.
type Head struct {
	EventID, RevisionHash, Evidence, Detail string
	Competitors                             []string
}

// Ambiguous reports whether the head's evidence is one of currency.AMBIGUOUS.
func (h Head) Ambiguous() bool { return slices.Contains(AmbiguousEvidence, h.Evidence) }

// record is head_revision's answer as the dict the relay prints: the same five keys in the same
// order, with "" for None.
func (h Head) record() contract.OrderedObject {
	competitors := anyStrings(h.Competitors)
	return contract.OrderedObject{{Key: "eventId", Value: nullText(h.EventID)}, {Key: "revisionHash", Value: nullText(h.RevisionHash)},
		{Key: "evidence", Value: h.Evidence}, {Key: "competitors", Value: competitors}, {Key: "detail", Value: h.Detail}}
}

// Record is record() for callers outside this package: delivery's adapters print the one judgment's
// answer, and the Obj is produced here once.
func (h Head) Record() contract.OrderedObject { return h.record() }

func ambiguousHead(evidence string, nodes []string, detail string) Head {
	sorted := slices.Clone(nodes)
	slices.Sort(sorted)
	return Head{Evidence: evidence, Competitors: sorted, Detail: detail}
}

// colString reads a column as store.Row.Text does: a TEXT value's string, a BLOB value's bytes as
// text, and "" for NULL, a number or an absent column. CRW-928: the correction anchor is read this
// way, so a verdict turn id the store holds as a BLOB reads as its bytes, exactly as the delivery
// reader read it before CRW-827 moved the judgment into this package. One rule, so no read in this
// package can drop a value another read keeps.
func colString(row store.Row, name string) string { return row.Text(name) }

// textOf reads a column as Row.Text does: text, or "" for NULL and for any other type.
func textOf(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// allRows reads every row of a statement through the caller's querier, so a read inside a
// transaction body sees that transaction's own writes.
func allRows(ctx context.Context, q store.Querier, query string, args ...any) (_ []store.Row, err error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []store.Row
	for rows.Next() {
		values := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := make(store.Row, len(names))
		for i, name := range names {
			row[i] = store.Column{Name: name, Value: values[i]}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Revision is one reviewable revision of a generation as the head judgment reads it: its event, its
// revision hash and the predecessor hash it declares ("" for none).
type Revision struct{ ID, Hash, Declared string }

// HeadRevisionSQL reads the reviewable receipts of one generation, each with the predecessor it
// declares and whether the relay holds it as suppressed, in one statement: a suppressed receipt is
// no revision, but what it declared is read when a revision names it (CRW-470), and one statement
// is one moment's answer for both. The lineage row is found by its primary key (relationship,
// generation, event): joined on event_id alone, SQLite scans the whole of revision_lineage once for
// every event row, so one judgment cost the revisions of the generation times the lineage rows of
// the store (CRW-416). The product stores an event and its lineage row together under one
// relationship and generation, so this reads the rows the event_id join read.
const HeadRevisionSQL = "SELECT e.event_id, e.revision_hash, l.supersedes_hash, e.suppressed_reason IS NOT NULL FROM events e LEFT JOIN revision_lineage l ON l.relationship_id = e.relationship_id AND l.execution_generation = e.execution_generation AND l.event_id = e.event_id WHERE e.relationship_id = ? AND e.execution_generation = ? AND e.outcome = ? ORDER BY e.event_id"

// ReadRevisions is every reviewable revision of the generation in event id order, and the suppressed
// receipts apart: the predecessors they declared by revision hash. Reading the revisions all is what
// a head is: a fork, a cycle or an unknown predecessor anywhere among them makes the whole
// generation ambiguous and names every revision as a competitor.
func ReadRevisions(ctx context.Context, q store.Querier, rid string, generation int64) (_ []Revision, suppressed map[string][]string, err error) {
	rows, err := q.QueryContext(ctx, HeadRevisionSQL, rid, generation, reviewable)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var out []Revision
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
		out = append(out, Revision{ID: textOf(id), Hash: textOf(hash), Declared: textOf(declared)})
	}
	return out, suppressed, rows.Err()
}

// RequestedPredecessors is currency._requested_predecessors: only the result whose ruling opened
// this correction is an external root. Its columns are read as Row.Text does (CRW-928), so an
// anchor the store holds with a BLOB verdict turn id reads as it did before CRW-827 moved the
// judgment here.
//
// The second result names the eligible rulings whose request identity cannot be computed. A row
// whose request identity cannot be read used to be skipped, and the comment here claimed the naming
// then resolves to nothing and the generation reads unknown_predecessor; that is not what happened,
// because a revision that declares no predecessor reads sole_revision whether or not an anchor was
// dropped. So the row is not dropped: the caller reads a generation holding one as
// unknown_predecessor and names the ruling it could not read. It is empty for a store that holds
// only rulings it can read, which is every store the product writes.
func RequestedPredecessors(ctx context.Context, q store.Querier, rid string, generation int64) (map[string][]string, []string, error) {
	// the generation a ruling's correction follows: the one before it, or the nearest one that was not withdrawn (CRW-446)
	before, err := store.LiveGenerationBefore(ctx, q, rid, generation)
	if err != nil {
		return nil, nil, err
	}
	rows, err := allRows(ctx, q, "SELECT p.event_id, p.revision_hash, v.verdict_turn_id, r.event_id AS request_id,"+
		" g.dispatch_request_id"+
		" FROM generations g"+
		" JOIN verdicts v ON v.next_generation = g.execution_generation"+
		" JOIN events p ON p.event_id = v.event_id AND p.relationship_id = g.relationship_id"+
		" JOIN events r ON r.relationship_id = g.relationship_id"+
		" AND r.execution_generation = g.execution_generation"+
		" WHERE g.relationship_id = ? AND g.execution_generation = ?"+
		" AND g.reason = 'needs_changes_revision' AND v.verdict = 'needs_changes'"+
		" AND p.execution_generation = ?"+
		" AND p.outcome = ? AND p.stage = 'final' AND p.suppressed_reason IS NULL"+
		" AND r.outcome = 'revision_request' AND r.producer = 'relay'"+
		" AND r.stage = 'final' AND r.suppressed_reason IS NULL", rid, generation, before, reviewable)
	if err != nil {
		return nil, nil, err
	}
	anchors := map[string][]string{}
	var unreadable []string
	for _, row := range rows {
		event := colString(row, "event_id")
		request, err := store.RevisionRequestEventID(rid, event, colString(row, "verdict_turn_id"))
		if err != nil {
			unreadable = append(unreadable, event)
			continue
		}
		if colString(row, "request_id") == request && colString(row, "dispatch_request_id") == "revision-"+request {
			hash := colString(row, "revision_hash")
			anchors[hash] = append(anchors[hash], event)
		}
	}
	// The statement has no ORDER BY, and every identifier list this file prints is sorted first, so
	// the detail a caller prints is the same on every run.
	slices.Sort(unreadable)
	return anchors, unreadable, nil
}

// ReadThroughSuppressed returns the revisions with each declared predecessor that names a suppressed
// receipt of the generation read as naming what that receipt replaced (CRW-470; the rule is
// ReadThrough's). A suppressed receipt is not a revision, and a child that restarted cannot know its
// earlier receipt was one. The revisions come back as they were when no receipt is suppressed, which
// is nearly every judgment; otherwise the answer is a copy, so a caller that judges twice over one
// reading (RefuseNewFork) keeps its first reading.
func ReadThroughSuppressed(revisions []Revision, suppressed map[string][]string, anchors map[string][]string) []Revision {
	if len(suppressed) == 0 {
		return revisions
	}
	held := make(map[string]int, len(revisions))
	seen := make(map[string]bool, len(revisions))
	named := make([]string, 0, len(revisions))
	// An event id is the events primary key, so each revision is listed once (as in JudgeHead).
	for _, r := range revisions {
		if !seen[r.ID] {
			seen[r.ID] = true
			held[r.Hash]++
		}
		named = append(named, r.Declared)
	}
	through := ReadThrough(named, func(hash string) int { return held[hash] + len(anchors[hash]) }, suppressed)
	if len(through) == 0 {
		return revisions
	}
	out := slices.Clone(revisions)
	for i := range out {
		if effective, ok := through[out[i].Declared]; ok {
			out[i].Declared = effective
		}
	}
	return out
}

// JudgeHead is currency.head_revision over the generation's reviewable revisions (at least one, in
// event id order) and its requested correction predecessors.
func JudgeHead(revisions []Revision, anchors map[string][]string) Head {
	nodes := make([]string, 0, len(revisions))
	hashOf := make(map[string]string, len(revisions))
	declaredOf := make(map[string]string, len(revisions))
	for _, r := range revisions {
		// An event id is the events primary key and the lineage row is found by its own key, so it
		// is listed once; were it repeated, it would keep its first place and its last values.
		if _, seen := hashOf[r.ID]; !seen {
			nodes = append(nodes, r.ID)
		}
		hashOf[r.ID] = r.Hash
		declaredOf[r.ID] = r.Declared
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
		return ambiguousHead(EvidenceUnknownPredecessor, nodes, "a declared predecessor is neither a unique revision of this generation nor its requested correction predecessor: "+strings.Join(unresolved, ", "))
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
				return ambiguousHead(EvidenceCycle, nodes, fmt.Sprintf("the declared chain from %s returns to %s", start, current))
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
		return ambiguousHead(EvidenceFork, nodes, "more than one revision declares the same predecessor: "+strings.Join(forked, ", "))
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
		return ambiguousHead(EvidenceFork, nodes, fmt.Sprintf("%d revisions in this generation are unsuperseded, so none of them is the head; a revision that replaces another says so when it is emitted", len(tips)))
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
			return ambiguousHead(EvidenceDisconnected, nodes, "the declared chain from the tip does not reach every revision in this generation")
		}
	}
	evidence := EvidenceChain
	if len(nodes) == 1 && len(edges) == 0 {
		evidence = EvidenceSole
	}
	return Head{EventID: tip, RevisionHash: hashOf[tip], Evidence: evidence, Competitors: []string{}}
}

// unreadableAnchorHead is what a generation reads when an eligible ruling's request identity could
// not be computed: unknown_predecessor (the existing name, no new evidence word), the generation's
// revisions as competitors, and a detail naming each ruling that could not be read. A reader that
// has seen the store hold an eligible ruling it cannot identify does not answer sole_revision
// (CRW-928), whether or not the revision declares a predecessor.
func unreadableAnchorHead(revisions []Revision, unreadable []string) Head {
	nodes := make([]string, 0, len(revisions))
	seen := make(map[string]bool, len(revisions))
	for _, r := range revisions {
		if !seen[r.ID] {
			seen[r.ID] = true
			nodes = append(nodes, r.ID)
		}
	}
	return ambiguousHead(EvidenceUnknownPredecessor, nodes,
		"an eligible needs_changes ruling whose correction anchor cannot be identified: "+strings.Join(unreadable, ", "))
}

// HeadRevisionFrom is currency.head_revision, read through the caller's snapshot, including a
// read-only guard connection.
func HeadRevisionFrom(ctx context.Context, q store.Querier, rid string, generation int64) (Head, error) {
	revisions, suppressed, err := ReadRevisions(ctx, q, rid, generation)
	if err != nil {
		return Head{}, err
	}
	if len(revisions) == 0 {
		return Head{Evidence: EvidenceNone, Competitors: []string{}, Detail: "no reviewable revision in this generation"}, nil
	}
	anchors, unreadable, err := RequestedPredecessors(ctx, q, rid, generation)
	if err != nil {
		return Head{}, err
	}
	if len(unreadable) > 0 {
		// The store holds an eligible ruling this read cannot identify, so no revision's naming can
		// be resolved against it: the generation reads unknown_predecessor rather than a confident
		// single head (CRW-928).
		return unreadableAnchorHead(revisions, unreadable), nil
	}
	return JudgeHead(ReadThroughSuppressed(revisions, suppressed, anchors), anchors), nil
}

// HeadRevision is currency.head_revision on a store: the entry the assignment view and the fault
// sweep use.
func HeadRevision(ctx context.Context, s *store.Store, rid string, generation int64) (Head, error) {
	return HeadRevisionFrom(ctx, s.Querier(ctx), rid, generation)
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
