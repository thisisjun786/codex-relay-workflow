package delivery

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_multi_parent_isolation.py MPI-1..MPI-5.

var projects = map[string][3]string{"a": {"/repo-a", "AAA-1", "linear://project-alpha"}, "b": {"/repo-b", "BBB-1", "linear://project-beta"}}

func (f *fixture) twoParentAssignment(name string, events int) (string, []string) {
	p := projects[name]
	par, chi := "01parent-"+name, "01child-"+name
	root := filepath.Join(f.root, name)
	mustDo(f.t, os.MkdirAll(root, 0o755))
	rid := f.register(regOpts{issue: p[1], dispatchRequest: "dispatch-" + name, parent: par, parentCwd: p[0], child: chi, childRoot: root, turn: "turn-" + name, scopeRef: p[2], recipients: []string{par, chi}, parentOnlySettings: true})
	f.host.addThread(par)
	f.host.addThread(chi)
	var ids []string
	for i := 0; i < events; i++ {
		path := filepath.Join(root, fmt.Sprintf("out-%d.txt", i))
		mustDo(f.t, os.WriteFile(path, []byte(fmt.Sprintf("%s-%d", name, i)), 0o644))
		entries, err := store.BuildManifest([]string{path}, []string{root})
		mustDo(f.t, err)
		revision, _ := store.ManifestRevision(entries)
		attempt := i + 1
		event, _ := store.EventID(rid, 1, revision, "ready_for_review", "turn-"+name, &attempt)
		payload := Obj{{Key: "eventId", Value: event}, {Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: int64(1)}, {Key: "attempt", Value: int64(attempt)}, {Key: "revisionHash", Value: revision}, {Key: "outcome", Value: "ready_for_review"}, {Key: "producer", Value: "child"},
			{Key: "turnRef", Value: Obj{{Key: "threadId", Value: chi}, {Key: "turnId", Value: "turn-" + name}, {Key: "turnStatus", Value: "completed"}}}, {Key: "manifest", Value: []any{Obj{{Key: "path", Value: path}, {Key: "sha256", Value: entries[0].SHA256}, {Key: "bytes", Value: *entries[0].Bytes}}}}, {Key: "emittedAt", Value: f.clock.ISO()}}
		_, err = f.accept(payload, store.AcceptOptions{})
		mustDo(f.t, err)
		_, err = f.delivery.Enqueue(f.ctx, event, "", "")
		mustDo(f.t, err)
		ids = append(ids, event)
		f.clock.Advance(1)
	}
	return rid, ids
}

func (f *fixture) ackedTurn(name, event string) TurnInfo {
	f.mustAttempt(event, at(f.clock.Now()))
	f.clock.Advance(5)
	return f.host.startTurn("01parent-"+name, "ack-"+name, "inProgress", "")
}

// inParallel runs each call on its own Store over the same file, released at one instant.
func (f *fixture) inParallel(work map[string]func(*Ack) (Obj, error)) (map[string]Obj, map[string]error) {
	path := filepath.Join(f.tree, "gostate", "relay.sqlite3")
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	results, errs := map[string]Obj{}, map[string]error{}
	for name, call := range work {
		s, err := store.Open(f.ctx, path, "")
		mustDo(f.t, err)
		f.t.Cleanup(func() { _ = s.Close() })
		d := NewService(s, f.clock)
		a := NewAck(d)
		a.Sync = VerdictSync(s, f.clock)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := call(a)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[name] = err
			} else {
				results[name] = r
			}
		}()
	}
	close(start)
	wg.Wait()
	return results, errs
}

func TestMPI01_each_parent_keeps_its_scope_reference_and_project_key(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "mpi", "scope")
	f := newFixture(t, tree)
	a, _ := f.twoParentAssignment("a", 1)
	b, _ := f.twoParentAssignment("b", 1)
	ra, _ := LoadRelationship(f.ctx, f.store, a)
	rb, _ := LoadRelationship(f.ctx, f.store, b)
	requireSameJSON(t, "keys", []any{ProjectKey(ra), ProjectKey(rb)}, python.Out["keys"])
	if ra.ScopeRef != "linear://project-alpha" || rb.ScopeRef != "linear://project-beta" || ProjectKey(ra) != "/repo-a" {
		t.Fatal("scope refs and project keys")
	}
	requireSameTables(t, f, python)
}

