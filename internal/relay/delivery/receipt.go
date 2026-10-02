package delivery

// The one reading of a stored receipt: guard.lookup_receipt. The Stop hook (hook.LookupReceipt) and
// the omission reader (ObserveOmission, DeriveOmission) judge a stored receipt through the
// functions here and nowhere else, so they cannot drift apart. What the answer carries (the
// evidence strings, the fields, their order) is what both consumers print and compare.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ReceiptQuery names the receipt a lookup is asked about: guard.lookup_receipt's arguments. They are
// held as the decoded values a marker or a Stop payload gives, because the guard gates on their
// type.
type ReceiptQuery struct {
	// Relationship, Session and Turn are the relationship the assignment published, the claiming
	// session and the turn. All three must be named (Named); otherwise nothing is looked up and the
	// answer is (nil, true).
	Relationship, Session, Turn any
	// Generation is the generation the assignment registered. nil means the marker carries no stamp,
	// which is compared against nothing; any other value is compared with the relationship's
	// current generation as Python's != compares them.
	Generation any
	// Dispatch is the dispatch request id this session claimed. nil means it claimed none, and the
	// generations table is not consulted.
	Dispatch any
}

func (q ReceiptQuery) named() bool {
	return Named(q.Relationship) && Named(q.Session) && Named(q.Turn)
}

// base is the answer's first four fields, a new list on every call.
func (q ReceiptQuery) base() Obj {
	return Obj{{Key: "relationshipId", Value: q.Relationship}, {Key: "sessionId", Value: q.Session}, {Key: "turnId", Value: q.Turn}, {Key: "atCurrentHead", Value: false}}
}

// LookupStoredReceipt is guard.lookup_receipt over a store the caller already holds: the reviewable
// receipt this turn produced, and whether it stands at the relationship's current head. It returns
// the guard's pair: the answer and whether it could be read at all. Not readable (with the
// unverifiable answer, or with nothing) is never the same as a receipt that is not there.
// err is only what the guard lets out of lookup_receipt: an exception the deliverable judgment
// raises (a *store.ManifestException) or a string the store's driver could not bind
// (*store.UnicodeEncodeError); every other failure of the reads is not readable.
//
// Where it reads. Inside an open transaction of s it reads through that transaction's connection
// and leaves the transaction alone. Outside one it takes a connection from s's pool for the length
// of the reads and runs them under one BEGIN DEFERRED snapshot, released before any artifact is
// hashed. The snapshot is not decoration: the daemon suppresses a staged head in a transaction of its
// own (stage='suppressed'), and reads that chose the head before that commit and fetched its event
// after it answer head_is_not_a_child_receipt, which no single state of the store gives. A caller
// that has no store of its own, only a path, uses LookupStoredReceiptAt. Every statement runs under
// ctx, so a caller inside a transaction must pass the ctx that carries it.
func LookupStoredReceipt(ctx context.Context, s *store.Store, want ReceiptQuery) (Obj, bool, error) {
	return lookupStoredReceipt(ctx, s, want, nil)
}

// LookupStoredReceiptAt is LookupStoredReceipt for a caller that has only the store's path: it opens
// the file read-only with a lock wait of timeout (shortened to what ctx has left) and reads under
// one snapshot. A path of "" is asked of fallback first, as the guard's db_path may be a resolver
// called only once the identities are known to be named; no path at all is not readable.
// store.ErrLiveState is the live-state guard's refusal to read, and it is returned, not reported as
// an unreadable store.
func LookupStoredReceiptAt(ctx context.Context, path string, fallback func() (string, error), timeout time.Duration, want ReceiptQuery) (Obj, bool, error) {
	return lookupStoredReceiptAt(ctx, path, fallback, timeout, want, nil)
}

// afterHead, when set, runs once the head is computed and before its event is read: the point where
// a competing commit would tear a read that holds no snapshot. Only tests set it.
func lookupStoredReceipt(ctx context.Context, s *store.Store, want ReceiptQuery, afterHead func()) (Obj, bool, error) {
	if !want.named() {
		return nil, true, nil
	}
	var (
		head     *receiptHead
		answer   Obj
		readable bool
		err      error
	)
	if s.InTransaction(ctx) {
		head, answer, readable, err = readReceiptHead(ctx, s.Q(ctx), want, afterHead)
	} else {
		head, answer, readable, err = readPooledSnapshot(ctx, s, want, afterHead)
	}
	if head == nil {
		return answer, readable, err
	}
	return judgeReceiptHead(ctx, want, head)
}

