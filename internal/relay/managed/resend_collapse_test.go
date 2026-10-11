package managed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// collapseApp is the scripted App Server with the pieces the orphan archive needs: a set of threads the host
// holds under another model (what the host reports for a thread a client loaded with its own settings), an
// archive that can answer, be refused or lose its reply, a second client that unarchives and reloads, and an
// archived listing a test can make miss. Every host call is recorded.
type collapseApp struct {
	*scriptedApp
	wrong    map[string]bool
	archived map[string]bool
	// archiveLostReply: the archive applies and its reply is lost (a transport error). archiveRefused: the host
	// answers with an error and archives nothing.
	archiveLostReply bool
	archiveRefused   bool
	// onArchive runs after the archive applied and before it answers: the window in which another client acts.
	onArchive         func()
	onArchivedList    func()
	archiveCalls      []string
	unarchiveCalls    []string
	archivedListCalls int
	createErr         error
	creates           int
	createStatus      string // the load status a created thread has once its standby turn has ended; idle when empty
}

func collapseKit(t *testing.T, wrong ...string) (*reconcileKit, *collapseApp) {
	t.Helper()
	k := newReconcileKit(t)
	k.host.initialTurns = []string{"standby"}
	app := &collapseApp{scriptedApp: k.host, wrong: map[string]bool{}, archived: map[string]bool{}}
	for _, id := range wrong {
		app.wrong[id] = true
	}
	k.start.Adapter = app
	return k, app
}

func (a *collapseApp) CreateThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	a.creates++
	if a.createErr != nil && a.creates > 1 {
		return nil, a.createErr
	}
	receipt, err := a.scriptedApp.CreateThread(ctx, in)
	if err == nil && a.createStatus != "" {
		a.threads[pyjson.Text(receipt["threadId"])].status = a.createStatus
	}
	return receipt, err
}

// otherClientReloads is another client unarchiving the thread and loading it again: it is idle and loaded, with
// the settings that client gave it.
func (a *collapseApp) otherClientReloads(id string) {
	delete(a.archived, id)
	a.threads[id].status = "idle"
	a.wrong[id] = true
}

func (a *collapseApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	id := pyjson.Text(params["threadId"])
	switch method {
	case "thread/archive":
		a.archiveCalls = append(a.archiveCalls, id)
		if a.archiveRefused {
			return nil, &appserver.RPCError{Method: method, Message: "refused"}
		}
		a.archived[id] = true
		a.threads[id].status = "notLoaded"
		if a.onArchive != nil {
			a.onArchive()
		}
		if a.archiveLostReply {
			return nil, errors.New("connection reset by peer")
		}
		return map[string]any{}, nil
	case "thread/unarchive":
		a.unarchiveCalls = append(a.unarchiveCalls, id)
		delete(a.archived, id)
		return map[string]any{}, nil
	case "thread/list":
		if params["archived"] == true {
			a.archivedListCalls++
			if a.onArchivedList != nil {
				a.onArchivedList()
			}
			data := []any{}
			for _, known := range a.order {
				if a.archived[known] {
					data = append(data, map[string]any{"id": known})
				}
			}
			return map[string]any{"data": data}, nil
		}
	case "thread/read":
		answer, err := a.scriptedApp.HostCall(ctx, method, params)
		if err == nil && a.wrong[id] {
			thread := answer["thread"].(map[string]any)
			thread["model"] = "another-model"
		}
		return answer, err
	}
	return a.scriptedApp.HostCall(ctx, method, params)
}

func (k *reconcileKit) orphanRows(kind string) []string {
	k.t.Helper()
	rows, err := k.start.Store.DB.Query("SELECT detail FROM journal WHERE kind=? ORDER BY seq", kind)
	if err != nil {
		k.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			k.t.Fatal(err)
		}
		out = append(out, detail)
	}
	return out
}

