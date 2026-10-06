package managed

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// orphanArchiveApp wraps the scripted App Server and records the thread/archive calls a
// reconciliation makes, so a test can see the one archive an abandoned orphan earns, make it fail,
// or make the next creation fail so a repeat reaches decide() again. The managed fake answers an
// unexpected host call with an error, so the archive needs its own seam.
type orphanArchiveApp struct {
	*scriptedApp
	archiveCalls []string
	archiveErr   error
	createErr    error
	creates      int
}

func (h *orphanArchiveApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if method != "thread/archive" {
		return h.scriptedApp.HostCall(ctx, method, params)
	}
	h.archiveCalls = append(h.archiveCalls, pyjson.Text(params["threadId"]))
	if h.archiveErr != nil {
		return nil, h.archiveErr
	}
	return map[string]any{}, nil
}

func (h *orphanArchiveApp) CreateThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	h.creates++
	if h.createErr != nil && h.creates > 1 {
		return nil, h.createErr
	}
	return h.scriptedApp.CreateThread(ctx, in)
}

// orphanStandby is CRW-783's standby receipt: the send failed while the host reported the thread
// idle, the resume found no rollout for the thread the timed-out creation left, and no turn exists.
func orphanStandby(thread string) map[string]any {
	return map[string]any{"operation": "send_message_to_thread", "status": "failed", "retrySafe": false,
		"threadId": thread, "statusBeforeResume": "idle",
		"error":    "thread/resume: no rollout found for thread id " + thread,
		"rpcError": map[string]any{"code": -32600, "message": "no rollout found for thread id " + thread}}
}

// orphanKit is the reconcile kit with the archive seam in front of the scripted host.
func orphanKit(t *testing.T, outcomes ...string) (*reconcileKit, *orphanArchiveApp) {
	t.Helper()
	k := newReconcileKit(t, outcomes...)
	app := &orphanArchiveApp{scriptedApp: k.host}
	k.start.Adapter = app
	return k, app
}

