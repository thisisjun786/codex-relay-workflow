package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// Restart (epoch.go): a parent that starts again rebuilds from the store alone. The tests crash a parent session at each boundary of a release and of a result's way to
// acceptance, start another session over the same store (the host keeps what it created), and require one child, one release row, the resume value the plan states, and that
// the session that crashed, which is still alive in these tests, can write nothing.

var errCrash = errors.New("the parent crashed here")

func (k *releaseKit) resumeOf(s *Scheduler, plan, actor, node string) (RestartNode, bool) {
	k.t.Helper()
	report, err := s.Restart(context.Background(), plan, actor)
	if err != nil {
		k.t.Fatalf("restart: %v", err)
	}
	for _, n := range report.Nodes {
		if n.NodeID == node {
			return n, true
		}
	}
	return RestartNode{}, false
}

func TestRestartOfTheSameTaskAtEveryBoundaryOfARelease(t *testing.T) {
	cases := []struct {
		name         string
		crash        func(k *releaseKit, s *Scheduler)
		owned        bool
		resume       string
		created      int
		releases     int
		releaseEpoch int64
	}{
		{name: "before the intent", crash: func(k *releaseKit, s *Scheduler) {
			s.testAfterReserve = func() error { return errCrash }
			if _, err := s.Release(context.Background(), "rp", "A", "parent", k.request(false)); !errors.Is(err, errCrash) {
				k.t.Fatalf("the crash was not reached: %v", err)
			}
			s.testAfterReserve = nil
		}, releaseEpoch: 2},
		{name: "after the intent, before the managed start", crash: func(k *releaseKit, s *Scheduler) {
			started := s.Start
			s.Start = func(context.Context, []byte) (StartAnswer, error) { return StartAnswer{}, errCrash }
			if _, err := s.Release(context.Background(), "rp", "A", "parent", k.request(false)); !errors.Is(err, errCrash) {
				k.t.Fatalf("the crash was not reached: %v", err)
			}
			s.Start = started
		}, owned: true, resume: ResumeReconcile, releases: 1, releaseEpoch: 1},
		{name: "the creation response lost", crash: func(k *releaseKit, s *Scheduler) {
			k.host.loseFirstCreation = true
			if res, err := s.Release(context.Background(), "rp", "A", "parent", k.request(false)); err != nil || res.Bound {
				k.t.Fatalf("the response was not lost: %+v, %v", res, err)
			}
		}, owned: true, resume: ResumeReconcile, created: 1, releases: 1, releaseEpoch: 1},
		{name: "after the managed start, before the bind", crash: func(k *releaseKit, s *Scheduler) {
			s.testAfterStart = func() error { return errCrash }
			if _, err := s.Release(context.Background(), "rp", "A", "parent", k.request(false)); !errors.Is(err, errCrash) {
				k.t.Fatalf("the crash was not reached: %v", err)
			}
			s.testAfterStart = nil
		}, owned: true, resume: ResumeReconcile, created: 1, releases: 1, releaseEpoch: 1},
		{name: "after the bind", crash: func(k *releaseKit, s *Scheduler) {
			if res, err := s.Release(context.Background(), "rp", "A", "parent", k.request(false)); err != nil || !res.Bound {
				k.t.Fatalf("release: %+v, %v", res, err)
			}
		}, owned: true, resume: ResumeAdopt, created: 1, releases: 1, releaseEpoch: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			first := k.sched
			k.claim(first, "rp", "parent", "session-1")
			c.crash(k, first)
			second := k.session("rp", "parent", "session-2")

			if created, _ := k.host.counts(); created != c.created {
				t.Fatalf("the host holds %d children after the crash, want %d", created, c.created)
			}
			node, found := k.resumeOf(second, "rp", "parent", "A")
			if found != c.owned || node.Resume != c.resume {
				t.Fatalf("restart says %+v (found %v), want resume %q (owned %v)", node, found, c.resume, c.owned)
			}
			if rows := k.rows(); rows.releases != c.releases {
				t.Fatalf("rows after the crash = %+v", rows)
			}

			res, err := second.Release(ctx, "rp", "A", "parent", k.request(false))
			if err != nil || !res.Bound || res.ChildTaskID != "child-1" {
				t.Fatalf("the new session's release = %+v, %v", res, err)
			}
			if res.Replayed != (c.releases == 1) {
				t.Fatalf("replayed = %v with %d release rows before the call", res.Replayed, c.releases)
			}
			if created, _ := k.host.counts(); created != 1 {
				t.Fatalf("the host holds %d children, want exactly 1", created)
			}
			if rows := k.rows(); rows != (rowCounts{releases: 1, requests: 1, manifests: 1, executions: 1, slots: 1}) {
				t.Fatalf("rows after the recovery = %+v", rows)
			}
			var epoch int64
			if err := k.s.DB.QueryRow("SELECT coordinator_epoch FROM dag_releases").Scan(&epoch); err != nil || epoch != c.releaseEpoch {
				t.Fatalf("the release carries epoch %d (%v), want the epoch of the session that decided it, %d", epoch, err, c.releaseEpoch)
			}
			// the child that was there is the one that is adopted, and a third session finds the same
			third := k.session("rp", "parent", "session-3")
			if node, found := k.resumeOf(third, "rp", "parent", "A"); !found || node.Resume != ResumeAdopt || node.RelationshipID == "" || node.ChildTaskID != "child-1" {
				t.Fatalf("after the recovery restart says %+v (found %v)", node, found)
			}
			if again, err := third.Release(ctx, "rp", "A", "parent", k.request(false)); err != nil || !again.Replayed || again.ChildTaskID != "child-1" {
				t.Fatalf("a third session's release = %+v, %v", again, err)
			}
			if created, _ := k.host.counts(); created != 1 {
				t.Fatalf("the host holds %d children after the third session, want 1", created)
			}
			// the first session is still alive in this test and holds no epoch: it decides nothing
			before := allRows(t, k.s.DB)
			if _, err := first.Release(ctx, "rp", "A", "parent", k.request(false)); !isStale(err) {
				t.Fatalf("the crashed session's release = %v", err)
			}
			if !equalRows(before, allRows(t, k.s.DB)) {
				t.Fatal("the crashed session's refused release changed the store")
			}
		})
	}
}

