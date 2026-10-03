package cli

import (
	"strings"
	"testing"
)

// The restatement of a late-thread scenario without a dispositions document, as the command
// printed it before dispositions existed: the bytes of its block in the output. This file ran green
// on that code and runs unchanged afterwards.
func TestRestatementWithoutDispositionsIsUnchanged(t *testing.T) {
	code, out, stderr := lateExec(t, scriptedForge{late: true}, "--restate", lateRecord(t))
	if code != 2 {
		t.Fatal(code, out, stderr)
	}
	const want = "\"restatement\": {\n    \"current\": false,\n    \"headSha\": \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\n    \"problems\": [\n      {\n        \"code\": \"late_finding\",\n        \"detail\": \"2 review thread(s) on this head are not in the record's threadsSeen, so the record did not see them and no longer describes the candidate: u/T1, u/T2\"\n      }\n    ]\n  },\n"
	if !strings.Contains(out, want) {
		t.Fatalf("the restatement block is not what it was: %s", out)
	}
	if strings.Contains(out, "lateDispositions") {
		t.Fatalf("the payload names lateDispositions without the flag: %s", out)
	}
}
