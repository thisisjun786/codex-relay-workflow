package manage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The parent notification queue: send-parent --queue writes one notice per file under
// <state_dir>/parent-queue/<thread>/, and the pump delivers them. A notice never opens a turn of
// its own: it is steered into the parent's active turn, or sent with the parent role once the
// oldest has waited max_queue_seconds, so an idle parent still receives it.
const (
	pumpQueueDir  = "parent-queue"
	pumpSentDir   = "sent"
	pumpQueueWait = "queued"
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
	// An accepted batch whose move did not finish is completed first, so an already accepted
	// notice is never sent again.
	if err := pumpQueueCompleteAccepted(cfg, st, dir, thread); err != nil {
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
	parts := make([]string, 0, len(names))
	var oldest time.Time
	for _, name := range names {
		path := filepath.Join(dir, name)
		// A notice is a regular file the producer wrote; a symlink would let the queue carry the
		// contents of a file outside it, so it is refused rather than followed.
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("crw manage pump: the notice %s is a symlink; refusing to send through it", name)
		}
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		parts = append(parts, strings.TrimSpace(string(raw)))
	}
	body := strings.Join(parts, "\n\n")
	if len(parts) > 1 {
		body = fmt.Sprintf("management session notices: %d queued\n\n", len(parts)) + body
	}
	if dry {
		fmt.Fprintf(e.Stdout, "queue %s %d notices\n%s\n", thread, len(parts), body)
		return nil
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
			return pumpQueueSend(ctx, e, cfg, st, dir, thread, names, body)
		}
		return nil
	}
	if e.Now().Sub(oldest).Seconds() < float64(pumpSettingInt(s.MaxQueueSeconds, pumpDefaultMaxQueueSeconds)) {
		return nil
	}
	return pumpQueueSend(ctx, e, cfg, st, dir, thread, names, body)
}

// pumpQueueSend delivers one thread's queued batch and moves the files only on accepted. The
// accepted membership is recorded before the first move, so a move that fails part way is
// completed by the next round instead of being sent again.
func pumpQueueSend(ctx context.Context, e *Env, cfg *Config, st *pumpState, dir, thread string, names []string, body string) error {
	logicalID := pumpQueueBatchID(thread, names)
	out, err := Deliver(ctx, e, cfg, Message{LogicalID: logicalID, Thread: thread, Text: body, Role: "parent", Settings: cfg.Settings.Parent})
	if err != nil && out.Class == "" {
		return err
	}
	pumpLog(cfg, fmt.Sprintf("queue thread=%s notices=%d request=%s class=%s received=%v applied=%v",
		thread, len(names), out.RequestID, out.Class, out.Class == deliverClassAccepted, false))
	if out.Class != deliverClassAccepted {
		return err
	}
	// The membership is durable before any move, so a crash between the moves is recoverable.
	st.QueueAccepted[thread] = append([]string(nil), names...)
	if err := st.pumpSave(cfg); err != nil {
		return err
	}
	return pumpQueueCompleteAccepted(cfg, st, dir, thread)
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
	return st.pumpSave(cfg)
}

// pumpQueueBatchID is a queue batch's logical id: the thread and the sorted notice names. The
// thread is part of the input because the delivery ledger is shared across threads, so two
// parents queueing the same file name must not collide on one logical id.
func pumpQueueBatchID(thread string, names []string) string {
	return pumpBatchIDStrings(append([]string{thread}, names...))
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