// A child the host holds under settings other than the request's, with only its standby turn, is archived once
// and the request creates its child again under the same request id; the new child is the one registered.
func TestStandbyOnlyWrongSettingsRecreated(t *testing.T) {
	t.Parallel()
	t.Run("archived and created again", func(t *testing.T) {
		t.Parallel()
		k, app := collapseKit(t, "t-1")
		got := k.run()
		k.expect(got, "admitted", "", "recreated")
		if got["childTaskId"] != "t-2" || !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || len(app.unarchiveCalls) != 0 {
			t.Fatalf("t-1 archived once, t-2 registered: child %v archive %v unarchive %v", got["childTaskId"], app.archiveCalls, app.unarchiveCalls)
		}
		if len(k.host.creates) != 2 || k.host.creates[0] == k.host.creates[1] || !strings.HasPrefix(k.host.creates[1], "managed-create-") {
			t.Fatalf("the second creation is a new operation of the same request: %v", k.host.creates)
		}
		if recon(got)["attempt"] != 1 || got["requestId"] != "managed-1" {
			t.Fatalf("attempt 1 of the same request: %v", recon(got))
		}
		k.effects(2, 1)
		if rows := k.orphanRows("managed_standby_orphan"); len(rows) != 2 || !strings.Contains(rows[0], `"archive":"attempting"`) || !strings.Contains(rows[1], `"archive":"ok"`) {
			t.Fatalf("a mark before the archive and its result after: %v", rows)
		}
		// a repeat changes nothing
		again := k.run()
		k.expect(again, "admitted", "", "")
		if again["childTaskId"] != "t-2" || len(app.archiveCalls) != 1 {
			t.Fatalf("repeat: child %v archives %v", again["childTaskId"], app.archiveCalls)
		}
		k.effects(2, 1)
	})
	t.Run("the host refuses the archive", func(t *testing.T) {
		t.Parallel()
		k, app := collapseKit(t, "t-1")
		app.archiveRefused = true
		got := k.run()
		k.expect(got, "admitted", "", "recreated")
		if got["childTaskId"] != "t-2" || len(app.archiveCalls) != 1 {
			t.Fatalf("a refusal is certain: the orphan stays, the child is created again: %v %v", got["childTaskId"], app.archiveCalls)
		}
	})
	t.Run("a crash between the archive and the next creation", func(t *testing.T) {
		t.Parallel()
		k, app := collapseKit(t, "t-1")
		app.createErr = errors.New("host unavailable")
		if _, err := k.runErr(); err == nil {
			t.Fatal("the second creation failed with the host's error")
		}
		app.createErr = nil
		got := k.run()
		k.expect(got, "admitted", "", "")
		if got["childTaskId"] != "t-2" || len(app.archiveCalls) != 1 {
			t.Fatalf("the repeat creates the next attempt and archives nothing again: %v %v", got["childTaskId"], app.archiveCalls)
		}
	})
	t.Run("the creations are bounded", func(t *testing.T) {
		t.Parallel()
		k, app := collapseKit(t, "t-1", "t-2", "t-3")
		got := k.run()
		k.expect(got, "incomplete", "creation_unknown", "attempts_exhausted")
		if app.creates != maxCreationAttempts || !reflect.DeepEqual(app.archiveCalls, []string{"t-1", "t-2"}) || k.host.sent != 0 {
			t.Fatalf("three creations, two archives, nothing sent: creates %d archives %v sent %d", app.creates, app.archiveCalls, k.host.sent)
		}
		k.expect(k.run(), "incomplete", "creation_unknown", "attempts_exhausted")
		if app.creates != maxCreationAttempts || len(app.archiveCalls) != 2 {
			t.Fatalf("a repeat makes no further creation or archive: %d %v", app.creates, app.archiveCalls)
		}
	})
	t.Run("settings as asked, or not loaded, are left alone", func(t *testing.T) {
		t.Parallel()
		k, app := collapseKit(t)
		k.expect(k.run(), "admitted", "", "")
		if len(app.archiveCalls) != 0 || app.creates != 1 {
			t.Fatalf("a child held under the settings asked for is not an orphan: %v", app.archiveCalls)
		}
		k, app = collapseKit(t, "t-1")
		app.createStatus = "notLoaded"
		k.expect(k.run(), "admitted", "", "")
		if len(app.archiveCalls) != 0 || app.creates != 1 {
			t.Fatalf("an unloaded child is resumed under the recorded settings, not replaced: %v", app.archiveCalls)
		}
	})
}

