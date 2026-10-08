package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The parent notification queue: send-parent --queue writes one notice per file under
// <state_dir>/parent-queue/<thread>/, and the pump delivers them. A notice never opens a turn of
// its own: it is steered into the parent's active turn, or sent with the parent role once the
// oldest has waited max_queue_seconds, so an idle parent still receives it.
const (
	pumpQueueDir             = "parent-queue"
	pumpSentDir              = "sent"
	pumpReview776OversizeDir = "oversize"
	// pumpReview776AsideDir holds the notices a move has taken out of the queue but not yet
	// published. It is a directory of its own because a notice is always written as
	// <logical id>.txt directly in the thread directory: the producer can create neither this
	// directory nor a file inside it, so nothing it writes can be mistaken for a move's aside.
	pumpReview776AsideDir = ".reclaim"
	pumpQueueWait         = "queued"
)

const (
	// pumpQueueRefusedDir holds the notices of a batch the bridge refused pumpQueueRefusalLimit times
	// under one batch id. They stay in the thread directory, out of the queue, with a log line each.
	pumpQueueRefusedDir = "refused"
	// pumpQueueRefusalLimit is how many times one batch id may be refused before its notices move aside.
	pumpQueueRefusalLimit = 3
)

// pumpQueueFlush delivers the queued notices of every thread. It first completes any accepted
// batch whose move did not finish, then reads each thread's notices. For each thread it probes the
// parent's active turn through the delivery core's own dialer: an active parent has the whole
// batch steered in at once; an idle parent is sent the batch with the parent role once the oldest
// notice has waited max_queue_seconds. Files move to sent/ only on accepted. A probe that does not
// confirm the thread idle leaves the notices queued.
func pumpQueueFlush(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings, dry bool) error {
	root := filepath.Join(cfg.StateDir, pumpQueueDir)
	if err := pumpQueueSafe(root, "parent queue directory"); err != nil {
		return err
	}
	threads, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, thread := range threads {
		if !thread.IsDir() {
			continue
		}
		if err := pumpQueueFlushThread(ctx, e, cfg, st, s, root, thread.Name(), dry); err != nil {
			pumpLog(cfg, "queue "+thread.Name()+": "+err.Error())
		}
	}
	return nil
}

// pumpQueueFlushThread delivers one thread's queued notices.
func pumpQueueFlushThread(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings, root, thread string, dry bool) error {
	dir := filepath.Join(root, thread)
	// A cancelled round makes no durable change: the pin, the membership and the moves are all
	// writes, and a first SIGINT must not leave one behind for a round that is already over.
	if err := ctx.Err(); err != nil {
		return err
	}
	// A move that an earlier round was interrupted in the middle of left a notice in the aside
	// directory; it is put back before anything is read, so an interrupted move never hides a notice
	// from the queue. A dry run reports it instead of moving it.
	if err := pumpReview776QueueRecoverAsides(ctx, e, dir, dry); err != nil {
		return err
	}
	// A pinned batch is reconciled first, under its own frozen logical id and body, before any new
	// batch is formed: a notice that may already have gone is never re-sent under a different id.
	if pin, pinned := st.QueueAttempt[thread]; pinned {
		if pin.Accepted {
			// A crash between the accepted mark and the moves is completed here without a resend. The
			// thread is settled for this round: a notice the delivery did not carry waits for the next
			// one, and a dry run reports only what it would complete.
			return pumpReview776QueueFinishAccepted(ctx, e, cfg, st, dir, thread, pin, dry)
		}
		stop, err := pumpReview776QueueRetry(ctx, e, cfg, st, s, dir, thread, dry)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	// An accepted batch whose move did not finish is completed next, so an already accepted notice
	// is never sent again. A dry run moves nothing, so it only reports what it would complete.
	if dry {
		if names := st.QueueAccepted[thread]; len(names) > 0 {
			fmt.Fprintf(e.Stdout, "queue %s: would complete %d accepted notices\n", thread, len(names))
			return nil
		}
	} else if err := pumpQueueCompleteAccepted(cfg, st, dir, thread); err != nil {
		return err
	}
	names, err := pumpQueueSortedNames(dir)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		// A quiet thread starts no bridge process.
		return nil
	}
	texts, err := pumpReview776QueueReadNotices(dir, names)
	if err != nil {
		return err
	}
	// An upgraded state has no pin, but the ledger may still hold a record under a pre-change
	// logical id. That lookup runs on the full set as it stands before any notice is moved aside and
	// before the size split, because the pre-change id hashed every queued name: a narrower set would
	// miss an old record and re-send an attempt that may already have gone.
	whole := pumpReview776QueueBatch{names: names, texts: texts, body: pumpReview776QueueBody(texts)}
	if dry {
		// A dry run makes no durable change, so it does not adopt a legacy record or save state. It
		// reports the notices it would move aside and the batch it would send.
		keptNames, keptTexts, err := pumpReview776QueueQuarantine(ctx, e, cfg, dir, thread, names, texts, true)
		if err != nil {
			return err
		}
		preview := pumpReview776QueueFit(keptNames, keptTexts)
		if len(preview.names) > 0 {
			fmt.Fprintf(e.Stdout, "queue %s %d notices\n%s\n", thread, len(preview.names), preview.body)
		}
		return nil
	}
	action, err := pumpReview776QueueAdoptLegacy(ctx, cfg, st, dir, thread, whole)
	if err != nil {
		return err
	}
	switch action {
	case pumpReview776QueueLegacyReconcile:
		// The old id and body are now pinned, so the gated retry reconciles them exactly like any
		// other pinned attempt, and a settled answer clears the pin.
		stop, err := pumpReview776QueueRetry(ctx, e, cfg, st, s, dir, thread, dry)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	case pumpReview776QueueLegacyComplete:
		// The pre-change ledger already accepted a batch, so exactly the notices it covered are moved
		// to sent/ instead of being delivered a second time. A notice queued after that accepted
		// attempt is not part of it and stays queued. The accepted pin is written before the first
		// move, so a crash part way through leaves the membership recoverable instead of letting the
		// remaining notices form a new batch and be delivered again.
		matched := st.QueueAttempt[thread]
		pin := pumpReview776QueuePin{
			LogicalID: matched.LogicalID, Names: append([]string(nil), matched.Names...),
			Body: matched.Body, SHA256: matched.SHA256, Accepted: true, Held: matched.Held}
		st.QueueAttempt[thread] = pin
		if err := st.pumpSave(cfg); err != nil {
			return err
		}
		if pin.Held {
			// The pre-change attempt was accepted but its text is not recoverable, so the pin holds the
			// thread: completing by name could archive a notice it never carried, and sending under a new
			// id could deliver one it did carry a second time.
			return nil
		}
		return pumpReview776QueueFinishAccepted(ctx, e, cfg, st, dir, thread, pin, false)
	case pumpReview776QueueLegacyHold:
		// An accepted attempt whose text is not the text on disk: the pin is written and the thread
		// waits, so nothing is archived undelivered and nothing is delivered twice.
		return nil
	}
	// A notice that alone exceeds the batch limit, or one that carries nothing once trimmed, is
	// moved aside, so the longest fitting prefix always has something to carry and one such notice
	// cannot hold the queue.
	names, texts, err = pumpReview776QueueQuarantine(ctx, e, cfg, dir, thread, names, texts, false)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	batch := pumpReview776QueueFit(names, texts)
	if len(batch.names) == 0 {
		// Every queued notice carries an empty body, so there is nothing to deliver this round.
		return nil
	}
	// The idle-parent timeout is judged on the whole queue left after the oversize move, so an older
	// notice outside the size-cut prefix makes the queue due. The batch itself carries only the prefix
	// that fits, and a notice moved aside as oversize cannot age the queue.
	oldest, err := pumpReview776QueueOldest(dir, names)
	if err != nil {
		return err
	}
	// The active-turn probe decides WHEN; Deliver's own read decides HOW. Both paths carry the
	// parent role, so an idle-to-active race between them does not change the semantics.
	state, err := pumpQueueTurnState(ctx, e, cfg, thread)
	if err != nil {
		// The probe failed: the notices stay queued and the thread is recorded as unmeasured, the
		// same state a source whose read failed reports.
		pumpLog(cfg, "queue "+thread+" "+pumpSourceUnmeasured+": "+err.Error())
		return nil
	}
	if state != pumpQueueIdle {
		// An active thread is steered below; an undetermined observation is not idle, so it never
		// triggers the timeout send.
		if state == pumpQueueActive {
			return pumpQueueSend(ctx, e, cfg, st, dir, thread, batch)
		}
		return nil
	}
	if e.Now().Sub(oldest).Seconds() < float64(pumpSettingInt(s.MaxQueueSeconds, pumpDefaultMaxQueueSeconds)) {
		return nil
	}
	return pumpQueueSend(ctx, e, cfg, st, dir, thread, batch)
}