func equalRows(a, b map[string][]map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for name, rows := range a {
		other := b[name]
		if len(rows) != len(other) {
			return false
		}
		for i := range rows {
			if len(rows[i]) != len(other[i]) {
				return false
			}
			for key, v := range rows[i] {
				if other[i][key] != v {
					return false
				}
			}
		}
	}
	return true
}

// The boundaries after the child is bound: its result stored, the ruling stored, the acceptance written. Each is crashed after, a new session of the same task starts, and what was stored before the crash
// is what the new session uses: nothing is adopted again and nothing is asked of the child twice.
func TestRestartOfTheSameTaskAfterTheResultAndTheRuling(t *testing.T) {
	ctx := context.Background()
	deleteRuling := func(k *releaseKit) (restore func()) {
		var event, revision, set string
		if err := k.s.DB.QueryRow("SELECT v.event_id, c.head_revision, c.set_digest FROM verdicts v JOIN verdict_context c ON c.event_id = v.event_id").Scan(&event, &revision, &set); err != nil {
			k.t.Fatal(err)
		}
		k.exec("DELETE FROM verdict_context")
		k.exec("DELETE FROM verdicts")
		return func() {
			k.exec("INSERT INTO verdicts (event_id, record, verdict, verdict_turn_id, decided_at) VALUES (?, '{}', 'verified', 'verdict-turn', ?)", event, k.clock())
			k.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, currency, head_event_id, head_revision, ack_evidence, recorded_at) VALUES (?, ?, '{}', 'current', ?, ?, '{}', ?)", event, set, event, revision, k.clock())
		}
	}
	setup := func(t *testing.T) (*releaseKit, *Scheduler, string) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		first := k.sched
		k.claim(first, "rp", "parent", "session-1")
		res := k.mustRelease("rp", "A")
		k.seedReport(res.RelationshipID, "A", "rp")
		return k, first, res.RelationshipID
	}

	t.Run("the result stored and the ruling not", func(t *testing.T) {
		k, first, _ := setup(t)
		restore := deleteRuling(k)
		second := k.session("rp", "parent", "session-2")
		if node, found := k.resumeOf(second, "rp", "parent", "A"); !found || node.Resume != ResumeAdopt {
			t.Fatalf("restart says %+v", node)
		}
		before := allRows(t, k.s.DB)
		if _, err := second.Accept(ctx, "rp", "A", "parent", AcceptInput{RuleVersion: verifier}); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("accepting before the ruling = %v", err)
		}
		if !equalRows(before, allRows(t, k.s.DB)) {
			t.Fatal("a refused acceptance changed the store")
		}
		restore()
		res, err := second.Accept(ctx, "rp", "A", "parent", AcceptInput{RuleVersion: verifier})
		if err != nil || res.Replayed || res.AcceptanceID == "" {
			t.Fatalf("accept after the ruling = %+v, %v", res, err)
		}
		if _, err := first.Accept(ctx, "rp", "A", "parent", AcceptInput{RuleVersion: verifier}); !isStale(err) {
			t.Fatalf("the crashed session's acceptance = %v", err)
		}
		if created, _ := k.host.counts(); created != 1 || k.count("SELECT COUNT(*) FROM dag_acceptances") != 1 {
			t.Fatalf("created %d, acceptances %d", created, k.count("SELECT COUNT(*) FROM dag_acceptances"))
		}
	})

	t.Run("the ruling stored and the acceptance not", func(t *testing.T) {
		k, first, _ := setup(t)
		second := k.session("rp", "parent", "session-2")
		if node, found := k.resumeOf(second, "rp", "parent", "A"); !found || node.Resume != ResumeAdopt {
			t.Fatalf("restart says %+v", node)
		}
		res, err := second.Accept(ctx, "rp", "A", "parent", AcceptInput{RuleVersion: verifier})
		if err != nil || res.Replayed {
			t.Fatalf("accept = %+v, %v", res, err)
		}
		var epoch int64
		if err := k.s.DB.QueryRow("SELECT coordinator_epoch FROM dag_acceptances").Scan(&epoch); err != nil || epoch != 2 {
			t.Fatalf("the acceptance carries epoch %d (%v), want 2", epoch, err)
		}
		if _, err := first.Accept(ctx, "rp", "A", "parent", AcceptInput{RuleVersion: verifier}); !isStale(err) {
			t.Fatalf("the crashed session's acceptance = %v", err)
		}
	})

	t.Run("accepted and the reply lost", func(t *testing.T) {
		k, first, _ := setup(t)
		if _, err := first.Accept(ctx, "rp", "A", "parent", AcceptInput{RuleVersion: verifier}); err != nil {
			t.Fatal(err)
		}
		second := k.session("rp", "parent", "session-2")
		// an acceptance is the node's value and belongs to no session: nothing is adopted, nothing is reconciled, and the second session's own accept is a replay
		if node, found := k.resumeOf(second, "rp", "parent", "A"); !found || node.Resume != ResumeNone || node.State != StateAccepted {
			t.Fatalf("restart says %+v (found %v)", node, found)
		}
		res, err := second.Accept(ctx, "rp", "A", "parent", AcceptInput{RuleVersion: verifier})
		if err != nil || !res.Replayed {
			t.Fatalf("a repeated acceptance = %+v, %v", res, err)
		}
		var epoch int64
		if err := k.s.DB.QueryRow("SELECT coordinator_epoch FROM dag_acceptances").Scan(&epoch); err != nil || epoch != 1 || k.count("SELECT COUNT(*) FROM dag_acceptances") != 1 {
			t.Fatalf("the acceptance keeps the epoch of the session that wrote it: %d (%v)", epoch, err)
		}
	})
}

