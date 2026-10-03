package dagsched

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-283: the project summary outbox (docs/relay/dag-outbox.md). The relay holds no Linear credential and makes no Linear call. It keeps, per plan and document, the ordered queue of the
// summaries the plan's parent writes with its own connector: the parent takes the newest entry (claim), writes it, reads the document back and confirms it (complete), and on a failure
// records it (fail) and takes the same entry again. An entry the plan has moved past is superseded and never written, so an older summary cannot be put over a newer one; every command
// writes the summary table and nothing else, so recovering a summary re-runs no child and re-sends no correction.

// The states of a summary entry. Only confirmed and superseded are final.
const (
	SummaryPending    = "pending"
	SummaryClaimed    = "claimed"
	SummaryConfirmed  = "confirmed"
	SummaryFailed     = "failed"
	SummarySuperseded = "superseded"
)

// MaxSummaryAttempts is how many failures an entry takes before it is failed and waits for an explicit retry.
const MaxSummaryAttempts = 8

const (
	maxSummaryDocument = 512
	maxSummaryError    = 2000
)

// SummaryEntry is one row of dag_summary_outbox.
type SummaryEntry struct {
	SummaryID, PlanID, ProjectKey, Document string
	PlanRevision, Seq                       int64
	SubjectDigest, StateDigest              string
	Summary, SummarySHA256                  string
	State                                   string
	Attempts                                int64
	LastError, ClaimToken, ClaimedBy        string
	ClaimedAt, Readback, ConfirmedAt        string
	EnqueuedBy                              string
	CoordinatorEpoch                        int64
	CreatedAt, UpdatedAt                    string
}

const summaryColumns = "summary_id, plan_id, project_key, document, plan_revision, seq, subject_digest, state_digest, summary, summary_sha256, state, attempts, last_error, claim_token, claimed_by, claimed_at, readback, confirmed_at, enqueued_by, coordinator_epoch, created_at, updated_at"

func scanSummary(row interface{ Scan(...any) error }) (SummaryEntry, error) {
	var e SummaryEntry
	var lastError, token, claimedBy, claimedAt, readback, confirmedAt sql.NullString
	err := row.Scan(&e.SummaryID, &e.PlanID, &e.ProjectKey, &e.Document, &e.PlanRevision, &e.Seq, &e.SubjectDigest, &e.StateDigest, &e.Summary, &e.SummarySHA256, &e.State, &e.Attempts,
		&lastError, &token, &claimedBy, &claimedAt, &readback, &confirmedAt, &e.EnqueuedBy, &e.CoordinatorEpoch, &e.CreatedAt, &e.UpdatedAt)
	e.LastError, e.ClaimToken, e.ClaimedBy, e.ClaimedAt, e.Readback, e.ConfirmedAt = lastError.String, token.String, claimedBy.String, claimedAt.String, readback.String, confirmedAt.String
	return e, err
}

// summaryToken is a fresh claim token; a variable so a test can fix it.
var summaryToken = func() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func summaryTableExists(ctx context.Context, q store.Querier) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'dag_summary_outbox'").Scan(&n)
	return n > 0, err
}

