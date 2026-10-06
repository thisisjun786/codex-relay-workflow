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
	// archiveHook runs as the archive is sent, so a test can cancel the caller's context in the
	// window between the send and its reply.
	archiveHook func()
	createErr   error
	creates     int
}

func (h *orphanArchiveApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if method != "thread/archive" {
		return h.scriptedApp.HostCall(ctx, method, params)
	}
	h.archiveCalls = append(h.archiveCalls, pyjson.Text(params["threadId"]))
	if h.archiveHook != nil {
		h.archiveHook()
	}
	// The real client lets cancellation win over the reply (internal/bridge/appserver/client.go), so
	// a cancelled context is answered with its error, as the archive that was sent and never
	// answered is.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

// orphanArchiveDetail is the newest managed_orphan_archive row's detail: an archive that was sent
// writes its result row after the mark the attempt was given, so the newest row is the result.
func orphanArchiveDetail(t *testing.T, k *reconcileKit) map[string]any {
	t.Helper()
	var raw string
	if err := k.start.Store.DB.QueryRow("SELECT detail FROM journal WHERE kind='managed_orphan_archive' ORDER BY seq DESC LIMIT 1").Scan(&raw); err != nil {
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
	// The attempt is marked before the host effect and its result recorded after it.
	if rows := orphanArchiveRows(t, k); rows != 2 {
		t.Fatalf("the archive wrote %d rows, want the attempting and ok rows", rows)
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
	if rows := orphanArchiveRows(t, k); rows != 2 {
		t.Fatalf("the failed archive wrote %d rows, want the attempting and failed rows", rows)
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
	if len(app.archiveCalls) != 1 || orphanArchiveRows(t, k) != 2 {
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
	if rows := orphanArchiveRows(t, k); rows != 1 {
		t.Fatalf("the skipped archive wrote %d rows, want one", rows)
	}
	detail := orphanArchiveDetail(t, k)
	if detail["archive"] != "skipped" {
		t.Fatalf("skipped row: %v", detail)
	}
}

// thread/archive unloads an active thread, so an orphan that has taken a turn since the refusal is
// left alone and the row says skipped.
func TestReconcileOrphanArchiveLeavesAThreadThatBecameActive(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	// Another client sends the thread its first turn before the next attempt.
	k.host.threads["t-1"].turns = []string{"someone-elses-turn"}
	got := k.run()
	k.expect(got, "admitted", "", "recreated")
	if got["childTaskId"] != "t-2" || len(app.archiveCalls) != 0 {
		t.Fatalf("an active thread was archived: child=%v calls=%v", got["childTaskId"], app.archiveCalls)
	}
	if rows := orphanArchiveRows(t, k); rows != 1 {
		t.Fatalf("the skipped archive wrote %d rows, want one", rows)
	}
	detail := orphanArchiveDetail(t, k)
	if detail["archive"] != "skipped" || !strings.Contains(pyjson.Text(detail["error"]), "active") {
		t.Fatalf("active-thread row: %v", detail)
	}
}

// The archive is a host effect, so the readiness policy withholds it: nothing is archived and no
// row is written, and a later repeat under a ready policy archives the orphan.
func TestReconcileOrphanArchiveIsWithheldWhenThePolicyIsNotReady(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	k.start.Readiness = func(context.Context, map[string]any) (string, error) { return "worker_not_ready", nil }
	if got := k.run(); got["state"] != "refused" || pyjson.Text(got["reason"]) != "worker_not_ready" {
		t.Fatalf("unready policy answered %v/%v", got["state"], got["reason"])
	}
	if len(app.archiveCalls) != 0 || orphanArchiveRows(t, k) != 0 {
		t.Fatalf("an unready policy archived: calls=%v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
	}
	k.start.Readiness = func(context.Context, map[string]any) (string, error) { return "", nil }
	k.expect(k.run(), "admitted", "", "recreated")
	if len(app.archiveCalls) != 1 || orphanArchiveRows(t, k) != 2 {
		t.Fatalf("the ready repeat did not archive: calls=%v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
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
	if len(app.archiveCalls) != 1 || app.archiveCalls[0] != "t-3" || orphanArchiveRows(t, k) != 2 {
		t.Fatalf("exhausted attempt archived: %v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
	}
	if detail := orphanArchiveDetail(t, k); detail["attempt"] != float64(last) || detail["archive"] != "ok" {
		t.Fatalf("row names the wrong attempt: %v", detail)
	}
}

// The two P1s of the merged orphan-archive work. The first is a transient re-read failure: the
// orphan is neither archived nor recorded and the next attempt is created anyway, so the promise
// "one archive and one row per attempt" is broken. The second is a cancellation after
// thread/archive is sent but before it answers: no durable mark is left, so a repeat on a fresh
// context sends thread/archive a second time.

// A transient, non-cancellation failure of the orphan's re-read stops the call as unobservable:
// nothing is created, nothing is sent, no row is written, and a repeat with a working read
// archives once, writes the attempting and ok rows, and creates the next attempt.
func TestReconcileOrphanInconclusiveReadStopsTheCall(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")

	// The orphan's re-read fails with a transient error that is neither a cancellation nor one of
	// the texts that say the host does not hold the thread.
	k.host.failures["thread/read|t-1"] = errors.New("thread/read: connection reset by peer")
	got := k.run()
	k.expect(got, "incomplete", "creation_unknown", "unobservable")
	if detail := pyjson.Text(recon(got)["detail"]); !strings.Contains(detail, "could not be read again") || !strings.Contains(detail, "connection reset by peer") {
		t.Fatalf("the stop's detail: %q", detail)
	}
	if len(app.archiveCalls) != 0 || orphanArchiveRows(t, k) != 0 {
		t.Fatalf("an inconclusive read archived: calls=%v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
	}
	if len(k.host.creates) != 1 {
		t.Fatalf("an inconclusive read created the next attempt: %v", k.host.creates)
	}
	// A repeat decides again: with the read working the orphan is archived once and t-2 is created.
	delete(k.host.failures, "thread/read|t-1")
	k.expect(k.run(), "admitted", "", "recreated")
	if len(app.archiveCalls) != 1 || app.archiveCalls[0] != "t-1" {
		t.Fatalf("the repeat's archive calls: %v", app.archiveCalls)
	}
	if rows := orphanArchiveRows(t, k); rows != 2 {
		t.Fatalf("the repeat wrote %d rows, want the attempting and ok rows", rows)
	}
	if detail := orphanArchiveDetail(t, k); detail["archive"] != "ok" || detail["thread"] != "t-1" || detail["attempt"] != float64(0) {
		t.Fatalf("result row: %v", detail)
	}
	if k.host.creates[1] != k.host.creates[0] && !strings.HasPrefix(k.host.creates[1], "managed-create-") {
		t.Fatalf("the next attempt is a derived operation: %v", k.host.creates)
	}
}

// The attempt is marked before thread/archive is sent. A caller cancellation that lands after the
// send leaves the attempting row, and a repeat on a fresh context does not archive a second time:
// it recreates from the mark the cancelled call left.
func TestReconcileOrphanArchiveCancellationLeavesTheAttemptingRow(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")

	// The archive send is made and the caller's context is cancelled before the reply.
	ctx, cancel := context.WithCancel(context.Background())
	app.archiveHook = func() { cancel() }
	if _, err := k.start.Run(ctx, k.raw); err == nil {
		t.Fatalf("the cancelled archive is returned")
	}
	app.archiveHook = nil
	if len(app.archiveCalls) != 1 {
		t.Fatalf("archive calls after the cancellation: %v", app.archiveCalls)
	}
	if rows := orphanArchiveRows(t, k); rows != 1 {
		t.Fatalf("the cancelled archive wrote %d rows, want one attempting row", rows)
	}
	if detail := orphanArchiveDetail(t, k); detail["archive"] != "attempting" || detail["thread"] != "t-1" {
		t.Fatalf("the cancelled call's row: %v", detail)
	}

	// The repeat runs on a fresh context: it does not send thread/archive again and recreates.
	got := k.run()
	k.expect(got, "admitted", "", "recreated")
	if len(app.archiveCalls) != 1 {
		t.Fatalf("the repeat archived again: %v", app.archiveCalls)
	}
	if rows := orphanArchiveRows(t, k); rows != 1 {
		t.Fatalf("the repeat added a row: %d", rows)
	}
	if got["childTaskId"] != "t-2" {
		t.Fatalf("the repeat did not recreate: %v", got["childTaskId"])
	}
}

// A result row whose error text carries a quote and a newline is still matched by the
// request-and-attempt probe, so the attempt cannot archive twice.
func TestReconcileOrphanArchiveHostileErrorTextDoesNotDefeatTheProbe(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	app.archiveErr = errors.New("thread/archive: refused \"t-1\"\nretry later")
	// The recreation fails too, so a repeat reaches decide() again and would archive a second time
	// if the hostile error text defeated the attempt probe.
	app.createErr = errors.New("thread/start: host refused")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	for range 2 {
		if _, err := k.runErr(); err == nil || !strings.Contains(err.Error(), "host refused") {
			t.Fatalf("the failing recreation is returned: %v", err)
		}
	}
	if len(app.archiveCalls) != 1 || orphanArchiveRows(t, k) != 2 {
		t.Fatalf("hostile error text: calls=%v rows=%d", app.archiveCalls, orphanArchiveRows(t, k))
	}
	detail := orphanArchiveDetail(t, k)
	if detail["archive"] != "failed" || !strings.Contains(pyjson.Text(detail["error"]), "refused") {
		t.Fatalf("failure row: %v", detail)
	}
}

// The rows a start writes are the raw journal detail; this asserts the shape the probe relies on:
// the attempt is the field "attempt":<n> followed by its separator, in every row of the attempt.
func TestReconcileOrphanArchiveRowsCarryTheAttemptField(t *testing.T) {
	t.Parallel()
	k, _ := orphanKit(t, "name-timeout", "accept")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	k.expect(k.run(), "admitted", "", "recreated")
	rows, err := k.start.Store.DB.Query("SELECT detail FROM journal WHERE kind='managed_orphan_archive' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seen []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(raw, `"attempt":0,`) {
			t.Fatalf("a row does not carry the attempt field the probe matches: %s", raw)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, detail)
	}
	if len(seen) != 2 || seen[0]["archive"] != "attempting" || seen[1]["archive"] != "ok" {
		t.Fatalf("rows in order: %v", seen)
	}
}
