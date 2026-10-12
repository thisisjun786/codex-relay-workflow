package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The command line of `crw flow audit`: what it refuses, the exit code of each result, and when it leaves a row in the journal.
// The audit itself is tested in internal/flow/audit and over a whole run in internal/flow/scenario.

func fixedClock(t *testing.T) {
	t.Helper()
	prev := AuditNow
	AuditNow = func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { AuditNow = prev })
}

// acceptedWithNoReview is a journal of a run with one acceptance whose review is not in it: an audit finds a mismatch.
func acceptedWithNoReview(finished bool) *engine.MemoryJournal {
	j := engine.NewMemoryJournal()
	j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_rel", Op: "crw.release", Event: engine.EvCompleted, Data: map[string]any{"node": "A", "milestone": "M1", "released": true}})
	j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_acc", Op: "crw.accept", Event: engine.EvCompleted, Data: map[string]any{"node": "A", "head": "abc", "acceptance_id": "acc-1", "plan": "p"}})
	if finished {
		j.Append(engine.Event{RunID: "run-1", Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": "completed"}})
	}
	return j
}

func useEnv(t *testing.T, j engine.Journal, finished bool, err error) *AuditRequest {
	t.Helper()
	var got AuditRequest
	prev := AuditEnv
	AuditEnv = func(_ context.Context, req AuditRequest) (AuditSetup, error) {
		got = req
		if err != nil {
			return AuditSetup{}, err
		}
		return AuditSetup{RunID: "run-1", Journal: j, Finished: finished}, nil
	}
	t.Cleanup(func() { AuditEnv = prev })
	return &got
}

func auditCmd(args ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	code = RunAudit(context.Background(), append([]string{"audit"}, args...), &o, &e)
	return code, o.String(), e.String()
}

func TestAuditCommandLine(t *testing.T) {
	if !Handles([]string{"audit", "--run", "r"}) || Handles([]string{"shadow"}) || Handles(nil) {
		t.Error("Handles")
	}
	for name, args := range map[string][]string{
		"no run and no acceptance": {},
		"a positional argument":    {"--run", "r", "extra"},
		"an unknown flag":          {"--run", "r", "--nope"},
	} {
		useEnv(t, acceptedWithNoReview(true), true, nil)
		if code, _, errOut := auditCmd(args...); code != 2 {
			t.Errorf("%s: exit %d, stderr %q", name, code, errOut)
		}
	}
	prev := AuditEnv
	AuditEnv = nil
	if code, _, errOut := auditCmd("--run", "r"); code != 2 || !strings.Contains(errOut, "installs no journal store") {
		t.Errorf("no environment: %d %q", code, errOut)
	}
	AuditEnv = prev
	useEnv(t, nil, false, errors.New("there is no relay store at /nowhere"))
	if code, _, errOut := auditCmd("--run", "r"); code != 2 || !strings.Contains(errOut, "no relay store") {
		t.Errorf("a store that does not open: %d %q", code, errOut)
	}
	// the request carries what the operator named
	req := useEnv(t, acceptedWithNoReview(true), true, nil)
	auditCmd("--acceptance", "acc-1", "--state", "/state", "--socket", "/sock", "--no-record")
	if req.AcceptanceID != "acc-1" || req.State != "/state" || req.Socket != "/sock" || req.RunID != "" {
		t.Errorf("request %+v", req)
	}
}

// A mismatch or an unobserved acceptance is exit 1; the result is a row of the journal (kind audit) when the run has finished,
// and it is not when the run is still writing its journal or --no-record is given.
func TestAuditCommandExitCodesAndRecording(t *testing.T) {
	fixedClock(t)
	j := acceptedWithNoReview(true)
	useEnv(t, j, true, nil)
	code, out, errOut := auditCmd("--run", "run-1", "--json")
	if code != 1 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var rep struct {
		Run      string         `json:"run"`
		Results  []audit.Result `json:"results"`
		Summary  audit.Summary  `json:"summary"`
		Recorded bool           `json:"recorded"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if rep.Run != "run-1" || len(rep.Results) != 1 || rep.Results[0].State != audit.Mismatch || rep.Summary.Mismatch != 1 || !rep.Recorded {
		t.Fatalf("%+v", rep)
	}
	var row *engine.Event
	for _, ev := range j.Events() {
		if ev.Op == audit.Op {
			row = &ev
		}
	}
	if row == nil || row.Event != engine.EvCompleted || !strings.HasPrefix(row.CallID, "audit_") {
		t.Fatalf("row %+v", row)
	}
	if res, _ := row.Data["results"].([]any); len(res) != 1 || row.Data["mismatch"] != float64(1) && row.Data["mismatch"] != 1 {
		t.Errorf("row data %+v", row.Data)
	}
	// text output names the acceptance and the state
	j2 := acceptedWithNoReview(true)
	useEnv(t, j2, true, nil)
	if code, out, _ := auditCmd("--run", "run-1"); code != 1 || !strings.Contains(out, "mismatch") || !strings.Contains(out, "acc-1") || !strings.Contains(out, "recorded in the journal") {
		t.Errorf("text: %d %q", code, out)
	}
	// --no-record, and a run that has not finished: the result is printed and nothing is appended
	for name, c := range map[string]struct {
		finished bool
		args     []string
	}{"no-record": {true, []string{"--no-record"}}, "running": {false, nil}} {
		j3 := acceptedWithNoReview(c.finished)
		useEnv(t, j3, c.finished, nil)
		before := len(j3.Events())
		code, out, _ := auditCmd(append([]string{"--run", "run-1", "--json"}, c.args...)...)
		var r struct {
			Recorded bool   `json:"recorded"`
			Why      string `json:"not_recorded_because"`
		}
		_ = json.Unmarshal([]byte(out), &r)
		if code != 1 || r.Recorded || r.Why == "" || len(j3.Events()) != before {
			t.Errorf("%s: exit %d, %+v, rows %d -> %d", name, code, r, before, len(j3.Events()))
		}
	}
	// an acceptance or a milestone that matches nothing is a command line that cannot be used
	useEnv(t, acceptedWithNoReview(true), true, nil)
	if code, _, errOut := auditCmd("--run", "run-1", "--acceptance", "acc-other"); code != 2 || !strings.Contains(errOut, "acc-other") {
		t.Errorf("another acceptance: %d %q", code, errOut)
	}
	if code, _, _ := auditCmd("--run", "run-1", "--milestone", "M9"); code != 2 {
		t.Errorf("another milestone: %d", code)
	}
}

// A store, a host client or a journal that fails to close after the audit is an error of the command: it says so on stderr and the
// exit code is 2, whatever the audit found (CRW-1206 correction, d8: the close ran after the exit code was chosen, so its error was
// dropped).
func TestAuditCommandReportsACloseError(t *testing.T) {
	fixedClock(t)
	for name, c := range map[string]struct {
		journal  *engine.MemoryJournal
		wantCode int
	}{
		"an audit with a mismatch": {acceptedWithNoReview(true), 2},
	} {
		prev := AuditEnv
		closed := 0
		AuditEnv = func(_ context.Context, req AuditRequest) (AuditSetup, error) {
			return AuditSetup{RunID: "run-1", Journal: c.journal, Finished: true, Close: func() error {
				closed++
				return errors.New("the store did not close")
			}}, nil
		}
		code, _, errOut := auditCmd("--run", "run-1", "--no-record")
		AuditEnv = prev
		if closed != 1 || code != c.wantCode || !strings.Contains(errOut, "the store did not close") {
			t.Errorf("%s: closed %d times, exit %d, stderr %q", name, closed, code, errOut)
		}
	}
}
