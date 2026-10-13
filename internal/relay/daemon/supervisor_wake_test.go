package daemon

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1334 (CRW-1314 design section 3): the daemon's idle edge serves the supervisor channel as it
// serves deliveries. A supervisor message that found its recipient busy keeps a subscription on that
// recipient, the recipient's idle or notLoaded report makes the message due in the same tick, and a
// busy_cap or attempt_cap hold is released by the same report. What is proved here is the wiring;
// the store side is proved in internal/relay/supervisor (wake_test.go) and the transport side end to
// end in internal/relay/adapter (supervisor_wake_e2e_test.go).

const supWakeRecipient = "supervisor"

// supervisorWakeDaemon is a daemon with the supervisor channel over a store holding one staged
// report from the parent of PRJ-1 to the supervisor of INI-1, and the host the test drives.
func supervisorWakeDaemon(t *testing.T, host *idleHost) (*Daemon, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, stmt := range []string{
		`INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-1','REL-1','active','parent','host','child','host',1,'[]','[]','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`,
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-1','rel-1',1,'abc123456789abcdef','ready_for_review','child','child','turn-1','completed','{}','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`,
		`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',1,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done','merge','2023-11-14T22:13:20.000000+00:00')`,
	} {
		exec(t, s, stmt)
	}
	if err := storeseed.RecordRelationshipScope(ctx, s, "rel-1", "PRJ-1", "t"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []store.ScopeBindingsRow{{BindingID: "b-child", Role: "child", ScopeKind: "issue", ScopeKey: "REL-1", TaskID: "child", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {BindingID: "b-parent", Role: "parent", ScopeKind: "project", ScopeKey: "PRJ-1", TaskID: "parent", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {BindingID: "b-supervisor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: supWakeRecipient, HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}} {
		if err := storeseed.InsertScopeBinding(ctx, s, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []store.ScopeLinksRow{{LinkID: "lnk-issue", LinkKind: "execution", UpperKind: "project", UpperKey: "PRJ-1", UpperTaskID: "parent", LowerKind: "issue", LowerKey: "REL-1", LowerTaskID: "child", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {LinkID: "lnk-project", LinkKind: "execution", UpperKind: "initiative", UpperKey: "INI-1", UpperTaskID: supWakeRecipient, LowerKind: "project", LowerKey: "PRJ-1", LowerTaskID: "parent", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}} {
		if err := storeseed.InsertScopeLink(ctx, s, l); err != nil {
			t.Fatal(err)
		}
	}
	channel := &supervisor.Channel{Store: s, Linkage: supervisor.StoreLinkage{Store: s}, Program: "codex-session-relay", Settings: &delivery.TaskSettings{}}
	o, err := channel.FromEvent(ctx, "event-1")
	if err != nil || o == nil {
		t.Fatalf("obligation %v %v", o, err)
	}
	staged, err := channel.Stage(ctx, *o, "", "2023-11-14T22:13:20.000000+00:00")
	if err != nil {
		t.Fatal(err)
	}
	return New(s, host, &delivery.FakeClock{T: idleNow}, channel), s, staged["messageId"].(string)
}

// supervisorDeferrals is how many busy answers the message has journaled: deferBusy writes one per answer.
func supervisorDeferrals(t *testing.T, s *store.Store, id string) int {
	t.Helper()
	return count(t, s, "SELECT COUNT(*) FROM journal WHERE kind = 'supervisor_message_deferred' AND subject = ?", id)
}

func TestIdleWake_a_supervisor_message_is_woken_by_its_recipients_edge(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"idle", "notLoaded"} {
		t.Run("a "+status+" report", func(t *testing.T) {
			t.Parallel()
			host := &idleHost{}
			d, s, id := supervisorWakeDaemon(t, host)
			ctx := context.Background()
			// The first tick attempts the staged report, finds the recipient busy and defers it, and
			// opens the relay's own hold on that recipient.
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if n := supervisorDeferrals(t, s, id); n != 1 {
				t.Fatalf("%d busy answers after the first tick, want 1", n)
			}
			if !slices.Contains(host.holds, supWakeRecipient) {
				t.Fatalf("holds %v, want one on the busy supervisor %s", host.holds, supWakeRecipient)
			}
			// A tick with no report leaves the message inside its backoff.
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if n := supervisorDeferrals(t, s, id); n != 1 {
				t.Fatalf("%d busy answers with no report, want the backoff to hold it at 1", n)
			}
			// The recipient's edge makes the message due in the same tick, before its deadline.
			host.reports = []delivery.IdleReport{{ThreadID: supWakeRecipient, Status: status}}
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM journal WHERE kind = 'supervisor_message_woken' AND subject = ?", id); n != 1 {
				t.Fatalf("%d supervisor_message_woken rows, want 1", n)
			}
			if n := supervisorDeferrals(t, s, id); n != 2 {
				t.Fatalf("%d busy answers after the %s report, want the woken attempt to have met the recipient", n, status)
			}
			if len(host.releases) != 0 {
				t.Fatalf("releases %v while the message still waits", host.releases)
			}
			// Delivered, the message no longer waits: the hold is released.
			exec(t, s, "UPDATE supervisor_messages SET state = 'dispatched', next_eligible_at = NULL WHERE message_id = ?", id)
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(host.releases, []string{supWakeRecipient}) {
				t.Fatalf("releases %v, want the supervisor's once its backlog emptied", host.releases)
			}
		})
	}
}

func TestIdleWake_a_capped_supervisor_message_is_released_by_its_recipients_edge(t *testing.T) {
	t.Parallel()
	for _, hold := range []string{delivery.BusyCap, delivery.AttemptCap} {
		t.Run(hold, func(t *testing.T) {
			t.Parallel()
			host := &idleHost{}
			d, s, id := supervisorWakeDaemon(t, host)
			ctx := context.Background()
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			exec(t, s, "UPDATE supervisor_messages SET hold_reason = ? WHERE message_id = ?", hold, id)
			// Held, the message is never due on its timer, and its recipient stays held for the edge.
			d.Clock.(*delivery.FakeClock).T = idleNow + 3600
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if n := supervisorDeferrals(t, s, id); n != 1 {
				t.Fatalf("%d busy answers while held, want 1", n)
			}
			if len(host.releases) != 0 {
				t.Fatalf("releases %v: the held recipient lost its subscription", host.releases)
			}
			host.reports = []delivery.IdleReport{{ThreadID: supWakeRecipient, Status: "idle"}}
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM journal WHERE kind = 'supervisor_message_released' AND subject = ?", id); n != 1 {
				t.Fatalf("%d supervisor_message_released rows, want 1", n)
			}
			if n := supervisorDeferrals(t, s, id); n != 2 {
				t.Fatalf("%d busy answers after the release, want the released message attempted in the same tick", n)
			}
		})
	}
}
