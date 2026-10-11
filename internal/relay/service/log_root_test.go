package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// logRootSelection is the store directory <home>/state, as the linux-only helpers name it.
func logRootSelection(home string) store.StateSelection {
	return store.StateSelection{Path: filepath.Join(home, "state")}
}

// CRW-958 round 2: a log_root the configuration file names moves the relay service's daemon log, the
// file the launched daemon writes, the journal notes and the tail a failed start reports: the log is
// below log_root in a directory of the store's own, and nothing is written to the store directory.
// With log_root not configured the log stays in the store directory, as it always was.
func TestServiceLogFollowsTheConfiguredLogRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	logs := filepath.Join(home, "logs-disk")
	config := filepath.Join(home, "crw.json")
	if err := os.WriteFile(config, []byte(`{"paths":{"log_root":"`+logs+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", config)
	selection := logRootSelection(home)
	s, err := New(context.Background(), selection, home+"/socket")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "echo daemon-started")
	if err := s.launch(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := s.JournalNote("degraded once"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.logPath(), logs+"/relay/") {
		t.Fatalf("log path %q is not below log_root %q", s.logPath(), logs)
	}
	raw, err := os.ReadFile(s.logPath())
	if err != nil || !strings.Contains(string(raw), "daemon-started") || !strings.Contains(string(raw), "degraded once") {
		t.Errorf("log below log_root: %q %v", raw, err)
	}
	if tail := s.logTail(); !strings.Contains(tail, "daemon-started") {
		t.Errorf("tail %q does not read the moved log", tail)
	}
	if _, err := os.Stat(filepath.Join(selection.Path, "daemon.log")); err == nil {
		t.Error("the daemon log was also written to the store directory")
	}

	// nothing configured: the log is in the store directory
	if err := os.WriteFile(config, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "other")
	s, err = New(context.Background(), logRootSelection(other), other+"/socket")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(other, "state", "daemon.log"); s.logPath() != want {
		t.Errorf("unconfigured log path %q, want %q", s.logPath(), want)
	}

	// a log_root that cannot be used refuses the service
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"paths":{"log_root":"`+plain+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), selection, home+"/socket"); err == nil || !strings.Contains(err.Error(), "not_directory") {
		t.Errorf("a log_root that is a file: %v", err)
	}
}