// Time decides nothing. Whatever lease, heartbeat or deadline a store holds, a node that has an owner is not ready again because a day passed, and a reading or a restart report is the same
// with a clock a year ahead.
func TestTimeReassignsNothing(t *testing.T) {
	ctx := context.Background()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.claim(k.sched, "rp", "parent", "session-1")
	k.mustRelease("rp", "A")
	second := k.session("rp", "parent", "session-2")
	before, err := second.Restart(ctx, "rp", "parent")
	if err != nil {
		t.Fatal(err)
	}
	reading := k.read("rp")
	later := func() string { return time.Now().AddDate(1, 0, 0).UTC().Format("2006-01-02T15:04:05.000000+00:00") }
	second.Now, k.sched.Now = later, later
	k.exec("UPDATE deliveries SET lease_until = 1, next_eligible_at = 1")
	after, err := second.Restart(ctx, "rp", "parent")
	if err != nil {
		t.Fatal(err)
	}
	if string(marshal(before.Object())) != string(marshal(after.Object())) {
		t.Fatalf("the report changed with the clock:\n%s\n%s", marshal(before.Object()), marshal(after.Object()))
	}
	if again := k.read("rp"); string(marshal(again.Object())) != string(marshal(reading.Object())) {
		t.Fatal("the reading changed with the clock")
	}
	if a := k.read("rp").node("A"); a.Disposition == DispReady {
		t.Fatalf("A = %+v: an owned node is ready again", a)
	}
	res, err := second.Release(ctx, "rp", "A", "parent", k.request(false))
	if err != nil || !res.Replayed {
		t.Fatalf("release after a year = %+v, %v", res, err)
	}
	if created, _ := k.host.counts(); created != 1 {
		t.Fatalf("%d children", created)
	}
}

