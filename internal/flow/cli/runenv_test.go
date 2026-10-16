package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// envOf is a test environment: the variables of m, the others unset.
func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// valueOf is a lookup as the one temp function reads it (a value, "" when unset).
func valueOf(env func(string) (string, bool)) func(string) string {
	return func(k string) string {
		v, _ := env(k)
		return v
	}
}

// runSpecs are the run.json files below a state directory.
func runSpecs(t *testing.T, state string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(RunDir(state, "*"), "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestStartRecordsTheOneFunctionsAnswer (CRW-1570 D1): `crw flow start` records in the run's spec the temporary location the one
// temp function (tmploc.DirFrom) answers for the starting environment, as written, and no other variable of that environment; the
// built-in land-head workflow records it the same way.
func TestStartRecordsTheOneFunctionsAnswer(t *testing.T) {
	isolateGit(t)
	start := func(t *testing.T, td *testDeps, w *world) Spec {
		t.Helper()
		code, out, errOut := td.do("start", helloScript(t), "--backend", "app-server", "--thread", "thread-owner", "--state", w.state)
		if code != 0 || len(td.spawned) != 1 {
			t.Fatalf("start: exit %d\n%s\n%s", code, out, errOut)
		}
		return td.spawned[0].Spec
	}
	check := func(t *testing.T, w *world, got Spec, env func(string) (string, bool)) {
		t.Helper()
		want, _, err := tmploc.DirFrom(valueOf(env))
		if err != nil {
			t.Fatalf("the one function refuses the environment: %v", err)
		}
		if len(got.Env) != 1 || got.Env["TMPDIR"] != want {
			t.Errorf("the spawned spec records env %v, want exactly TMPDIR=%s", got.Env, want)
		}
		stored, err := ReadSpec(w.state, got.RunID)
		if err != nil || len(stored.Env) != 1 || stored.Env["TMPDIR"] != want {
			t.Errorf("the stored spec records env %v (%v), want exactly TMPDIR=%s", stored.Env, err, want)
		}
		raw, err := os.ReadFile(specPath(w.state, got.RunID))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"PATH", "GOFLAGS", "GOTOOLCHAIN", "HOME"} {
			if strings.Contains(string(raw), `"`+name+`"`) {
				t.Errorf("run.json records %s:\n%s", name, raw)
			}
		}
	}

	t.Run("TMPDIR set", func(t *testing.T) {
		w := newWorld(t)
		td := w.deps()
		env := envOf(map[string]string{"TMPDIR": t.TempDir(), "PATH": "/usr/bin:/bin", "GOFLAGS": "-p=4", "GOTOOLCHAIN": "local", "HOME": t.TempDir()})
		td.Getenv = env
		check(t, w, start(t, td, w), env)
	})

	t.Run("TMPDIR unset", func(t *testing.T) {
		w := newWorld(t)
		td := w.deps()
		env := envOf(map[string]string{"PATH": "/usr/bin:/bin", "GOFLAGS": "-p=4", "GOTOOLCHAIN": "local"})
		td.Getenv = env
		dir, _, err := tmploc.DirFrom(valueOf(env))
		if err == nil {
			_, err = tmploc.CheckDir(dir)
		}
		if err != nil {
			// whatever default the one function lands with, a start refuses what it refuses
			code, out, errOut := td.do("start", helloScript(t), "--backend", "app-server", "--thread", "thread-owner", "--state", w.state)
			if code != 1 || len(td.spawned) != 0 || !strings.Contains(errOut, err.Error()) {
				t.Fatalf("start without TMPDIR whose default is unusable (%v): exit %d\n%s\n%s", err, code, out, errOut)
			}
			return
		}
		check(t, w, start(t, td, w), env)
	})

	t.Run("land-head", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		env := envOf(map[string]string{"TMPDIR": t.TempDir(), "PATH": "/usr/bin:/bin", "GOFLAGS": "-p=4", "GOTOOLCHAIN": "local"})
		l.td.Getenv = env
		check(t, l.w, l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file), env)
	})
}

