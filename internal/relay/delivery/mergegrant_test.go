package delivery

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// promotedGrant drives the same real sender as other delivery tests: a waiter on an
// authorized assignment is promoted when a rival parent releases the target.
func promotedGrant(t *testing.T) (*fixture, *mergeturn.Service, string, string) {
	t.Helper()
	f := newFixture(t, "")
	rid := f.register(regOpts{})
	r := &registry.Registry{Store: f.store, Now: f.clock.ISO}
	for _, p := range []struct{ project, task, host string }{{"PRJ-A", parent, host}, {"PRJ-B", "01rival-task", "host-b"}} {
		_, err := r.BindScope(f.ctx, "parent", p.project, registry.Endpoint{TaskID: p.task, HostID: p.host, Cwd: sql.NullString{String: "/parent", Valid: true}})
		mustDo(t, err)
	}
	f.delivery.RoleGate = func(context.Context, *store.Store, string, *TaskSettings) error { return nil }
	m := &mergeturn.Service{Store: f.store, Registry: r, Now: f.clock.ISO, Delivery: mergeturn.StoreDelivery{Store: f.store}}
	rival, e := m.Request(f.ctx, "owner/repo", "dev", "PRJ-B", "01rival-task", "host-b", "head-b", true)
	mustDo(t, e)
	waiter, e := m.Request(f.ctx, "owner/repo", "dev", "PRJ-A", parent, host, "head-a", true, mergeturn.ClaimOptions{Relationship: sql.NullString{String: rid, Valid: true}})
	mustDo(t, e)
	_, e = m.Release(f.ctx, rival["turnId"].(string), "01rival-task", "returned", "not ready", "")
	mustDo(t, e)
	turn := waiter["turnId"].(string)
	current, e := m.Turn(f.ctx, turn)
	mustDo(t, e)
	id := current["grant"].(map[string]any)["wake"].(map[string]any)["eventId"].(string)
	return f, m, turn, id
}

func Test26_MTW_3_real_sender_wakes_parent_once_on_own_thread(t *testing.T) {
	f, m, turn, event := promotedGrant(t)
	before := len(f.host.threads[parent].turns)
	sent, err := f.delivery.Attempt(f.ctx, event, f.host, nil, "")
	mustDo(t, err)
	if str(sent, "deliveryState") != Dispatched || len(f.host.threads[parent].turns) != before+1 || len(f.host.sends) != 1 || f.host.sends[0].thread != parent {
		t.Fatal(sent, f.host.sends, f.row(event))
	}
	text := f.host.threads[parent].items[len(f.host.threads[parent].items)-1][1]
	record, err := m.Turn(f.ctx, turn)
	mustDo(t, err)
	grant := record["grant"].(map[string]any)["grantId"].(string)
	for _, fragment := range []string{grant, "merge turn granted", "owner/repo", "merge-turn-acknowledge"} {
		if !strings.Contains(text, fragment) {
			t.Fatal(fragment, text)
		}
	}
	if strings.Contains(text, "ack-proof") {
		t.Fatal(text)
	}
	state, err := f.delivery.SnapshotItem(f.ctx, event)
	mustDo(t, err)
	if str(state, "phase") != "awaiting_grant_acknowledgement" {
		t.Fatal(state)
	}
	_, err = m.Acknowledge(f.ctx, turn, parent, grant, "read the grant")
	mustDo(t, err)
	state, err = f.delivery.SnapshotItem(f.ctx, event)
	mustDo(t, err)
	if str(state, "phase") != "grant_acknowledged" {
		t.Fatal(state)
	}
	again, err := f.delivery.Attempt(f.ctx, event, f.host, nil, "")
	mustDo(t, err)
	if again != nil || len(f.host.sends) != 1 {
		t.Fatal(again, f.host.sends)
	}
}

func Test26_MTW_4_answered_grant_is_suppressed_before_send(t *testing.T) {
	f, m, turn, event := promotedGrant(t)
	record, err := m.Turn(f.ctx, turn)
	mustDo(t, err)
	grant := record["grant"].(map[string]any)["grantId"].(string)
	_, err = m.Acknowledge(f.ctx, turn, parent, grant, "read the grant")
	mustDo(t, err)
	answer, err := f.delivery.Attempt(f.ctx, event, f.host, nil, "")
	mustDo(t, err)
	if str(answer, "sendAttempted") != "no" || str(answer, "supersededReason") != "merge_turn_grant_answered" || len(f.host.sends) != 0 {
		t.Fatal(answer, f.host.sends)
	}
}