func loadSummary(ctx context.Context, q store.Querier, id string) (SummaryEntry, error) {
	if ok, err := summaryTableExists(ctx, q); err != nil {
		return SummaryEntry{}, err
	} else if !ok {
		return SummaryEntry{}, refuse(contract.RefusalUnregisteredScope, "no summary entry %q", id)
	}
	e, err := scanSummary(q.QueryRowContext(ctx, "SELECT "+summaryColumns+" FROM dag_summary_outbox WHERE summary_id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return SummaryEntry{}, refuse(contract.RefusalUnregisteredScope, "no summary entry %q", id)
	}
	return e, err
}

func newestSeq(ctx context.Context, q store.Querier, plan, document string) (int64, error) {
	var seq sql.NullInt64
	err := q.QueryRowContext(ctx, "SELECT MAX(seq) FROM dag_summary_outbox WHERE plan_id = ? AND document = ?", plan, document).Scan(&seq)
	return seq.Int64, err
}

// summaryActor refuses a task that is not the one registered parent of the entry's project: only that parent writes a plan's summary, and a replaced parent is refused at once.
func summaryActor(ctx context.Context, q store.Querier, project, actor string) error {
	parents, err := projectParents(ctx, q, project)
	if err != nil {
		return err
	}
	if len(parents) != 1 || parents[0] != actor {
		return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the registered parent of project %s", actor, project)
	}
	return nil
}

func validDocument(document string) error {
	switch {
	case document == "" || strings.TrimSpace(document) != document:
		return refuse(contract.RefusalMalformedReceipt, "a summary names its document (the Linear document id or URL), without padding")
	case !utf8.ValidString(document) || utf8.RuneCountInString(document) > maxSummaryDocument:
		return refuse(contract.RefusalMalformedReceipt, "a document is at most %d characters of valid text", maxSummaryDocument)
	}
	for _, r := range document {
		if unicode.IsControl(r) {
			return refuse(contract.RefusalMalformedReceipt, "a document is one line of text: it holds a control character")
		}
	}
	return nil
}

// SummaryEnqueued is the answer of EnqueueSummary.
type SummaryEnqueued struct {
	Entry      SummaryEntry
	Replayed   bool
	Superseded []string
}

// EnqueueSummary records the summary the plan owes the document now. It reads the plan's progress in the transaction it writes in, so the entry states exactly the plan it was made from.
// The same state of the plan is one entry: when the stream's newest entry already states it, that entry is answered (a replay), whatever became of it. Another state appends the next
// entry and supersedes every entry of the stream that was not confirmed, in the same transaction, so there is one open entry per stream and an older one is never written after it.
// again records the state even when its newest entry is confirmed: the parent says the document no longer carries the confirmed summary (a concurrent edit removed it).
func (s *Scheduler) EnqueueSummary(ctx context.Context, plan, actor, document string, again bool) (SummaryEnqueued, error) {
	var out SummaryEnqueued
	if err := validDocument(document); err != nil {
		return out, err
	}
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		snap, _, err := dag.SnapshotAt(txCtx, q, plan, 0)
		if err != nil {
			return err
		}
		if err := summaryActor(txCtx, q, snap.ProjectKey, actor); err != nil {
			return err
		}
		progress, err := s.Progress(txCtx, q, plan)
		if err != nil {
			return err
		}
		newest, found, err := newestEntry(txCtx, q, plan, document)
		if err != nil {
			return err
		}
		if found && newest.SubjectDigest == progress.Digest && !(again && newest.State == SummaryConfirmed) {
			out.Entry, out.Replayed = newest, true
			return nil
		}
		now := s.now()
		entry := SummaryEntry{PlanID: plan, ProjectKey: progress.ProjectKey, Document: document, PlanRevision: progress.Reading.PlanRevision, Seq: newest.Seq + 1, SubjectDigest: progress.Digest,
			StateDigest: progress.Reading.StateDigest, Summary: SummaryText(progress), State: SummaryPending, EnqueuedBy: actor, CreatedAt: now, UpdatedAt: now}
		entry.SummarySHA256 = sha(entry.Summary)
		entry.SummaryID = "sum-" + shaOf([]byte(dag.Canonical(map[string]any{"plan_id": plan, "document": document, "plan_revision": entry.PlanRevision, "subject_digest": entry.SubjectDigest, "seq": entry.Seq})))[:32]
		rows, err := q.QueryContext(txCtx, "SELECT summary_id FROM dag_summary_outbox WHERE plan_id = ? AND document = ? AND state IN ('pending','claimed','failed') ORDER BY seq", plan, document)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			out.Superseded = append(out.Superseded, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range out.Superseded {
			if _, err := q.ExecContext(txCtx, "UPDATE dag_summary_outbox SET state = 'superseded', claim_token = NULL, updated_at = ? WHERE summary_id = ?", now, id); err != nil {
				return err
			}
		}
		if _, err := q.ExecContext(txCtx, "INSERT INTO dag_summary_outbox ("+summaryColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,0,NULL,NULL,NULL,NULL,NULL,NULL,?,0,?,?)",
			entry.SummaryID, entry.PlanID, entry.ProjectKey, entry.Document, entry.PlanRevision, entry.Seq, entry.SubjectDigest, entry.StateDigest, entry.Summary, entry.SummarySHA256, entry.State,
			entry.EnqueuedBy, entry.CreatedAt, entry.UpdatedAt); err != nil {
			return err
		}
		out.Entry = entry
		return nil
	})
	return out, err
}

