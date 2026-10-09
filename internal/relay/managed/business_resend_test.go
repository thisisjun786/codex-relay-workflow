package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Synthetic version of the copied legacy receipt: the adapter stored a resume
// settings refusal without attemptedEffects/delivery, before turn/start.
func businessResendLegacyReceipt(thread string) map[string]any {
	return map[string]any{"operation": "send_message_to_thread", "status": "failed", "threadId": thread, "retrySafe": false,
		"statusBeforeResume": "idle", "mcpOverridesTransmitted": true,
		"error":    "thread/resume: settings_not_preserved: mcpServers returned different settings; message withheld",
		"rpcError": map[string]any{"code": "settings_not_preserved", "message": "mcpServers returned different settings; message withheld"}}
}

func TestBusinessResendNeverRepeatsPossibleDelivery(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"unknown", func(r map[string]any) { r["status"] = "outcome_unknown" }},
		{"in-flight", func(r map[string]any) { r["status"] = "in_progress_or_unknown" }},
		{"turn-id", func(r map[string]any) { r["turnId"] = "business" }},
		{"started", func(r map[string]any) { r["delivery"] = "turn_started" }},
		{"rejected", func(r map[string]any) { r["delivery"] = "rejected" }},
		{"unknown-delivery", func(r map[string]any) { r["delivery"] = "outcome_unknown" }},
		{"turn-start", func(r map[string]any) { r["attemptedEffects"] = []any{"thread/resume", "turn/start"} }},
		{"contradictory-not-attempted", func(r map[string]any) { r["status"] = "not_attempted"; r["attemptedEffects"] = []any{"turn/start"} }},
		{"turn-error", func(r map[string]any) { r["error"] = "turn/start: settings_not_preserved" }},
		{"different-thread", func(r map[string]any) { r["threadId"] = "other" }},
		{"different-operation", func(r map[string]any) { r["operation"] = "create_thread" }},
		{"no-structured-refusal", func(r map[string]any) { delete(r, "rpcError") }},
		{"arbitrary-error", func(r map[string]any) { r["error"] = "settings_not_preserved" }},
		{"malformed-effects", func(r map[string]any) { r["attemptedEffects"] = []any{42} }},
		{"empty-effect", func(r map[string]any) { r["attemptedEffects"] = []any{""} }},
		{"misspelled-turn", func(r map[string]any) { r["attemptedEffects"] = []any{"turn/start "} }},
		{"unknown-effect", func(r map[string]any) { r["attemptedEffects"] = []any{"something/else"} }},
		{"contradictory-error", func(r map[string]any) { r["attemptedEffects"] = []any{}; r["error"] = "turn/start: host refusal" }},
		{"null-effects", func(r map[string]any) { r["attemptedEffects"] = nil }},
		{"label-without-trace", func(r map[string]any) { r["delivery"] = "not_delivered" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			k, _, failure := businessResendKit(t)
			scenario.change(failure)
			for range 2 {
				if got := k.run(); got["state"] == "admitted" {
					t.Fatal("unsafe record admitted")
				}
			}
			if k.host.sent != 0 || len(k.host.sends) != 0 {
				t.Fatal("possible delivery was resent")
			}
		})
	}
}

func TestBusinessResendModernTraceAndBound(t *testing.T) {
	for _, effects := range []any{[]any{"thread/resume"}, []string{"thread/resume"}} {
		k, business, failure := businessResendKit(t)
		failure["attemptedEffects"], failure["delivery"] = effects, "not_delivered"
		delete(failure, "rpcError")
		for attempt := 1; attempt < maxBusinessResendAttempts-1; attempt++ {
			k.host.operations[businessResendID("managed-1", business, attempt)] = businessResendLegacyReceipt("t-1")
		}
		if got := k.run(); got["state"] != "admitted" || k.host.sent != 1 || k.host.sends[0] != businessResendID("managed-1", business, 2) {
			t.Fatal("last bounded attempt did not recover")
		}
	}
	k, business, _ := businessResendKit(t)
	for attempt := 1; attempt < maxBusinessResendAttempts; attempt++ {
		k.host.operations[businessResendID("managed-1", business, attempt)] = businessResendLegacyReceipt("t-1")
	}
	for range 2 {
		k.expect(k.run(), "incomplete", "business_failed", "")
	}
	if k.host.sent != 0 || len(k.host.operations) != 4 {
		t.Fatal("exhausted operation chain changed")
	}
	// An unknown successor stops the chain, even with a safe original failure.
	k.host.operations[businessResendID("managed-1", business, 1)]["status"] = "outcome_unknown"
	k.expect(k.run(), "incomplete", "business_outcome_unknown", "")
	if k.host.sent != 0 {
		t.Fatal("unknown successor skipped")
	}
}

