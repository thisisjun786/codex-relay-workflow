package delivery

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// registered is the detail of a registration that names another generation than the current one.
func registered(value string) string {
	return "the assignment registered generation " + value + " and the relationship now stands on generation 1"
}

// The generation a registration names is compared with the relationship's current one as the guard
// compares it, Python's != on the decoded value, and the dispatch and the identities gate the rest
// of the lookup in the guard's order. Each case states the whole answer.
func TestStoredReceiptLookupAnswersEachStateOfTheRelationship(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := stageReceipt(t)
	other := func(q *ReceiptQuery) { q.Dispatch = "another-dispatch" }
	for _, c := range []struct {
		name string
		// apply and undo rewrite and restore the store around the case.
		apply, undo []string
		ask         func(q *ReceiptQuery)
		want        func() Obj
		// absent is a lookup that is answered (nil, true) without reading the store.
		absent bool
	}{
		{name: "generation-integer-equal", ask: func(q *ReceiptQuery) { q.Generation = int64(1) }, want: s.atHead},
		{name: "generation-integer-different", ask: func(q *ReceiptQuery) { q.Generation = int64(2) },
			want: func() Obj {
				return s.answer(false, "registration_generation_mismatch", F{Key: "detail", Value: registered("2")})
			}},
		{name: "generation-true-is-one", ask: func(q *ReceiptQuery) { q.Generation = true }, want: s.atHead},
		{name: "generation-false-is-zero", ask: func(q *ReceiptQuery) { q.Generation = false },
			want: func() Obj {
				return s.answer(false, "registration_generation_mismatch", F{Key: "detail", Value: registered("False")})
			}},
		{name: "generation-integral-float", ask: func(q *ReceiptQuery) { q.Generation = 1.0 }, want: s.atHead},
		{name: "generation-integral-float-different", ask: func(q *ReceiptQuery) { q.Generation = 2.0 },
			want: func() Obj {
				return s.answer(false, "registration_generation_mismatch", F{Key: "detail", Value: registered("2.0")})
			}},
		{name: "generation-fractional-float", ask: func(q *ReceiptQuery) { q.Generation = 1.5 },
			want: func() Obj {
				return s.answer(false, "registration_generation_mismatch", F{Key: "detail", Value: registered("1.5")})
			}},
		{name: "generation-numeric-string-is-not-the-integer", ask: func(q *ReceiptQuery) { q.Generation = "1" },
			want: func() Obj {
				return s.answer(false, "registration_generation_mismatch", F{Key: "detail", Value: registered("1")})
			}},
		{name: "generation-no-stamp-is-compared-against-nothing", ask: func(q *ReceiptQuery) { q.Generation = nil }, want: s.atHead},

		{name: "dispatch-none-claimed", ask: func(q *ReceiptQuery) { q.Dispatch = nil }, want: s.atHead},
		{name: "dispatch-another", ask: other,
			want: func() Obj {
				return s.answer(false, "generation_dispatch_mismatch", F{Key: "detail", Value: "the relationship stands on generation 1, which a different dispatch request opened"})
			}},
		{name: "dispatch-blank-is-not-a-claim", ask: func(q *ReceiptQuery) { q.Dispatch = "" },
			want: func() Obj {
				return s.answer(false, "generation_dispatch_mismatch", F{Key: "detail", Value: "the relationship stands on generation 1, which a different dispatch request opened"})
			}},
		{name: "dispatch-not-a-string", ask: func(q *ReceiptQuery) { q.Dispatch = 5 },
			want: func() Obj {
				return s.answer(false, "generation_dispatch_mismatch", F{Key: "detail", Value: "the relationship stands on generation 1, which a different dispatch request opened"})
			}},
		{name: "generation-is-answered-before-the-dispatch", ask: func(q *ReceiptQuery) { q.Generation, q.Dispatch = int64(2), "another-dispatch" },
			want: func() Obj {
				return s.answer(false, "registration_generation_mismatch", F{Key: "detail", Value: registered("2")})
			}},

		// The relationship has moved to a generation no row says a dispatch opened.
		{name: "generation-with-no-dispatch-record", apply: []string{"UPDATE relationships SET execution_generation = 2"}, undo: []string{"UPDATE relationships SET execution_generation = 1"},
			ask: func(q *ReceiptQuery) { q.Generation = int64(2) },
			want: func() Obj {
				return s.answer(false, "generation_absent", F{Key: "detail", Value: "the relationship reports generation 2 and the store holds no record of which dispatch opened it"})
			}},
		{name: "generation-with-no-dispatch-claimed-has-no-revision", apply: []string{"UPDATE relationships SET execution_generation = 2"}, undo: []string{"UPDATE relationships SET execution_generation = 1"},
			ask:  func(q *ReceiptQuery) { q.Generation, q.Dispatch = int64(2), nil },
			want: func() Obj { return s.answer(false, "no_reviewable_revision") }},

		{name: "relationship-absent", ask: func(q *ReceiptQuery) { q.Relationship = "rel-unknown" },
			want: func() Obj {
				return Obj{{Key: "relationshipId", Value: "rel-unknown"}, {Key: "sessionId", Value: child}, {Key: "turnId", Value: dispatchTurn}, {Key: "atCurrentHead", Value: false}, {Key: "evidence", Value: "relationship_absent"}}
			}},
		{name: "relationship-not-active", apply: []string{"UPDATE relationships SET status = 'archived'"}, undo: []string{"UPDATE relationships SET status = 'active'"},
			want: func() Obj { return s.answer(false, "relationship_not_active") }},
		{name: "relationship-superseded", apply: []string{"UPDATE relationships SET superseded_by = 'rel-later'"}, undo: []string{"UPDATE relationships SET superseded_by = NULL"},
			want: func() Obj { return s.answer(false, "relationship_not_active") }},

		{name: "head-suppressed-is-no-revision", apply: []string{"UPDATE events SET suppressed_reason = 'the turn ended'"}, undo: []string{"UPDATE events SET suppressed_reason = NULL"},
			want: func() Obj { return s.answer(false, "no_reviewable_revision") }},
		{name: "head-stage-is-not-a-receipt", apply: []string{"UPDATE events SET stage = 'suppressed'"}, undo: []string{"UPDATE events SET stage = '" + s.stage + "'"},
			want: func() Obj { return s.answer(false, "head_is_not_a_child_receipt") }},
		{name: "head-belongs-to-another-session", ask: func(q *ReceiptQuery) { q.Session = "01another-session" },
			want: func() Obj {
				return Obj{{Key: "relationshipId", Value: s.f.rid}, {Key: "sessionId", Value: "01another-session"}, {Key: "turnId", Value: dispatchTurn}, {Key: "atCurrentHead", Value: false}, {Key: "evidence", Value: "head_belongs_to_another_turn"}}
			}},
		{name: "head-belongs-to-another-turn", ask: func(q *ReceiptQuery) { q.Turn = "turn-later" },
			want: func() Obj {
				return Obj{{Key: "relationshipId", Value: s.f.rid}, {Key: "sessionId", Value: child}, {Key: "turnId", Value: "turn-later"}, {Key: "atCurrentHead", Value: false}, {Key: "evidence", Value: "head_belongs_to_another_turn"}}
			}},

		// An identity that is not named is not looked up at all.
		{name: "relationship-not-named", ask: func(q *ReceiptQuery) { q.Relationship = "" }, absent: true},
		{name: "relationship-blank", ask: func(q *ReceiptQuery) { q.Relationship = " \t" }, absent: true},
		{name: "session-not-a-string", ask: func(q *ReceiptQuery) { q.Session = nil }, absent: true},
		{name: "turn-not-a-string", ask: func(q *ReceiptQuery) { q.Turn = 5 }, absent: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.f.t = t
			for _, statement := range c.apply {
				s.exec(statement)
			}
			defer func() {
				for _, statement := range c.undo {
					s.exec(statement)
				}
			}()
			q := s.query()
			if c.ask != nil {
				c.ask(&q)
			}
			if c.absent {
				viaStore, viaPath := s.both(ctx, q)
				for name, got := range map[string]lookedUp{"LookupStoredReceipt": viaStore, "LookupStoredReceiptAt": viaPath} {
					if got.answer != nil || !got.readable || got.err != nil {
						t.Errorf("%s: %v readable=%v err=%v, want no answer and readable", name, got.answer, got.readable, got.err)
					}
				}
				return
			}
			s.expect(ctx, q, c.want(), true)
		})
	}
	s.f.t = t
	s.expect(ctx, s.query(), s.atHead(), true)
}