func lookupStoredReceiptAt(ctx context.Context, path string, fallback func() (string, error), timeout time.Duration, want ReceiptQuery, afterHead func()) (Obj, bool, error) {
	if !want.named() {
		return nil, true, nil
	}
	if path == "" && fallback != nil {
		var err error
		if path, err = fallback(); err != nil {
			return nil, false, err
		}
	}
	if path == "" {
		return nil, false, nil
	}
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, max(0, time.Until(deadline)))
	}
	ro, err := store.OpenReadOnly(ctx, path, timeout)
	if errors.Is(err, store.ErrLiveState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, nil
	}
	head, answer, readable, _, err := readSnapshot(ctx, ro, want, afterHead)
	_ = ro.Close()
	if head == nil {
		return answer, readable, err
	}
	return judgeReceiptHead(ctx, want, head)
}

// receiptHead is what the reads found when they did not settle the answer: the head event of the
// relationship's current generation and the relationship's artifact roots, as stored.
type receiptHead struct {
	roots                            string
	id, stage, producer, receipt     string
	revision, thread, turn, manifest sql.NullString
}

// readPooledSnapshot reads under one snapshot on a connection of s's pool. A connection that
// could not roll its snapshot back is discarded, never returned with the transaction open.
func readPooledSnapshot(ctx context.Context, s *store.Store, want ReceiptQuery, afterHead func()) (*receiptHead, Obj, bool, error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return readFailure(err)
	}
	head, answer, readable, rollbackErr, err := readSnapshot(ctx, conn, want, afterHead)
	if rollbackErr != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = conn.Close()
	return head, answer, readable, err
}

// readSnapshot runs the reads on q inside BEGIN DEFERRED and rolls the snapshot back before it
// returns, so artifact I/O never holds it open. The rollback runs whatever became of ctx. A
// rollback that failed makes a read that must go on to the artifacts not readable (rollbackErr says
// so for the caller that owns the connection); one that already has its answer keeps it.
func readSnapshot(ctx context.Context, q store.Querier, want ReceiptQuery, afterHead func()) (head *receiptHead, answer Obj, readable bool, rollbackErr, err error) {
	if _, beginErr := q.ExecContext(ctx, "BEGIN DEFERRED"); beginErr != nil {
		head, answer, readable, err = readFailure(beginErr)
		return head, answer, readable, nil, err
	}
	head, answer, readable, err = readReceiptHead(ctx, q, want, afterHead)
	if _, rollbackErr = q.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); rollbackErr != nil && head != nil {
		return nil, nil, false, rollbackErr, nil
	}
	return head, answer, readable, rollbackErr, err
}

// readFailure is what a failed read answers. The guard's except sqlite3.Error is (None, False), but
// a string the store's driver could not bind raises UnicodeEncodeError, which is not a
// sqlite3.Error and leaves lookup_receipt.
func readFailure(err error) (*receiptHead, Obj, bool, error) {
	if store.EncodeError(err) != nil {
		return nil, nil, false, err
	}
	return nil, nil, false, nil
}

