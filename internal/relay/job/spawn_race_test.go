package job

import (
	"fmt"
	"syscall"
	"testing"
	"time"
)

// CRW-1155, verification fix round 1.

// A launch publishes its pid between the moment cancel read a reservation and the moment it takes the store lock: cancel then acts on
// the started job instead of reporting a cancel that changed nothing.
func TestCancelOfAReservationWhoseLaunchPublishesItsPidActsOnTheStartedJob(t *testing.T) {
	needPS(t)
	live := child(t).Process.Pid
	token, _ := ProcessStartToken(live)
	ws := workspace(t)
	r := mk(ws, "race")
	r.StartedAt = noon().UTC().Format(isoLayout) // young enough for reconcile to leave the reservation running
	save(t, ws, r)
	unlock, err := lockStore(ws) // the barrier: cancel has read the reservation and waits for the lock
	if err != nil {
		t.Fatal(err)
	}
	s := &signals{probe: syscall.ESRCH}
	type result struct {
		rec BgRecord
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := cancel(ws, r, noonClock, s.kill)
		done <- result{got, err}
	}()
	time.Sleep(300 * time.Millisecond)
	started := r
	started.PID, started.StartToken = &live, &token
	if err := WriteRecord(ws, started); err != nil {
		t.Fatal(err)
	}
	unlock()
	res := <-done
	onDisk, _ := ReadRecord(ws, "race")
	if res.err != nil || res.rec.Status != StatusCancelled || onDisk.Status != StatusCancelled {
		t.Errorf("cancel across the pid publish: %+v %v, on disk %s", res.rec, res.err, onDisk.Status)
	}
	if len(s.calls) != 1 || s.calls[0] != fmt.Sprintf("%d:%d", -live, syscall.SIGTERM) {
		t.Errorf("the started job was not signalled: %v", s.calls)
	}
}
