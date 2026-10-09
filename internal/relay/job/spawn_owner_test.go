package job

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-1155: a job runs only once its record names it, and cancel says cancelled only for a job it saw stop. Every case starts its own
// jobs in its own workspace and signals them by the pid it recorded.

const requestedStatus = BgStatus("cancellation-requested")

// gone is whether pid has ended: no such process, or a zombie nobody has reaped yet.
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

// groupGone is whether no process is left in the process group pid leads.
func groupGone(pid int) bool { return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) }

// marked is a job command that leaves a file behind as soon as it runs.
func marked(t *testing.T) (string, []string) {
	marker := filepath.Join(t.TempDir(), "ran")
	return marker, []string{"/bin/sh", "-c", "echo ran > " + shellQuotePosix(marker) + "; sleep 30"}
}

func TestRunBackgroundLeavesNoUntrackedJobWhenThePublishFails(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	store(t, ws)
	marker, command := marked(t)
	pid := 0
	_, err := runBackground(ws, RunOptions{ID: "fixed", Command: command}, time.Now, func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			return err
		}
		pid = cmd.Process.Pid
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		// A directory where the record goes: the record of the started shell cannot be published.
		_ = os.Remove(RecordPath(ws, "fixed"))
		return os.Mkdir(RecordPath(ws, "fixed"), 0o755)
	})
	if err == nil {
		t.Fatal("a record that cannot be published is an error")
	}
	until(t, "the shell to end", func() bool { return gone(pid) })
	if exists(marker) {
		t.Error("the command ran although no record names its process")
	}
}