// An unknown creation stays unknown until the host's record of it is read, and is never created again; an unknown merge turn stays unknown until the head is observed, and no second turn is requested.
func TestUnknownEffectsAreNotRerun(t *testing.T) {
	ctx := context.Background()
	t.Run("a creation whose response was lost, across three sessions", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.claim(k.sched, "rp", "parent", "session-1")
		k.host.loseFirstCreation = true
		if res, err := k.sched.Release(ctx, "rp", "A", "parent", k.request(false)); err != nil || res.Bound {
			t.Fatalf("release = %+v, %v", res, err)
		}
		for i, nonce := range []string{"session-2", "session-3", "session-4"} {
			s := k.session("rp", "parent", nonce)
			node, found := k.resumeOf(s, "rp", "parent", "A")
			if i == 0 && (!found || node.Resume != ResumeReconcile || node.Reason != BlockedCreationUnknown) {
				t.Fatalf("restart says %+v", node)
			}
			if i == 0 {
				if res, err := s.Release(ctx, "rp", "A", "parent", k.request(false)); err != nil || !res.Bound || res.ChildTaskID != "child-1" {
					t.Fatalf("reconciling release = %+v, %v", res, err)
				}
			}
			if created, _ := k.host.counts(); created != 1 {
				t.Fatalf("after session %s the host holds %d children", nonce, created)
			}
		}
	})

	t.Run("a merge turn whose effect is unknown", func(t *testing.T) {
		k := newJudgeKit(t)
		k.claim(k.sched, "g", "parent", "session-1")
		if _, turn, err := k.sched.RequestMergeTurn(ctx, "g", "I", "parent", MergeRequestInput{Host: "host"}); err != nil || turn == nil {
			t.Fatalf("request = %v, %v", turn, err)
		}
		k.exec("UPDATE merge_turns SET state = 'unknown'")
		second := k.session("g", "parent", "session-2")
		node, found := k.resumeOf(second, "g", "parent", "I")
		if !found || node.Reason != BlockedEffectUnknown || node.Resume != ResumeReconcile {
			t.Fatalf("restart says %+v (found %v)", node, found)
		}
		turns := k.count("SELECT COUNT(*) FROM merge_turns")
		// the merge lane answers the holder's repeated request with the turn it already has or refuses it; either way no second turn exists and the unknown turn is still unknown
		_, _, _ = second.RequestMergeTurn(ctx, "g", "I", "parent", MergeRequestInput{Host: "host"})
		if k.count("SELECT COUNT(*) FROM merge_turns") != turns || k.count("SELECT COUNT(*) FROM merge_turns WHERE state = 'unknown'") != 1 {
			t.Fatalf("a second merge turn was requested, or the unknown one moved (%d turns)", k.count("SELECT COUNT(*) FROM merge_turns"))
		}
	})
}

