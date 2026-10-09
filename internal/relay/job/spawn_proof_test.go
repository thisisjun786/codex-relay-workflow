package job

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-1155, post-evaluation round (f24d11cd): a cancel signals a group only when the record shows it is the job's, and a reservation is
// not settled from the exit file of the id's previous job.

// orphanGroup starts a process group of its own whose leader has ended and been reaped while a member lives on, the shape of a job
// whose shell ended on SIGTERM; it returns the group id.
func orphanGroup(t *testing.T) int {
	t.Helper()
	c := exec.Command("sh", "-c", "sleep 60 & exit 0")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if !PidGone(pid) || groupEnded(pid) {
		t.Fatalf("not an orphaned group: leader gone %v, group ended %v", PidGone(pid), groupEnded(pid))
	}
	return pid
}

func asRequested(r BgRecord, at time.Time) BgRecord {
	r.Status = requestedStatus
	r.Extra = append(r.Extra, Member{"cancelRequestedAt", at.UTC().Format(isoLayout)})
	return r
}

// A cancel that was requested may signal the group of a shell that has ended, but only when the group is shown to be the job's: a
// numeric group id is handed out again once the group is empty, so a leader that is gone and a group that answers signal 0 prove
// nothing by themselves. Every member of the job's group started before the request or descends from one that did; the members of a
// group that took the number afterwards started after the job's group had ended.
func TestCancelOfARequestedJobSignalsOnlyAGroupItCanShowIsTheJobs(t *testing.T) {
	needPS(t)
	pid := orphanGroup(t)
	saved := cancelWait
	cancelWait = 50 * time.Millisecond
	t.Cleanup(func() { cancelWait = saved })
	for name, tc := range map[string]struct {
		extra  bool
		at     time.Time
		signal bool
	}{
		"a request made before the group's members started": {true, time.Now().Add(-time.Hour), false},
		"a request that no record can place":                {false, time.Time{}, false},
		"a request made after its members started":          {true, time.Now().Add(time.Hour), true},
	} {
		ws := workspace(t)
		r := mk(ws, "q")
		r.PID, r.StartToken = &pid, sp("Thu Jan  1 00:00:00 1970") // the leader is gone: the token cannot match
		if tc.extra {
			r = asRequested(r, tc.at)
		} else {
			r.Status = requestedStatus
		}
		save(t, ws, r)
		s := &signals{} // signal 0 answers: the group is there
		_, err := cancel(ws, r, noonClock, s.kill)
		var unproven ErrOwnerUnproven
		if tc.signal != (len(s.calls) == 1 && s.calls[0] == fmt.Sprintf("%d:%d", -pid, syscall.SIGKILL)) || tc.signal != (err == nil) || !tc.signal && !errors.As(err, &unproven) {
			t.Errorf("%s: signalled %v, error %v", name, s.calls, err)
		}
	}
}

// The proof is checked again under the store lock, where the signal follows it at once: an owner shown before a wait for the lock is
// not the owner after it.
func TestCancelChecksTheOwnerAgainAfterWaitingForTheStoreLock(t *testing.T) {
	needPS(t)
	c := child(t)
	live := c.Process.Pid
	token, _ := ProcessStartToken(live)
	ws := workspace(t)
	r := mk(ws, "w")
	r.PID, r.StartToken = &live, &token
	save(t, ws, r)
	unlock, err := lockStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	s := &signals{probe: syscall.ESRCH}
	done := make(chan error, 1)
	go func() { _, err := cancel(ws, r, noonClock, s.kill); done <- err }()
	time.Sleep(300 * time.Millisecond)
	stop(c) // the shell ends while cancel waits; its pid could be anyone's by the time cancel gets the lock
	unlock()
	var unproven ErrOwnerUnproven
	if err := <-done; !errors.As(err, &unproven) || len(s.calls) != 0 {
		t.Errorf("signalled %v after the owner ended while cancel waited: %v", s.calls, err)
	}
}

// A reservation is published before the stale files of a reused id are cleared: no exit file can be this job's then, since its shell
// cannot run before its pid is published.
func TestAReservationIsNotSettledFromAnExitFile(t *testing.T) {
	ws := workspace(t)
	r := mk(ws, "fixed")
	r.StartedAt = noon().UTC().Format(isoLayout) // young: only an exit file could settle it
	r.Extra = []Member{{launchingKey, true}}
	save(t, ws, r)
	put(t, ExitPath(ws, "fixed"), "0")
	got, err := Reconcile(ws, r, noonClock)
	if err != nil || got.Status != StatusRunning {
		t.Fatalf("a reservation was settled from the previous job's exit file: %+v %v", got, err)
	}
	recs, err := ListRecords(ws, noonClock)
	if err != nil || len(recs) != 1 || recs[0].Status != StatusRunning {
		t.Errorf("list: %+v %v", recs, err)
	}
	if onDisk, _ := ReadRecord(ws, "fixed"); onDisk.Status != StatusRunning || onDisk.ExitCode != nil {
		t.Errorf("on disk: %+v", onDisk)
	}
	// It is older than the grace window: nothing launched it, and the exit file is still not its own.
	later := func() time.Time { return noon().Add(time.Minute) }
	if got, err := Reconcile(ws, r, later); err != nil || got.Status != StatusFailed || got.ExitCode != nil {
		t.Errorf("a reservation that nothing launched: %+v %v", got, err)
	}
}

// The mark of a reservation is only on the record while it is one.
func TestTheReservationMarkLeavesWithTheReservation(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", "exit 0"}})
	if len(rec.Extra) != 0 || strings.Contains(get(t, RecordPath(ws, rec.ID)), launchingKey) {
		t.Errorf("a started job keeps the reservation mark: %+v", rec.Extra)
	}
	failed, err := runBackground(ws, RunOptions{Command: []string{"true"}}, noonClock, func(*exec.Cmd) error { return syscall.ENOENT })
	if err != nil || failed.Status != StatusFailed || len(failed.Extra) != 0 || strings.Contains(get(t, RecordPath(ws, failed.ID)), launchingKey) {
		t.Errorf("a job that never started keeps the reservation mark: %+v %v", failed, err)
	}
}

// groupProven judges a group by when its members started: before the request, or under a member that did.
func TestGroupProven(t *testing.T) {
	seen := time.Date(2026, 10, 10, 12, 0, 30, 400_000_000, time.UTC)
	at := func(sec int) time.Time { return time.Date(2026, 10, 10, 12, 0, sec, 0, time.UTC) }
	const g = 500
	for name, tc := range map[string]struct {
		procs []procEntry
		want  bool
	}{
		"members from before the request":            {[]procEntry{{501, 1, g, at(10)}, {502, 501, g, at(30)}}, true},
		"a child that started after, under a member": {[]procEntry{{501, 1, g, at(10)}, {502, 501, g, at(45)}, {503, 502, g, at(50)}}, true},
		"a member that started after, under no one":  {[]procEntry{{501, 1, g, at(10)}, {502, 1, g, at(45)}}, false},
		"a recycled group, its leader reaped":        {[]procEntry{{900, 1, g, at(55)}, {901, 900, g, at(56)}}, false},
		"a parent that is not in the group":          {[]procEntry{{501, 1, g, at(10)}, {777, 1, 777, at(10)}, {502, 777, g, at(45)}}, false},
		"a start that did not read":                  {[]procEntry{{501, 1, g, time.Time{}}}, false},
		"no member":                                  {[]procEntry{{777, 1, 777, at(10)}}, false},
	} {
		if got := groupProven(tc.procs, g, seen); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}