// A lookup that is given neither a path nor a resolver cannot look, and says so, where one that is
// not asked about a named turn never needs a store, so its resolver is not called.
func TestStoredReceiptLookupAtNeedsAPathOnlyForANamedTurn(t *testing.T) {
	ctx := context.Background()
	q := ReceiptQuery{Relationship: "rel", Session: "session", Turn: "turn"}
	if answer, readable, err := LookupStoredReceiptAt(ctx, "", nil, time.Second, q); answer != nil || readable || err != nil {
		t.Errorf("no path: %v readable=%v err=%v, want not readable", answer, readable, err)
	}
	asked := false
	resolver := func() (string, error) { asked = true; return "", errors.New("no store") }
	if _, readable, err := LookupStoredReceiptAt(ctx, "", resolver, time.Second, q); readable || err == nil || !asked {
		t.Errorf("a resolver that refuses: readable=%v err=%v asked=%v", readable, err, asked)
	}
	asked = false
	q.Turn = ""
	if answer, readable, err := LookupStoredReceiptAt(ctx, "", resolver, time.Second, q); answer != nil || !readable || err != nil || asked {
		t.Errorf("an unnamed turn: %v readable=%v err=%v asked=%v, want readable without asking", answer, readable, err, asked)
	}
	if answer, readable, err := LookupStoredReceiptAt(ctx, t.TempDir()+"/absent.sqlite3", nil, time.Second, ReceiptQuery{Relationship: "rel", Session: "session", Turn: "turn"}); answer != nil || readable || err != nil {
		t.Errorf("a store that is not there: %v readable=%v err=%v, want not readable", answer, readable, err)
	}
}

