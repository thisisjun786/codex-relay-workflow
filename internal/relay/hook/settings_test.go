package hook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func Test33SettingsPython(t *testing.T) {
	base := Object{{Key: "configVersion", Value: int64(1)}, {Key: "relayExecutable", Value: "/relay"}, {Key: "markerRoot", Value: "/markers"}, {Key: "mode", Value: Observe}}
	values := []any{nil, false, true, int64(-1), int64(0), int64(1), float64(1.5), float64(9), float64(86401), "", "relative", "/absolute", Object{}, []any{}, "plugin", "hold", "faults_only"}
	inputs := []any{nil, []any{}, base}
	for _, key := range []string{"configVersion", "relayExecutable", "markerRoot", "dbPath", "socketPath", "mode", "owner", "adapterInterpreter", "adapterEntryPoint", "timeoutSeconds", "journalRoot", "journalPolicy"} {
		for _, v := range values {
			inputs = append(inputs, set(append(Object{}, base...), key, v))
		}
	}
	for _, budget := range []any{int64(5), int64(9), int64(86401), true} {
		o := set(append(Object{}, base...), "owner", "plugin")
		o = set(o, "timeoutSeconds", budget)
		inputs = append(inputs, o)
	}
	raw := evidence.Dumps(inputs, false, false, true)
	out := pyoracle.Answer(t, "complaints", func() ([]byte, error) {
		return pythonScript(t, nil, []byte(raw), "-c", "import json,sys;sys.path.insert(0,sys.argv[1]);from crw_runtime.completion import complaints;print(json.dumps([complaints(v) for v in json.load(sys.stdin)]))", filepath.Join(testRoot, "scripts"))
	})
	var expected [][]string
	if err := json.Unmarshal(out, &expected); err != nil {
		t.Fatal(err)
	}
	for i, v := range inputs {
		got := Complaints(v)
		if !reflect.DeepEqual(got, expected[i]) {
			t.Fatalf("case %d %s\nGo %v\nPython %v", i, evidence.Dumps(v, false, false, true), got, expected[i])
		}
	}
}
func Test33SettingsSpecialFiles(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "settings")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	_, failure, _ := ReadSettings(context.Background(), path)
	if failure != "config_unreadable" {
		t.Fatal(failure)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}
	_, failure, _ = ReadSettings(context.Background(), path)
	if failure != "config_unreadable" {
		t.Fatal(failure)
	}
}
