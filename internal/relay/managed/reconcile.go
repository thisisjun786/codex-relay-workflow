package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// A creation whose answer was lost leaves a bridge receipt that is outcome_unknown (or in_progress_or_unknown) and never runs again. The
// engine does not repeat it: it asks the App Server what the creation left behind and either continues the same request on that thread,
// creates again under the same request once no thread is shown after the grace period, or stops and says why.

// DefaultCreationGrace is how long after a creation's answer was lost a thread that is not listed yet may still appear.
const DefaultCreationGrace = 2 * time.Minute

const (
	// maxCreationAttempts bounds the creations one managed request makes: the original and two after it.
	maxCreationAttempts = 3
	loadedPages         = 20
	loadedPage          = 500
	maxThreadReads      = 64
	// idSlack is how far outside the creation's window a UUIDv7 thread id may be and still be read: an id that says the thread was made
	// long before the creation began, or long after its grace period, is not read at all (a fleet loads hundreds of threads).
	idSlack = time.Minute
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
	rec    reconciliation
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
	d, err := r.decide(ctx)
	if err != nil {
		return nil, err
	}
	switch d.action {
	case "continue":
		r.receipt, r.adopted = adopted(r.receipt, d.thread), true
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

// adopted is the unknown receipt as a creation that reached its thread: the thread is the one observed and thread/start is among the effects,
// which is what the standby recovery asks of a receipt. The receipt the host kept is not changed.
func adopted(receipt map[string]any, thread string) map[string]any {
	out := make(map[string]any, len(receipt)+2)
	for k, v := range receipt {
		out[k] = v
	}
	out["threadId"] = thread
	if _, ok := out["attemptedEffects"].([]any); !ok {
		out["attemptedEffects"] = []any{"thread/start"}
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
		if abandonable(standby) {
			return r.replace(base, thread, "the host refused to resume it"), nil
		}
		if delivery.ValidSegment(thread) {
			base.state, base.thread, base.detail = reconAdopted, thread, "the standby send for this thread was made before"
			return decision{action: "continue", thread: thread, rec: base}, nil
		}
	}
	if thread := pyjson.Text(r.receipt["threadId"]); delivery.ValidSegment(thread) {
		return r.observe(ctx, base, thread)
	}
	return r.scan(ctx, base)
}

// abandonable is a standby recovery the host refused for a reason that cannot get better: it will not resume the thread, or no longer knows it.
// The fields are those the transport saves (transport.go guardedSend): a refused resume leaves status failed, the status read before it, no
// resumed record (a settings finding saves one) and an error that names the method; the transport's own connection_unavailable is not a refusal.
func abandonable(recovery map[string]any) bool {
	if recovery["status"] != "failed" || recovery["turnId"] != nil {
		return false
	}
	text := pyjson.Text(recovery["error"])
	if strings.Contains(text, "thread not found") && (strings.HasPrefix(text, "thread/read:") || strings.HasPrefix(text, "thread/resume:")) {
		return true
	}
	if _, resumed := recovery["resumed"]; resumed || recovery["statusBeforeResume"] != "notLoaded" || !strings.HasPrefix(text, "thread/resume:") {
		return false
	}
	return pyjson.Map(recovery["rpcError"])["code"] != "connection_unavailable"
}

// replace gives a thread up and creates the next attempt, unless the creations are spent.
func (r *startRun) replace(base reconciliation, thread, why string) decision {
	base.thread = thread
	detail := why
	if thread != "" {
		detail = fmt.Sprintf("thread %s abandoned: %s", thread, why)
	}
	if r.attempt+1 >= maxCreationAttempts {
		return base.stopped(reconExhausted, "%s, and %d creations were made already", detail, maxCreationAttempts)
	}
	base.state, base.detail = reconRecreated, detail
	return decision{action: "recreate", rec: base}
}

func hostText(err error) string { return err.Error() }

func gone(err error) bool { return strings.Contains(err.Error(), "thread not found") }

// unreadable is a thread/read the host answers with one of the three refusals for a thread it cannot serve: it does not know the thread, or the
// rollout its state is read from is missing (docs/live-trial.md names them). Such a thread cannot be resumed either.
func unreadable(err error) bool {
	for _, text := range []string{"thread not found", "missing source rollout", "no rollout found"} {
		if strings.Contains(err.Error(), text) {
			return true
		}
	}
	return false
}

// A thread has no turn when the host says it never got its first message, or that its rollout (where turns live) does not exist.
func noTurnAnswer(err error) bool {
	for _, text := range []string{"not materialized", "missing source rollout", "no rollout found"} {
		if strings.Contains(err.Error(), text) {
			return true
		}
	}
	return false
}

// observe reads a thread the creation named or the scan found: whether it has a turn decides, and the receipt decides whether the standby
// turn/start may have been sent. Neither is concluded from the other.
func (r *startRun) observe(ctx context.Context, base reconciliation, thread string) (decision, error) {
	read, err := r.m.Adapter.HostCall(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false})
	if err != nil {
		if ctx.Err() != nil {
			return decision{}, ctx.Err()
		}
		if unreadable(err) {
			return r.replace(base, thread, "the host cannot read it ("+hostText(err)+")"), nil
		}
		return base.stopped(reconUnobserved, "reading thread %s: %s", thread, hostText(err)), nil
	}
	facts := pyjson.Map(read["thread"])
	hasTurn := pyjson.Text(pyjson.Map(facts["status"])["type"]) == "active" || strings.TrimSpace(pyjson.Text(facts["preview"])) != ""
	turns, err := r.m.Adapter.HostCall(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 1, "itemsView": "summary"})
	switch {
	case err == nil:
		rows, _ := turns["data"].([]any)
		hasTurn = hasTurn || len(rows) > 0
	case ctx.Err() != nil:
		return decision{}, ctx.Err()
	case gone(err):
		return r.replace(base, thread, "the host no longer knows it"), nil
	case !noTurnAnswer(err):
		return base.stopped(reconUnobserved, "listing the turns of thread %s: %s", thread, hostText(err)), nil
	}
	base.thread = thread
	if hasTurn {
		return base.stopped(reconHasTurn, "thread %s already has a turn, so it cannot be told from a standby turn this creation sent", thread), nil
	}
	if effects, ok := r.receipt["attemptedEffects"].([]any); ok {
		for _, effect := range effects {
			if effect == "turn/start" {
				return base.stopped(reconTurnUnknown, "the creation sent turn/start and no answer came back (attemptedEffects); it is not sent again (I-473)"), nil
			}
		}
	} else if r.receipt["title"] != nil {
		// The bridge saves the title after thread/name/set and sends turn/start next without another save: a receipt that stopped here may have sent it.
		return base.stopped(reconTurnUnknown, "the receipt saved the title, so turn/start may have been sent before the process stopped; it is not sent again (I-473)"), nil
	}
	base.state, base.detail = reconAdopted, fmt.Sprintf("thread %s exists and has no turn", thread)
	return decision{action: "continue", thread: thread, rec: base}, nil
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

// uuidV7Millis is the creation time (milliseconds) a UUIDv7 thread id carries; ok is false for any other id.
func uuidV7Millis(id string) (int64, bool) {
	digits := strings.ReplaceAll(id, "-", "")
	if len(digits) != 32 || digits[12] != '7' {
		return 0, false
	}
	ms, err := strconv.ParseInt(digits[:12], 16, 64)
	return ms, err == nil
}

// earlierThreads are the threads attempts before the current one named, in their creation or their standby recovery receipt: they were given up
// (or are another attempt's) and no scan adopts them.
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

// scan looks for the thread a creation left when its receipt names none: the loaded threads that fit the creation. None is absence only
// once the listing was read to its end and the grace period has passed.
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
			return base.stopped(reconUnobserved, "listing the loaded threads: %s", hostText(err)), nil
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
				return base.stopped(reconUnobserved, "reading thread %s: %s", id, hostText(err)), nil
			}
			th := pyjson.Map(read["thread"])
			created, ok := epoch(th["createdAt"])
			name, model, effort := pyjson.Text(th["name"]), pyjson.Text(th["model"]), pyjson.Text(th["reasoningEffort"])
			if cwds[pyjson.Text(th["cwd"])] && ok && created >= math.Floor(started)-2 && created <= ended+r.m.grace().Seconds() && th["ephemeral"] != true && pyjson.Text(th["parentThreadId"]) == "" &&
				strings.TrimSpace(pyjson.Text(th["preview"])) == "" && (name == "" || name == pyjson.Text(child["title"])) &&
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
		return r.observe(ctx, base, matches[0])
	case 0:
		after := time.Unix(int64(ended), 0).Add(r.m.grace())
		if now.Before(after) {
			base.state, base.repeatAfter = reconPending, after.UTC().Format(time.RFC3339)
			base.detail = "no thread is listed yet; the creation's answer was lost and a thread may still appear"
			return decision{action: "stop", rec: base}, nil
		}
		return r.replace(base, "", "no thread was shown after the grace period"), nil
	}
	return base.stopped(reconAmbiguous, "%d threads fit the creation (%s); the engine adopts none of them", len(matches), strings.Join(matches, ", ")), nil
}
