package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func turn(id, holder, state, requested string, tenure int64, ready int64) MergeTurnsRow {
	return MergeTurnsRow{TurnID: id, TargetKey: "repo|main", Repository: "repo", BaseRef: "main", ProjectKey: "P", HolderTaskID: holder, HolderHostID: "h", CandidateHead: "c1", DeclaredReady: ready, State: state, Tenure: tenure, RequestedAt: requested, UpdatedAt: requested}
}

func TestMergeTurnReaders_answer_occupancy_claims_and_tenure(t *testing.T) {
	// Given: a holder, two waiters (one ready) and a closed tenure of the first waiter.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertMergeTurn(ctx, turn("mtn-h", "alpha", "holding", "t1", 1, 1)))
	must(t, s.InsertMergeTurn(ctx, turn("mtn-old", "beta", "returned", "t0", 1, 0)))
	must(t, s.InsertMergeTurn(ctx, turn("mtn-w1", "beta", "waiting", "t3", 2, 1)))
	must(t, s.InsertMergeTurn(ctx, turn("mtn-w2", "gamma", "waiting", "t2", 1, 0)))
	// When: the readers are asked.
	occupant, err := s.MergeTargetOccupant(ctx, "repo|main")
	must(t, err)
	claim, err := s.LiveMergeClaim(ctx, "repo|main", "beta")
	must(t, err)
	tenure, err := s.HighestMergeTenure(ctx, "repo|main", "beta")
	must(t, err)
	fresh, err := s.HighestMergeTenure(ctx, "repo|main", "nobody")
	must(t, err)
	ready, err := s.ReadyMergeWaiters(ctx, "repo|main")
	must(t, err)
	all, err := s.MergeTurnsForTarget(ctx, "repo|main")
	must(t, err)
	outstanding, err := s.OutstandingMergeClaims(ctx, "beta")
	must(t, err)
	// Then: each answers as mergeturn.py reads it.
	if occupant.TurnID != "mtn-h" || occupant.State != "holding" {
		t.Fatalf("occupant=%v", occupant)
	}
	if claim.TurnID != "mtn-w1" || tenure != 2 || fresh != 0 {
		t.Fatalf("claim=%v tenure=%d fresh=%d", claim.TurnID, tenure, fresh)
	}
	if len(ready) != 1 || ready[0].TurnID != "mtn-w1" {
		t.Fatalf("ready waiters=%v", ready)
	}
	if len(all) != 4 || all[0].TurnID != "mtn-old" || all[3].TurnID != "mtn-w1" {
		t.Fatalf("target rows are in request order: %v", all)
	}
	if len(outstanding) != 1 || outstanding[0].TurnID != "mtn-w1" {
		t.Fatalf("outstanding=%v", outstanding)
	}
}

func TestMergeTurnTransitions_record_what_python_writes(t *testing.T) {
	// Given: a holding turn that lands, and a waiter.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertMergeTurn(ctx, turn("mtn-h", "alpha", "holding", "t1", 1, 1)))
	must(t, s.InsertMergeTurn(ctx, turn("mtn-w", "beta", "waiting", "t2", 1, 0)))
	// When: the holder lands with a sha, the close is restated without one, the waiter declares
	// readiness on a new head and is promoted.
	must(t, s.CloseMergeTurn(ctx, "mtn-h", "landed", "merged", "t3", text("sha-1"), text("base-1")))
	must(t, s.CloseMergeTurn(ctx, "mtn-h", "landed", "merged again", "t4", sql.NullString{}, sql.NullString{}))
	must(t, s.DeclareMergeReadiness(ctx, "mtn-w", 1, "c2", "waiting", sql.NullString{}, "t5"))
	must(t, s.PromoteMergeTurn(ctx, "mtn-w", "holding", "t6"))
	landed, err := s.MergeTurn(ctx, "mtn-h")
	must(t, err)
	promoted, err := s.MergeTurn(ctx, "mtn-w")
	must(t, err)
	// Then: a restated close keeps the recorded shas; readiness and promotion move the waiter.
	if landed.LandedSHA.String != "sha-1" || landed.ObservedBaseSHA.String != "base-1" || landed.CloseReason.String != "merged again" || landed.ClosedAt.String != "t4" {
		t.Fatalf("landed=%+v", landed)
	}
	if promoted.State != "holding" || promoted.HeldAt.String != "t6" || promoted.DeclaredReady != 1 || promoted.CandidateHead != "c2" {
		t.Fatalf("promoted=%+v", promoted)
	}
}

