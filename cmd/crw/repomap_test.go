package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func mapEnv(values map[string]string) host.LookupEnv {
	return func(k string) (string, bool) { v, ok := values[k]; return v, ok }
}

func TestRepoMapLadderFromBin(t *testing.T) {
	p := mapPaths{script: "/skill/repomap.py", requirements: "/skill/requirements.txt", python: "/cache/python3", hasUv: true, hasVenv: true}
	for _, c := range []struct {
		name     string
		args     []string
		override string
		uv, venv bool
		cmd      string
		prefix   []string
	}{
		{"help", []string{"--help"}, "", true, true, "python3", []string{"-B", p.script}},
		{"short-help", []string{"-h"}, "", true, true, "python3", []string{"-B", p.script}},
		{"override", []string{"."}, "/override/python", true, true, "/override/python", []string{"-B", p.script}},
		{"help-override", []string{"--help"}, "/override/python", true, true, "/override/python", []string{"-B", p.script}},
		{"uv", []string{"."}, "", true, true, "uv", []string{"run", "--quiet", "--with-requirements", p.requirements, "python", "-B", p.script}},
		{"venv", []string{"."}, "", false, true, p.python, []string{"-B", p.script}},
		{"bare", []string{"."}, "", false, false, "python3", []string{"-B", p.script}},
		{"blank-uv", []string{"."}, " ", true, true, "uv", []string{"run", "--quiet", "--with-requirements", p.requirements, "python", "-B", p.script}},
		{"blank-bare", []string{"."}, " ", false, false, "python3", []string{"-B", p.script}},
		{"blank-venv", []string{"."}, " \t", false, true, p.python, []string{"-B", p.script}},
		{"padded-override", []string{"."}, " /override/python ", true, true, "/override/python", []string{"-B", p.script}},
		{"blank-help", []string{"--help"}, " ", true, true, "python3", []string{"-B", p.script}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p.hasUv, p.hasVenv = c.uv, c.venv
			got := selectRepoMapCommand(c.args, mapEnv(map[string]string{"CRW_PYTHON": c.override}), p)
			want := append(append([]string{}, c.prefix...), c.args...)
			if got.cmd != c.cmd || !reflect.DeepEqual(got.args, want) {
				t.Fatalf("%+v want %s %v", got, c.cmd, want)
			}
		})
	}
	env := mapEnv(map[string]string{"HOME": "/home-test", "CODEX_HOME": "/codex-test", "CRW_HOME": "/cache", "CRW_SKILLS_DIR": "/staged"})
	p, err := repoMapPaths(env)
	if err != nil || p.script != "/staged/crw-repo-map/scripts/repomap.py" || p.python != "/cache/venvs/repomap/bin/python3" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = repoMapPaths(mapEnv(map[string]string{"HOME": "/home-test"}))
	if err != nil || p.script != "/home-test/.codex/skills/crw-repo-map/scripts/repomap.py" || p.python != "/home-test/.crw/venvs/repomap/bin/python3" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = repoMapPaths(mapEnv(map[string]string{"HOME": "/home-test", "CODEX_HOME": "/codex-test", "CRW_HOME": "\ufeff", "CRW_SKILLS_DIR": " "}))
	if err != nil || p.script != "/codex-test/skills/crw-repo-map/scripts/repomap.py" || p.python != "/home-test/.crw/venvs/repomap/bin/python3" {
		t.Fatalf("%+v %v", p, err)
	}
}

type mapCall struct {
	cmd   string
	args  []string
	quiet bool
}