type businessResendObservationApp struct {
	Adapter
	list func() (map[string]any, error)
}

func (a *businessResendObservationApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if method == "thread/turns/list" {
		return a.list()
	}
	return a.Adapter.HostCall(ctx, method, params)
}

func TestBusinessResendHostHistoryWithholds(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		rows   any
		cursor any
		err    error
		reason string
	}{
		{"another", []any{map[string]any{"id": "standby"}, map[string]any{"id": "other"}}, nil, nil, "business_identity_unobserved"},
		{"another-more", []any{map[string]any{"id": "standby"}, map[string]any{"id": "other"}}, "more", nil, "business_identity_unobserved"},
		{"another-first-more", []any{map[string]any{"id": "other"}, map[string]any{"id": "standby"}}, "more", nil, "business_identity_unobserved"},
		{"wrong-anchor", []any{map[string]any{"id": "other"}}, nil, nil, "business_identity_unobserved"},
		{"empty", []any{}, nil, nil, "lifecycle_unknown"},
		{"malformed", "bad", nil, nil, "lifecycle_unknown"},
		{"missing-id", []any{map[string]any{}}, nil, nil, "lifecycle_unknown"},
		{"missing-id-after-standby", []any{map[string]any{"id": "standby"}, map[string]any{}}, nil, nil, "lifecycle_unknown"},
		{"malformed-before-standby", []any{42, map[string]any{"id": "standby"}}, nil, nil, "lifecycle_unknown"},
		{"missing-id-before-foreign-more", []any{map[string]any{}, map[string]any{"id": "other"}}, "more", nil, "business_identity_unobserved"},
		{"foreign-before-missing-id-more", []any{map[string]any{"id": "other"}, map[string]any{}}, "more", nil, "business_identity_unobserved"},
		{"duplicate-standby", []any{map[string]any{"id": "standby"}, map[string]any{"id": "standby"}}, nil, nil, "lifecycle_unknown"},
		{"more", []any{map[string]any{"id": "standby"}}, "more", nil, "lifecycle_unknown"},
		{"bad-cursor", []any{map[string]any{"id": "standby"}}, 3, nil, "lifecycle_unknown"},
		{"read-error", nil, nil, errors.New("observation failed"), "lifecycle_unknown"},
		{"no-rollout", nil, nil, errors.New("thread/turns/list: no rollout found"), "lifecycle_unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			k, business, _ := businessResendKit(t)
			k.start.Adapter = &businessResendObservationApp{Adapter: k.host, list: func() (map[string]any, error) {
				return map[string]any{"data": scenario.rows, "nextCursor": scenario.cursor}, scenario.err
			}}
			state := "incomplete"
			if scenario.reason == "business_identity_unobserved" {
				state = "refused"
			}
			k.expect(k.run(), state, scenario.reason, "")
			if len(k.host.sends) != 0 || k.host.sent != 0 || k.host.operations[businessResendID("managed-1", business, 1)] != nil {
				t.Fatal("unobserved/foreign history consumed operation")
			}
		})
	}
}

func TestBusinessResendPaginatedFinalGuardWithholds(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		rows   []any
		reason string
	}{
		{"another-more", []any{map[string]any{"id": "standby"}, map[string]any{"id": "other"}}, "business_identity_unobserved"},
		{"another-first-more", []any{map[string]any{"id": "other"}, map[string]any{"id": "standby"}}, "business_identity_unobserved"},
		{"standby-more", []any{map[string]any{"id": "standby"}}, "lifecycle_unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			k, business, _ := businessResendKit(t)
			final := false
			k.start.Adapter = &businessResendObservationApp{Adapter: k.host, list: func() (map[string]any, error) {
				if final {
					return map[string]any{"data": scenario.rows, "nextCursor": "more"}, nil
				}
				return map[string]any{"data": []any{map[string]any{"id": "standby"}}}, nil
			}}
			k.host.beforeSend = func(in SendRequest) {
				final = true
				withhold, err := in.BeforeStart(t.Context())
				if err != nil || withhold["code"] != scenario.reason {
					t.Fatalf("final history guard: %v %v", withhold, err)
				}
			}
			k.expect(k.run(), "incomplete", "business_failed", "")
			if !final || k.host.sent != 0 || k.host.operations[businessResendID("managed-1", business, 1)] != nil {
				t.Fatal("final history guard consumed operation or was not reached")
			}
		})
	}
}