func TestMergeLedger_records_one_fact_per_idempotency_key_in_order(t *testing.T) {
	// Given: two entries, the second written twice with different evidence.
	s := recordStore(t)
	ctx := context.Background()
	entry := MergeTurnLedgerRow{EntryID: "e2", TurnID: "mtn", Kind: "transition", EvidenceKind: "claim", ActorTaskID: "a", Evidence: "first", IdempotencyKey: "request:1", RecordedAt: "t2"}
	must(t, s.WriteMergeLedger(ctx, MergeTurnLedgerRow{EntryID: "e1", TurnID: "mtn", Kind: "attestation", EvidenceKind: "note", ActorTaskID: "a", Evidence: "x", IdempotencyKey: "note:1", RecordedAt: "t1"}))
	must(t, s.WriteMergeLedger(ctx, entry))
	entry.EntryID, entry.Evidence = "e3", "replayed"
	must(t, s.WriteMergeLedger(ctx, entry))
	// When: the ledger is read.
	ledger, err := s.MergeLedger(ctx, "mtn")
	must(t, err)
	kept, err := s.MergeLedgerEntry(ctx, "mtn", "request:1")
	must(t, err)
	// Then: the replay is one fact, the first, and entries keep recording order.
	if len(ledger) != 2 || ledger[0].EntryID != "e1" || kept.Evidence != "first" || kept.EntryID != "e2" {
		t.Fatalf("ledger=%v kept=%+v", ledger, kept)
	}
}

func TestMergeChecks_converge_on_the_check_id_and_read_newest_first(t *testing.T) {
	// Given: one check restated with a new result, and an older one.
	s := recordStore(t)
	ctx := context.Background()
	check := MergeTurnChecksRow{CheckID: "chk-2", TurnID: "mtn", HeadSHA: "h", BaseSHA: "b", Required: "[]", ChecksDigest: "cd", Checks: "[]", ReviewDigest: "rd", Review: "{}", Result: "refused", RefusalReason: text("merge_turn_not_held"), RecordedAt: "t2"}
	must(t, s.RecordMergeCheck(ctx, MergeTurnChecksRow{CheckID: "chk-1", TurnID: "mtn", HeadSHA: "h0", BaseSHA: "b", Required: "[]", ChecksDigest: "x", Checks: "[]", ReviewDigest: "y", Review: "{}", Result: "current", RecordedAt: "t1"}))
	must(t, s.RecordMergeCheck(ctx, check))
	check.Result, check.RefusalReason, check.RecordedAt, check.HeadSHA = "current", sql.NullString{}, "t3", "ignored"
	must(t, s.RecordMergeCheck(ctx, check))
	// When: the checks are read.
	rows, err := s.MergeChecks(ctx, "mtn")
	must(t, err)
	// Then: the restatement updated result, reason and time only; newest first.
	if len(rows) != 2 || rows[0].CheckID != "chk-2" || rows[0].Result != "current" || rows[0].RefusalReason.Valid || rows[0].HeadSHA != "h" || rows[0].RecordedAt != "t3" {
		t.Fatalf("rows=%+v", rows)
	}
}

func slot(id, subject, parent, project string, tenure int64) ExecutionSlotsRow {
	return ExecutionSlotsRow{SlotID: id, SubjectKind: "assignment", SubjectKey: subject, ParentTaskID: parent, ProjectKey: project, InitiativeKey: text("INIT"), Tenure: tenure, State: "held", ReservedBy: parent, ReservedAt: "t" + id}
}

