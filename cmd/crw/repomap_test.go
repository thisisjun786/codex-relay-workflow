package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		{"blank-bare", []string{"."}, " ", false, false, " ", []string{"-B", p.script}},
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
	}{
		{"success", false, false, "", 0, 0, 1, 4, "venv", false},
		{"mk-fails", false, false, "", 1, 0, 1, 3, "python3", false},
		{"pip-fails", false, false, "", 0, 1, 1, 4, "python3", true},
		{"uv-wins-after-bootstrap", false, false, "", 0, 0, 0, 4, "uv", false},
		{"help-still-bootstraps", true, false, "", 0, 0, 1, 4, "python3", false},
		{"existing", false, true, "", 0, 0, 1, 2, "venv", false},
		{"override", false, false, "/custom/python", 0, 0, 1, 2, "/custom/python", false},
		{"blank-suppresses-bootstrap", false, false, " ", 0, 0, 1, 2, " ", false},
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
			if strings.Contains(stderr.String(), "venv bootstrap failed") != c.removed {
				t.Fatal(stderr.String())
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
