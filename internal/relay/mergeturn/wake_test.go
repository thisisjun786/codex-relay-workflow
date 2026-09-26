package mergeturn

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// grantFixture is a promotion through the turn's own authorized assignment.
func grantFixture(t *testing.T) (*fx, string, string) {
	t.Helper()
	w := newFx(t)
	ctx := context.Background()
	_, err := w.s.Querier(ctx).ExecContext(ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, "rel-a", "ISS-1", "active", alpha.TaskID, alpha.HostID, "task-child", "host-child", 3, "[]", `["task-alpha"]`, fxISO, fxISO)
	if err != nil {
		t.Fatal(err)
	}
	w.m.Delivery = StoreDelivery{Store: w.s}
	return w, "", "rel-a"
}

func queueFixture(t *testing.T) (*fx, StoreDelivery, string) {
	t.Helper()
	w, _, relationship := grantFixture(t)
	return w, StoreDelivery{Store: w.s}, relationship
}
func queueGrant(t *testing.T, w *fx, d StoreDelivery, relationship, grant string) string {
	t.Helper()
	id := GrantEventID(relationship, grant)
	err := w.s.Transaction(w.ctx, func(ctx context.Context, _ *sql.Conn) error {
		return d.Queue(ctx, id, relationship, alpha.TaskID, `{"kind":"merge_turn_grant","grantId":"`+grant+`"}`, grant, fxISO)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func queuedRows(t *testing.T, w *fx, id string) (store.Row, store.Row) {
	t.Helper()
	e, err := w.s.One(w.ctx, "SELECT * FROM events WHERE event_id=?", id)
	if err != nil {
		t.Fatal(err)
	}
	d, err := w.s.One(w.ctx, "SELECT * FROM deliveries WHERE event_id=?", id)
	if err != nil {
		t.Fatal(err)
	}
	return e, d
}
func Test26_queue_one_notice_per_grant_identity(t *testing.T) {
	w, d, r := queueFixture(t)
	a := queueGrant(t, w, d, r, "grant-1")
	queueGrant(t, w, d, r, "grant-1")
	b := queueGrant(t, w, d, r, "grant-2")
	if a == b {
		t.Fatal("grant identity collapsed")
	}
	rows, err := w.s.All(w.ctx, "SELECT event_id FROM deliveries ORDER BY event_id")
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, id := range []string{a, b} {
		e, delivery := queuedRows(t, w, id)
		if e == nil || delivery == nil || delivery.Get("state") != "queued" {
			t.Fatal(e, delivery)
		}
	}
}
func Test26_MTW_2_only_promotions_queue_a_wake(t *testing.T) {
	w := newFx(t)
	w.m.Delivery = StoreDelivery{Store: w.s}
	a := w.held()
	w.must(w.claimOn(beta, fxB, "head-b", fxBase, false))
	w.must(w.m.Release(w.ctx, a, alpha.TaskID, "returned", "done", ""))
	rows, err := w.s.All(w.ctx, "SELECT * FROM deliveries WHERE kind='merge_turn_grant'")
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}

func Test26_MTW_1_promotion_queues_exactly_one_wake(t *testing.T) {
	w, _, relationship := grantFixture(t)
	holder := w.held()
	// A second project has its own parent; the waiter's address is its own assignment.
	_, err := w.s.Querier(w.ctx).ExecContext(w.ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, "rel-b", "ISS-2", "active", beta.TaskID, beta.HostID, "child-b", "child-host", 2, "[]", `["task-beta"]`, fxISO, fxISO)
	if err != nil {
		t.Fatal(err)
	}
	waiter := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxB, beta.TaskID, beta.HostID, "head-b", true, ClaimOptions{Relationship: sql.NullString{String: "rel-b", Valid: true}}))
	result := w.must(w.m.Release(w.ctx, holder, alpha.TaskID, "returned", "done", ""))
	promoted := result["promoted"].(map[string]any)
	grant := promoted["grant"].(map[string]any)
	wake := grant["wake"].(map[string]any)
	eventID := GrantEventID("rel-b", grant["grantId"].(string))
	if wake["eventId"] != eventID || promoted["turnId"] != waiter["turnId"] {
		t.Fatal(result)
	}
	e, d := queuedRows(t, w, eventID)
	if e == nil || d == nil || e.Get("receipt") == nil {
		t.Fatal(e, d)
	}
	rows, err := w.s.All(w.ctx, "SELECT * FROM deliveries WHERE kind='merge_turn_grant'")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	_ = relationship
}
func Test26_queue_notice_is_a_final_relay_owned_delivery(t *testing.T) {
	w, d, r := queueFixture(t)
	id := queueGrant(t, w, d, r, "grant-1")
	event, delivery := queuedRows(t, w, id)
	if event.Get("stage") != "final" || event.Get("outcome") != "merge_turn_grant" || event.Get("producer") != "relay" || event.Get("revision_hash") != strings.Repeat("0", 64) || event.Get("execution_generation") != int64(3) || delivery.Get("recipient_thread_id") != alpha.TaskID {
		t.Fatal(event, delivery)
	}
}
func Test26_queue_notice_carries_its_own_grant_receipt(t *testing.T) {
	w, d, r := queueFixture(t)
	id := queueGrant(t, w, d, r, "grant-1")
	event, _ := queuedRows(t, w, id)
	var got map[string]any
	if err := json.Unmarshal([]byte(event.Get("receipt").(string)), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]any{"kind": "merge_turn_grant", "grantId": "grant-1"}) || event.Get("turn_id") != "grant-1" {
		t.Fatal(got, event)
	}
}
func Test26_queue_does_not_annotate_its_own_supersession(t *testing.T) {
	w, d, r := queueFixture(t)
	id := queueGrant(t, w, d, r, "grant-1")
	rows, err := w.s.All(w.ctx, "SELECT * FROM delivery_supersession WHERE event_id=?", id)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
func Test26_MTW_6_unaddressable_grant_has_exact_python_refusal(t *testing.T) {
	w, d, r := queueFixture(t)
	cases := []struct{ relationship, recipient, project, want string }{{"", alpha.TaskID, fxA, "the turn names no assignment"}, {r, beta.TaskID, fxA, "assignment 'rel-a' is addressed to parent 'task-alpha', not to 'task-beta'"}, {r, alpha.TaskID, fxB, "assignment 'rel-a' is attached to project 'PRJ-A', not to this turn's 'PRJ-B'"}}
	_, err := w.s.Querier(w.ctx).ExecContext(w.ctx, "INSERT INTO relationship_scope (relationship_id,project_key,recorded_at) VALUES (?,?,?)", r, fxA, fxISO)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		refused, event, err := d.Channel(w.ctx, sql.NullString{String: c.relationship, Valid: c.relationship != ""}, c.recipient, "grant", c.project)
		if err != nil || refused != c.want || event != "" {
			t.Fatalf("%+v: %q %q %v", c, refused, event, err)
		}
	}
	_, err = w.s.Querier(w.ctx).ExecContext(w.ctx, "UPDATE relationships SET allowed_recipients=? WHERE relationship_id=?", `["task-child"]`, r)
	if err != nil {
		t.Fatal(err)
	}
	refused, event, err := d.Channel(w.ctx, sql.NullString{String: r, Valid: true}, alpha.TaskID, "grant", fxA)
	if err != nil || refused != "'task-alpha' is not in the recipients assignment 'rel-a' authorizes" || event != "" {
		t.Fatal(refused, event, err)
	}
	_, err = w.s.Querier(w.ctx).ExecContext(w.ctx, "UPDATE relationships SET allowed_recipients=? WHERE relationship_id=?", `["task-alpha"]`, r)
	if err != nil {
		t.Fatal(err)
	}
	refused, event, err = d.Channel(w.ctx, sql.NullString{String: r, Valid: true}, alpha.TaskID, "grant", fxA)
	if err != nil || refused != "" || event != GrantEventID(r, "grant") {
		t.Fatal(refused, event, err)
	}
}
func Test26_MTW_7_without_delivery_grant_has_no_wake(t *testing.T) {
	w := newFx(t)
	turn := w.held()
	record := w.must(w.m.Turn(w.ctx, turn))
	grant := record["grant"].(map[string]any)
	if _, ok := grant["wake"]; ok {
		t.Fatal(grant)
	}
}