func TestRepoMapBootstrapWithFakesOnly(t *testing.T) {
	for _, c := range []struct {
		name           string
		help, existing bool
		override       string
		mk, pip, uv    int
		calls          int
		final          string
		removed        bool
		dirExisted     bool // the venv directory was there before this attempt
	}{
		{"success", false, false, "", 0, 0, 1, 4, "venv", false, false},
		{"mk-fails", false, false, "", 1, 0, 1, 3, "python3", false, false},
		{"pip-fails", false, false, "", 0, 1, 1, 4, "python3", true, false},
		// A tree that was there before this attempt is not this attempt's to delete (CRW-1147).
		{"pip-fails-preexisting-dir", false, false, "", 0, 1, 1, 4, "python3", false, true},
		{"uv-wins-after-bootstrap", false, false, "", 0, 0, 0, 4, "uv", false, false},
		// help answers before the bootstrap, the venv check and the uv probe (CRW-1147).
		{"help-skips-bootstrap", true, false, "", 0, 0, 1, 1, "python3", false, false},
		{"help-with-override", true, false, "/custom/python", 0, 0, 1, 1, "/custom/python", false, false},
		{"existing", false, true, "", 0, 0, 1, 2, "venv", false, false},
		// An explicit interpreter needs no uv probe (CRW-1147).
		{"override", false, false, "/custom/python", 0, 0, 1, 1, "/custom/python", false, false},
		// A blank override is unset: it neither runs as a command nor suppresses the bootstrap (CRW-1147).
		{"blank-override-is-unset", false, false, " ", 0, 0, 1, 4, "venv", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			env := mapEnv(map[string]string{"HOME": root, "CODEX_HOME": filepath.Join(root, "codex"), "CRW_HOME": root, "CRW_MAP_BOOTSTRAP": "1", "CRW_PYTHON": c.override})
			p, err := repoMapPaths(env)
			if err != nil {
				t.Fatal(err)
			}
			var calls []mapCall
			var removed string
			var stderr strings.Builder
			d := mapDeps{exists: func(path string) bool {
				if path == filepath.Join(root, "venvs", "repomap") {
					return c.dirExisted
				}
				if path != p.python {
					t.Fatal(path)
				}
				return c.existing
			}, remove: func(path string) error { removed = path; return nil }, run: func(cmd string, args []string, quiet bool) (int, error) {
				calls = append(calls, mapCall{cmd, append([]string{}, args...), quiet})
				if reflect.DeepEqual(args, []string{"-m", "venv", root + "/venvs/repomap"}) {
					return c.mk, nil
				}
				if len(args) > 1 && args[0] == "-m" && args[1] == "pip" {
					if cmd != p.python || !reflect.DeepEqual(args, []string{"-m", "pip", "install", "-q", "-r", p.requirements}) {
						t.Fatalf("pip %s %v", cmd, args)
					}
					return c.pip, nil
				}
				if cmd == "uv" && reflect.DeepEqual(args, []string{"--version"}) {
					if !quiet {
						t.Fatal("uv probe inherits streams")
					}
					return c.uv, nil
				}
				return 0, nil
			}}
			args := []string{".", "--tokens", "200"}
			if c.help {
				args = []string{"--help"}
			}
			if launchRepoMap(args, env, &stderr, d) != 0 || len(calls) != c.calls {
				t.Fatalf("calls %+v stderr %q", calls, stderr.String())
			}
			last := calls[len(calls)-1]
			final := c.final
			if final == "venv" {
				final = p.python
			}
			if last.cmd != final || last.quiet {
				t.Fatalf("last %+v want %s", last, final)
			}
			want := append([]string{"-B", p.script}, args...)
			if final == "uv" {
				want = append([]string{"run", "--quiet", "--with-requirements", p.requirements, "python", "-B", p.script}, args...)
			}
			if !reflect.DeepEqual(last.args, want) {
				t.Fatalf("argv %v want %v", last.args, want)
			}
			if (removed != "") != c.removed || c.removed && removed != root+"/venvs/repomap" {
				t.Fatal("cleanup", removed)
			}
			if strings.Contains(stderr.String(), "venv bootstrap failed") != (c.pip != 0 && !c.help && c.calls > 1) {
				t.Fatal(stderr.String())
			}
			for _, call := range calls {
				if call.cmd == "uv" && (c.help || c.override != "" && strings.TrimSpace(c.override) != "") {
					t.Fatalf("uv probed for a run it cannot serve: %+v", calls)
				}
			}
		})
	}
}

