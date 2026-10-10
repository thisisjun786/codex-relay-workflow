package hook_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1112, isolated trial r1 S5-F2: in session s4 an executor quoted the verifier directive (which itself names
// `EVIDENCE_RECORDED: <path>`) and ended its reply with a correct marker line for a real, non-empty receipt. The first-match
// extraction took the quoted "<path>`", refused the receipt, and the child spent its budget. The message is the child's own final
// answer from that trial, with the trial's workspace path replaced by <CWD>.
func TestSubagentStopS4QuotedDirectiveReceiptIsAccepted(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "subagentstop_s4_quoted_directive.txt"))
	if err != nil {
		t.Fatal(err)
	}
	cwd := subagentStopWorkspace(t)
	subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/out4-verification.txt"), "cat out4.txt -> ok (exit 0)\n")
	message := strings.ReplaceAll(string(raw), "<CWD>", cwd)
	if out := counterStop(t, cwd, "s1", "a1", "t1", message); out != "" {
		t.Fatalf("the S4 child's valid receipt was refused: %s", out)
	}
}

// A receipt whose name starts with two dots lies inside the evidence root and is accepted; a path that leaves it is not.
func TestSubagentStopDotDotNamedReceipt(t *testing.T) {
	for _, name := range []string{"..notes.txt", "..d/result.txt"} {
		cwd := subagentStopWorkspace(t)
		subagentStopPut(t, filepath.Join(cwd, ".crw/evidence", name), "verified")
		if out := counterStop(t, cwd, "s1", "a1", "", "EVIDENCE_RECORDED: .crw/evidence/"+name); out != "" {
			t.Fatalf("%s: %s", name, out)
		}
	}
}
