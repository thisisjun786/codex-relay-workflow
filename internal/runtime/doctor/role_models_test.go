package doctor_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// roleStore writes the helper role store (subagents.json) of an isolated host and points CRW_HOME at its directory.
func (h *host) roleStore(t *testing.T, document string) {
	t.Helper()
	dir := filepath.Join(h.home, "crw-home")
	h.env = append(h.env, "CRW_HOME="+dir)
	write(t, filepath.Join(dir, "subagents.json"), document, 0o600)
}

// stringsAt is the strings of the array at path in the report.
func stringsAt(t *testing.T, report record.Object, path ...string) []string {
	t.Helper()
	list, ok := at(report, path...).([]any)
	if !ok {
		t.Fatalf("%v is not an array: %s", path, golden.Canon(at(report, path...)))
	}
	out := []string{}
	for _, item := range list {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

// CRW-1324 item 9: a model a helper role routes to that no known model catalogue lists is a warning, with the catalogue that was
// read named in the output, so a role pointing at a model the provider does not serve (inferhub/kimi-k3, 10-13) is seen before a
// spawn fails. The catalogue is read through the Options seam: no OCX process and no network call is made here.
func TestDoctorWarnsOfARoleHelperModelNoCatalogueLists(t *testing.T) {
	h := newHost(t)
	h.roleStore(t, `{"roles":{
		"explorer":{"mode":"model","model":"gpt-5.5","effort":"low"},
		"reviewer":{"mode":"model","model":"inferhub/kimi-k3","effort":"high"},
		"executor":{"mode":"model","model":"gpt-5.5","fallback":{"model":"inferhub/gone-2","effort":null}},
		"architect":{"mode":"default"}}}`)
	calls := 0
	options := h.options()
	options.ModelCatalog = func(context.Context, scope.Env) doctor.ModelCatalogReading {
		calls++
		return doctor.ModelCatalogReading{State: "ocx-active", Source: "ocx", Models: []string{"gpt-5.5", "inferhub/kimi-k2"}}
	}
	report := doctor.Diagnose(context.Background(), options)
	if calls != 1 {
		t.Errorf("the catalogue was read %d times, want once", calls)
	}
	warnings := stringsAt(t, report, "roleModels", "warnings")
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q, want one for the reviewer and one for the executor's fallback", warnings)
	}
	for i, want := range [][]string{{"reviewer", "inferhub/kimi-k3", "ocx-active"}, {"executor", "fallback", "inferhub/gone-2", "ocx-active"}} {
		for _, part := range want {
			if !strings.Contains(warnings[i], part) {
				t.Errorf("warning %d lacks %q: %s", i, part, warnings[i])
			}
		}
	}
	if got := at(report, "roleModels", "catalogue", "state"); got != "ocx-active" {
		t.Errorf("the catalogue read is not named: %s", golden.Canon(at(report, "roleModels", "catalogue")))
	}
	canon := golden.Canon(at(report, "roleModels", "roles"))
	for _, want := range []string{`"model":"gpt-5.5"`, `"listed":true`, `"listed":false`} {
		if !strings.Contains(canon, want) {
			t.Errorf("the model list lacks %s:\n%s", want, canon)
		}
	}
}

// Roles that follow the main model name no model, so there is nothing to check and no catalogue is read.
func TestDoctorReadsNoModelCatalogueWhenNoRoleNamesAModel(t *testing.T) {
	h := newHost(t)
	options := h.options()
	options.ModelCatalog = func(context.Context, scope.Env) doctor.ModelCatalogReading {
		t.Fatal("the catalogue was read although no role names a model")
		return doctor.ModelCatalogReading{}
	}
	report := doctor.Diagnose(context.Background(), options)
	if w := stringsAt(t, report, "roleModels", "warnings"); len(w) != 0 {
		t.Errorf("warnings with no role model: %q", w)
	}
}

// A catalogue that could not be read lists nothing, so it condemns nothing: the state is recorded and no model is warned about.
func TestDoctorRecordsAnUnreadableModelCatalogueWithoutWarningOfModels(t *testing.T) {
	h := newHost(t)
	h.roleStore(t, `{"roles":{"reviewer":{"mode":"model","model":"inferhub/kimi-k3"}}}`)
	options := h.options()
	options.ModelCatalog = func(context.Context, scope.Env) doctor.ModelCatalogReading {
		return doctor.ModelCatalogReading{State: "unavailable", Source: "ocx", Detail: "OCX model discovery failed. Check OCX and refresh."}
	}
	report := doctor.Diagnose(context.Background(), options)
	if w := stringsAt(t, report, "roleModels", "warnings"); len(w) != 0 {
		t.Errorf("a model was condemned by a catalogue nobody read: %q", w)
	}
	if got := golden.Canon(at(report, "roleModels", "catalogue")); !strings.Contains(got, "unavailable") || !strings.Contains(got, "OCX model discovery failed") {
		t.Errorf("the unread catalogue is not recorded: %s", got)
	}
}