// newestEntry is the entry of the stream with the highest sequence number, if there is one.
func newestEntry(ctx context.Context, q store.Querier, plan, document string) (SummaryEntry, bool, error) {
	if ok, err := summaryTableExists(ctx, q); err != nil || !ok {
		return SummaryEntry{}, false, err
	}
	e, err := scanSummary(q.QueryRowContext(ctx, "SELECT "+summaryColumns+" FROM dag_summary_outbox WHERE plan_id = ? AND document = ? ORDER BY seq DESC LIMIT 1", plan, document))
	if errors.Is(err, sql.ErrNoRows) {
		return SummaryEntry{}, false, nil
	}
	return e, err == nil, err
}

// SummaryStream is one document of a plan: its newest entry, whether the plan owes it a write, and whether that entry states the plan as it is now.
type SummaryStream struct {
	Document string
	Newest   SummaryEntry
	// Owed: the newest entry is not confirmed, so the document is behind what the relay holds.
	Owed bool
	// UpToDate: the newest entry states the plan as the store reads it now (a plan that moved on since has more to say than any entry).
	UpToDate bool
	History  []SummaryEntry
	Counts   map[string]int
}

// SummaryStatus is the answer of Scheduler.SummaryStatus.
type SummaryStatus struct {
	PlanID, ProjectKey string
	PlanRevision       int64
	ProgressDigest     string
	Streams            []SummaryStream
}

