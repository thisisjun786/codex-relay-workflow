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
		// A pin another build left behind may predate the search for pre-change records, and completing or
		// replaying it can move a notice a pre-change record names, after which that record is no longer a
		// set of the queue and can never be found. The search therefore runs before the pin is touched,
		// while every member is still queued (see pumpReview776QueueLegacyBeforePin).
		if !dry && !st.QueueLegacyChecked[thread] && len(pin.Overlap) == 0 {
			stop, err := pumpReview776QueueLegacyBeforePin(ctx, e, cfg, st, dir, thread, pin)
			if err != nil || stop {
				return err
			}
		}
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
	} else if err := pumpQueueCompleteAccepted(ctx, cfg, st, dir, thread); err != nil {
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
	texts, modTimes, err := pumpReview776QueueReadNotices(dir, names)
	if err != nil {
		return err
	}
	// An upgraded state has no pin, but the ledger may still hold a record under a pre-change
	// logical id. That lookup runs on the full set as it stands before any notice is moved aside and
	// before the size split, because the pre-change id hashed every queued name: a narrower set would
	// miss an old record and re-send an attempt that may already have gone. The lookup judges each
	// member on the modification time read with its text, so a notice the producer replaced since the
	// read cannot lend its newer time to the older text.
	whole := pumpReview776QueueBatch{names: names, texts: texts, body: pumpReview776QueueBody(texts), modTimes: modTimes}
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
	action, err := pumpReview776QueueAdoptLegacy(ctx, e, cfg, st, dir, thread, whole)
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
		// The adoption wrote the accepted pin, which carries the digests of the text the record proves it
		// delivered.
		return pumpReview776QueueFinishAccepted(ctx, e, cfg, st, dir, thread, st.QueueAttempt[thread], false)
	case pumpReview776QueueLegacyWait:
		// Pre-change attempts hold the thread, had notices completed, or could not be identified: the
		// queue the round read is no longer one it may send from, so nothing more is sent this round.
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
		// A local failure reached no bridge, so the pin lifts -- unless the round was cancelled, which
		// may have stopped a send part way: the pin then stays for the next round to reconcile.
		return pumpReview776QueuePinLift(ctx, cfg, st, thread, err)
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
		return pumpReview776QueuePinLift(ctx, cfg, st, thread, err)
	default:
		// Unknown: the pin stays, so the next round reconciles the same logical id and body.
		return err
	}
}

// pumpQueueCompleteAccepted moves a legacy membership's notices into sent/ by name. A membership
// written before the pin carried digests names its members only, so it is completed as it was
// written. A membership the pin wrote is completed by pumpReview776QueueFinishAccepted instead.
//
// The membership's batch was the whole queue when it was sent, and the pre-change pump moved notices only
// after an accepted answer, so every notice an earlier pre-change record carried that is still queued is
// one of the membership's members: completing it first cannot hide a record the later search would need.
func pumpQueueCompleteAccepted(ctx context.Context, cfg *Config, st *pumpState, dir, thread string) error {
	names := st.QueueAccepted[thread]
	if len(names) == 0 {
		return nil
	}
	// The moves, the sent/ directory and the membership clear are durable effects: a cancelled round
	// makes none of them.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pumpReview776QueueMoveByName(ctx, dir, names); err != nil {
		return err
	}
	// A round cancelled after the last move keeps the membership: the next round finds those notices
	// gone and clears it then.
	if err := ctx.Err(); err != nil {
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
	// modTimes is the modification time of the file each text was read from, taken through the same
	// open handle as the text. Only the whole queue the round read carries it, for the legacy lookup.
	modTimes []time.Time
}

// pumpReview776QueueReadNotices reads one thread's notices in name order, each with the
// modification time of the file its text came from. A notice is a regular file the producer wrote; a
// symlink would let the queue carry the contents of a file outside it, so it is refused rather than
// followed.
func pumpReview776QueueReadNotices(dir string, names []string) ([]string, []time.Time, error) {
	texts := make([]string, 0, len(names))
	modTimes := make([]time.Time, 0, len(names))
	for _, name := range names {
		text, modified, err := pumpReview776QueueReadNotice(dir, name)
		if err != nil {
			return nil, nil, err
		}
		texts = append(texts, text)
		modTimes = append(modTimes, modified)
	}
	return texts, modTimes, nil
}

// pumpReview776QueueReadTries bounds the re-reads of one notice the producer keeps replacing, so a
// read fails loudly instead of spinning.
const pumpReview776QueueReadTries = 8