func TestExecutionSlots_count_held_runs_per_scope_and_keep_every_tenure(t *testing.T) {
	// Given: S1 held and released, S1 held again, S2 held under another project.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertExecutionSlot(ctx, slot("1", "S1", "a", "PA", 1)))
	released, err := s.ReleaseExecutionSlot(ctx, "1", "released", "tr", "a", "completed", "held")
	must(t, err)
	must(t, s.InsertExecutionSlot(ctx, slot("2", "S1", "a", "PA", 2)))
	must(t, s.InsertExecutionSlot(ctx, slot("3", "S2", "b", "PB", 1)))
	// When: the counters and readers are asked.
	store, err := s.RunsIn(ctx, "store", "store", "held")
	must(t, err)
	project, err := s.RunsIn(ctx, "project", "PA", "held")
	must(t, err)
	initiative, err := s.RunsIn(ctx, "initiative", "INIT", "held")
	must(t, err)
	newest, err := s.ExecutionSlot(ctx, "assignment", "S1")
	must(t, err)
	tenures, err := s.ExecutionSlotTenures(ctx, "assignment", "S1")
	must(t, err)
	top, err := s.HighestSlotTenure(ctx, "assignment", "S1")
	must(t, err)
	held, err := s.HeldExecutionSlot(ctx, "assignment", "S1", "held")
	must(t, err)
	listing, err := s.ExecutionSlotsInState(ctx, "held")
	must(t, err)
	// Then: released slots never count; readers see the newest tenure first.
	if !released || store != 2 || project != 1 || initiative != 2 {
		t.Fatalf("released=%v store=%d project=%d initiative=%d", released, store, project, initiative)
	}
	if newest.SlotID != "2" || len(tenures) != 2 || tenures[0].Tenure != 2 || top != 2 || held.SlotID != "2" {
		t.Fatalf("newest=%v tenures=%v top=%d held=%v", newest.SlotID, tenures, top, held.SlotID)
	}
	if len(listing) != 2 || listing[0].SlotID != "2" {
		t.Fatalf("held listing is in reservation order: %v", listing)
	}
}

func TestReleaseExecutionSlot_releases_once(t *testing.T) {
	// Given: one held slot.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertExecutionSlot(ctx, slot("1", "S1", "a", "PA", 1)))
	// When: it is released twice with different reasons.
	first, err := s.ReleaseExecutionSlot(ctx, "1", "released", "t1", "a", "completed", "held")
	must(t, err)
	second, err := s.ReleaseExecutionSlot(ctx, "1", "released", "t2", "b", "failed", "held")
	must(t, err)
	row, err := s.ExecutionSlot(ctx, "assignment", "S1")
	must(t, err)
	// Then: the guard in the UPDATE lets only the first write.
	if !first || second || row.ReleaseReason.String != "completed" || row.ReleasedBy.String != "a" {
		t.Fatalf("first=%v second=%v row=%+v", first, second, row)
	}
}

