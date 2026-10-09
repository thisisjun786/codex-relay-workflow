package spawn

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The recorder's JSON transform keeps every key of a payload, "__proto__" included (CRW-1029, verifier P2): a plain assignment into
// the copy would run the __proto__ setter and drop the key from the recorded input and the oracle expectation.
func TestRecorderDeepKeepsEveryJSONKey(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	out, err := exec.Command("node", filepath.Join("testdata", "hook", "deep-check.mjs")).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "deep: ok") {
		t.Fatalf("deep-check.mjs: %v\n%s", err, out)
	}
}
