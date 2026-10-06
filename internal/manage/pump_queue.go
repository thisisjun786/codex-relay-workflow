package manage

import (
	"context"
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

// pumpQueueFlush delivers the queued notices of every thread. For each thread it probes the
// parent's active turn through the delivery core's own dialer: an active parent has the whole
// batch steered in at once; an idle parent is sent the batch with the parent role once the oldest
// notice has waited max_queue_seconds. Files move to sent/ only on accepted. A probe that fails
// leaves the notices queued and is recorded as unmeasured for that thread.
func pumpQueueFlush(ctx context.Context, e *Env, cfg *Config, s pumpSettings, dry bool) error {
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
		if err := pumpQueueFlushThread(ctx, e, cfg, s, root, thread.Name(), dry); err != nil {
			pumpLog(cfg, "queue "+thread.Name()+": "+err.Error())
		}
	}
	return nil
}

// pumpQueueFlushThread delivers one thread's queued notices.
func pumpQueueFlushThread(ctx context.Context, e *Env, cfg *Config, s pumpSettings, root, thread string, dry bool) error {
	dir := filepath.Join(root, thread)
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
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
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
	active, err := pumpQueueActive(ctx, e, cfg, thread)
	if err != nil {
		// The probe failed: the notices stay queued and the thread is recorded as unmeasured, the
		// same state a source whose read failed reports.
		pumpLog(cfg, "queue "+thread+" "+pumpSourceUnmeasured+": "+err.Error())
		return nil
	}
	if !active {
		waited := e.Now().Sub(oldest).Seconds()
		if waited < float64(pumpSettingInt(s.MaxQueueSeconds, pumpDefaultMaxQueueSeconds)) {
			return nil
		}
	}
	logicalID := pumpBatchIDStrings(names)
	out, err := Deliver(ctx, e, cfg, Message{LogicalID: logicalID, Thread: thread, Text: body, Role: "parent", Settings: cfg.Settings.Parent})
	if err != nil && out.Class == "" {
		return err
	}
	pumpLog(cfg, fmt.Sprintf("queue thread=%s notices=%d request=%s class=%s received=%v applied=%v",
		thread, len(parts), out.RequestID, out.Class, out.Class == deliverClassAccepted, false))
	if out.Class != deliverClassAccepted {
		return err
	}
	// Only an accepted delivery moves the notices: an unknown or refused one stays for the next
	// round, which reconciles it under the same logical id.
	sent := filepath.Join(dir, pumpSentDir)
	if err := os.MkdirAll(sent, 0o700); err != nil {
		return err
	}
	for _, name := range names {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(sent, name)); err != nil {
			return err
		}
	}
	return nil
}

// pumpQueueActive reports whether a parent thread has an active turn. It dials the same bridge the
// delivery core dials and asks get_active_turn; an observation that is not a confirmed active turn
// is idle, and a call that fails is reported so the notices stay queued.
func pumpQueueActive(ctx context.Context, e *Env, cfg *Config, thread string) (bool, error) {
	bridge, err := deliverDial(ctx, e, cfg)
	if err != nil {
		return false, err
	}
	defer bridge.close()
	reply := bridge.call(ctx, deliverToolActive, map[string]any{"thread_id": thread})
	if reply.Err != nil {
		return false, reply.Err
	}
	if reply.Payload == nil {
		return false, fmt.Errorf("crw manage pump: the active-turn probe answered nothing readable")
	}
	observation, _ := reply.Payload["observation"].(string)
	turnID, _ := reply.Payload["activeTurnId"].(string)
	return observation == "active" && turnID != "", nil
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