// A replacement parent, with the relay's own commands: the new parent registers the child under a new relationship with the old one superseded, the project is handed over, the new parent
// claims the epoch, and Restart says what to do before anything is bound. Adopt binds the successor to the node; nothing is created; the old parent decides nothing.
type replacement struct {
	k      *releaseKit
	s1, s2 *Scheduler
	r1, r2 string
	child  string
}

func replace(t *testing.T) replacement {
	t.Helper()
	ctx := context.Background()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	s1 := k.sched
	k.claim(s1, "rp", "parent", "session-1")
	res := k.mustRelease("rp", "A")
	reg := &registry.Registry{Store: k.s, Now: k.clock}
	r2, err := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: "parent-2", HostID: "host"}, Child: registry.Endpoint{TaskID: res.ChildTaskID, HostID: "host"}, IssueKey: "CRW-A",
		ArtifactRoots: []string{k.root}, AllowedRecipients: []string{"parent-2", res.ChildTaskID}, DispatchRequestID: "dispatch-handover", DispatchTurnID: sql.NullString{String: "turn-handover", Valid: true},
		ProjectKey: "P-TEST", Supersedes: res.RelationshipID})
	if err != nil {
		t.Fatalf("registering the successor: %v", err)
	}
	outstanding, err := reg.Outstanding(ctx, "P-TEST", sql.NullString{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Handover(ctx, "parent", "P-TEST", "parent", registry.Endpoint{TaskID: "parent-2", HostID: "host"}, outstanding, "the parent session was replaced", "parent-2"); err != nil {
		t.Fatalf("handover: %v", err)
	}
	s2 := k.session("rp", "parent-2", "session-2")
	return replacement{k: k, s1: s1, s2: s2, r1: res.RelationshipID, r2: r2.ID, child: res.ChildTaskID}
}

func TestAReplacementParentAdoptsTheLiveChild(t *testing.T) {
	ctx := context.Background()
	r := replace(t)
	k := r.k
	node, found := k.resumeOf(r.s2, "rp", "parent-2", "A")
	if !found || node.Resume != ResumeAdoptNeeded {
		t.Fatalf("restart before the adoption says %+v (found %v)", node, found)
	}
	// the replaced parent decides nothing, and the replacement cannot yet act on a relationship the node does not stand on
	before := allRows(t, k.s.DB)
	if _, err := r.s1.Release(ctx, "rp", "A", "parent", k.request(false)); !isStale(err) {
		t.Fatalf("the old parent's release = %v", err)
	}
	if !equalRows(before, allRows(t, k.s.DB)) {
		t.Fatal("a refused write changed the store")
	}
	if _, err := r.s2.Accept(ctx, "rp", "A", "parent-2", AcceptInput{RuleVersion: verifier}); refusalReason(err) != "relationship_not_active" {
		t.Fatalf("accepting before the adoption = %v", err)
	}

	// a replacement that holds the epoch adopts; the same call again is a replay
	if _, err := r.s2.Adopt(ctx, "rp", "A", "parent"); !isStale(err) && refusalReason(err) == "" {
		t.Fatalf("adopting as another actor = %v", err)
	}
	res, err := r.s2.Adopt(ctx, "rp", "A", "parent-2")
	if err != nil || res.Replayed || res.RelationshipID != r.r2 || res.FromRelationshipID != r.r1 || res.ChildTaskID != r.child || res.Generation != 1 {
		t.Fatalf("adopt = %+v, %v", res, err)
	}
	var kind, manifest, original string
	if err := k.s.DB.QueryRow("SELECT kind, manifest_digest FROM dag_node_executions WHERE relationship_id = ?", r.r2).Scan(&kind, &manifest); err != nil || kind != "parent_handover" {
		t.Fatalf("execution row: %s (%v)", kind, err)
	}
	if err := k.s.DB.QueryRow("SELECT manifest_digest FROM dag_node_executions WHERE relationship_id = ?", r.r1).Scan(&original); err != nil || original != manifest {
		t.Fatalf("the adopted execution rests on manifest %s, the child was released under %s (%v)", manifest, original, err)
	}
	if again, err := r.s2.Adopt(ctx, "rp", "A", "parent-2"); err != nil || !again.Replayed {
		t.Fatalf("adopting again = %+v, %v", again, err)
	}
	if created, _ := k.host.counts(); created != 1 || k.count("SELECT COUNT(*) FROM relationships WHERE issue_key = 'CRW-A'") != 2 {
		t.Fatalf("adoption made a child or a relationship (created %d)", created)
	}
	if node, found := k.resumeOf(r.s2, "rp", "parent-2", "A"); !found || node.Resume != ResumeAdopt || node.RelationshipID != r.r2 {
		t.Fatalf("restart after the adoption says %+v", node)
	}

	// the child reports to its new parent; the replacement accepts and returns the slot the previous parent reserved, as that parent, without reserving another
	k.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, set_digest, recorded_at) VALUES (?, 'c1', 'criterion', 1, ?, ?)", r.r2, releaseCriteriaDigest(), k.clock())
	k.exec("INSERT INTO verification_mode (relationship_id, mode, recorded_at) VALUES (?, 'managed', ?)", r.r2, k.clock())
	k.seedReport(r.r2, "A", "rp")
	acc, err := r.s2.Accept(ctx, "rp", "A", "parent-2", AcceptInput{RuleVersion: verifier})
	if err != nil || !acc.SlotReleased || acc.RelationshipID != r.r2 {
		t.Fatalf("accept after the adoption = %+v, %v", acc, err)
	}
	var state, by, reason string
	if err := k.s.DB.QueryRow("SELECT state, released_by, release_reason FROM execution_slots WHERE subject_key = 'rp/A'").Scan(&state, &by, &reason); err != nil || state != "released" || by != "parent" || reason != "dag_accepted" {
		t.Fatalf("slot: %s by %s for %s (%v)", state, by, reason, err)
	}
	if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = 'rp/A'") != 1 {
		t.Fatal("a second slot was reserved")
	}
	if _, err := r.s1.Accept(ctx, "rp", "A", "parent", AcceptInput{RuleVersion: verifier}); !isStale(err) {
		t.Fatalf("the old parent's acceptance = %v", err)
	}
}

