package command

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLedgerIgnoresATornTailAndRefusesAMalformedLine(t *testing.T) {
	l := &ledger{dir: t.TempDir(), now: func() time.Time { return time.Date(2026, 10, 4, 23, 59, 59, 0, time.UTC) }}
	for _, event := range []string{"started", "finished"} {
		if err := l.append(record{Event: event, PatchID: "p1", Head: "h1"}); err != nil {
			t.Fatal(err)
		}
	}
	appendRaw := func(text string) {
		f, err := os.OpenFile(l.path(), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}
	appendRaw(`{"time":"2026-10-04T00:00:00Z","event":"star`) // an append that was torn
	if recs, err := l.read(); err != nil || len(recs) != 2 {
		t.Fatalf("a torn tail must be ignored: %v %v", recs, err)
	}
	if err := l.append(record{Event: "started", PatchID: "p2"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(l.path())
	recs, err := l.read()
	if err != nil || len(recs) != 3 || strings.Contains(string(data), "2026-10-04T00:00:00Z") || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("the next append must cut the torn tail off: %v %v\n%s", recs, err, data)
	}
	appendRaw("not a record\n")
	if _, err := l.read(); err == nil || !strings.Contains(err.Error(), l.path()) || !strings.Contains(err.Error(), "line 4") {
		t.Fatalf("a malformed line must fail closed naming the file and the line: %v", err)
	}
}

// A started record followed by a lock_wait record was an attempt where agy never ran: it counts toward neither the cap nor the one more attempt.
func TestLockWaitRecordDoesNotCountOrSpendTheRetry(t *testing.T) {
	recs := []record{
		{Time: "2026-10-04T09:00:00Z", Event: "started", PatchID: "p1"},
		{Time: "2026-10-04T09:01:00Z", Event: "lock_wait", PatchID: "p1", Reason: "lock_wait_expired"},
		{Time: "2026-10-04T10:00:00Z", Event: "unavailable", PatchID: "p1", Reason: "quota"},
		{Time: "2026-10-04T11:00:00Z", Event: "started", PatchID: "p1"},
		{Time: "2026-10-04T11:01:00Z", Event: "lock_wait", PatchID: "p1", Reason: "lock_wait_expired"},
	}
	if n := runsOn(recs, "2026-10-04"); n != 0 {
		t.Fatalf("runsOn = %d, want 0: a lock wait is not an attempt", n)
	}
	st := standingOf(recs, "p1")
	if st.unavailable == nil || st.retried || !st.open() {
		t.Fatalf("a lock wait spent the one more attempt: %+v", st)
	}
}

// A process killed between the started record and its result leaves a bare started: it counts toward the cap and, after an unavailable, spends the one more attempt. This is the accepted,
// pre-existing behavior of an attempt that ends without a record; narrowing the window is a follow-up.
func TestAKilledAttemptCountsAndSpendsTheRetry(t *testing.T) {
	recs := []record{
		{Time: "2026-10-04T10:00:00Z", Event: "unavailable", PatchID: "p1", Reason: "quota"},
		{Time: "2026-10-05T09:00:00Z", Event: "started", PatchID: "p1"}, // the process died here: no record follows
	}
	if n := runsOn(recs, "2026-10-05"); n != 1 {
		t.Fatalf("runsOn = %d, want 1: a bare started counts", n)
	}
	st := standingOf(recs, "p1")
	if !st.retried || st.open() {
		t.Fatalf("a bare started must spend the one more attempt: %+v", st)
	}
	if r, closed := st.closer(); !closed || r.Event != "unavailable" {
		t.Fatalf("closer = %+v %v, want the unavailable record", r, closed)
	}
}
