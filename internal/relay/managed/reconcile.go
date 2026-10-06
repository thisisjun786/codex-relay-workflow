package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// A creation whose answer was lost leaves a bridge receipt that is outcome_unknown (or in_progress_or_unknown) and never runs again. The engine asks the
// App Server what the creation left: it continues the same request on that thread, creates again under the same request once no thread is shown after the
// grace period, or stops and says why.

// DefaultCreationGrace is how long after a creation's answer was lost a thread that is not listed yet may still appear.
const DefaultCreationGrace = 2 * time.Minute

const (
	// maxCreationAttempts bounds the creations one managed request makes: the original and two after it.
	maxCreationAttempts = 3
	loadedPages         = 20
	loadedPage          = 500
	maxThreadReads      = 64
	idSlack             = time.Minute // slack around the UUIDv7 creation window
)

// The states creationReconciliation reports.
const (
	reconAdopted     = "adopted"
	reconRecreated   = "recreated"
	reconPending     = "pending"
	reconUnobserved  = "unobservable"
	reconAmbiguous   = "ambiguous"
	reconHasTurn     = "thread_has_turn"
	reconTurnUnknown = "standby_turn_unknown"
	reconExhausted   = "attempts_exhausted"
)

// reconciliation says what the engine did about a creation whose outcome the receipt does not give, or why it did nothing.
type reconciliation struct {
	state, detail, thread, attemptID, repeatAfter string
	attempt                                       int
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (c reconciliation) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "state", Value: c.state}, {Key: "detail", Value: c.detail}, {Key: "attempt", Value: c.attempt}, {Key: "attemptRequestId", Value: c.attemptID},
		{Key: "thread", Value: nullIfEmpty(c.thread)}, {Key: "repeatAfter", Value: nullIfEmpty(c.repeatAfter)}}
}

func (c reconciliation) stopped(state, format string, args ...any) decision {
	c.state, c.detail = state, fmt.Sprintf(format, args...)
	return decision{action: "stop", rec: c}
}

// decision is what reconciliation concluded: continue on a thread, create again, or stop.
type decision struct {
	action string // "continue", "recreate" or "stop"
	thread string
	// turn is the standby turn the decision recognised on that thread (reconcile.go observe); empty
	// when the thread's turn, if any, was not recognised as this creation's.
	turn string
	rec  reconciliation
}

// attemptID is the bridge operation id of creation attempt n of a request: attempt 0 is the request's own id, and each later one derives from
// the request and n, so the ledger never replays an earlier attempt's receipt for it.
func attemptID(requestID, first string, n int) string {
	if n == 0 {
		return first
	}
	sum := sha256.Sum256([]byte(requestID + "\x00recreate\x00" + strconv.Itoa(n)))
	return "managed-create-" + hex.EncodeToString(sum[:])
}

// recoveryID is the operation id of attempt n's standby recovery; attempt 0 keeps the id the failed-creation recovery always used.
func recoveryID(requestID string, n int) string {
	if n > 0 {
		requestID += "\x00recreate\x00" + strconv.Itoa(n)
	}
	sum := sha256.Sum256([]byte(requestID))
	return "managed-standby-" + hex.EncodeToString(sum[:])
}

// unknownCreation is a creation receipt that says neither that the host did it nor that it did not.
func unknownCreation(receipt map[string]any) bool {
	if receipt == nil {
		return false
	}
	switch receipt["status"] {
	case "accepted", "failed", "not_attempted":
		return false
	}
	return true
}

func (m *Start) grace() time.Duration {
	if m.CreationGrace > 0 {
		return m.CreationGrace
	}
	return DefaultCreationGrace
}

// currentCreation is the newest creation attempt the host holds a receipt for. Attempt 0 is the request's own operation; each later one was made
// after the one before it was shown to have left no thread, so the newest receipt is the one that counts and no attempt is ever made twice.
func (r *startRun) currentCreation(ctx context.Context) (map[string]any, error) {
	receipt, err := r.m.Adapter.GetOperation(ctx, r.identity.CreateRequestID)
	if err != nil {
		return nil, err
	}
	r.attempt = 0
	// Only an unknown attempt can have a successor, so a receipt that says what the host did is asked about nothing more.
	for unknownCreation(receipt) && r.attempt+1 < maxCreationAttempts {
		next, err := r.m.Adapter.GetOperation(ctx, attemptID(r.identity.RequestID, r.identity.CreateRequestID, r.attempt+1))
		if err != nil {
			return nil, err
		}
		if next == nil {
			break
		}
		receipt, r.attempt = next, r.attempt+1
	}
	return receipt, nil
}