func TestAdoptionSurvivesACeilingLoweredBelowUse(t *testing.T) {
	ctx := context.Background()
	r := replace(t)
	k := r.k
	k.holdSlots(2)
	k.declareLimit("project", "P-TEST", "runs", 1)
	if _, err := r.s2.Adopt(ctx, "rp", "A", "parent-2"); err != nil {
		t.Fatalf("adopt under a ceiling below use: %v", err)
	}
	k.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, set_digest, recorded_at) VALUES (?, 'c1', 'criterion', 1, ?, ?)", r.r2, releaseCriteriaDigest(), k.clock())
	k.exec("INSERT INTO verification_mode (relationship_id, mode, recorded_at) VALUES (?, 'managed', ?)", r.r2, k.clock())
	k.seedReport(r.r2, "A", "rp")
	if acc, err := r.s2.Accept(ctx, "rp", "A", "parent-2", AcceptInput{RuleVersion: verifier}); err != nil || !acc.SlotReleased {
		t.Fatalf("accept under a ceiling below use = %+v, %v", acc, err)
	}
	var tenure int64
	if err := k.s.DB.QueryRow("SELECT tenure FROM execution_slots WHERE subject_key = 'rp/A' AND state = 'released'").Scan(&tenure); err != nil || tenure != 1 {
		t.Fatalf("the original tenure was not the one returned: %d (%v)", tenure, err)
	}
}