// pumpReview776QueueReadNotice reads one notice's trimmed text and the modification time of the very
// file the text came from. Both come from one open handle: the producer's --queue write is an atomic
// rename onto the name, so a time read from the path after the text could belong to a newer notice,
// and pairing an older text with that newer time would make a ledger record that carried the older
// text look stale (the record is judged on the time, the completion on the text). A handle whose file
// is no longer the one the name held when it was checked, or whose file changed while it was read, is
// read again.
func pumpReview776QueueReadNotice(dir, name string) (string, time.Time, error) {
	path := filepath.Join(dir, name)
	for attempt := 0; attempt < pumpReview776QueueReadTries; attempt++ {
		info, err := os.Lstat(path)
		if err != nil {
			return "", time.Time{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", time.Time{}, fmt.Errorf("crw manage pump: the notice %s is a symlink; refusing to send through it", name)
		}
		text, modified, same, err := pumpReview776QueueReadHandle(path, info)
		if err != nil {
			return "", time.Time{}, err
		}
		if same {
			return text, modified, nil
		}
	}
	return "", time.Time{}, fmt.Errorf("crw manage pump: the notice %s kept changing while it was read", name)
}

// pumpReview776QueueReadHandle reads the file at path through one handle and reports whether that
// file is the one checked (the same file, unchanged from before the read to after it).
func pumpReview776QueueReadHandle(path string, checked os.FileInfo) (string, time.Time, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, false, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return "", time.Time{}, false, err
	}
	if !os.SameFile(checked, before) {
		return "", time.Time{}, false, nil
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return "", time.Time{}, false, err
	}
	after, err := file.Stat()
	if err != nil {
		return "", time.Time{}, false, err
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		return "", time.Time{}, false, nil
	}
	return strings.TrimSpace(string(raw)), before.ModTime(), true, nil
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
		if len(pin.Overlap) > 0 {
			fmt.Fprintf(e.Stdout, "queue %s: %d notices wait on %d pre-change attempts\n", thread, len(pin.Names), len(pin.Overlap))
			return true, nil
		}
		fmt.Fprintf(e.Stdout, "queue %s %d notices\n%s\n", thread, len(pin.Names), pin.Body)
		return true, nil
	}
	if len(pin.Overlap) > 0 {
		// The pre-change attempts are reconciled together, every round, from their own answers, held or
		// not: the pin is never sent under, so no gate applies, and a held pin still completes a notice a
		// provable record shows delivered. Only a pin that settled with nothing delivered and nothing held
		// lets the round go on to form a new batch.
		outcome, err := pumpReview776QueueSettleOverlap(ctx, e, cfg, st, dir, thread, pin, true)
		return err != nil || outcome != pumpReview776QueueOverlapFree, err
	}
	if pin.Held {
		// The pre-change attempt was accepted but its text is not recoverable, so no queued notice can
		// be told apart from one it already carried. The thread waits, which loses nothing and
		// duplicates nothing; nothing is sent and no bridge process starts.
		return true, nil
	}
	if pin.Legacy {
		// A legacy pin without an overlap cannot prove which text its attempt carried, so no answer about
		// it -- the ledger's or the bridge's -- may complete or resend a queued notice.
		return pumpReview776QueueSettleLegacy(ctx, cfg, st, thread, pin)
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
// A legacy pin, taken for a pre-change ledger record whose text the ledger does not store, is never
// replayed with the notice text on disk (see pumpReview776QueueSettleLegacy).
func pumpReview776QueueSettle(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, pin pumpReview776QueuePin) (bool, error) {
	if pin.Legacy {
		return pumpReview776QueueSettleLegacy(ctx, cfg, st, thread, pin)
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

// pumpReview776QueueSettleLegacy settles a legacy pin without an overlap: one an earlier build of the
// queue took for a pre-change record whose text the ledger does not store, without per-member digests.
// Such a record cannot prove which text it carried, so its receipt is no evidence for any queued
// notice: an accepted answer cannot show which text went, and an answer that nothing went cannot show
// which text went nowhere. The pin is held, with a legacy_unprovable_hold line, and the thread sends
// nothing until an operator settles it. A cancelled round writes nothing.
func pumpReview776QueueSettleLegacy(ctx context.Context, cfg *Config, st *pumpState, thread string, pin pumpReview776QueuePin) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	pin.Held = true
	st.QueueAttempt[thread] = pin
	if err := st.pumpSave(cfg); err != nil {
		return true, err
	}
	pumpReview776QueueLogHold(cfg, thread, pin)
	return true, nil
}

// pumpReview776QueueFinishAccepted completes an accepted batch: it moves to sent/ only the members
// whose current file still carries the digest that was sent, leaves a replaced member queued with a
// log line, and clears the pin. It runs from the accepted send and from a later round that finds an
// accepted pin.
//
// Every move and the pin clear is a durable change, so the cancellation is checked before each of
// them: a round told to stop part way through moves nothing more and keeps the accepted pin, and the
// next round completes the rest from it without a send (a member already moved is gone and is passed
// over).
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
		if err := ctx.Err(); err != nil {
			return err
		}
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
	if err := ctx.Err(); err != nil {
		return err
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
		if err := st.pumpSave(cfg); err != nil {
			return true, err
		}
		pumpReview776QueueLogHold(cfg, thread, pin)
		return true, nil
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
		return pumpReview776QueuePinLift(ctx, cfg, st, thread, cause)
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
		return pumpReview776QueuePinLift(ctx, cfg, st, thread, cause)
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
		// Each move is durable: a round told to stop moves nothing more, and the pin and the count stay
		// for the next round, which finds the moved notices gone and moves the rest.
		if err := ctx.Err(); err != nil {
			return err
		}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(st.QueueRefused, thread)
	return pumpReview776QueuePinLift(ctx, cfg, st, thread, cause)
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
func pumpReview776QueueMoveByName(ctx context.Context, dir string, names []string) error {
	// The sent/ directory is a durable change too, so a cancelled round does not create it.
	if err := ctx.Err(); err != nil {
		return err
	}
	sent := filepath.Join(dir, pumpSentDir)
	if err := os.MkdirAll(sent, 0o700); err != nil {
		return err
	}
	for _, name := range names {
		// Each move is durable, so a round told to stop moves nothing more.
		if err := ctx.Err(); err != nil {
			return err
		}
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
	// pumpReview776QueueLegacyNone: no pre-change record carried a queued notice -- there is none, it was
	// refused, or it was written before every member it names was written again. The batch is sent
	// under its current id as usual.
	pumpReview776QueueLegacyNone pumpReview776QueueLegacyAction = iota
	// pumpReview776QueueLegacyReconcile: the only record that carried queued notices proves its text and
	// is unsettled. Its old id and body are pinned, so the attempt is reconciled -- and sent once under
	// that id when it never went -- rather than re-sent under a new id.
	pumpReview776QueueLegacyReconcile
	// pumpReview776QueueLegacyComplete: the only record that carried queued notices proves its text and
	// was accepted. Its notices were delivered before the upgrade, so they are moved to sent/ instead of
	// being sent again.
	pumpReview776QueueLegacyComplete
	// pumpReview776QueueLegacyWait: several records, or one that cannot prove its text, are reconciled
	// under an overlap pin that holds the thread or completed notices this round; or the queue is too
	// large for the search and the ledger holds a record that may cover it. The round sends nothing.
	pumpReview776QueueLegacyWait
)

// pumpReview776QueueLegacyCandidate is one set of queued notice names a pre-change record's id may
// hash, in name order, with each member's text and the modification time read with that text.
type pumpReview776QueueLegacyCandidate struct {
	names, texts []string
	modTimes     []time.Time
}

// pumpQueueLegacyScanLimit is the most queued notices the search for pre-change records matches: it
// hashes every non-empty subset of the queue's names, 2^n - 1 ids, so a larger queue is not searched.
const pumpQueueLegacyScanLimit = 16

// pumpQueueLegacyScanLimitHold names the log line of a thread whose queue is too large for the search
// while the ledger holds a record that could have carried one of its notices, for the operator.
const pumpQueueLegacyScanLimitHold = "legacy_scan_limit_hold"

// pumpReview776QueueLegacyScan finds every pre-change record that carried a notice of the queue the
// round read. The pre-change id hashed the thread and the sorted names of the whole queue at the time,
// and the queue has changed since in ways no order recovers: a notice added later may sort anywhere,
// and a member the producer replaced atomically takes a new modification time, so the old set need be
// neither a name-ordered nor an oldest-first prefix of today's queue. The search therefore does not
// guess which set an attempt covered: it hashes every non-empty subset of today's names and looks each
// id up among the ledger's records (one directory read, then a lookup per id the ledger holds). A
// record over a set that names a notice no longer queued is not found: the pre-change pump moved a
// batch's notices only all together, after an accepted answer, so such a record carried nothing that
// is still queued; and this build runs the search before any move of its own can take a notice out of
// the queue -- before a batch is formed, and before a pin another build left is acted on
// (pumpReview776QueueLegacyBeforePin) -- and keeps every record it found that still matters in a stored
// overlap pin before the first move, so no move of the queue's own hides a record it has to answer for.
//
// A record is kept when it carried at least one member (carriedBy) -- a record written before every
// member it names was written again carried none of the texts queued now -- unless it was refused and
// proves its text, which shows it sent nothing; a refused record that cannot prove its text is kept, so
// it holds the thread like any other. The records come longest set first. over reports a queue larger than
// pumpQueueLegacyScanLimit, which is not searched.
//
// Each member's time is the one read with its text (batch.modTimes), never one read again from the
// path: a notice the producer replaced since the read would otherwise pair the older text with the
// newer time, and the record that carried the older text would look stale. A batch without those
// times is refused rather than judged on other times.
func pumpReview776QueueLegacyScan(cfg *Config, thread string, batch pumpReview776QueueBatch) (found []pumpReview776QueueLegacyFound, over bool, err error) {
	if len(batch.modTimes) != len(batch.names) || len(batch.texts) != len(batch.names) {
		return nil, false, fmt.Errorf("crw manage pump: the queue snapshot has %d names, %d texts and %d modification times", len(batch.names), len(batch.texts), len(batch.modTimes))
	}
	if len(batch.names) > pumpQueueLegacyScanLimit {
		return nil, true, nil
	}
	ids, err := pumpReview776QueueOutboxIDs(cfg)
	if err != nil || len(ids) == 0 {
		return nil, false, err
	}
	n := len(batch.names)
	for mask := 1; mask < 1<<n; mask++ {
		var candidate pumpReview776QueueLegacyCandidate
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				// batch.names is name-ordered, so the set is listed in the order the pre-change id hashed.
				candidate.names = append(candidate.names, batch.names[i])
				candidate.texts = append(candidate.texts, batch.texts[i])
				candidate.modTimes = append(candidate.modTimes, batch.modTimes[i])
			}
		}
		oldID := pumpQueueLegacyBatchID(thread, candidate.names)
		if !ids[oldID] {
			continue
		}
		record, known, err := deliverLoad(cfg, oldID)
		if err != nil {
			return nil, false, err
		}
		if !known {
			continue
		}
		carried := candidate.carriedBy(record.CreatedAt)
		if len(carried) == 0 {
			// Every member was last modified after the record was written, so the attempt carried none of
			// the texts queued now: it can neither pin a stale attempt nor hold the thread.
			continue
		}
		f := pumpReview776QueueLegacyFound{oldID: oldID, record: record, candidate: candidate, carried: carried}
		if record.State == deliverStateRefused && pumpReview776QueueLegacyUnprovable(f) == "" {
			// A refusal of a record that proves its text sent nothing, so its notices are sent once (R1). A
			// refused record that cannot prove its text is kept: its answer never authorises a send (R2).
			continue
		}
		found = append(found, f)
	}
	sort.SliceStable(found, func(a, b int) bool {
		if la, lb := len(found[a].candidate.names), len(found[b].candidate.names); la != lb {
			return la > lb
		}
		return found[a].oldID < found[b].oldID
	})
	return found, false, nil
}

// pumpReview776QueueOutboxIDs is the set of logical ids the delivery ledger holds a record for, read
// from the outbox directory's names.
func pumpReview776QueueOutboxIDs(cfg *Config) (map[string]bool, error) {
	if err := deliverOutboxDirSafe(cfg); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Dir(deliverOutboxPath(cfg, "id")))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	ids := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), ".json"); ok && deliverPathComponent(id, "logical id") == nil {
			ids[id] = true
		}
	}
	return ids, nil
}