// SummaryStatus reads a plan's streams (one document, or all) in one read transaction. It writes nothing; with history it lists every entry of each stream, oldest first.
func (s *Scheduler) SummaryStatus(ctx context.Context, plan, document string, history bool) (SummaryStatus, error) {
	var out SummaryStatus
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		progress, err := s.Progress(txCtx, q, plan)
		if err != nil {
			return err
		}
		out = SummaryStatus{PlanID: plan, ProjectKey: progress.ProjectKey, PlanRevision: progress.Reading.PlanRevision, ProgressDigest: progress.Digest}
		if ok, err := summaryTableExists(txCtx, q); err != nil || !ok {
			return err
		}
		query, args := "SELECT "+summaryColumns+" FROM dag_summary_outbox WHERE plan_id = ?", []any{plan}
		if document != "" {
			query, args = query+" AND document = ?", append(args, document)
		}
		rows, err := q.QueryContext(txCtx, query+" ORDER BY document, seq", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var current *SummaryStream
		for rows.Next() {
			e, err := scanSummary(rows)
			if err != nil {
				return err
			}
			if current == nil || current.Document != e.Document {
				out.Streams = append(out.Streams, SummaryStream{Document: e.Document, Counts: map[string]int{}})
				current = &out.Streams[len(out.Streams)-1]
			}
			current.Newest = e
			current.Counts[e.State]++
			if history {
				current.History = append(current.History, e)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out.Streams {
			st := &out.Streams[i]
			st.Owed = st.Newest.State != SummaryConfirmed
			st.UpToDate = st.Newest.SubjectDigest == progress.Digest
		}
		return nil
	})
	return out, err
}

// SummaryOperation is what the parent needs to write one entry: where, the markers of the plan's container, the exact block, the exact container text it replaces the document's current
// container with, and the empty container to create once when the document has none.
type SummaryOperation struct {
	Document, ContainerStart, ContainerEnd string
	Block, Container, EmptyContainer       string
	Protocol                               []string
}

// SummaryClaim is the answer of ClaimSummary: the entry, the fresh claim token and the operation.
type SummaryClaim struct {
	Entry     SummaryEntry
	Token     string
	Operation SummaryOperation
}

var summaryProtocol = []string{
	"read the document (get_document) and keep that one text: everything below is judged on it and writes only against it",
	"run reconcile on that text. Do not write when writable is false or relation is newer: the entry was overtaken, so take the newest entry (status). already_written: confirm with complete and write nothing",
	"repair says how to write. replace_container (the document has the plan's container once): ONE atomic conditional replacement of that read's container text, markers included, with this claim's container text: Linear save_document with patch [{op: replace, old_string: <the container text from your read>, new_string: <the container text>}], replace_all off",
	"repair initialize (the document has no container): ONE atomic conditional replacement of the WHOLE document text you read with that same text followed by a blank line and the empty_container: patch [{op: replace, old_string: <the document as you read it>, new_string: <the document as you read it, a blank line, the empty_container>}]. If anything changed since your read the replacement is refused whole, so two claimants cannot make two containers. Never an append, an insert or a whole-document content save for a summary or its container, since none of them is conditioned. Then read again and start at reconcile. A document created for the summary should be created with the empty_container as its content",
	"repair manual (malformed: a block or container that is not closed, two containers, or a block outside its container): write nothing. Record the failure with fail, naming the document and what reconcile says, and report it to a person: the relay never confirms a corrupt document, and one replacement cannot repair markers that appear twice or text outside the container",
	"read the document back and pass the whole text to complete with the claim token and this document",
	"on any failure record it with fail and claim the same entry again; take no other entry, and do not re-run a child or re-send a correction",
}

func operationOf(e SummaryEntry) SummaryOperation {
	return SummaryOperation{Document: e.Document, ContainerStart: containerStart(e.PlanID), ContainerEnd: containerEnd(e.PlanID), Block: summaryBlock(e), Container: summaryContainer(e),
		EmptyContainer: emptyContainer(e.PlanID), Protocol: slices.Clone(summaryProtocol)}
}

func notClaimable(e SummaryEntry, newest int64) error {
	switch e.State {
	case SummaryConfirmed:
		return refuse(contract.RefusalSyncNotClaimable, "%s is already confirmed", e.SummaryID)
	case SummarySuperseded:
		return refuse(contract.RefusalSyncNotClaimable, "%s (#%d) is superseded by a newer summary of the document; take the newest entry (dag-summary-status)", e.SummaryID, e.Seq)
	case SummaryFailed:
		return refuse(contract.RefusalSyncNotClaimable, "%s failed after %d attempts; retry it deliberately (dag-summary-retry)", e.SummaryID, e.Attempts)
	}
	if newest > e.Seq {
		return refuse(contract.RefusalSyncNotClaimable, "%s (#%d) is not the newest entry of its document (#%d is)", e.SummaryID, e.Seq, newest)
	}
	return nil
}

// ClaimSummary takes an entry to write: only the newest, still open entry of its stream, and only for the project's registered parent. A claim gives a fresh token and invalidates the one
// before it, so a parent that lost the answer of its claim claims again at once, and a claimant that was overtaken (or replaced) can no longer confirm or fail the entry.
func (s *Scheduler) ClaimSummary(ctx context.Context, id, actor string) (SummaryClaim, error) {
	var out SummaryClaim
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		e, err := loadSummary(txCtx, q, id)
		if err != nil {
			return err
		}
		if err := summaryActor(txCtx, q, e.ProjectKey, actor); err != nil {
			return err
		}
		newest, err := newestSeq(txCtx, q, e.PlanID, e.Document)
		if err != nil {
			return err
		}
		if err := notClaimable(e, newest); err != nil {
			return err
		}
		token, err := summaryToken()
		if err != nil {
			return err
		}
		now := s.now()
		if _, err := q.ExecContext(txCtx, "UPDATE dag_summary_outbox SET state = 'claimed', claim_token = ?, claimed_by = ?, claimed_at = ?, updated_at = ? WHERE summary_id = ?", token, actor, now, now, id); err != nil {
			return err
		}
		e.State, e.ClaimToken, e.ClaimedBy, e.ClaimedAt, e.UpdatedAt = SummaryClaimed, token, actor, now, now
		out = SummaryClaim{Entry: e, Token: token, Operation: operationOf(e)}
		return nil
	})
	return out, err
}

