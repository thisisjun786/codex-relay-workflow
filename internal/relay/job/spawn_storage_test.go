package job

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// storageJobConfig points the process at a configuration file whose paths section is the given pairs.
func storageJobConfig(t *testing.T, pairs ...string) {
	t.Helper()
	var paths []string
	for i := 0; i+1 < len(pairs); i += 2 {
		paths = append(paths, `"`+pairs[i]+`":"`+pairs[i+1]+`"`)
	}
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{"paths":{`+strings.Join(paths, ",")+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", file)
}

// CRW-958: a job runs with the tool roots the configuration names. Only the configuration file is
// changed here, and the job's own shell reports the GOCACHE, GOMODCACHE and GOTMPDIR it was given; a
// build_work that did not exist is made first, and the rest of the environment is inherited.
func TestRunBackgroundGivesTheJobTheConfiguredToolRoots(t *testing.T) {
	ws := workspace(t)
	disk := t.TempDir()
	cache, mod, work := filepath.Join(disk, "bc"), filepath.Join(disk, "mc"), filepath.Join(disk, "gotmp")
	storageJobConfig(t, "build_cache", cache, "module_cache", mod, "build_work", work)
	t.Setenv("CRW_JOB_PROBE", "inherited")
	t.Setenv("GOCACHE", "/caller/gocache")
	rec, err := runBackground(ws, RunOptions{Command: []string{"/bin/sh", "-c", `printf '%s|%s|%s|%s' "$GOCACHE" "$GOMODCACHE" "$GOTMPDIR" "$CRW_JOB_PROBE"`}}, time.Now, func(cmd *exec.Cmd) error { return cmd.Start() })
	if err != nil {
		t.Fatal(err)
	}
	if rec.PID != nil {
		pid := *rec.PID
		t.Cleanup(func() { _ = unix.Kill(-pid, unix.SIGKILL) })
	}
	settled(t, ws, rec.ID)
	if got, want := get(t, OutPath(ws, rec.ID)), strings.Join([]string{cache, mod, work, "inherited"}, "|"); got != want {
		t.Errorf("the job saw %q, want %q", got, want)
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		t.Errorf("build_work was not made before the job started: %v", err)
	}
}

// CRW-958: with no tool root configured the job inherits the environment untouched (cmd.Env stays
// unset), and a tool root that fails its check stops the job before anything starts.
func TestRunBackgroundLeavesTheEnvironmentAloneWithoutConfiguredRoots(t *testing.T) {
	ws := workspace(t)
	t.Setenv("CRW_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	var env []string
	var set bool
	stop := errors.New("stop before the start")
	_, err := runBackground(ws, RunOptions{Command: []string{"true"}}, time.Now, func(cmd *exec.Cmd) error {
		env, set = cmd.Env, cmd.Env != nil
		return stop
	})
	if err == nil || set {
		t.Errorf("nothing configured: err = %v, Env = %q", err, env)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	storageJobConfig(t, "build_cache", plain)
	starts := 0
	_, err = runBackground(ws, RunOptions{Command: []string{"true"}}, time.Now, func(*exec.Cmd) error { starts++; return nil })
	if err == nil || starts != 0 || !strings.Contains(err.Error(), "build_cache") {
		t.Errorf("a build_cache that is a file: err = %v, starts = %d", err, starts)
	}
}
