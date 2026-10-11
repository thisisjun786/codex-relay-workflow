package evidence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CRW-958: the process runner the forge reads through starts its commands with the tool roots the
// configuration names, and with nothing configured passes the environment on untouched.
func TestExecRunnerStartsCommandsWithTheConfiguredToolRoots(t *testing.T) {
	disk := t.TempDir()
	work := filepath.Join(disk, "gotmp")
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{"paths":{"build_cache":"`+filepath.Join(disk, "bc")+`","build_work":"`+work+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", file)
	t.Setenv("GOCACHE", "/caller/gocache")
	run := ExecRunner(context.Background())
	code, out, errOut, err := run([]string{"/bin/sh", "-c", `printf '%s|%s' "$GOCACHE" "$GOTMPDIR"`}, 20*time.Second)
	if err != nil || code != 0 || out != filepath.Join(disk, "bc")+"|"+work {
		t.Errorf("configured: code %d out %q err %q %v", code, out, errOut, err)
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		t.Errorf("build_work not made: %v", err)
	}
	t.Setenv("CRW_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	_, out, _, err = run([]string{"/bin/sh", "-c", `printf '%s|%s' "$GOCACHE" "$GOTMPDIR"`}, 20*time.Second)
	if err != nil || !strings.HasPrefix(out, "/caller/gocache|") {
		t.Errorf("nothing configured: out %q err %v", out, err)
	}
}
