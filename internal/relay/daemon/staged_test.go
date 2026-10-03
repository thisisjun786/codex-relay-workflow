package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// A claim a child emitted from inside its own turn is staged; the daemon's observation of that
// turn settles it (store.ReceiptIntake.ResolveStaged's cases, which the daemon carries out
// itself, decision R3F-7): a normal ending makes it final (the stage a delivery reads as
// deliverable) exactly once; a
// failed or interrupted ending suppresses it, saying why; a turn still running leaves it staged.
func Test29ASettledTurnResolvesItsStagedClaims(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ status, stage, journal string }{
		{"completed", "final", `{"finalized": ["staged-1"], "suppressed": [], "status": "completed"}`},
		{"failed", "suppressed", `{"finalized": [], "suppressed": ["staged-1"], "status": "failed"}`},
		{"interrupted", "suppressed", `{"finalized": [], "suppressed": ["staged-1"], "status": "interrupted"}`},
		{"inProgress", "staged", ""},
	} {
		t.Run(c.status, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			seed(t, s, "r", "parent", "child", "anchor")
			_, err = s.DB.Exec("INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,staged_at,first_seen_at,last_seen_at) VALUES('staged-1','r',1,'rev','ready_for_review','child','child','anchor','inProgress','{}','staged','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
			if err != nil {
				t.Fatal(err)
			}
			d := New(s, &observationHost{status: c.status}, &delivery.FakeClock{T: 1700000000}, nil)
			// Settlement, not delivery: a final claim is not sent to this host, which reads turns only.
			d.Policy.MaxSends = -1 // the scheduler reads 0 as its default
			for range 2 {
				if _, err := d.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			row, err := s.One(ctx, "SELECT stage,finalizing_status,suppressed_reason FROM events WHERE event_id='staged-1'")
			if err != nil || row.Get("stage") != c.stage {
				t.Fatal(row, err)
			}
			switch c.stage {
			case "final":
				if row.Get("finalizing_status") != "completed" || row.Get("suppressed_reason") != nil {
					t.Fatal(row)
				}
			case "suppressed":
				if row.Get("suppressed_reason") != "the turn ended "+c.status+", so the staged claim is not promoted" {
					t.Fatal(row)
				}
			}
			journal, err := s.All(ctx, "SELECT subject,detail FROM journal WHERE kind='staged_resolved'")
			if err != nil {
				t.Fatal(err)
			}
			if c.journal == "" {
				if len(journal) != 0 {
					t.Fatal(journal)
				}
				return
			}
			// Settled once, however many ticks observe the turn.
			if len(journal) != 1 || journal[0].Get("subject") != "anchor" || journal[0].Get("detail") != c.journal {
				t.Fatal(journal)
			}
		})
	}
}
