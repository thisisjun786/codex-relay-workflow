package faults

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// CRW-1476 (bundle n2-layer-words): the text the fault ledger and the notice deliverer write for a
// person names the layer (부모, 자식), never a relay role id written as a bare word. The relay role
// id `supervisor` is the 부모 and `parent` is the 자식
// (plugins/crw/skills/crw-canon/references/hierarchy.md "The levels"). Each text below is built by
// the package itself, and layername.Find (the matcher the ci ratchet shares) must find no bare role
// word in it. Command names, owners, table names and refusal codes stay as they are.

func requireNoRoleWords(t *testing.T, what, text string) {
	t.Helper()
	if text == "" {
		t.Errorf("%s: nothing rendered", what)
		return
	}
	if hits := layername.Find(text); len(hits) > 0 {
		var words []string
		for _, h := range hits {
			start, end := max(0, h.Offset-40), min(len(text), h.Offset+len(h.Word)+40)
			words = append(words, h.Word+" in "+`"`+text[start:end]+`"`)
		}
		t.Errorf("%s names a role id where a person reads a layer (write 부모 / 자식): %s", what, strings.Join(words, "; "))
	}
}

// requireNames fails when text does not hold want.
func requireNames(t *testing.T, what, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("%s %q does not hold %q", what, text, want)
	}
}

// unobservedChannel is the 부모 channel as the deliverer meets it when the relay parent a notice is
// owed through cannot be observed: every measurement fails.
type unobservedChannel struct{ *parkRecorder }

func (unobservedChannel) Measure(context.Context, string) error {
	return &NoticeError{Kind: "host_unreachable", Detail: "the host did not answer"}
}