// SummaryReconciliation is what a document says of an entry. Outcome is one of already_written (the document carries exactly this entry's block, alone in the plan's container), absent
// (no block of the plan), stale (the plan's one block is another entry's, an older or a newer one, or this entry's with other text, or the container holds more than the block), duplicate
// (more than one block of the plan) and malformed (a block or container that is not closed, a block outside the container, or two containers).
type SummaryReconciliation struct {
	SummaryID, State, Outcome, Detail string
	// Writable: the entry is open (pending or claimed), so the parent may write it. A superseded or confirmed entry is not.
	Writable bool
	// Again: the entry is the confirmed newest and the document no longer carries it; enqueue --again records the summary anew.
	Again bool
	// Repair is how the entry is written into this document: replace_container (the plan's one container is there: one conditional replacement of its text), initialize (no container:
	// one conditional replacement of the whole document with the empty container appended), manual (the document is malformed and no replacement can repair it: a person does) or
	// none (nothing to write: the block is there, the entry was overtaken, or it is confirmed).
	Repair            string
	Container         string // present or absent
	DocumentSummaryID string
	DocumentSeq       int64
	Relation          string // of the block the document holds to this entry: older, newer, same
	PreviousBlock     string
}

// reconcileDocument judges a document against an entry.
func reconcileDocument(e SummaryEntry, observed string) SummaryReconciliation {
	r := SummaryReconciliation{SummaryID: e.SummaryID, State: e.State, Container: "absent", Writable: e.State == SummaryPending || e.State == SummaryClaimed}
	d := scanSummaryDocument(observed, e.PlanID)
	if d.containerN > 0 {
		r.Container = "present"
	}
	finish := func(outcome, repair, detail string) SummaryReconciliation {
		r.Outcome, r.Repair, r.Detail = outcome, repair, detail
		r.Again = e.State == SummaryConfirmed && outcome != "already_written"
		switch {
		case e.State == SummarySuperseded:
			r.Repair, r.Detail = "none", "this entry is superseded by a newer summary of the plan: do not write it, take the newest entry (dag-summary-status). The document: "+detail
		case r.Again:
			r.Repair, r.Detail = "none", detail+" This entry is confirmed: enqueue --again records the plan's state as a new entry."
		}
		return r
	}
	switch {
	case len(d.problems) > 0:
		return finish("malformed", "manual", strings.Join(d.problems, "; ")+": write nothing, record the failure and ask a person to remove the surplus markers or text so that one container holding at most one block remains")
	case len(d.containers) > 1:
		return finish("malformed", "manual", "the document holds more than one container of this plan: write nothing, record the failure and ask a person to remove all but one")
	}
	for _, b := range d.blocks {
		if b.container < 0 {
			return finish("malformed", "manual", "a summary block of this plan lies outside its container ("+b.id+"): write nothing, record the failure and ask a person to remove it")
		}
	}
	switch len(d.blocks) {
	case 0:
		if r.Container == "absent" {
			return finish("absent", "initialize", "the document has no container of this plan: add the empty container by one conditional replacement of the whole document, read again, then write this entry")
		}
		return finish("absent", "replace_container", "no summary block of this plan is in the document: write this entry by the conditional replacement of the container's current text")
	case 1:
	default:
		return finish("duplicate", "replace_container", "more than one summary block of this plan is in the container, which is not one successful unique record: replace the container's current text with this entry's")
	}
	b := d.blocks[0]
	r.DocumentSummaryID, r.PreviousBlock = b.id, b.text
	r.DocumentSeq, _ = strconv.ParseInt(b.headers["seq"], 10, 64)
	if b.id != e.SummaryID {
		switch {
		case r.DocumentSeq > e.Seq:
			r.Relation = "newer"
			return finish("stale", "none", "the document already holds a newer summary of this plan (#"+strconv.FormatInt(r.DocumentSeq, 10)+"): do not write this entry, take the newest entry")
		case r.DocumentSeq < e.Seq:
			r.Relation = "older"
			return finish("stale", "replace_container", "the document holds an older summary of this plan (#"+strconv.FormatInt(r.DocumentSeq, 10)+"): replace the container's current text with this entry's")
		}
		r.Relation = "same"
		return finish("stale", "replace_container", "the document holds a block of this plan with this sequence number and another identity: replace the container's current text with this entry's")
	}
	if canonText(b.text) != canonText(summaryBlock(e)) {
		r.Relation = "same"
		return finish("stale", "replace_container", "the block in the document is not this entry's block (a header or the text differs): replace the container's current text with this entry's")
	}
	if d.containerBody(b.container) != canonText(b.text) {
		r.Relation = "same"
		return finish("stale", "replace_container", "the container holds more than this entry's block: replace the container's current text with this entry's")
	}
	return finish("already_written", "none", "this entry's block is in the document: confirm it with complete and do not write again")
}

