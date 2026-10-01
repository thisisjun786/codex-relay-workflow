package daemon

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// A turn of an active relationship is pending while the relay still has something to learn by reading it: a
// staged claim of the relationship's child waits on it, or the relationship has no settlement for it (worth).
// A relationship with no pending turn has nothing to observe. The census finds every pending turn at once, from
// store rows only, so a pass loads and reads only what needs it.

// How a turn came to be pending, in the order that breaks a tie between turns that have waited equally long.
const (
	currentAnchor = iota
	stagedClaim
	admittedTurn
	otherAnchor
)

// pendingTurn is a turn still worth reading.
type pendingTurn struct {
	id     string
	staged bool // a staged claim waits on it (class A); every other pending turn is class B
	how    int
	pos    int
	// since is when the turn became pending (the earliest of its reasons), attempt the latest read of it in any
	// generation: the read loop stores a turn under the relationship's current generation, so an older
	// generation's anchor is stored under that one.
	since, attempt string
}

// waiting is the stamp the turn has waited since: when it became pending or was last read, whichever is later.
// Stamps are the relay's own ISO microsecond strings, which order as text.
func (t *pendingTurn) waiting() string { return max(t.since, t.attempt) }

// pending is what the store says one active relationship still has to be observed for.
type pending struct {
	id string
	// class holds the pending turns of each class (0: other, 1: staged), longest-waiting first, and rank the
	// wait of the first of them: a relationship waits, in a class, as long as the turn it deals next.
	class [2][]*pendingTurn
	rank  [2]string
	turns map[string]*pendingTurn
}

// The three reasons a turn is pending, and the reads already made. The latest read is the latest valid one: a stamp
// from before a clock set-back must not hide a valid stamp written under another generation.
const (
	stagedTurns = `SELECT r.relationship_id AS rid, e.turn_id AS turn, e.first_seen_at AS since, 0 AS current
FROM relationships r JOIN events e ON e.relationship_id=r.relationship_id AND e.turn_thread_id=r.child_task_id
WHERE r.status='active' AND r.superseded_by IS NULL AND e.stage='staged'
ORDER BY r.relationship_id, e.first_seen_at, e.event_id`

	unsettledAnchors = `SELECT r.relationship_id AS rid, g.dispatch_turn_id AS turn, COALESCE(g.bound_at,g.opened_at) AS since,
  g.execution_generation=r.execution_generation AS current
FROM relationships r JOIN generations g ON g.relationship_id=r.relationship_id
WHERE r.status='active' AND r.superseded_by IS NULL AND g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id<>''
  AND NOT EXISTS (SELECT 1 FROM assignment_settlements s WHERE s.relationship_id=r.relationship_id AND s.thread_id=r.child_task_id AND s.turn_id=g.dispatch_turn_id)
ORDER BY r.relationship_id, g.execution_generation`

	// What makes a generation_turns row an admission is the store's own predicate (see laterReceiptQuery).
	unsettledAdmissions = `SELECT r.relationship_id AS rid, t.turn_id AS turn, t.admitted_at AS since, 0 AS current
FROM relationships r
JOIN generation_turns t ON t.relationship_id=r.relationship_id
JOIN generations g ON g.relationship_id=t.relationship_id AND g.execution_generation=t.execution_generation
WHERE r.status='active' AND r.superseded_by IS NULL AND g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id<>''
  AND t.evidence=('explicit_admission_bound:' || g.dispatch_turn_id)
  AND NOT EXISTS (SELECT 1 FROM assignment_settlements s WHERE s.relationship_id=r.relationship_id AND s.thread_id=r.child_task_id AND s.turn_id=t.turn_id)
ORDER BY r.relationship_id, t.rowid`

	// The reads of one relationship, by its primary key prefix: only a relationship with a pending turn is asked. One row
	// per generation a turn was read under; each is judged on its own (see past) before the latest is taken.
	attemptsOfARelationship = `SELECT turn_id AS turn, last_attempt_at AS attempt FROM poll_observations WHERE relationship_id=?`
)

// census returns the active relationships that have a pending turn, in relationship id order.
func (d *Daemon) census(ctx context.Context) ([]*pending, error) {
	byID := map[string]*pending{}
	for _, q := range []struct {
		query  string
		staged bool
		how    int // for a turn that is not the current anchor
	}{{stagedTurns, true, stagedClaim}, {unsettledAnchors, false, otherAnchor}, {unsettledAdmissions, false, admittedTurn}} {
		rows, err := d.Store.All(ctx, q.query)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			id, turn, since := row.Get("rid").(string), row.Get("turn").(string), row.Get("since").(string)
			how := q.how
			if row.Get("current").(int64) != 0 {
				how = currentAnchor
			}
			p := byID[id]
			if p == nil {
				p = &pending{id: id, turns: map[string]*pendingTurn{}}
				byID[id] = p
			}
			t := p.turns[turn]
			if t == nil {
				t = &pendingTurn{id: turn, how: how, pos: len(p.turns), since: since}
				p.turns[turn] = t
			}
			t.staged = t.staged || q.staged
			t.how = min(t.how, how)
			t.since = min(t.since, since)
		}
	}
	if len(byID) == 0 {
		return nil, nil
	}
	// A stamp later than the clock now was written before the clock was set back, so it is older than anything the
	// clock stamps now. It counts as the oldest stamp there is; reading the turn replaces it with a valid one, so
	// turns stamped in a clock's future neither rank behind the turns just read nor hold the same front for ever.
	now := delivery.ISOOf(d.Clock.Now())
	past := func(stamp string) string {
		if stamp > now {
			return ""
		}
		return stamp
	}
	out := make([]*pending, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		p := byID[id]
		attempts, err := d.Store.All(ctx, attemptsOfARelationship, id)
		if err != nil {
			return nil, err
		}
		for _, row := range attempts {
			if t := p.turns[row.Get("turn").(string)]; t != nil {
				attempt, _ := row.Get("attempt").(string)
				t.attempt = max(t.attempt, past(attempt))
			}
		}
		for _, t := range p.turns {
			t.since = past(t.since)
			class := 0
			if t.staged {
				class = 1
			}
			p.class[class] = append(p.class[class], t)
		}
		for class, turns := range p.class {
			slices.SortFunc(turns, func(a, b *pendingTurn) int {
				return cmp.Or(cmp.Compare(a.waiting(), b.waiting()), cmp.Compare(a.how, b.how), cmp.Compare(a.pos, b.pos))
			})
			if len(turns) > 0 {
				p.rank[class] = turns[0].waiting()
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// read is one read of the host the pass may make: a turn of a relationship, dealt for a class.
type read struct {
	p     *pending
	t     *pendingTurn
	class int
}

// deal lists the reads of one class: the relationships with a pending turn of it, longest-waiting first, deal one
// turn each, their longest-waiting one, round after round, so the budget is shared by relationships and one with
// many pending turns cannot dilute another.
func deal(work []*pending, class int) []read {
	ranked := slices.DeleteFunc(slices.Clone(work), func(p *pending) bool { return len(p.class[class]) == 0 })
	slices.SortStableFunc(ranked, func(a, b *pending) int {
		return cmp.Or(cmp.Compare(a.rank[class], b.rank[class]), cmp.Compare(a.id, b.id))
	})
	var out []read
	for round := 0; ; round++ {
		more := false
		for _, p := range ranked {
			if turns := p.class[class]; round < len(turns) {
				out = append(out, read{p, turns[round], class})
				more = true
			}
		}
		if !more {
			return out
		}
	}
}