func TestBusinessResendLoadObservationIsLast(t *testing.T) {
	k, business, failure := businessResendKit(t)
	// This test is about ordering, not about the unload: a non-settings pre-turn failure keeps
	// the loaded child on the plain recipient_not_idle path it has always taken.
	failure["attemptedEffects"], failure["delivery"] = []any{"thread/resume"}, "not_delivered"
	delete(failure, "rpcError")
	k.start.Adapter = &businessResendObservationApp{Adapter: k.host, list: func() (map[string]any, error) {
		k.host.threads["t-1"].status = "idle"
		return map[string]any{"data": []any{map[string]any{"id": "standby"}}}, nil
	}}
	for range 2 {
		k.expect(k.run(), "incomplete", "recipient_not_idle", "")
	}
	if len(k.host.sends) != 0 || k.host.operations[businessResendID("managed-1", business, 1)] != nil {
		t.Fatal("loaded observation consumed retry")
	}
	k.start.Adapter = &businessResendReplayApp{Adapter: k.host, host: k.host}
	k.host.threads["t-1"].status = "notLoaded"
	if got := k.run(); got["state"] != "admitted" {
		t.Fatal("unloaded child did not recover")
	}
}

func TestBusinessResendRechecksHistoryJustBeforeStart(t *testing.T) {
	k, _, _ := businessResendKit(t)
	k.host.beforeSend = func(SendRequest) {
		k.host.threads["t-1"].turns = append(k.host.threads["t-1"].turns, "foreign-completed")
		k.host.threads["t-1"].status = "idle"
	}
	k.expect(k.run(), "incomplete", "business_failed", "")
	if k.host.sent != 0 {
		t.Fatal("final history guard bypassed")
	}
}

func TestBusinessResendRetainsAcceptedAttemptAfterAdmissionLoss(t *testing.T) {
	k, business, _ := businessResendKit(t)
	retry := businessResendID("managed-1", business, 1)
	k.host.operations[retry] = map[string]any{"status": "accepted", "threadId": "t-1", "turnId": "already-started"}
	k.host.threads["t-1"].turns = append(k.host.threads["t-1"].turns, "already-started")
	if got := k.run(); got["businessTurnId"] != "already-started" || got["state"] != "admitted" {
		t.Fatal("accepted retry was not admitted")
	}
	if k.host.sent != 0 {
		t.Fatal("accepted operation repeated")
	}
}

func businessResendKit(t *testing.T) (*reconcileKit, string, map[string]any) {
	t.Helper()
	k := newReconcileKit(t)
	k.host.initialTurns = []string{"standby"}
	k.host.standby = "inProgress"
	k.expect(k.run(), "incomplete", "standby_incomplete", "")
	k.host.standby = "completed"
	k.host.threads["t-1"].status = "notLoaded"
	_, business := OperationIDs("managed-1")
	failure := businessResendLegacyReceipt("t-1")
	k.host.operations[business] = failure
	k.start.Adapter = &businessResendReplayApp{Adapter: k.host, host: k.host}
	return k, business, failure
}

// Match the production adapter's terminal receipt replay without dispatching
// again. Argument collisions are exercised against the real ledger/socket.
type businessResendReplayApp struct {
	Adapter
	host *scriptedApp
}

func (a *businessResendReplayApp) SendMessage(ctx context.Context, in SendRequest) (map[string]any, error) {
	if retained := a.host.operations[in.RequestID]; retained != nil && retained["status"] == "accepted" {
		return retained, nil
	}
	return a.Adapter.SendMessage(ctx, in)
}

type businessResendBudgetApp struct {
	Adapter
	pages int
	calls []string
}

func (a *businessResendBudgetApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	a.calls = append(a.calls, method)
	if method == "thread/list" {
		if params["archived"] != true {
			return nil, errors.New("unnecessary unarchived scan")
		}
		a.pages++
		var cursor any
		if a.pages < 4 {
			cursor = fmt.Sprint(a.pages)
		}
		return map[string]any{"data": []any{}, "nextCursor": cursor}, nil
	}
	return a.Adapter.HostCall(ctx, method, params)
}