// pumpQueueSend delivers one thread's queued batch and moves the files only on accepted. The
// batch is pinned before the send, so an unknown outcome leaves a frozen logical id and body that
// the next round reconciles instead of forming a new batch around a notice that may already have
// gone. The accepted membership is recorded before the first move, so a move that fails part way
// is completed by the next round instead of being sent again.
func pumpQueueSend(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, batch pumpReview776QueueBatch) error {
	if err := ctx.Err(); err != nil {
		// The pin and the membership are durable effects: a cancelled round makes neither.
		return err
	}
	if !utf8.ValidString(batch.body) {
		// JSON replaces invalid bytes with the replacement character, so a pinned body would stop
		// being the text on disk; the delivery core refuses such a message anyway.
		return fmt.Errorf("crw manage pump: the queued notices of %s are not valid UTF-8", thread)
	}
	base := pumpQueueBatchID(thread, batch.idNames, batch.body)
	logicalID := base
	// A batch the ledger refused under this id is tried again under the next ordinal of the id. The
	// ledger's refusal of the id itself is final, so resending under it would only replay that refusal.
	if refusal := st.QueueRefused[thread]; refusal.ID == base && refusal.Count > 0 {
		logicalID = fmt.Sprintf("%s-r%d", base, refusal.Count)
	}
	st.QueueAttempt[thread] = pumpReview776QueuePin{
		LogicalID: logicalID, Base: base, Names: append([]string(nil), batch.names...), Body: batch.body,
		SHA256: pumpReview776QueueDigests(batch.names, batch.texts)}
	if err := st.pumpSave(cfg); err != nil {
		return err
	}
	out, err := Deliver(ctx, e, cfg, Message{LogicalID: logicalID, Thread: thread, Text: batch.body, Role: "parent", Settings: cfg.Settings.Parent})
	if err != nil && out.Class == "" {
		return pumpReview776QueuePinLift(cfg, st, thread, err)
	}
	pumpLog(cfg, fmt.Sprintf("queue thread=%s notices=%d request=%s class=%s received=%v applied=%v",
		thread, len(batch.names), out.RequestID, out.Class, out.Class == deliverClassAccepted, false))
	switch out.Class {
	case deliverClassAccepted:
		// The accepted mark is durable before any move, so a crash between the moves is recoverable:
		// the next round completes them from the pin without sending again.
		if err := ctx.Err(); err != nil {
			return err
		}
		pin := st.QueueAttempt[thread]
		pin.Accepted = true
		st.QueueAttempt[thread] = pin
		if err := st.pumpSave(cfg); err != nil {
			return err
		}
		return pumpReview776QueueFinishAccepted(ctx, e, cfg, st, dir, thread, pin, false)
	case deliverClassRefused:
		// A refusal is terminal and nothing was sent, so the pin lifts: the notices stay queued and a
		// later round forms the batch again. A refusal the ledger proves is counted against the batch.
		if pumpReview776QueueRefusalSettled(cfg, logicalID) {
			return pumpQueueRefusedLift(ctx, cfg, st, dir, thread, st.QueueAttempt[thread], err)
		}
		return pumpReview776QueuePinLift(cfg, st, thread, err)
	default:
		// Unknown: the pin stays, so the next round reconciles the same logical id and body.
		return err
	}
}

// pumpQueueCompleteAccepted moves a legacy membership's notices into sent/ by name. A membership
// written before the pin carried digests names its members only, so it is completed as it was
// written. A membership the pin wrote is completed by pumpReview776QueueFinishAccepted instead.
func pumpQueueCompleteAccepted(cfg *Config, st *pumpState, dir, thread string) error {
	names := st.QueueAccepted[thread]
	if len(names) == 0 {
		return nil
	}
	if err := pumpReview776QueueMoveByName(dir, names); err != nil {
		return err
	}
	delete(st.QueueAccepted, thread)
	delete(st.QueueAttempt, thread)
	return st.pumpSave(cfg)
}

// pumpReview776QueueBatch is one queue batch: the notices it carries, their trimmed bodies and the
// body it sends. The per-notice texts let the pin store each member's digest, so an accepted batch
// moves only the members still carrying the text it sent.
type pumpReview776QueueBatch struct {
	names []string
	texts []string
	body  string
	// idNames is the queued name set the logical id is computed over. It is the whole queue the round
	// read, not the prefix the batch carries, so a notice the producer adds after the prefix still
	// changes the id and lets a batch the ledger already refused be tried again under a new id.
	idNames []string
}

// pumpReview776QueueReadNotices reads one thread's notices in name order. A notice is a regular
// file the producer wrote; a symlink would let the queue carry the contents of a file outside it,
// so it is refused rather than followed.
func pumpReview776QueueReadNotices(dir string, names []string) ([]string, error) {
	texts := make([]string, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("crw manage pump: the notice %s is a symlink; refusing to send through it", name)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		texts = append(texts, strings.TrimSpace(string(raw)))
	}
	return texts, nil
}

// pumpReview776QueueOldest is the oldest modification time among one batch's notices. It is
// measured on the notices the batch actually carries, so a notice moved aside as oversize or left
// for the next round cannot age a batch it is not part of.
func pumpReview776QueueOldest(dir string, names []string) (time.Time, error) {
	var oldest time.Time
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return oldest, err
		}
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	return oldest, nil
}

// pumpReview776QueueQuarantine moves aside a notice that can never form a deliverable batch: one
// whose body alone exceeds the batch limit, and one that carries nothing once trimmed. Both would
// otherwise be picked as a singleton every round and refused, holding the thread's queue forever.
// The notice goes to the thread's oversize/ directory with a log line naming it, its size and why.
// A dry run reports the move instead of making it.
func pumpReview776QueueQuarantine(ctx context.Context, e *Env, cfg *Config, dir, thread string, names, texts []string, dry bool) ([]string, []string, error) {
	keptNames := make([]string, 0, len(names))
	keptTexts := make([]string, 0, len(texts))
	for i, name := range names {
		if len(texts[i]) <= pumpBatchLimit && texts[i] != "" {
			keptNames = append(keptNames, name)
			keptTexts = append(keptTexts, texts[i])
			continue
		}
		if dry {
			fmt.Fprintf(e.Stdout, "queue %s: would move notice %s (%d bytes) to oversize/\n", thread, name, len(texts[i]))
			continue
		}
		// The move is durable, so a cancelled round does not make it.
		if err := ctx.Err(); err != nil {
			return keptNames, keptTexts, err
		}
		oversizeDir := filepath.Join(dir, pumpReview776OversizeDir)
		// The destination is checked the way the queue root is: a symlink planted in its place would
		// redirect the move outside the state directory.
		if err := pumpQueueSafe(oversizeDir, "oversize directory"); err != nil {
			return keptNames, keptTexts, err
		}
		if err := os.MkdirAll(oversizeDir, 0o700); err != nil {
			return keptNames, keptTexts, err
		}
		// The move is verified against the text this round read, so a notice the producer replaced
		// with a normal-sized one stays in the queue instead of being quarantined, and no move ever
		// unlinks a notice the producer wrote.
		destination, moved, err := pumpReview776QueueMoveVerified(dir, name, oversizeDir, pumpReview776BodyDigest(texts[i]))
		if err != nil {
			return keptNames, keptTexts, err
		}
		if !moved {
			// The move is retried next round; the notice is left out of this round's batch so one
			// immovable file cannot hold the thread's other notices.
			pumpLog(cfg, fmt.Sprintf("queue %s: notice %s could not be moved aside", thread, name))
			continue
		}
		reason := "oversize"
		if texts[i] == "" {
			reason = "empty"
		}
		pumpLog(cfg, fmt.Sprintf("queue %s: %s notice %s %d bytes moved to oversize/%s", thread, reason, name, len(texts[i]), filepath.Base(destination)))
	}
	return keptNames, keptTexts, nil
}

