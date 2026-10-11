package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// linkedRoot makes <base>/a/link -> <base>/b/deep and returns the root <base>/a/link/../root, which the
// kernel resolves to <base>/b/root, together with the real place and the place a lexical cleaning of the
// spelling would name (<base>/a/root). Both parents are writable.
func linkedRoot(t *testing.T) (spelled, real, cleaned string) {
	t.Helper()
	base := t.TempDir()
	for _, dir := range []string{"a", "b/deep"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "b", "deep"), filepath.Join(base, "a", "link")); err != nil {
		t.Fatal(err)
	}
	return base + "/a/link/../root", filepath.Join(base, "b", "root"), filepath.Join(base, "a", "root")
}

// CRW-958 fix round 3: a log_root spelled through a symbolic link and ".." names the directory the
// filesystem gives that spelling. The daemon log is made and opened there, not in the directory a
// lexical cleaning of the spelling names.
func TestServiceLogRootKeepsItsRawSpelling(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	spelled, real, cleaned := linkedRoot(t)
	config := filepath.Join(home, "crw.json")
	if err := os.WriteFile(config, []byte(`{"paths":{"log_root":"`+spelled+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", config)
	s, err := New(context.Background(), logRootSelection(home), home+"/socket")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "echo daemon-started")
	if err := s.launch(cmd); err != nil {
		t.Fatalf("the daemon log below %s could not be opened: %v", spelled, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	logs, _ := filepath.Glob(filepath.Join(real, "relay", "*", "daemon.log"))
	if len(logs) != 1 {
		t.Fatalf("daemon log below the real directory %s: %v", real, logs)
	}
	if raw, _ := os.ReadFile(logs[0]); !strings.Contains(string(raw), "daemon-started") {
		t.Errorf("log %q", raw)
	}
	if _, err := os.Stat(cleaned); err == nil {
		t.Errorf("the lexically cleaned directory %s was made", cleaned)
	}
}