func TestRepoMapFinalExitAndSpawnErrors(t *testing.T) {
	for _, c := range []struct {
		code int
		err  error
		want int
		hint bool
	}{
		{0, nil, 0, false}, {3, nil, 3, false}, {127, nil, 1, true}, {9009, nil, 1, true},
		{-1, os.ErrNotExist, 1, true}, {-1, errors.New("EACCES"), 1, false}, {-1, nil, 1, false},
	} {
		var stderr strings.Builder
		env := mapEnv(map[string]string{"HOME": t.TempDir()})
		d := mapDeps{exists: func(string) bool { return false }, run: func(cmd string, args []string, quiet bool) (int, error) {
			if quiet {
				return 1, nil
			}
			return c.code, c.err
		}, remove: func(string) error { t.Fatal("unexpected bootstrap"); return nil }}
		got := launchRepoMap([]string{"a b", "; no shell", "--flag=\"quoted\""}, env, &stderr, d)
		if got != c.want || (stderr.Len() != 0) != c.hint {
			t.Fatalf("%+v got %d %q", c, got, stderr.String())
		}
	}
}

func TestRepoMapNoBootstrapUnlessExactOptIn(t *testing.T) {
	for _, flag := range []string{"", "0", "true", " 1"} {
		var stderr strings.Builder
		calls := 0
		env := mapEnv(map[string]string{"HOME": t.TempDir(), "CRW_MAP_BOOTSTRAP": flag})
		d := mapDeps{exists: func(string) bool { return false }, run: func(_ string, args []string, quiet bool) (int, error) {
			calls++
			if len(args) > 0 && args[0] == "-m" {
				t.Fatal("bootstrap")
			}
			return 0, nil
		}}
		if launchRepoMap(nil, env, &stderr, d) != 0 || calls != 2 || stderr.Len() != 0 {
			t.Fatalf("opt-in %q calls %d %q", flag, calls, stderr.String())
		}
	}
}

