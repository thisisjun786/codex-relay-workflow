package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// storageDoctorSelection runs `crw relay --socket <socket> doctor` and returns the state selection it
// reports: path and source.
func storageDoctorSelection(t *testing.T, args ...string) (path, source string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	run(context.Background(), "crw", append(append([]string{"relay"}, args...), "doctor"), &stdout, &stderr)
	var report struct {
		StateSelection struct {
			Path   string `json:"path"`
			Source string `json:"source"`
		} `json:"stateSelection"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("relay doctor: %v\nstdout %s\nstderr %s", err, stdout.String(), stderr.String())
	}
	return report.StateSelection.Path, report.StateSelection.Source
}

// CRW-958: the relay chosen by the configuration file alone. `crw relay` started directly selects the
// scope directory below paths.relay_state, which is what the management tool's relay child (it asks
// this same doctor) selects for the same configuration and socket; CODEX_SESSION_RELAY_STATE and
// --state come first, and a host with no configured root keeps the XDG base.
func TestRun_a_configured_relay_state_is_chosen_by_every_relay_start(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	short, err := os.MkdirTemp("", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	root := filepath.Join(short, "relay") // short: the store socket below it stays under the bind limit
	file := filepath.Join(home, "crw.json")
	if err := os.WriteFile(file, []byte(`{"paths":{"relay_state":"`+root+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", file)
	socket := filepath.Join(home, "s.sock")
	scope, err := store.SocketScope(socket)
	if err != nil {
		t.Fatal(err)
	}

	if path, source := storageDoctorSelection(t, "--socket", socket); path != filepath.Join(root, scope) || source != "config" {
		t.Errorf("configuration only: %s from %s, want %s from config", path, source, filepath.Join(root, scope))
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(home, "env-store"))
	if path, source := storageDoctorSelection(t, "--socket", socket); path != filepath.Join(home, "env-store") || source != "env" {
		t.Errorf("with the environment variable: %s from %s", path, source)
	}
	if path, source := storageDoctorSelection(t, "--state", filepath.Join(home, "flag-store"), "--socket", socket); path != filepath.Join(home, "flag-store") || source != "flag" {
		t.Errorf("with --state: %s from %s", path, source)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Setenv("CRW_CONFIG", filepath.Join(home, "absent.json"))
	if path, source := storageDoctorSelection(t, "--socket", socket); path != filepath.Join(home, "xdg", "codex-session-relay", scope) || source != "xdg" {
		t.Errorf("no configuration: %s from %s", path, source)
	}
}