// attemptIdentity is the request's identity with the operation id of the current attempt.
func (r *startRun) attemptIdentity() Identity {
	id := r.identity
	id.CreateRequestID = attemptID(r.identity.RequestID, r.identity.CreateRequestID, r.attempt)
	return id
}

// reconcileCreation settles a creation whose outcome is unknown by what the App Server shows, and goes on with the thread it found or the
// creation it made, or leaves the receipt unknown for incompleteCreation to answer with the reason.
func (r *startRun) reconcileCreation(ctx context.Context) (contract.OrderedObject, error) {
	if !unknownCreation(r.receipt) {
		return nil, nil
	}
	// A repeat whose reservation already recorded the accepted child and standby turn adopts that
	// recorded identity and never reads the host: the identity was published by a run that observed
	// it, and a host that has moved on since (the business turn beside the standby turn, or the
	// standby turn gone from a shorter listing) must not talk the engine out of it.
	if child, standby, ok := r.reservationRecordedIdentity(); ok {
		base := reconciliation{attempt: r.attempt, attemptID: r.attemptIdentity().CreateRequestID}
		base.state, base.thread = reconAdopted, child
		base.detail = fmt.Sprintf("the reservation recorded standby turn %s for thread %s", standby, child)
		r.receipt, r.adopted = adopted(r.receipt, child, standby), true
		r.reconciled = &base
		return nil, nil
	}
	d, err := r.decide(ctx)
	if err != nil {
		return nil, err
	}
	switch d.action {
	case "continue":
		if r.row.ReceiptStatus.String != "accepted" {
			if err := r.holdProject(ctx); err != nil {
				return nil, err
			}
		}
		r.receipt, r.adopted = adopted(r.receipt, d.thread, d.turn), true
	case "recreate":
		r.projectLock()
		r.attempt++
		d.rec.attempt, d.rec.attemptID = r.attempt, r.attemptIdentity().CreateRequestID
		r.reconciled = &d.rec
		var notReady string
		r.receipt, notReady, r.projectLock, err = r.m.create(ctx, r.attemptIdentity(), r.req, r.ledger)
		if err != nil {
			return nil, err
		}
		if notReady != "" {
			return r.answer(ctx, "refused", "creation", notReady)
		}
		return nil, nil
	}
	r.reconciled = &d.rec
	return nil, nil
}

// holdProject takes the project's lock and asks the project-scope decision again before a thread the creation left is sent to: the lock the creation
// held was let go when the start that made it stopped, and the parent binding may have moved since. The lock is kept until register has registered the child.
func (r *startRun) holdProject(ctx context.Context) error {
	r.projectLock()
	if project := pyjson.Text(r.req["projectKey"]); project != "" {
		held, err := LockProject(ctx, r.m.Store.Path, project)
		if err != nil {
			return err
		}
		var once sync.Once
		r.projectLock = func() { once.Do(func() { _ = held() }) }
	}
	return r.m.scopeRefusal(ctx, r.attemptIdentity(), r.req)
}

// reservationRecordedIdentity is the child and standby turn the reservation already published for this
// creation, when it published them for the very thread the unknown receipt names.
//
// Reservation.Receipt records an accepted row only with a non-blank child and turn
// (reservation.go), so a row that says accepted always carries both; the equality against the
// receipt's own thread is what keeps the recorded identity from being adopted for a thread the
// creation did not leave (a receipt that names none, or a newer attempt whose receipt names
// another thread, falls through to decide() unchanged).
func (r *startRun) reservationRecordedIdentity() (string, string, bool) {
	child, standby := r.row.ChildTaskID.String, r.row.StandbyTurnID.String
	receiptThread := pyjson.Text(r.receipt["threadId"])
	if !r.row.ReceiptStatus.Valid || r.row.ReceiptStatus.String != "accepted" || !delivery.ValidSegment(child) || !delivery.ValidSegment(standby) || !delivery.ValidSegment(receiptThread) || child != receiptThread {
		return "", "", false
	}
	return child, standby, true
}