// ReconcileSummary says what a document holds of an entry. It writes nothing, and any entry can be asked about: it describes the document, and says whether the entry may be written.
func (s *Scheduler) ReconcileSummary(ctx context.Context, id, observed string) (SummaryReconciliation, error) {
	var out SummaryReconciliation
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		e, err := loadSummary(txCtx, s.Store.Q(txCtx), id)
		if err != nil {
			return err
		}
		out = reconcileDocument(e, observed)
		return nil
	})
	return out, err
}

// SummaryCompleted is the answer of CompleteSummary.
type SummaryCompleted struct {
	Entry    SummaryEntry
	Replayed bool
}

// CompleteSummary confirms an entry from the document the parent read back after its write: the document must be the entry's, and its plan container must hold this entry's block exactly
// and nothing else. A readback that does not is refused (readback_mismatch) and the entry stays claimed with the problem kept, so the parent repairs and reads again. A confirmation is
// monotonic: completing an entry that is confirmed answers its record and checks nothing more.
func (s *Scheduler) CompleteSummary(ctx context.Context, id, actor, token, document, readback string) (SummaryCompleted, error) {
	var out SummaryCompleted
	var problem string
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		e, err := loadSummary(txCtx, q, id)
		if err != nil {
			return err
		}
		if err := summaryActor(txCtx, q, e.ProjectKey, actor); err != nil {
			return err
		}
		if e.State == SummaryConfirmed {
			out = SummaryCompleted{Entry: e, Replayed: true}
			return nil
		}
		if err := heldBy(e, token); err != nil {
			return err
		}
		if e.Document != document {
			return refuse(contract.RefusalSyncTargetMismatch, "this entry is for the document %q, not %q; validating the right text in the wrong document validates nothing", e.Document, document)
		}
		rec := reconcileDocument(e, readback)
		now := s.now()
		if rec.Outcome != "already_written" {
			problem = rec.Outcome + ": " + rec.Detail
			_, err := q.ExecContext(txCtx, "UPDATE dag_summary_outbox SET last_error = ?, updated_at = ? WHERE summary_id = ?", clip(problem, maxSummaryError), now, id)
			return err
		}
		if _, err := q.ExecContext(txCtx, "UPDATE dag_summary_outbox SET state = 'confirmed', claim_token = NULL, readback = ?, confirmed_at = ?, last_error = NULL, updated_at = ? WHERE summary_id = ?",
			rec.PreviousBlock, now, now, id); err != nil {
			return err
		}
		e.State, e.ClaimToken, e.Readback, e.ConfirmedAt, e.LastError, e.UpdatedAt = SummaryConfirmed, "", rec.PreviousBlock, now, "", now
		out = SummaryCompleted{Entry: e}
		return nil
	})
	if err == nil && problem != "" {
		return SummaryCompleted{}, refuse(contract.RefusalReadbackMismatch, "the readback does not carry this entry's block (%s)", problem)
	}
	return out, err
}