// The slot of a node is returned as its recorded holder only when the holder is the parent of a relationship in the node's own execution chain and the slot is held for the plan's project;
// any other holder keeps it and the caller is refused, as capacity refuses it.
func TestAForeignSlotIsNotReturnedAsItsHolder(t *testing.T) {
	// a former parent of the node: a relationship of the node's own chain that the parent former held
	inChain := func(k *releaseKit) {
		now := k.clock()
		if err := storeseed.RecordRelationship(context.Background(), k.s, store.Relationship{ID: "rel-former", IssueKey: "CRW-A", Status: "archived", ParentTaskID: "former", ChildTaskID: "child-1", Generation: 1,
			ArtifactRoots: "[" + jsonString(k.root) + "]", AllowedRecipients: "[\"former\"]", CreatedAt: now, UpdatedAt: now},
			store.Generation{RelationshipID: "rel-former", Number: 1, DispatchRequestID: "dispatch-former", AnchorState: store.AnchorBound, DispatchTurnID: sql.NullString{String: "turn-former", Valid: true}, OpenedAt: now,
				BoundAt: sql.NullString{String: now, Valid: true}}, "host", "host"); err != nil {
			t.Fatal(err)
		}
		k.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES ('rp', 'A', 'rel-former', 1, ?, 'initial', NULL)", dig("former manifest"))
	}
	for name, c := range map[string]struct {
		prepare func(k *releaseKit)
		mutate  string
		refused bool
	}{
		"an unrelated task holds it":                               {mutate: "UPDATE execution_slots SET parent_task_id = 'stranger' WHERE subject_key = 'rp/A'", refused: true},
		"a former parent of the node, held for another project":    {prepare: inChain, mutate: "UPDATE execution_slots SET parent_task_id = 'former', project_key = 'P-OTHER' WHERE subject_key = 'rp/A'", refused: true},
		"a former parent of the node, held for the plan's project": {prepare: inChain, mutate: "UPDATE execution_slots SET parent_task_id = 'former' WHERE subject_key = 'rp/A'"},
	} {
		t.Run(name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			res := k.mustRelease("rp", "A")
			k.seedReport(res.RelationshipID, "A", "rp")
			if c.prepare != nil {
				c.prepare(k)
			}
			k.exec(c.mutate)
			before := allRows(t, k.s.DB)
			_, err := k.accept("rp", "A", AcceptInput{})
			if !c.refused {
				if err != nil || k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = 'rp/A' AND state = 'released' AND released_by = 'former'") != 1 {
					t.Fatalf("accept = %v: the slot of a former parent of the node was not returned as that parent", err)
				}
				return
			}
			if refusalReason(err) != "scope_role_mismatch" {
				t.Fatalf("accept = %v, want scope_role_mismatch", err)
			}
			if !equalRows(before, allRows(t, k.s.DB)) {
				t.Fatal("a refused acceptance changed the store")
			}
			if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = 'rp/A' AND state = 'held'") != 1 {
				t.Fatal("the slot of another holder was released")
			}
		})
	}
}

func TestAdoptRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("a node that has an acceptance", func(t *testing.T) {
		r := replace(t)
		r.k.acceptNode("rp", "A", acceptOpts{Suffix: "-acc"})
		if _, err := r.s2.Adopt(ctx, "rp", "A", "parent-2"); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("adopt = %v", err)
		}
	})
	t.Run("another child is a replacement of the node, not an adoption", func(t *testing.T) {
		r := replace(t)
		r.k.exec("UPDATE relationships SET child_task_id = 'another-child' WHERE relationship_id = ?", r.r2)
		if _, err := r.s2.Adopt(ctx, "rp", "A", "parent-2"); refusalReason(err) != "relationship_conflict" {
			t.Fatalf("adopt = %v", err)
		}
	})
	t.Run("a relationship held by another task", func(t *testing.T) {
		r := replace(t)
		r.k.exec("UPDATE relationships SET parent_task_id = 'someone' WHERE relationship_id = ?", r.r2)
		if _, err := r.s2.Adopt(ctx, "rp", "A", "parent-2"); refusalReason(err) != "scope_role_mismatch" {
			t.Fatalf("adopt = %v", err)
		}
	})
	t.Run("a successor that was closed meanwhile, also for a replay", func(t *testing.T) {
		r := replace(t)
		if _, err := r.s2.Adopt(ctx, "rp", "A", "parent-2"); err != nil {
			t.Fatal(err)
		}
		r.k.exec("UPDATE relationships SET status = 'archived' WHERE relationship_id = ?", r.r2)
		if _, err := r.s2.Adopt(ctx, "rp", "A", "parent-2"); refusalReason(err) != "relationship_not_active" {
			t.Fatalf("a replay of an adoption of a closed relationship = %v", err)
		}
	})
	t.Run("a relationship that was not replaced and is held by another parent", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.mustRelease("rp", "A")
		k.replaceParent("parent-2")
		s2 := k.session("rp", "parent-2", "session-2")
		if _, err := s2.Adopt(ctx, "rp", "A", "parent-2"); refusalReason(err) != "relationship_conflict" {
			t.Fatalf("adopt = %v", err)
		}
		if node, found := k.resumeOf(s2, "rp", "parent-2", "A"); !found || node.Resume != ResumeNeedsOperator {
			t.Fatalf("restart says %+v", node)
		}
	})
}

