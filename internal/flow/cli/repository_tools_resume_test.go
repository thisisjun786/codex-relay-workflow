package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1488 r1: `crw flow resume RUN SCRIPT` runs the engine's resume, whose Config is built in the engine's command; it loads the same
// repository tools as the run process (through engine.ResumeHold), so a call the resumed run starts gets the go of verifier.go_root.
func TestScriptResumeGivesItsCallsTheRepositoryTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config := filepath.Join(home, "config.json")
	root := filepath.Join(home, "go")
	if err := os.WriteFile(config, []byte(`{"verifier":{"go_root":"`+root+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := scriptResumeCallTools(t, home, config, nil)
	want := []string{"PATH=" + filepath.Join(root, "bin") + ":/usr/bin", "GOROOT=" + root, "GOTOOLCHAIN=local"}
	if got == nil || !slices.Equal(got.Env, want) || len(got.Facts) != 1 {
		t.Errorf("the resumed run's call was started with tools %+v, want env %q", got, want)
	}
}

// CRW-1570: the resumed run's calls get the run's recorded TMPDIR (env.TMPDIR of its spec) first among their tools, not the TMPDIR
// of the shell that resumes it, as the run process's calls do.
func TestScriptResumeGivesItsCallsTheRunsTMPDIR(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config := filepath.Join(home, "config.json")
	root := filepath.Join(home, "go")
	if err := os.WriteFile(config, []byte(`{"verifier":{"go_root":"`+root+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := t.TempDir()
	got := scriptResumeCallTools(t, home, config, map[string]string{"TMPDIR": a})
	want := []string{"TMPDIR=" + a, "PATH=" + filepath.Join(root, "bin") + ":/usr/bin", "GOROOT=" + root, "GOTOOLCHAIN=local"}
	if got == nil || !slices.Equal(got.Env, want) || !strings.HasPrefix(got.Prefix, "TMPDIR="+shellWord(a)+" ") || len(got.Facts) != 2 {
		t.Errorf("the resumed run's call was started with tools %+v, want env %q and the TMPDIR first in the prefix", got, want)
	}
}

// scriptResumeCallTools resumes a paused App Server run whose spec records env with `crw flow resume RUN SCRIPT`, from a process
// whose TMPDIR is the world's, and returns the tools its one call's thread was started with.
func scriptResumeCallTools(t *testing.T, home, config string, env map[string]string) *engine.RepositoryTools {
	t.Helper()
	w := newWorld(t)
	td := w.deps()
	td.Getenv = td.w.withTemp(func(k string) (string, bool) {
		v, ok := map[string]string{"HOME": home, "CRW_CONFIG": config, "PATH": "/usr/bin"}[k]
		return v, ok
	})
	run, owner := "run-0123456789a1", "01a30000-0000-7000-8000-0000000000a1"
	script := filepath.Join(t.TempDir(), "look.js")
	if err := os.WriteFile(script, []byte("export const meta = { name: 'look' }\nconst a = await agent.call('look around', { agentType: 'explorer', label: 'exp' })\nreturn [a.status]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Schema: SpecSchema, RunID: run, Workflow: "look", Script: script, Thread: owner, Backend: BackendAppServer, CreatedAt: "2026-10-14T00:00:00Z", Env: env}
	if err := WriteSpec(w.state, spec); err != nil {
		t.Fatal(err)
	}
	src, err := spec.Load()
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(ctx, w.st, run, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: owner, EpochNonce: "n"}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "look", "script_digest": src.Program.Digest,
		"args_digest": argsDigest([]byte("null")), "backend": engine.BackendAppServer}})
	j.Append(engine.Event{RunID: run, Kind: engine.KindFlow, Event: engine.EvRunPaused, Data: map[string]any{"by": "operator", "reason": "crw flow pause"}})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	fake := engine.NewFake()
	var mu sync.Mutex
	var specs []engine.ThreadSpec
	fake.StartHook = func(s engine.ThreadSpec) (engine.ThreadInfo, error) {
		mu.Lock()
		specs = append(specs, s)
		mu.Unlock()
		return engine.ThreadInfo{ID: "thr_" + s.CallID, Pair: s.Pair}, nil
	}
	prev := engine.ResumeEnv
	t.Cleanup(func() { engine.ResumeEnv = prev })
	engine.ResumeEnv = func(ctx context.Context, req engine.ResumeEnvRequest) (engine.ResumeSetup, error) {
		st, err := store.Open(ctx, filepath.Join(req.State, "relay.sqlite3"), "")
		if err != nil {
			return engine.ResumeSetup{}, err
		}
		rj, err := journal.Open(ctx, st, req.RunID, journal.Identity{}, journal.Options{})
		if err != nil {
			_ = st.Close()
			return engine.ResumeSetup{}, err
		}
		return engine.ResumeSetup{Journal: rj, Backend: fake, Close: func() error { return errJoin(rj.Close(), st.Close()) }}, nil
	}
	code, out, errOut := td.do("resume", run, script, "--state", w.state)
	if code != 0 {
		t.Fatalf("the resume: exit %d\n%s\n%s", code, out, errOut)
	}
	if len(specs) != 1 {
		t.Fatalf("%d threads started, want 1", len(specs))
	}
	return specs[0].Tools
}