// A child whose history shows more than its standby turn is somebody's work: it is neither archived nor
// replaced, and the request is held.
func TestNonStandbyHistoryHeld(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name   string
		turns  []string
		state  string
		reason string
	}{
		{"another turn", []string{"standby", "other"}, "refused", "business_identity_unobserved"},
		{"another turn first", []string{"other", "standby"}, "refused", "business_identity_unobserved"},
		{"no turn listed", nil, "incomplete", "lifecycle_unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			k, app := collapseKit(t, "t-1")
			k.host.initialTurns = scenario.turns
			for range 2 {
				got := k.run()
				if got["state"] != scenario.state || got["stage"] != "standby" || got["reason"] != scenario.reason || recon(got)["state"] != "standby_orphan_held" {
					t.Fatalf("held: %v/%v/%v %v", got["state"], got["stage"], got["reason"], recon(got))
				}
			}
			if len(app.archiveCalls) != 0 || app.creates != 1 || k.host.sent != 0 {
				t.Fatalf("nothing archived, created or sent: archives %v creates %d sent %d", app.archiveCalls, app.creates, k.host.sent)
			}
		})
	}
}

// restart is a new engine over the same store and host: nothing but the journal and the ledger is shared.
func (k *reconcileKit) restart() {
	k.t.Helper()
	fresh := *k.start
	k.start = &fresh
}

// CRW-1070: the archive's reply is lost, another client unarchives and loads the child again, and the archived
// listing misses it. The archive is not asked a second time, in the same call, in a repeat of the request or in
// a process that starts over.
func TestCRW1070RaceArchivedAtMostOnce(t *testing.T) {
	t.Parallel()
	k, app := collapseKit(t, "t-1")
	app.archiveLostReply = true
	app.onArchive = func() { app.otherClientReloads("t-1") } // the archive applied, then the other client acted, then the reply was lost
	app.onArchivedList = func() { t.Error("the archived listing is not the evidence of an archive") }
	for round := range 3 {
		got := k.run()
		if got["state"] != "incomplete" || got["stage"] != "standby" || got["reason"] != "lifecycle_unknown" || recon(got)["state"] != "standby_orphan_held" {
			t.Fatalf("round %d: held: %v/%v/%v %v", round, got["state"], got["stage"], got["reason"], recon(got))
		}
		if round == 1 {
			k.restart()
		}
	}
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || app.archivedListCalls != 0 || app.creates != 1 || k.host.sent != 0 {
		t.Fatalf("one archive of t-1, no listing, no second creation, no send: archives %v lists %d creates %d sent %d", app.archiveCalls, app.archivedListCalls, app.creates, k.host.sent)
	}
}

// CRW-1070: a request whose archive reply was lost is held and retried without limit by its caller; the retries end
// at the one mark, and an uncertain archive is never run again.
func TestCRW1070LostArchiveReplyRetryBounded(t *testing.T) {
	t.Parallel()
	k, app := collapseKit(t, "t-1")
	app.archiveLostReply = true
	for round := range 6 {
		got := k.run()
		if got["state"] != "incomplete" || got["reason"] != "lifecycle_unknown" {
			t.Fatalf("round %d: %v/%v", round, got["state"], got["reason"])
		}
		if round == 3 {
			k.restart()
		}
	}
	rows := k.orphanRows("managed_standby_orphan")
	if len(app.archiveCalls) != 1 || len(rows) != 1 || !strings.Contains(rows[0], `"archive":"attempting"`) {
		t.Fatalf("one archive and one mark with no result across six calls: %v %v", app.archiveCalls, rows)
	}
	if app.creates != 1 || k.host.sent != 0 || len(app.unarchiveCalls) != 0 {
		t.Fatalf("nothing created, sent or unarchived: %d %d %v", app.creates, k.host.sent, app.unarchiveCalls)
	}
}