// adopted is the unknown receipt as a creation that reached its thread: the thread is the one observed and thread/start is among the effects,
// which is what the standby recovery asks of a receipt. The receipt the host kept is not changed.
//
// turn is the standby turn this creation sent, when observe() recognised it on the thread. A receipt that carries no turnId is one the standby
// recovery would send to, so the recognised turn is recorded exactly as the standby recovery records the turn it sent: creationStatus keeps the
// unknown status, the receipt becomes accepted, and turnId names the turn. Nothing else would make the receipt accepted - recoverStandby returns
// early on a turnId - and without an accepted receipt verifyCreation cannot take the child's identity from it, so the recognised turn could never
// become the standbyTurnId the reservation records.
func adopted(receipt map[string]any, thread, turn string) map[string]any {
	out := make(map[string]any, len(receipt)+2)
	for k, v := range receipt {
		out[k] = v
	}
	out["threadId"] = thread
	if _, ok := out["attemptedEffects"].([]any); !ok {
		out["attemptedEffects"] = []any{"thread/start"}
	}
	if turn != "" && out["turnId"] == nil {
		out["creationStatus"] = receipt["status"]
		out["status"] = "accepted"
		out["turnId"] = turn
	}
	return out
}

func (r *startRun) decide(ctx context.Context) (decision, error) {
	base := reconciliation{attempt: r.attempt, attemptID: r.attemptIdentity().CreateRequestID}
	standby, err := r.m.Adapter.GetOperation(ctx, recoveryID(r.identity.RequestID, r.attempt))
	if err != nil {
		return decision{}, err
	}
	if standby != nil {
		// The standby send was decided before: it only names the thread, and what to do about it is recoverStandby's rule for its status.
		thread := pyjson.Text(standby["threadId"])
		if abandonable(standby) && !creationMayHaveTurn(r.receipt) {
			return r.abandonOrphan(ctx, base, thread, pyjson.Text(standby["error"]))
		}
		if delivery.ValidSegment(thread) {
			base.state, base.thread, base.detail = reconAdopted, thread, "the standby send for this thread was made before"
			return decision{action: "continue", thread: thread, rec: base}, nil
		}
	}
	if thread := pyjson.Text(r.receipt["threadId"]); delivery.ValidSegment(thread) {
		return r.observe(ctx, base, thread, true)
	}
	return r.scan(ctx, base)
}

// A failed read/resume, before any turn or verified resume, can leave an orphan.
// Connection establishment, settings findings and uncertain sends do not license replacement.
func abandonable(receipt map[string]any) bool {
	if receipt["status"] != "failed" || receipt["turnId"] != nil {
		return false
	}
	if _, resumed := receipt["resumed"]; resumed || pyjson.Map(receipt["rpcError"])["code"] == "connection_unavailable" {
		return false
	}
	err := pyjson.Text(receipt["error"])
	if unreadableText(err) && (strings.HasPrefix(err, "thread/read:") || strings.HasPrefix(err, "thread/resume:")) {
		return true
	}
	return receipt["statusBeforeResume"] == "notLoaded" && strings.HasPrefix(err, "thread/resume:")
}

func (r *startRun) abandon(base reconciliation, thread, why string) decision {
	base.thread = thread
	return r.recreate(base, fmt.Sprintf("thread %s abandoned: %s", thread, why))
}

// abandonOrphan is the standby receipt's abandonment: the host refused to read or resume a thread
// that has no turn, so the thread is archived once before the next attempt is created and then
// abandoned under the same request. Only this path archives; observe()'s abandonment is unchanged.
func (r *startRun) abandonOrphan(ctx context.Context, base reconciliation, thread, errText string) (decision, error) {
	if err := r.archiveOrphan(ctx, thread, errText); err != nil {
		var inconclusive *orphanReadInconclusive
		if errors.As(err, &inconclusive) {
			// The orphan could not be read again, so nothing is decided about it: this call stops
			// with the existing unobservable state and creates nothing. A repeat decides again.
			return base.stopped(reconUnobserved, "%s", inconclusive.Error()), nil
		}
		return decision{}, err
	}
	return r.abandon(base, thread, "the host refused to resume it"), nil
}

// orphanReadInconclusive is an orphan re-read that ended without a conclusion: the host answered
// with an error that is neither a cancellation nor one of the texts that say it does not hold the
// thread. Nothing is decided from it, so the call stops rather than creating the next attempt, and
// the detail says what the host answered.
type orphanReadInconclusive struct {
	thread string
	err    error
}

