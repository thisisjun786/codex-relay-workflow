package manage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A real SIGINT-like cancellation that lands asynchronously, from another goroutine, the moment the
// first notice of an accepted batch is published under sent/. The move that had already passed its
// cancellation check when the cancel landed may finish; nothing after it moves, and the accepted pin
// stays so the next round completes the rest.
func TestPumpQueueAcceptedCompletionStopsAtAnAsynchronousCancellation(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "/nonexistent/crw-bridge-991")
	thread := "parent-1"
	const count = 512
	var names, texts []string
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("%016d.txt", i)
		pumpQueueTestNotice(t, cfg, thread, name, "delivered")
		names, texts = append(names, name), append(texts, "delivered")
	}
	pumpQueueTestPin(t, cfg, thread, "acceptedid", names, texts)
	st := pumpTestReadStatePtr(t, cfg)
	pin := st.QueueAttempt[thread]
	pin.Accepted = true
	st.QueueAttempt[thread] = pin
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	sent := filepath.Join(cfg.StateDir, pumpQueueDir, thread, pumpSentDir)
	if err := os.MkdirAll(sent, 0o700); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.InotifyInit()
	if err != nil {
		t.Skipf("no inotify: %v", err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, sent, syscall.IN_CREATE); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := make(chan int, 1)
	go func() {
		var buf [4096]byte
		if _, err := syscall.Read(fd, buf[:]); err != nil {
			observed <- -1
			return
		}
		cancel()
		entries, _ := os.ReadDir(sent)
		observed <- len(entries)
	}()
	if err := pumpQueueFlush(ctx, e, cfg, st, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	atCancel := <-observed
	if atCancel < 1 {
		t.Fatalf("the watcher saw no move: %d", atCancel)
	}
	if atCancel >= count-1 {
		t.Skipf("the cancellation landed after %d of %d moves; nothing left to observe", atCancel, count)
	}
	final, err := os.ReadDir(sent)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) > atCancel+1 {
		t.Errorf("cancelled after %d moves, the round moved %d more", atCancel, len(final)-atCancel)
	}
	if disk, pinned := pumpReview776QueueAttempt(t, cfg, thread); !pinned || disk["accepted"] != true {
		t.Errorf("the cancelled round did not keep the accepted pin: %v", disk)
	}
}