// TestRunBackgroundHelperDiesBeforeThePublish is the caller of the next case: it starts a job and dies before it records it.
func TestRunBackgroundHelperDiesBeforeThePublish(t *testing.T) {
	ws, marker := os.Getenv("CRW_BG_DIE_WS"), os.Getenv("CRW_BG_DIE_MARKER")
	if ws == "" {
		t.Skip("the caller half of TestRunBackgroundLeavesNoUntrackedJobWhenTheCallerDies")
	}
	_, _ = runBackground(ws, RunOptions{Command: []string{"/bin/sh", "-c", "echo ran > " + shellQuotePosix(marker) + "; sleep 30"}}, time.Now, func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			return err
		}
		_ = os.WriteFile(filepath.Join(ws, "pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	})
}

func TestRunBackgroundLeavesNoUntrackedJobWhenTheCallerDies(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	marker := filepath.Join(t.TempDir(), "ran")
	c := exec.Command(os.Args[0], "-test.run=^TestRunBackgroundHelperDiesBeforeThePublish$")
	c.Env = append(os.Environ(), "CRW_BG_DIE_WS="+ws, "CRW_BG_DIE_MARKER="+marker)
	_ = c.Run()
	pid, err := strconv.Atoi(get(t, filepath.Join(ws, "pid")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	until(t, "the orphaned shell to end", func() bool { return gone(pid) })
	if exists(marker) {
		t.Error("the command ran although its caller died before any record named it")
	}
}

func TestRunBackgroundRefusesAJobWithoutAStartIdentity(t *testing.T) {
	ws := workspace(t)
	t.Setenv("PATH", t.TempDir()) // no ps: the start identity cannot be read
	marker, command := marked(t)
	rec, err := RunBackground(ws, RunOptions{Command: command}, time.Now)
	if rec.PID != nil {
		pid := *rec.PID
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	}
	time.Sleep(200 * time.Millisecond)
	if err == nil || exists(marker) {
		t.Errorf("a job whose start identity cannot be recorded ran: %+v %v", rec, err)
	}
	if onDisk, ok := ReadRecord(ws, rec.ID); ok && onDisk.Status == StatusRunning {
		t.Errorf("the refused job is still recorded running: %+v", onDisk)
	}
}

func TestRunBackgroundReservesTheIDBeforeTheStart(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	var second BgRecord
	first, err := runBackground(ws, RunOptions{ID: "same", Command: []string{"true"}}, time.Now, func(cmd *exec.Cmd) error {
		var err2 error
		second, err2 = RunBackground(ws, RunOptions{ID: "same", Command: []string{"true"}}, time.Now)
		if err2 != nil {
			t.Error(err2)
		}
		return cmd.Start()
	})
	if err != nil || first.ID != "same" || second.ID == "same" || second.ID == "" {
		t.Errorf("two jobs drew one id: %q and %q (%v)", first.ID, second.ID, err)
	}
}

// signals is a kill that records every signal and answers probe for the signal 0 that asks after the group.
type signals struct {
	calls []string
	term  error
	probe error
}

func (s *signals) kill(pid int, sig syscall.Signal) error {
	if sig == 0 {
		return s.probe
	}
	s.calls = append(s.calls, fmt.Sprintf("%d:%d", pid, sig))
	return s.term
}

func TestCancelNeverSignalsAProcessItCannotProveIsTheJob(t *testing.T) {
	needPS(t)
	live := child(t).Process.Pid
	for name, token := range map[string]*string{"no start token": nil, "a stale start token": sp("Thu Jan  1 00:00:00 1970")} {
		ws := workspace(t)
		r := mk(ws, "o")
		r.PID, r.StartToken = &live, token
		save(t, ws, r)
		pending(t, ws, "o") // the exit file is on its way, so the record stays running and cancel's own check decides
		s := &signals{probe: syscall.ESRCH}
		got, err := cancel(ws, r, noonClock, s.kill)
		onDisk, _ := ReadRecord(ws, "o")
		if err == nil || len(s.calls) != 0 || got.Status == StatusCancelled || onDisk.Status == StatusCancelled {
			t.Errorf("%s: %+v %v, signalled %v, on disk %s", name, got, err, s.calls, onDisk.Status)
		}
	}
}

func TestCancelReportsASignalThatFailed(t *testing.T) {
	needPS(t)
	live := child(t).Process.Pid
	token, _ := ProcessStartToken(live)
	ws := workspace(t)
	r := mk(ws, "e")
	r.PID, r.StartToken = &live, &token
	save(t, ws, r)
	s := &signals{term: syscall.EPERM, probe: nil}
	got, err := cancel(ws, r, noonClock, s.kill)
	onDisk, _ := ReadRecord(ws, "e")
	if err == nil || got.Status != requestedStatus || onDisk.Status != requestedStatus || len(s.calls) != 1 || s.calls[0] != fmt.Sprintf("%d:%d", -live, syscall.SIGTERM) {
		t.Errorf("EPERM: %+v %v, signalled %v, on disk %s", got, err, s.calls, onDisk.Status)
	}
}

func TestCancelOfAJobThatIgnoresSIGTERMIsNotCancelledUntilItStops(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	job := `trap '' TERM; echo ready > ready.tmp && mv ready.tmp ready; while :; do sleep 0.05; done`
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", job}})
	until(t, "the job to ignore SIGTERM", func() bool { return exists(filepath.Join(ws, "ready")) })
	pid := *rec.PID
	got, err := Cancel(ws, rec, time.Now)
	onDisk, _ := ReadRecord(ws, rec.ID)
	if err != nil || got.Status != requestedStatus || onDisk.Status != requestedStatus || groupGone(pid) {
		t.Fatalf("a job that ignores SIGTERM: %+v %v, on disk %s, group gone %v", got, err, onDisk.Status, groupGone(pid))
	}
	if again, err := Reconcile(ws, onDisk, time.Now); err != nil || again.Status != requestedStatus {
		t.Errorf("a requested cancel of a live job is not settled: %+v %v", again, err)
	}
	got, err = Cancel(ws, onDisk, time.Now) // a second cancel does not ask again
	if err != nil || got.Status != StatusCancelled || !groupGone(pid) {
		t.Errorf("second cancel: %+v %v, group gone %v", got, err, groupGone(pid))
	}
}

func TestCancelSaysCancelledOnlyOnceTheWholeGroupIsGone(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	// The job's shell takes a moment over SIGTERM and has a child of its own, so the leader and its child do not stop at once.
	job := `trap 'sleep 0.3; exit 143' TERM; sleep 30 & echo ready > ready.tmp && mv ready.tmp ready; wait`
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", job}})
	until(t, "the job to install its trap", func() bool { return exists(filepath.Join(ws, "ready")) })
	pid := *rec.PID
	got, err := Cancel(ws, rec, time.Now)
	if err != nil || got.Status != StatusCancelled || !groupGone(pid) {
		t.Errorf("cancelled while the group lives: %+v %v, group gone %v", got, err, groupGone(pid))
	}
	if rows := ledger(t, ws); len(rows) != 2 || !strings.Contains(rows[1], `"event":"cancelled"`) {
		t.Errorf("ledger %q", rows)
	}
}

// The verb says what the cancel did: a request that has not seen the job stop is not "cancelled", and a refusal exits 1 with the way
// to settle the record by hand.
func TestCLICancelReportsTheRequestAndTheRefusal(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	live := child(t).Process.Pid
	r := mk(ws, "legacy")
	r.PID = &live // no start token: an old record
	save(t, ws, r)
	if got := cliResult(t, ws, "cancel", "legacy"); got.Code != 1 || !strings.HasPrefix(got.Out.(string), "legacy running\n") || !strings.Contains(got.Out.(string), "crw relay job get legacy") || PidGone(live) {
		t.Errorf("a cancel without a provable owner: %+v", got)
	}
	job := `trap '' TERM; echo ready > ready.tmp && mv ready.tmp ready; while :; do sleep 0.05; done`
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", job}})
	until(t, "the job to ignore SIGTERM", func() bool { return exists(filepath.Join(ws, "ready")) })
	if got := cliResult(t, ws, "cancel", rec.ID); got.Code != 0 || !strings.HasPrefix(got.Out.(string), rec.ID+" cancellation-requested\n") {
		t.Errorf("a requested cancel: %+v", got)
	}
	if got := cliResult(t, ws, "cancel", rec.ID); got.Code != 0 || got.Out != rec.ID+" cancelled" {
		t.Errorf("the second cancel: %+v", got)
	}
}