func (e *orphanReadInconclusive) Error() string {
	return fmt.Sprintf("the orphan %s could not be read again before its archive: %s; nothing was created; a repeat decides again", e.thread, e.err)
}

// archiveOrphan tries thread/archive once on the orphan and records the attempt in one
// managed_orphan_archive row, the result in a second: a row already written for this request and
// attempt stops a second archive, as the resend unload's row does. An error naming a thread the
// host does not hold is not attempted at all.
//
// thread/archive unloads an active thread and the sub-threads under it, so the orphan is read again
// immediately before the call, as the resend unload reads the child it lowers, and the readiness
// policy and the ledger are asked again with it because the archive is a host effect. A thread that
// has become active is left alone, and an archive that fails or is withheld is recorded and never
// stops the recreation.
//
// The attempt is marked in its own row before the host effect, on a context the caller's
// cancellation cannot take away, and the result is recorded after the reply. The mark is what makes
// an archive that was sent and never answered leave a durable trace, so the next repeat neither
// archives the orphan again nor loses the attempt.
func (r *startRun) archiveOrphan(ctx context.Context, thread, errText string) error {
	archived, err := r.orphanArchived(ctx)
	if err != nil {
		return err
	}
	if archived {
		return nil
	}
	detail := map[string]any{"attempt": r.attempt, "thread": thread, "error": ""}
	switch {
	case strings.Contains(errText, "thread not found"):
		// The host does not hold the thread, so there is nothing to archive.
		detail["archive"] = "skipped"
	default:
		turnless, why, err := r.orphanStillTurnless(ctx, thread)
		if err != nil {
			return err
		}
		if !turnless {
			detail["archive"], detail["error"] = "skipped", why
			break
		}
		readiness, err := r.m.ready(ctx, r.req)
		if err != nil {
			return err
		}
		if readiness != "" {
			// The policy withholds every host effect, so this one is not taken and no row is
			// written: a later repeat under a ready policy archives the orphan.
			return nil
		}
		if err := r.m.Adapter.RequireLedger(ctx, r.ledger); err != nil {
			return err
		}
		// The attempt is marked before the host effect, on a context the caller's cancellation
		// cannot take away, so an archive that was sent and never answered leaves the mark behind
		// and the next repeat does not send thread/archive a second time.
		mark := map[string]any{"attempt": r.attempt, "thread": thread, "error": "", "archive": "attempting"}
		if err := r.recordOrphanArchive(context.WithoutCancel(ctx), mark); err != nil {
			return err
		}
		if _, err := r.m.Adapter.HostCall(ctx, "thread/archive", map[string]any{"threadId": thread}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			detail["archive"], detail["error"] = "failed", err.Error()
		} else {
			detail["archive"] = "ok"
		}
	}
	// The orphan is archived either way, so the row is written on a context the caller's
	// cancellation cannot take away, as the resend unload's row is.
	return r.recordOrphanArchive(context.WithoutCancel(ctx), detail)
}

// orphanStillTurnless reads the orphan immediately before the archive. A thread the host still
// cannot read has no rollout and no turn; a thread that now carries a turn, or that the host
// reports active, has become someone's work and is left alone. An inconclusive read answers with
// orphanReadInconclusive, and the caller then stops the call rather than creating the next attempt.
func (r *startRun) orphanStillTurnless(ctx context.Context, thread string) (bool, string, error) {
	read, err := r.m.Adapter.HostCall(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false})
	if err != nil {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		if unreadable(err) {
			return true, "", nil
		}
		return false, "", &orphanReadInconclusive{thread: thread, err: err}
	}
	facts := pyjson.Map(read["thread"])
	if pyjson.Text(pyjson.Map(facts["status"])["type"]) == "active" || strings.TrimSpace(pyjson.Text(facts["preview"])) != "" {
		return false, "thread/read: the thread has become active", nil
	}
	turns, err := r.m.Adapter.HostCall(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 1, "itemsView": "summary"})
	if err != nil {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		if noTurnAnswer(err) {
			return true, "", nil
		}
		return false, "", &orphanReadInconclusive{thread: thread, err: err}
	}
	if rows, _ := turns["data"].([]any); len(rows) > 0 {
		return false, "thread/turns/list: the thread has a turn", nil
	}
	return true, "", nil
}

