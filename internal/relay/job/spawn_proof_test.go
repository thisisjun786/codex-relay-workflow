package job

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
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

func asRequested(r BgRecord, members []groupMember) BgRecord {
	r.Status = requestedStatus
	if members != nil {
		r = withGroup(r, members)
	}
	return r
}

// lstart is a moment as psProcesses spells a start.
func lstart(at time.Time) string { return strings.Join(strings.Fields(at.Format(lstartLayout)), " ") }

// A cancel that was requested may signal the group of a shell that has ended, but only when the group is shown to be the job's: a
// numeric group id is handed out again once the group is empty, so a leader that is gone and a group that answers signal 0 prove
// nothing by themselves, and neither does the age of a member. Every member must be one the request saw in the group while the shell
// was alive (the same pid with the same start), or a child of one.
func TestCancelOfARequestedJobSignalsOnlyAGroupItCanShowIsTheJobs(t *testing.T) {
	needPS(t)
	pid := orphanGroup(t)
	procs, err := listProcesses()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(procs, func(p procEntry) bool { return p.PGID == pid && p.Start != "" })
	if i < 0 {
		t.Fatal("the orphaned group has no member with a start")
	}
	member := procs[i]
	saved := cancelWait
	cancelWait = 50 * time.Millisecond
	t.Cleanup(func() { cancelWait = saved })
	for name, tc := range map[string]struct {
		seen   []groupMember
		signal bool
	}{
		"the member the request saw":                            {[]groupMember{{pid, member.Start}, {member.PID, member.Start}}, true},
		"an older member the request did not see":               {[]groupMember{{pid, member.Start}}, false},
		"a member whose pid the request saw with another start": {[]groupMember{{member.PID, "Thu Jan 1 00:00:00 1970"}}, false},
		"a request that holds no members":                       {nil, false},
	} {
		ws := workspace(t)
		r := mk(ws, "q")
		r.PID, r.StartToken = &pid, sp("Thu Jan  1 00:00:00 1970") // the leader is gone: the token cannot match
		r = asRequested(r, tc.seen)
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

// groupProven judges a group by the members the request saw: the same pid with the same start, or a child of such a member.
func TestGroupProven(t *testing.T) {
	at := func(sec int) string { return lstart(time.Date(2026, 10, 10, 12, 0, sec, 0, time.UTC)) }
	const g = 500
	seen := []groupMember{{g, at(5)}, {501, at(10)}, {502, at(30)}}
	for name, tc := range map[string]struct {
		procs []procEntry
		want  bool
	}{
		"the members it saw":                    {[]procEntry{{501, 1, g, at(10)}, {502, 501, g, at(30)}}, true},
		"a child started after, under a member": {[]procEntry{{501, 1, g, at(10)}, {503, 501, g, at(45)}, {504, 503, g, at(50)}}, true},
		"an older member it did not see":        {[]procEntry{{501, 1, g, at(10)}, {600, 1, g, at(1)}}, false},
		"a pid it saw, with another start":      {[]procEntry{{501, 1, g, at(11)}}, false},
		"a recycled group, its leader reaped":   {[]procEntry{{900, 1, g, at(55)}, {901, 900, g, at(56)}}, false},
		"a parent that is not in the group":     {[]procEntry{{501, 1, g, at(10)}, {777, 1, 777, at(10)}, {503, 777, g, at(45)}}, false},
		"a start that did not read":             {[]procEntry{{501, 1, g, ""}}, false},
		"no member":                             {[]procEntry{{777, 1, 777, at(10)}}, false},
	} {
		if got := groupProven(tc.procs, g, seen); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	if groupProven([]procEntry{{501, 1, g, at(10)}}, g, nil) {
		t.Error("a request that saw no member proves a group")
	}
}

// The members a cancel request saw are kept while the job runs and leave the record when the job is over: a finished record is the
// oracle's thirteen keys.
func TestTheMembersARequestSawLeaveWithTheJob(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	job := `trap '' TERM; echo ready > ready.tmp && mv ready.tmp ready; while :; do sleep 0.05; done`
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", job}})
	until(t, "the job to ignore SIGTERM", func() bool { return exists(filepath.Join(ws, "ready")) })
	got, err := Cancel(ws, rec, time.Now)
	onDisk, _ := ReadRecord(ws, rec.ID)
	seen, ok := recordedGroup(onDisk)
	if err != nil || got.Status != requestedStatus || !ok || !slices.ContainsFunc(seen, func(m groupMember) bool { return m.PID == *rec.PID }) {
		t.Fatalf("a requested cancel keeps the members it saw: %+v %v, on disk %+v", got, err, onDisk.Extra)
	}
	if got, err = Cancel(ws, got, time.Now); err != nil || got.Status != StatusCancelled || len(got.Extra) != 0 || strings.Contains(get(t, RecordPath(ws, rec.ID)), groupKey) {
		t.Errorf("a cancelled record keeps the members: %+v %v", got, err)
	}
}

// survivingJob is the command of a job whose shell ends on SIGTERM while a child that ignores it lives on in the job's group.
const survivingJob = `(trap '' TERM; while :; do sleep 0.05; done) & echo ready > ready.tmp && mv ready.tmp ready; wait`

// requestOnce starts survivingJob and cancels it once: the shell ends, its child keeps the group, and the record says requested.
func requestOnce(t *testing.T, ws string) BgRecord {
	t.Helper()
	saved := cancelWait
	cancelWait = 300 * time.Millisecond
	t.Cleanup(func() { cancelWait = saved })
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", survivingJob}})
	until(t, "the job's child to ignore SIGTERM", func() bool { return exists(filepath.Join(ws, "ready")) })
	got, err := Cancel(ws, rec, time.Now)
	if err != nil || got.Status != requestedStatus {
		t.Fatalf("the first cancel: %+v %v", got, err)
	}
	until(t, "the job's shell to end", func() bool { return PidGone(*rec.PID) })
	if groupEnded(*rec.PID) {
		t.Fatal("the job's child did not outlive the shell")
	}
	return got
}

// A process joins a group (setpgid) long after it started, so the start time of a member says nothing of when it entered the group: a
// group that took the job's number once the job's group had emptied can hold, after its own leader has gone, only processes older than
// the request. A member is the job's only when cancel saw it in the job's group while the shell was alive, or when its parent is such
// a member.
func TestCancelRefusesAnOldMemberTheRequestNeverSaw(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	got := requestOnce(t, ws)
	pid := *got.PID
	saved := listProcesses
	t.Cleanup(func() { listProcesses = saved })
	listProcesses = func() ([]procEntry, error) {
		// Another session's process, started an hour before the request, moved into a group that took the number; its leader has gone.
		return []procEntry{{PID: pid + 1, PPID: 1, PGID: pid, Start: lstart(time.Now().Add(-time.Hour))}}, nil
	}
	s := &signals{}
	_, err := cancel(ws, got, time.Now, s.kill)
	var unproven ErrOwnerUnproven
	if len(s.calls) != 0 || !errors.As(err, &unproven) {
		t.Errorf("an old process the request never saw was signalled: %v, error %v", s.calls, err)
	}
}

// The child that the first cancel saw in the group, and the processes it starts afterwards, are the job's: a second cancel after the
// shell has ended stops them and says cancelled.
func TestASecondCancelStopsTheMembersTheRequestSaw(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	got := requestOnce(t, ws)
	pid := *got.PID
	again, err := Cancel(ws, got, time.Now)
	if err != nil || again.Status != StatusCancelled || !groupEnded(pid) {
		t.Errorf("the second cancel: %+v %v, group ended %v", again, err, groupEnded(pid))
	}
}