func Test26_MTW_4_generation_advance_does_not_suppress_grant(t *testing.T) {
	f, m, turn, event := promotedGrant(t)
	_, err := execSQL(f.ctx, f.store, `INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,reason,opened_at) VALUES (?,?,?,'pending','needs_changes_revision',?)`, f.rid, 2, "revision-1", f.clock.ISO())
	mustDo(t, err)
	_, err = execSQL(f.ctx, f.store, "UPDATE relationships SET execution_generation=execution_generation+1 WHERE relationship_id=?", f.rid)
	mustDo(t, err)
	sent, err := f.delivery.Attempt(f.ctx, event, f.host, nil, "")
	mustDo(t, err)
	if str(sent, "deliveryState") != Dispatched {
		t.Fatal(sent)
	}
	state, err := f.delivery.SnapshotItem(f.ctx, event)
	mustDo(t, err)
	if str(state, "phase") != "awaiting_grant_acknowledgement" {
		t.Fatal(state)
	}
	record, err := m.Turn(f.ctx, turn)
	mustDo(t, err)
	if record["state"] != "holding" {
		t.Fatal(record)
	}
}

func Test26_MTW_5_queuing_grant_does_not_supersede_child_correction(t *testing.T) {
	f := newFixture(t, "")
	rid := f.register(regOpts{recipients: []string{parent, child}})
	correction := strings.Repeat("f", 32)
	receipt := dumps(Obj{{Key: "eventId", Value: correction}, {Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: int64(1)}, {Key: "kind", Value: Revision}, {Key: "criteria", Value: []any{}}})
	_, err := execSQL(f.ctx, f.store, `INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,attempt,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at,observation_count) VALUES (?,?,?,?,?,?,NULL,?,?,?,?,'final',?,?,1)`, correction, rid, 1, strings.Repeat("0", 64), Revision, "relay", parent, "turn-verdict-1", "completed", receipt, f.clock.ISO(), f.clock.ISO())
	mustDo(t, err)
	_, err = f.delivery.Enqueue(f.ctx, correction, Revision, child)
	mustDo(t, err)
	// The promotion uses this same assignment; its relay notice must leave the
	// child's outstanding correction claimable on its own thread.
	r := &registry.Registry{Store: f.store, Now: f.clock.ISO}
	for _, p := range []struct{ project, task, host string }{{"PRJ-A", parent, host}, {"PRJ-B", "01rival-task", "host-b"}} {
		_, e := r.BindScope(f.ctx, "parent", p.project, registry.Endpoint{TaskID: p.task, HostID: p.host, Cwd: sql.NullString{String: "/parent", Valid: true}})
		mustDo(t, e)
	}
	f.delivery.RoleGate = func(context.Context, *store.Store, string, *TaskSettings) error { return nil }
	m := &mergeturn.Service{Store: f.store, Registry: r, Now: f.clock.ISO, Delivery: mergeturn.StoreDelivery{Store: f.store}}
	rival, e := m.Request(f.ctx, "owner/repo", "dev", "PRJ-B", "01rival-task", "host-b", "head-b", true)
	mustDo(t, e)
	_, e = m.Request(f.ctx, "owner/repo", "dev", "PRJ-A", parent, host, "head-a", true, mergeturn.ClaimOptions{Relationship: sql.NullString{String: rid, Valid: true}})
	mustDo(t, e)
	_, e = m.Release(f.ctx, rival["turnId"].(string), "01rival-task", "returned", "done", "")
	mustDo(t, e)
	reason, e := f.delivery.SupersessionReason(f.ctx, correction)
	mustDo(t, e)
	note, e := f.store.One(f.ctx, "SELECT reason FROM delivery_supersession WHERE event_id=?", correction)
	mustDo(t, e)
	if reason != "" || note != nil {
		t.Fatal(reason, note)
	}
	sent, e := f.delivery.Attempt(f.ctx, correction, f.host, nil, "")
	mustDo(t, e)
	if str(sent, "deliveryState") != Dispatched || len(f.host.threads[child].turns) != 1 {
		t.Fatal(sent, f.host.sends)
	}
}
