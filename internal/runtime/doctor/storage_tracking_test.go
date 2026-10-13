package doctor_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// CRW-1341: a malformed tracking section is a warning in the storage entry of the plain doctor
// report and never its refusal; a file without the section adds nothing, since a host that has not
// set it yet is diagnosed on every run.
func TestDoctorStorageWarnsOnAMalformedTrackingSection(t *testing.T) {
	h := newHost(t)
	config := filepath.Join(h.home, "crw.json")
	write(t, config, `{}`, 0o600)
	h.env = append(h.env, "CRW_CONFIG="+config)
	storage := golden.Obj(record.Get(h.diagnose(t), "storage"))
	if refusal := record.Get(storage, "refusal"); refusal != nil || strings.Contains(golden.Canon(storage), `"tracking":`) || strings.Contains(golden.Canon(storage), "tracking_") {
		t.Errorf("a file without the section:\n%s", golden.Canon(storage))
	}
	write(t, config, `{"tracking":{"kind":"github","repo":"not a repo"}}`, 0o600)
	storage = golden.Obj(record.Get(h.diagnose(t), "storage"))
	findings := golden.Canon(record.Get(storage, "findings"))
	if refusal := record.Get(storage, "refusal"); refusal != nil || !strings.Contains(findings, "tracking_invalid") || !strings.Contains(findings, "OWNER/NAME") {
		t.Errorf("a malformed section:\n%s", golden.Canon(storage))
	}
}
