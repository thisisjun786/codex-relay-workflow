package install_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
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

// The plugin bridge record is written byte for byte as bridgerecord.document plus
// json.dumps(indent=2, sort_keys=True) writes it, version 1 without a policy and version 2 with
// one (goldens captured from scripts/crw_runtime).
func TestBridgeRecordBytesArePythons(t *testing.T) {
	for _, f := range golden.Obj(record.Get(golden.Obj(golden.Section(t, "settingsDocuments")), "bridgeRecords")) {
		given := golden.Obj(record.Get(golden.Obj(f.Value), "inputs"))
		var policy record.Object
		if p := golden.Obj(record.Get(given, "policy")); p != nil {
			policy = record.Object{{Key: "path", Value: record.Get(p, "path")}, {Key: "digest", Value: record.Get(p, "digest")}}
		}
		got := string(record.Encode(install.BridgeDocument(text(record.Get(given, "command")), strList(record.Get(given, "args")), text(record.Get(given, "name")), text(record.Get(given, "issue")), policy)))
		if want := text(record.Get(golden.Obj(f.Value), "bytes")); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", f.Key, got, want)
		}
	}
}

// The plugin-owned Stop settings are completion.configuration's bytes with adapterInterpreter
// /usr/bin/env and the Go hook through the pointer as adapterEntryPoint, and both the Python
// reader and the Go hook's own reader accept them.
func TestHookSettingsBytesArePythons(t *testing.T) {
	for _, f := range golden.Obj(record.Get(golden.Obj(golden.Section(t, "settingsDocuments")), "hookSettings")) {
		given := golden.Obj(record.Get(golden.Obj(f.Value), "inputs"))
		timeout, _ := record.Get(given, "timeout").(int64)
		document, err := install.HookSettings{
			Destination: text(record.Get(given, "destination")), MarkerRoot: text(record.Get(given, "marker_root")), Database: text(record.Get(given, "database")),
			Mode: text(record.Get(given, "mode")), Timeout: timeout, JournalRoot: text(record.Get(given, "journal_root")), CodexHome: text(record.Get(given, "codex_home")),
			Issue: text(record.Get(given, "issue")), Isolation: text(record.Get(given, "isolation")), Socket: text(record.Get(given, "socket")),
		}.Document(func() (string, error) { t.Fatal("the marker root is given"); return "", nil })
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(record.Encode(document)), text(record.Get(golden.Obj(f.Value), "bytes")); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", f.Key, got, want)
		}
		if len(golden.List(record.Get(golden.Obj(f.Value), "complaints"))) != 0 || len(install.Complaints(document)) != 0 {
			t.Errorf("%s is refused: %v", f.Key, install.Complaints(document))
		}
		if install.PythonEra(document) || !install.GoEra(document) {
			t.Errorf("%s is not read as the Go adapter's", f.Key)
		}
	}
}

// Superseding copies a document aside under <path>.superseded-<stamp>, a name that sorts after
// every archive already there (a future stamp included), with its bytes and permission bits, and
// leaves the document where it is: it is then replaced by a rename over the path, so a Stop
// never finds the path empty.
func TestSupersedeCopiesAsideAndLeavesThePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, install.SettingsName)
	write(t, path+".superseded-29990101T000000Z", "future")
	write(t, path, "live")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	archived, err := install.Supersede(path)
	if err != nil {
		t.Fatal(err)
	}
	if archived != path+".superseded-29990101T000000Z-001" {
		t.Fatalf("archived to %s", archived)
	}
	if raw, _ := os.ReadFile(archived); string(raw) != "live" {
		t.Fatal("the archived bytes are not the document's")
	}
	if info, err := os.Stat(archived); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("the archive does not keep the document's permission bits: %v %v", info.Mode(), err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "live" {
		t.Fatalf("the document left its path: %q %v", raw, err)
	}
	if read := reading.ReadText(path+".superseded-29990101T000000Z", "x"); !strings.Contains(text(read.Value), "future") {
		t.Fatal("an older archive was touched")
	}
}