// pumpReview776QueueDestName is the name a moved notice takes in its destination directory. The
// notice's own name is preferred, so the common move keeps the name it arrived with; when that
// name is taken the move falls back to <name>.<UTC stamp>-<n>, which keeps a second notice under
// the same name apart from the first. The fallback is chosen only after os.Link reported the name
// taken, so a destination is never picked by a check the move itself could invalidate.
func pumpReview776QueueDestName(destDir, name string, attempt int) string {
	if attempt == 0 {
		return filepath.Join(destDir, name)
	}
	now := time.Now()
	return filepath.Join(destDir, fmt.Sprintf("%s.%s-%d",
		name, now.UTC().Format("20060102T150405"), attempt))
}

// pumpReview776QueueFit is the longest name-ordered prefix of a thread's notices whose body stays
// inside the batch limit, the same rule the management batch uses. The rest waits for the next
// round. The body length is accumulated as the prefix grows rather than rebuilt for each length,
// so a long backlog costs one pass.
func pumpReview776QueueFit(names, texts []string) pumpReview776QueueBatch {
	if len(names) == 0 {
		return pumpReview776QueueBatch{}
	}
	idNames := append([]string(nil), names...)
	total, fit := 0, 0
	for i, text := range texts {
		total += len(text)
		if i > 0 {
			// The separator between two notices, plus the header line the batch grows once it carries
			// more than one.
			total += 2
		}
		if total+len(pumpReview776QueueHeader(i+1)) > pumpBatchLimit {
			break
		}
		fit = i + 1
	}
	if fit == 0 {
		// A notice that alone exceeds the limit, and one with an empty body, were moved aside already.
		// This is only a guard against a batch the delivery core could never accept, so it carries the
		// longest prefix whose body is non-empty.
		for i, text := range texts {
			if text != "" {
				return pumpReview776QueueBatch{names: names[:i+1], texts: texts[:i+1], body: pumpReview776QueueBody(texts[:i+1]), idNames: idNames}
			}
		}
		return pumpReview776QueueBatch{}
	}
	return pumpReview776QueueBatch{names: names[:fit], texts: texts[:fit], body: pumpReview776QueueBody(texts[:fit]), idNames: idNames}
}

// pumpReview776QueueHeader is the count line a queue batch carries once it holds more than one
// notice.
func pumpReview776QueueHeader(count int) string {
	if count <= 1 {
		return ""
	}
	return fmt.Sprintf("management session notices: %d queued\n\n", count)
}

// pumpReview776QueueBody is a queue batch's delivered text: the notices' bodies, with the count
// line the issue fixes for a batch carrying more than one.
func pumpReview776QueueBody(texts []string) string {
	return pumpReview776QueueHeader(len(texts)) + strings.Join(texts, "\n\n")
}

// pumpReview776QueueRetry reconciles a pinned queue batch. It reports whether the thread is settled
// for this round, so the caller does not also form a fresh batch in the same round.
//
// The reconciliation always uses the pin's own logical id and body, never a body rebuilt from disk:
// a member the producer replaced says nothing about the attempt that may already have gone, and
// dropping the pin on that change would let the unchanged members be sent again under a new id.
// The retry repeats the queue's own gate: an idle parent is only sent to once the oldest pinned
// notice has waited max_queue_seconds, so reconciling never opens a parent turn early.
func pumpReview776QueueRetry(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings, dir, thread string, dry bool) (bool, error) {
	pin := st.QueueAttempt[thread]
	if dry {
		fmt.Fprintf(e.Stdout, "queue %s %d notices\n%s\n", thread, len(pin.Names), pin.Body)
		return true, nil
	}
	if pin.Held {
		// The pre-change attempt was accepted but its text is not recoverable, so no queued notice can
		// be told apart from one it already carried. The thread waits, which loses nothing and
		// duplicates nothing; nothing is sent and no bridge process starts.
		return true, nil
	}
	// The bridge's own receipt is read every round, before any gate: the lookup opens no parent turn,
	// so an attempt the bridge already settled must not be held back until an idle parent has waited
	// max_queue_seconds. Only the send that would follow an unsettled receipt is gated.
	record, known, err := deliverLoad(cfg, pin.LogicalID)
	if err != nil {
		return true, err
	}
	if known {
		// The ledger's own settled answer is read first, with no bridge: an accepted record completes the
		// batch and a refused one lifts the pin, the answers Deliver gives a settled message. Only an
		// unsettled record asks the bridge for its receipt.
		switch record.State {
		case deliverStateAccepted:
			return pumpReview776QueueAcceptPin(ctx, e, cfg, st, dir, thread, pin)
		case deliverStateRefused:
			return false, pumpQueueRefusedLift(ctx, cfg, st, dir, thread, pin, nil)
		}
		settled, err := pumpReview776QueueReconcileReceipt(ctx, e, cfg, st, dir, thread, pin, record)
		if err != nil {
			return true, err
		}
		if settled {
			return true, nil
		}
		if _, pinned := st.QueueAttempt[thread]; !pinned {
			// The receipt lifted the pin: the refusal is counted once, and nothing is sent under the id now.
			return false, nil
		}
	}
	// The receipt is not settled (or there is none): the attempt may still be sent, but only under
	// the queue's own gate, so an idle parent is not opened early. The gate judges the pinned notices and
	// every queued notice the next batch would keep, so an older one that would be moved aside cannot make
	// the pin due.
	gate, err := pumpReview776QueueGateNames(dir, pin.Names)
	if err != nil {
		return true, err
	}
	present := pumpReview776QueuePresent(dir, gate)
	oldest, err := pumpReview776QueueOldest(dir, present)
	if err != nil {
		return true, err
	}
	state, err := pumpQueueTurnState(ctx, e, cfg, thread)
	if err != nil {
		// The probe failed: the pin stays and the notices are left alone, the same state a source
		// whose read failed reports.
		pumpLog(cfg, "queue "+thread+" "+pumpSourceUnmeasured+": "+err.Error())
		return true, nil
	}
	if state == pumpQueueOther {
		return true, nil
	}
	if state == pumpQueueIdle && e.Now().Sub(oldest).Seconds() < float64(pumpSettingInt(s.MaxQueueSeconds, pumpDefaultMaxQueueSeconds)) {
		return true, nil
	}
	return pumpReview776QueueSettle(ctx, e, cfg, st, dir, thread, pin)
}

