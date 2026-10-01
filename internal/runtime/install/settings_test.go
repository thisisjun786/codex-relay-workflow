package install_test

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	expected "github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func text(v any) string {
	s, _ := v.(string)
	return s
}

func strList(v any) []string {
	var out []string
	for _, item := range golden.List(v) {
		out = append(out, text(item))
	}
	return out
}

// settingsInputs is one section of the fixture settings-documents-inputs.json, the inputs the
// Python writers were given.
func settingsInputs(t *testing.T, section string) record.Object {
	t.Helper()
	inputs, err := reading.Decode(expected.Fixture(t, "settings-documents-inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return golden.Obj(record.Get(golden.Obj(inputs), section))
}

// The plugin bridge record is written byte for byte as bridgerecord.document plus
// json.dumps(indent=2, sort_keys=True) writes it, version 1 without a policy and version 2 with
// one: the golden began as scripts/crw_runtime's bytes.
func TestBridgeRecordBytesArePythons(t *testing.T) {
	for _, f := range settingsInputs(t, "bridgeRecords") {
		given := golden.Obj(f.Value)
		var policy record.Object
		if p := golden.Obj(record.Get(given, "policy")); p != nil {
			policy = record.Object{{Key: "path", Value: record.Get(p, "path")}, {Key: "digest", Value: record.Get(p, "digest")}}
		}
		expected.Check(t, f.Key, record.Encode(install.BridgeDocument(text(record.Get(given, "command")), strList(record.Get(given, "args")), text(record.Get(given, "name")), text(record.Get(given, "issue")), policy)))
	}
}

// The plugin-owned Stop settings are completion.configuration's bytes without the retired
// adapterInterpreter and adapterEntryPoint (decision 66): the golden began as those bytes. The
// Go hook's own reader accepts them.
func TestHookSettingsBytesArePythons(t *testing.T) {
	for _, f := range settingsInputs(t, "hookSettings") {
		given := golden.Obj(f.Value)
		timeout, _ := record.Get(given, "timeout").(int64)
		document, err := install.HookSettings{
			Destination: text(record.Get(given, "destination")), MarkerRoot: text(record.Get(given, "marker_root")), Database: text(record.Get(given, "database")),
			Mode: text(record.Get(given, "mode")), Timeout: timeout, JournalRoot: text(record.Get(given, "journal_root")), CodexHome: text(record.Get(given, "codex_home")),
			Issue: text(record.Get(given, "issue")), Isolation: text(record.Get(given, "isolation")), Socket: text(record.Get(given, "socket")),
		}.Document(func() (string, error) { t.Fatal("the marker root is given"); return "", nil })
		if err != nil {
			t.Fatal(err)
		}
		expected.Check(t, f.Key, record.Encode(document))
		if complaints := install.Complaints(document); len(complaints) != 0 {
			t.Errorf("%s is refused: %v", f.Key, complaints)
		}
	}
}