func TestFaultNoticeTextNamesTheLayers(t *testing.T) {
	t.Parallel()
	const (
		fault        = "INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at) VALUES ('abc123','crw','report_omitted','c','broken','{}','{\"projectKey\": \"P\"}','P','open','t','t','t')"
		notification = "INSERT INTO fault_notifications (notification_id, fault_id, product, kind, cycle, state, created_at, updated_at) VALUES ('n1','abc123','crw','blocking',1,'pending','t','t')"
	)
	seed := func(t *testing.T, statements ...string) (*Ledger, context.Context) {
		t.Helper()
		l, ctx := testLedger(t)
		for _, statement := range statements {
			if _, err := l.Store.Q(ctx).ExecContext(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		return l, ctx
	}
	live := map[string]any{"sender": "parent", "recipient": "supervisor", "projectKey": "P"}

	t.Run("the notice's own message, held or rechecked by the 부모 channel", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			name, assignment, want string
		}{
			{"held", "hold_reason='held'", " is held by the 부모 channel: held"},
			{"rechecked later", "next_eligible_at=100100", "the 부모 channel rechecks the level above at "},
		} {
			l, ctx := seed(t, fault, notification)
			seedNoticeMessage(t, ctx, l.Store, "m1", "supervisor", "queued", "2023-11-14T22:13:20.000000+00:00", "obligation_kind='fault_notification'", "obligation_id='n1'", "project_key='P'", c.assignment)
			d := &NoticeDeliverer{Ledger: l, Channel: resolveOnly{live: live}}
			reason := d.WaitingFor(ctx, map[string]any{"notificationId": "n1", "faultId": "abc123"}, 100000)
			requireNames(t, "the waiting reason ("+c.name+")", reason, c.want)
			requireNoRoleWords(t, "the waiting reason ("+c.name+")", reason)
		}
	})

	t.Run("the 자식 a notice is owed through could not be observed", func(t *testing.T) {
		t.Parallel()
		l, ctx := seed(t,
			"INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-1','ISSUE-1','active','01parent','host','01child','host',1,'[]','[]','t','t')",
			"INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at) VALUES ('abc123','crw','report_omitted','c','broken','{\"relationship\": \"rel-1\"}','{}','','open','t','t','t')",
			notification)
		d := &NoticeDeliverer{Ledger: l, Channel: unobservedChannel{&parkRecorder{t: t, l: l}}, Owner: "relay-daemon"}
		answer, err := d.Tick(ctx, 100000, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(answer.Waiting) != 1 {
			t.Fatalf("waiting %v: want the one notice waiting on its 자식", answer.Waiting)
		}
		reason := answer.Waiting[0][1]
		requireNames(t, "the waiting reason", reason, "its 자식 01parent could not be observed (host_unreachable)")
		requireNoRoleWords(t, "the waiting reason", reason)
	})

	t.Run("a deliverer's own message, its reference", func(t *testing.T) {
		t.Parallel()
		l, ctx := seed(t, fault, notification)
		seedNoticeMessage(t, ctx, l.Store, "m1", "supervisor", "dispatched", "2023-11-14T22:13:20.000000+00:00", "obligation_kind='fault_notification'", "obligation_id='n1'")
		d := &NoticeDeliverer{Ledger: l}
		r, err := d.get(ctx, "m1")
		if err != nil {
			t.Fatal(err)
		}
		ref, err := d.ref(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if ref != "부모 message m1 dispatched" {
			t.Errorf("the reference %q, want %q", ref, "부모 message m1 dispatched")
		}
		requireNoRoleWords(t, "the reference", ref)
	})

	t.Run("the daemon's reservations and their settlement", func(t *testing.T) {
		t.Parallel()
		l, ctx := seed(t, fault)
		_, err := executeD(ctx, l, "fault-notification-reserve", map[string]string{"--owner": "relay-daemon"})
		requireNames(t, "the reserve refusal", refusalText(err), "settled from the 부모 channel's records")
		requireNoRoleWords(t, "the reserve refusal", refusalText(err))

		l, ctx = seed(t, fault, "INSERT INTO fault_notifications (notification_id, fault_id, product, kind, cycle, state, owner, created_at, updated_at) VALUES ('n1','abc123','crw','blocking',1,'uncertain','relay-daemon','t','t')")
		_, err = executeD(ctx, l, "fault-notification-reconcile", map[string]string{"--notification": "n1", "--delivered": "yes", "--ref": "somebody says it arrived"})
		requireNames(t, "the reconcile refusal", refusalText(err), "whose transport is the 부모 channel")
		requireNames(t, "the reconcile refusal", refusalText(err), "crw relay supervisor-read")
		requireNoRoleWords(t, "the reconcile refusal", refusalText(err))

		seedNoticeMessage(t, ctx, l.Store, "m1", "supervisor", "sending", "2023-11-14T22:13:20.000000+00:00", "obligation_kind='fault_notification'", "obligation_id='n1'")
		if _, err := l.Store.Q(ctx).ExecContext(ctx, "INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, transport_started_at, observed_at) VALUES ('sup-m1-a1','m1',1,'m','settled','yes',0,'{}','t','t','t')"); err != nil {
			t.Fatal(err)
		}
		_, err = executeD(ctx, l, "fault-notification-reconcile", map[string]string{"--notification": "n1", "--delivered": "no", "--ref": "not sent"})
		requireNames(t, "the not-delivered refusal", refusalText(err), "fault_state_conflict: cannot mark this notification as not delivered because 부모 channel attempt sup-m1-a1 may have sent")
		requireNoRoleWords(t, "the not-delivered refusal", refusalText(err))
	})

	t.Run("no contact reading", func(t *testing.T) {
		requireNames(t, "the missing reading", errNoContacts.Error(), "no 부모 contact reading is installed")
		requireNoRoleWords(t, "the missing reading", errNoContacts.Error())
	})
}

// TestFaultNoticeIDsIgnoreWording (plan D8): a fault's id and its notifications' ids hash the
// product, class, signature, kind, cycle and reason, never the human text, so rewording what the
// ledger and the deliverer write moves no id. The hex literals were taken at dev 7f21f0069, before
// the rewording; this test is green before and after it.
func TestFaultNoticeIDsIgnoreWording(t *testing.T) {
	t.Parallel()
	const (
		wantFault    = "4c4ef0d33eaf63cf1af8cb7c937577e0"
		wantBlocking = "95b9b5faf7311d9af7d713187053e502"
		wantDecision = "4b7db742593fd828ba55ca107528b6fc"
	)
	check := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %q, want %q (an id moved with the wording)", what, got, want)
		}
	}
	l, ctx := testLedger(t)
	if recorded, err := l.Record(ctx, testObservation("k1", "report_omitted", Broken)); err != nil || !recorded {
		t.Fatalf("record: %v %v", recorded, err)
	}
	r, err := l.Store.One(ctx, "SELECT fault_id FROM fault_ledger")
	if err != nil {
		t.Fatal(err)
	}
	id := r.Text("fault_id")
	check("fault_id", id, wantFault)
	blocking, err := l.Store.One(ctx, "SELECT notification_id FROM fault_notifications WHERE fault_id=? AND kind='blocking'", id)
	if err != nil || blocking == nil {
		t.Fatalf("blocking notification %v %v", blocking, err)
	}
	check("blocking notification_id", blocking.Text("notification_id"), wantBlocking)
	if _, err := executeD(ctx, l, "fault-notification-raise", map[string]string{"--fault": id, "--reason": "the user owes a decision"}); err != nil {
		t.Fatal(err)
	}
	decision, err := l.Store.One(ctx, "SELECT notification_id FROM fault_notifications WHERE fault_id=? AND kind='decision'", id)
	if err != nil || decision == nil {
		t.Fatalf("decision notification %v %v", decision, err)
	}
	check("decision notification_id", decision.Text("notification_id"), wantDecision)
}

