package manage

import (
	"context"
	"errors"
	"fmt"
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
	// A pinned batch is reconciled first, under its own frozen logical id and body, before any new
	// batch is formed: a notice that may already have gone is never re-sent under a different id.
	if _, pinned := st.QueueAttempt[thread]; pinned {
		settled, err := pumpReview776QueueRetry(ctx, e, cfg, st, s, dir, thread, dry)
		if settled || err != nil {
			return err
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
	// A notice that alone exceeds the batch limit is moved aside first, so the longest fitting
	// prefix always has something to carry and one oversized notice cannot hold the queue.
	names, texts, err = pumpReview776QueueOversize(e, cfg, dir, thread, names, texts, dry)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	batch := pumpReview776QueueFit(names, texts)
	if dry {
		fmt.Fprintf(e.Stdout, "queue %s %d notices\n%s\n", thread, len(batch.names), batch.body)
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
	if !utf8.ValidString(batch.body) {
		// JSON replaces invalid bytes with the replacement character, so a pinned body would stop
		// being the text on disk; the delivery core refuses such a message anyway.
		return fmt.Errorf("crw manage pump: the queued notices of %s are not valid UTF-8", thread)
	}
	logicalID := pumpQueueBatchID(thread, batch.names, batch.body)
	st.QueueAttempt[thread] = pumpReview776QueuePin{
		LogicalID: logicalID, Names: append([]string(nil), batch.names...), Body: batch.body}
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
		// The membership is durable before any move, so a crash between the moves is recoverable.
		st.QueueAccepted[thread] = append([]string(nil), batch.names...)
		delete(st.QueueAttempt, thread)
		if err := st.pumpSave(cfg); err != nil {
			return err
		}
		return pumpQueueCompleteAccepted(cfg, st, dir, thread)
	case deliverClassRefused:
		// A refusal is terminal and nothing was sent, so the pin lifts: the notices stay queued and
		// a later round forms a new batch under a new id.
		return pumpReview776QueuePinLift(cfg, st, thread, err)
	default:
		// Unknown: the pin stays, so the next round reconciles the same logical id and body.
		return err
	}
}

// pumpQueueCompleteAccepted moves an accepted batch's notices into sent/. A thread with no
// recorded membership is done already.
func pumpQueueCompleteAccepted(cfg *Config, st *pumpState, dir, thread string) error {
	names := st.QueueAccepted[thread]
	if len(names) == 0 {
		return nil
	}
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
	delete(st.QueueAccepted, thread)
	delete(st.QueueAttempt, thread)
	return st.pumpSave(cfg)
}

// pumpReview776QueueBatch is one queue batch: the notices it carries and the body it sends.
type pumpReview776QueueBatch struct {
	names []string
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

// pumpReview776QueueOversize moves a notice whose body alone exceeds the batch limit out of the
// queue into the thread's oversize/ directory, logging its name and size. A single notice that can
// never fit is never sent: a batch the delivery core would refuse every round would hold that
// thread's queue forever. A dry run reports the move instead of making it.
func pumpReview776QueueOversize(e *Env, cfg *Config, dir, thread string, names, texts []string, dry bool) ([]string, []string, error) {
	keptNames := make([]string, 0, len(names))
	keptTexts := make([]string, 0, len(texts))
	for i, name := range names {
		if len(texts[i]) <= pumpBatchLimit {
			keptNames = append(keptNames, name)
			keptTexts = append(keptTexts, texts[i])
			continue
		}
		if dry {
			fmt.Fprintf(e.Stdout, "queue %s: would move oversize notice %s (%d bytes) to oversize/\n", thread, name, len(texts[i]))
			continue
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
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(oversizeDir, name)); err != nil {
			// The move is retried next round; the notice is left out of this round's batch so one
			// immovable file cannot hold the thread's other notices.
			pumpLog(cfg, fmt.Sprintf("queue %s: oversize notice %s could not be moved: %v", thread, name, err))
			continue
		}
		pumpLog(cfg, fmt.Sprintf("queue %s: oversize notice %s %d bytes moved to oversize/", thread, name, len(texts[i])))
	}
	return keptNames, keptTexts, nil
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
		// An oversize singleton was moved aside already, so this is only a guard against a batch the
		// delivery core could never accept.
		return pumpReview776QueueBatch{names: names[:1], body: pumpReview776QueueBody(texts[:1])}
	}
	return pumpReview776QueueBatch{names: names[:fit], body: pumpReview776QueueBody(texts[:fit])}
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

// pumpReview776QueueRetry reconciles a pinned queue batch. It reports whether it settled the
// thread for this round, so the caller does not also form a fresh batch in the same round. The
// retry repeats the queue's own gate: an idle parent is only sent to once the oldest pinned notice
// has waited max_queue_seconds, so reconciling never opens a parent turn early.
func pumpReview776QueueRetry(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings, dir, thread string, dry bool) (bool, error) {
	pin := st.QueueAttempt[thread]
	texts, err := pumpReview776QueueReadNotices(dir, pin.Names)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pumpReview776QueuePinDrop(cfg, e, st, thread, dry, "the pinned notices changed")
		}
		return true, err
	}
	if pumpReview776QueueBody(texts) != pin.Body {
		// A notice was replaced under the same name, so the frozen batch no longer describes what is
		// queued: the pin lifts and the current notices form their own batch under their own id.
		return pumpReview776QueuePinDrop(cfg, e, st, thread, dry, "the pinned notices changed")
	}
	if dry {
		fmt.Fprintf(e.Stdout, "queue %s %d notices\n%s\n", thread, len(pin.Names), pin.Body)
		return true, nil
	}
	oldest, err := pumpReview776QueueOldest(dir, pin.Names)
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
		// The membership is the pin's own names, never a fresh read of the directory: a notice queued
		// after the pin was taken was not in the delivered body and must not move to sent/.
		st.QueueAccepted[thread] = append([]string(nil), pin.Names...)
		delete(st.QueueAttempt, thread)
		if err := st.pumpSave(cfg); err != nil {
			return true, err
		}
		return true, pumpQueueCompleteAccepted(cfg, st, dir, thread)
	case deliverClassRefused:
		return true, pumpReview776QueuePinLift(cfg, st, thread, err)
	default:
		return true, err
	}
}

// pumpReview776QueuePinDrop drops a pin whose frozen batch no longer describes the queue, because
// the pinned notices changed or are gone. The thread then forms a batch from what is queued, which
// is why the caller is told the thread was not settled.
func pumpReview776QueuePinDrop(cfg *Config, e *Env, st *pumpState, thread string, dry bool, reason string) (bool, error) {
	delete(st.QueueAttempt, thread)
	if dry {
		fmt.Fprintf(e.Stdout, "queue %s: would drop the pin, %s\n", thread, reason)
		return false, nil
	}
	pumpLog(cfg, fmt.Sprintf("queue %s: pin dropped, %s", thread, reason))
	if err := st.pumpSave(cfg); err != nil {
		return true, err
	}
	return false, nil
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