func TestBusinessResendGuardBudget(t *testing.T) {
	k, _, _ := businessResendKit(t)
	a := &businessResendBudgetApp{Adapter: k.host}
	if code, err := businessResendCheckHost(t.Context(), a, "t-1", true); code != "" || err != nil {
		t.Fatalf("readiness: %s %v", code, err)
	}
	k.start.Adapter = a
	r := &startRun{m: k.start, task: "t-1", standby: "standby"}
	if code, err := r.businessResendOnlyStandby(t.Context()); code != "" || err != nil {
		t.Fatalf("history: %s %v", code, err)
	}
	if len(a.calls) != 7 || a.pages != 4 {
		t.Fatalf("guard exceeds its ten-call budget: %v", a.calls)
	}
}

func TestBusinessResendLegacySameRequest(t *testing.T) {
	k, business, failure := businessResendKit(t)
	before, _ := json.Marshal(failure)
	first := k.run()
	if first["state"] != "admitted" || first["businessTurnId"] != "business" {
		t.Fatalf("pre-turn failed receipt must recover: state=%v reason=%v", first["state"], first["reason"])
	}
	for range 3 {
		if got := k.run(); !reflect.DeepEqual(got, first) {
			t.Fatalf("replay changed managed identity: %v", got)
		}
	}
	after, _ := json.Marshal(k.host.operations[business])
	if string(before) != string(after) || k.host.created != 1 || k.host.sent != 1 || len(k.host.sends) != 1 || k.host.sends[0] == business || first["businessRequestId"] != business {
		t.Fatalf("failure/identity/effects changed: created=%d sent=%d IDs=%v", k.host.created, k.host.sent, k.host.sends)
	}
	if !reflect.DeepEqual(k.host.threads["t-1"].turns, []string{"standby", "business"}) {
		t.Fatal("business turn duplicated")
	}
}

func TestBusinessResendIDStability(t *testing.T) {
	// Independently calculated with Node's SHA256, not the production helper.
	for attempt, want := range []string{
		"original-dispatch",
		"managed-business-0234b0a039e13b7420850fc7500c963463f5841e0f2e80ec4d03d67843748bc4",
		"managed-business-88d99ca77068dbecc2b55e5e2ec061fdd2dac2e080bccb230a4dd4400bd435de",
	} {
		if got := businessResendID("managed-1", "original-dispatch", attempt); got != want {
			t.Fatalf("attempt %d: got %s, want %s", attempt, got, want)
		}
	}
}

// businessResendUnloadApp serves the archive/unarchive pair the resend gate uses to lower an
// idle child the host holds loaded under other MCP settings. scriptedApp and managedFake serve
// neither method, so the unload tests wrap them with this.
type businessResendUnloadApp struct {
	Adapter
	host         *scriptedApp
	archiveErr   error
	unarchiveErr error
	onUnarchive  func()
	stayLoaded   bool
	// archiveLostReply models an archive that applied and whose reply was lost: the call returns
	// an error, but the child is left archived and unloaded, so the archived listing confirms it
	// and an unarchive restores it. archivedListing overrides what the archived page answers
	// ("incomplete" or "error"), and onArchivedList runs on each archived page the reply check
	// asks for.
	archiveLostReply bool
	archivedListing  string
	onArchivedList   func()
	archived         map[string]bool
	listingCalls     int
	archiveCalls     []string
	unarchiveCalls   []string
}

func (a *businessResendUnloadApp) markArchived(id string, archived bool) {
	if a.archived == nil {
		a.archived = map[string]bool{}
	}
	if archived {
		a.archived[id] = true
		return
	}
	delete(a.archived, id)
}

func (a *businessResendUnloadApp) archivedPage() (map[string]any, error) {
	a.listingCalls++
	if a.onArchivedList != nil {
		a.onArchivedList()
	}
	switch a.archivedListing {
	case "incomplete":
		return map[string]any{"data": "bad"}, nil
	case "error":
		return nil, errors.New("thread/list: host refused")
	}
	data := []any{}
	for _, known := range a.host.order {
		if a.archived[known] {
			data = append(data, map[string]any{"id": known})
		}
	}
	return map[string]any{"data": data}, nil
}

func (a *businessResendUnloadApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	id := pyjson.Text(params["threadId"])
	switch method {
	case "thread/list":
		if params["archived"] == true {
			return a.archivedPage()
		}
	case "thread/archive":
		a.archiveCalls = append(a.archiveCalls, id)
		if a.archiveLostReply {
			a.markArchived(id, true)
			if !a.stayLoaded {
				a.host.threads[id].status = "notLoaded"
			}
			return nil, errors.New("thread/archive: reply lost")
		}
		if a.archiveErr != nil {
			return nil, a.archiveErr
		}
		if !a.stayLoaded {
			a.host.threads[id].status = "notLoaded"
		}
		return map[string]any{}, nil
	case "thread/unarchive":
		a.unarchiveCalls = append(a.unarchiveCalls, id)
		if a.onUnarchive != nil {
			a.onUnarchive()
		}
		if a.unarchiveErr != nil {
			return nil, a.unarchiveErr
		}
		a.markArchived(id, false)
		return map[string]any{}, nil
	}
	return a.Adapter.HostCall(ctx, method, params)
}