// TestStartRefusesAnUnusableTempLocation (CRW-1570 D1): a TMPDIR the one function refuses, or a directory that cannot be used, stops
// the start with the reason before the run's spec is written and before any process is started.
func TestStartRefusesAnUnusableTempLocation(t *testing.T) {
	isolateGit(t)
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, tmpdir string
		want         []string
	}{
		{"relative", "rel/tmp", []string{"tmpdir_not_absolute", "rel/tmp"}},
		{"missing", filepath.Join(t.TempDir(), "gone"), []string{"temp_dir_unusable", "gone", "does not exist"}},
		{"a file", file, []string{"temp_dir_unusable", "a-file", "is not a directory"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			td := w.deps()
			td.Getenv = envOf(map[string]string{"TMPDIR": c.tmpdir})
			code, out, errOut := td.do("start", helloScript(t), "--backend", "app-server", "--thread", "thread-owner", "--state", w.state)
			if code != 1 {
				t.Fatalf("start with TMPDIR=%s: exit %d, want 1\n%s\n%s", c.tmpdir, code, out, errOut)
			}
			for _, want := range c.want {
				if !strings.Contains(errOut, want) {
					t.Errorf("the refusal lacks %q: %s", want, errOut)
				}
			}
			if len(td.spawned) != 0 {
				t.Errorf("a process was started: %+v", td.spawned)
			}
			if specs := runSpecs(t, w.state); len(specs) != 0 {
				t.Errorf("a run spec was written: %v", specs)
			}
		})
	}
}

// TestCheckRunEnv (CRW-1570): a spec without env passes as before; a spec whose recorded TMPDIR is usable passes; one whose recorded
// directory was removed is refused naming the directory, why, and what to do.
func TestCheckRunEnv(t *testing.T) {
	if err := checkRunEnv(Spec{}); err != nil {
		t.Errorf("a spec without env: %v", err)
	}
	dir := t.TempDir()
	if err := checkRunEnv(Spec{Env: map[string]string{"TMPDIR": dir}}); err != nil {
		t.Errorf("a usable recorded TMPDIR: %v", err)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	err := checkRunEnv(Spec{Env: map[string]string{"TMPDIR": gone}})
	if err == nil {
		t.Fatal("a removed recorded TMPDIR passes")
	}
	for _, want := range []string{"the run's recorded TMPDIR " + gone + " is not usable", "does not exist", "make the directory again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	file := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(file, nil, 0o600)
	if err := checkRunEnv(Spec{Env: map[string]string{"TMPDIR": file}}); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("a recorded TMPDIR that is a file: %v", err)
	}
	if err := checkRunEnv(Spec{Env: map[string]string{"TMPDIR": "rel"}}); err == nil || !strings.Contains(err.Error(), "rel") {
		t.Errorf("a relative recorded TMPDIR: %v", err)
	}
}

// TestReadSpecOfASpecWithoutEnv (CRW-1570): a spec written before env (crw-flow-run/1 without the member) reads as before, with Env
// nil, loads its workflow and passes the run env check; a spec with no env writes no env member.
func TestReadSpecOfASpecWithoutEnv(t *testing.T) {
	old, err := os.ReadFile(filepath.Join("testdata", "run-spec-without-env.json"))
	if err != nil {
		t.Fatal(err)
	}
	state, run := t.TempDir(), "run-0123456789e0"
	if err := os.MkdirAll(RunDir(state, run), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath(state, run), old, 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := ReadSpec(state, run)
	if err != nil {
		t.Fatalf("the old spec does not read: %v", err)
	}
	if spec.Env != nil || spec.Workflow != "land-head" || spec.Thread != "thread-owner" || spec.Backend != BackendAppServer {
		t.Errorf("the old spec reads as %+v", spec)
	}
	if _, err := spec.Load(); err != nil {
		t.Errorf("the old spec's workflow does not load: %v", err)
	}
	if err := checkRunEnv(spec); err != nil {
		t.Errorf("the old spec is refused: %v", err)
	}

	again := spec
	again.RunID = "run-0123456789e1"
	if err := WriteSpec(state, again); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(specPath(state, again.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"env"`) {
		t.Errorf("a spec without env writes an env member:\n%s", raw)
	}
	if _, err := ReadSpec(state, again.RunID); err != nil {
		t.Errorf("the rewritten spec: %v", err)
	}
}