// recordOrphanArchive writes the one managed_orphan_archive row an abandoned orphan leaves.
func (r *startRun) recordOrphanArchive(ctx context.Context, detail map[string]any) error {
	encoded, err := compactPythonJSON(detail)
	if err != nil {
		return err
	}
	_, err = r.m.Store.Querier(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", r.m.now(), "managed_orphan_archive", r.identity.RequestID, string(encoded))
	return err
}

// orphanArchived reports whether this request and attempt already archived its orphan, read from
// the row the archive wrote. The detail is the compact, key-sorted object recordOrphanArchive
// writes, so the attempt is the field "attempt":<n> followed by its separator.
func (r *startRun) orphanArchived(ctx context.Context) (bool, error) {
	var rows int
	err := r.m.Store.Querier(ctx).QueryRowContext(ctx, "SELECT COUNT(*) FROM journal WHERE kind='managed_orphan_archive' AND subject=? AND detail LIKE ?", r.identity.RequestID, `%"attempt":`+strconv.Itoa(r.attempt)+`,%`).Scan(&rows)
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func creationMayHaveTurn(receipt map[string]any) bool {
	effects, recorded := receipt["attemptedEffects"].([]any)
	for _, effect := range effects {
		if effect == "turn/start" {
			return true
		}
	}
	return receipt["turnId"] != nil || !recorded && receipt["title"] != nil
}

// unreadableText is the one list of texts the host answers a read or a resume with when the thread
// it names has no rollout to read. observe() reads them as an unreadable thread; abandonable()
// reads the same texts on a standby receipt, so one helper serves both.
func unreadableText(text string) bool {
	for _, known := range []string{"thread not found", "missing source rollout", "no rollout found"} {
		if strings.Contains(text, known) {
			return true
		}
	}
	return false
}

func unreadable(err error) bool {
	return unreadableText(err.Error())
}

// recreate creates the next attempt, unless the creations are spent.
func (r *startRun) recreate(base reconciliation, why string) decision {
	if r.attempt+1 >= maxCreationAttempts {
		return base.stopped(reconExhausted, "%s, and %d creations were made already", why, maxCreationAttempts)
	}
	base.state, base.detail = reconRecreated, why
	return decision{action: "recreate", rec: base}
}

// A thread has no turn when the host says it never got its first message (turns/list refuses it before the first user message).
func noTurnAnswer(err error) bool {
	return strings.Contains(err.Error(), "not materialized") || strings.Contains(err.Error(), "missing source rollout") || strings.Contains(err.Error(), "no rollout found")
}

// observe reads a thread the creation named or the scan found: whether it has a turn decides, and the receipt decides whether the standby
// turn/start may have been sent. Neither is concluded from the other.
//
// fromReceipt says the thread is the one the creation receipt itself named, not a thread the scan found. Only then can a turn the host lists
// be this creation's standby turn, so only then is it recognised (standbyTurnDecision).
func (r *startRun) observe(ctx context.Context, base reconciliation, thread string, fromReceipt bool) (decision, error) {
	read, err := r.m.Adapter.HostCall(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false})
	if err != nil {
		if ctx.Err() != nil {
			return decision{}, ctx.Err()
		}
		if unreadable(err) {
			if creationMayHaveTurn(r.receipt) {
				return base.stopped(reconTurnUnknown, "the creation may have sent turn/start; an unreadable thread is not replaced (I-473)"), nil
			}
			return r.abandon(base, thread, "the host cannot read it ("+err.Error()+")"), nil
		}
		return base.stopped(reconUnobserved, "reading thread %s: %s", thread, err), nil
	}
	facts := pyjson.Map(read["thread"])
	hasTurn := pyjson.Text(pyjson.Map(facts["status"])["type"]) == "active" || strings.TrimSpace(pyjson.Text(facts["preview"])) != ""
	// Two turns are asked for rather than one: whether the thread has a turn is still read from the first, and a thread that holds exactly
	// the standby turn this creation sent is told from one that holds more (standbyTurnDecision). One call serves both readings.
	turns, err := r.m.Adapter.HostCall(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 2, "itemsView": "summary"})
	var rows []any
	var nextPage string
	switch {
	case err == nil:
		rows, _ = turns["data"].([]any)
		nextPage = pyjson.Text(turns["nextCursor"])
		hasTurn = hasTurn || len(rows) > 0
	case ctx.Err() != nil:
		return decision{}, ctx.Err()
	case strings.Contains(err.Error(), "thread not found") && !hasTurn && !creationMayHaveTurn(r.receipt):
		return r.abandon(base, thread, "the host no longer knows it"), nil
	case !noTurnAnswer(err):
		return base.stopped(reconUnobserved, "listing the turns of thread %s: %s", thread, err), nil
	}
	base.thread = thread
	if hasTurn {
		if fromReceipt {
			// The receipt cannot say whether its turn/start reached the host, and the thread shows a turn. The host's history decides whether
			// that turn is the standby this creation sent; when it is, the creation continues on it exactly as a receipt that named its turnId.
			if recognised, ok := r.standbyTurnDecision(base, thread, rows, nextPage); ok {
				return recognised, nil
			}
		}
		return base.stopped(reconHasTurn, "thread %s already has a turn, so it cannot be told from a standby turn this creation sent", thread), nil
	}
	if _, recorded := r.receipt["attemptedEffects"].([]any); recorded {
		if attemptedTurnStart(r.receipt) {
			return base.stopped(reconTurnUnknown, "the creation sent turn/start and no answer came back (attemptedEffects); it is not sent again (I-473)"), nil
		}
	} else if r.receipt["title"] != nil {
		// The bridge saves the title after thread/name/set and sends turn/start next without another save: a receipt that stopped here may have sent it.
		return base.stopped(reconTurnUnknown, "the receipt saved the title, so turn/start may have been sent before the process stopped; it is not sent again (I-473)"), nil
	}
	base.state, base.detail = reconAdopted, fmt.Sprintf("thread %s exists and has no turn", thread)
	return decision{action: "continue", thread: thread, rec: base}, nil
}

