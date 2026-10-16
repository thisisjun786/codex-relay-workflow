package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc/tmploctest"
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
	// a socket directory (never below TMPDIR): the store socket below the root stays under the bind limit whatever TMPDIR is (CRW-1568)
	root := filepath.Join(tmploctest.SocketDir(t, "rs"), "relay")
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

// CRW-958 round 2: a split key alone refuses a relay started directly, as it refuses the management
// tool; a relay_state the selection does not use (CODEX_SESSION_RELAY_STATE or --state chose the
// store) is not judged by either, so an unusable one does not stop a relay the environment points
// at a good store.
func TestRun_the_relay_refuses_a_split_key_alone_and_ignores_an_unused_relay_state(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	socket := filepath.Join(home, "s.sock")
	file := filepath.Join(home, "crw.json")
	if err := os.WriteFile(file, []byte(`{"paths":{"relay_wal":"`+filepath.Join(home, "wal")+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", file)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), "crw", []string{"relay", "--socket", socket, "doctor"}, &stdout, &stderr); code == 0 || !bytes.Contains(append(stdout.Bytes(), stderr.Bytes()...), []byte("state_split")) {
		t.Errorf("a split key alone: exit %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"paths":{"relay_state":"`+plain+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(home, "env-store"))
	if path, source := storageDoctorSelection(t, "--socket", socket); path != filepath.Join(home, "env-store") || source != "env" {
		t.Errorf("env over an unused relay_state: %s from %s", path, source)
	}
}