// readReceiptHead is lookup_receipt's reads, all on q, in the guard's order: the relationship, the
// generation the assignment registered, the dispatch that opened the current generation, the head
// of that generation and the head's event. It returns either the finished answer (readable), the
// head to judge, or neither (not readable).
func readReceiptHead(ctx context.Context, q store.Querier, want ReceiptQuery, afterHead func()) (*receiptHead, Obj, bool, error) {
	relationship, _ := want.Relationship.(string)
	answer := func(evidence string, extra ...F) (*receiptHead, Obj, bool, error) {
		return nil, append(set(want.base(), "evidence", evidence), extra...), true, nil
	}
	var status, roots string
	var superseded sql.NullString
	var current int64
	err := q.QueryRowContext(ctx, "SELECT status, superseded_by, execution_generation, artifact_roots FROM relationships WHERE relationship_id = ?", relationship).Scan(&status, &superseded, &current, &roots)
	if errors.Is(err, sql.ErrNoRows) {
		return answer("relationship_absent")
	}
	if err != nil {
		return readFailure(err)
	}
	if status != "active" || (superseded.Valid && superseded.String != "") {
		return answer("relationship_not_active")
	}
	if want.Generation != nil && !pyvalue.Equal(want.Generation, current) {
		return answer("registration_generation_mismatch", F{Key: "detail", Value: "the assignment registered generation " + pyvalue.Str(want.Generation) + " and the relationship now stands on generation " + strconv.FormatInt(current, 10)})
	}
	if want.Dispatch != nil {
		var opened sql.NullString
		err = q.QueryRowContext(ctx, "SELECT dispatch_request_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", relationship, current).Scan(&opened)
		if errors.Is(err, sql.ErrNoRows) {
			return answer("generation_absent", F{Key: "detail", Value: "the relationship reports generation " + strconv.FormatInt(current, 10) + " and the store holds no record of which dispatch opened it"})
		}
		if err != nil {
			return readFailure(err)
		}
		if !SameIdentity(opened.String, want.Dispatch) {
			return answer("generation_dispatch_mismatch", F{Key: "detail", Value: "the relationship stands on generation " + strconv.FormatInt(current, 10) + ", which a different dispatch request opened"})
		}
	}
	head, err := HeadRevisionFrom(ctx, q, relationship, current)
	if err != nil {
		return readFailure(err)
	}
	if afterHead != nil {
		afterHead()
	}
	if slices.Contains(ambiguousEvidence, str(head, "evidence")) {
		return answer("head_" + str(head, "evidence"))
	}
	eventID := str(head, "eventId")
	if eventID == "" {
		return answer("no_reviewable_revision")
	}
	found := receiptHead{roots: roots}
	err = q.QueryRowContext(ctx, "SELECT event_id, stage, revision_hash, turn_thread_id, turn_id, producer, receipt, manifest_ref FROM events WHERE event_id = ?", eventID).Scan(&found.id, &found.stage, &found.revision, &found.thread, &found.turn, &found.producer, &found.receipt, &found.manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return answer("no_reviewable_revision")
	}
	if err != nil {
		return readFailure(err)
	}
	return &found, nil, false, nil
}

// judgeReceiptHead is what lookup_receipt does with the head event once its snapshot is closed:
// whose receipt it is, and whether the artifacts it was computed over are still those artifacts.
// Standing at the head is a statement about the lineage; a receipt that outlived its deliverable
// would release a turn whose work no longer matches anything anyone reviewed.
func judgeReceiptHead(ctx context.Context, want ReceiptQuery, head *receiptHead) (Obj, bool, error) {
	base := want.base()
	answer := func(evidence string, extra ...F) (Obj, bool, error) {
		return append(set(base, "evidence", evidence), extra...), true, nil
	}
	if head.producer != "child" || (head.stage != "staged" && head.stage != "final") {
		return answer("head_is_not_a_child_receipt")
	}
	if !SameIdentity(head.thread.String, want.Session) || !SameIdentity(head.turn.String, want.Turn) {
		return answer("head_belongs_to_another_turn")
	}
	roots, payload, unreadable := store.DecodeStoredReceipt(head.roots, head.receipt)
	if unreadable != "" {
		return answer("stored_receipt_unreadable", F{Key: "detail", Value: unreadable}, F{Key: "eventId", Value: head.id})
	}
	state, binding, detail, err := store.DeliverableState(ctx, payload, head.manifest.String, roots)
	if err != nil {
		return nil, false, err
	}
	switch state {
	case store.DeliverableUnverifiable:
		return append(set(base, "evidence", "deliverable_unverifiable"), F{Key: "detail", Value: nullable(detail)}), false, nil
	case store.DeliverableChanged:
		return answer("artifacts_changed_since_receipt", F{Key: "detail", Value: nullable(detail)}, F{Key: "eventId", Value: head.id}, F{Key: "revisionHash", Value: nullable(head.revision.String)})
	}
	base = set(base, "atCurrentHead", true)
	return answer("at_head", F{Key: "eventId", Value: head.id}, F{Key: "revisionHash", Value: nullable(head.revision.String)}, F{Key: "stage", Value: head.stage}, F{Key: "deliverableBinding", Value: binding})
}
