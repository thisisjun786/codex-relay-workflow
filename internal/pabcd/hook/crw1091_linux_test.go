//go:build linux

package hook

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1091: one Stop reads the metrics ledger once, inside the lock, and an already announced total cap returns without
// reading the ledger or the plan and without writing the state. The reads are counted by the kernel (inotify IN_OPEN on the
// file), so the count holds whichever code path opens it.

// crw1091Watch counts the inotify events of mask on path from now on; the returned function drains and counts what arrived.
// The kernel merges an event into an identical one still unread at the tail of the queue, so the close events are watched too:
// they separate two sequential opens of one file, which then count as two.
func crw1091Watch(t *testing.T, path string, mask uint32) func() int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, path, mask|unix.IN_CLOSE_WRITE|unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatal(err)
	}
	return func() int {
		n, buf := 0, make([]byte, 64*1024)
		for {
			got, err := unix.Read(fd, buf)
			if errors.Is(err, unix.EAGAIN) {
				return n
			}
			if err != nil {
				t.Fatal(err)
			}
			for off := 0; off+unix.SizeofInotifyEvent <= got; {
				var ev unix.InotifyEvent
				if err := binary.Read(bytes.NewReader(buf[off:off+unix.SizeofInotifyEvent]), binary.NativeEndian, &ev); err != nil {
					t.Fatal(err)
				}
				if ev.Mask&mask != 0 {
					n++
				}
				off += unix.SizeofInotifyEvent + int(ev.Len)
			}
		}
	}
}

func crw1091Metrics(cwd string) string { return filepath.Join(cwd, ".crw", metric.MetricsFile) }

func crw1091Plan(cwd, slug string) string {
	return filepath.Join(cwd, ".crw", "goalplans", slug, goalplan.GoalplanFile)
}

// End condition 1: a fresh Stop of an active goal mid-cycle, with no kind file and new metric rows, opens the ledger once
// (the oracle: four times, cursor, progress, kind inference and plateau). The bound plan is read once before the lock (the
// approval wait) and once under it, where the work phase, the plateau scope and the text share it.
func TestCRW1091OneStopReadsTheLedgerOnce(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "plan" })
	stopWritePlan(t, cwd, "plan", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "One", goalplan.WorkPhaseInProgress)}
	})
	for _, v := range []float64{1, 2} {
		crw1088Record(t, cwd, "score", v, nil)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw", metric.ObjectiveKindDir)); !os.IsNotExist(err) {
		t.Fatalf("the case needs no kind file: %v", err)
	}
	ledger := crw1091Watch(t, crw1091Metrics(cwd), unix.IN_OPEN)
	plan := crw1091Watch(t, crw1091Plan(cwd, "plan"), unix.IN_OPEN)
	stopBlockReason(t, stopRun(cwd, env))
	if got := ledger(); got != 1 {
		t.Errorf("one Stop opened the metrics ledger %d times, want 1", got)
	}
	if got := plan(); got != 2 {
		t.Errorf("one Stop opened the bound goalplan %d times, want 2 (before the lock and under it)", got)
	}
	if s := state.ReadState(cwd, stopSID); s.StopMetricCursor != 2 || s.StopBlockCount != 1 {
		t.Errorf("the single snapshot still feeds the cursor and the counter: %+v", s)
	}
}

// End condition 3: the first total-cap event is answered and recorded once; every later Stop of the same turn reads neither
// the ledger nor the plan and writes nothing.
func TestCRW1091AnnouncedTotalCapReadsAndWritesNothing(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) {
		s.Slug, s.StopBlockTotal, s.StopBlockTurnID = "plan", StopMaxBlocksTotal, ptr(crw1086Turn)
	})
	stopWritePlan(t, cwd, "plan", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "One", goalplan.WorkPhaseInProgress)}
	})
	crw1088Record(t, cwd, "score", 1, nil)
	if a := crw1086Stop(cwd, env); a.Stdout != stopSystemMessage(stopTotalCapMessage) {
		t.Fatalf("the first total-cap event: %+v", a)
	}
	s := state.ReadState(cwd, stopSID)
	if !s.StopBlockCapNotified || s.StopBlockTotal != StopMaxBlocksTotal+1 {
		t.Fatalf("the announcement is not recorded: %+v", s)
	}
	before := stopStateBytes(t, cwd)
	ledger := crw1091Watch(t, crw1091Metrics(cwd), unix.IN_OPEN)
	plan := crw1091Watch(t, crw1091Plan(cwd, "plan"), unix.IN_OPEN)
	sessions := crw1091Watch(t, filepath.Dir(state.StatePath(cwd, stopSID)), unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_MODIFY)
	for i := 0; i < 3; i++ {
		crw1088Record(t, cwd, "score", float64(i+2), nil) // new rows do not wake an announced cap either
		ledger()
		if a := crw1086Stop(cwd, env); a != (StopAnswer{}) {
			t.Errorf("capped Stop %d: %+v", i+1, a)
		}
		if got := ledger(); got != 0 {
			t.Errorf("capped Stop %d opened the metrics ledger %d times", i+1, got)
		}
	}
	if got := plan(); got != 0 {
		t.Errorf("capped Stops opened the bound goalplan %d times", got)
	}
	if got := sessions(); got != 0 {
		t.Errorf("capped Stops wrote %d entries in the sessions directory", got)
	}
	if stopStateBytes(t, cwd) != before {
		t.Errorf("capped Stops rewrote the state")
	}
}