// orphanArchiveRows counts the managed_orphan_archive rows written for the request.
func orphanArchiveRows(t *testing.T, k *reconcileKit) int {
	t.Helper()
	var n int
	if err := k.start.Store.DB.QueryRow("SELECT COUNT(*) FROM journal WHERE kind='managed_orphan_archive'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// orphanArchiveDetail is the single managed_orphan_archive row's detail.
func orphanArchiveDetail(t *testing.T, k *reconcileKit) map[string]any {
	t.Helper()
	var raw string
	if err := k.start.Store.DB.QueryRow("SELECT detail FROM journal WHERE kind='managed_orphan_archive'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

// The CRW-783 receipt is adopted today; after the change the orphan is archived once and the next
// attempt is created under the same request.
func TestReconcileArchivesTheOrphanAStandbyResumeRefusalLeaves(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	// The first run records the standby receipt and answers as it always did.
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	if len(app.archiveCalls) != 0 || orphanArchiveRows(t, k) != 0 {
		t.Fatalf("the first run archived: %v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
	}
	got := k.run()
	k.expect(got, "admitted", "", "recreated")
	if got["childTaskId"] != "t-2" {
		t.Fatalf("the orphan was used: %v", got["childTaskId"])
	}
	if len(app.archiveCalls) != 1 || app.archiveCalls[0] != "t-1" {
		t.Fatalf("archive calls: %v", app.archiveCalls)
	}
	detail := orphanArchiveDetail(t, k)
	if detail["archive"] != "ok" || detail["thread"] != "t-1" || pyjson.Text(detail["error"]) != "" || detail["attempt"] != float64(0) {
		t.Fatalf("archive detail: %v", detail)
	}
}

// An archive the host refuses is recorded as failed and never stops the recreation.
func TestReconcileOrphanArchiveFailureStillRecreates(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	app.archiveErr = errors.New("thread/archive: host refused")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	got := k.run()
	k.expect(got, "admitted", "", "recreated")
	if got["childTaskId"] != "t-2" || len(app.archiveCalls) != 1 {
		t.Fatalf("the archive failure stopped the recreation: %v calls=%v", got["childTaskId"], app.archiveCalls)
	}
	detail := orphanArchiveDetail(t, k)
	if detail["archive"] != "failed" || !strings.Contains(pyjson.Text(detail["error"]), "host refused") {
		t.Fatalf("failure detail: %v", detail)
	}
}

// A repeat that reaches decide() again neither archives the orphan again nor adds a row.
func TestReconcileOrphanArchiveRepeatDoesNotArchiveAgain(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	app.archiveErr = errors.New("thread/archive: host refused")
	app.createErr = errors.New("thread/start: host refused")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	for range 2 {
		if _, err := k.runErr(); err == nil || !strings.Contains(err.Error(), "host refused") {
			t.Fatalf("the failing recreation is returned: %v", err)
		}
	}
	if len(app.archiveCalls) != 1 || orphanArchiveRows(t, k) != 1 {
		t.Fatalf("a repeat archived again: calls=%v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
	}
}

// A thread the host does not hold is not archived at all; the row says skipped.
func TestReconcileOrphanArchiveSkipsAThreadTheHostDoesNotHold(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	refusal := orphanStandby("t-1")
	refusal["error"] = "thread/resume: thread not found"
	refusal["rpcError"] = map[string]any{"code": -32600, "message": "thread not found"}
	k.host.sendReceipts = []map[string]any{refusal}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	k.expect(k.run(), "admitted", "", "recreated")
	if len(app.archiveCalls) != 0 {
		t.Fatalf("an unknown thread was archived: %v", app.archiveCalls)
	}
	detail := orphanArchiveDetail(t, k)
	if detail["archive"] != "skipped" {
		t.Fatalf("skipped row: %v", detail)
	}
}

// Every receipt that does not license abandonment keeps today's answer, and no orphan is archived.
func TestReconcileOrphanArchiveKeepsEveryUnchangedAnswer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"connection unavailable", func(r map[string]any) { r["rpcError"] = map[string]any{"code": "connection_unavailable"} }},
		{"a turn id", func(r map[string]any) { r["turnId"] = "turn-1" }},
		{"a verified resume", func(r map[string]any) { r["resumed"] = map[string]any{"cwd": "/tmp"} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			k, app := orphanKit(t, "name-timeout")
			refusal := orphanStandby("t-1")
			c.change(refusal)
			k.host.sendReceipts = []map[string]any{refusal}
			k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
			k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
			if len(app.archiveCalls) != 0 || orphanArchiveRows(t, k) != 0 {
				t.Fatalf("a held receipt archived: %v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
			}
		})
	}
}

// A creation that may have sent turn/start is never abandoned, so its standby is not archived.
func TestReconcileOrphanArchiveNeverTouchesACreationThatMayHaveTurned(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "turn-timeout")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "standby_turn_unknown")
	k.expect(k.run(), "incomplete", "creation_unknown", "standby_turn_unknown")
	if len(app.archiveCalls) != 0 || orphanArchiveRows(t, k) != 0 {
		t.Fatalf("a possible turn archived an orphan: %v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
	}
}

// Spent creations stay attempts_exhausted; the orphan of the last attempt is archived once, and a
// repeat neither archives it again nor adds a row.
func TestReconcileOrphanArchiveExhaustedAttemptsStayExhausted(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "name-timeout", "name-timeout")
	last := maxCreationAttempts - 1
	create, _ := OperationIDs("managed-1")
	for n := 1; n <= last; n++ {
		k.host.operations[attemptID("managed-1", create, n)] = map[string]any{"status": "outcome_unknown", "threadId": "t-1", "attemptedEffects": []any{"thread/start"}}
	}
	k.host.operations[recoveryID("managed-1", last)] = orphanStandby("t-3")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-3")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	for range 2 {
		k.expect(k.run(), "incomplete", "creation_unknown", "attempts_exhausted")
	}
	if len(app.archiveCalls) != 1 || app.archiveCalls[0] != "t-3" || orphanArchiveRows(t, k) != 1 {
		t.Fatalf("exhausted attempt archived: %v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
	}
	if detail := orphanArchiveDetail(t, k); detail["attempt"] != float64(last) || detail["archive"] != "ok" {
		t.Fatalf("row names the wrong attempt: %v", detail)
	}
}
