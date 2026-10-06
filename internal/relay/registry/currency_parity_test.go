package registry_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-827: the head judgment was ported twice (registry.HeadRevision and delivery.HeadRevisionFrom)
// and had already drifted, so this file pins them together. It is written against the tree BEFORE
// the move and must pass there; after the move, delivery.HeadRevisionFrom is an adapter over
// registry.HeadRevisionFrom and the same answers must still hold.
//
// The two entry points take different arguments (a *store.Store and a store.Querier) and return
// different types (a contract.OrderedObject and a Head), so the comparison renders registry's Head
// with renderHead - the five keys in the order and spelling both readers print - and compares that
// with the dict delivery prints, byte for byte. After the move delivery's adapter returns exactly
// registry's rendering, so this equality also pins the shared renderer.

// renderHead is registry.Head as the dict the relay prints: eventId, revisionHash, evidence,
// competitors, detail, with "" read as null for the two ids and the competitors as a JSON array.
func renderHead(h registry.Head) contract.OrderedObject {
	competitors := make([]any, len(h.Competitors))
	for i, c := range h.Competitors {
		competitors[i] = c
	}
	var eventID, revisionHash any
	if h.EventID != "" {
		eventID = h.EventID
	}
	if h.RevisionHash != "" {
		revisionHash = h.RevisionHash
	}
	return contract.OrderedObject{
		{Key: "eventId", Value: eventID},
		{Key: "revisionHash", Value: revisionHash},
		{Key: "evidence", Value: h.Evidence},
		{Key: "competitors", Value: competitors},
		{Key: "detail", Value: h.Detail},
	}
}

// parityEvent is one event of a shape. Zero values mean generation 1, outcome ready_for_review,
// stage final, not suppressed.
type parityEvent struct {
	id, hash, declared string
	generation         int64
	outcome            string
	stage              string
	suppressed         bool
}

// parityCase is a shape and the answer both judgments must give for it.
type parityCase struct {
	name string
	// current is the generation the relationship stands on; zero means 1. judge is the generation
	// the head is read for; zero means current.
	current, judge int64
	events         []parityEvent
	// requested seeds the correction of "p1" (generation 1) that opened generation 2: the request
	// event and the ruling that name it.
	requested bool
	evidence  string
	head      string
}

// parityStore is one store holding many shapes, each under a relationship and event ids of its own.
type parityStore struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
}

func newParityStore(t *testing.T) *parityStore {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &parityStore{t: t, ctx: ctx, store: s}
}

func (g *parityStore) exec(query string, args ...any) {
	g.t.Helper()
	if _, err := g.store.Querier(g.ctx).ExecContext(g.ctx, query, args...); err != nil {
		g.t.Fatalf("%v\n%s", err, query)
	}
}

// seeded is a shape as stored: the relationship it lives under and the prefix its event ids carry.
type seeded struct {
	parityCase
	rid, prefix string
	judged      int64
}

// seed stores case number n.
func (g *parityStore) seed(n int, c parityCase) seeded {
	t := g.t
	t.Helper()
	rid, prefix := fmt.Sprintf("rel-parity-%02d", n), fmt.Sprintf("c%02d-", n)
	current := max(c.current, 1)
	g.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?, ?, 'active', 'parent-parity', 'host-1', 'child-parity', 'host-1', ?, '[]', '[]', 'x', 'x')", rid, "PARITY-"+prefix, current)
	request, ruling := "", "vt"
	if c.requested {
		var err error
		if request, err = store.RevisionRequestEventID(rid, prefix+"p1", ruling); err != nil {
			t.Fatal(err)
		}
	}
	for generation := int64(1); generation <= current; generation++ {
		reason, dispatch := "initial_assignment", fmt.Sprintf("dispatch-%d", generation)
		if generation > 1 {
			reason = "needs_changes_revision"
		}
		if generation == 2 && c.requested {
			dispatch = "revision-" + request
		}
		g.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?, ?, ?, 'bound', 'anchor', ?, 'x', 'x')", rid, generation, dispatch, reason)
	}
	events := c.events
	if c.requested {
		events = append(events,
			parityEvent{id: "p1", hash: "hp", generation: 1},
			parityEvent{id: "request", hash: "-", generation: 2, outcome: "revision_request"})
		g.exec("INSERT INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at) VALUES (?, '{}', 'needs_changes', 2, ?, 'x')", prefix+"p1", ruling)
	}
	for _, e := range events {
		id := prefix + e.id
		if e.id == "request" {
			id = request
		}
		generation, outcome, stage, producer := max(e.generation, 1), e.outcome, e.stage, "child"
		if outcome == "" {
			outcome = "ready_for_review"
		}
		if outcome == "revision_request" {
			producer = "relay"
		}
		if stage == "" {
			stage = "final"
		}
		var suppressed any
		if e.suppressed {
			suppressed = "superseded"
		}
		g.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, suppressed_reason, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, 'child-parity', ?, 'completed', '{}', ?, ?, 'x', 'x')", id, rid, generation, e.hash, outcome, producer, "turn-"+id, stage, suppressed)
		if outcome == "ready_for_review" {
			var declared any
			declaredBy := "undeclared"
			if e.declared != "" {
				declared, declaredBy = e.declared, "child_declared"
			}
			g.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash, declared_by, recorded_at) VALUES (?, ?, ?, ?, ?, ?, 'x')", rid, generation, id, e.hash, declared, declaredBy)
		}
	}
	judged := c.judge
	if judged == 0 {
		judged = current
	}
	return seeded{parityCase: c, rid: rid, prefix: prefix, judged: judged}
}

