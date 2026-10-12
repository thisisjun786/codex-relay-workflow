package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/callstate"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// crw runs the built binary and returns its exit code and output.
func crw(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(testsupport.CRW(t), args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

// guided is a stored run whose script is a file, with one call of node n1 that the first process left at a dispatch intent. The
// run record carries the digests of the script and its arguments, as the engine writes them, so `crw flow resume` accepts it.
type guided struct {
	*holdFixture
	repo, script string
}

func newGuided(t *testing.T, unknown bool) *guided {
	t.Helper()
	// the id the engine gives the script's first call: its run, path and position
	idSum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", runID, "root", 1)))
	g := &guided{holdFixture: &holdFixture{host: fakehost.Start(t), cwd: "/w/attempt-1", base: time.Unix(1_780_000_000, 0), callID: "c_" + hex.EncodeToString(idSum[:])[:12]}}
	dir := t.TempDir()
	g.script = filepath.Join(dir, "w.js")
	text := header + "const a = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'a' })\nreturn a.status\n"
	if err := os.WriteFile(g.script, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	prog, err := script.Load(script.Source{Name: "w.js", Text: []byte(text)})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("null"))
	g.repo = filepath.Join(dir, "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", g.repo}, {"-C", g.repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "base"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	g.state = t.TempDir()
	g.db = filepath.Join(g.state, "relay.sqlite3")
	st := openStore(t, g.db)
	j, err := Open(ctx, st, runID, identity, Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "t", "script_digest": prog.Digest, "args_digest": hex.EncodeToString(sum[:])}})
	row := func(event string, data map[string]any) {
		j.Append(engine.Event{RunID: runID, Kind: engine.KindCall, CallID: g.callID, Path: "root", Position: 1, Op: "agent", Label: "a", Event: event, Data: data})
	}
	row(engine.EvPrepared, map[string]any{"role": "executor", "family": "executor", "node": "n1"})
	row(engine.EvThreadRequested, map[string]any{"cwd": g.cwd, "send": 1, "at": g.base.UnixMilli(), "owner": "proc-dead"})
	if unknown {
		row(engine.EvOutcomeUnknown, map[string]any{"reason": engine.ReasonDispatchUnknown, "detail": "the answer was lost"})
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *guided) resume(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	all := append([]string{"flow", "resume", "--state", g.state, "--socket", g.host.SocketPath, "--repo", g.repo}, args...)
	return crw(t, append(all, runID, g.script)...)
}

func (g *guided) kinds(t *testing.T) []string {
	t.Helper()
	st := openStore(t, g.db)
	events, _, err := Load(ctx, st, runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Kind == engine.KindCall && e.CallID == g.callID {
			out = append(out, e.Event)
		}
	}
	_ = st.Close()
	return out
}

// The command a restart report names for a call is accepted by that call's state: `crw flow recover` takes a call in recovery
// hold, and `crw flow resume` takes a call at a dispatch intent nobody answered and a call whose hold was cleared. Neither is a
// refusal for the state it was named for.
func TestGuidedCommandsAcceptEachCallState(t *testing.T) {
	t.Run("a dispatch intent nobody answered: crw flow resume", func(t *testing.T) {
		g := newGuided(t, false)
		thread := g.host.Threads().Seed(fakehost.SeedThread{Cwd: g.cwd, CreatedAt: g.base.Add(time.Second)})
		code, out, errOut := g.resume(t)
		if code != 0 || !strings.Contains(out, `"status": "completed"`) {
			t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
		}
		if rows := g.kinds(t); !slices.Contains(rows, engine.EvThreadStarted) || rows[len(rows)-1] != engine.EvCompleted {
			t.Errorf("rows %v: reconcile found the thread the host showed (%s) and the call ended completed", rows, thread)
		}
	})
	t.Run("a recovery hold: crw flow recover, then crw flow resume for the call it cleared", func(t *testing.T) {
		g := newGuided(t, true)
		g.host.Threads().Seed(fakehost.SeedThread{Cwd: g.cwd, CreatedAt: g.base.Add(time.Second)})
		// the command the report names for a call in recovery hold accepts it
		if code, out, errOut := crwRecover(t, "--state", g.state, "--socket", g.host.SocketPath, "--absent", g.callID); code != 0 {
			t.Fatalf("recover: exit %d\nstdout %s\nstderr %s", code, out, errOut)
		}
		// the call is cleared now: recover no longer takes it, and the command the report names for it takes it
		if code, _, errOut := crwRecover(t, "--state", g.state, "--socket", g.host.SocketPath, "--absent", g.callID); code != 1 || !strings.Contains(errOut, "not in recovery_hold") {
			t.Errorf("recover of a cleared call: exit %d %q", code, errOut)
		}
		code, out, errOut := g.resume(t)
		if code != 0 || !strings.Contains(out, `"status": "completed"`) {
			t.Fatalf("resume: exit %d\nstdout %s\nstderr %s", code, out, errOut)
		}
	})
	// A call that ended held for a reason other than dispatch_unknown. The first process is the engine itself on the fake host,
	// so the rows are the ones a real hold writes. dag-restart reads them with callstate.Classify, the reading `crw flow recover`
	// accepts a call by, and names `crw flow resume` for it; the built binary refuses `crw flow recover` for the call (exit 1,
	// not in recovery_hold) and takes `crw flow resume`, which retries the held call in place. That is why one command resolves
	// every held reason: what a hold needs is a process that goes on with the call, and the reason decides only what that
	// process sends (the packet again, or the continuation of a delivered one).
	for _, hc := range []struct {
		name, reason string
		budget       int64
		setup        func(th *fakehost.Threads)
	}{
		{"budget_exhausted at the gate", engine.ReasonBudgetExhausted, 1, func(*fakehost.Threads) {}},
		{"a host refusal of thread/start", "host_infra_usageLimitExceeded", 0, func(th *fakehost.Threads) {
			th.OnStart(func(p map[string]any) map[string]any {
				if cwd, _ := p["cwd"].(string); strings.Contains(cwd, string(filepath.Separator)+"wt"+string(filepath.Separator)) {
					return fakehost.ErrorWithInfo(-32600, "usage limit", "usageLimitExceeded")
				}
				return nil
			})
		}},
		{"a turn that failed serverOverloaded", "host_infra_serverOverloaded", 0, func(th *fakehost.Threads) {
			th.OnTurn(func(r fakehost.TurnRequest) fakehost.TurnPlan {
				if strings.Contains(r.Text, "build it") && r.N == 1 {
					return fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "busy", CodexErrorInfo: "serverOverloaded"}}
				}
				return fakehost.TurnPlan{Message: "done"}
			})
		}},
	} {
		t.Run("held for "+hc.name+": crw flow resume", func(t *testing.T) {
			g := newHeldRun(t, hc.budget, hc.setup)
			rows := g.callRows(t)
			if last := rows[len(rows)-1]; last.Event != engine.EvHeld || last.Reason != hc.reason {
				t.Fatalf("the first process left the call at %+v, want held for %s", last, hc.reason)
			}
			if got := callstate.Classify(rows); got != callstate.Unsettled {
				t.Fatalf("the call is %s: dag-restart names crw flow recover only for a call in recovery hold", got)
			}
			if code, _, errOut := crwRecover(t, "--state", g.state, "--socket", g.host.SocketPath, "--absent", g.callID); code != 1 || !strings.Contains(errOut, "not in recovery_hold") {
				t.Errorf("recover of a held call: exit %d %q", code, errOut)
			}
			g.host.Threads().OnStart(nil)
			g.host.Threads().OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: "done"} })
			code, out, errOut := g.resume(t)
			if code != 0 || !strings.Contains(out, `"status": "completed"`) {
				t.Fatalf("resume: exit %d\nstdout %s\nstderr %s", code, out, errOut)
			}
			got := g.kinds(t)
			if got[len(got)-1] != engine.EvCompleted || countRows(got, engine.EvPrepared) != 1 {
				t.Errorf("rows %v: the held call did not go on in its own attempt and complete", got)
			}
			if !g.holdCleared(t) {
				t.Errorf("the resume wrote no hold_cleared row for %s", g.callID)
			}
		})
	}
	t.Run("the script and arguments have to be the run's", func(t *testing.T) {
		g := newGuided(t, false)
		if code, _, errOut := g.resume(t, "--args", `{"x":1}`); code != 2 || !strings.Contains(errOut, "arguments are not the ones") {
			t.Errorf("other arguments: exit %d %q", code, errOut)
		}
		if err := os.WriteFile(g.script, []byte(header+"return 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := g.resume(t); code != 2 || !strings.Contains(errOut, "script is not the one") {
			t.Errorf("another script: exit %d %q", code, errOut)
		}
		if code, _, errOut := crw(t, "flow", "resume", "--state", g.state, "--socket", g.host.SocketPath, "run-nothing", g.script); code != 2 || !strings.Contains(errOut, "holds no run") {
			t.Errorf("another run: exit %d %q", code, errOut)
		}
		if code, _, errOut := crw(t, "flow", "resume", g.script); code != 2 || !strings.Contains(errOut, "a run id and a script file") {
			t.Errorf("one argument: exit %d %q", code, errOut)
		}
	})
}