// attemptedTurnStart reports whether the creation's receipt recorded turn/start among the effects it tried. A receipt that kept no list has
// not recorded it, which is what observe() reads as the title-only case.
func attemptedTurnStart(receipt map[string]any) bool {
	effects, ok := receipt["attemptedEffects"].([]any)
	if !ok {
		return false
	}
	for _, effect := range effects {
		if effect == "turn/start" {
			return true
		}
	}
	return false
}

// standbyTurnDecision recognises the standby turn this creation sent on a thread the receipt names, under every condition at once:
//
//   - the receipt recorded turn/start among the effects it tried;
//   - thread/turns/list (limit 2, itemsView summary) answers exactly one turn and no next page, so the thread holds that turn and nothing else;
//   - that turn's first user message text is byte for byte the bootstrap this creation sends.
//
// A completed turn then continues exactly as a creation receipt that carried its turnId does: reconAdopted, with the turn recorded as the
// standby turn (adopted). An inProgress turn is reconPending, re-read after the same repeatAfter a thread that has not shown up yet is.
// Everything else keeps observe()'s existing answer - two or more turns, other text, failed or interrupted, input it cannot read, a listing
// error - and turn/start is never sent again (I-473): recognising a turn reads it, it sends nothing.
func (r *startRun) standbyTurnDecision(base reconciliation, thread string, rows []any, nextPage string) (decision, bool) {
	if !attemptedTurnStart(r.receipt) {
		return decision{}, false
	}
	if len(rows) != 1 || nextPage != "" {
		return decision{}, false
	}
	row := pyjson.Map(rows[0])
	turn := pyjson.Text(row["id"])
	if turn == "" || standbyTurnInput(row) != bootstrap {
		return decision{}, false
	}
	switch pyjson.Text(row["status"]) {
	case "completed":
		base.state, base.detail = reconAdopted, fmt.Sprintf("thread %s holds the standby turn %s this creation sent", thread, turn)
		return decision{action: "continue", thread: thread, turn: turn, rec: base}, true
	case "inProgress":
		after, ok := r.standbyRecheck()
		if !ok {
			// The engine's clock does not read as a time, so there is no window to name; the turn keeps today's answer.
			return decision{}, false
		}
		base.state, base.repeatAfter = reconPending, after
		base.detail = fmt.Sprintf("thread %s holds the standby turn %s this creation sent, still in progress", thread, turn)
		return decision{action: "stop", rec: base}, true
	}
	return decision{}, false
}