// CRW-1070: another client unarchives and loads a standby-only idle child between two calls of the request; the
// listing of archived threads misses it; the child that was archived once is not archived again.
func TestCRW1070OtherClientUnarchiveRace(t *testing.T) {
	t.Parallel()
	k, app := collapseKit(t, "t-1")
	app.archiveLostReply = true
	app.onArchivedList = func() { t.Error("the archived listing is not the evidence of an archive") }
	k.expect(k.run(), "incomplete", "lifecycle_unknown", "standby_orphan_held")
	app.otherClientReloads("t-1") // unarchived and loaded again, idle, with its other settings; the archived listing no longer holds it
	for round := range 3 {
		k.expect(k.run(), "incomplete", "lifecycle_unknown", "standby_orphan_held")
		if round == 0 {
			k.restart()
		}
	}
	if !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) || app.archivedListCalls != 0 {
		t.Fatalf("the same thread is archived once: %v lists %d", app.archiveCalls, app.archivedListCalls)
	}
}

// legacyKit is a request that the earlier unload-and-resend recovery worked on: its business send failed with
// the structured settings refusal before turn/start, and the child stands in the host.
func legacyKit(t *testing.T, begin, closing bool, status string) (*reconcileKit, *collapseApp, string) {
	t.Helper()
	k, business, _ := businessResendKit(t)
	if _, err := k.start.Store.DB.Exec("DELETE FROM journal WHERE kind='managed_resend_unloaded'"); err != nil {
		t.Fatal(err)
	}
	if begin {
		k.seedUnloadRows(1, closing)
	}
	k.host.threads["t-1"].status = status
	app := &collapseApp{scriptedApp: k.host, wrong: map[string]bool{}, archived: map[string]bool{}}
	k.start.Adapter = &businessResendReplayApp{Adapter: app, host: k.host}
	return k, app, business
}

// A begin row with no closing row is an archive that may or may not have happened: the request is held exactly
// as it always was, and nothing is archived, unarchived or sent.
func TestLegacyBeginRowWithoutCloseStaysHeld(t *testing.T) {
	t.Parallel()
	k, app, business := legacyKit(t, true, false, "idle")
	for range 3 {
		k.expect(k.run(), "incomplete", "recipient_not_idle", "")
	}
	if len(app.archiveCalls) != 0 || len(app.unarchiveCalls) != 0 || k.host.sent != 0 || len(k.host.sends) != 0 {
		t.Fatalf("no archive, no successor: %v %v %d %v", app.archiveCalls, app.unarchiveCalls, k.host.sent, k.host.sends)
	}
	if k.host.operations[legacyResendID("managed-1", business, 1)] != nil {
		t.Fatal("a successor operation was made")
	}
	if rows := k.orphanRows("managed_resend_unloaded"); len(rows) != 1 {
		t.Fatalf("the begin row is read, never added to: %v", rows)
	}
}

// A closing row is replayed as recorded: the child the earlier recovery lowered and the host has unloaded is sent
// to under the successor operation, once, and a closing row whose child is still loaded stays held.
func TestLegacyClosingRowReplays(t *testing.T) {
	t.Parallel()
	t.Run("unloaded", func(t *testing.T) {
		t.Parallel()
		k, app, business := legacyKit(t, true, true, "notLoaded")
		first := k.run()
		k.expect(first, "admitted", "", "")
		if first["businessRequestId"] != business || len(k.host.sends) != 1 || k.host.sends[0] != legacyResendID("managed-1", business, 1) {
			t.Fatalf("one send under the first successor: %v", k.host.sends)
		}
		for range 2 {
			if got := k.run(); !reflect.DeepEqual(got, first) {
				t.Fatalf("a replay answers the same receipt: %v", got)
			}
		}
		if len(app.archiveCalls)+len(app.unarchiveCalls) != 0 || k.host.sent != 1 {
			t.Fatalf("nothing archived, one business turn: %v %v %d", app.archiveCalls, app.unarchiveCalls, k.host.sent)
		}
		if rows := k.orphanRows("managed_resend_unloaded"); len(rows) != 2 {
			t.Fatalf("the rows are read, never added to: %v", rows)
		}
	})
	t.Run("still loaded", func(t *testing.T) {
		t.Parallel()
		k, app, _ := legacyKit(t, true, true, "idle")
		for range 2 {
			k.expect(k.run(), "incomplete", "recipient_not_idle", "")
		}
		if len(app.archiveCalls) != 0 || k.host.sent != 0 {
			t.Fatalf("a loaded child is not archived again: %v %d", app.archiveCalls, k.host.sent)
		}
	})
}

