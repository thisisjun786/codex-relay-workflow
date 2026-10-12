package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// crw runs the built binary and returns its exit code and output.
func crw(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(testsupport.CRW(t), append([]string{"flow"}, args...)...)
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

// TestRecoverCommandRecordsEvidence: `crw flow recover` of the built binary, reached through the run surface, takes the run lock
// (a run a process holds is refused and nothing is appended), settles a call in recovery_hold with the thread the operator names
// and leaves the evidence it relied on in the journal (recovery_cleared with the mode, the thread and the evidence, then the
// thread_started row marked recovered), after which `show` no longer marks the call and `watch` lists the evidence rows.
func TestRecoverCommandRecordsEvidence(t *testing.T) {
	w := newWorld(t)
	host := fakehost.Start(t)
	base := time.Unix(1_780_000_000, 0)
	cwd := "/w/attempt-1"
	r := w.newRun("run-rec", "thread-owner")
	r.call("c_hold", "executor", "n1", 1, []string{engine.EvPrepared, engine.EvThreadRequested, engine.EvOutcomeUnknown}, map[string]map[string]any{
		engine.EvThreadRequested: {"cwd": cwd, "send": 1, "at": base.UnixMilli(), "owner": "proc-dead"},
		engine.EvOutcomeUnknown:  {"reason": engine.ReasonDispatchUnknown, "detail": "the answer was lost"},
	})
	r.close()
	w.close()
	thread := host.Threads().Seed(fakehost.SeedThread{Cwd: cwd, CreatedAt: base.Add(time.Second)})
	flowArgs := func(more ...string) []string {
		return append([]string{"--state", w.state, "--socket", host.SocketPath}, more...)
	}
	show := func() RunDetail {
		_, out, errOut := crw(t, append([]string{"show", "run-rec", "--json"}, flowArgs()...)...)
		var d RunDetail
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			t.Fatalf("show: %v\n%s\n%s", err, out, errOut)
		}
		return d
	}
	if d := show(); d.Holds != 1 || !d.Nodes[0].RecoveryHold {
		t.Fatalf("before the recovery: %+v", d.RunInfo)
	}

	// a process that holds the run: the recovery is refused and appends nothing
	lock, err := TakeLock(w.state, "run-rec", "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, _, errOut := crw(t, append([]string{"recover"}, flowArgs("--adopt", thread, "c_hold")...)...)
	lock.Release()
	if code != 2 || !strings.Contains(errOut, "a process holds the run run-rec") {
		t.Fatalf("recover of a run a process holds: exit %d %q", code, errOut)
	}
	if d := show(); d.Holds != 1 {
		t.Fatalf("a refused recovery changed the run: %+v", d.RunInfo)
	}

	code, out, errOut := crw(t, append([]string{"recover"}, flowArgs("--adopt", thread, "c_hold")...)...)
	if code != 0 {
		t.Fatalf("recover: exit %d\n%s\n%s", code, out, errOut)
	}
	var res engine.RecoverResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.CallID != "c_hold" || res.Mode != "adopt" || res.ThreadID != thread || res.Evidence == nil {
		t.Fatalf("the recovery: %v %+v", err, res)
	}
	if LockHeld(w.state, "run-rec") {
		t.Error("the recover command left the run lock held")
	}
	d := show()
	if d.Holds != 0 || d.Nodes[0].RecoveryHold || d.Phases[0].Agents[0].RecoveryHold {
		t.Errorf("after the recovery the call is still marked: %+v", d.RunInfo)
	}
	// the evidence rows are in the journal the watcher follows
	_, out, _ = crw(t, append([]string{"watch", "run-rec", "--after", "0", "--limit", "100", "--json"}, flowArgs()...)...)
	var page Page
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &page); err != nil {
		t.Fatal(err)
	}
	var cleared, started *Row
	for i := range page.Rows {
		switch page.Rows[i].Event {
		case "recovery_cleared":
			cleared = &page.Rows[i]
		case "thread_started":
			started = &page.Rows[i]
		}
	}
	if cleared == nil || cleared.Data["mode"] != "adopt" || cleared.Data["thread_id"] != thread || cleared.Data["evidence"] == nil {
		t.Fatalf("the recovery_cleared row: %+v", cleared)
	}
	if started == nil || started.Data["recovered"] != true || started.Data["thread_id"] != thread || started.Seq <= cleared.Seq {
		t.Errorf("the thread_started row: %+v", started)
	}
}