// standbyTurnInput is the text of a summary turn's first user message, and only when that message
// is the whole of what this creation sent: the standby is recognised on the message, not on a
// fragment of it, so a message that carries anything besides the bootstrap is not it.
//
// A summary item may state the text directly on the item or nest it in content, and the bridge's
// own reads accept both (delivery.itemText reads an item's text first). Either way the whole
// message has to be one text part: an item's own text is read only when the content beside it is
// absent, empty, or that same single text part, and a message without its own text is read only
// when the content is exactly one text part. The part's other keys are the host's (text_elements
// and the like) and are not read. Two or more parts, a part that is not text, or an item text that
// differs from the content answer "", which never equals the bootstrap.
func standbyTurnInput(row map[string]any) string {
	items, _ := row["items"].([]any)
	for _, item := range items {
		message := pyjson.Map(item)
		if pyjson.Text(message["type"]) != "userMessage" {
			continue
		}
		return standbyMessageText(message)
	}
	return ""
}

// standbyMessageText is one user message's standby text, or "" when the message is not the
// bootstrap alone (standbyTurnInput).
func standbyMessageText(message map[string]any) string {
	parts := standbyMessageParts(message)
	if text := pyjson.Text(message["text"]); text != "" {
		if len(parts) == 0 {
			return text
		}
		if len(parts) == 1 && pyjson.Text(parts[0]["type"]) == "text" && pyjson.Text(parts[0]["text"]) == text {
			return text
		}
		return ""
	}
	if len(parts) == 1 && pyjson.Text(parts[0]["type"]) == "text" {
		return pyjson.Text(parts[0]["text"])
	}
	return ""
}

// standbyMessageParts is a message's content as a list of parts; a message that carries none reads as no
// parts, whatever shape the host left behind.
func standbyMessageParts(message map[string]any) []map[string]any {
	content, _ := message["content"].([]any)
	parts := make([]map[string]any, 0, len(content))
	for _, part := range content {
		parts = append(parts, pyjson.Map(part))
	}
	return parts
}

// standbyRecheck is when a recognised standby turn that is still in progress is looked at again: the grace period after the later of the
// receipt's own creation times, which is the window the scan's pending answer gives a thread that has not shown up yet. That instant can
// already be past when a repeat reaches a creation made long before, and a past instant would read as "look again now" and spend a repeat
// on a turn that is still running, so the window is measured from the engine's clock when the creation's own has closed. A receipt that
// records no creation time still gets the clock's window; a clock that does not read as a time gives none.
func (r *startRun) standbyRecheck() (string, bool) {
	now, err := time.Parse(time.RFC3339Nano, r.m.now())
	if err != nil {
		return "", false
	}
	at := now.Add(r.m.grace())
	started, hasStart := epoch(r.receipt["startedAt"])
	updated, hasUpdate := epoch(r.receipt["updatedAt"])
	if hasStart || hasUpdate {
		if !hasStart {
			started = updated
		}
		if !hasUpdate {
			updated = started
		}
		if later := time.Unix(int64(math.Max(started, updated)), 0).Add(r.m.grace()); later.After(at) {
			at = later
		}
	}
	return at.UTC().Format(time.RFC3339), true
}

func uuidV7Millis(id string) (int64, bool) {
	digits := strings.ReplaceAll(id, "-", "")
	if len(digits) != 32 || digits[12] != '7' {
		return 0, false
	}
	bytes, err := hex.DecodeString(digits)
	if err != nil || bytes[8]&0xc0 != 0x80 {
		return 0, false
	}
	ms, err := strconv.ParseInt(digits[:12], 16, 64)
	return ms, err == nil
}

func (r *startRun) earlierThreads(ctx context.Context) (map[string]bool, error) {
	known := map[string]bool{}
	for n := 0; n < r.attempt; n++ {
		for _, id := range []string{attemptID(r.identity.RequestID, r.identity.CreateRequestID, n), recoveryID(r.identity.RequestID, n)} {
			receipt, err := r.m.Adapter.GetOperation(ctx, id)
			if err != nil {
				return nil, err
			}
			if thread := pyjson.Text(receipt["threadId"]); thread != "" {
				known[thread] = true
			}
		}
	}
	return known, nil
}