// The caller's own connection is the one the lookup reads on: inside the store's transaction it
// sees that transaction's rewrite of the receipt, which a second connection to the file cannot, and
// it neither waits for a second pool connection (the pool has one, and the transaction holds it)
// nor leaves the transaction changed.
func TestStoredReceiptLookupReadsThroughTheCallersTransaction(t *testing.T) {
	ctx := context.Background()
	s := stageReceipt(t)
	rolledBack := errors.New("rolled back on purpose")
	err := s.f.store.Transaction(ctx, func(txCtx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(txCtx, "UPDATE events SET receipt = '' WHERE event_id = ?", s.event); err != nil {
			return err
		}
		bounded, cancel := context.WithTimeout(txCtx, 20*time.Second)
		defer cancel()
		answer, readable, err := LookupStoredReceipt(bounded, s.f.store, s.query())
		if err != nil || !readable || !equalObj(answer, s.unreadable("JSONDecodeError: Expecting value: line 1 column 1 (char 0)")) {
			t.Errorf("through the transaction: %v readable=%v err=%v, want the receipt it rewrote unreadable", answer, readable, err)
		}
		committed, readable, err := LookupStoredReceiptAt(bounded, s.f.store.Path, nil, time.Second, s.query())
		if err != nil || !readable || !equalObj(committed, s.atHead()) {
			t.Errorf("through the file: %v readable=%v err=%v, want the committed receipt at the head", committed, readable, err)
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) {
		t.Fatalf("the transaction: %v", err)
	}
	s.expect(ctx, s.query(), s.atHead(), true)
}

// The lookup reads under one snapshot. The daemon suppresses a staged head in a transaction of its
// own (stage and suppressed_reason together, as observe.go does); with the head already chosen
// and its event not yet read, a read without a snapshot answers that the head is not a child's
// receipt, an answer no single state of the store gives: the receipt was at the head before the
// commit and there is no reviewable revision after it.
func TestStoredReceiptLookupReadsUnderOneSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := stageReceipt(t)
	// suppress commits, through a connection of its own, what the daemon commits.
	suppress := func() {
		db, err := sql.Open("sqlite", "file:"+s.f.store.Path+"?_pragma=busy_timeout(5000)")
		mustDo(t, err)
		defer db.Close()
		_, err = db.ExecContext(ctx, "UPDATE events SET stage='suppressed', suppressed_reason='the turn ended interrupted, so the staged claim is not promoted' WHERE event_id=?", s.event)
		mustDo(t, err)
	}
	// unsuppress puts the head back for the next case.
	unsuppress := func() {
		s.exec("UPDATE events SET stage = ?, suppressed_reason = NULL WHERE event_id = ?", s.stage, s.event)
	}
	t.Run("through the store", func(t *testing.T) {
		s.f.t = t
		defer unsuppress()
		answer, readable, err := lookupStoredReceipt(ctx, s.f.store, s.query(), suppress)
		if err != nil || !readable || !equalObj(answer, s.atHead()) {
			t.Errorf("%v readable=%v err=%v, want the receipt as it stood when the lookup began", answer, readable, err)
		}
	})
	t.Run("through the path", func(t *testing.T) {
		s.f.t = t
		defer unsuppress()
		answer, readable, err := lookupStoredReceiptAt(ctx, s.f.store.Path, nil, time.Second, s.query(), suppress)
		if err != nil || !readable || !equalObj(answer, s.atHead()) {
			t.Errorf("%v readable=%v err=%v, want the receipt as it stood when the lookup began", answer, readable, err)
		}
	})
	t.Run("without a snapshot the same commit tears the read", func(t *testing.T) {
		s.f.t = t
		defer unsuppress()
		head, answer, readable, err := readReceiptHead(ctx, s.f.store.Q(ctx), s.query(), suppress)
		if err != nil || head == nil || answer != nil || readable {
			t.Fatalf("the bare reads: head=%v answer=%v readable=%v err=%v", head, answer, readable, err)
		}
		got, _, err := judgeReceiptHead(ctx, s.query(), head)
		if err != nil || !equalObj(got, s.answer(false, "head_is_not_a_child_receipt")) {
			t.Errorf("the torn read answered %v (%v), want head_is_not_a_child_receipt", got, err)
		}
	})
	t.Run("a commit before the lookup is seen", func(t *testing.T) {
		s.f.t = t
		defer unsuppress()
		suppress()
		s.expect(ctx, s.query(), s.answer(false, "no_reviewable_revision"), true)
	})
	s.f.t = t
	s.expect(ctx, s.query(), s.atHead(), true)
}

// A lookup whose context ends, between the head and its event, is not readable and leaves no
// transaction behind: the snapshot is rolled back whatever became of the context, so the store's
// one connection goes back to the pool clean and the next transaction begins.
func TestStoredReceiptLookupLeavesNoTransactionWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	s := stageReceipt(t)
	ctx, cancel := context.WithCancel(context.Background())
	answer, readable, err := lookupStoredReceipt(ctx, s.f.store, s.query(), cancel)
	if answer != nil || readable || err != nil {
		t.Errorf("a context that ended: %v readable=%v err=%v, want not readable", answer, readable, err)
	}
	begun := context.Background()
	if err := s.f.store.Transaction(begun, func(context.Context, *sql.Conn) error { return nil }); err != nil {
		t.Fatalf("the store's next transaction: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	answer, readable, err = lookupStoredReceiptAt(ctx, s.f.store.Path, nil, time.Second, s.query(), cancel)
	if answer != nil || readable || err != nil {
		t.Errorf("a context that ended over the path: %v readable=%v err=%v, want not readable", answer, readable, err)
	}
	s.expect(begun, s.query(), s.atHead(), true)
}

// A string the store's driver cannot bind raises where the guard's Python let a UnicodeEncodeError
// out of lookup_receipt: the lookup returns it, from either entry point, and the omission reads it
// as evidence_unreadable with the exception's words. Any other failure of the reads, or a
// deliverable nobody could compare, is only not readable, which the omission leaves as
// receipt_unreadable.
func TestStoredReceiptLookupReturnsWhatTheDriverCannotBind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := stageReceipt(t)
	q := s.query()
	q.Relationship = "rel-\xff"
	viaStore, viaPath := s.both(ctx, q)
	for name, got := range map[string]lookedUp{"LookupStoredReceipt": viaStore, "LookupStoredReceiptAt": viaPath} {
		if got.answer != nil || got.readable || store.EncodeError(got.err) == nil {
			t.Errorf("%s: %v readable=%v err=%v, want the encode error", name, got.answer, got.readable, got.err)
			continue
		}
		const words = "'utf-8' codec can't encode character '\\udcff' in position 4: surrogates not allowed"
		if reason := omissionReceiptUnmeasured(got.readable, got.err); reason != "evidence_unreadable: "+words {
			t.Errorf("%s: the omission is left %q", name, reason)
		}
	}
	for _, c := range []struct {
		name     string
		readable bool
		err      error
		want     string
	}{
		{"read", true, nil, ""},
		{"not readable", false, nil, "receipt_unreadable"},
		{"another failure", true, errors.New("disk I/O error"), "receipt_unreadable"},
		{"not readable and failed", false, errors.New("disk I/O error"), "receipt_unreadable"},
	} {
		if got := omissionReceiptUnmeasured(c.readable, c.err); got != c.want {
			t.Errorf("%s: the omission is left %q, want %q", c.name, got, c.want)
		}
	}
}
