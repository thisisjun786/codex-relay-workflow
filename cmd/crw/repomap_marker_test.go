package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The venv completion marker (CRW-1162). These tests run the real launcher (runRepoMap) against a fake python3 and a fake venv
// interpreter on PATH, so no real pip ever runs; a killed bootstrap is a real process killed by SIGKILL in the middle of the
// fake venv or the fake pip.

const repoMapKillHelperEnv = "CRW_MAP_KILL_HELPER"

func init() {
	if os.Getenv(repoMapKillHelperEnv) != "" {
		os.Exit(runRepoMap(invocation{ctx: context.Background(), args: []string{"."}, stdout: os.Stdout, stderr: os.Stderr}))
	}
}

// fakeVenvPython is the interpreter the fake `python3 -m venv` leaves in the venv: pip installs behave as FAKE_PIP says (hang,
// fail or succeed), anything else is the map run.
const fakeVenvPython = `#!/bin/sh
echo "venv-python $*" >> "$FAKE_LOG"
if [ "$1" = "-m" ] && [ "$2" = "pip" ]; then
  case "$FAKE_PIP" in
    hang) : > "$FAKE_SIGNAL"; exec /bin/sleep 60;;
    fail) exit 2;;
  esac
  exit 0
fi
echo "venv ran $*"
`

// fakePython3 is the python3 on PATH: `-m venv DIR` creates DIR/bin/python3 as FAKE_VENV says (hang after creating it, exit 3
// after creating it, exit 3 before touching anything, or succeed); any other call is the fallback map run.
const fakePython3 = `#!/bin/sh
echo "python3 $*" >> "$FAKE_LOG"
if [ "$1" = "-m" ] && [ "$2" = "venv" ]; then
  [ "$FAKE_VENV" = failearly ] && exit 3
  /bin/mkdir -p "$3/bin" || exit 1
  /bin/cp "$FAKE_VENV_PY" "$3/bin/python3" && /bin/chmod 755 "$3/bin/python3" || exit 1
  case "$FAKE_VENV" in
    hang) : > "$FAKE_SIGNAL"; exec /bin/sleep 60;;
    fail) exit 3;;
  esac
  exit 0
fi
echo "ran $*"
`

type fakeMapHost struct {
	t               *testing.T
	home, venv, log string
	signal          string
}