// What a replacement parent cannot recover is said, not hidden: a release that never bound its child (the managed start is frozen under the previous parent) and a result that is accepted and
// not landed (its merge turn and merged mark belong to the previous parent) read needs_operator, and no child is created for either.
func TestAReplacementParentNamesWhatItCannotRecover(t *testing.T) {
	ctx := context.Background()
	t.Run("a release that never bound its child", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		first := k.sched
		k.claim(first, "rp", "parent", "session-1")
		first.Start = func(context.Context, []byte) (StartAnswer, error) { return StartAnswer{}, errCrash }
		if _, err := first.Release(ctx, "rp", "A", "parent", k.request(false)); !errors.Is(err, errCrash) {
			t.Fatalf("the crash was not reached: %v", err)
		}
		k.replaceParent("parent-2")
		second := k.session("rp", "parent-2", "session-2")
		if node, found := k.resumeOf(second, "rp", "parent-2", "A"); !found || node.Resume != ResumeNeedsOperator {
			t.Fatalf("restart says %+v (found %v)", node, found)
		}
		before := allRows(t, k.s.DB)
		if _, err := second.Release(ctx, "rp", "A", "parent-2", k.request(false)); err == nil {
			t.Fatal("a replacement continued a release frozen under the previous parent")
		}
		if created, _ := k.host.counts(); created != 0 || !equalRows(before, allRows(t, k.s.DB)) {
			t.Fatalf("a child was created or a row written (created %d)", created)
		}
	})
	t.Run("an accepted result that has not landed", func(t *testing.T) {
		k := newJudgeKit(t)
		k.claim(k.sched, "g", "parent", "session-1")
		var rid string
		if err := k.s.DB.QueryRow("SELECT relationship_id FROM dag_acceptances WHERE node_id = 'I'").Scan(&rid); err != nil {
			t.Fatal(err)
		}
		now := k.clock()
		if err := insertSuccessor(k.releaseKit, rid, "rel-g-I-2", "parent-2", now); err != nil {
			t.Fatal(err)
		}
		k.replaceParent("parent-2")
		second := k.session("g", "parent-2", "session-2")
		if node, found := k.resumeOf(second, "g", "parent-2", "I"); !found || node.Resume != ResumeNeedsOperator {
			t.Fatalf("restart says %+v (found %v)", node, found)
		}
		if _, err := second.Adopt(ctx, "g", "I", "parent-2"); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("adopting an accepted node = %v", err)
		}
	})
}

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// insertSuccessor is a replacement relationship seeded as the registry leaves one: the old relationship archived and superseded, the new one active under the new parent for the same child.
func insertSuccessor(k *releaseKit, old, id, parent, now string) error {
	var issue, child string
	if err := k.s.DB.QueryRow("SELECT issue_key, child_task_id FROM relationships WHERE relationship_id = ?", old).Scan(&issue, &child); err != nil {
		return err
	}
	if _, err := k.s.DB.Exec("UPDATE relationships SET status = 'archived', superseded_by = ? WHERE relationship_id = ?", id, old); err != nil {
		return err
	}
	return storeseed.RecordRelationship(context.Background(), k.s, store.Relationship{ID: id, IssueKey: issue, Status: "active", ParentTaskID: parent, ChildTaskID: child, Generation: 1,
		ArtifactRoots: "[" + jsonString(k.root) + "]", AllowedRecipients: "[\"" + parent + "\"]", CreatedAt: now, UpdatedAt: now},
		store.Generation{RelationshipID: id, Number: 1, DispatchRequestID: "dispatch-" + id, AnchorState: store.AnchorBound, DispatchTurnID: sql.NullString{String: "turn-" + id, Valid: true}, OpenedAt: now,
			BoundAt: sql.NullString{String: now, Valid: true}}, "host", "host")
}