// A plugin installation keeps the repo-map skill under the Codex home's plugin cache and creates no
// standalone skill link: crw map runs that script instead of a nonexistent <CODEX_HOME>/skills one
// (CRW-392), and an explicit CRW_SKILLS_DIR or an existing link still wins.
func TestRepoMapFindsThePluginInstalledSkill(t *testing.T) {
	write := func(t *testing.T, dir string, age time.Duration) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(dir, "repomap.py")
		if err := os.WriteFile(script, []byte("print()\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(script, when, when); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	launch := func(t *testing.T, env map[string]string) string {
		t.Helper()
		var script string
		d := mapDeps{
			exists:         func(string) bool { return false },
			remove:         func(string) error { return nil },
			installedSkill: installedRepoMapDir,
			run: func(cmd string, args []string, quiet bool) (int, error) {
				for i, a := range args {
					if a == "-B" && i+1 < len(args) {
						script = args[i+1]
					}
				}
				return 0, nil
			},
		}
		if code := launchRepoMap([]string{"--help"}, mapEnv(env), &strings.Builder{}, d); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return script
	}
	t.Run("plugin cache", func(t *testing.T) {
		home := t.TempDir()
		codex := filepath.Join(home, "codex")
		old := write(t, filepath.Join(codex, "plugins", "cache", "market", "crw", "0.9.0", "skills", "crw-repo-map", "scripts"), 48*time.Hour)
		cur := write(t, filepath.Join(codex, "plugins", "cache", "market", "crw", "0.10.0", "skills", "crw-repo-map", "scripts"), time.Hour)
		_ = old
		if got := launch(t, map[string]string{"HOME": home, "CODEX_HOME": codex}); got != filepath.Join(cur, "repomap.py") {
			t.Fatalf("script %q, want the newest installed %q", got, filepath.Join(cur, "repomap.py"))
		}
	})
	t.Run("plugin root from the host", func(t *testing.T) {
		home := t.TempDir()
		root := filepath.Join(home, "pkg")
		dir := write(t, filepath.Join(root, "skills", "crw-repo-map", "scripts"), time.Hour)
		if got := launch(t, map[string]string{"HOME": home, "CODEX_HOME": filepath.Join(home, "codex"), "PLUGIN_ROOT": root}); got != filepath.Join(dir, "repomap.py") {
			t.Fatalf("script %q", got)
		}
	})
	t.Run("plugin root beats a newer cached older version", func(t *testing.T) {
		home := t.TempDir()
		codex := filepath.Join(home, "codex")
		root := filepath.Join(home, "pkg")
		active := write(t, filepath.Join(root, "skills", "crw-repo-map", "scripts"), 48*time.Hour)
		write(t, filepath.Join(codex, "plugins", "cache", "market", "crw", "0.9.0", "skills", "crw-repo-map", "scripts"), time.Minute)
		if got := launch(t, map[string]string{"HOME": home, "CODEX_HOME": codex, "PLUGIN_ROOT": root}); got != filepath.Join(active, "repomap.py") {
			t.Fatalf("script %q, want the active plugin's %q", got, filepath.Join(active, "repomap.py"))
		}
	})
	t.Run("an unusable plugin root falls back to the cache", func(t *testing.T) {
		home := t.TempDir()
		codex := filepath.Join(home, "codex")
		root := filepath.Join(home, "pkg")
		if err := os.MkdirAll(filepath.Join(root, "skills", "crw-repo-map", "scripts", "repomap.py"), 0o755); err != nil {
			t.Fatal(err)
		}
		cached := write(t, filepath.Join(codex, "plugins", "cache", "market", "crw", "1.0.0", "skills", "crw-repo-map", "scripts"), time.Hour)
		if got := launch(t, map[string]string{"HOME": home, "CODEX_HOME": codex, "PLUGIN_ROOT": root}); got != filepath.Join(cached, "repomap.py") {
			t.Fatalf("script %q, want the cached %q", got, filepath.Join(cached, "repomap.py"))
		}
	})
	t.Run("a skills link wins", func(t *testing.T) {
		home := t.TempDir()
		codex := filepath.Join(home, "codex")
		link := write(t, filepath.Join(codex, "skills", "crw-repo-map", "scripts"), time.Hour)
		write(t, filepath.Join(codex, "plugins", "cache", "market", "crw", "1.0.0", "skills", "crw-repo-map", "scripts"), time.Minute)
		if got := launch(t, map[string]string{"HOME": home, "CODEX_HOME": codex}); got != filepath.Join(link, "repomap.py") {
			t.Fatalf("script %q", got)
		}
	})
	t.Run("an explicit skills dir wins", func(t *testing.T) {
		home := t.TempDir()
		codex := filepath.Join(home, "codex")
		write(t, filepath.Join(codex, "plugins", "cache", "market", "crw", "1.0.0", "skills", "crw-repo-map", "scripts"), time.Minute)
		if got := launch(t, map[string]string{"HOME": home, "CODEX_HOME": codex, "CRW_SKILLS_DIR": "/staged"}); got != "/staged/crw-repo-map/scripts/repomap.py" {
			t.Fatalf("script %q", got)
		}
	})
	t.Run("nothing installed keeps the default", func(t *testing.T) {
		home := t.TempDir()
		codex := filepath.Join(home, "codex")
		if got := launch(t, map[string]string{"HOME": home, "CODEX_HOME": codex}); got != filepath.Join(codex, "skills", "crw-repo-map", "scripts", "repomap.py") {
			t.Fatalf("script %q", got)
		}
	})
}

// fakeMapFS is a venv directory the fakes build, so two bootstraps can see one another's work.
type fakeMapFS struct {
	mu      sync.Mutex
	present map[string]bool
	removed []string
}

func (f *fakeMapFS) has(p string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.present[p] }
func (f *fakeMapFS) set(p string)      { f.mu.Lock(); defer f.mu.Unlock(); f.present[p] = true }
func (f *fakeMapFS) remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, p)
	delete(f.present, p)
	return nil
}