// pumpReview776QueueReconcileReceipt reads the bridge's own receipt for a pinned attempt and settles
// what it answers, without ever sending. It reports whether the pin is settled for this round, so the
// caller neither sends nor forms a new batch. An accepted receipt completes the batch (or holds it
// when the pin cannot prove which notices the attempt carried); a refused one lifts the pin, because
// nothing was sent under it; an undetermined one keeps the pin.
func pumpReview776QueueReconcileReceipt(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, pin pumpReview776QueuePin, record deliverRecord) (bool, error) {
	bridge, err := deliverDial(ctx, e, cfg)
	if err != nil {
		// A bridge that could not be started says nothing about the attempt, so the pin stays.
		pumpLog(cfg, "queue "+thread+" "+pumpSourceUnmeasured+": "+err.Error())
		return true, nil
	}
	defer bridge.close()
	switch deliverReconcile(ctx, bridge, record) {
	case deliverReconcileAccepted:
		at := deliverNow(e)
		if err := ctx.Err(); err != nil {
			return true, err
		}
		if !pin.Legacy && len(pin.SHA256) > 0 {
			record.State, record.Received, record.Applied = deliverStateAccepted, true, false
			record.Attempts = append(record.Attempts, deliverAttempt{At: at, Class: deliverClassAccepted, ReceiptExcerpt: "get_operation: the pinned attempt was dispatched"})
			if err := deliverSave(cfg, record); err != nil {
				return true, err
			}
		}
		return pumpReview776QueueAcceptPin(ctx, e, cfg, st, dir, thread, pin)
	case deliverReconcileRefused:
		// The bridge settled the attempt as a refusal before any dispatch, so nothing was sent. The
		// refusal is recorded the way Deliver records one, and counted against the batch.
		at := deliverNow(e)
		if err := ctx.Err(); err != nil {
			// The outbox record, the count and the pin clear are durable effects: a cancelled round makes none.
			return true, err
		}
		record.State = deliverStateRefused
		record.Attempts = append(record.Attempts, deliverAttempt{At: at, Class: deliverClassRefused, ReceiptExcerpt: "get_operation: refused before the dispatch"})
		if err := deliverSave(cfg, record); err != nil {
			return true, err
		}
		return false, pumpQueueRefusedLift(ctx, cfg, st, dir, thread, pin, nil)
	case deliverReconcileResendSame, deliverReconcileResendNew:
		// Nothing was sent under the attempt, so the caller may make it under the queue's gate.
		return false, nil
	default:
		// Undetermined: the pin stays, so the next round reconciles the same logical id and body.
		return true, nil
	}
}

// pumpReview776QueueSettle reconciles a pinned queue batch. It reports whether the thread is settled
// for this round, so the caller does not also form a fresh batch.
//
// A pin taken for a pre-change ledger record whose text the ledger does not store is reconciled
// through the bridge's own receipt instead (see pumpReview776QueueSettleLegacy): the notice text on
// disk is never replayed for it, because a notice the producer replaced no longer carries what the
// old attempt sent.
func pumpReview776QueueSettle(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, pin pumpReview776QueuePin) (bool, error) {
	if pin.Legacy {
		return pumpReview776QueueSettleLegacy(ctx, e, cfg, st, dir, thread, pin)
	}
	out, err := Deliver(ctx, e, cfg, Message{LogicalID: pin.LogicalID, Thread: thread, Text: pin.Body, Role: "parent", Settings: cfg.Settings.Parent})
	if err != nil && out.Class == "" {
		// An unclassified local failure reached no bridge and learned nothing about the pinned
		// attempt, so the pin stays and the next round reconciles it again.
		return true, err
	}
	pumpLog(cfg, fmt.Sprintf("queue thread=%s notices=%d request=%s class=%s received=%v applied=%v",
		thread, len(pin.Names), out.RequestID, out.Class, out.Class == deliverClassAccepted, false))
	switch out.Class {
	case deliverClassAccepted:
		// The accepted mark is durable before any move, so a crash between the moves is completed by
		// the next round from the pin without sending again.
		if err := ctx.Err(); err != nil {
			return true, err
		}
		pin.Accepted = true
		st.QueueAttempt[thread] = pin
		if err := st.pumpSave(cfg); err != nil {
			return true, err
		}
		return true, pumpReview776QueueFinishAccepted(ctx, e, cfg, st, dir, thread, pin, false)
	case deliverClassRefused:
		// A refusal clears the pin only when the ledger proves the pinned attempt itself was refused.
		// The delivery core reports a local refusal (an unconfigured bridge policy, for example)
		// before it reads the ledger at all, and that says nothing about the attempt that may already
		// have gone: the pin then stays and the next round reconciles it again.
		if !pumpReview776QueueRefusalSettled(cfg, pin.LogicalID) {
			return true, err
		}
		return true, pumpQueueRefusedLift(ctx, cfg, st, dir, thread, pin, err)
	default:
		// Unknown: the pin stays, so the next round reconciles the same logical id and body.
		return true, err
	}
}

// pumpReview776QueueSettleLegacy reconciles a pin taken for a pre-change ledger record. The ledger
// does not store the attempt's text, so the answer is read from the bridge's own receipt for the
// old request id instead of replaying the notice text on disk, which a producer may have replaced.
// It reports whether the thread is settled for this round.
func pumpReview776QueueSettleLegacy(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, pin pumpReview776QueuePin) (bool, error) {
	record, known, err := deliverLoad(cfg, pin.LogicalID)
	if err != nil {
		return true, err
	}
	if !known {
		// The record is gone, so there is nothing left to reconcile: the pin goes and the batch may
		// be formed from what is on disk.
		return false, pumpReview776QueuePinLift(cfg, st, thread, nil)
	}
	bridge, err := deliverDial(ctx, e, cfg)
	if err != nil {
		// A bridge that could not be started says nothing about the old attempt, so the pin stays.
		pumpLog(cfg, "queue "+thread+" "+pumpSourceUnmeasured+": "+err.Error())
		return true, nil
	}
	defer bridge.close()
	switch verdict := deliverReconcile(ctx, bridge, record); verdict {
	case deliverReconcileAccepted:
		// The old attempt was dispatched. It covered exactly the names the pin holds, because the pin was
		// taken for the pre-change id of those names, so those notices are already delivered and must not
		// be sent again.
		//
		// When the pin carries the digests of the text the attempt sent, every queued member is provably
		// one it carried, so completing them by name archives nothing undelivered. When it does not (a
		// legacy pin, whose attempt's text the ledger does not store), a queued notice cannot be told
		// apart from one the attempt never carried: completing by name could archive an undelivered
		// notice, and sending under a new id could deliver a carried one twice. The pin holds the thread
		// instead, which loses nothing and duplicates nothing.
		if err := ctx.Err(); err != nil {
			return true, err
		}
		if len(pin.SHA256) == 0 {
			pin.Held = true
			st.QueueAttempt[thread] = pin
			if err := st.pumpSave(cfg); err != nil {
				return true, err
			}
			pumpLog(cfg, fmt.Sprintf("queue %s: the pre-change attempt %s was accepted and its text is not recoverable; the notices stay queued", thread, pin.LogicalID))
			return true, nil
		}
		pin.Accepted = true
		st.QueueAttempt[thread] = pin
		if err := st.pumpSave(cfg); err != nil {
			return true, err
		}
		return true, pumpReview776QueueFinishAccepted(ctx, e, cfg, st, dir, thread, pin, false)
	case deliverReconcileRefused, deliverReconcileResendSame, deliverReconcileResendNew:
		// Nothing was sent under the old id, so the batch may take its current id this round.
		pumpLog(cfg, fmt.Sprintf("queue %s: the pre-change attempt %s settled without a delivery", thread, pin.LogicalID))
		return false, pumpReview776QueuePinLift(cfg, st, thread, nil)
	default:
		// Undetermined: the pin stays, so the next round reconciles the same old id.
		return true, nil
	}
}

