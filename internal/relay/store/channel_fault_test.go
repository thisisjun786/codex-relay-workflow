package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func message(id, recipient, staged, state string) SupervisorMessagesRow {
	return SupervisorMessagesRow{MessageID: id, ObligationID: "obl-" + id, ObligationKind: "report", RelationshipID: "rel", Purpose: "report", Kind: "k", SenderTaskID: "parent", RecipientTaskID: recipient, Subject: "s", Packet: "{}", State: state, StagedAt: staged, UpdatedAt: staged}
}

func TestSupervisorMessages_stage_once_and_list_claimable_rows_in_staging_order(t *testing.T) {
	// Given: three messages, one staged twice, one held, and one already sending.
	s := recordStore(t)
	ctx := context.Background()
	first, err := s.StageSupervisorMessage(ctx, message("m2", "sup", "t1", "queued"))
	must(t, err)
	again, err := s.StageSupervisorMessage(ctx, message("m2", "other", "t9", "queued"))
	must(t, err)
	_, err = s.StageSupervisorMessage(ctx, message("m1", "sup", "t1", "deferred_busy"))
	must(t, err)
	_, err = s.StageSupervisorMessage(ctx, message("m3", "sup", "t0", "sending"))
	must(t, err)
	// When: the claimable rows are listed.
	rows, err := s.ClaimableSupervisorMessages(ctx, [3]string{"queued", "deferred_busy", "withheld_pre_send"}, 100, 10)
	must(t, err)
	kept, err := s.SupervisorMessage(ctx, "m2")
	must(t, err)
	byObligation, err := s.SupervisorMessageFor(ctx, "report", "obl-m1")
	must(t, err)
	// Then: INSERT OR IGNORE kept the first staging; order is staged_at then message_id.
	if !first || again || kept.RecipientTaskID != "sup" || kept.AttemptCount != 0 || kept.NextEligibleAt.Valid {
		t.Fatalf("first=%v again=%v kept=%+v", first, again, kept)
	}
	if len(rows) != 2 || rows[0].MessageID != "m1" || rows[1].MessageID != "m2" || byObligation.MessageID != "m1" {
		t.Fatalf("rows=%v byObligation=%v", rows, byObligation.MessageID)
	}
}

func TestSettleSupervisorMessage_moves_only_the_claim_that_holds_it(t *testing.T) {
	// Given: a message sending on attempt 1 under owner-a.
	s := recordStore(t)
	ctx := context.Background()
	m := message("m1", "sup", "t1", "sending")
	m.AttemptCount, m.LeaseOwner = 1, text("owner-a")
	_, err := s.DB.ExecContext(ctx, "INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, attempt_count, lease_owner, lease_until, staged_at, updated_at) VALUES ('m1','o','report','rel','p','k','parent','sup','s','{}','sending',1,'owner-a',50,'t1','t1')")
	must(t, err)
	// When: a stale owner settles it, then the holding owner does.
	stale, err := s.SettleSupervisorMessage(ctx, "m1", "dispatched", sql.NullFloat64{}, sql.NullString{}, "t2", "sending", 1, text("owner-b"))
	must(t, err)
	moved, err := s.SettleSupervisorMessage(ctx, "m1", "withheld_pre_send", sql.NullFloat64{Float64: 70, Valid: true}, text("cap"), "t3", "sending", 1, text("owner-a"))
	must(t, err)
	row, err := s.SupervisorMessage(ctx, "m1")
	must(t, err)
	// Then: only the holder moves it, and the lease is released.
	if stale || !moved || row.State != "withheld_pre_send" || row.HoldReason.String != "cap" || row.NextEligibleAt.Float64 != 70 || row.LeaseOwner.Valid || row.LeaseUntil.Valid {
		t.Fatalf("stale=%v moved=%v row=%+v", stale, moved, row)
	}
}

func TestSupervisorAttempts_record_a_send_and_settle_its_receipt(t *testing.T) {
	// Given: two attempts on one message.
	s := recordStore(t)
	ctx := context.Background()
	for _, a := range []SupervisorAttemptsRow{
		{RequestID: "sreq-2", MessageID: "m1", AttemptNo: 2, Message: "b2", State: "held_uncertain", SendAttempted: "unknown", Record: "{}", SentAt: "t2", ObservedAt: "t2", DeliveryToken: text("tok2")},
		{RequestID: "sreq-1", MessageID: "m1", AttemptNo: 1, Message: "b1", State: "held_uncertain", SendAttempted: "unknown", Record: "{}", SentAt: "t1", ObservedAt: "t1"},
	} {
		must(t, s.InsertSupervisorAttempt(ctx, a))
	}
	// When: the second starts its transport and settles.
	must(t, s.StartSupervisorTransport(ctx, "sreq-2", "t3"))
	must(t, s.SettleSupervisorAttempt(ctx, "sreq-2", "dispatched", "yes", 1, text("turn-9"), `{"ok":true}`, "t4"))
	all, err := s.SupervisorAttempts(ctx, "m1")
	must(t, err)
	latest, err := s.LatestSupervisorAttempt(ctx, "m1")
	must(t, err)
	owner, err := s.SupervisorAttemptMessage(ctx, "sreq-1")
	must(t, err)
	// Then: attempts start unsafe to retry with no turn; settling writes the receipt.
	if all[0].RequestID != "sreq-1" || all[0].RetrySafe != 0 || all[0].TurnID.Valid || all[0].TransportStartedAt.Valid {
		t.Fatalf("first attempt=%+v", all[0])
	}
	if latest.RequestID != "sreq-2" || latest.State != "dispatched" || latest.RetrySafe != 1 || latest.TurnID.String != "turn-9" || latest.TransportStartedAt.String != "t3" || latest.ObservedAt != "t4" || owner != "m1" {
		t.Fatalf("latest=%+v owner=%q", latest, owner)
	}
}