func TestMPI02_two_parents_acknowledging_at_once_do_not_cross(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "mpi", "acks")
	f := newFixture(t, tree)
	a, ai := f.twoParentAssignment("a", 1)
	b, bi := f.twoParentAssignment("b", 1)
	turns := map[string]TurnInfo{"a": f.ackedTurn("a", ai[0]), "b": f.ackedTurn("b", bi[0])}
	events := map[string]string{"a": ai[0], "b": bi[0]}
	work := map[string]func(*Ack) (Obj, error){}
	for _, name := range []string{"a", "b"} {
		name := name
		work[name] = func(ack *Ack) (Obj, error) {
			e := events[name]
			return ack.Acknowledge(context.Background(), e, turns[name].TurnID, AckProof(e, turns[name].TurnID), true, nil, f.host)
		}
	}
	results, errs := f.inParallel(work)
	if len(errs) != 0 || len(python.Out["errors"].(map[string]any)) != 0 {
		t.Fatalf("errors go %v python %v", errs, python.Out["errors"])
	}
	for name, rid := range map[string]string{"a": a, "b": b} {
		if v, _ := get(results[name], "accepted"); v != true {
			t.Fatalf("%s not accepted", name)
		}
		row := f.one("SELECT * FROM acks WHERE event_id = ?", events[name])
		if row.S("ack_turn_id") != "ack-"+name || f.row(events[name]).S("state") != Acknowledged || f.one("SELECT relationship_id FROM events WHERE event_id = ?", events[name]).S("relationship_id") != rid {
			t.Fatalf("%s crossed", name)
		}
	}
}

func TestMPI03_two_parents_ruling_needs_changes_at_once_open_one_generation_each(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "mpi", "verdicts")
	f := newFixture(t, tree)
	a, ai := f.twoParentAssignment("a", 1)
	b, bi := f.twoParentAssignment("b", 1)
	ack := NewAck(f.delivery)
	events := map[string]string{"a": ai[0], "b": bi[0]}
	for _, name := range []string{"a", "b"} {
		turn := f.ackedTurn(name, events[name])
		_, err := ack.Acknowledge(f.ctx, events[name], turn.TurnID, AckProof(events[name], turn.TurnID), true, nil, f.host)
		mustDo(t, err)
	}
	work := map[string]func(*Ack) (Obj, error){}
	for _, name := range []string{"a", "b"} {
		name := name
		work[name] = func(ack *Ack) (Obj, error) {
			return ack.RecordVerdict(context.Background(), events[name], "needs_changes", "verdict-"+name, nil, []any{finding("c1", "needs_changes", "fix it")}, nil, nil)
		}
	}
	results, errs := f.inParallel(work)
	if len(errs) != 0 {
		t.Fatalf("errors %v", errs)
	}
	next := map[string]any{}
	for name, rid := range map[string]string{"a": a, "b": b} {
		n, _ := get(results[name], "nextExecutionGeneration")
		next[name] = n
		r, _ := LoadRelationship(f.ctx, f.store, rid)
		revisions, err := all(f.ctx, f.store, "SELECT * FROM deliveries WHERE kind = ? AND relationship_id = ?", Revision, rid)
		mustDo(t, err)
		if r.Generation != 2 || len(revisions) != 1 || revisions[0].S("recipient_task_id") != "01child-"+name {
			t.Fatalf("%s: generation %d revisions %v", name, r.Generation, revisions)
		}
	}
	requireSameJSON(t, "next", next, python.Out["next"])
}

func TestMPI04_each_outbox_job_names_its_document_and_one_claim_cannot_complete_another(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "mpi", "outbox")
	f := newFixture(t, tree)
	ack := NewAck(f.delivery)
	ack.Sync = VerdictSync(f.store, f.clock)
	jobs := map[string]any{}
	for _, tc := range []struct{ name, doc string }{{"a", "https://linear.app/example/document/project-alpha-0000"}, {"b", "https://linear.app/example/document/project-beta-00000"}} {
		rid, ids := f.twoParentAssignment(tc.name, 1)
		turn := f.ackedTurn(tc.name, ids[0])
		_, err := ack.Acknowledge(f.ctx, ids[0], turn.TurnID, AckProof(ids[0], turn.TurnID), true, nil, f.host)
		mustDo(t, err)
		_, err = execSQL(f.ctx, f.store, "INSERT INTO sync_targets (relationship_id, target, target_ref, recorded_at) VALUES (?,?,?,?) ON CONFLICT(relationship_id, target) DO UPDATE SET target_ref = excluded.target_ref, recorded_at = excluded.recorded_at", rid, coordinationDocument, tc.doc, f.clock.ISO())
		mustDo(t, err)
		_, err = ack.RecordVerdict(f.ctx, ids[0], "verified", "verdict-"+tc.name, nil, nil, nil, nil)
		mustDo(t, err)
		jobs[tc.name] = f.one("SELECT sync_id FROM sync_outbox WHERE relationship_id = ?", rid).S("sync_id")
	}
	requireSameJSON(t, "jobs", jobs, python.Out["jobs"])
	alpha, err := SyncClaim(f.ctx, f.store, f.clock, jobs["a"].(string), "worker-1", f.clock.Now())
	mustDo(t, err)
	_, err = SyncClaim(f.ctx, f.store, f.clock, jobs["b"].(string), "worker-1", f.clock.Now())
	mustDo(t, err)
	_, err = SyncFenced(f.ctx, f.store, jobs["b"].(string), str(alpha, "claimToken"))
	requireReason(t, err, SyncNotClaimable)
	requireSameJSON(t, "complete", refusalOf(err), python.Out["complete"])
}

