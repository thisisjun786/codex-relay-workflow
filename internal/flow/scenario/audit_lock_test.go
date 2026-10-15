package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
)

// TestFlowAuditWritesNothingWhileAProcessHoldsTheRun (CRW-1487): the end row of a run is written before its process is done with the
// journal (a paused end reconciles the summary and the escalations after it, and those rows take the next numbers). So `crw flow audit`
// records its result only under the run lock: while a process holds the lock the audit reads and reports, and records nothing, even
// though the end row is there; once the lock is free it records, and the row it adds takes the number after the last stored row.
func TestFlowAuditWritesNothingWhileAProcessHoldsTheRun(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	if got := closeOf(t, l); got.State != "closed" || got.Audit.OK != 2 {
		t.Fatalf("%+v", got)
	}
	prev := flowcli.AuditEnv
	flowcli.AuditEnv = flowcli.OpenAuditStore
	t.Cleanup(func() { flowcli.AuditEnv = prev })
	audit_ := func() (int, map[string]any, string) {
		_ = l.Store.Close()
		defer func() { l.Store = l.openStore() }()
		var out, errOut bytes.Buffer
		code := flowcli.RunAudit(context.Background(), []string{"audit", "--state", l.Root, "--json", "--run", l.RunID, "--socket", l.Host.SocketPath}, &out, &errOut)
		var rep map[string]any
		_ = json.Unmarshal(out.Bytes(), &rep)
		return code, rep, errOut.String()
	}
	auditRows := func() int {
		var n int
		if err := l.Store.DB.QueryRow(`SELECT COUNT(*) FROM flow_calls WHERE run_id = ? AND kind = 'crw.audit' AND event = 'completed'`, l.RunID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := auditRows()

	// a process still holds the run (its end row is written and it reconciles what goes up)
	lock, err := flowcli.TakeLock(l.Root, l.RunID, "run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, rep, errOut := audit_()
	lock.Release()
	why, _ := rep["not_recorded_because"].(string)
	if code != 0 || rep["recorded"] != false || !strings.Contains(why, "holds the run") {
		t.Fatalf("an audit while a process holds the run: exit %d, %v, %s", code, rep, errOut)
	}
	if after := auditRows(); after != before {
		t.Fatalf("an audit while a process holds the run wrote %d rows", after-before)
	}

	// the lock is free: the audit records, after the last stored row
	last := l.LastSeq()
	code, rep, errOut = audit_()
	if code != 0 || rep["recorded"] != true {
		t.Fatalf("an audit of a run no process holds: exit %d, %v, %s", code, rep, errOut)
	}
	if after := auditRows(); after != before+1 {
		t.Fatalf("%d audit rows, want %d", after, before+1)
	}
	events, _, err := journal.Load(context.Background(), l.Store, l.RunID)
	if err != nil {
		t.Fatal(err)
	}
	seq := 0
	for _, ev := range events {
		if ev.Op == "crw.audit" {
			seq = ev.Seq
		}
	}
	if seq <= last {
		t.Errorf("the audit row's seq %d, want after %d", seq, last)
	}
}
