package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const loadStamp = "2023-11-14T22:13:20.000000+00:00"

// loadStore seeds a store the way a long-lived relay holds one: relationships rel-N, the first active of them
// active and the rest archived, each on generation gens with an anchor for every generation (the current one
// unsettled, the older ones settled), and admitted turns spread over its generations, all settled but the last
// pending of them (an archived relationship is seeded the same way, so its last admissions stay unsettled but no
// tick lists them). Every admitted turn has the poll row a read of it leaves, under the generation it was admitted to.
func loadStore(b *testing.B, active, archived, admitted, gens, pending int) *store.Store {
	b.Helper()
	s, err := fixtureStore(context.Background(), filepath.Join(b.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	run := func(statement string, args ...any) {
		b.Helper()
		if _, err := s.DB.Exec(statement, args...); err != nil {
			b.Fatalf("%s: %v", statement, err)
		}
	}
	run(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at)
SELECT 'rel-'||i,'ISSUE-'||i,CASE WHEN i<? THEN 'active' ELSE 'archived' END,'parent-'||i,'host','child-'||i,'host',?,'[]','[]',?,? FROM n`, active+archived-1, active, gens, loadStamp, loadStamp)
	run(`WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM g WHERE n<?)
INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at)
SELECT r.relationship_id,g.n,'dispatch-'||g.n||'-'||r.relationship_id,'bound','anchor-'||g.n||'-'||r.relationship_id,'initial_assignment',?,? FROM relationships r, g`, gens, loadStamp, loadStamp)
	run(`INSERT INTO assignment_settlements(relationship_id,thread_id,turn_id,terminal_status,settled_at)
SELECT g.relationship_id,r.child_task_id,g.dispatch_turn_id,'completed',? FROM generations g JOIN relationships r ON r.relationship_id=g.relationship_id WHERE g.execution_generation<?`, loadStamp, gens)
	run(`WITH RECURSIVE k(j) AS (SELECT 0 UNION ALL SELECT j+1 FROM k WHERE j<?)
INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at)
SELECT r.relationship_id,k.j%?+1,'turn-'||k.j||'-'||r.relationship_id,'explicit_admission_bound:anchor-'||(k.j%?+1)||'-'||r.relationship_id,'child','admitted',? FROM relationships r, k`, admitted-1, gens, gens, loadStamp)
	run(`INSERT INTO assignment_settlements(relationship_id,thread_id,turn_id,terminal_status,settled_at)
SELECT t.relationship_id,r.child_task_id,t.turn_id,'completed',? FROM generation_turns t JOIN relationships r ON r.relationship_id=t.relationship_id
WHERE CAST(substr(t.turn_id,6,instr(substr(t.turn_id,6),'-')-1) AS INTEGER)<?`, loadStamp, admitted-pending)
	run(`INSERT INTO poll_observations(relationship_id,execution_generation,turn_id,last_status,last_polled_at,last_attempt_at)
SELECT relationship_id,execution_generation,turn_id,'inProgress',?,? FROM generation_turns`, loadStamp, loadStamp)
	return s
}

// BenchmarkCensus measures what one tick spends on the census, and on the admissions query alone, for stores of
// different size: go test ./internal/relay/daemon -run '^$' -bench Census -benchtime 30x -count 5. A name gives the
// active relationships, the admitted turns they hold in all (the same number in each), the archived relationships
// beside them (seeded like the active ones), and, when more than one, the generations of each relationship and the
// pending admitted turns it has.
func BenchmarkCensus(b *testing.B) {
	for _, c := range []struct {
		name                                      string
		active, archived, admitted, gens, pending int
	}{
		{"active 200, admitted 5000", 200, 0, 25, 1, 1},
		{"active 200, admitted 5000, archived 800", 200, 800, 25, 1, 1},
		{"active 200, admitted 50000", 200, 0, 250, 1, 1},
		{"active 200, admitted 5000, 5 generations, 10 pending", 200, 0, 25, 5, 10},
		{"active 200, admitted 5000, 20 generations, 10 pending", 200, 0, 25, 20, 10},
	} {
		b.Run(c.name, func(b *testing.B) {
			s := loadStore(b, c.active, c.archived, c.admitted, c.gens, c.pending)
			d := New(s, &observationHost{status: "inProgress"}, movingClock(), nil)
			ctx := context.Background()
			b.Run("census", func(b *testing.B) {
				for range b.N {
					if _, err := d.census(ctx); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("admissions query", func(b *testing.B) {
				for range b.N {
					if _, err := s.All(ctx, unsettledAdmissions); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