func (f *fixture) tick(sc *Scheduler) {
	mustDo(f.t, sc.Deliver(f.ctx, f.host, f.clock.Now(), &TickCounts{}))
}

func (f *fixture) drain(sc *Scheduler, ids []string) map[string]bool {
	delivered := map[string]bool{}
	for i := 0; i < 12 && len(delivered) < len(ids); i++ {
		f.clock.Advance(3600)
		f.tick(sc)
		for _, e := range ids {
			if f.row(e).S("state") == Dispatched {
				delivered[e] = true
			}
		}
	}
	return delivered
}

func TestMPI05_one_parents_limits_never_starve_the_other(t *testing.T) {
	scheduler := func(f *fixture) *Scheduler { return &Scheduler{Delivery: f.delivery, Ack: NewAck(f.delivery)} }
	t.Run("A at its attempt cap", func(t *testing.T) {
		f := newFixture(t, "")
		_, ai := f.twoParentAssignment("a", 1)
		_, bi := f.twoParentAssignment("b", 3)
		for i := int64(0); i < f.delivery.Policy.MaxAttempts; i++ {
			f.host.script = []string{"read_fail"}
			f.clock.Advance(100000)
			f.mustAttempt(ai[0], at(f.clock.Now()))
		}
		if f.row(ai[0]).S("hold_reason") != AttemptCap {
			t.Fatal("attempt_cap")
		}
		before := f.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ?", ai[0])
		if len(f.drain(scheduler(f), bi)) != 3 || f.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ?", ai[0]) != before {
			t.Fatal("B drained and A's cap held")
		}
	})
	t.Run("A at the hourly recipient cap", func(t *testing.T) {
		f := newFixture(t, "")
		_, ai := f.twoParentAssignment("a", 1)
		_, bi := f.twoParentAssignment("b", 3)
		now := f.clock.Now()
		_, err := execSQL(f.ctx, f.store, "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at) VALUES (?,?,?,?)", "01parent-a", math.Floor(now/3600)*3600, f.delivery.Policy.MaxSendsPerRecipientPerHour, now)
		mustDo(t, err)
		sc := scheduler(f)
		f.tick(sc)
		sent := map[string]bool{}
		for _, s := range f.host.sends {
			sent[s.thread] = true
		}
		if sent["01parent-a"] || !sent["01parent-b"] || f.row(ai[0]).S("state") == Dispatched {
			t.Fatalf("sent in the capped window: %v", sent)
		}
		if len(f.drain(sc, bi)) != 3 || f.row(ai[0]).S("state") != Dispatched {
			t.Fatal("B drains; the cap is a delay, not a wall")
		}
	})
	t.Run("A paused, B served in that tick", func(t *testing.T) {
		f := newFixture(t, "")
		a, ai := f.twoParentAssignment("a", 1)
		_, bi := f.twoParentAssignment("b", 2)
		f.rid = a
		f.setStatusBy("paused", "the owner of project alpha")
		f.clock.Advance(3600)
		before := len(f.host.sends)
		f.tick(scheduler(f))
		sent := map[string]bool{}
		for _, s := range f.host.sends[before:] {
			sent[s.thread] = true
		}
		arrived := 0
		for _, e := range bi {
			if f.row(e).S("state") == Dispatched {
				arrived++
			}
		}
		if !sent["01parent-b"] || sent["01parent-a"] || arrived == 0 || f.row(ai[0]).S("state") == Dispatched {
			t.Fatalf("sent %v arrived %d", sent, arrived)
		}
	})
	t.Run("A archived, the shared scheduler keeps serving B", func(t *testing.T) {
		f := newFixture(t, "")
		a, ai := f.twoParentAssignment("a", 1)
		_, bi := f.twoParentAssignment("b", 2)
		f.rid = a
		f.setStatusBy("paused", "the owner of project alpha")
		f.setStatusBy("archived", "the owner of project alpha")
		if len(f.drain(scheduler(f), bi)) != 2 || f.row(ai[0]).S("state") == Dispatched {
			t.Fatal("B drains, A is never delivered")
		}
	})
}