// compare reads the same generation through both entry points and fails unless they print the same
// bytes. It returns registry's answer so a caller can also check the shape's expected evidence.
func (g *parityStore) compare(t *testing.T, s seeded, ctx context.Context) registry.Head {
	t.Helper()
	view, err := delivery.HeadRevisionFrom(ctx, g.store.Q(ctx), s.rid, s.judged)
	if err != nil {
		t.Fatal(err)
	}
	head, err := registry.HeadRevision(ctx, g.store, s.rid, s.judged)
	if err != nil {
		t.Fatal(err)
	}
	got, want := pyjson.Dumps(view, pyjson.Options{}), pyjson.Dumps(renderHead(head), pyjson.Options{})
	if got != want {
		t.Fatalf("the two head judgments differ:\nview %s\nhead %s", got, want)
	}
	return head
}

func pv(id, hash, declared string) parityEvent {
	return parityEvent{id: id, hash: hash, declared: declared}
}

// parityCases are the shapes the issue names: an empty generation, one revision, a chain, a fork
// with one predecessor named twice, two roots, a cycle, an unknown predecessor, a naming read
// through a suppressed receipt, and a correction anchor. The issue also names a disconnected chain;
// that reading is unreachable and is pinned by TestNoSeededShapeReadsDisconnected below.
func parityCases() []parityCase {
	suppressed := pv("cut", "hcut", "h1")
	suppressed.suppressed = true
	execOnly := parityEvent{id: "f1", hash: "h9", outcome: "failed"}
	return []parityCase{
		{name: "empty generation", events: []parityEvent{execOnly}, evidence: "no_revision"},
		{name: "one revision", events: []parityEvent{pv("e1", "h1", "")}, evidence: "sole_revision", head: "e1"},
		{name: "a chain", events: []parityEvent{pv("e1", "h1", ""), pv("e2", "h2", "h1"), pv("e3", "h3", "h2")}, evidence: "declared_chain", head: "e3"},
		{name: "a fork with one predecessor named twice", events: []parityEvent{pv("e1", "h1", ""), pv("e2", "h2", "h1"), pv("e3", "h3", "h1")}, evidence: "fork"},
		{name: "two roots", events: []parityEvent{pv("e1", "h1", ""), pv("e2", "h2", "")}, evidence: "fork"},
		{name: "a cycle", events: []parityEvent{pv("e1", "h1", "h2"), pv("e2", "h2", "h1")}, evidence: "cycle"},
		{name: "an unknown predecessor", events: []parityEvent{pv("e1", "h1", ""), pv("e2", "h2", "nope")}, evidence: "unknown_predecessor"},
		{name: "a naming read through a suppressed receipt", events: []parityEvent{pv("e1", "h1", ""), suppressed, pv("e2", "h2", "hcut")}, evidence: "declared_chain", head: "e2"},
		{name: "a correction anchor", current: 2, requested: true, events: []parityEvent{{id: "r1", hash: "hr", declared: "hp", generation: 2}}, evidence: "declared_chain", head: "r1"},
	}
}