// heldBy is the fence of the claim token: the entry must be claimed and the token the one the last claim issued.
func heldBy(e SummaryEntry, token string) error {
	switch {
	case e.State == SummarySuperseded:
		return refuse(contract.RefusalSyncNotClaimable, "%s (#%d) is superseded by a newer summary of the document; its claim is gone", e.SummaryID, e.Seq)
	case e.State != SummaryClaimed || e.ClaimToken == "" || token != e.ClaimToken:
		return refuse(contract.RefusalSyncNotClaimable, "this claim token is not the one currently held for %s (claim it again)", e.SummaryID)
	}
	return nil
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// FailSummary records that writing or reading back an entry failed: the entry goes back to pending with the failure kept and the claim gone, so the parent claims the same entry again. The
// eighth failure makes it failed, which waits for an explicit retry. Only the holder of the claim can fail an entry, so a failure that arrives after a newer summary overtook it is refused.
func (s *Scheduler) FailSummary(ctx context.Context, id, actor, token, message string) (SummaryEntry, error) {
	var out SummaryEntry
	if strings.TrimSpace(message) == "" || utf8.RuneCountInString(message) > maxSummaryError || !utf8.ValidString(message) {
		return out, refuse(contract.RefusalMalformedReceipt, "a failure names its cause, in at most %d characters", maxSummaryError)
	}
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		e, err := loadSummary(txCtx, q, id)
		if err != nil {
			return err
		}
		if err := summaryActor(txCtx, q, e.ProjectKey, actor); err != nil {
			return err
		}
		if e.State == SummaryConfirmed {
			return refuse(contract.RefusalSyncNotClaimable, "%s is confirmed; a later failure cannot undo it", id)
		}
		if err := heldBy(e, token); err != nil {
			return err
		}
		attempts, state := e.Attempts+1, SummaryPending
		if attempts >= MaxSummaryAttempts {
			state = SummaryFailed
		}
		now := s.now()
		if _, err := q.ExecContext(txCtx, "UPDATE dag_summary_outbox SET state = ?, attempts = ?, last_error = ?, claim_token = NULL, updated_at = ? WHERE summary_id = ?", state, attempts, message, now, id); err != nil {
			return err
		}
		e.State, e.Attempts, e.LastError, e.ClaimToken, e.UpdatedAt = state, attempts, message, "", now
		out = e
		return nil
	})
	return out, err
}

// SummaryRetried is the answer of RetrySummary.
type SummaryRetried struct {
	Entry    SummaryEntry
	Replayed bool
}

// RetrySummary reopens a failed entry (the eighth failure parks it): it goes back to pending with its attempts at zero and the last failure kept. An entry that is open is answered as it
// is; a confirmed or superseded entry is final and is refused. It touches the one entry it names.
func (s *Scheduler) RetrySummary(ctx context.Context, id, actor string) (SummaryRetried, error) {
	var out SummaryRetried
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		e, err := loadSummary(txCtx, q, id)
		if err != nil {
			return err
		}
		if err := summaryActor(txCtx, q, e.ProjectKey, actor); err != nil {
			return err
		}
		switch e.State {
		case SummaryConfirmed, SummarySuperseded:
			return refuse(contract.RefusalSyncNotClaimable, "%s is %s: it is final and is never retried", id, e.State)
		case SummaryPending, SummaryClaimed:
			out = SummaryRetried{Entry: e, Replayed: true}
			return nil
		}
		now := s.now()
		if _, err := q.ExecContext(txCtx, "UPDATE dag_summary_outbox SET state = 'pending', attempts = 0, updated_at = ? WHERE summary_id = ?", now, id); err != nil {
			return err
		}
		e.State, e.Attempts, e.UpdatedAt = SummaryPending, 0, now
		out = SummaryRetried{Entry: e}
		return nil
	})
	return out, err
}