func businessResendJournalCount(t *testing.T, k *reconcileKit, kind string) int {
	t.Helper()
	var n int
	if err := k.start.Store.DB.QueryRow("SELECT COUNT(*) FROM journal WHERE kind=?", kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// businessResendLoweringRows counts the lowerings an unload recorded: the closing rows that name an
// archive. The begin marks and the rows that say nothing was archived are not lowerings.
func businessResendLoweringRows(t *testing.T, k *reconcileKit) int {
	t.Helper()
	var n int
	if err := k.start.Store.DB.QueryRow(`SELECT COUNT(*) FROM journal WHERE kind='managed_resend_unloaded' AND detail LIKE '%"phase":"end"%' AND detail NOT LIKE '%"archive":"none"%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A child the host holds loaded under other MCP settings, idle with exactly its standby turn, is
// lowered once and then resent: archive, unarchive, notLoaded, one business turn, one journal row.
func TestBusinessResendUnloadsIdleStandbyOnlyChild(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	got := k.run()
	if got["state"] != "admitted" || got["businessTurnId"] != "business" {
		t.Fatalf("loaded idle child did not recover: %v %v", got["state"], got["reason"])
	}
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || !reflect.DeepEqual(app.unarchiveCalls, []string{"t-1"}) {
		t.Fatalf("archive/unarchive exactly once: %v %v", app.archiveCalls, app.unarchiveCalls)
	}
	if !reflect.DeepEqual(k.host.threads["t-1"].turns, []string{"standby", "business"}) {
		t.Fatalf("business turn count: %v", k.host.threads["t-1"].turns)
	}
	if n := businessResendLoweringRows(t, k); n != 1 {
		t.Fatalf("unload journal rows: %d", n)
	}
}

// Only an idle child whose immediately preceding business failure is the structured settings
// refusal is lowered; every other case holds without touching the thread.
func TestBusinessResendUnloadOnlyForIdleSettingsMismatch(t *testing.T) {
	for _, c := range []struct {
		name          string
		change        func(*reconcileKit, map[string]any)
		state, reason string
	}{
		{"active", func(k *reconcileKit, _ map[string]any) { k.host.threads["t-1"].status = "busy" }, "incomplete", "recipient_not_idle"},
		{"foreign-history", func(k *reconcileKit, _ map[string]any) {
			k.host.threads["t-1"].turns = []string{"standby", "other"}
			k.host.threads["t-1"].status = "idle"
		}, "refused", "business_identity_unobserved"},
		{"not-settings", func(_ *reconcileKit, failure map[string]any) {
			failure["attemptedEffects"], failure["delivery"] = []any{"thread/resume"}, "not_delivered"
			delete(failure, "rpcError")
		}, "incomplete", "recipient_not_idle"},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, _, failure := businessResendKit(t)
			app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host}
			k.start.Adapter = app
			k.host.threads["t-1"].status = "idle"
			c.change(k, failure)
			for range 2 {
				k.expect(k.run(), c.state, c.reason, "")
			}
			if len(app.archiveCalls) != 0 || len(app.unarchiveCalls) != 0 || k.host.sent != 0 || len(k.host.sends) != 0 {
				t.Fatalf("held case touched the thread: %v %v sent=%d", app.archiveCalls, app.unarchiveCalls, k.host.sent)
			}
			if n := businessResendLoweringRows(t, k); n != 0 {
				t.Fatalf("held case wrote %d unload rows", n)
			}
		})
	}
}

// An archive error answers incomplete recipient_not_idle with no unarchive, no send and no row:
// the thread was never lowered.
func TestBusinessResendUnloadArchiveErrorHolds(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host, archiveErr: errors.New("thread/archive: host refused")}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	for range 2 {
		k.expect(k.run(), "incomplete", "recipient_not_idle", "")
	}
	if len(app.unarchiveCalls) != 0 || k.host.sent != 0 || len(k.host.sends) != 0 {
		t.Fatalf("archive failure unarchived or sent: %v sent=%d", app.unarchiveCalls, k.host.sent)
	}
	if n := businessResendLoweringRows(t, k); n != 0 {
		t.Fatalf("archive failure wrote %d unload rows", n)
	}
}

// An unarchive that fails twice answers incomplete lifecycle_unknown, records the archived thread
// in the journal detail with the operator hint, and never sends.
func TestBusinessResendUnloadUnarchiveErrorHolds(t *testing.T) {
	k, business, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host, unarchiveErr: errors.New("thread/unarchive: host refused")}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	k.expect(k.run(), "incomplete", "lifecycle_unknown", "")
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || len(app.unarchiveCalls) != 2 || k.host.sent != 0 || len(k.host.sends) != 0 || k.host.operations[businessResendID("managed-1", business, 1)] != nil {
		t.Fatalf("unarchive retry/withhold: archive=%v unarchive=%v sent=%d", app.archiveCalls, app.unarchiveCalls, k.host.sent)
	}
	var detail string
	if err := k.start.Store.DB.QueryRow("SELECT detail FROM journal WHERE kind='managed_resend_unloaded' ORDER BY seq DESC LIMIT 1").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "t-1") || !strings.Contains(detail, "thread/unarchive t-1") {
		t.Fatalf("unarchive failure detail: %s", detail)
	}
	if n := businessResendLoweringRows(t, k); n != 1 {
		t.Fatalf("unload journal rows: %d", n)
	}
}

// A thread still loaded after the unload answers incomplete recipient_not_idle with no send.
func TestBusinessResendUnloadStillLoadedHolds(t *testing.T) {
	k, business, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host, stayLoaded: true}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	k.expect(k.run(), "incomplete", "recipient_not_idle", "")
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || !reflect.DeepEqual(app.unarchiveCalls, []string{"t-1"}) || k.host.sent != 0 || len(k.host.sends) != 0 || k.host.operations[businessResendID("managed-1", business, 1)] != nil {
		t.Fatalf("still loaded case sent: %v %v sent=%d", app.archiveCalls, app.unarchiveCalls, k.host.sent)
	}
	// A replay reconstructs the same attempt, so the one-lowering-per-attempt bound has to be
	// durable: the row the first lowering wrote is what stops the second one.
	k.expect(k.run(), "incomplete", "recipient_not_idle", "")
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || !reflect.DeepEqual(app.unarchiveCalls, []string{"t-1"}) {
		t.Fatalf("replay lowered the child again: %v %v", app.archiveCalls, app.unarchiveCalls)
	}
	if n := businessResendLoweringRows(t, k); n != 1 {
		t.Fatalf("unload journal rows: %d", n)
	}
}

// The archive is a host effect, and the engine asks readiness again before each one: a policy
// that went away while the gate was reading withholds the archive and answers its code.
func TestBusinessResendUnloadRechecksReadinessBeforeArchive(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.host, host: k.host}
	k.host.threads["t-1"].status = "idle"
	k.start.Adapter = app
	k.start.Readiness = func(context.Context, map[string]any) (string, error) { return "worker_policy_unconfigured", nil }
	r := &startRun{m: k.start, task: "t-1", standby: "standby", businessAttempt: 1, resendFailure: businessResendLegacyReceipt("t-1")}
	if code, err := r.businessResendUnload(t.Context()); code != "worker_policy_unconfigured" || err != nil {
		t.Fatalf("readiness recheck: %q %v", code, err)
	}
	if len(app.archiveCalls) != 0 || len(app.unarchiveCalls) != 0 {
		t.Fatalf("archive ran without readiness: %v %v", app.archiveCalls, app.unarchiveCalls)
	}
}

// Once the archive has succeeded the child is archived, so the row recording it has to survive the
// caller's cancellation: an archived child with no row leaves an operator nothing to read.
func TestBusinessResendUnloadRecordsArchivedChildWhenCancelled(t *testing.T) {
	k, _, _ := businessResendKit(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app := &businessResendUnloadApp{Adapter: k.host, host: k.host, unarchiveErr: errors.New("thread/unarchive: cancelled"), onUnarchive: cancel}
	k.host.threads["t-1"].status = "idle"
	k.start.Adapter = app
	r := &startRun{m: k.start, task: "t-1", standby: "standby", businessAttempt: 1, resendFailure: businessResendLegacyReceipt("t-1"), identity: Identity{RequestID: "managed-1"}, ledger: k.host.ledger}
	if code, err := r.businessResendUnload(ctx); code != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled unarchive: %q %v", code, err)
	}
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || !reflect.DeepEqual(app.unarchiveCalls, []string{"t-1"}) {
		t.Fatalf("archive/unarchive calls: %v %v", app.archiveCalls, app.unarchiveCalls)
	}
	var detail string
	if err := k.start.Store.DB.QueryRow("SELECT detail FROM journal WHERE kind='managed_resend_unloaded' ORDER BY seq DESC LIMIT 1").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "t-1") || !strings.Contains(detail, "thread/unarchive t-1") {
		t.Fatalf("cancelled unload detail: %s", detail)
	}
}

// businessResendUnloadDetail is the single managed_resend_unloaded row's detail.
func businessResendUnloadDetail(t *testing.T, k *reconcileKit) string {
	t.Helper()
	var detail string
	if err := k.start.Store.DB.QueryRow("SELECT detail FROM journal WHERE kind='managed_resend_unloaded' ORDER BY seq DESC LIMIT 1").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

// An archive that applied but whose reply was lost leaves the child archived. The gate asks the
// host once, sees it in the archived listing, and continues exactly as after a successful archive:
// one unarchive, one journal row naming the lost reply, then the resend.
func TestBusinessResendUnloadRecoversLostArchiveReply(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host, archiveLostReply: true}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	got := k.run()
	if got["state"] != "admitted" || got["businessTurnId"] != "business" {
		t.Fatalf("lost archive reply did not recover: %v %v", got["state"], got["reason"])
	}
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || !reflect.DeepEqual(app.unarchiveCalls, []string{"t-1"}) {
		t.Fatalf("archive/unarchive exactly once: %v %v", app.archiveCalls, app.unarchiveCalls)
	}
	if !reflect.DeepEqual(k.host.threads["t-1"].turns, []string{"standby", "business"}) {
		t.Fatalf("business turn count: %v", k.host.threads["t-1"].turns)
	}
	if n := businessResendLoweringRows(t, k); n != 1 {
		t.Fatalf("unload journal rows: %d", n)
	}
	detail := businessResendUnloadDetail(t, k)
	if !strings.Contains(detail, `"archive":"reply_lost"`) || !strings.Contains(detail, `"unarchive":"ok"`) || !strings.Contains(detail, `"attempt":1`) {
		t.Fatalf("lost archive reply detail: %s", detail)
	}
}

// The reply-lost recovery is still one lowering per business attempt: a replay reconstructs the
// same attempt from the retained failure, and the row the first lowering wrote stops the second.
func TestBusinessResendUnloadLostArchiveReplyIsBoundedPerAttempt(t *testing.T) {
	k, business, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host, archiveLostReply: true, stayLoaded: true}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	for range 2 {
		k.expect(k.run(), "incomplete", "recipient_not_idle", "")
	}
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || !reflect.DeepEqual(app.unarchiveCalls, []string{"t-1"}) {
		t.Fatalf("replay lowered the child again: %v %v", app.archiveCalls, app.unarchiveCalls)
	}
	if k.host.sent != 0 || k.host.operations[businessResendID("managed-1", business, 1)] != nil {
		t.Fatal("still loaded child was resent")
	}
	if n := businessResendLoweringRows(t, k); n != 1 {
		t.Fatalf("unload journal rows: %d", n)
	}
}

// An archive error the host does not confirm as applied keeps today's answer: the child is not in
// the archived listing, the listing is incomplete, or the listing read fails, and in every case
// there is no unarchive, no row and no send.
func TestBusinessResendUnloadLostArchiveReplyUnconfirmedHolds(t *testing.T) {
	for _, c := range []struct {
		name            string
		archivedListing string
	}{
		{"not-archived", ""},
		{"listing-incomplete", "incomplete"},
		{"listing-error", "error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, _, _ := businessResendKit(t)
			app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host, archiveErr: errors.New("thread/archive: host refused"), archivedListing: c.archivedListing}
			k.start.Adapter = app
			k.host.threads["t-1"].status = "idle"
			for range 2 {
				k.expect(k.run(), "incomplete", "recipient_not_idle", "")
			}
			if len(app.unarchiveCalls) != 0 || k.host.sent != 0 || len(k.host.sends) != 0 {
				t.Fatalf("unconfirmed lost reply unarchived or sent: %v sent=%d", app.unarchiveCalls, k.host.sent)
			}
			if n := businessResendLoweringRows(t, k); n != 0 {
				t.Fatalf("unconfirmed lost reply wrote %d unload rows", n)
			}
		})
	}
}

// A lost archive reply the host confirms, followed by two failed unarchives, answers
// lifecycle_unknown and records the archived thread for an operator, as the success path does.
func TestBusinessResendUnloadLostArchiveReplyUnarchiveErrorHolds(t *testing.T) {
	k, business, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host, archiveLostReply: true, unarchiveErr: errors.New("thread/unarchive: host refused")}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	k.expect(k.run(), "incomplete", "lifecycle_unknown", "")
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || len(app.unarchiveCalls) != 2 || k.host.sent != 0 || k.host.operations[businessResendID("managed-1", business, 1)] != nil {
		t.Fatalf("lost reply unarchive retry/withhold: archive=%v unarchive=%v sent=%d", app.archiveCalls, app.unarchiveCalls, k.host.sent)
	}
	detail := businessResendUnloadDetail(t, k)
	if !strings.Contains(detail, `"archive":"reply_lost"`) || !strings.Contains(detail, "thread/unarchive t-1") {
		t.Fatalf("lost reply unarchive failure detail: %s", detail)
	}
	if n := businessResendLoweringRows(t, k); n != 1 {
		t.Fatalf("unload journal rows: %d", n)
	}
}

// A cancellation that lands during the archive-reply check is reported as the context error, not
// as a hold: the check's outcome never overrides a cancelled context.
func TestBusinessResendUnloadLostArchiveReplyCancelledDuringCheck(t *testing.T) {
	k, _, _ := businessResendKit(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host, archiveErr: errors.New("thread/archive: cancelled")}
	app.onArchivedList = cancel
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	r := &startRun{m: k.start, task: "t-1", standby: "standby", businessAttempt: 1, resendFailure: businessResendLegacyReceipt("t-1"), identity: Identity{RequestID: "managed-1"}, ledger: k.host.ledger}
	if code, err := r.businessResendUnload(ctx); code != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reply check: %q %v", code, err)
	}
	if len(app.unarchiveCalls) != 0 {
		t.Fatalf("cancelled check unarchived: %v", app.unarchiveCalls)
	}
	if n := businessResendLoweringRows(t, k); n != 0 {
		t.Fatalf("cancelled check wrote %d unload rows", n)
	}
}

// A failure the host answered with its own JSON-RPC error response is this call being refused, not
// an uncertain outcome: another client may have archived the child first, so this invocation did
// not apply the archive. The archived listing is never asked, nothing is unarchived and no row is
// written, even when the listing would show the child archived.
func TestBusinessResendUnloadHostRefusedArchiveHolds(t *testing.T) {
	k, business, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host, archiveErr: &appserver.RPCError{Method: "thread/archive", Code: -32603, Message: "no rollout found"}}
	k.start.Adapter = app
	k.host.threads["t-1"].status = "idle"
	app.markArchived("t-1", true)
	for range 2 {
		k.expect(k.run(), "incomplete", "recipient_not_idle", "")
	}
	if len(app.archiveCalls) != 2 || len(app.unarchiveCalls) != 0 || k.host.sent != 0 || k.host.operations[businessResendID("managed-1", business, 1)] != nil {
		t.Fatalf("host-refused archive touched the thread: archive=%v unarchive=%v sent=%d", app.archiveCalls, app.unarchiveCalls, k.host.sent)
	}
	if app.listingCalls != 0 {
		t.Fatalf("host-refused archive asked the archived listing %d times", app.listingCalls)
	}
	if n := businessResendLoweringRows(t, k); n != 0 {
		t.Fatalf("host-refused archive wrote %d unload rows", n)
	}
}

// The unload archives only a child whose history is exactly its standby turn, so no sub-thread can
// exist to be carried into the archive. This is the constraint the gate pins: a history with any
// other turn never reaches thread/archive. Widening the precondition beyond the standby-only
// history would need the childcleanup sub-thread check before the archive; nothing here inspects
// sub-threads today.
func TestBusinessResendUnloadNeverArchivesNonStandbyHistory(t *testing.T) {
	k, _, _ := businessResendKit(t)
	app := &businessResendUnloadApp{Adapter: k.start.Adapter, host: k.host}
	k.start.Adapter = app
	k.host.threads["t-1"].turns = []string{"standby", "other"}
	k.host.threads["t-1"].status = "idle"
	for range 2 {
		k.expect(k.run(), "refused", "business_identity_unobserved", "")
	}
	if len(app.archiveCalls) != 0 || len(app.unarchiveCalls) != 0 || k.host.sent != 0 {
		t.Fatalf("non-standby history reached the archive: %v %v sent=%d", app.archiveCalls, app.unarchiveCalls, k.host.sent)
	}
	if n := businessResendLoweringRows(t, k); n != 0 {
		t.Fatalf("non-standby history wrote %d unload rows", n)
	}
}
