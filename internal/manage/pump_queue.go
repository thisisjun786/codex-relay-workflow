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
	pumpQueueWait            = "queued"
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
	action, err := pumpReview776QueueAdoptLegacy(cfg, st, thread, whole)
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
			Body: matched.Body, SHA256: matched.SHA256, Accepted: true}
		st.QueueAttempt[thread] = pin
		if err := st.pumpSave(cfg); err != nil {
			return err
		}
		return pumpReview776QueueFinishAccepted(ctx, e, cfg, st, dir, thread, pin, false)
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
	// The idle-parent timeout is measured on the notices the batch actually carries, so a notice
	// moved aside as oversize or left for the next round cannot age it.
	oldest, err := pumpReview776QueueOldest(dir, batch.names)
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
	logicalID := pumpQueueBatchID(thread, batch.names, batch.body)
	st.QueueAttempt[thread] = pumpReview776QueuePin{
		LogicalID: logicalID, Names: append([]string(nil), batch.names...), Body: batch.body,
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
		// A refusal is terminal and nothing was sent, so the pin lifts: the notices stay queued and
		// a later round forms a new batch under a new id.
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
				return pumpReview776QueueBatch{names: names[:i+1], texts: texts[:i+1], body: pumpReview776QueueBody(texts[:i+1])}
			}
		}
		return pumpReview776QueueBatch{}
	}
	return pumpReview776QueueBatch{names: names[:fit], texts: texts[:fit], body: pumpReview776QueueBody(texts[:fit])}
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
	// A pinned name that is gone is left out of the age and the move, but it never cancels the
	// reconciliation: the answer still decides the pin, and the removed name is handled after it.
	present := pumpReview776QueuePresent(dir, pin.Names)
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
		return true, pumpReview776QueuePinLift(cfg, st, thread, err)
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
		// The old attempt was dispatched, so its notices are completed rather than sent again.
		if err := ctx.Err(); err != nil {
			return true, err
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

// pumpReview776QueueTakeAside takes the queue name aside with one atomic rename and reports the name
// it landed under, or the empty string when the name was already gone. The aside name is
// dot-prefixed and carries no .txt suffix, so the queue collector never reads a notice this move
// holds, and it lives in the queue directory, so the rename never crosses a filesystem. It carries
// the process id and a nanosecond clock, so two moves cannot choose the same name.
func pumpReview776QueueTakeAside(dir, name string) (string, error) {
	aside := filepath.Join(dir, fmt.Sprintf(".reclaim-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Rename(filepath.Join(dir, name), aside); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return aside, nil
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
// membership shape uses.
func pumpReview776QueueMoveByName(dir string, names []string) error {
	sent := filepath.Join(dir, pumpSentDir)
	if err := os.MkdirAll(sent, 0o700); err != nil {
		return err
	}
	for _, name := range names {
		source := filepath.Join(dir, name)
		if _, err := os.Lstat(source); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if err := os.Rename(source, filepath.Join(sent, name)); err != nil {
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
)

// pumpReview776QueueAdoptLegacy looks up a pre-change logical id for a batch that has no pin, so an
// upgrade neither re-sends a delivery that may already have gone nor sends an accepted one again. It
// answers with the action the caller takes.
//
// The pre-change id hashed the names the queue held when the attempt was made, and the queue only
// grew afterwards, so the candidate sets are the name-ordered prefixes of what is queued now. They
// are tried longest first -- the most recent attempt first -- and the first id the ledger knows is
// the one that is acted on. A whole-set lookup alone would miss an attempt whose names were a
// prefix of today's queue, and that attempt's notices would then be sent again under a new id.
//
// An unsettled record is adopted whatever its message digest says. The digest is not evidence that
// the attempt never went: a record whose text is not the text now on disk is still an attempt that
// may already have been delivered, so its old id is pinned and reconciled through the bridge's own
// receipt instead of the batch taking a new id and sending the unchanged members a second time.
//
// A settled accepted record is completed, because its notices were delivered before the upgrade and
// completing them is what keeps the promise that no notice is sent twice.
func pumpReview776QueueAdoptLegacy(cfg *Config, st *pumpState, thread string, batch pumpReview776QueueBatch) (pumpReview776QueueLegacyAction, error) {
	for cut := len(batch.names); cut >= 1; cut-- {
		names, texts := batch.names[:cut], batch.texts[:cut]
		oldID := pumpQueueLegacyBatchID(thread, names)
		record, known, err := deliverLoad(cfg, oldID)
		if err != nil {
			return pumpReview776QueueLegacyNone, err
		}
		if !known {
			continue
		}
		body := pumpReview776QueueBody(texts)
		sameBody := record.MessageSHA256 == deliverMessageSHA256(body)
		switch record.State {
		case deliverStateAccepted:
			if !sameBody {
				// The text the attempt was accepted with is not the text on disk, so at least one queued
				// notice was never delivered by it. Completing by name would take an undelivered notice to
				// sent/, so this attempt is left to the new id instead.
				return pumpReview776QueueLegacyNone, nil
			}
			// The attempt's own text is still on disk, so its notices are the ones that were delivered;
			// anything queued after it is not part of it and stays queued. The accepted pin is written
			// before the first move, so a crash part way through is completed by the next round.
			st.QueueAttempt[thread] = pumpReview776QueuePin{
				LogicalID: oldID, Names: append([]string(nil), names...), Body: body,
				SHA256: pumpReview776QueueDigests(names, texts), Accepted: true}
			return pumpReview776QueueLegacyComplete, st.pumpSave(cfg)
		case deliverStateRefused:
			// A refusal is terminal and nothing was sent, so the batch takes its current id as usual.
			return pumpReview776QueueLegacyNone, nil
		default:
			// pending or unknown: an unsettled attempt. Pin the old id so the next step reconciles it
			// instead of sending under the new id.
			pin := pumpReview776QueuePin{
				LogicalID: oldID, Names: append([]string(nil), names...), Body: body}
			if sameBody {
				// The batch's own text is the text the old attempt carried, so the delivery core can
				// replay it and the per-member digests prove which files were sent.
				pin.SHA256 = pumpReview776QueueDigests(names, texts)
			} else {
				// The attempt's text is not recoverable from the ledger, so the pin is reconciled
				// through the bridge's own receipt. It carries no per-member digest, because the text the
				// old attempt sent is not the text now on disk: an accepted answer must not complete a
				// notice nobody has evidence was delivered.
				pin.Legacy = true
			}
			st.QueueAttempt[thread] = pin
			return pumpReview776QueueLegacyReconcile, st.pumpSave(cfg)
		}
	}
	return pumpReview776QueueLegacyNone, nil
}

// pumpQueueLegacyBatchID is the pre-change queue batch id: the thread and the sorted notice names,
// without the body. It is read only to find an unsettled record an upgrade left behind.
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
