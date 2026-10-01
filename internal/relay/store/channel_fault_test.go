package store

import (
	"context"
	"database/sql"
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
	// Given: routes.store_incident (routes.py:224) keeps every incident for keep=None and none for
	// keep=0.
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
	// Then: keep=None keeps all three and keep=0 none, as routes.store_incident does.
	checkText(t, "incidents kept for keep=None, keep=0", fmt.Sprintf("%d %d", len(all), len(none)))
}
