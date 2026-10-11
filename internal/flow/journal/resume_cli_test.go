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

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
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