func TestSupervisorReadback_keeps_the_latest_readback(t *testing.T) {
	// Given/When: a message read back twice.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordSupervisorReadback(ctx, SupervisorReadbacksRow{MessageID: "m1", ReadTurnID: "turn-1", Proof: "p1", Verified: "mismatch", ReadAt: "t1"}))
	must(t, s.RecordSupervisorReadback(ctx, SupervisorReadbacksRow{MessageID: "m1", ReadTurnID: "turn-2", Proof: "p2", Verified: "host_read", RequestID: text("sreq-1"), Detail: text("{}"), ReadAt: "t2"}))
	// Then: one row, holding the second.
	row, err := s.SupervisorReadback(ctx, "m1")
	if err != nil || row.ReadTurnID != "turn-2" || row.Verified != "host_read" || row.RequestID.String != "sreq-1" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func job(id, relationship, created string) SyncOutboxRow {
	return SyncOutboxRow{SyncID: id, RelationshipID: relationship, IssueKey: "CRW-1", Target: "coordination_document", TargetRef: "doc", SubjectKind: "verdict", IdentityDigest: "d-" + id, Summary: "s", State: "pending", CreatedAt: created, UpdatedAt: created}
}

func TestSyncTargets_keep_one_target_per_relationship_and_kind(t *testing.T) {
	// Given/When: a target set twice.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.SetSyncTarget(ctx, "rel", "coordination_document", "doc-1", "t1"))
	must(t, s.SetSyncTarget(ctx, "rel", "coordination_document", "doc-2", "t2"))
	// Then: the latest reference stands.
	row, err := s.SyncTarget(ctx, "rel", "coordination_document")
	if err != nil || row.TargetRef != "doc-2" || row.RecordedAt != "t2" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestSyncOutbox_enqueues_once_and_offers_due_unleased_jobs(t *testing.T) {
	// Given: three jobs, one enqueued twice, one leased into the future, one backing off.
	s := recordStore(t)
	ctx := context.Background()
	inserted, err := s.EnqueueSync(ctx, job("s1", "rel", "t1"))
	must(t, err)
	replay, err := s.EnqueueSync(ctx, job("s1", "rel", "t9"))
	must(t, err)
	_, err = s.EnqueueSync(ctx, job("s2", "rel", "t2"))
	must(t, err)
	_, err = s.EnqueueSync(ctx, job("s3", "rel-2", "t3"))
	must(t, err)
	must(t, s.ClaimSync(ctx, "s2", "claimed", "writer", 500, "tok", "t4"))
	must(t, s.FailSync(ctx, "s3", "pending", 1, "boom", 300, "t5"))
	// When: jobs due at 100 are listed, and at 600 for one target.
	due, err := s.NextSyncJobs(ctx, [3]string{"pending", "written", "claimed"}, "", 100, 4)
	must(t, err)
	later, err := s.NextSyncJobs(ctx, [3]string{"pending", "written", "claimed"}, "coordination_document", 600, 4)
	must(t, err)
	snapshot, err := s.SyncSnapshot(ctx, "rel")
	must(t, err)
	// Then: the replay inserted nothing, the lease and backoff hide jobs until they lapse.
	if !inserted || replay || len(due) != 1 || due[0].SyncID != "s1" {
		t.Fatalf("inserted=%v replay=%v due=%v", inserted, replay, due)
	}
	if len(later) != 3 || later[0].SyncID != "s1" || later[1].SyncID != "s2" {
		t.Fatalf("later=%v", later)
	}
	if len(snapshot) != 2 || snapshot[0].CreatedAt != "t1" {
		t.Fatalf("snapshot=%v", snapshot)
	}
}

func TestSyncOutbox_confirmation_is_final(t *testing.T) {
	// Given: a job written, then confirmed twice.
	s := recordStore(t)
	ctx := context.Background()
	_, err := s.EnqueueSync(ctx, job("s1", "rel", "t1"))
	must(t, err)
	must(t, s.ClaimSync(ctx, "s1", "claimed", "writer", 500, "tok", "t2"))
	must(t, s.ConfirmSync(ctx, "s1", "confirmed", text("ext-1"), "block", "t3"))
	must(t, s.ConfirmSync(ctx, "s1", "confirmed", text("ext-1"), "block", "t4"))
	// When: a retry is requested.
	must(t, s.RetrySync(ctx, "s1", "pending", "t5", "confirmed"))
	row, err := s.SyncJob(ctx, "s1")
	must(t, err)
	// Then: it stays confirmed, keeps its first write time, and holds no lease.
	if row.State != "confirmed" || row.WrittenAt.String != "t3" || row.ConfirmedAt.String != "t4" || row.LeaseOwner.Valid || row.ExternalRef.String != "ext-1" {
		t.Fatalf("row=%+v", row)
	}
}

func TestProductRouting_registry_bindings_and_policy_replace_their_record(t *testing.T) {
	// Given: two products, bindings and a policy, each restated once.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RegisterProduct(ctx, "beta", `{"product":"beta"}`, "t1"))
	must(t, s.RegisterProduct(ctx, "alpha", `{"v":1}`, "t1"))
	must(t, s.RegisterProduct(ctx, "alpha", `{"v":2}`, "t2"))
	must(t, s.BindProduct(ctx, ProductBindingsRow{ProductKey: "alpha", Kind: "project", Ref: "p1", Record: "{}", RecordedAt: "t1"}))
	must(t, s.BindProduct(ctx, ProductBindingsRow{ProductKey: "alpha", Kind: "issue", Ref: "i1", Record: `{"v":1}`, RecordedAt: "t1"}))
	must(t, s.BindProduct(ctx, ProductBindingsRow{ProductKey: "alpha", Kind: "issue", Ref: "i1", Record: `{"v":2}`, ObservedAt: text("o"), RecordedAt: "t2"}))
	must(t, s.SetRoutingPolicy(ctx, "project_creation", `{"a":1}`, "basis-1", "t1"))
	must(t, s.SetRoutingPolicy(ctx, "project_creation", `{"a":2}`, "basis-2", "t2"))
	// When: they are read.
	registry, err := s.ProductRegistry(ctx, "alpha")
	must(t, err)
	registries, err := s.ProductRegistries(ctx)
	must(t, err)
	bindings, err := s.ProductBindings(ctx, "alpha")
	must(t, err)
	policy, err := s.RoutingPolicy(ctx, "project_creation")
	must(t, err)
	// Then: each restatement replaced the record; listings keep key order.
	if registry.Record != `{"v":2}` || len(registries) != 2 || registries[0].ProductKey != "alpha" {
		t.Fatalf("registry=%v registries=%v", registry, registries)
	}
	if len(bindings) != 2 || bindings[0].Kind != "issue" || bindings[0].Record != `{"v":2}` || bindings[0].ObservedAt.String != "o" {
		t.Fatalf("bindings=%v", bindings)
	}
	if policy.Record != `{"a":2}` || policy.Basis != "basis-2" {
		t.Fatalf("policy=%+v", policy)
	}
}

func route(severity string, goal, classification sql.NullString) IncidentRoutesRow {
	return IncidentRoutesRow{FaultID: "f1", ProductKey: "alpha", Workspace: "w", Disposition: "hold", Stage: "held", Target: "{}", Origin: "o", ClaimedSeverity: severity, Goal: goal, Classification: classification, Detail: text("d"), CreatedAt: "t", UpdatedAt: "t"}
}

func TestIncidentRoute_severity_only_rises_and_kept_fields_survive_a_restatement(t *testing.T) {
	// Given: a route claimed broken with a goal and a classification.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.UpsertIncidentRoute(ctx, route("broken", text("goal-1"), text("c1")), true))
	// When: it is restated as notice, keeping the goal, with no classification.
	restated := route("notice", text("goal-ignored"), sql.NullString{})
	restated.UpdatedAt = "t2"
	must(t, s.UpsertIncidentRoute(ctx, restated, false))
	kept, err := s.IncidentRoute(ctx, "f1")
	must(t, err)
	// And: restated as degraded, replacing the goal.
	must(t, s.UpsertIncidentRoute(ctx, route("degraded", text("goal-2"), sql.NullString{}), true))
	replaced, err := s.IncidentRoute(ctx, "f1")
	must(t, err)
	// Then: the claimed severity never falls, and KEEP and COALESCE hold the prior values.
	if kept.ClaimedSeverity != "broken" || kept.Goal.String != "goal-1" || kept.Classification.String != "c1" || kept.CreatedAt != "t" || kept.UpdatedAt != "t2" {
		t.Fatalf("kept=%+v", kept)
	}
	if replaced.ClaimedSeverity != "broken" || replaced.Goal.String != "goal-2" {
		t.Fatalf("replaced=%+v", replaced)
	}
}

func TestIncidentRoute_updates_target_stage_report_and_rotation(t *testing.T) {
	// Given: two routes.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.UpsertIncidentRoute(ctx, route("notice", sql.NullString{}, sql.NullString{}), false))
	second := route("notice", sql.NullString{}, sql.NullString{})
	second.FaultID = "f2"
	must(t, s.UpsertIncidentRoute(ctx, second, false))
	// When: f1 is retargeted, settled, reported and checked twice, f2 once.
	must(t, s.SetIncidentTarget(ctx, "f1", `{"project":"p"}`, "t2"))
	must(t, s.SettleIncidentRoute(ctx, "f1", "filed", "done", "t3"))
	must(t, s.SetIncidentReported(ctx, "f1", `{"r":1}`, "t4"))
	must(t, s.CheckIncidentRoute(ctx, "f1"))
	must(t, s.CheckIncidentRoute(ctx, "f2"))
	must(t, s.CheckIncidentRoute(ctx, "f1"))
	row, err := s.IncidentRoute(ctx, "f1")
	must(t, err)
	other, err := s.IncidentRoute(ctx, "f2")
	must(t, err)
	// Then: each column moves, and every check sends the route behind every other one.
	if row.Target != `{"project":"p"}` || row.Stage != "filed" || row.Detail.String != "done" || row.Reported.String != `{"r":1}` || row.UpdatedAt != "t4" {
		t.Fatalf("row=%+v", row)
	}
	if row.CheckedSeq != 3 || other.CheckedSeq != 2 {
		t.Fatalf("checked f1=%d f2=%d", row.CheckedSeq, other.CheckedSeq)
	}
}

func TestRouteIncidents_sequence_replace_and_keep_only_the_newest(t *testing.T) {
	// Given: three incidents of one fault, keeping two, and a restatement of the newest.
	s := recordStore(t)
	ctx := context.Background()
	two := int64(2)
	must(t, s.StoreRouteIncident(ctx, "i1", "f1", "r1", "t1", &two, false))
	must(t, s.StoreRouteIncident(ctx, "i2", "f1", "r2", "t2", &two, false))
	must(t, s.StoreRouteIncident(ctx, "i3", "f1", "r3", "t3", &two, false))
	must(t, s.StoreRouteIncident(ctx, "i3", "f1", "r3-kept", "t4", &two, false))
	kept, err := s.RouteIncidents(ctx, "f1")
	must(t, err)
	// When: the newest is replaced, keeping every incident.
	must(t, s.StoreRouteIncident(ctx, "i2", "f1", "r2-new", "t5", nil, true))
	replaced, err := s.RouteIncidents(ctx, "f1")
	must(t, err)
	// Then: the oldest was dropped; DO NOTHING kept i3; a replacement takes the next sequence
	// after the retained rows (MAX(recorded_seq)+1 = 4).
	if len(kept) != 2 || kept[0].IncidentID != "i2" || kept[1].Record != "r3" || kept[1].RecordedSeq != 3 {
		t.Fatalf("kept=%+v", kept)
	}
	if len(replaced) != 2 || replaced[1].IncidentID != "i2" || replaced[1].Record != "r2-new" || replaced[1].RecordedSeq != 4 {
		t.Fatalf("replaced=%+v", replaced)
	}
}

func TestRouteIncidents_keep_none_keeps_all_and_keep_zero_deletes_all_like_python(t *testing.T) {
	// Given: Python's store_incident with keep=None and keep=0, three incidents each (routes.py:224).
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("TMPDIR", root)
	python := pythonStoreValue(t, `
import os, sys
from codex_session_relay.store import Store
from codex_session_relay.clock import FakeClock
from codex_session_relay import routes
s = Store(os.path.join(sys.argv[1], "ri.sqlite3")); c = FakeClock()
counts = []
for keep in (None, 0):
    fid = "f-" + str(keep)
    with s.transaction() as db:
        for key in ("a", "b", "c"):
            routes.store_incident(db, c, fid, {"occurrenceKey": key}, keep=keep)
    counts.append(str(s.db.execute("SELECT COUNT(*) FROM route_incidents WHERE fault_id = ?", (fid,)).fetchone()[0]))
print(" ".join(counts))
`, root)
	if python != "3 0" {
		t.Fatalf("Python kept %q incidents for keep=None, keep=0", python)
	}
	// When: Go stores three incidents with keep nil and with keep 0.
	s := recordStore(t)
	ctx := context.Background()
	zero := int64(0)
	for _, id := range []string{"a", "b", "c"} {
		must(t, s.StoreRouteIncident(ctx, "all-"+id, "f-all", "{}", "t", nil, false))
		must(t, s.StoreRouteIncident(ctx, "none-"+id, "f-none", "{}", "t", &zero, false))
	}
	all, err := s.RouteIncidents(ctx, "f-all")
	must(t, err)
	none, err := s.RouteIncidents(ctx, "f-none")
	must(t, err)
	// Then: the same counts.
	if got := fmt.Sprintf("%d %d", len(all), len(none)); got != python {
		t.Fatalf("Go kept %q, Python %q", got, python)
	}
}

func fault(id string) FaultLedgerRow {
	return FaultLedgerRow{FaultID: id, Product: "crw", FaultClass: "report_omitted", Component: "relay", Severity: "broken", Signature: "{}", Scope: "{}", ScopeKey: "crw:CRW", State: "observed", Detail: text("d"), FirstSeenAt: "t1", LastSeenAt: "t1", UpdatedAt: "t1"}
}

func TestFaultLedger_opens_at_cycle_one_and_keeps_its_first_reference(t *testing.T) {
	// Given: a fault opened with other counters in the row.
	s := recordStore(t)
	ctx := context.Background()
	f := fault("flt-1")
	f.Cycle, f.OccurrenceCount, f.ReopenCount = 9, 9, 9
	must(t, s.OpenFaultLedger(ctx, f))
	opened, err := s.FaultLedger(ctx, "flt-1")
	must(t, err)
	// Then (at open): faults.py:1175 writes the literals cycle 1, occurrence_count 0, reopen_count 0,
	// whatever the row carried, and episode takes its column default 1.
	if opened.Cycle != 1 || opened.OccurrenceCount != 0 || opened.ReopenCount != 0 || opened.Episode != 1 || opened.ExternalRef.Valid {
		t.Fatalf("opened=%+v", opened)
	}
	f.Cycle, f.OccurrenceCount, f.ReopenCount = 1, 0, 0
	// When: it is observed, set, and given two references.
	must(t, s.ObserveFault(ctx, "flt-1", "open", 1, "degraded", 1, 3, 1, text("d2"), `{"publish":true}`, "t2", sql.NullString{}, sql.NullString{}, "t2"))
	must(t, s.SetFaultState(ctx, "flt-1", "fix_pending", "t3"))
	must(t, s.MaterializeFaultRef(ctx, "flt-1", "REL-1", "t4"))
	must(t, s.MaterializeFaultRef(ctx, "flt-1", "REL-2", "t5"))
	row, err := s.FaultLedger(ctx, "flt-1")
	must(t, err)
	// Then: cycle 1 and zero counts at open, reopen_count adds, and the first ref stands.
	if row.Cycle != 1 || row.Episode != 1 || row.OccurrenceCount != 3 || row.ReopenCount != 1 || row.State != "fix_pending" || row.ExternalRef.String != "REL-1" || row.Suppression.String != `{"publish":true}` {
		t.Fatalf("row=%+v", row)
	}
}

func TestFaultAliases_resolve_to_the_canonical_fault(t *testing.T) {
	// Given: an alias for flt-1.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordFaultAlias(ctx, "flt-old", "flt-1", "t"))
	// When/Then: the alias resolves; any other id is its own; a second alias row is refused.
	if id, err := s.FaultAliasTarget(ctx, "flt-old"); err != nil || id != "flt-1" {
		t.Fatalf("alias: %q %v", id, err)
	}
	if id, err := s.FaultAliasTarget(ctx, "flt-2"); err != nil || id != "flt-2" {
		t.Fatalf("plain id: %q %v", id, err)
	}
	if err := s.RecordFaultAlias(ctx, "flt-old", "flt-3", "t"); err == nil {
		t.Fatal("an alias was re-pointed")
	}
}

func TestFaultTargets_set_team_and_project_together(t *testing.T) {
	// Given: a target set, then restated without a project.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.SetFaultTarget(ctx, "crw:CRW", "team-1", "crw", text("proj-1"), "t1"))
	must(t, s.SetFaultTarget(ctx, "crw:CRW", "team-2", "crw", sql.NullString{}, "t2"))
	// When: it is read through the join and each table.
	target, err := s.FaultTarget(ctx, "crw:CRW")
	must(t, err)
	team, err := s.FaultTargetTeam(ctx, "crw:CRW")
	must(t, err)
	project, err := s.FaultTargetProject(ctx, "crw:CRW")
	must(t, err)
	// Then: both halves carry the restatement.
	if target.TrackerRef != "team-2" || target.Product.String != "crw" || target.ProjectRef.Valid || team.RecordedAt != "t2" || project.RecordedAt != "t2" {
		t.Fatalf("target=%+v team=%+v project=%+v", target, team, project)
	}
}

func occurrence(id, key string, episode int64) FaultOccurrencesRow {
	return FaultOccurrencesRow{OccurrenceID: id, FaultID: "flt-1", Episode: episode, OccurrenceKey: key, Severity: "broken", Evidence: "[]", EvidenceDigest: "d", RecordedAt: "t", RecordedTS: 1}
}

func TestFaultOccurrences_record_once_per_episode_and_prune_the_oldest(t *testing.T) {
	// Given: three occurrences, one of them recorded twice.
	s := recordStore(t)
	ctx := context.Background()
	for i, o := range []FaultOccurrencesRow{occurrence("o1", "k1", 1), occurrence("o2", "k2", 1), occurrence("o3", "k1", 2)} {
		recorded, err := s.RecordFaultOccurrence(ctx, o)
		if err != nil || !recorded {
			t.Fatalf("occurrence %d: %v %v", i, recorded, err)
		}
	}
	duplicate, err := s.RecordFaultOccurrence(ctx, occurrence("o1", "k1", 1))
	must(t, err)
	newest, err := s.FaultOccurrences(ctx, "flt-1", 2, true)
	must(t, err)
	oldest, err := s.FaultOccurrences(ctx, "flt-1", 1, false)
	must(t, err)
	// When: all but the newest two are pruned.
	removed, err := s.PruneFaultOccurrences(ctx, "flt-1", 2)
	must(t, err)
	left, err := s.FaultOccurrences(ctx, "flt-1", 10, false)
	must(t, err)
	// Then: a replay is not recorded; order is by rowid; pruning drops the oldest.
	if duplicate || len(newest) != 2 || newest[0].OccurrenceID != "o3" || oldest[0].OccurrenceID != "o1" {
		t.Fatalf("duplicate=%v newest=%v oldest=%v", duplicate, newest, oldest)
	}
	if removed != 1 || len(left) != 2 || left[0].OccurrenceID != "o2" {
		t.Fatalf("removed=%d left=%v", removed, left)
	}
}

func TestFaultTimeline_appends_in_sequence_and_counts_by_kind(t *testing.T) {
	// Given: three entries of two kinds.
	s := recordStore(t)
	ctx := context.Background()
	for _, kind := range []string{"occurrence", "fix", "occurrence"} {
		must(t, s.AppendFaultTimeline(ctx, FaultTimelineRow{FaultID: "flt-1", Cycle: 1, Kind: kind, RefID: text("r"), RecordedAt: "t", RecordedTS: 1}))
	}
	// When/Then: they read in sequence and count by kind.
	rows, err := s.FaultTimeline(ctx, "flt-1")
	must(t, err)
	count, err := s.FaultTimelineCount(ctx, "flt-1", "occurrence")
	must(t, err)
	if len(rows) != 3 || rows[0].Seq >= rows[2].Seq || rows[1].Kind != "fix" || count != 2 {
		t.Fatalf("rows=%v count=%d", rows, count)
	}
}

func TestFaultAdoption_first_adoption_stands_until_materialized(t *testing.T) {
	// Given: an adoption recorded twice.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordFaultAdoption(ctx, FaultAdoptionsRow{FaultID: "flt-1", ExternalRef: "REL-1", Scope: "{}", State: "pending", CreatedAt: "t1", UpdatedAt: "t1"}))
	must(t, s.RecordFaultAdoption(ctx, FaultAdoptionsRow{FaultID: "flt-1", ExternalRef: "REL-2", Scope: "{}", State: "pending", CreatedAt: "t2", UpdatedAt: "t2"}))
	// When: it materializes.
	must(t, s.MaterializeFaultAdoption(ctx, "flt-1", "t3"))
	// Then: the first reference, now materialized.
	row, err := s.FaultAdoption(ctx, "flt-1")
	if err != nil || row.ExternalRef != "REL-1" || row.State != "materialized" || row.UpdatedAt != "t3" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestFaultPolicy_and_limit_are_replaced_by_their_key(t *testing.T) {
	// Given: a policy and a limit, each set twice.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.SetFaultPolicy(ctx, FaultPoliciesRow{Product: "crw", FaultClass: "c", Severity: "degraded", Threshold: sql.NullInt64{Int64: 3, Valid: true}, Reason: "r1", UpdatedAt: "t1"}))
	must(t, s.SetFaultPolicy(ctx, FaultPoliciesRow{Product: "crw", FaultClass: "c", Severity: "degraded", WindowSeconds: sql.NullFloat64{Float64: 60, Valid: true}, Reason: "r2", UpdatedAt: "t2"}))
	must(t, s.SetFaultLimit(ctx, "crw", "open_record", 5, 3600, "t1"))
	must(t, s.SetFaultLimit(ctx, "crw", "open_record", 2, 60, "t2"))
	// When/Then: the second statement of each is what stands.
	policy, err := s.FaultPolicy(ctx, "crw", "c", "degraded")
	must(t, err)
	limit, err := s.FaultLimit(ctx, "crw", "open_record")
	must(t, err)
	if policy.Threshold.Valid || policy.WindowSeconds.Float64 != 60 || policy.Reason != "r2" || limit.MaxCount != 2 || limit.WindowSeconds != 60 {
		t.Fatalf("policy=%+v limit=%+v", policy, limit)
	}
}

func TestFaultRemediations_record_once_and_list_the_newest_oldest_first(t *testing.T) {
	// Given: three remediations, one recorded twice.
	s := recordStore(t)
	ctx := context.Background()
	for _, id := range []string{"r1", "r2", "r3", "r1"} {
		_, err := s.RecordFaultRemediation(ctx, FaultRemediationsRow{RemediationID: id, FaultID: "flt-1", Cycle: 1, Kind: "fix", Ref: id, RecordedAt: "t"})
		must(t, err)
	}
	again, err := s.RecordFaultRemediation(ctx, FaultRemediationsRow{RemediationID: "r2", FaultID: "flt-1", Cycle: 1, Kind: "fix", Ref: "x", RecordedAt: "t"})
	must(t, err)
	// When/Then: the newest two come back oldest first; the replay recorded nothing.
	rows, err := s.FaultRemediations(ctx, "flt-1", 2)
	if err != nil || again || len(rows) != 2 || rows[0].RemediationID != "r2" || rows[1].RemediationID != "r3" || rows[0].Ref != "r2" {
		t.Fatalf("rows=%v again=%v err=%v", rows, again, err)
	}
}

func TestFaultPublications_queue_claim_attempt_and_payload(t *testing.T) {
	// Given: two publications of one fault.
	s := recordStore(t)
	ctx := context.Background()
	for _, p := range []FaultPublicationsRow{
		{PublicationID: "pub-1", FaultID: "flt-1", Kind: "open_record", TriggerKey: "open", Cycle: 1, Summary: "s", IdentityDigest: "d", State: "pending", Attempts: 9, CreatedAt: "t", UpdatedAt: "t"},
		{PublicationID: "pub-2", FaultID: "flt-1", Kind: "append_comment", TriggerKey: "recur", Cycle: 1, Summary: "s", IdentityDigest: "d", State: "pending", CreatedAt: "t", UpdatedAt: "t"},
	} {
		must(t, s.QueueFaultPublication(ctx, p))
	}
	// When: pub-1 is claimed with two attempts, the newest issued, and its payload written twice.
	must(t, s.ClaimFaultPublication(ctx, "pub-1", "claimed", "tok", "owner", 90, 1, "t2"))
	first, err := s.RecordFaultPublicationAttempt(ctx, FaultPublicationAttemptsRow{PublicationID: "pub-1", Attempt: 1, Owner: "a", ClaimedAt: "t", ClaimedTS: 1})
	must(t, err)
	second, err := s.RecordFaultPublicationAttempt(ctx, FaultPublicationAttemptsRow{PublicationID: "pub-1", Attempt: 2, Owner: "b", Takeover: 1, ClaimedAt: "t", ClaimedTS: 2})
	must(t, err)
	must(t, s.IssueCurrentFaultAttempt(ctx, "pub-1", "t3", 3))
	must(t, s.WriteFaultPublicationPayload(ctx, FaultPublicationPayloadsRow{PublicationID: "pub-1", Payload: text("{}"), UpdatedAt: "t1"}))
	must(t, s.WriteFaultPublicationPayload(ctx, FaultPublicationPayloadsRow{PublicationID: "pub-1", ProjectRef: text("proj"), HoldReason: text("hold"), UpdatedAt: "t2", TargetMode: text("m")}))
	claimed, err := s.FaultPublication(ctx, "pub-1")
	must(t, err)
	comments, err := s.FaultPublications(ctx, "flt-1", "append_comment", "", 0, 10)
	must(t, err)
	all, err := s.FaultPublications(ctx, "flt-1", "", "", 0, 10)
	must(t, err)
	attempts, err := s.FaultPublicationAttempts(ctx, "pub-1", 10)
	must(t, err)
	payload, err := s.FaultPublicationPayload(ctx, "pub-1")
	must(t, err)
	// Then: queuing starts at zero attempts; filters narrow; only the newest attempt was issued.
	if claimed.State != "claimed" || claimed.ClaimToken.String != "tok" || claimed.Attempts != 1 || claimed.LeaseUntil.Float64 != 90 {
		t.Fatalf("claimed=%+v", claimed)
	}
	if len(comments) != 1 || comments[0].PublicationID != "pub-2" || comments[0].Attempts != 0 || len(all) != 2 {
		t.Fatalf("comments=%v all=%d", comments, len(all))
	}
	if second != first+1 || len(attempts) != 2 || attempts[0].IssuedAt.Valid || attempts[1].IssuedAt.String != "t3" || attempts[1].Takeover != 1 {
		t.Fatalf("attempts=%+v", attempts)
	}
	if payload.Payload.Valid || payload.ProjectRef.String != "proj" || payload.TargetMode.String != "m" {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestFaultLinks_open_at_revision_zero_and_follow_the_project(t *testing.T) {
	// Given/When: a link opened then pointed at a project.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.OpenFaultLink(ctx, FaultLinksRow{FaultID: "flt-1", ExternalRef: "REL-1", State: "unlinked", Revision: 7, UpdatedAt: "t1"}))
	must(t, s.SetFaultLinkProject(ctx, "flt-1", text("proj"), "linked", "t2"))
	// Then: revision 0 whatever was passed; the project and state move.
	row, err := s.FaultLink(ctx, "flt-1")
	if err != nil || row.Revision != 0 || row.ProjectRef.String != "proj" || row.State != "linked" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestFaultBudget_counts_uses_in_the_window_and_returns_an_unspent_one(t *testing.T) {
	// Given: two uses at 10 and 20, and a duplicate ref.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.ConsumeFaultBudget(ctx, "crw", "open_record", "p:1", "t", 10))
	must(t, s.ConsumeFaultBudget(ctx, "crw", "open_record", "p:2", "t", 20))
	duplicate := s.ConsumeFaultBudget(ctx, "crw", "open_record", "p:1", "t", 30)
	// When: the window after 15 is counted, then p:2 is returned.
	inWindow, err := s.FaultBudgetUsed(ctx, "crw", "open_record", 15)
	must(t, err)
	must(t, s.ReturnFaultBudget(ctx, "crw", "open_record", "p:2"))
	afterReturn, err := s.FaultBudgetUsed(ctx, "crw", "open_record", 0)
	must(t, err)
	// Then: one ref is consumed once; the window is strict; a returned unit is gone.
	if duplicate == nil || inWindow != 1 || afterReturn != 1 {
		t.Fatalf("duplicate=%v inWindow=%d afterReturn=%d", duplicate, inWindow, afterReturn)
	}
	if _, err := s.FaultBudgetUse(ctx, "crw", "open_record", "p:2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("returned use still present: %v", err)
	}
}

func notification(id string) FaultNotificationsRow {
	return FaultNotificationsRow{NotificationID: id, FaultID: "flt-1", Product: "crw", Kind: "blocking", Cycle: 1, CreatedAt: "t1", UpdatedAt: "t1"}
}

func TestFaultNotifications_raise_again_only_when_withdrawn(t *testing.T) {
	// Given: a notification raised, reserved, then raised again.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RaiseFaultNotification(ctx, notification("n1"), "pending", "withdrawn"))
	must(t, s.ReserveFaultNotification(ctx, "n1", "reserved", "tok", "owner", 50, "t2"))
	must(t, s.RaiseFaultNotification(ctx, notification("n1"), "pending", "withdrawn"))
	reserved, err := s.FaultNotification(ctx, "n1")
	must(t, err)
	// When: it is withdrawn and raised again.
	_, err = s.DB.ExecContext(ctx, "UPDATE fault_notifications SET state='withdrawn', last_error='x' WHERE notification_id='n1'")
	must(t, err)
	raised := notification("n1")
	raised.UpdatedAt = "t3"
	must(t, s.RaiseFaultNotification(ctx, raised, "pending", "withdrawn"))
	again, err := s.FaultNotification(ctx, "n1")
	must(t, err)
	// Then: a live one is untouched by a raise; a withdrawn one is pending again, error cleared.
	if reserved.State != "reserved" || reserved.Attempts != 1 || reserved.Token.String != "tok" {
		t.Fatalf("reserved=%+v", reserved)
	}
	if again.State != "pending" || again.LastError.Valid || again.UpdatedAt != "t3" {
		t.Fatalf("again=%+v", again)
	}
}

func TestFaultNotifications_lapse_deliver_and_list_by_state(t *testing.T) {
	// Given: two reserved notifications, leases ending at 10 and 100.
	s := recordStore(t)
	ctx := context.Background()
	for i, id := range []string{"n1", "n2"} {
		must(t, s.RaiseFaultNotification(ctx, notification(id), "pending", "withdrawn"))
		must(t, s.ReserveFaultNotification(ctx, id, "reserved", "tok", "owner", float64(10+90*i), "t2"))
	}
	// When: leases lapse at 50, and n2 is delivered.
	must(t, s.LapseFaultNotifications(ctx, "uncertain", "t3", "reserved", 50))
	must(t, s.DeliverFaultNotification(ctx, "n2", "delivered", "t4", "ack-1"))
	uncertain, err := s.FaultNotificationsInState(ctx, "uncertain", 0, 10)
	must(t, err)
	delivered, err := s.FaultNotification(ctx, "n2")
	must(t, err)
	// Then: only the lapsed lease became uncertain; delivery records its ack and drops the token.
	if len(uncertain) != 1 || uncertain[0].NotificationID != "n1" || uncertain[0].Token.Valid {
		t.Fatalf("uncertain=%v", uncertain)
	}
	if delivered.State != "delivered" || delivered.AckRef.String != "ack-1" || delivered.DeliveredAt.String != "t4" || delivered.Token.Valid {
		t.Fatalf("delivered=%+v", delivered)
	}
}

func TestFaultCursors_restart_their_page_count_when_moved(t *testing.T) {
	// Given: a cursor with pages counted.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.WriteFaultCursor(ctx, "readings", text(`{"a":1}`), "t1"))
	_, err := s.DB.ExecContext(ctx, "UPDATE fault_cursors SET pages = 4")
	must(t, err)
	// When: it is moved, and a second source has no position.
	must(t, s.WriteFaultCursor(ctx, "readings", text(`{"a":2}`), "t2"))
	must(t, s.WriteFaultCursor(ctx, "deliveries", sql.NullString{}, "t2"))
	rows, err := s.FaultCursors(ctx)
	must(t, err)
	// Then: the moved cursor restarts at zero pages; a null position stays NULL.
	byName := map[string]FaultCursorsRow{}
	for _, row := range rows {
		byName[row.Source] = row
	}
	if byName["readings"].Pages != 0 || byName["readings"].Position.String != `{"a":2}` || byName["deliveries"].Position.Valid {
		t.Fatalf("cursors=%+v", rows)
	}
}