// CRW-1554 (follow-up of CRW-1448): the clears text of a fault class is a contract text (contract/schema/fault-kinds.json
// holds the same words, and Test22_FLT_2_Registry requires the two to be equal), and the publication summary of every
// fault carries it as "clears when: ...". It names the layer (부모), never the relay role id `supervisor` as a bare word,
// in the declared class and in the contract file alike. integration_target_moved says the blocking notice is not withdrawn
// before the 부모 is told.
func TestClearsTextNamesTheLayers(t *testing.T) {
	t.Parallel()
	for name, policy := range declaredClasses {
		requireNoRoleWords(t, "the clears text of "+name, policy.clears)
	}
	requireNames(t, "the clears text of integration_target_moved", declaredClasses["integration_target_moved"].clears, "before the 부모 is told")
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contract", "schema", "fault-kinds.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Classes map[string]struct{ Clears string }
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for name, policy := range manifest.Classes {
		requireNoRoleWords(t, "fault-kinds.json clears of "+name, policy.Clears)
	}
	requireNames(t, "fault-kinds.json clears of integration_target_moved", manifest.Classes["integration_target_moved"].Clears, "before the 부모 is told")
}

// A publication row keeps the summary it was written with: the clears text is rendered into the row when the publication is
// created, and the row's identity (publication id, identity digest) and its block digest (summarySha256 of the stored summary)
// never read the declared class again. A row written under the old words reads with them, and creating the publication
// again leaves it alone; only a new publication carries the new words.
func TestAPublicationRowKeepsItsOldClearsText(t *testing.T) {
	l, ctx := testLedger(t)
	if err := RegisterClass("integration_target_moved", 0); err != nil {
		t.Fatal(err)
	}
	o := testObservation("moved-1", "integration_target_moved", Broken)
	record(t, l, ctx, o)
	row, err := l.Store.One(ctx, "SELECT publication_id, fault_id, kind, trigger_key, identity_digest, summary FROM fault_publications ORDER BY created_at LIMIT 1")
	if err != nil || row == nil {
		t.Fatalf("no publication for the broken fault: %v", err)
	}
	summary := row.Text("summary")
	requireNames(t, "a new publication", summary, "clears when: ")
	requireNames(t, "a new publication", summary, "before the 부모 is told")
	requireNoRoleWords(t, "a new publication", summary)

	oldSummary := strings.Replace(summary, "before the 부모 is told", "before the supervisor is told", 1)
	if oldSummary == summary {
		t.Fatal("the fixture holds no clears text to age")
	}
	if _, err := l.Store.Q(ctx).ExecContext(ctx, "UPDATE fault_publications SET summary = ? WHERE publication_id = ?", oldSummary, row.Text("publication_id")); err != nil {
		t.Fatal(err)
	}
	fault, err := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id = ?", row.Text("fault_id"))
	if err != nil || fault == nil {
		t.Fatalf("no fault row: %v", err)
	}
	if err := l.insertPublication(ctx, row.Text("fault_id"), row.Text("kind"), row.Text("trigger_key"), l.Clock.ISO(), declaredClasses["integration_target_moved"].clears); err != nil {
		t.Fatal(err)
	}
	after, err := l.Store.One(ctx, "SELECT summary, identity_digest FROM fault_publications WHERE publication_id = ?", row.Text("publication_id"))
	if err != nil || after == nil {
		t.Fatalf("the row is gone: %v", err)
	}
	if after.Text("summary") != oldSummary {
		t.Errorf("a publication row written under the old words was rewritten:\n%s", after.Text("summary"))
	}
	if after.Text("identity_digest") != row.Text("identity_digest") {
		t.Errorf("the identity digest moved with the clears text: %s then %s", row.Text("identity_digest"), after.Text("identity_digest"))
	}
	if identityDigest(row.Text("fault_id"), row.Text("kind"), row.Text("trigger_key"), integer(fault, "cycle")) != row.Text("identity_digest") {
		t.Error("the identity digest reads something besides the fault, the kind, the trigger and the cycle")
	}
}
