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

// CRW-1359 d4: a valid tracking section makes the storage entry of the plain doctor report carry a
// tracking key with set, kind and repo, in that order, before the findings; no section makes none.
func TestDoctorStorageCarriesTheTrackingKeyForAValidSection(t *testing.T) {
	h := newHost(t)
	config := filepath.Join(h.home, "crw.json")
	h.env = append(h.env, "CRW_CONFIG="+config)
	for name, tc := range map[string]struct {
		doc        string
		kind, repo string
	}{
		"github":       {`{"tracking":{"kind":"github","repo":"o/work"}}`, "github", "o/work"},
		"default kind": {`{"tracking":{"repo":"o/work"}}`, "github", "o/work"},
		"linear":       {`{"tracking":{"kind":"linear"}}`, "linear", ""},
	} {
		write(t, config, tc.doc, 0o600)
		storage := golden.Obj(record.Get(h.diagnose(t), "storage"))
		tracking, ok := record.Get(storage, "tracking").(record.Object)
		if !ok {
			t.Errorf("%s: the storage entry has no tracking key:\n%s", name, golden.Canon(storage))
			continue
		}
		var keys []string
		for _, f := range tracking {
			keys = append(keys, f.Key)
		}
		if strings.Join(keys, ",") != "set,kind,repo" || record.Get(tracking, "set") != true || record.Get(tracking, "kind") != tc.kind || record.Get(tracking, "repo") != tc.repo {
			t.Errorf("%s: tracking %s", name, golden.Canon(tracking))
		}
		before := true
		for _, f := range storage {
			if f.Key == "tracking" {
				break
			}
			before = before && f.Key != "findings"
		}
		if !before || strings.Contains(golden.Canon(record.Get(storage, "findings")), "tracking_") {
			t.Errorf("%s: tracking after findings, or a finding for a valid section:\n%s", name, golden.Canon(storage))
		}
	}
	write(t, config, `{}`, 0o600)
	if storage := golden.Obj(record.Get(h.diagnose(t), "storage")); record.Get(storage, "tracking") != nil {
		t.Errorf("no section, yet a tracking key:\n%s", golden.Canon(storage))
	}
}