func TestExecutionLimits_update_in_place_and_list_by_dimension(t *testing.T) {
	// Given: two ceilings on one scope, one declared twice, one not enforced.
	s := recordStore(t)
	ctx := context.Background()
	limit := ExecutionLimitsRow{LimitID: "lim-runs", ScopeKind: "project", ScopeKey: "P", Dimension: "runs", Unit: "runs", Ceiling: 2, Enforce: 1, DeclaredBy: "sup", Source: "s", Revision: 1, DeclaredAt: "t1", UpdatedAt: "t1"}
	must(t, s.DeclareExecutionLimit(ctx, limit))
	must(t, s.DeclareExecutionLimit(ctx, ExecutionLimitsRow{LimitID: "lim-fd", ScopeKind: "project", ScopeKey: "P", Dimension: "fds", Unit: "fds", Ceiling: 9, Enforce: 0, DeclaredBy: "sup", Source: "s", Revision: 1, DeclaredAt: "t1", UpdatedAt: "t1"}))
	limit.Ceiling, limit.Revision, limit.DeclaredAt, limit.UpdatedAt = 3, 2, "t9", "t2"
	must(t, s.DeclareExecutionLimit(ctx, limit))
	// When: the limits are read.
	one, err := s.ExecutionLimit(ctx, "lim-runs")
	must(t, err)
	all, err := s.ExecutionLimits(ctx, "project", "P")
	must(t, err)
	enforced, err := s.EnforcedExecutionLimits(ctx, "project", "P")
	must(t, err)
	// Then: a re-declaration updates the ceiling and revision but keeps declared_at.
	if one.Ceiling != 3 || one.Revision != 2 || one.DeclaredAt != "t1" || one.UpdatedAt != "t2" {
		t.Fatalf("limit=%+v", one)
	}
	if len(all) != 2 || all[0].Dimension != "fds" || len(enforced) != 1 || enforced[0].Dimension != "runs" {
		t.Fatalf("all=%v enforced=%v", all, enforced)
	}
}

func TestExecutionUsage_keeps_the_latest_observation(t *testing.T) {
	// Given: a dimension observed twice.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.ObserveExecutionUsage(ctx, ExecutionUsageRow{ScopeKind: "project", ScopeKey: "P", Dimension: "fds", Observed: 4, ObservedBy: "a", Method: "lsof", ObservedAt: "t1"}))
	must(t, s.ObserveExecutionUsage(ctx, ExecutionUsageRow{ScopeKind: "project", ScopeKey: "P", Dimension: "fds", Observed: 7.5, ObservedBy: "b", Method: "proc", ObservedAt: "t2"}))
	// When/Then: one row holding the latest measurement; an unmeasured dimension has none.
	row, err := s.ExecutionUsage(ctx, "project", "P", "fds")
	if err != nil || row.Observed != 7.5 || row.ObservedBy != "b" || row.Method != "proc" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	if _, err := s.ExecutionUsage(ctx, "project", "P", "dollars"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unmeasured dimension: %v", err)
	}
}

func agreement(id, region, left, right, state string, tenure int64) EditAgreementsRow {
	return EditAgreementsRow{AgreementID: id, RegionID: region, Repository: "repo", BaseRevision: "rev1", LeftProject: left, RightProject: right, PeerLinkID: "lnk", ProposerTaskID: "a", ConstraintText: "keep", State: state, Tenure: tenure, ProposedAt: "t" + id, UpdatedAt: "t" + id}
}

