package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
)

// The spawned process's readiness: an anonymous pipe it inherits (its write end is ExtraFiles of the exec.Cmd), acknowledged by
// the _run command once this process's started row is durable (engine.Config.Started). A lock any process holds, a result file's
// mtime or another writer cannot satisfy it: the descriptor belongs to this one exec. EOF without the byte is a refused start.
type spawnReadyKey struct {
	state, run string
	pid        int
}

type spawnReady struct {
	reader *os.File
	timer  *time.Timer
}

var spawnReadiness sync.Map // spawnReadyKey -> *spawnReady

func rememberSpawnReady(state, run string, pid int, reader *os.File) {
	key := spawnReadyKey{state, run, pid}
	ready := &spawnReady{reader: reader}
	// publish before the timer is armed: the caller cannot await the pid until this returns, so a fired cleanup always
	// finds the entry it owns and a suspended registration cannot keep an unconsumed pipe forever
	spawnReadiness.Store(key, ready)
	// A caller that deliberately supplies no Ready callback must not retain the
	// descriptor indefinitely. Default callers consume it immediately.
	ready.timer = time.AfterFunc(15*time.Second, func() {
		if spawnReadiness.CompareAndDelete(key, ready) {
			_ = reader.Close()
		}
	})
}

func awaitSpawnReady(ctx context.Context, state, run string, pid int) (bool, error) {
	value, ok := spawnReadiness.LoadAndDelete(spawnReadyKey{state, run, pid})
	if !ok {
		return false, nil
	}
	ready := value.(*spawnReady)
	defer ready.reader.Close()
	ready.timer.Stop()
	done := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := io.ReadFull(ready.reader, b[:])
		if err == nil && b[0] != 1 {
			err = fmt.Errorf("invalid start acknowledgement")
		}
		done <- err
	}()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return true, fmt.Errorf("process %d ended before it started the run: %s", pid, logTail(state, run))
		}
		return true, nil
	case <-ctx.Done():
		return true, ctx.Err()
	case <-timer.C:
		return true, fmt.Errorf("process %d did not start the run in time: %s", pid, logTail(state, run))
	}
}

// sessionStartHeld says whether the spawned process of pid recorded a durable start: the run's newest started row names a nonempty
// `process` token whose process lock is still held (processHeld), no later KindRun paused/finished end is recorded, and the run
// lock still names the same pid as a run process. It is the fallback of AwaitLock for a spawn of another implementation, where no
// readiness pipe exists. It answers false on any read error and uses no timestamp heuristic.
func sessionStartHeld(ctx context.Context, state, run string, pid int) bool {
	r, err := OpenReader(ctx, state, "")
	if err != nil {
		return false
	}
	defer r.Close()
	events, found, err := journal.LoadLean(ctx, r.St, run)
	if err != nil || !found {
		return false
	}
	newest, ended := -1, false
	for i, ev := range events {
		if ev.RunID != run || ev.Kind != engine.KindRun {
			continue
		}
		switch ev.Event {
		case "started":
			newest, ended = i, false
		case "paused", "finished":
			if i > newest {
				ended = true
			}
		}
	}
	if newest < 0 || ended {
		return false
	}
	token := str(events[newest].Data, "process")
	if token == "" || !processHeld(state, run, token) {
		return false
	}
	rec, held := lockHolderRecord(state, run)
	return held && rec.PID == pid && runProcessCommand(rec.Command)
}
