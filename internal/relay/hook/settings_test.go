package hook

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func Test33SettingsPython(t *testing.T) {
	base := Object{{Key: "configVersion", Value: int64(1)}, {Key: "relayExecutable", Value: "/relay"}, {Key: "markerRoot", Value: "/markers"}, {Key: "mode", Value: Observe}}
	values := []any{nil, false, true, int64(-1), int64(0), int64(1), float64(1.5), float64(9), float64(86401), "", "relative", "/absolute", Object{}, []any{}, "plugin", "hold", "faults_only"}
	inputs := []any{nil, []any{}, base}
	for _, key := range []string{"configVersion", "relayExecutable", "markerRoot", "dbPath", "socketPath", "mode", "owner", "adapterInterpreter", "adapterEntryPoint", "timeoutSeconds", "journalRoot", "journalPolicy"} {
		for _, v := range values {
			inputs = append(inputs, append(Object{}, base...).Set(key, v))
		}
	}
	for _, budget := range []any{int64(5), int64(9), int64(86401), true} {
		o := append(Object{}, base...).Set("owner", "plugin")
		o = o.Set("timeoutSeconds", budget)
		inputs = append(inputs, o)
	}
	// Each input's complaints are the golden, which began as completion.complaints's. Python
	// also named the adapter keys the retired launchers ran; nothing reads them since decision 66,
	// so its complaints about them were never this reader's.
	var complaints []any
	for _, v := range inputs {
		complaints = append(complaints, Complaints(v))
	}
	goldenDumps(t, "complaints", complaints, false)
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