// TestTheTwoHeadJudgmentsAgreeOnTheIssuesShapes is the parity proof: the dict
// delivery.HeadRevisionFrom prints and registry.HeadRevision's answer rendered by renderHead are
// compared byte for byte, and the evidence word and head the shape must produce are checked.
func TestTheTwoHeadJudgmentsAgreeOnTheIssuesShapes(t *testing.T) {
	t.Parallel()
	g := newParityStore(t)
	for n, c := range parityCases() {
		seeded := g.seed(n, c)
		t.Run(c.name, func(t *testing.T) {
			head := g.compare(t, seeded, g.ctx)
			if head.Evidence != c.evidence {
				t.Fatalf("evidence %q, the shape expects %q", head.Evidence, c.evidence)
			}
			if c.head != "" && head.EventID != seeded.prefix+c.head {
				t.Fatalf("head %q, the shape expects %q", head.EventID, seeded.prefix+c.head)
			}
			if c.head == "" && head.EventID != "" {
				t.Fatalf("head %q, the shape expects none", head.EventID)
			}
		})
	}
}

// The two readers must honour the same connection inside one transaction: a read on the
// transaction's own connection sees the transaction's own uncommitted writes, and both entry points
// route through the ctx-aware querier (store/records.go). A reader that used the pool instead would
// not see the revision written inside the transaction at all, so the new revision is written there
// and both readers must report it as the head.
func TestTheTwoHeadJudgmentsAgreeInsideOneTransaction(t *testing.T) {
	t.Parallel()
	g := newParityStore(t)
	seeded := g.seed(0, parityCase{name: "a chain", events: []parityEvent{pv("e1", "h1", ""), pv("e2", "h2", "h1")}, evidence: "declared_chain", head: "e2"})
	err := g.store.Transaction(g.ctx, func(ctx context.Context, _ *sql.Conn) error {
		// A revision and its lineage row, written inside the transaction and not yet committed.
		third := seeded.prefix + "e3"
		if _, err := g.store.Querier(ctx).ExecContext(ctx, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?, ?, 1, 'h3', 'ready_for_review', 'child', 'child-parity', ?, 'completed', '{}', 'final', 'x', 'x')", third, seeded.rid, "turn-"+third); err != nil {
			return err
		}
		if _, err := g.store.Querier(ctx).ExecContext(ctx, "INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash, declared_by, recorded_at) VALUES (?, 1, ?, 'h3', 'h2', 'child_declared', 'x')", seeded.rid, third); err != nil {
			return err
		}
		head := g.compare(t, seeded, ctx)
		if head.EventID != third {
			t.Fatalf("the head is %q, and the revision written in the transaction is %q: a reader did not see the transaction's own writes", head.EventID, third)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The disconnected branch cannot be reached through the store: a revision that an edge targets but
// the single tip's walk does not cover would have to lead to a second tip or to a cycle, and both
// are answered first (a second tip as fork, a cycle as cycle). This enumerates every small shape
// through the store and asserts neither reader ever reads disconnected, which is why the parity
// test above covers nine seeded shapes rather than ten.
func TestNoSeededShapeReadsDisconnected(t *testing.T) {
	t.Parallel()
	g := newParityStore(t)
	hashes := []string{"h1", "h2", "h3"}
	choices := append([]string{""}, hashes...)
	n := 0
	for size := 1; size <= 3; size++ {
		declared := make([]string, size)
		var walk func(i int)
		walk = func(i int) {
			if i == size {
				n++
				events := make([]parityEvent, size)
				for k := range events {
					events[k] = pv(fmt.Sprintf("e%d", k+1), hashes[k], declared[k])
				}
				seeded := g.seed(n, parityCase{name: fmt.Sprintf("graph %d", n), events: events})
				head := g.compare(t, seeded, g.ctx)
				if head.Evidence == "disconnected" {
					t.Fatalf("a disconnected reading was reachable after all: declared %v", declared)
				}
				return
			}
			for _, choice := range choices {
				declared[i] = choice
				walk(i + 1)
			}
		}
		walk(0)
	}
	if n < 80 {
		t.Fatalf("the enumeration covered only %d shapes", n)
	}
}
