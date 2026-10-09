package managed

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func businessResendUnloadRun(k *reconcileKit) *startRun {
	return &startRun{m: k.start, task: "t-1", standby: "standby", businessAttempt: 1, resendFailure: businessResendLegacyReceipt("t-1"), identity: Identity{RequestID: "managed-1"}, ledger: k.host.ledger}
}

// A transfer that starts the child's turn while the gate waits for readiness must be seen by the
// idle read that immediately precedes the archive: nothing is archived and the answer is the
// ordinary not-idle hold (CRW-1023 item 1, CRW-1053 item 1).
func TestBusinessResendUnloadReadsIdleAfterReadiness(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.host, host: k.host}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	k.start.Readiness = func(context.Context, map[string]any) (string, error) {
		k.host.threads["t-1"].status = "active"
		return "", nil
	}
	r := businessResendUnloadRun(k)
	if code, err := r.businessResendUnload(t.Context()); code != "recipient_not_idle" || err != nil {
		t.Fatalf("a child that became active: %q %v", code, err)
	}
	if len(app.archiveCalls) != 0 || len(app.unarchiveCalls) != 0 {
		t.Fatalf("a child that became active was archived: %v %v", app.archiveCalls, app.unarchiveCalls)
	}
}

// The idle read is the last thing before the archive: no journal write sits between them.
func TestBusinessResendUnloadArchivesRightAfterTheIdleRead(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.host, host: k.host}
	k.host.threads["t-1"].status = "idle"
	traced := &businessResendTraceApp{Adapter: app, k: k}
	k.start.Adapter = traced
	r := businessResendUnloadRun(k)
	if code, err := r.businessResendUnload(t.Context()); code != "" || err != nil {
		t.Fatalf("unload: %q %v", code, err)
	}
	for i, call := range traced.calls {
		if call.method != "thread/archive" {
			continue
		}
		if i == 0 || traced.calls[i-1].method != "thread/read" || traced.calls[i-1].journal != call.journal {
			t.Fatalf("the archive did not follow the idle read directly: %+v", traced.calls)
		}
		return
	}
	t.Fatalf("no archive: %+v", traced.calls)
}

type businessResendTraceCall struct {
	method  string
	journal int
}

type businessResendTraceApp struct {
	Adapter
	k     *reconcileKit
	calls []businessResendTraceCall
}

func (a *businessResendTraceApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	var n int
	if err := a.k.start.Store.DB.QueryRow("SELECT COUNT(*) FROM journal").Scan(&n); err != nil {
		return nil, err
	}
	a.calls = append(a.calls, businessResendTraceCall{method, n})
	return a.Adapter.HostCall(ctx, method, params)
}

func businessResendRejectRows(t *testing.T, k *reconcileKit, name, when string) {
	t.Helper()
	stmt := `CREATE TRIGGER ` + name + ` BEFORE INSERT ON journal WHEN NEW.kind='managed_resend_unloaded' AND ` + when + ` BEGIN SELECT RAISE(ABORT, 'journal refused'); END`
	if _, err := k.start.Store.DB.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

// The mark is written before the archive: when it cannot be written nothing is archived
// (CRW-1023 item 2).
func TestBusinessResendUnloadMarkFailureArchivesNothing(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.host, host: k.host}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	businessResendRejectRows(t, k, "refuse_begin", `NEW.detail LIKE '%"phase":"begin"%'`)
	r := businessResendUnloadRun(k)
	if code, err := r.businessResendUnload(t.Context()); err == nil {
		t.Fatalf("a mark that was not written still answered %q", code)
	}
	if len(app.archiveCalls) != 0 || len(app.unarchiveCalls) != 0 {
		t.Fatalf("archived without a mark: %v %v", app.archiveCalls, app.unarchiveCalls)
	}
}

// A result row that cannot be written leaves the begin mark: the same request run again does not
// lower the child a second time (CRW-1053 item 2).
func TestBusinessResendUnloadResultFailureKeepsTheOncePerAttemptBound(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.host, host: k.host}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	businessResendRejectRows(t, k, "refuse_result", `NEW.detail LIKE '%"unarchive"%'`)
	k.expect(k.run(), "incomplete", "lifecycle_unknown", "") // an unrecorded unload is never reported as done
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || !reflect.DeepEqual(app.unarchiveCalls, []string{"t-1"}) {
		t.Fatalf("first run: %v %v", app.archiveCalls, app.unarchiveCalls)
	}
	// The child came back loaded and idle in the meantime, as a rerun can find it.
	k.host.threads["t-1"].status = "idle"
	got, err := k.runErr()
	if err != nil {
		t.Fatal(err)
	}
	k.expect(got, "incomplete", "recipient_not_idle", "")
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || !reflect.DeepEqual(app.unarchiveCalls, []string{"t-1"}) || k.host.sent != 0 {
		t.Fatalf("the rerun lowered the child again: %v %v sent=%d", app.archiveCalls, app.unarchiveCalls, k.host.sent)
	}
}

// A lowering that was started and not carried out - the child was not idle at the last read -
// does not use up the attempt: the begin mark is closed by a row that says nothing was archived.
func TestBusinessResendUnloadNotArchivedDoesNotSpendTheAttempt(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.host, host: k.host}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	flip := true
	k.start.Readiness = func(context.Context, map[string]any) (string, error) {
		if flip {
			k.host.threads["t-1"].status = "active"
		}
		return "", nil
	}
	r := businessResendUnloadRun(k)
	if code, err := r.businessResendUnload(t.Context()); code != "recipient_not_idle" || err != nil {
		t.Fatalf("first: %q %v", code, err)
	}
	flip = false
	k.host.threads["t-1"].status = "idle"
	if code, err := r.businessResendUnload(t.Context()); code != "" || err != nil || !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) {
		t.Fatalf("the unload was not available again: %q %v %v", code, err, app.archiveCalls)
	}
	for _, row := range []string{`"phase":"begin"`, `"archive":"none"`} {
		var n int
		if err := k.start.Store.DB.QueryRow(`SELECT COUNT(*) FROM journal WHERE kind='managed_resend_unloaded' AND detail LIKE ?`, "%"+row+"%").Scan(&n); err != nil || n == 0 {
			t.Fatalf("missing %s row (%d, %v)", row, n, err)
		}
	}
	if detail := businessResendUnloadDetail(t, k); !strings.Contains(detail, `"phase":"end"`) || !strings.Contains(detail, `"archive":"ok"`) {
		t.Fatalf("result row: %s", detail)
	}
}