type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuilder) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// Two bootstraps of one crw home run one after the other: the second waits, finds the first's venv and neither builds nor
// deletes anything, where a failed pip of the second used to remove the tree the first was still using (CRW-1147).
func TestRepoMapBootstrapsAreSerializedAndLeaveTheOthersTree(t *testing.T) {
	root := t.TempDir()
	env := mapEnv(map[string]string{"HOME": root, "CODEX_HOME": filepath.Join(root, "codex"), "CRW_HOME": root, "CRW_MAP_BOOTSTRAP": "1"})
	p, err := repoMapPaths(env)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "venvs", "repomap")
	fs := &fakeMapFS{present: map[string]bool{}}
	pipStarted, pipRelease := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var venvRuns, pipRuns []string
	mk := func(name string, pipFails bool, stderr io.Writer) mapDeps {
		return mapDeps{
			exists: fs.has, remove: fs.remove,
			lock: func(venvs string) (func(), error) {
				return repoMapBootstrapLock(context.Background(), venvs, stderr)
			},
			run: func(cmd string, args []string, quiet bool) (int, error) {
				switch {
				case len(args) >= 2 && args[0] == "-m" && args[1] == "venv":
					mu.Lock()
					venvRuns = append(venvRuns, name)
					mu.Unlock()
					fs.set(dir)
				case len(args) >= 2 && args[0] == "-m" && args[1] == "pip":
					mu.Lock()
					pipRuns = append(pipRuns, name)
					mu.Unlock()
					if name == "A" {
						close(pipStarted)
						<-pipRelease
					}
					if pipFails {
						return 1, nil
					}
					fs.set(p.python)
				}
				return 0, nil
			},
		}
	}
	oldPoll := repoMapLockPoll
	repoMapLockPoll = 5 * time.Millisecond
	t.Cleanup(func() { repoMapLockPoll = oldPoll })
	var aErr, bErr lockedBuilder
	done := make(chan int, 2)
	go func() { done <- launchRepoMap([]string{"."}, env, &aErr, mk("A", false, &aErr)) }()
	<-pipStarted
	// B would fail its pip, and with no serialization it would delete A's tree.
	go func() { done <- launchRepoMap([]string{"."}, env, &bErr, mk("B", true, &bErr)) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(bErr.String(), "waiting for another venv bootstrap") {
		if time.Now().After(deadline) {
			t.Fatalf("B never waited for A: %q", bErr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(pipRelease)
	for range 2 {
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("exit %d, A %q B %q", code, aErr.String(), bErr.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("bootstraps did not finish")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(venvRuns, []string{"A"}) || !reflect.DeepEqual(pipRuns, []string{"A"}) || len(fs.removed) != 0 || !fs.has(p.python) || !fs.has(dir) {
		t.Fatalf("venv runs %v pip runs %v removed %v: B did not leave A's venv alone", venvRuns, pipRuns, fs.removed)
	}
}

// The lock cannot be taken: the bootstrap is skipped (nothing built, nothing removed) and the run falls back.
func TestRepoMapBootstrapSkippedWhenTheLockCannotBeTaken(t *testing.T) {
	root := t.TempDir()
	env := mapEnv(map[string]string{"HOME": root, "CODEX_HOME": filepath.Join(root, "codex"), "CRW_HOME": root, "CRW_MAP_BOOTSTRAP": "1"})
	var calls []string
	var stderr strings.Builder
	d := mapDeps{exists: func(string) bool { return false }, remove: func(string) error { t.Fatal("removed"); return nil },
		lock: func(string) (func(), error) { return nil, errors.New("lock unavailable") },
		run: func(cmd string, args []string, quiet bool) (int, error) {
			calls = append(calls, cmd+" "+strings.Join(args, " "))
			return 1, nil
		}}
	launchRepoMap([]string{"."}, env, &stderr, d)
	for _, c := range calls {
		if strings.Contains(c, "-m venv") || strings.Contains(c, "-m pip") {
			t.Fatalf("bootstrap ran without the lock: %v", calls)
		}
	}
	if !strings.Contains(stderr.String(), "venv bootstrap skipped: lock unavailable") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

// The uv probe is bounded: a uv that does not answer is a tool that is not there, and the run goes on to python3 (CRW-1147).
func TestRepoMapUvProbeIsBounded(t *testing.T) {
	bin := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("uv", "exec /bin/sleep 30")
	write("python3", `echo "ran $*"`)
	root := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CRW_HOME", root)
	t.Setenv("CRW_MAP_BOOTSTRAP", "")
	t.Setenv("CRW_PYTHON", "")
	t.Setenv("CRW_SKILLS_DIR", "")
	old := repoMapProbeTimeout
	repoMapProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { repoMapProbeTimeout = old })
	var out, errOut strings.Builder
	started := time.Now()
	code := runRepoMap(invocation{ctx: context.Background(), args: []string{"."}, stdout: &out, stderr: &errOut})
	if code != 0 || !strings.Contains(out.String(), "ran -B") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errOut.String())
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("the run waited %v on a uv that never answers", took)
	}
}
