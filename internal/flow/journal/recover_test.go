package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// crwRecover runs `crw flow recover` of the built binary and returns its exit code and output.
func crwRecover(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(testsupport.CRW(t), append([]string{"flow", "recover"}, args...)...)
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

// holdFixture is a stored run with one call whose creation request got no answer and that reconcile left unknown: its rows
// end with outcome_unknown, and the node is in recovery_hold.
type holdFixture struct {
	host   *fakehost.Server
	state  string
	db     string
	cwd    string
	base   time.Time
	callID string
}

func newHold(t *testing.T) *holdFixture {
	t.Helper()
	f := &holdFixture{host: fakehost.Start(t), cwd: "/w/attempt-1", base: time.Unix(1_780_000_000, 0), callID: "c_hold"}
	f.state = t.TempDir()
	f.db = filepath.Join(f.state, "relay.sqlite3")
	st := openStore(t, f.db)
	j, err := Open(ctx, st, runID, identity, Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "t", "script_digest": "d", "args_digest": "a"}})
	row := func(event string, data map[string]any) {
		j.Append(engine.Event{RunID: runID, Kind: engine.KindCall, CallID: f.callID, Path: "root", Position: 1, Op: "agent", Label: "a", Event: event, Data: data})
	}
	row(engine.EvPrepared, map[string]any{"role": "executor", "family": "executor", "node": "n1"})
	row(engine.EvThreadRequested, map[string]any{"cwd": f.cwd, "send": 1, "at": f.base.UnixMilli(), "owner": "proc-dead"})
	row(engine.EvOutcomeUnknown, map[string]any{"reason": engine.ReasonDispatchUnknown, "detail": "the answer was lost"})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *holdFixture) recover(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return crwRecover(t, append([]string{"--state", f.state, "--socket", f.host.SocketPath}, args...)...)
}

func (f *holdFixture) rows(t *testing.T) []string {
	t.Helper()
	st := openStore(t, f.db)
	events, _, err := Load(ctx, st, runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Kind+"/"+e.Event)
	}
	_ = st.Close()
	return out
}

// `crw flow recover` of the built binary reaches the stored journal and the host: it is no longer the exit 2 of a build that
// installs nothing. With --absent it accepts the call when the host shows the proof of the automatic reconcile and refuses
// when it does not, appending nothing in that case.
func TestBuiltCrwFlowRecoverReachesTheStoredJournal(t *testing.T) {
	t.Run("accepts a call whose thread the host shows", func(t *testing.T) {
		f := newHold(t)
		id := f.host.Threads().Seed(fakehost.SeedThread{Cwd: f.cwd, CreatedAt: f.base.Add(time.Second)})
		code, out, errOut := f.recover(t, "--absent", f.callID)
		if code != 0 {
			t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
		}
		var res engine.RecoverResult
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if res.CallID != f.callID || res.Mode != "absent" || res.Action != engine.RecoverThreadReady || res.ThreadID != id {
			t.Errorf("%+v, want the thread %s", res, id)
		}
		rows := f.rows(t)
		if got := rows[len(rows)-2:]; !slices.Equal(got, []string{"call/recovery_cleared", "call/thread_started"}) {
			t.Errorf("rows %v: the recovery is stored", rows)
		}
		// the hold is released: the same command is refused with exit 1 and nothing more is stored
		before := len(rows)
		if code, _, errOut := f.recover(t, "--absent", f.callID); code != 1 || !strings.Contains(errOut, "not in recovery_hold") {
			t.Errorf("a second recover: exit %d %q", code, errOut)
		}
		if n := len(f.rows(t)); n != before {
			t.Errorf("a refused recover stored %d rows", n-before)
		}
	})
	t.Run("refuses a call when the host shows a thread that is not its own", func(t *testing.T) {
		f := newHold(t)
		f.host.Threads().Seed(fakehost.SeedThread{Cwd: f.cwd, CreatedAt: f.base.Add(time.Second), Turns: []fakehost.SeedTurn{{ClientID: "other/t1", Text: "someone else's work"}}})
		before := len(f.rows(t))
		code, out, errOut := f.recover(t, "--absent", f.callID)
		if code != 1 || !strings.Contains(errOut, "proof does not hold") || out != "" {
			t.Errorf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
		}
		if n := len(f.rows(t)); n != before {
			t.Errorf("a refused recover stored %d rows", n-before)
		}
	})
	t.Run("--adopt names the thread", func(t *testing.T) {
		f := newHold(t)
		id := f.host.Threads().Seed(fakehost.SeedThread{Cwd: f.cwd, CreatedAt: f.base.Add(time.Second)})
		code, out, errOut := f.recover(t, "--adopt", id, f.callID)
		if code != 0 || !strings.Contains(out, id) {
			t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
		}
		if rows := f.rows(t); rows[len(rows)-1] != "call/thread_started" {
			t.Errorf("rows %v", rows)
		}
	})
	t.Run("a command line it cannot use is exit 2", func(t *testing.T) {
		f := newHold(t)
		if code, _, errOut := f.recover(t, f.callID); code != 2 || !strings.Contains(errOut, "exactly one of --adopt and --absent") {
			t.Errorf("exit %d %q", code, errOut)
		}
		if code, _, errOut := f.recover(t, "--absent", "c_nothing"); code != 2 || !strings.Contains(errOut, "holds call c_nothing") {
			t.Errorf("a call no run holds: exit %d %q", code, errOut)
		}
		if code, _, errOut := crwRecover(t, "--state", t.TempDir(), "--socket", f.host.SocketPath, "--absent", f.callID); code != 2 || !strings.Contains(errOut, "no relay store") {
			t.Errorf("a state directory without a store: exit %d %q", code, errOut)
		}
	})
	t.Run("the state directory is selected the way the relay selects it", func(t *testing.T) {
		f := newHold(t)
		f.host.Threads().Seed(fakehost.SeedThread{Cwd: f.cwd, CreatedAt: f.base.Add(time.Second)})
		cmd := exec.Command(testsupport.CRW(t), "flow", "recover", "--socket", f.host.SocketPath, "--run", runID, "--absent", f.callID)
		cmd.Env = append(cmd.Environ(), "CODEX_SESSION_RELAY_STATE="+f.state)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil || !strings.Contains(out.String(), `"action": "thread_ready"`) {
			t.Errorf("%v %s", err, out.String())
		}
	})
}