// callRows are the rows of the guided call in the stored journal, each with the reason it carries.
func (g *guided) callRows(t *testing.T) []callstate.Row {
	t.Helper()
	st := openStore(t, g.db)
	events, _, err := Load(ctx, st, runID)
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	var rows []callstate.Row
	for _, e := range events {
		if e.Kind == engine.KindCall && e.CallID == g.callID {
			reason, _ := e.Data["reason"].(string)
			rows = append(rows, callstate.Row{Event: e.Event, Reason: reason})
		}
	}
	return rows
}

const heldScript = header + "const warm = await agent.call('warm up', { agentType: 'explorer', label: 'warm' })\n" +
	"const a = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'a' })\n" +
	"return [warm.status, a.status, a.reason]\n"

// newHeldRun runs heldScript in this process, as the engine on the fake host, in a store of its own with the host faulted by
// setup (and the token budget given): the executor call 'a' ends held. The process is gone when it returns; the call, the
// store and the script file are what a `crw flow resume` of the built binary finds.
func newHeldRun(t *testing.T, budget int64, setup func(th *fakehost.Threads)) *guided {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	g := &guided{holdFixture: &holdFixture{host: fakehost.Start(t), base: time.Unix(1_780_000_000, 0)}}
	g.host.Threads().OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: "done"} })
	setup(g.host.Threads())
	dir := t.TempDir()
	g.script = filepath.Join(dir, "w.js")
	if err := os.WriteFile(g.script, []byte(heldScript), 0o644); err != nil {
		t.Fatal(err)
	}
	prog, err := script.Load(script.Source{Name: "w.js", Text: []byte(heldScript)})
	if err != nil {
		t.Fatal(err)
	}
	g.repo = filepath.Join(dir, "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", g.repo}, {"-C", g.repo, "commit", "-q", "--allow-empty", "-m", "base"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	g.state = t.TempDir()
	g.db = filepath.Join(g.state, "relay.sqlite3")
	st := openStore(t, g.db)
	j, err := Open(ctx, st, runID, identity, Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: g.repo, StateDir: g.state, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	threads := agent.NewThreads()
	client := appserver.New(g.host.SocketPath, appserver.DefaultBounds, agent.ClientOption(threads))
	backend, err := agent.New(agent.Config{Client: client, Allocator: alloc, Threads: threads})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(ctx, prog, engine.Config{Backend: backend, Policy: engine.FakePolicy(), Journal: j, RunID: runID, TokenBudget: budget})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("the first process: %v %+v", err, res)
	}
	events, _, err := Load(ctx, st, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == engine.KindCall && e.Label == "a" {
			g.callID = e.CallID
		}
	}
	backend.Close()
	if err := errors.Join(j.Close(), client.Close(), st.Close()); err != nil {
		t.Fatal(err)
	}
	return g
}

// holdCleared says whether the journal holds a hold_cleared row of the guided call.
func (g *guided) holdCleared(t *testing.T) bool {
	t.Helper()
	st := openStore(t, g.db)
	events, _, err := Load(ctx, st, runID)
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == engine.EvHoldCleared && e.Data["call_id"] == g.callID {
			return true
		}
	}
	return false
}

func countRows(rows []string, event string) int {
	n := 0
	for _, r := range rows {
		if r == event {
			n++
		}
	}
	return n
}