// pumpReview776QueueLegacyUnidentified lists the records a queue too large for the search may still
// have to answer for: a record for the thread written no earlier than the second of the oldest queued
// notice's modification, so it could have carried a notice queued now. A refused one is listed too: its
// members are unknown, so it cannot prove its text, and its refusal does not authorise a send (R2). A
// stamp that does not parse is no evidence against the record. Every record in the ledger is read.
func pumpReview776QueueLegacyUnidentified(cfg *Config, thread string, batch pumpReview776QueueBatch) ([]deliverRecord, error) {
	ids, err := pumpReview776QueueOutboxIDs(cfg)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var oldest time.Time
	for i, modified := range batch.modTimes {
		if i == 0 || modified.Before(oldest) {
			oldest = modified
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	var out []deliverRecord
	for _, id := range sorted {
		record, known, err := deliverLoad(cfg, id)
		if err != nil {
			return nil, err
		}
		if !known || record.TargetThread != thread {
			continue
		}
		if created, err := time.Parse(time.RFC3339, record.CreatedAt); err == nil && oldest.Truncate(time.Second).After(created) {
			continue
		}
		out = append(out, record)
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

// pumpReview776QueueAdoptLegacy looks for the pre-change attempts a batch that has no pin still has to
// answer for, so an upgrade neither re-sends a delivery that may already have gone nor sends an
// accepted one again. It answers with the action the caller takes.
//
// The pre-change id hashed the thread and the sorted notice names of the batch that was actually
// sent, so a record is only acted on when its logical id is exactly the pre-change id of the names
// it is being applied to; pumpReview776QueueLegacyScan finds every such record over any set of today's
// names. The id check is what keeps a record for another batch -- a direct send-parent delivery, or one
// over different names -- from completing notices it never carried.
//
// A record is handled automatically only when it proves exactly which text it carried
// (pumpReview776QueueLegacyUnprovable): it carried every member its id names and its message digest is
// the digest of those members' text on disk. The only such record is adopted by itself
// (pumpReview776QueueAdoptOne): accepted, its notices are completed; unsettled, it is replayed under its
// own id, which sends it once when it never went. Several are reconciled together under one overlap
// pin (pumpReview776QueueSettleOverlap), which keeps the invariant per notice:
//
//   - I1: a notice a provable record shows delivered is never sent again; it is completed.
//   - I2: a notice a provable record that carried it has no settled answer for is not sent.
//   - I3: a notice every record that carried it shows never went is sent once, in a new batch.
//
// A record that cannot prove its text -- one that carried only part of its members, or one whose
// digest is not the digest of its members' text on disk -- holds the thread: the overlap pin is marked
// held, nothing is sent from the queue until an operator settles it, and a legacy_unprovable_hold line
// names the attempt, its members and why it cannot prove its text. Its answer completes nothing and
// never lets a notice be sent, because no notice's current text is shown to be the text it delivered or
// the text it did not; a notice a provable record shows delivered is still completed under the hold.
// The round never falls through to a new batch while such a record is there.
//
// A queue too large for the search is held for the round, with a legacy_scan_limit_hold line, while
// the ledger holds any record for the thread that could have carried one of its notices; the members of
// such a record are unknown, so it is treated as one that cannot prove its text.
//
// A search that finds nothing to answer for -- no record, or only records that settled as never
// delivered with none holding the thread -- marks the thread searched, and the search does not run for it
// again: the pre-change pump no longer writes records, and a notice queued later is newer than every
// record it wrote, so it can never be one such a record carried.
func pumpReview776QueueAdoptLegacy(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, batch pumpReview776QueueBatch) (pumpReview776QueueLegacyAction, error) {
	if st.QueueLegacyChecked[thread] {
		return pumpReview776QueueLegacyNone, nil
	}
	found, over, err := pumpReview776QueueLegacyScan(cfg, thread, batch)
	if err != nil {
		return pumpReview776QueueLegacyWait, err
	}
	if over {
		records, err := pumpReview776QueueLegacyUnidentified(cfg, thread, batch)
		if err != nil {
			return pumpReview776QueueLegacyWait, err
		}
		for _, record := range records {
			pumpLog(cfg, fmt.Sprintf("queue %s %s: the pre-change attempt %s (state %s, written %s) may have carried a queued notice, but the queue holds %d notices, more than the %d the search for its members can match; its members are unknown and it cannot prove its text, so nothing is sent until an operator settles it",
				thread, pumpQueueLegacyScanLimitHold, record.LogicalID, record.State, record.CreatedAt, len(batch.names), pumpQueueLegacyScanLimit))
		}
		if len(records) > 0 {
			return pumpReview776QueueLegacyWait, nil
		}
	}
	if len(found) == 0 {
		// The mark is written with the round's next save (the pin of the batch it sends), not on its own:
		// a round that saves nothing leaves it unwritten, and the next round searches again and finds the
		// same nothing.
		st.QueueLegacyChecked[thread] = true
		return pumpReview776QueueLegacyNone, nil
	}
	if len(found) == 1 && pumpReview776QueueLegacyUnprovable(found[0]) == "" {
		return pumpReview776QueueAdoptOne(ctx, cfg, st, thread, found[0])
	}
	pin := pumpReview776QueueOverlapPin(found)
	outcome, err := pumpReview776QueueSettleOverlap(ctx, e, cfg, st, dir, thread, pin, false)
	if err != nil {
		return pumpReview776QueueLegacyWait, err
	}
	if outcome == pumpReview776QueueOverlapFree {
		// Every record found settled as never delivered and none holds the thread, so there is nothing
		// left to answer for: the batch this round sends carries those notices once (I3), and the thread is
		// marked searched with that batch's pin, so its later completion cannot hide a record that still
		// mattered.
		st.QueueLegacyChecked[thread] = true
		return pumpReview776QueueLegacyNone, nil
	}
	return pumpReview776QueueLegacyWait, nil
}

// pumpReview776QueueOverlapPin is the overlap pin over the records the search found: their refs, the
// notices they carried and those notices' digests. Every candidate set comes from the one snapshot the
// round read, so a name's text, and its digest, is the same whichever record lists it. A record that
// cannot prove its text marks the pin held.
func pumpReview776QueueOverlapPin(found []pumpReview776QueueLegacyFound) pumpReview776QueuePin {
	pin := pumpReview776QueuePin{LogicalID: found[0].oldID, Legacy: true, SHA256: map[string]string{}}
	for _, f := range found {
		ref := pumpReview776QueueLegacyRef{LogicalID: f.oldID, Members: append([]string(nil), f.candidate.names...)}
		for _, i := range f.carried {
			name := f.candidate.names[i]
			ref.Names = append(ref.Names, name)
			if _, listed := pin.SHA256[name]; !listed {
				pin.Names = append(pin.Names, name)
				pin.SHA256[name] = pumpReview776BodyDigest(f.candidate.texts[i])
			}
		}
		if reason := pumpReview776QueueLegacyUnprovable(f); reason != "" {
			ref.Unprovable, ref.Reason = true, reason
			pin.Held = true
		}
		pin.Overlap = append(pin.Overlap, ref)
	}
	sort.Strings(pin.Names)
	return pin
}

// pumpReview776QueueLegacyBeforePin runs the search for pre-change records on a thread that holds a pin
// but was never searched, before the pin is completed, replayed or lifted. It reports whether the
// thread is settled for this round.
//
// A pin is taken only after the search in this build, so such a pin is one another build left: an old
// id pin an earlier adoption wrote without the evidence of the other records, or a size-split prefix
// pin written before any search. Acting on it first loses evidence for good. Replaying an old id pin
// whose record says nothing went resends a notice a shorter record shows delivered; completing an
// accepted prefix pin moves a member another record names, and that record is then a set of no queue
// the search can match, so the notices it delivered would form a new batch.
//
// The search therefore reads the whole queue while every member is still in it. When it finds nothing,
// or only the pin's own record (the pin then is the whole evidence, frozen when it was taken), the
// thread is handled through the pin as before. Otherwise the pin is folded into the overlap pin of the
// found records as one more ref -- its attempt answered by its accepted mark, else by the ledger and the
// bridge like any record, its notices those still carrying the digest it sent -- and the overlap
// reconciliation keeps I1-I3 over the whole set: a notice any of them shows delivered is completed, a
// notice one of them has no answer for waits, and a record or pin that cannot prove its text holds the
// thread (R2). A pin that cannot prove its text (a legacy pin, a held one, or one without digests) is
// such a ref. When the whole set settles with nothing delivered, held or waiting, the pin's attempt went
// nowhere either: the pin lifts and the next round forms its batch under the search as usual.
//
// A queue too large for the search holds the round, with a legacy_scan_limit_hold line, while the ledger
// keeps a record for the thread other than the pin's own that could have carried a queued notice.
func pumpReview776QueueLegacyBeforePin(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, pin pumpReview776QueuePin) (bool, error) {
	names, err := pumpQueueSortedNames(dir)
	if err != nil {
		return true, err
	}
	if len(names) == 0 {
		// Nothing is queued, so no record carried a queued notice, and a notice queued later is newer than
		// every pre-change record.
		st.QueueLegacyChecked[thread] = true
		return false, nil
	}
	texts, modTimes, err := pumpReview776QueueReadNotices(dir, names)
	if err != nil {
		return true, err
	}
	whole := pumpReview776QueueBatch{names: names, texts: texts, body: pumpReview776QueueBody(texts), modTimes: modTimes}
	found, over, err := pumpReview776QueueLegacyScan(cfg, thread, whole)
	if err != nil {
		return true, err
	}
	if over {
		records, err := pumpReview776QueueLegacyUnidentified(cfg, thread, whole)
		if err != nil {
			return true, err
		}
		held := false
		for _, record := range records {
			if record.LogicalID == pin.LogicalID {
				continue
			}
			held = true
			pumpLog(cfg, fmt.Sprintf("queue %s %s: the pre-change attempt %s (state %s, written %s) may have carried a queued notice, but the queue holds %d notices, more than the %d the search for its members can match; its members are unknown and it cannot prove its text, so the pin %s is not acted on and nothing is sent until an operator settles it",
				thread, pumpQueueLegacyScanLimitHold, record.LogicalID, record.State, record.CreatedAt, len(names), pumpQueueLegacyScanLimit, pin.LogicalID))
		}
		if held {
			return true, nil
		}
	}
	own := -1
	for i, f := range found {
		if f.oldID == pin.LogicalID {
			own = i
		}
	}
	if len(found) == 0 || (len(found) == 1 && own == 0) {
		if len(found) == 0 {
			st.QueueLegacyChecked[thread] = true
		}
		return false, nil
	}
	overlap := pumpReview776QueueOverlapPin(found)
	if own >= 0 {
		// The pin is one of the found records; the ref built from the snapshot stands for it, with the
		// pin's own accepted mark.
		for i := range overlap.Overlap {
			if overlap.Overlap[i].LogicalID == pin.LogicalID {
				overlap.Overlap[i].Accepted = pin.Accepted
			}
		}
	} else {
		current := pumpReview776QueueDigests(names, texts)
		provable := !pin.Legacy && !pin.Held && len(pin.SHA256) > 0
		ref := pumpReview776QueueLegacyRef{LogicalID: pin.LogicalID, Members: append([]string(nil), pin.Names...), Accepted: pin.Accepted}
		for _, name := range pin.Names {
			digest, queued := current[name]
			if !queued || (provable && pin.SHA256[name] != digest) {
				// Gone, or written again since the pin was taken: not a notice its attempt carried.
				continue
			}
			ref.Names = append(ref.Names, name)
			if _, listed := overlap.SHA256[name]; !listed {
				overlap.Names = append(overlap.Names, name)
				overlap.SHA256[name] = digest
			}
		}
		if !provable {
			ref.Unprovable = true
			ref.Reason = "the pin an earlier build left for this attempt does not carry the digest of the text it sent"
			overlap.Held = true
		}
		overlap.Overlap = append(overlap.Overlap, ref)
		sort.Strings(overlap.Names)
	}
	pumpLog(cfg, fmt.Sprintf("queue %s: the pin %s is reconciled together with the pre-change attempts the search found", thread, pin.LogicalID))
	outcome, err := pumpReview776QueueSettleOverlap(ctx, e, cfg, st, dir, thread, overlap, false)
	if err != nil {
		return true, err
	}
	if outcome == pumpReview776QueueOverlapFree {
		return true, pumpReview776QueuePinLift(ctx, cfg, st, thread, nil)
	}
	return true, nil
}

// pumpReview776QueueLegacyUnprovable says why a pre-change record cannot prove which text it carried,
// or nothing when it can: it carried every member its id names and its message digest is the digest of
// the body their text on disk forms. Nothing weaker is proof. A record that carried only some members
// (the others were written again after it) has a digest over texts that are gone, and a member's
// modification time older than the stamp does not show the record carried the text it holds now: the
// old pump read the queue, then probed the parent's turn, and only then did the delivery core stamp the
// record, so an atomic --queue write in that window leaves a notice older than the stamp that the
// attempt never carried.
func pumpReview776QueueLegacyUnprovable(f pumpReview776QueueLegacyFound) string {
	if len(f.carried) != len(f.candidate.names) {
		carried := make(map[int]bool, len(f.carried))
		for _, i := range f.carried {
			carried[i] = true
		}
		var later []string
		for i, name := range f.candidate.names {
			if !carried[i] {
				later = append(later, name)
			}
		}
		return fmt.Sprintf("its members %s were written again after it, so its digest covers text that is gone", strings.Join(later, ","))
	}
	if f.record.MessageSHA256 != deliverMessageSHA256(pumpReview776QueueBody(f.candidate.texts)) {
		return "its message digest is not the digest of its members' text on disk"
	}
	return ""
}

// pumpQueueLegacyUnprovableHold names the log line of a thread a pin holds because a pre-change attempt
// cannot prove which text it carried, for the operator.
const pumpQueueLegacyUnprovableHold = "legacy_unprovable_hold"

// pumpReview776QueueLogHold logs a held pin with the notices it holds and the attempt it holds on.
func pumpReview776QueueLogHold(cfg *Config, thread string, pin pumpReview776QueuePin) {
	pumpLog(cfg, fmt.Sprintf("queue %s %s: the pre-change attempt %s cannot prove which text of %s it carried; nothing is sent or completed until an operator settles it",
		thread, pumpQueueLegacyUnprovableHold, pin.LogicalID, strings.Join(pin.Names, ",")))
}

// pumpReview776QueueLegacyFound is one pre-change record a candidate set of today's queue matched: its
// id, the record, the set and the indexes of the members it carried.
type pumpReview776QueueLegacyFound struct {
	oldID     string
	record    deliverRecord
	candidate pumpReview776QueueLegacyCandidate
	carried   []int
}

// pumpReview776QueueAdoptOne adopts the only pre-change record that carried a queued notice, one that
// proves its text (pumpReview776QueueLegacyUnprovable): it carried every member, so the pin names them
// all, with that body and the per-member digests. An accepted record is completed, the only case where
// every pinned member is known to have been delivered; an unsettled one is replayed under its own id,
// which reconciles it and sends it once when it never went.
func pumpReview776QueueAdoptOne(ctx context.Context, cfg *Config, st *pumpState, thread string, f pumpReview776QueueLegacyFound) (pumpReview776QueueLegacyAction, error) {
	names := append([]string(nil), f.candidate.names...)
	texts := f.candidate.texts
	pin := pumpReview776QueuePin{LogicalID: f.oldID, Names: names, Body: pumpReview776QueueBody(texts), SHA256: pumpReview776QueueDigests(names, texts)}
	action := pumpReview776QueueLegacyReconcile
	if f.record.State == deliverStateAccepted {
		pin.Accepted = true
		action = pumpReview776QueueLegacyComplete
	}
	if err := ctx.Err(); err != nil {
		return pumpReview776QueueLegacyWait, err
	}
	st.QueueAttempt[thread] = pin
	if err := st.pumpSave(cfg); err != nil {
		return pumpReview776QueueLegacyWait, err
	}
	return action, nil
}

// pumpReview776QueueOverlapOutcome is what reconciling an overlap pin left for the round.
type pumpReview776QueueOverlapOutcome int

const (
	// pumpReview776QueueOverlapFree: no record shows a notice delivered, none is undetermined and none
	// holds the thread, so there is no pin and the notices may form a new batch this round.
	pumpReview776QueueOverlapFree pumpReview776QueueOverlapOutcome = iota
	// pumpReview776QueueOverlapDone: the delivered notices were completed and nothing is undetermined, so
	// the pin is gone; the queue changed under the round, which therefore sends nothing more.
	pumpReview776QueueOverlapDone
	// pumpReview776QueueOverlapHold: the pin holds the thread.
	pumpReview776QueueOverlapHold
	// pumpReview776QueueOverlapChanged: a pin not yet stored found nothing to complete, hold or wait on,
	// but a notice a record carried no longer holds the text the round read. Nothing was written; the
	// round sends nothing, because the batch it would form is the text it read, which a record may have
	// delivered, and the next round reads the queue as it now is.
	pumpReview776QueueOverlapChanged
)

// pumpQueueLegacyOverlapHold names the log line of a thread an overlap pin waits on, for the operator.
const pumpQueueLegacyOverlapHold = "legacy_overlap_hold"

// pumpReview776QueueOverlapVerdict is one pre-change record's answer in one round.
type pumpReview776QueueOverlapVerdict int

const (
	pumpReview776QueueOverlapUndetermined pumpReview776QueueOverlapVerdict = iota
	pumpReview776QueueOverlapDelivered
	pumpReview776QueueOverlapNotDelivered
)

// pumpReview776QueueSettleOverlap reconciles an overlap pin: the pre-change records that carried
// queued notices. stored says whether the pin is already in the state; a pin that is not is written
// only when the round has something to complete, to wait on or to hold, and always before the first
// move, so a crash part way leaves the evidence set behind instead of a queue whose records can no
// longer be found.
//
// Every provable record's answer is read each round: the ledger's settled answer, else the bridge's own
// receipt. A notice counts only while its file still carries the text the records were matched against;
// one the producer wrote again since is a notice none of them carried (and a round that still holds the
// text it read before that write sends nothing, see pumpReview776QueueOverlapChanged). A notice a
// provable record shows delivered is completed (I1). A notice a provable record that carried it has no
// settled answer for keeps the pin (I2): the thread waits, with a legacy_overlap_hold line naming the
// notices and the records, and nothing is sent -- not even a notice that is free -- because a hold that
// sent around it would have to form a batch the pin does not describe. Otherwise the pin goes, and every
// notice still queued is one every record that carried it shows never went, which the next batch sends
// once under its own id (I3).
//
// A pin with a record that cannot prove its text is held (see pumpReview776QueueAdoptLegacy): that
// record's answer is not read, because it is evidence for no notice, the proven notices are completed
// each round, and nothing else is completed or sent, with a legacy_unprovable_hold line per such record,
// until an operator settles it.
//
// This is the fail-closed form of the reconciliation: it never replays an old id and never sends while
// any record is undetermined or the pin is held, so no combination of answers can deliver a notice
// twice; the cost is that a free notice waits with the others until the undetermined records settle,
// and stays queued behind a held pin for the operator.
func pumpReview776QueueSettleOverlap(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, pin pumpReview776QueuePin, stored bool) (pumpReview776QueueOverlapOutcome, error) {
	verdicts, err := pumpReview776QueueOverlapVerdicts(ctx, e, cfg, thread, pin.Overlap)
	if err != nil {
		return pumpReview776QueueOverlapHold, err
	}
	intact := map[string]bool{}
	for _, name := range pin.Names {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return pumpReview776QueueOverlapHold, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return pumpReview776QueueOverlapHold, err
		}
		intact[name] = pumpReview776BodyDigest(string(raw)) == pin.SHA256[name]
	}
	// Each notice is classed by the provable records that carried it, one record at a time, so one
	// record's uncertainty never hides another record's proof: proven when one shows it delivered,
	// undetermined when one that carried it has no settled answer.
	held := pin.Held
	proven, undetermined := map[string]bool{}, map[string]bool{}
	var waitingOn []string
	for i, ref := range pin.Overlap {
		if ref.Unprovable {
			held = true
			continue
		}
		for _, name := range ref.Names {
			if !intact[name] {
				continue
			}
			switch verdicts[i] {
			case pumpReview776QueueOverlapDelivered:
				proven[name] = true
			case pumpReview776QueueOverlapUndetermined:
				undetermined[name] = true
				if !pumpReview776QueueListed(waitingOn, ref.LogicalID) {
					waitingOn = append(waitingOn, ref.LogicalID)
				}
			}
		}
	}
	var complete, waiting []string
	for _, name := range pin.Names {
		switch {
		case proven[name]:
			complete = append(complete, name)
		case undetermined[name]:
			waiting = append(waiting, name)
		}
	}
	if held {
		// The held pin is durable before the first move, so a crash part way leaves the hold behind, and
		// the next round completes the rest of the proven notices under it.
		if !stored || !pin.Held {
			if err := ctx.Err(); err != nil {
				return pumpReview776QueueOverlapHold, err
			}
			pin.Held = true
			st.QueueAttempt[thread] = pin
			if err := st.pumpSave(cfg); err != nil {
				return pumpReview776QueueOverlapHold, err
			}
		}
		if err := pumpReview776QueueCompleteProven(ctx, cfg, dir, thread, pin, complete); err != nil {
			return pumpReview776QueueOverlapHold, err
		}
		for _, ref := range pin.Overlap {
			if ref.Unprovable {
				pumpLog(cfg, fmt.Sprintf("queue %s %s: the pre-change attempt %s over %s carried %s and cannot prove which text it delivered: %s; nothing is sent and nothing it carried is completed until an operator settles it",
					thread, pumpQueueLegacyUnprovableHold, ref.LogicalID, strings.Join(ref.Members, ","), strings.Join(ref.Names, ","), ref.Reason))
			}
		}
		return pumpReview776QueueOverlapHold, nil
	}
	if len(complete) == 0 && len(waiting) == 0 {
		if !stored {
			// A member the producer wrote again while the round read the receipts counts for nothing above,
			// but the round still holds the text it read, and a record may have delivered that text: sending
			// it under a new id would deliver it twice. The round ends with nothing written instead, and the
			// next one reads the new notice, which no record carried.
			for _, name := range pin.Names {
				if !intact[name] {
					pumpLog(cfg, fmt.Sprintf("queue %s: the notice %s changed while the pre-change attempts were read; nothing is sent this round", thread, name))
					return pumpReview776QueueOverlapChanged, nil
				}
			}
			return pumpReview776QueueOverlapFree, nil
		}
		if err := ctx.Err(); err != nil {
			return pumpReview776QueueOverlapHold, err
		}
		pumpLog(cfg, fmt.Sprintf("queue %s: the overlapping pre-change attempts settled without a delivery", thread))
		return pumpReview776QueueOverlapFree, pumpReview776QueuePinLift(ctx, cfg, st, thread, nil)
	}
	// The pin is written before the first move, so a crash part way leaves an overlap pin that the next
	// round reconciles again from the records' answers: a notice already moved is gone and counts for
	// nothing, and the rest is classed as before.
	if !stored {
		if err := ctx.Err(); err != nil {
			return pumpReview776QueueOverlapHold, err
		}
		st.QueueAttempt[thread] = pin
		if err := st.pumpSave(cfg); err != nil {
			return pumpReview776QueueOverlapHold, err
		}
	}
	if err := pumpReview776QueueCompleteProven(ctx, cfg, dir, thread, pin, complete); err != nil {
		return pumpReview776QueueOverlapHold, err
	}
	if len(waiting) > 0 {
		pumpLog(cfg, fmt.Sprintf("queue %s %s: notices %s wait for the pre-change attempts %s, which carried them and have no settled receipt; nothing is sent until they settle",
			thread, pumpQueueLegacyOverlapHold, strings.Join(waiting, ","), strings.Join(waitingOn, ",")))
		return pumpReview776QueueOverlapHold, nil
	}
	return pumpReview776QueueOverlapDone, pumpReview776QueuePinLift(ctx, cfg, st, thread, nil)
}

// pumpReview776QueueListed reports whether a list already holds an entry.
func pumpReview776QueueListed(list []string, entry string) bool {
	for _, listed := range list {
		if listed == entry {
			return true
		}
	}
	return false
}

// pumpReview776QueueCompleteProven moves the notices a provable pre-change record shows delivered to
// sent/, each only while its file still carries the text the record was matched against. Each move is
// durable, so the cancellation is checked before each of them.
func pumpReview776QueueCompleteProven(ctx context.Context, cfg *Config, dir, thread string, pin pumpReview776QueuePin, complete []string) error {
	if len(complete) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sent := filepath.Join(dir, pumpSentDir)
	if err := os.MkdirAll(sent, 0o700); err != nil {
		return err
	}
	for _, name := range complete {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, moved, err := pumpReview776QueueMoveVerified(dir, name, sent, pin.SHA256[name])
		if err != nil {
			return err
		}
		if !moved {
			pumpLog(cfg, fmt.Sprintf("queue %s: the delivered notice %s was replaced or is gone; left queued", thread, name))
		}
	}
	return nil
}

// pumpReview776QueueOverlapVerdicts reads each provable pre-change record's answer: the ledger's
// settled state when it has one, else the bridge's own receipt for the record's request id, through one
// bridge for the round; a record that cannot prove its text is left undetermined, unread. A record the ledger no longer holds has nothing left to reconcile, the answer a single
// legacy pin takes for it. A bridge that cannot be started, and a receipt that settles nothing, leave
// the record undetermined.
func pumpReview776QueueOverlapVerdicts(ctx context.Context, e *Env, cfg *Config, thread string, refs []pumpReview776QueueLegacyRef) ([]pumpReview776QueueOverlapVerdict, error) {
	verdicts := make([]pumpReview776QueueOverlapVerdict, len(refs))
	var bridge *deliverBridge
	dialed := false
	defer func() {
		if bridge != nil {
			bridge.close()
		}
	}()
	for i, ref := range refs {
		if ref.Unprovable {
			// A record that cannot prove its text is evidence for no notice, so its answer is not read.
			continue
		}
		if ref.Accepted {
			// A pin the queue held as accepted, folded into the set: its own mark is its answer.
			verdicts[i] = pumpReview776QueueOverlapDelivered
			continue
		}
		record, known, err := deliverLoad(cfg, ref.LogicalID)
		if err != nil {
			return nil, err
		}
		switch {
		case !known || record.State == deliverStateRefused:
			verdicts[i] = pumpReview776QueueOverlapNotDelivered
			continue
		case record.State == deliverStateAccepted:
			verdicts[i] = pumpReview776QueueOverlapDelivered
			continue
		}
		if !dialed {
			dialed = true
			b, err := deliverDial(ctx, e, cfg)
			if err != nil {
				pumpLog(cfg, "queue "+thread+" "+pumpSourceUnmeasured+": "+err.Error())
			} else {
				bridge = b
			}
		}
		if bridge == nil {
			verdicts[i] = pumpReview776QueueOverlapUndetermined
			continue
		}
		switch deliverReconcile(ctx, bridge, record) {
		case deliverReconcileAccepted:
			verdicts[i] = pumpReview776QueueOverlapDelivered
		case deliverReconcileRefused, deliverReconcileResendSame, deliverReconcileResendNew:
			verdicts[i] = pumpReview776QueueOverlapNotDelivered
		default:
			verdicts[i] = pumpReview776QueueOverlapUndetermined
		}
	}
	return verdicts, nil
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
//
// The lift is a durable change reached after a send attempt, so a cancelled round makes none: the pin
// stays, in memory and on disk, and the next round reconciles it again. The cancellation is returned
// with the cause, so the caller sees both.
func pumpReview776QueuePinLift(ctx context.Context, cfg *Config, st *pumpState, thread string, cause error) error {
	if err := ctx.Err(); err != nil {
		if cause == nil {
			return err
		}
		return errors.Join(cause, err)
	}
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