func newFakeMapHost(t *testing.T) *fakeMapHost {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	venvPy := filepath.Join(root, "venv-python")
	for path, body := range map[string]string{filepath.Join(bin, "python3"): fakePython3, venvPy: fakeVenvPython} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h := &fakeMapHost{t: t, home: filepath.Join(root, "home"), log: filepath.Join(root, "log"), signal: filepath.Join(root, "signal")}
	h.venv = filepath.Join(h.home, "venvs", "repomap")
	// PATH holds only the fakes: no uv, and never the host's python3 or pip.
	t.Setenv("PATH", bin)
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CRW_HOME", h.home)
	t.Setenv("FAKE_LOG", h.log)
	t.Setenv("FAKE_SIGNAL", h.signal)
	t.Setenv("FAKE_VENV_PY", venvPy)
	for _, k := range []string{"CRW_SKILLS_DIR", "CRW_PYTHON", "PLUGIN_ROOT"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	h.set("", "", true)
	return h
}

// set chooses how the fakes behave and whether the run opts into the bootstrap.
func (h *fakeMapHost) set(venv, pip string, bootstrap bool) {
	h.t.Setenv("FAKE_VENV", venv)
	h.t.Setenv("FAKE_PIP", pip)
	if bootstrap {
		h.t.Setenv("CRW_MAP_BOOTSTRAP", "1")
	} else {
		h.t.Setenv("CRW_MAP_BOOTSTRAP", "")
	}
}

// run is one in-process `crw map .`; it returns the exit code, stdout, stderr and the calls the fakes logged, and clears the log.
func (h *fakeMapHost) run() (int, string, string, []string) {
	h.t.Helper()
	_ = os.Remove(h.log)
	var stdout, stderr strings.Builder
	code := runRepoMap(invocation{ctx: context.Background(), args: []string{"."}, stdout: &stdout, stderr: &stderr})
	return code, stdout.String(), stderr.String(), h.calls()
}

func (h *fakeMapHost) calls() []string {
	data, err := os.ReadFile(h.log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// killDuring starts a real `crw map .` bootstrap, waits for the fake to reach its hang point and kills the whole process group
// with SIGKILL: neither crw nor its children run any cleanup.
func (h *fakeMapHost) killDuring(venv, pip string) {
	h.t.Helper()
	h.set(venv, pip, true)
	_ = os.Remove(h.signal)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), repoMapKillHelperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(h.signal); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			h.t.Fatal("the bootstrap never reached the fake's hang point")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-reaped
	_ = os.Remove(h.log)
}

// killLauncherOnly is killDuring for the case where only the crw process dies: the fake venv or pip it started is left running
// as an orphan (it is in the launcher's process group, which the cleanup kills). It returns when the fake reached its hang point
// and crw is gone.
func (h *fakeMapHost) killLauncherOnly(venv, pip string) int {
	h.t.Helper()
	h.set(venv, pip, true)
	_ = os.Remove(h.signal)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), repoMapKillHelperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	h.t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(h.signal); err == nil {
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatal("the bootstrap never reached the fake's hang point")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
	<-reaped
	_ = os.Remove(h.log)
	return pgid
}

// Killing only the crw process leaves the installer it started running. The bootstrap lock must stay held for as long as that
// installer lives, or a retry would build over a venv that is still being written (CRW-1162).
func TestRepoMapOrphanedInstallerKeepsTheBootstrapLock(t *testing.T) {
	for _, c := range []struct{ name, venv, pip string }{
		{"orphan-venv", "hang", ""},
		{"orphan-pip", "", "hang"},
	} {
		t.Run(c.name, func(t *testing.T) {
			poll := repoMapLockPoll
			repoMapLockPoll = 10 * time.Millisecond
			t.Cleanup(func() { repoMapLockPoll = poll })
			h := newFakeMapHost(t)
			orphan := h.killLauncherOnly(c.venv, c.pip)

			h.set("", "", true)
			ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
			defer cancel()
			var stderr strings.Builder
			runRepoMap(invocation{ctx: ctx, args: []string{"."}, stdout: io.Discard, stderr: &stderr})
			if !strings.Contains(stderr.String(), "waiting for another venv bootstrap") || countCalls(h.calls(), "-m venv") != 0 || countCalls(h.calls(), "-m pip") != 0 {
				t.Fatalf("a retry built over a venv whose installer is still running: stderr %q calls %v", stderr.String(), h.calls())
			}

			_ = syscall.Kill(-orphan, syscall.SIGKILL)
			_ = os.Remove(h.log)
			code, stdout, stderr2, calls := h.run()
			if code != 0 || !strings.Contains(stdout, "venv ran") || countCalls(calls, "-m pip install") != 1 {
				t.Fatalf("after the installer ended the retry did not rebuild once: exit %d stdout %q stderr %q calls %v", code, stdout, stderr2, calls)
			}
		})
	}
}

// A stale marker path that is not a marker file (a directory with contents the attempt did not create) is never deleted: the
// bootstrap refuses instead of removing it recursively (CRW-1162).
func TestRepoMapMarkerPathThatIsADirectoryIsNeverDeleted(t *testing.T) {
	h := newFakeMapHost(t)
	marker := filepath.Join(h.venv, repoMapVenvMarker)
	precious := filepath.Join(marker, "sub", "precious")
	if err := os.MkdirAll(filepath.Dir(precious), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(precious, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.set("fail", "", true)
	code, _, stderr, calls := h.run()
	if _, err := os.Stat(precious); err != nil {
		t.Fatalf("the contents of the marker-path directory were deleted: %v (exit %d stderr %q)", err, code, stderr)
	}
	if code == 0 || countCalls(calls, "-m venv") != 0 {
		t.Fatalf("the bootstrap went on past a marker path it could not clear: exit %d stderr %q calls %v", code, stderr, calls)
	}
	// With the interpreter present the directory is not a marker either: the venv is not ready and not used.
	if err := os.MkdirAll(filepath.Join(h.venv, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.venv, "bin", "python3"), []byte("#!/bin/sh\necho venv ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.set("", "", false)
	if code, stdout, _, _ := h.run(); code != 0 || strings.Contains(stdout, "venv ran") {
		t.Fatalf("a directory at the marker path made the venv ready: exit %d stdout %q", code, stdout)
	}
}

func countCalls(calls []string, part string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, part) {
			n++
		}
	}
	return n
}

// A bootstrap killed after python3 -m venv made the interpreter, or while pip was installing, leaves an interpreter without its
// requirements. It must not count as a ready venv: a run that did not opt in falls back, and the next opted-in run builds the
// venv again, once; after that the venv is ready and nothing is built (CRW-1162).
func TestRepoMapKilledBootstrapIsRebuiltOnce(t *testing.T) {
	for _, c := range []struct{ name, venv, pip string }{
		{"killed-right-after-venv", "hang", ""},
		{"killed-during-pip", "", "hang"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newFakeMapHost(t)
			h.killDuring(c.venv, c.pip)
			if _, err := os.Stat(filepath.Join(h.venv, "bin", "python3")); err != nil {
				t.Fatalf("the kill left no interpreter, so the scenario is not the one under test: %v", err)
			}
			h.set("", "", false)
			code, stdout, stderr, calls := h.run()
			if code != 0 || strings.Contains(stdout, "venv ran") || countCalls(calls, "venv-python") != 0 {
				t.Fatalf("a run without the bootstrap used the interpreter without requirements: exit %d stdout %q stderr %q calls %v", code, stdout, stderr, calls)
			}
			h.set("", "", true)
			code, stdout, stderr, calls = h.run()
			if code != 0 || !strings.Contains(stdout, "venv ran") || countCalls(calls, "-m pip install") != 1 {
				t.Fatalf("the rebuild did not install the requirements once: exit %d stdout %q stderr %q calls %v", code, stdout, stderr, calls)
			}
			code, stdout, stderr, calls = h.run()
			if code != 0 || !strings.Contains(stdout, "venv ran") || countCalls(calls, "-m venv") != 0 || countCalls(calls, "-m pip") != 0 {
				t.Fatalf("a ready venv was built again: exit %d stdout %q stderr %q calls %v", code, stdout, stderr, calls)
			}
			h.set("", "", false)
			if code, stdout, _, _ = h.run(); code != 0 || !strings.Contains(stdout, "venv ran") {
				t.Fatalf("a run without the bootstrap ignored the ready venv: exit %d stdout %q", code, stdout)
			}
		})
	}
}

// python3 -m venv can exit nonzero after it made the interpreter. The failed attempt removes what it made: the directory when it
// created it, only the interpreter when the directory was there before (its other contents stay), and the next opted-in run
// builds the venv again (CRW-1162).
func TestRepoMapVenvFailureCleansUpWhatItMade(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		name := "fresh-directory"
		if preexisting {
			name = "preexisting-directory"
		}
		t.Run(name, func(t *testing.T) {
			h := newFakeMapHost(t)
			kept := filepath.Join(h.venv, "kept-from-before")
			if preexisting {
				if err := os.MkdirAll(h.venv, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(kept, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h.set("fail", "", true)
			code, stdout, stderr, calls := h.run()
			if code != 0 || !strings.Contains(stdout, "ran -B") || strings.Contains(stdout, "venv ran") {
				t.Fatalf("the failed venv was not left to the fallback: exit %d stdout %q stderr %q calls %v", code, stdout, stderr, calls)
			}
			if _, err := os.Stat(filepath.Join(h.venv, "bin", "python3")); !os.IsNotExist(err) {
				t.Fatalf("the failed venv left its interpreter behind: %v", err)
			}
			_, keptErr := os.Stat(kept)
			if preexisting && keptErr != nil {
				t.Fatalf("the directory that was there before lost its contents: %v", keptErr)
			}
			if _, err := os.Stat(h.venv); preexisting == os.IsNotExist(err) {
				t.Fatalf("directory presence after a failed venv: preexisting %v, stat error %v", preexisting, err)
			}
			h.set("", "", true)
			code, stdout, stderr, calls = h.run()
			if code != 0 || !strings.Contains(stdout, "venv ran") || countCalls(calls, "-m pip install") != 1 {
				t.Fatalf("the next run did not build the venv: exit %d stdout %q stderr %q calls %v", code, stdout, stderr, calls)
			}
		})
	}
}

// An interpreter that was there before the attempt is not the attempt's to delete: a working venv from before the marker keeps
// its interpreter when the rebuild fails before it touched anything (python3 -m venv exits nonzero having made nothing) and when
// pip fails after python3 -m venv succeeded. The next opted-in run, with the failure gone, still builds the venv and marks it
// ready (CRW-1162).
func TestRepoMapFailedRebuildKeepsAnInterpreterThatWasThereBefore(t *testing.T) {
	for _, c := range []struct{ name, venv, pip string }{
		{"venv-fails-before-making-anything", "failearly", ""},
		{"pip-fails-after-venv", "", "fail"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newFakeMapHost(t)
			if err := os.MkdirAll(filepath.Join(h.venv, "bin"), 0o700); err != nil {
				t.Fatal(err)
			}
			legacy := filepath.Join(h.venv, "bin", "python3")
			kept := filepath.Join(h.venv, "kept-from-before")
			for _, f := range []string{legacy, kept} {
				if err := os.WriteFile(f, []byte(fakeVenvPython), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			h.set(c.venv, c.pip, true)
			code, stdout, stderr, calls := h.run()
			if code != 0 || !strings.Contains(stdout, "ran -B") || strings.Contains(stdout, "venv ran") {
				t.Fatalf("the failed rebuild was not left to the fallback: exit %d stdout %q stderr %q calls %v", code, stdout, stderr, calls)
			}
			for _, f := range []string{legacy, kept} {
				if _, err := os.Stat(f); err != nil {
					t.Fatalf("the failed rebuild deleted %s, which was there before it: %v", f, err)
				}
			}
			if _, err := os.Stat(filepath.Join(h.venv, repoMapVenvMarker)); !os.IsNotExist(err) {
				t.Fatalf("a failed rebuild left the completion marker: %v", err)
			}
			h.set("", "", true)
			code, stdout, stderr, calls = h.run()
			if code != 0 || !strings.Contains(stdout, "venv ran") || countCalls(calls, "-m pip install") != 1 {
				t.Fatalf("the next run did not build the venv: exit %d stdout %q stderr %q calls %v", code, stdout, stderr, calls)
			}
		})
	}
}

// A venv from before the marker has an interpreter and no marker. It is built again once, by the next opted-in run, which then
// marks it ready; a run that did not opt in does not take it for ready (CRW-1162).
func TestRepoMapRebuildsAnUnmarkedExistingVenvOnce(t *testing.T) {
	h := newFakeMapHost(t)
	if err := os.MkdirAll(filepath.Join(h.venv, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(h.venv, "bin", "python3")
	if err := os.WriteFile(legacy, []byte(fakeVenvPython), 0o755); err != nil {
		t.Fatal(err)
	}
	h.set("", "", false)
	if code, stdout, _, _ := h.run(); code != 0 || strings.Contains(stdout, "venv ran") {
		t.Fatalf("an unmarked venv counted as ready without the bootstrap: exit %d stdout %q", code, stdout)
	}
	h.set("", "", true)
	code, stdout, stderr, calls := h.run()
	if code != 0 || !strings.Contains(stdout, "venv ran") || countCalls(calls, "-m venv") != 1 || countCalls(calls, "-m pip install") != 1 {
		t.Fatalf("the unmarked venv was not built again once: exit %d stdout %q stderr %q calls %v", code, stdout, stderr, calls)
	}
	code, stdout, stderr, calls = h.run()
	if code != 0 || !strings.Contains(stdout, "venv ran") || countCalls(calls, "-m venv") != 0 || countCalls(calls, "-m pip") != 0 {
		t.Fatalf("the venv was built a second time: exit %d stdout %q stderr %q calls %v", code, stdout, stderr, calls)
	}
	// The marker is the only file the bootstrap adds beside the venv's own tree, and no temporary file of its write remains.
	entries, err := os.ReadDir(h.venv)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("venv directory holds %v, want bin and the marker only", names)
	}
}

// The marker is written whole by a rename, so a reader sees no marker or the finished one, and no temporary file stays behind; a
// marker that cannot be written leaves the attempt failed and cleaned up, not ready (CRW-1162).
func TestRepoMapMarkerIsWrittenAtomicallyAndAFailedWriteIsNotReady(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, repoMapVenvMarker)
	if err := writeRepoMapMarker(marker); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != repoMapVenvMarker {
		t.Fatalf("entries %v, err %v: want the marker alone", entries, err)
	}
	if data, err := os.ReadFile(marker); err != nil || len(data) == 0 {
		t.Fatalf("marker %q, err %v", data, err)
	}
	if err := writeRepoMapMarker(filepath.Join(dir, "missing", repoMapVenvMarker)); err == nil {
		t.Fatal("a marker in a directory that is not there was written")
	}

	// The order of the file operations is what makes the write atomic: the content is written and synced to a temporary file,
	// the file is closed, and only then renamed onto the marker. The marker does not exist before the rename, and a failed
	// rename leaves no marker and no temporary file.
	for _, failRename := range []bool{false, true} {
		var trace []string
		odir := t.TempDir()
		omarker := filepath.Join(odir, repoMapVenvMarker)
		ops := markerOps{
			createTemp: func(d, pattern string) (markerFile, error) {
				f, err := osMarkerOps.createTemp(d, pattern)
				if err != nil {
					return nil, err
				}
				trace = append(trace, "create")
				return &tracedMarkerFile{markerFile: f, trace: &trace, final: omarker}, nil
			},
			rename: func(oldpath, newpath string) error {
				trace = append(trace, "rename")
				if _, err := os.Stat(newpath); err == nil {
					t.Error("the marker existed before the rename")
				}
				if failRename {
					return os.ErrPermission
				}
				return os.Rename(oldpath, newpath)
			},
			remove: func(path string) error { trace = append(trace, "remove"); return os.Remove(path) },
		}
		err := writeRepoMapMarkerWith(omarker, ops)
		got := strings.Join(trace, ",")
		entries, rerr := os.ReadDir(odir)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if failRename {
			if err == nil || got != "create,write,sync,close,rename,remove" || len(entries) != 0 {
				t.Fatalf("failed rename: err %v trace %s entries %v: want an error, no marker and no temporary file", err, got, entries)
			}
		} else if err != nil || got != "create,write,sync,close,rename" || len(entries) != 1 || entries[0].Name() != repoMapVenvMarker {
			t.Fatalf("err %v trace %s entries %v: want create,write,sync,close,rename and the marker alone", err, got, entries)
		}
	}

	h := newFakeMapHost(t)
	env := mapEnv(map[string]string{"CRW_HOME": h.home})
	p, err := repoMapPaths(env)
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	present := map[string]bool{}
	d := mapDeps{
		exists: func(path string) bool { return present[path] },
		remove: func(path string) error { removed = append(removed, path); return nil },
		mark:   func(string) error { return os.ErrPermission },
		run: func(cmd string, args []string, quiet bool) (int, error) {
			if len(args) > 1 && args[1] == "venv" {
				present[p.python] = true
			}
			return 0, nil
		},
	}
	var stderr strings.Builder
	ok, exit := bootstrapRepoMapVenv(p, &stderr, d)
	if ok || exit != 0 || len(removed) != 1 || removed[0] != filepath.Dir(filepath.Dir(p.python)) {
		t.Fatalf("ok %v exit %d removed %v stderr %q: a venv whose marker could not be written was taken as ready", ok, exit, removed, stderr.String())
	}
}

// A marker whose interpreter is gone is stale: it is removed before the venv is built again, so a kill during that build cannot
// leave an interpreter that looks ready (CRW-1162).
func TestRepoMapStaleMarkerIsRemovedBeforeTheRebuild(t *testing.T) {
	h := newFakeMapHost(t)
	p, err := repoMapPaths(mapEnv(map[string]string{"CRW_HOME": h.home}))
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{p.marker: true, filepath.Dir(filepath.Dir(p.python)): true}
	var order []string
	d := mapDeps{
		exists: func(path string) bool { return present[path] },
		removeMarker: func(path string) error {
			order = append(order, "remove "+filepath.Base(path))
			delete(present, path)
			return nil
		},
		mark: func(path string) error { order = append(order, "mark"); present[path] = true; return nil },
		run: func(cmd string, args []string, quiet bool) (int, error) {
			order = append(order, "run "+args[1])
			present[p.python] = true
			return 0, nil
		},
	}
	if ok, exit := bootstrapRepoMapVenv(p, io.Discard, d); !ok || exit != 0 {
		t.Fatalf("ok %v exit %d order %v", ok, exit, order)
	}
	if got := strings.Join(order, ","); got != "remove "+repoMapVenvMarker+",run venv,run pip,mark" {
		t.Fatalf("order %s", got)
	}
}

// tracedMarkerFile records the order of the writes to the temporary file and checks the final marker is absent meanwhile.
type tracedMarkerFile struct {
	markerFile
	trace *[]string
	final string
}

func (f *tracedMarkerFile) note(op string) {
	*f.trace = append(*f.trace, op)
	if _, err := os.Stat(f.final); err == nil {
		panic("the marker existed while it was being written: " + op) // the write is not atomic
	}
}

func (f *tracedMarkerFile) WriteString(s string) (int, error) {
	f.note("write")
	return f.markerFile.WriteString(s)
}
func (f *tracedMarkerFile) Sync() error  { f.note("sync"); return f.markerFile.Sync() }
func (f *tracedMarkerFile) Close() error { f.note("close"); return f.markerFile.Close() }

// removeRepoMapMarker removes a file or a link, accepts an absent path, and refuses a directory (empty or not) and anything else
// that is not a marker file (CRW-1162).
func TestRemoveRepoMapMarkerIsNonRecursiveAndTypeChecked(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "full")
	if err := os.MkdirAll(filepath.Join(full, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, file, filepath.Join(dir, "absent")} {
		if err := removeRepoMapMarker(path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if _, err := os.Lstat(file); !os.IsNotExist(err) {
		t.Fatalf("the marker file was not removed: %v", err)
	}
	for _, path := range []string{full, empty} {
		if err := removeRepoMapMarker(path); err == nil {
			t.Fatalf("%s: a directory was removed or accepted", path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was deleted: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(full, "a")); err != nil {
		t.Fatalf("the directory contents were deleted: %v", err)
	}
}
