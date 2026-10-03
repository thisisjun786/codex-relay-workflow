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
	if n := runsOn(recs, "2026-10-04"); n != 2 || runsOn(recs, "2026-10-05") != 0 {
		t.Fatalf("runs on the UTC day: %d", n)
	}
	appendRaw("not a record\n")
	if _, err := l.read(); err == nil || !strings.Contains(err.Error(), l.path()) || !strings.Contains(err.Error(), "line 4") {
		t.Fatalf("a malformed line must fail closed naming the file and the line: %v", err)
	}
}
