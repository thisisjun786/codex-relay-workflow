package registry_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-928: the correction anchor is read as store.Row.Text does, and an eligible ruling whose
// request identity cannot be computed fails closed as unknown_predecessor instead of being skipped.
//
// Both shapes are corrupt-store shapes: the product writer stores a non-empty TEXT verdict turn id
// (delivery/ack.go refuses a ruling whose RevisionRequestEventID cannot be computed before it is
// written), so neither exists in a store the relay wrote. They are seeded here, on temporary stores
// only, to pin what the shared judgment does when it meets one.

// anchorCase is one correction-anchor shape: generation 2 was opened by a needs_changes ruling on
// generation 1's revision p1, and the ruling's verdict_turn_id is stored as turnSQL.
type anchorCase struct {
	name string
	// turnSQL is the SQL literal written into verdicts.verdict_turn_id: a TEXT literal, a BLOB
	// literal, or the empty TEXT literal.
	turnSQL string
	// declared is the predecessor hash the judged generation's revision r1 declares ("" for none).
	declared string
}

// anchorStore is one seeded correction: the relationship, the ruling, the revision_request it
// opened and the judged generation's single revision.
type anchorStore struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	rid   string
	// ruling is the ruling event id (the verdicts row's event_id, named in the fail-closed detail).
	ruling string
	// revision is the judged generation's revision event id.
	revision string
}

const (
	anchorRulingHash   = "hp"
	anchorRevisionHash = "hr"
)

func newAnchorStore(t *testing.T, c anchorCase) *anchorStore {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	a := &anchorStore{t: t, ctx: ctx, store: s, rid: "rel-anchor-928", ruling: "p1-928", revision: "r1-928"}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.Querier(ctx).ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	// The request id the ruling names, computed the way the reader computes it. It is empty when the
	// stored turn id cannot be read, and then no dispatch request id can match it either.
	// The request event and the dispatch request id the ruling names, computed the way the reader
	// computes them. When the stored turn id cannot be read they are a shape no ruling can name, so
	// the read cannot resolve the anchor however the row is stored.
	request, dispatch := "request-928", "revision-928"
	if turn, ok := anchorTurnText(c.turnSQL); ok {
		request, err = store.RevisionRequestEventID(a.rid, a.ruling, turn)
		if err != nil {
			t.Fatal(err)
		}
		dispatch = "revision-" + request
	}
	exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?, 'ANCHOR-928', 'active', 'parent-928', 'host-1', 'child-928', 'host-1', 2, '[]', '[]', 'x', 'x')", a.rid)
	exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?, 1, 'dispatch-1', 'bound', 'anchor', 'initial_assignment', 'x', 'x')", a.rid)
	exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?, 2, ?, 'bound', 'anchor', 'needs_changes_revision', 'x', 'x')", a.rid, dispatch)
	exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?, ?, 1, ?, 'ready_for_review', 'child', 'child-928', ?, 'completed', '{}', 'final', 'x', 'x')", a.ruling, a.rid, anchorRulingHash, "turn-"+a.ruling)
	exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?, ?, 2, '-', 'revision_request', 'relay', 'child-928', 'turn-request', 'completed', '{}', 'final', 'x', 'x')", request, a.rid)
	// The ruling row, with the verdict turn id written as the case's literal.
	exec("INSERT INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at) VALUES (?, '{}', 'needs_changes', 2, "+c.turnSQL+", 'x')", a.ruling)
	exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?, ?, 2, ?, 'ready_for_review', 'child', 'child-928', ?, 'completed', '{}', 'final', 'x', 'x')", a.revision, a.rid, anchorRevisionHash, "turn-"+a.revision)
	var declared any
	declaredBy := "undeclared"
	if c.declared != "" {
		declared, declaredBy = c.declared, "child_declared"
	}
	exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash, declared_by, recorded_at) VALUES (?, 2, ?, ?, ?, ?, 'x')", a.rid, a.revision, anchorRevisionHash, declared, declaredBy)
	return a
}

// anchorTurnText is the text a case's verdict_turn_id literal holds once the column is read as
// store.Row.Text does: X'7674' is the bytes "vt", 'vt' is "vt", and ” is empty.
func anchorTurnText(turnSQL string) (string, bool) {
	switch turnSQL {
	case "'vt'", "X'7674'":
		return "vt", true
	}
	return "", false
}

