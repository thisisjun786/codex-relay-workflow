package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1348 with CRW-1331: a session run compares only the decided parts of a role's pair. A role of the helper role store (CRW-1330)
// may name a model and leave its effort undecided, or leave its whole pair to the host; the owner's binding carries such a role with
// its empty parts, the role_models fact says which part is undecided, and the run's environment is built whatever the explorer's and
// the reviewer's pairs are, because a session run starts no App Server thread whose binding needs them.

// undecidedRoleStore leaves the explorer to the host (mode default) and the reviewer's effort undecided.
const undecidedRoleStore = `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "medium"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol"},
  "explorer": {"mode": "default"}}}`

func undecidedStore(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "crw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, role.StoreFile), []byte(undecidedRoleStore), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_HOME", dir)
	t.Setenv(execution.EnvPolicy, "")
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST", "")
}

// effortUndecidedTable is the fake table with executor-t1's effort undecided and the explorer left to the host.
func effortUndecidedTable() *engine.Policy {
	table := engine.FakePolicy()
	t1 := table.Roles["executor-t1"]
	t1.Pair.Effort = ""
	table.Roles["executor-t1"] = t1
	ex := table.Roles["explorer"]
	ex.Pair = engine.Pair{}
	table.Roles["explorer"] = ex
	return table
}

func TestBindOwnerCarriesTheDecidedPartsOfEachRole(t *testing.T) {
	isolateGit(t)
	w := newWorld(t)
	table := effortUndecidedTable()
	res, err := bindOwner(w.state, "run-0123456789ab", sessOwner, table)
	if err != nil || !res.Bound {
		t.Fatalf("bind: %+v %v", res, err)
	}
	got := bound.Lookup(sessOwner)
	if got.State != bound.Orchestrator || len(got.Line.HelperPairs) != 6 ||
		got.Line.HelperPairs["executor-t1"] != (bound.Pair{Model: "claude-fake-sonnet"}) || got.Line.HelperPairs["explorer"] != (bound.Pair{}) ||
		got.Line.HelperPairs["executor"] != (bound.Pair{Model: "claude-fake-haiku", Effort: "xhigh"}) {
		t.Fatalf("the owner's binding: %+v", got)
	}
}

func TestRoleModelsFactNamesTheUndecidedPart(t *testing.T) {
	isolateGit(t)
	w := newWorld(t)
	facts := sessionFacts(sessionFactRequest{State: w.state, Thread: sessOwner, Table: effortUndecidedTable(), Getenv: func(string) string { return "" }})
	var fact Fact
	for _, f := range facts {
		if f.Name == FactRoleModels {
			fact = f
		}
	}
	if fact.Level != LevelWarn || !strings.Contains(fact.Reading, "no pair for explorer") || !strings.Contains(fact.Reading, "no effort for executor-t1") ||
		!strings.Contains(fact.Reading, "executor-t1 claude-fake-sonnet") || strings.Contains(fact.Reading, "no pair for executor-t1") {
		t.Fatalf("role_models %+v", fact)
	}
}

// The world of a session run's process (a start and a resume) and the foreground resume's environment are built on a table whose
// explorer is undecided and whose reviewer's effort is undecided; a run on the App Server still needs both pairs for its binding.
func TestASessionRunIsBuiltOnUndecidedHelperPairs(t *testing.T) {
	isolateGit(t)
	undecidedStore(t)
	w := newWorld(t)
	r := w.newRun("run-sess-undecided", sessOwner)
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "hello", "backend": engine.BackendSession}})
	if err := r.j.Close(); err != nil {
		t.Fatal(err)
	}
	w.close()
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	socket := filepath.Join(t.TempDir(), "app.sock")
	for _, resume := range []bool{false, true} {
		spec := Spec{Schema: SpecSchema, RunID: "run-sess-undecided", Workflow: "hello", Thread: sessOwner, Repo: repo, Socket: socket, Backend: BackendSession}
		world, err := ProductionWorld(context.Background(), WorldRequest{Spec: spec, State: w.state, Resume: resume})
		if err != nil {
			t.Fatalf("the world of a session run (resume %v): %v", resume, err)
		}
		session := world.Session
		if err := world.Close(); err != nil {
			t.Fatal(err)
		}
		if session == nil {
			t.Fatalf("resume %v: the world has no session configuration", resume)
		}
	}
	setup, err := journal.ResumeEnv(context.Background(), engine.ResumeEnvRequest{RunID: "run-sess-undecided", State: w.state, Socket: socket, Repo: repo})
	if err != nil {
		t.Fatalf("the foreground resume's environment of a session run: %v", err)
	}
	session := setup.Session
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("the resume's environment has no session configuration")
	}
	// a run on the App Server binds every engine turn with the explorer pair: it is refused on this table, as before
	spec := Spec{Schema: SpecSchema, RunID: "run-app-undecided", Workflow: "hello", Thread: sessOwner, Repo: repo, Socket: socket, Backend: BackendAppServer}
	if world, err := ProductionWorld(context.Background(), WorldRequest{Spec: spec, State: w.state}); err == nil {
		_ = world.Close()
		t.Fatal("an App Server run was built on a table without an explorer pair")
	} else if !strings.Contains(err.Error(), "explorer pair") {
		t.Fatalf("an App Server run on an undecided explorer: %v", err)
	}
}
