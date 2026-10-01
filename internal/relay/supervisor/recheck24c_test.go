package supervisor

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func TestR24C_OlderFirstVsDrift(t *testing.T) {
	f := fixture24(t)
	stagedID(t, f)
	sweepExec(t, f,
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-2','rel-1',1,'def','ready_for_review','child','child','turn-2','completed','{}','2023-11-14T22:13:21.000000+00:00','2023-11-14T22:13:21.000000+00:00')`,
		`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-2',1,'rel-1',1,'def','thisisjun786/codex-relay-workflow','DONE','proved','v1','second','merge','2023-11-14T22:13:21.000000+00:00')`)
	o, err := f.c.FromEvent(f.ctx, "event-2")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := f.c.Stage(f.ctx, *o, "", "2023-11-14T22:13:21.000000+00:00")
	if err != nil {
		t.Fatal(err)
	}
	m2 := s2["messageId"].(string)
	moveDevinSupervisor(t, f)
	r := newBRun(t, f)
	r.att(t, m2, 1700000000)
	bCheck(t, r)
}

func TestR24C_ObsoleteAtClaimKeepsEligible(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	sweepExec(t, f, "UPDATE supervisor_messages SET state='withheld_pre_send',next_eligible_at=1699990000")
	r := newBRun(t, f)
	f.c.beforeClaimRead = func(tx context.Context) {
		for _, statement := range []string{"DELETE FROM work_reports WHERE event_id='event-1'", "DELETE FROM events WHERE event_id='event-1'"} {
			if _, err := f.s.Q(tx).ExecContext(tx, statement); err != nil {
				t.Fatal(err)
			}
		}
		f.c.beforeClaimRead = nil
	}
	r.att(t, id, 1700000000)
	// Go's transaction rolls the hook's fixture mutation back with the stale claim;
	// Python's nested test hook commits it. Normalize that harness-only difference.
	sweepExec(t, f, "DELETE FROM work_reports WHERE event_id='event-1'", "DELETE FROM events WHERE event_id='event-1'")
	bCheck(t, r)
}

func TestR24C_SettingsStopStanding(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	f.c.Settings = nil
	calls := 0
	f.c.SettingsLoader = func(context.Context, string) (*delivery.TaskSettings, error) {
		calls++
		if calls > 1 {
			return nil, Refusal{"settings_unavailable", "gone"}
		}
		return &delivery.TaskSettings{}, nil
	}
	r.att(t, id, 1700000000)
	bCheck(t, r)
}
