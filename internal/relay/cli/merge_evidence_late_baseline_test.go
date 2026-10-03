package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// The restatement of a late-thread scenario without a dispositions document, as the command
// printed it before dispositions existed. This file ran green on that code and runs unchanged
// afterwards.
func TestRestatementWithoutDispositionsIsUnchanged(t *testing.T) {
	code, p := lateRestate(t)
	if code != 2 {
		t.Fatal(code, p)
	}
	raw, err := json.Marshal(lateRestatement(t, p))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"current":false,"headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","problems":[{"code":"late_finding","detail":"2 review thread(s) on this head are not in the record's threadsSeen, so the record did not see them and no longer describes the candidate: u/T1, u/T2"}]}`
	if string(raw) != want {
		t.Fatalf("got %s", raw)
	}
	_, out, _ := lateExec(t, scriptedForge{late: true}, "--restate", lateRecord(t))
	if strings.Contains(out, "lateDispositions") {
		t.Fatalf("the payload names lateDispositions without the flag: %s", out)
	}
}