// The request, dispatch, relationship and generation identity of a request that holds unload rows is the one it
// registered with: the successor operation changes none of them.
func TestIdentityPreservedAcrossCollapse(t *testing.T) {
	t.Parallel()
	k, _, business := legacyKit(t, true, true, "notLoaded")
	var relationship, dispatch string
	var generation int
	identity := func() {
		t.Helper()
		if err := k.start.Store.DB.QueryRow("SELECT r.relationship_id, g.dispatch_request_id, g.execution_generation FROM relationships r JOIN generations g ON g.relationship_id=r.relationship_id").Scan(&relationship, &dispatch, &generation); err != nil {
			t.Fatal(err)
		}
	}
	identity()
	registered := relationship
	first := k.run()
	k.expect(first, "admitted", "", "")
	identity()
	if relationship != registered || dispatch != business || generation != 1 || first["relationshipId"] != registered || first["businessRequestId"] != business || first["requestId"] != "managed-1" || first["childTaskId"] != "t-1" {
		t.Fatalf("identity changed: relationship %s (was %s) dispatch %s generation %d receipt %v", relationship, registered, dispatch, generation, first["relationshipId"])
	}
	var relationships int
	if err := k.start.Store.DB.QueryRow("SELECT COUNT(*) FROM relationships").Scan(&relationships); err != nil || relationships != 1 {
		t.Fatalf("one relationship: %d %v", relationships, err)
	}
}

// The unload-and-resend path is gone: the source holds no business resend function and writes no unload row, and a
// pre-turn settings refusal of a request with no unload rows is not sent again, archived or unarchived.
func TestNoBusinessResendPathRemains(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(source)
		for _, gone := range []string{"businessResend", "resendUnloadLowered", "recordResendUnload"} {
			if strings.Contains(text, gone) {
				t.Errorf("%s still holds %s", name, gone)
			}
		}
		if strings.Contains(text, "INSERT INTO journal") && strings.Contains(text, `"managed_resend_unloaded"`) {
			t.Errorf("%s writes managed_resend_unloaded rows", name)
		}
	}
	if _, err := os.Stat("business_resend.go"); err == nil {
		t.Error("business_resend.go is still there")
	}
	k, business, _ := businessResendKit(t)
	if _, err := k.start.Store.DB.Exec("DELETE FROM journal WHERE kind='managed_resend_unloaded'"); err != nil {
		t.Fatal(err)
	}
	app := &collapseApp{scriptedApp: k.host, wrong: map[string]bool{}, archived: map[string]bool{}}
	k.start.Adapter = &businessResendReplayApp{Adapter: app, host: k.host}
	for _, status := range []string{"idle", "notLoaded"} {
		k.host.threads["t-1"].status = status
		for range 2 {
			k.expect(k.run(), "incomplete", "business_failed", "")
		}
	}
	if len(app.archiveCalls)+len(app.unarchiveCalls) != 0 || k.host.sent != 0 || k.host.operations[legacyResendID("managed-1", business, 1)] != nil {
		t.Fatalf("a failed business send is kept: archives %v successor %v", app.archiveCalls, k.host.operations[legacyResendID("managed-1", business, 1)])
	}
}