// pumpReview776QueueFinishAccepted completes an accepted batch: it moves to sent/ only the members
// whose current file still carries the digest that was sent, leaves a replaced member queued with a
// log line, and clears the pin. It runs from the accepted send and from a later round that finds an
// accepted pin.
func pumpReview776QueueFinishAccepted(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, pin pumpReview776QueuePin, dry bool) error {
	if dry {
		fmt.Fprintf(e.Stdout, "queue %s: would complete %d accepted notices\n", thread, len(pin.Names))
		return nil
	}
	if err := ctx.Err(); err != nil {
		// The moves and the pin clear are durable effects: a cancelled round makes neither.
		return err
	}
	sent := filepath.Join(dir, pumpSentDir)
	created := false
	for _, name := range pin.Names {
		if !created {
			if err := os.MkdirAll(sent, 0o700); err != nil {
				return err
			}
			created = true
		}
		// Only a member whose file still carries the text that was sent moves to sent/. The move
		// verifies the content through an open handle, links it to a free name under sent/ and removes
		// the queue name only while it still names that same file, so a notice the producer replaced
		// is left queued rather than taken to sent/ undelivered.
		_, moved, err := pumpReview776QueueMoveVerified(dir, name, sent, pin.SHA256[name])
		if err != nil {
			return err
		}
		if !moved {
			pumpLog(cfg, fmt.Sprintf("queue %s: the accepted notice %s was replaced or is gone; left queued", thread, name))
		}
	}
	delete(st.QueueAttempt, thread)
	delete(st.QueueAccepted, thread)
	return st.pumpSave(cfg)
}

// pumpReview776QueueAcceptPin completes a pin whose attempt is settled as accepted. A pin that cannot
// prove which notices the attempt carried is held instead, so nothing is archived undelivered and
// nothing is sent twice. It reports the thread as settled for this round.
func pumpReview776QueueAcceptPin(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, pin pumpReview776QueuePin) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if len(pin.SHA256) == 0 {
		// The attempt's text is not recoverable, so no queued notice can be shown to be one it
		// carried: the pin holds the thread rather than archiving an undelivered notice.
		pin.Held = true
		st.QueueAttempt[thread] = pin
		return true, st.pumpSave(cfg)
	}
	pin.Accepted = true
	st.QueueAttempt[thread] = pin
	if err := st.pumpSave(cfg); err != nil {
		return true, err
	}
	return true, pumpReview776QueueFinishAccepted(ctx, e, cfg, st, dir, thread, pin, false)
}

// pumpQueueRefusedLift drops the pin of a batch the ledger proves the bridge refused, and counts the
// refusal against the batch id, so the batch's next attempt goes out under its next ordinal. The
// pumpQueueRefusalLimit-th refusal of one id moves the batch's notices to refused/ instead, with a
// log line naming each one, so the notices queued behind it flow. A pin without digests, and a legacy
// pin (a pre-change attempt, whose refusal was not a refusal of this queue's batch), is only lifted.
func pumpQueueRefusedLift(ctx context.Context, cfg *Config, st *pumpState, dir, thread string, pin pumpReview776QueuePin, cause error) error {
	if err := ctx.Err(); err != nil {
		// The count, the moves and the pin clear are durable effects: a cancelled round makes none.
		return err
	}
	if len(pin.SHA256) == 0 || pin.Legacy {
		return pumpReview776QueuePinLift(cfg, st, thread, cause)
	}
	base := pin.Base
	if base == "" {
		base = pin.LogicalID
	}
	count := 1
	if refusal := st.QueueRefused[thread]; refusal.ID == base {
		count = refusal.Count + 1
	}
	if count < pumpQueueRefusalLimit {
		st.QueueRefused[thread] = pumpQueueRefusal{ID: base, Count: count}
		return pumpReview776QueuePinLift(cfg, st, thread, cause)
	}
	refused := filepath.Join(dir, pumpQueueRefusedDir)
	// The directory is checked the way the queue root is, so a symlink planted in its place cannot
	// redirect the moves outside the thread.
	if err := pumpQueueSafe(refused, "refused directory"); err != nil {
		return err
	}
	if err := os.MkdirAll(refused, 0o700); err != nil {
		return err
	}
	reason := pumpQueueRefusalReason(cfg, pin.LogicalID)
	for _, name := range pin.Names {
		destination, moved, err := pumpReview776QueueMoveVerified(dir, name, refused, pin.SHA256[name])
		if err != nil {
			return err
		}
		if !moved {
			pumpLog(cfg, fmt.Sprintf("queue %s: the refused notice %s was replaced or is gone; left queued", thread, name))
			continue
		}
		info, err := os.Stat(destination)
		if err != nil {
			return err
		}
		pumpLog(cfg, fmt.Sprintf("queue %s: refused notice %s %d bytes moved to %s/%s after %d refusals: %s",
			thread, name, info.Size(), pumpQueueRefusedDir, filepath.Base(destination), count, reason))
	}
	delete(st.QueueRefused, thread)
	return pumpReview776QueuePinLift(cfg, st, thread, cause)
}

// pumpQueueRefusalReason is the bridge's last answer the ledger keeps for a logical id, cut short and
// kept on one line for the log.
func pumpQueueRefusalReason(cfg *Config, logicalID string) string {
	record, known, err := deliverLoad(cfg, logicalID)
	if err != nil || !known || len(record.Attempts) == 0 {
		return "no receipt excerpt"
	}
	excerpt := strings.Join(strings.Fields(record.Attempts[len(record.Attempts)-1].ReceiptExcerpt), " ")
	if excerpt == "" {
		return "refused"
	}
	if len(excerpt) > 200 {
		excerpt = strings.ToValidUTF8(excerpt[:200], "") + "..."
	}
	return excerpt
}

// pumpReview776QueueRefusalSettled reports whether the ledger holds a record for a logical id that
// the delivery core settled as refused. A refusal the core reports before it reads the ledger (an
// unconfigured bridge policy) leaves the record untouched, so it is not evidence about the attempt.
func pumpReview776QueueRefusalSettled(cfg *Config, logicalID string) bool {
	record, known, err := deliverLoad(cfg, logicalID)
	if err != nil || !known {
		return false
	}
	return record.State == deliverStateRefused
}

// pumpReview776QueueDestTries bounds the free-name search of one move, so a directory that is full
// of colliding names fails loudly instead of spinning.
const pumpReview776QueueDestTries = 64

// pumpReview776QueueMoveVerified moves one notice into destDir only while it still carries the
// expected text. It never replaces an entry and never removes a notice the producer wrote.
//
// The queue name is first taken aside with one atomic rename, never with a remove. A producer's
// --queue write is itself an atomic rename onto the same name, so the file the take-aside moves is
// whatever the name held at that instant; from then on this move touches only its own aside name, so
// no check-then-act window can delete a notice the producer wrote after the check. The taken file
// is compared with the file this move opened: the verified one is published under a free name in
// destDir with os.Link (which fails when the name exists, so an earlier quarantine or an earlier
// accepted move is never overwritten) and the aside name is dropped; anything else is a producer's
// newer notice and goes straight back under the queue name it came from.
//
// A vanished source or a notice whose text changed is reported as (…, false, nil): nothing moved
// and nothing was disturbed. Any other failure -- an unreadable file, a rename, link or removal
// that failed -- is returned, so a caller never records a batch as moved when it was not.
func pumpReview776QueueMoveVerified(dir, name, destDir, expectedDigest string) (string, bool, error) {
	source := filepath.Join(dir, name)
	file, err := os.Open(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return "", false, err
	}
	if pumpReview776BodyDigest(string(raw)) != expectedDigest {
		return "", false, nil
	}
	aside, err := pumpReview776QueueTakeAside(dir, name)
	if err != nil {
		return "", false, err
	}
	if aside == "" {
		// The name was already gone when the take-aside ran: nothing moved and nothing was disturbed.
		return "", false, nil
	}
	taken, err := os.Lstat(aside)
	if err != nil {
		return "", false, err
	}
	if !os.SameFile(opened, taken) {
		// The producer replaced the notice before the take-aside, so what it moved is its newer
		// notice and this move has nothing verified to publish. That notice goes back under the queue
		// name it came from.
		return "", false, pumpReview776QueuePutBack(aside, source)
	}
	dest, err := pumpReview776QueuePublish(aside, destDir, name)
	if err != nil {
		// Nothing was published, so the verified notice goes back to the queue name instead of being
		// left under this move's own name. The publish failure is what the caller must see: a move
		// that did not happen is never reported as one.
		if putErr := pumpReview776QueuePutBack(aside, source); putErr != nil {
			return "", false, putErr
		}
		return "", false, err
	}
	// The verified bytes are durable under dest, so the aside name, which only this move owns, is
	// dropped.
	if err := os.Remove(aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return dest, true, err
	}
	return dest, true, nil
}

