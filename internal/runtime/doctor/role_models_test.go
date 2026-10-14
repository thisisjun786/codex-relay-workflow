package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// CRW-1360 d1: a native catalogue the role helper would not accept as authoritative (the file is older than a day or gone) is a
// list that proves nothing, so it condemns no model: no "does not list" warning, the freshness is recorded, and a listed model is
// still listed. A fresh native or OCX catalogue still warns.
func TestDoctorDoesNotCondemnAModelOnAStaleNativeCatalogue(t *testing.T) {
	store := `{"roles":{"reviewer":{"mode":"model","model":"inferhub/kimi-k3"},"explorer":{"mode":"model","model":"gpt-5.5"}}}`
	for name, tc := range map[string]struct {
		reading doctor.ModelCatalogReading
		warns   bool
	}{
		"stale native": {doctor.ModelCatalogReading{State: "native-catalog", Source: "native", Models: []string{"gpt-5.5"}, Freshness: "native-stale"}, false},
		"missing file": {doctor.ModelCatalogReading{State: "native-catalog", Source: "native", Models: []string{"gpt-5.5"}, Freshness: "native-missing"}, false},
		"fresh native": {doctor.ModelCatalogReading{State: "native-catalog", Source: "native", Models: []string{"gpt-5.5"}, Freshness: "authoritative"}, true},
		"fresh ocx":    {doctor.ModelCatalogReading{State: "ocx-active", Source: "ocx", Models: []string{"gpt-5.5"}, Freshness: "authoritative"}, true},
	} {
		h := newHost(t)
		h.roleStore(t, store)
		options := h.options()
		options.ModelCatalog = func(context.Context, scope.Env) doctor.ModelCatalogReading { return tc.reading }
		report := doctor.Diagnose(context.Background(), options)
		warnings := stringsAt(t, report, "roleModels", "warnings")
		if tc.warns && len(warnings) != 1 || !tc.warns && len(warnings) != 0 {
			t.Errorf("%s: warnings %q", name, warnings)
		}
		catalogue := golden.Canon(at(report, "roleModels", "catalogue"))
		if !strings.Contains(catalogue, `"freshness":"`+tc.reading.Freshness+`"`) || !strings.Contains(catalogue, `"authoritative":`+strconv.FormatBool(tc.warns)) {
			t.Errorf("%s: the freshness is not recorded:\n%s", name, catalogue)
		}
		roles := golden.Canon(at(report, "roleModels", "roles"))
		if !strings.Contains(roles, `"model":"gpt-5.5"`) || !strings.Contains(roles, `"listed":true`) {
			t.Errorf("%s: a listed model is not listed:\n%s", name, roles)
		}
		if tc.warns != strings.Contains(roles, `"listed":false`) || (!tc.warns && !strings.Contains(roles, `"listed":null`)) {
			t.Errorf("%s: the unlisted model's state:\n%s", name, roles)
		}
	}
}

// CRW-1360 d1, through the production reader: the native catalogue file's age against the doctor's clock decides, with no OCX on
// PATH. The doctor writes nothing while it reads.
func TestDoctorReadsTheNativeCatalogueFreshnessFromTheFile(t *testing.T) {
	h := newHost(t)
	h.roleStore(t, `{"roles":{"reviewer":{"mode":"model","model":"inferhub/kimi-k3"}}}`)
	catalogue := filepath.Join(h.home, "models.json")
	write(t, catalogue, `[{"id":"gpt-5.5"}]`, 0o600)
	mtime := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(catalogue, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	noOcx := filepath.Join(h.home, "empty-path")
	mkdir(t, noOcx)
	h.env = append(h.env, "CODEX_MODELS_CACHE_PATH="+catalogue, "PATH="+noOcx)
	for name, tc := range map[string]struct {
		now   time.Time
		warns bool
	}{
		"stale": {mtime.Add(25 * time.Hour), false},
		"fresh": {mtime.Add(2 * time.Hour), true},
		// a file dated after the clock proves nothing either
		"future": {mtime.Add(-time.Hour), false},
	} {
		options := h.options()
		options.Now = func() time.Time { return tc.now }
		before := snapshot(t, h.home)
		report := doctor.Diagnose(context.Background(), options)
		if got := len(stringsAt(t, report, "roleModels", "warnings")) == 1; got != tc.warns {
			t.Errorf("%s: warns = %v, want %v\n%s", name, got, tc.warns, golden.Canon(at(report, "roleModels")))
		}
		if state := at(report, "roleModels", "catalogue", "state"); state != "native-catalog" {
			t.Errorf("%s: the catalogue read is %v", name, state)
		}
		if after := snapshot(t, h.home); after != before {
			t.Errorf("%s: the doctor wrote under the home", name)
		}
	}
}