// head reads the judged generation through registry.HeadRevision.
func (a *anchorStore) head() registry.Head {
	a.t.Helper()
	head, err := registry.HeadRevision(a.ctx, a.store, a.rid, 2)
	if err != nil {
		a.t.Fatal(err)
	}
	return head
}

// The evaluation's BLOB case: a correction anchor whose verdict turn id the store holds as a BLOB
// reads as its bytes' text, so the revision that names it is the head, exactly as the delivery
// reader read it before CRW-827 moved the judgment into registry. The same fixture stored as TEXT
// reads the same way before and after.
func TestAnchorTextBLOBVerdictTurnIDReadsAsItsBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []anchorCase{
		{name: "stored as TEXT", turnSQL: "'vt'", declared: anchorRulingHash},
		{name: "stored as BLOB", turnSQL: "X'7674'", declared: anchorRulingHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAnchorStore(t, tc)
			head := a.head()
			if head.Evidence != registry.EvidenceChain {
				t.Fatalf("evidence %q, want %q (detail %q)", head.Evidence, registry.EvidenceChain, head.Detail)
			}
			if head.EventID != a.revision || head.RevisionHash != anchorRevisionHash {
				t.Fatalf("head %q/%q, want %q/%q", head.EventID, head.RevisionHash, a.revision, anchorRevisionHash)
			}
		})
	}
}

// The evaluation's empty-turn case: an eligible needs_changes ruling whose request identity cannot
// be computed makes the generation unknown_predecessor, naming that ruling, whether or not the
// revision declares a predecessor. Before this change the undeclared shape read sole_revision.
func TestAnchorTextUnreadableRulingFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []anchorCase{
		{name: "a revision declaring nothing", turnSQL: "''"},
		{name: "a revision declaring a predecessor", turnSQL: "''", declared: anchorRulingHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAnchorStore(t, tc)
			head := a.head()
			if head.Evidence != registry.EvidenceUnknownPredecessor {
				t.Fatalf("evidence %q, want %q", head.Evidence, registry.EvidenceUnknownPredecessor)
			}
			if !head.Ambiguous() {
				t.Fatalf("%q is not ambiguous", head.Evidence)
			}
			if !strings.Contains(head.Detail, a.ruling) {
				t.Fatalf("detail %q does not name the unreadable ruling %q", head.Detail, a.ruling)
			}
			if head.EventID != "" || head.RevisionHash != "" {
				t.Fatalf("an ambiguous answer names no head: %q/%q", head.EventID, head.RevisionHash)
			}
		})
	}
}

// The delivery entry is the same judgment (CRW-827), so it reads the BLOB anchor the same way.
func TestAnchorTextDeliveryReadsTheSameBLOBRuling(t *testing.T) {
	t.Parallel()
	a := newAnchorStore(t, anchorCase{name: "BLOB", turnSQL: "X'7674'", declared: anchorRulingHash})
	view, err := delivery.HeadRevisionFrom(a.ctx, a.store.Q(a.ctx), a.rid, 2)
	if err != nil {
		t.Fatal(err)
	}
	evidence, _ := view.Lookup("evidence")
	eventID, _ := view.Lookup("eventId")
	if evidence != registry.EvidenceChain || eventID != a.revision {
		t.Fatalf("delivery reads %v/%v, want %q/%q", evidence, eventID, registry.EvidenceChain, a.revision)
	}
}

// RefuseNewFork reads the same anchors. When the ruling is unreadable the generation already reads
// no single head, so the judgment leaves it as it is (nil) instead of refusing a receipt against a
// generation the head judgment answers unknown_predecessor for.
func TestAnchorTextRefuseNewForkLeavesAnUnreadableGenerationAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		declared *string
	}{
		{name: "a candidate declaring nothing"},
		{name: "a candidate declaring a predecessor", declared: ptrTo(anchorRulingHash)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAnchorStore(t, anchorCase{name: "unreadable", turnSQL: "''"})
			err := delivery.RefuseNewFork(a.ctx, a.store.Q(a.ctx), a.rid, 2, "candidate-928", "hc", tc.declared)
			if err != nil {
				t.Fatalf("RefuseNewFork refused: %v", err)
			}
		})
	}
}

func ptrTo(v string) *string { return &v }