func TestEditRegion_is_classified_once(t *testing.T) {
	// Given: a region recorded as source.
	s := recordStore(t)
	ctx := context.Background()
	region := EditRegionsRow{RegionID: "reg-1", Repository: "repo", BaseRevision: "rev1", Path: "a.py", RegionKind: "file", RegionClass: "source", RecordedAt: "t1"}
	must(t, s.RecordEditRegion(ctx, region))
	// When: it is recorded again as generated.
	region.RegionClass, region.RecordedAt = "generated", "t2"
	must(t, s.RecordEditRegion(ctx, region))
	// Then: ON CONFLICT DO NOTHING keeps the first classification.
	row, err := s.EditRegion(ctx, "reg-1")
	if err != nil || row.RegionClass != "source" || row.RecordedAt != "t1" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestEditAgreements_lifecycle_updates_as_python_writes_them(t *testing.T) {
	// Given: a region with a proposed agreement between A and B.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordEditRegion(ctx, EditRegionsRow{RegionID: "reg-1", Repository: "repo", BaseRevision: "rev1", Path: "a.py", RegionKind: "file", RegionClass: "source", RecordedAt: "t"}))
	must(t, s.InsertEditAgreement(ctx, agreement("agr-1", "reg-1", "A", "B", "proposed", 1)))
	// When: both sides accept, it is agreed, then carried onto a successor.
	must(t, s.AcceptEditAgreementSide(ctx, "agr-1", "left", "t2"))
	must(t, s.AcceptEditAgreementSide(ctx, "agr-1", "right", "t3"))
	must(t, s.SetEditAgreementState(ctx, "agr-1", "agreed", "t4"))
	live, err := s.LiveEditAgreement(ctx, "reg-1", "A", "B")
	must(t, err)
	must(t, s.SupersedeEditAgreement(ctx, "agr-1", "released", "reaffirmed onto rev2", "agr-2", "t5"))
	carried, err := s.EditAgreement(ctx, "agr-1")
	must(t, err)
	top, err := s.HighestAgreementTenure(ctx, "reg-1", "A", "B")
	must(t, err)
	// Then: each side's acceptance lands in its own column; a carried one is no longer live.
	if live.AgreementID != "agr-1" || live.LeftAcceptedAt.String != "t2" || live.RightAcceptedAt.String != "t3" || live.State != "agreed" {
		t.Fatalf("live=%+v", live)
	}
	if carried.State != "released" || carried.SupersededBy.String != "agr-2" || carried.CloseReason.String != "reaffirmed onto rev2" || top != 1 {
		t.Fatalf("carried=%+v top=%d", carried, top)
	}
	if _, err := s.LiveEditAgreement(ctx, "reg-1", "A", "B"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a superseded agreement is still live: %v", err)
	}
}

func TestEditAgreements_reopen_only_open_agreements_on_the_superseded_revision(t *testing.T) {
	// Given: agreements on rev1 in proposed, agreed and withdrawn states.
	s := recordStore(t)
	ctx := context.Background()
	for _, a := range []EditAgreementsRow{agreement("agr-p", "r1", "A", "B", "proposed", 1), agreement("agr-a", "r2", "A", "B", "agreed", 1), agreement("agr-w", "r3", "A", "B", "proposed", 1)} {
		must(t, s.InsertEditAgreement(ctx, a))
	}
	must(t, s.CloseEditAgreement(ctx, "agr-w", "withdrawn", "no longer needed", "t2"))
	// When: rev1 is superseded.
	must(t, s.ReopenEditAgreements(ctx, "repo", "rev1", "reopened", "t3"))
	// Then: open agreements reopen; the closed one keeps its state and close reason.
	for id, want := range map[string]string{"agr-p": "reopened", "agr-a": "reopened", "agr-w": "withdrawn"} {
		row, err := s.EditAgreement(ctx, id)
		if err != nil || row.State != want {
			t.Fatalf("%s: %+v %v", id, row, err)
		}
	}
}

func TestRegionAgreements_join_each_agreement_to_its_place_in_proposal_order(t *testing.T) {
	// Given: two regions with an agreement each, proposed in the opposite order to their ids.
	s := recordStore(t)
	ctx := context.Background()
	for _, r := range []string{"r1", "r2"} {
		must(t, s.RecordEditRegion(ctx, EditRegionsRow{RegionID: r, Repository: "repo", BaseRevision: "rev1", Path: r + ".py", RegionKind: "symbol", RegionKey: "f", RegionClass: "generated", RegenerateFrom: text("gen.sh"), RecordedAt: "t"}))
	}
	late, early := agreement("agr-1", "r1", "A", "B", "proposed", 1), agreement("agr-2", "r2", "A", "B", "proposed", 1)
	late.ProposedAt, early.ProposedAt = "t9", "t1"
	must(t, s.InsertEditAgreement(ctx, late))
	must(t, s.InsertEditAgreement(ctx, early))
	// When/Then: the listing carries each agreement's place and follows proposal order.
	rows, err := s.RegionAgreements(ctx, "repo")
	if err != nil || len(rows) != 2 || rows[0].AgreementID != "agr-2" || rows[0].RegionPath != "r2.py" || rows[0].RegionKind != "symbol" || rows[0].RegenerateFrom.String != "gen.sh" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestEditFollowups_converge_and_move_through_acceptance_and_settlement(t *testing.T) {
	// Given: a follow-up recorded twice, and a second one.
	s := recordStore(t)
	ctx := context.Background()
	f := EditFollowupsRow{FollowupID: "fup-2", AgreementID: "agr", TriggerText: "when x", AcceptanceText: "y", State: "open", RecordedBy: "a", RecordedAt: "t2", UpdatedAt: "t2"}
	must(t, s.RecordEditFollowup(ctx, f))
	f.TriggerText = "changed"
	must(t, s.RecordEditFollowup(ctx, f))
	must(t, s.RecordEditFollowup(ctx, EditFollowupsRow{FollowupID: "fup-1", AgreementID: "agr", TriggerText: "t", AcceptanceText: "a", State: "open", RecordedBy: "a", RecordedAt: "t3", UpdatedAt: "t3"}))
	// When: it is accepted, then settled.
	must(t, s.AcceptEditFollowup(ctx, "fup-2", "beta", "PB", "accepted", "t4"))
	accepted, err := s.EditFollowup(ctx, "fup-2")
	must(t, err)
	must(t, s.SettleEditFollowup(ctx, "fup-2", "done", text("shipped"), "t5"))
	settled, err := s.EditFollowup(ctx, "fup-2")
	must(t, err)
	all, err := s.EditFollowups(ctx, "agr")
	must(t, err)
	// Then: the replay kept the first text; acceptance and settlement write their columns.
	if accepted.TriggerText != "when x" || accepted.AssigneeTaskID.String != "beta" || accepted.AssigneeProject.String != "PB" || accepted.AcceptedAt.String != "t4" || accepted.State != "accepted" {
		t.Fatalf("accepted=%+v", accepted)
	}
	if settled.State != "done" || settled.CloseReason.String != "shipped" || len(all) != 2 || all[0].FollowupID != "fup-2" {
		t.Fatalf("settled=%+v all=%v", settled, all)
	}
}

func TestEditRevisionMarks_hold_one_successor_per_revision(t *testing.T) {
	// Given: rev1 marked as superseded by rev2.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordEditRevisionMark(ctx, EditRevisionMarksRow{MarkID: "m1", Repository: "repo", FromRevision: "rev1", ToRevision: "rev2", Actor: "a", RecordedAt: "t"}))
	// When: a second successor is marked for rev1.
	err := s.RecordEditRevisionMark(ctx, EditRevisionMarksRow{MarkID: "m2", Repository: "repo", FromRevision: "rev1", ToRevision: "rev3", Actor: "a", RecordedAt: "t"})
	mark, readErr := s.EditRevisionMark(ctx, "repo", "rev1")
	// Then: UNIQUE(repository, from_revision) refuses it; rev2 has no outgoing mark.
	if err == nil || readErr != nil || mark.ToRevision != "rev2" {
		t.Fatalf("second successor err=%v mark=%+v readErr=%v", err, mark, readErr)
	}
	if _, err := s.EditRevisionMark(ctx, "repo", "rev2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("current revision has a mark: %v", err)
	}
}

func TestEditReaffirmation_records_the_carry_of_an_agreement(t *testing.T) {
	// Given/When: a carry recorded with one side's condition revision absent.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordEditReaffirmation(ctx, EditReaffirmationsRow{AgreementID: "agr-2", PredecessorID: "agr-1", Actor: "a", ActorProject: "A", FromRevision: "rev1", ToRevision: "rev2", ConstraintRevision: "rev1", LeftConditionRevision: text("rev2"), RecordedAt: "t"}))
	// Then: it reads back with the absent side NULL.
	row, err := s.EditReaffirmation(ctx, "agr-2")
	if err != nil || row.PredecessorID != "agr-1" || row.LeftConditionRevision.String != "rev2" || row.RightConditionRevision.Valid {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}