// epoch reads a time the ledger or the host wrote as seconds.
func epoch(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// scan looks for the thread a creation left when its receipt names none: the loaded threads that fit the creation. None is absence only
// once the listing was read to its end and the grace period has passed. UUIDv7 IDs outside the window and earlier attempts are not read.
func (r *startRun) scan(ctx context.Context, base reconciliation) (decision, error) {
	started, hasStart := epoch(r.receipt["startedAt"])
	updated, hasUpdate := epoch(r.receipt["updatedAt"])
	switch {
	case !hasStart && !hasUpdate:
		return base.stopped(reconUnobserved, "the receipt records no creation time, so a thread cannot be matched to it"), nil
	case !hasStart:
		started = updated
	case !hasUpdate:
		updated = started
	}
	ended := math.Max(started, updated)
	now, err := time.Parse(time.RFC3339Nano, r.m.now())
	if err != nil {
		return base.stopped(reconUnobserved, "the engine's clock %q does not read as a time", r.m.now()), nil
	}
	known, err := r.earlierThreads(ctx)
	if err != nil {
		return decision{}, err
	}
	child := pyjson.Map(r.req["child"])
	settings := pyjson.Map(child["settings"])
	cwds := map[string]bool{pyjson.Text(settings["cwd"]): true}
	if resolved, err := filepath.EvalSymlinks(pyjson.Text(settings["cwd"])); err == nil {
		cwds[resolved] = true
	}
	var matches []string
	reads, exhausted, cursor := 0, false, ""
	for page := 0; page < loadedPages && !exhausted; page++ {
		params := map[string]any{"limit": loadedPage}
		if cursor != "" {
			params["cursor"] = cursor
		}
		listing, err := r.m.Adapter.HostCall(ctx, "thread/loaded/list", params)
		if err != nil {
			if ctx.Err() != nil {
				return decision{}, ctx.Err()
			}
			return base.stopped(reconUnobserved, "listing the loaded threads: %s", err), nil
		}
		ids, _ := listing["data"].([]any)
		for _, item := range ids {
			id := pyjson.Text(item)
			if id == "" || known[id] {
				continue
			}
			if ms, ok := uuidV7Millis(id); ok && (ms < int64(started*1000)-idSlack.Milliseconds() || ms > int64((ended+r.m.grace().Seconds())*1000)+idSlack.Milliseconds()) {
				continue
			}
			if reads++; reads > maxThreadReads {
				return base.stopped(reconUnobserved, "more than %d loaded threads to inspect", maxThreadReads), nil
			}
			read, err := r.m.Adapter.HostCall(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
			if err != nil {
				if ctx.Err() != nil {
					return decision{}, ctx.Err()
				}
				if unreadable(err) {
					continue
				}
				return base.stopped(reconUnobserved, "reading thread %s: %s", id, err), nil
			}
			th := pyjson.Map(read["thread"])
			created, ok := epoch(th["createdAt"])
			name, model, effort := pyjson.Text(th["name"]), pyjson.Text(th["model"]), pyjson.Text(th["reasoningEffort"])
			// The creation set the host name to childTitle's answer, so a thread this engine named carries the
			// normalized title; the raw title stays accepted so a thread an older build named is still adopted.
			title := pyjson.Text(child["title"])
			if cwds[pyjson.Text(th["cwd"])] && ok && created >= math.Floor(started)-2 && created <= ended+r.m.grace().Seconds() && th["ephemeral"] != true && pyjson.Text(th["parentThreadId"]) == "" &&
				strings.TrimSpace(pyjson.Text(th["preview"])) == "" && (name == "" || name == title || name == childTitle(r.identity.IssueKey, title)) &&
				(model == "" || model == pyjson.Text(settings["model"])) && (effort == "" || effort == pyjson.Text(settings["reasoningEffort"])) {
				matches = append(matches, id)
			}
		}
		cursor = pyjson.Text(listing["nextCursor"])
		exhausted = cursor == ""
	}
	if !exhausted {
		return base.stopped(reconUnobserved, "the loaded listing was not read to its end (%d pages)", loadedPages), nil
	}
	switch len(matches) {
	case 1:
		return r.observe(ctx, base, matches[0], false)
	case 0:
		after := time.Unix(int64(ended), 0).Add(r.m.grace())
		if now.Before(after) {
			base.state, base.repeatAfter = reconPending, after.UTC().Format(time.RFC3339)
			base.detail = "no thread is listed yet; the creation's answer was lost and a thread may still appear"
			return decision{action: "stop", rec: base}, nil
		}
		return r.recreate(base, "no thread was shown after the grace period"), nil
	}
	return base.stopped(reconAmbiguous, "%d threads fit the creation (%s); the engine adopts none of them", len(matches), strings.Join(matches, ", ")), nil
}
