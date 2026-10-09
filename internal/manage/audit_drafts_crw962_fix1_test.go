package manage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-962 (fix round 1): a posted draft whose severity is not one of P0-P3 is refused by name.
func TestCRW962MarkPostedRefusesAnUnknownSeverity(t *testing.T) {
	state, _, fingerprint := crw962Posted(t)
	path := filepath.Join(state, "drafts", fingerprint+".json")
	doc := auditDraftLoadAt(t, state, fingerprint)
	doc.Severity = ""
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := crw962MarkPosted(t, fingerprint, "--to", "P1")
	if code == 0 || !strings.Contains(stderr, "invalid_severity") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}
