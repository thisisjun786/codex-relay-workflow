package delivery

import (
	"fmt"
	"math"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1334 (CRW-1314 design section 3): the upward supervisor channel stages a lead's reports under
// "initiative:<key>" and a run's step wakes under "run:<id>". Their hour cap is the orchestrator
// class's (60), not the ordinary twelve, and the recipient's minimum gap between two sends stays
// five seconds whatever the class.
func TestSendRefusal_run_and_initiative_relationships_take_the_orchestrator_cap(t *testing.T) {
	t.Parallel()
	for _, relationship := range []string{"initiative:INI-1334", "run:run-1334"} {
		t.Run(relationship, func(t *testing.T) {
			f := newFixture(t, "")
			const recipient = "management-1334"
			now := f.clock.Now()
			window := math.Floor(now/RateWindowSeconds) * RateWindowSeconds
			stamp := store.SendStamp(window + 1)
			sent := 0
			spend := func(n int) {
				t.Helper()
				for ; n > 0; n-- {
					sent++
					id := fmt.Sprintf("msg-%s-%d", relationship, sent)
					_, err := execSQL(f.ctx, f.store, "INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES (?,?,'lead_report',?,'report','report','lead-1334',?,'s','{}','dispatched',?,?)", id, id, relationship, recipient, stamp, stamp)
					mustDo(t, err)
					_, err = execSQL(f.ctx, f.store, "INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, transport_started_at, observed_at) VALUES (?,?,1,'m','dispatched','yes',0,'{}',?,?,?)", id+"-a1", id, stamp, stamp, stamp)
					mustDo(t, err)
				}
			}
			ordinary := int(f.delivery.Policy.MaxSendsPerRelationshipPerHour)
			orchestrator := int(f.delivery.Policy.MaxSendsPerOrchestratorRelationshipPerHour)
			if orchestrator != 60 {
				t.Fatalf("the orchestrator class cap is %d, want 60", orchestrator)
			}
			spend(ordinary)
			if refused, err := f.delivery.SendRefusal(f.ctx, relationship, recipient, now); err != nil || refused != "" {
				t.Fatalf("%d sends of %s this hour: refused %q (%v), want none below the orchestrator cap %d", ordinary, relationship, refused, err, orchestrator)
			}
			spend(orchestrator - ordinary - 1)
			if refused, err := f.delivery.SendRefusal(f.ctx, relationship, recipient, now); err != nil || refused != "" {
				t.Fatalf("%d sends: refused %q (%v), want none", orchestrator-1, refused, err)
			}
			spend(1)
			if refused, err := f.delivery.SendRefusal(f.ctx, relationship, recipient, now); err != nil || refused != HourlyCap {
				t.Fatalf("%d sends: refused %q (%v), want %s", orchestrator, refused, err, HourlyCap)
			}
			// The gap is the recipient's and is unchanged: a send two seconds ago refuses the next one
			// for the five seconds' minimum interval, below the cap as well.
			g := newFixture(t, "")
			_, err := execSQL(g.ctx, g.store, "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at) VALUES (?,?,1,?)", recipient, window, now-2)
			mustDo(t, err)
			if refused, err := g.delivery.SendRefusal(g.ctx, relationship, recipient, now); err != nil || refused != MinSendInterval {
				t.Fatalf("a send two seconds after the last: refused %q (%v), want %s", refused, err, MinSendInterval)
			}
			if gap := g.delivery.Policy.MinSendInterval; gap != 5 {
				t.Fatalf("the minimum send interval is %v s, want 5", gap)
			}
			if refused, err := g.delivery.SendRefusal(g.ctx, relationship, recipient, now+3.5); err != nil || refused != "" {
				t.Fatalf("a send 5.5 s after the last: refused %q (%v), want none", refused, err)
			}
		})
	}
}