// pumpReview776QueueAsideDir is the directory a move holds a notice in between taking it out of the
// queue and publishing it. It sits inside the thread directory, so the take-aside rename never
// crosses a filesystem, and the producer writes only <logical id>.txt there, so it can neither
// create this directory nor a file inside it.
func pumpReview776QueueAsideDir(dir string) string {
	return filepath.Join(dir, pumpReview776AsideDir)
}

// pumpReview776QueueTakeAside takes the queue name into the aside directory with one atomic rename
// and reports the name it landed under, or the empty string when the name was already gone. The
// rename is the only step that touches the queue name, so whatever the producer last wrote is moved
// whole.
func pumpReview776QueueTakeAside(dir, name string) (string, error) {
	asideDir := pumpReview776QueueAsideDir(dir)
	// The aside directory is checked the way the queue root is: a symlink planted in its place would
	// send the notice, and every later recovery, outside the state directory.
	if err := pumpQueueSafe(asideDir, "the queue's aside directory"); err != nil {
		return "", err
	}
	if err := os.MkdirAll(asideDir, 0o700); err != nil {
		return "", err
	}
	aside := filepath.Join(asideDir, name)
	if err := os.Rename(filepath.Join(dir, name), aside); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return aside, nil
}

// pumpReview776QueueRecoverAsides resolves the notices a move left in the aside directory when its
// process died between the take-aside and the publish. A notice is published by linking the aside to
// its destination, so the aside's inode is also present under sent/ or oversize/ exactly when the
// publish already happened: the aside is then dropped, because putting it back would deliver the
// notice twice. Otherwise the notice never left the queue, and it goes back to the name it came
// from. A notice written in the meantime keeps the queue name and the aside is dropped. A dry run
// reports what it would do instead of moving anything.
func pumpReview776QueueRecoverAsides(ctx context.Context, e *Env, dir string, dry bool) error {
	asideDir := pumpReview776QueueAsideDir(dir)
	// A symlink in the aside directory's place would make every file of its target look like one of
	// this move's asides, and the recovery would then unlink files outside the queue.
	if err := pumpQueueSafe(asideDir, "the queue's aside directory"); err != nil {
		return err
	}
	entries, err := os.ReadDir(asideDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		aside := filepath.Join(asideDir, name)
		info, err := os.Lstat(aside)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		published, err := pumpReview776QueuePublished(dir, info)
		if err != nil {
			return err
		}
		if dry {
			action := "restore"
			if published {
				action = "drop"
			}
			fmt.Fprintf(e.Stdout, "queue: would %s the notice %s an interrupted move set aside\n", action, name)
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if published {
			if err := os.Remove(aside); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := pumpReview776QueuePutBack(aside, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// pumpReview776QueuePublished reports whether a notice taken aside is already linked under sent/,
// oversize/ or refused/ -- the three directories a move publishes into -- which is what the publish
// step of a move leaves behind when the process died before it dropped the aside.
func pumpReview776QueuePublished(dir string, aside os.FileInfo) (bool, error) {
	for _, destDir := range []string{filepath.Join(dir, pumpSentDir), filepath.Join(dir, pumpReview776OversizeDir), filepath.Join(dir, pumpQueueRefusedDir)} {
		entries, err := os.ReadDir(destDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, err
		}
		for _, entry := range entries {
			info, err := os.Lstat(filepath.Join(destDir, entry.Name()))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return false, err
			}
			if os.SameFile(aside, info) {
				return true, nil
			}
		}
	}
	return false, nil
}

// pumpReview776QueuePutBack returns a notice taken aside to the queue name it came from. The queue
// name wins when something else already holds it -- that is a newer notice the producer wrote, which
// supersedes the taken one -- so the taken copy is then dropped and no move ever destroys a notice.
func pumpReview776QueuePutBack(aside, source string) error {
	if err := os.Link(aside, source); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := os.Remove(aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// pumpReview776QueuePublish links source to a free name under destDir and reports the destination.
// os.Link refuses a name that exists, so the fallback names are tried in order and an earlier
// quarantine is never replaced. The source is the caller's own aside name, which only that move
// owns, so the linked entry is exactly the file the move verified. A link that failed for any
// reason is returned as an error.
func pumpReview776QueuePublish(source, destDir, name string) (string, error) {
	for attempt := 0; attempt < pumpReview776QueueDestTries; attempt++ {
		dest := pumpReview776QueueDestName(destDir, name, attempt)
		if err := os.Link(source, dest); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		return dest, nil
	}
	return "", fmt.Errorf("crw manage pump: no free destination name for the notice %s", name)
}

// pumpReview776QueueDigests is each member's delivered body digest, stored in the pin so an
// accepted batch moves only the members still carrying the text it sent.
func pumpReview776QueueDigests(names, texts []string) map[string]string {
	digests := make(map[string]string, len(names))
	for i, name := range names {
		if i < len(texts) {
			digests[name] = pumpReview776BodyDigest(texts[i])
		}
	}
	return digests
}

// pumpReview776BodyDigest is one notice body's digest, taken over the same trimmed text the batch
// carries.
func pumpReview776BodyDigest(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:])
}

// pumpReview776QueuePresent is the pinned names whose file still exists, in the pin's order.
func pumpReview776QueuePresent(dir string, names []string) []string {
	present := make([]string, 0, len(names))
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			present = append(present, name)
		}
	}
	return present
}

// pumpReview776QueueMoveByName moves the named notices into sent/, the completion the pre-pin
// membership shape uses. It takes each name aside with one atomic rename and publishes the taken
// file with os.Link, so it never replaces an entry that is already under sent/ and never deletes a
// notice the producer wrote: a replacement keeps the queue name while the taken copy is completed.
func pumpReview776QueueMoveByName(dir string, names []string) error {
	sent := filepath.Join(dir, pumpSentDir)
	if err := os.MkdirAll(sent, 0o700); err != nil {
		return err
	}
	for _, name := range names {
		aside, err := pumpReview776QueueTakeAside(dir, name)
		if err != nil {
			return err
		}
		if aside == "" {
			// The notice is already gone; nothing to complete.
			continue
		}
		if _, err := pumpReview776QueuePublish(aside, sent, name); err != nil {
			if putErr := pumpReview776QueuePutBack(aside, filepath.Join(dir, name)); putErr != nil {
				return putErr
			}
			return err
		}
		if err := os.Remove(aside); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// pumpReview776QueueLegacyAction is what the pre-change ledger holds for a batch that has no pin.
type pumpReview776QueueLegacyAction int

const (
	// pumpReview776QueueLegacyNone: no record, a refused record, or one for different text. The batch
	// is sent under its current id as usual.
	pumpReview776QueueLegacyNone pumpReview776QueueLegacyAction = iota
	// pumpReview776QueueLegacyReconcile: an unsettled record whose text is this batch. Its old id and
	// body are pinned so the attempt is reconciled rather than re-sent under the new id.
	pumpReview776QueueLegacyReconcile
	// pumpReview776QueueLegacyComplete: an accepted record whose text is this batch. The notices were
	// delivered before the upgrade, so they are moved to sent/ instead of being sent again.
	pumpReview776QueueLegacyComplete
	// pumpReview776QueueLegacyHold: an accepted record whose text is not the text on disk. The attempt
	// covered the batch's names, but a queued notice cannot be told apart from one it never carried, so
	// the thread waits while the pin holds rather than archiving an undelivered notice or delivering a
	// covered one twice.
	pumpReview776QueueLegacyHold
)

// pumpReview776QueueLegacyCandidate is one set of notice names a pre-change ledger record may cover,
// with each member's modification time for the record's age check. Every set is checked the same
// way, whichever order it came from.
type pumpReview776QueueLegacyCandidate struct {
	names, texts []string
	modTimes     []time.Time
}

// pumpReview776QueueLegacyCandidates lists the sets a pre-change record may cover, longest first: the
// name-ordered prefixes of the queue, then the oldest-first prefixes. An oldest-first prefix matters
// when a notice queued later sorts before the notices the pre-change attempt carried. A set that is
// already a name-ordered prefix is listed once, as a name-ordered prefix.
func pumpReview776QueueLegacyCandidates(dir string, batch pumpReview776QueueBatch) ([]pumpReview776QueueLegacyCandidate, error) {
	modTimes := make([]time.Time, len(batch.names))
	for i, name := range batch.names {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		modTimes[i] = info.ModTime()
	}
	build := func(indices []int) pumpReview776QueueLegacyCandidate {
		c := pumpReview776QueueLegacyCandidate{}
		for _, i := range indices {
			c.names = append(c.names, batch.names[i])
			c.texts = append(c.texts, batch.texts[i])
			c.modTimes = append(c.modTimes, modTimes[i])
		}
		return c
	}
	var out []pumpReview776QueueLegacyCandidate
	for cut := len(batch.names); cut >= 1; cut-- {
		indices := make([]int, cut)
		for i := range indices {
			indices[i] = i
		}
		out = append(out, build(indices))
	}
	order := make([]int, len(batch.names))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ma, mb := modTimes[order[a]], modTimes[order[b]]
		if !ma.Equal(mb) {
			return ma.Before(mb)
		}
		return batch.names[order[a]] < batch.names[order[b]]
	})
	for cut := len(order); cut >= 1; cut-- {
		picked := append([]int(nil), order[:cut]...)
		// batch.names is name-ordered, so the sorted indices list the set in name order.
		sort.Ints(picked)
		if picked[cut-1] == cut-1 {
			continue
		}
		out = append(out, build(picked))
	}
	return out, nil
}

// pumpReview776QueueCarriedBy lists the members of a set that were already queued, unchanged, when a
// ledger record was written: those whose modification falls in the stamp's second or an earlier one.
// The stamp has whole seconds, so a member counts as later only when its modification falls in a later
// second than the stamp. A member outside the list was written after the attempt, so the attempt cannot
// have carried what it holds now. A stamp that does not parse is no evidence against the record, so
// every member is listed; the pump writes a stamp on every record.
func (c pumpReview776QueueLegacyCandidate) carriedBy(createdAt string) []int {
	created, err := time.Parse(time.RFC3339, createdAt)
	var carried []int
	for i, modified := range c.modTimes {
		if err == nil && modified.Truncate(time.Second).After(created) {
			continue
		}
		carried = append(carried, i)
	}
	return carried
}

// pumpReview776QueueGateNames is the set an idle-parent gate on a pinned attempt judges: the pinned
// names still present and every queued notice the next batch would keep, which are the notices that
// are neither empty nor over the batch limit. A notice the queue would move aside cannot age the pin.
func pumpReview776QueueGateNames(dir string, pinned []string) ([]string, error) {
	queued, err := pumpQueueSortedNames(dir)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(pinned)+len(queued))
	var gate []string
	for _, name := range append(append([]string(nil), pinned...), queued...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		text := strings.TrimSpace(string(raw))
		if text == "" || len(text) > pumpBatchLimit {
			continue
		}
		gate = append(gate, name)
	}
	return gate, nil
}

// pumpReview776QueueAdoptLegacy looks for a pre-change attempt a batch that has no pin still has to
// answer for, so an upgrade neither re-sends a delivery that may already have gone nor sends an
// accepted one again. It answers with the action the caller takes.
//
// The pre-change id hashed the thread and the sorted notice names of the batch that was actually
// sent, so a record is only acted on when its logical id is exactly the pre-change id of the names
// it is being applied to. The candidate name sets are the name-ordered prefixes of today's queue,
// longest first: the pre-change pump had no size split, so it always sent the whole queue, and a
// notice queued afterwards extends the set rather than changing it. The id check is what keeps a
// record for another batch -- a direct send-parent delivery, or one over different names -- from
// completing notices it never carried. A record is also adopted only when it was written no earlier
// than the second a member was last modified, for every candidate set: a notice written after the
// attempt cannot have been carried by it.
//
// An unsettled record is adopted whatever its message digest says. The digest is not evidence that
// the attempt never went: a record whose text is not the text now on disk is still an attempt that
// may already have been delivered, so it is pinned and reconciled through the bridge's own receipt
// instead of the batch taking a new id and sending the unchanged members a second time. When the
// record's text is the batch's own text the pin carries that body and the per-member digests, so the
// delivery core can replay it; otherwise the attempt's text is not recoverable, the pin is marked
// legacy, and it is reconciled through the receipt alone.
//
// A record is judged member by member. When some members were written again after the record and
// others were not, the unchanged ones are the members the attempt carried and the replaced ones are
// notices it never carried, so only the unchanged members are adopted (see
// pumpReview776QueueAdoptPartial): a record that is stale for one member never becomes a reason to send
// another member again under a new id. Records can overlap, so the unsettled ones of this kind are
// adopted last: every other candidate is read first, and an accepted record for any of the sets, whole
// or partial, is acted on before them, because a receipt that says an unsettled attempt never went must
// not leave an accepted one unread.
//
// A settled accepted record is completed only while its own text is still the text on disk, because
// that is the only case where every member is known to have been delivered.
func pumpReview776QueueAdoptLegacy(ctx context.Context, cfg *Config, st *pumpState, dir, thread string, batch pumpReview776QueueBatch) (pumpReview776QueueLegacyAction, error) {
	candidates, err := pumpReview776QueueLegacyCandidates(dir, batch)
	if err != nil {
		return pumpReview776QueueLegacyNone, err
	}
	// An unsettled record that covers only some of its members is the weakest evidence: the bridge may
	// say it never went, and the pin is then lifted and the batch takes a new id. It is therefore held
	// back until every candidate has been read, so a settled record for a shorter or other set of the same
	// members is never hidden behind it.
	var weak *pumpReview776QueueWeakAdoption
	for _, candidate := range candidates {
		names, texts := candidate.names, candidate.texts
		oldID := pumpQueueLegacyBatchID(thread, names)
		record, known, err := deliverLoad(cfg, oldID)
		if err != nil {
			return pumpReview776QueueLegacyNone, err
		}
		if !known {
			continue
		}
		carried := candidate.carriedBy(record.CreatedAt)
		if len(carried) == 0 {
			// Every member was last modified after the record was written, so none of them came before the
			// attempt and the set is not the one the attempt covered. This holds for a name-ordered set as
			// for an oldest-first one: a record for names a producer has since written again is not
			// adopted, so it can neither pin a stale attempt nor hold the thread.
			continue
		}
		if len(carried) < len(names) {
			// Some members were written again after the attempt and some were not. The record is the pre-change
			// id of exactly these names, so the unchanged members are the ones the attempt carried, and the
			// replaced ones are notices it never carried. Only the unchanged members are adopted, by the
			// digest of the text they still hold; the attempt's own text is not recoverable, so a replay is
			// never an option, and a member that cannot be shown to have been carried is never sent under a
			// new id on the attempt's behalf.
			if record.State != deliverStateAccepted {
				if record.State != deliverStateRefused && weak == nil {
					weak = &pumpReview776QueueWeakAdoption{oldID: oldID, record: record, candidate: candidate, carried: carried}
				}
				continue
			}
			if action, adopted, err := pumpReview776QueueAdoptPartial(ctx, cfg, st, thread, oldID, record, candidate, carried); adopted || err != nil {
				return action, err
			}
			continue
		}
		body := pumpReview776QueueBody(texts)
		sameBody := record.MessageSHA256 == deliverMessageSHA256(body)
		switch record.State {
		case deliverStateAccepted:
			// The record's logical id is the pre-change id of exactly these names, so the attempt was
			// made over them. When the text on disk is still the text the attempt was accepted with, every
			// member is provably one it carried, and completing them is what keeps the promise that no
			// notice is sent twice. When it is not, a queued notice cannot be told apart from one the
			// attempt never carried, so the pin holds the thread rather than archiving an undelivered
			// notice or delivering a covered one again.
			pin := pumpReview776QueuePin{
				LogicalID: oldID, Names: append([]string(nil), names...), Body: body}
			if sameBody {
				pin.SHA256 = pumpReview776QueueDigests(names, texts)
				pin.Accepted = true
				if err := ctx.Err(); err != nil {
					return pumpReview776QueueLegacyNone, err
				}
				st.QueueAttempt[thread] = pin
				return pumpReview776QueueLegacyComplete, st.pumpSave(cfg)
			}
			pin.Held = true
			if err := ctx.Err(); err != nil {
				return pumpReview776QueueLegacyNone, err
			}
			st.QueueAttempt[thread] = pin
			return pumpReview776QueueLegacyHold, st.pumpSave(cfg)
		case deliverStateRefused:
			// A refusal is terminal and nothing was sent, so the batch takes its current id as usual.
			continue
		default:
			// pending or unknown: an unsettled attempt over exactly these names. Pin it so the next step
			// reconciles the attempt that was actually made instead of sending under a new id.
			pin := pumpReview776QueuePin{
				LogicalID: oldID, Names: append([]string(nil), names...), Body: body}
			if sameBody {
				pin.SHA256 = pumpReview776QueueDigests(names, texts)
			} else {
				// The attempt's text is not recoverable from the ledger, so the pin is reconciled through
				// the bridge's own receipt. It carries no per-member digest, because the text the old
				// attempt sent is not the text now on disk: an accepted answer must not complete a notice
				// nobody has evidence was delivered.
				pin.Legacy = true
			}
			if err := ctx.Err(); err != nil {
				return pumpReview776QueueLegacyNone, err
			}
			st.QueueAttempt[thread] = pin
			return pumpReview776QueueLegacyReconcile, st.pumpSave(cfg)
		}
	}
	if weak != nil {
		if action, adopted, err := pumpReview776QueueAdoptPartial(ctx, cfg, st, thread, weak.oldID, weak.record, weak.candidate, weak.carried); adopted || err != nil {
			return action, err
		}
	}
	// No prefix of today's queue is the pre-change id of an attempt. The pre-change id hashed the
	// sorted names of the whole queue at the time, and a notice the producer added later carries a
	// name it chose, so the old set need not be a prefix of today's. Nothing is adopted then: the
	// batch takes its own id, because an attempt the ledger holds under some other name set is not
	// this queue's attempt and completing its notices would archive a delivery nobody made. An
	// unsettled attempt whose names cannot be recovered is the upgrade path the issue leaves to the
	// batch's own id, not one this code can answer for.
	return pumpReview776QueueLegacyNone, nil
}

// pumpReview776QueueWeakAdoption is an unsettled pre-change record over a set of which only some members
// were unchanged since the record, kept aside while the other candidates are read.
type pumpReview776QueueWeakAdoption struct {
	oldID     string
	record    deliverRecord
	candidate pumpReview776QueueLegacyCandidate
	carried   []int
}

// pumpReview776QueueAdoptPartial adopts a pre-change record for the members of its set that were not
// written again after it (carried indexes them). The pin names only those members and carries the digest
// of the text each still holds: an accepted record completes them without a send, and an unsettled one
// is reconciled through the bridge's own receipt, which completes them when the attempt was dispatched
// and lifts the pin when it was not. The pin is marked legacy because the attempt's own text is not
// recoverable, so it is never replayed. A refused record sent nothing, so it adopts nothing and the
// reported flag is false. The replaced members are not in the pin and form a batch of their own once
// the pin settles.
func pumpReview776QueueAdoptPartial(ctx context.Context, cfg *Config, st *pumpState, thread, oldID string, record deliverRecord, candidate pumpReview776QueueLegacyCandidate, carried []int) (pumpReview776QueueLegacyAction, bool, error) {
	if record.State == deliverStateRefused {
		return pumpReview776QueueLegacyNone, false, nil
	}
	pin := pumpReview776QueuePin{LogicalID: oldID}
	var texts []string
	for _, i := range carried {
		pin.Names = append(pin.Names, candidate.names[i])
		texts = append(texts, candidate.texts[i])
	}
	pin.SHA256 = pumpReview776QueueDigests(pin.Names, texts)
	action := pumpReview776QueueLegacyReconcile
	if record.State == deliverStateAccepted {
		pin.Accepted = true
		action = pumpReview776QueueLegacyComplete
	} else {
		pin.Legacy = true
	}
	if err := ctx.Err(); err != nil {
		return pumpReview776QueueLegacyNone, false, err
	}
	st.QueueAttempt[thread] = pin
	return action, true, st.pumpSave(cfg)
}

// pumpQueueLegacyBatchID is the pre-change queue batch id: the thread and the sorted notice names,
// without the body. It is read only to find an attempt an upgrade left behind, so the record that is
// adopted is bound to the names it was actually sent over.
func pumpQueueLegacyBatchID(thread string, names []string) string {
	return pumpBatchIDStrings(append([]string{thread}, names...))
}

// pumpReview776QueuePinLift drops a pin whose delivery settled without an acceptance: a refusal is
// terminal and a local failure reached no bridge, so a later round may carry the notices again
// under a new id.
func pumpReview776QueuePinLift(cfg *Config, st *pumpState, thread string, cause error) error {
	delete(st.QueueAttempt, thread)
	if err := st.pumpSave(cfg); err != nil {
		return err
	}
	return cause
}

// pumpQueueBatchID is a queue batch's logical id: the thread, the sorted notice names and the body
// the batch carries. The thread is part of the input because the delivery ledger is shared across
// threads, so two parents queueing the same file name must not collide on one logical id. The body
// is part of it so a notice a producer replaced under the same name is a batch of its own rather
// than one the ledger answers for the text it no longer carries.
func pumpQueueBatchID(thread string, names []string, body string) string {
	parts := make([]string, 0, len(names)+2)
	parts = append(parts, thread)
	parts = append(parts, names...)
	parts = append(parts, body)
	return pumpBatchIDStrings(parts)
}

// The three observations the active-turn probe settles on. Only an explicit idle may reach the
// timeout send; anything else leaves the notices queued.
const (
	pumpQueueActive = "active"
	pumpQueueIdle   = "idle"
	pumpQueueOther  = "other"
)

// pumpQueueTurnState asks the parent thread's active turn through the same bridge the delivery
// core dials. An observation that is neither a confirmed active turn nor a confirmed idle one
// is other, and a call that fails is reported so the notices stay queued.
func pumpQueueTurnState(ctx context.Context, e *Env, cfg *Config, thread string) (string, error) {
	bridge, err := deliverDial(ctx, e, cfg)
	if err != nil {
		return pumpQueueOther, err
	}
	defer bridge.close()
	reply := bridge.call(ctx, deliverToolActive, map[string]any{"thread_id": thread})
	if reply.Err != nil {
		return pumpQueueOther, reply.Err
	}
	if reply.Payload == nil {
		return pumpQueueOther, fmt.Errorf("crw manage pump: the active-turn probe answered nothing readable")
	}
	observation, _ := reply.Payload["observation"].(string)
	turnID, _ := reply.Payload["activeTurnId"].(string)
	switch {
	case observation == "active" && turnID != "":
		return pumpQueueActive, nil
	case observation == "idle":
		return pumpQueueIdle, nil
	default:
		return pumpQueueOther, nil
	}
}

// pumpQueueSafe refuses a queue path that is a symlink, the same guard send-parent applies, so a
// link planted in the producer's place cannot redirect a notice out of the state directory.
func pumpQueueSafe(path, what string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("crw manage pump: the %s is a symlink; refusing to write through it", what)
	}
	return nil
}
