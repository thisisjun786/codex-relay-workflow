package faults

import (
	"context"
	"fmt"
	"testing"
)

// Every FN replay below runs the same CLI transitions in Go and live Python.
// f1ReplayCLI compares exit status, stdout, stderr, and every fault_* table row
// after each transition, so the assertions cover the complete notification
// state rather than a hand-picked projection.
func fnReplayFault(t *testing.T) (context.Context, string, string, string) {
	t.Helper()
	ctx, gd, pd := f1ReplayStores(t)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
	answer := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"rel","turn":"turn"},"occurrenceKey":"one","scope":{"projectKey":"P"}}`})
	return ctx, gd, pd, answer["faultId"].(string)
}

func fnRaise(t *testing.T, ctx context.Context, gd, pd, fault, reason string) map[string]any {
	t.Helper()
	return f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-raise", "--fault", fault, "--reason", reason})
}

func fnReserve(t *testing.T, ctx context.Context, gd, pd string) map[string]any {
	t.Helper()
	answer := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "1"})
	return answer["reserved"].([]any)[0].(map[string]any)
}

func Test22_FN_1_OneNotificationWholeOutput(t *testing.T) {
	noticeReplay(t, "ABrokenFaultReachesTheLevelAboveOnce", "test_one_notification_is_one_message_across_ticks_and_a_restart")
}

func Test22_FN_2_NoticeIdentifiersWholeOutput(t *testing.T) {
	for _, tc := range [][2]string{
		{"ABrokenFaultReachesTheLevelAboveOnce", "test_the_bytes_say_what_the_fault_is_and_nothing_it_recorded"},
		{"WhatTheSecondAuditFound", "test_an_adopted_reference_that_is_not_an_identifier_is_not_sent"},
		{"WhatTheSecondAuditFound", "test_an_observed_issue_key_is_not_what_the_notice_names"},
		{"WhatTheSeventhAuditFound", "test_an_issue_a_project_fault_names_that_is_not_an_identifier_is_left_out"},
		{"WhatTheSeventhAuditFound", "test_a_published_linear_issue_link_travels_with_the_notice"},
		{"WhatTheSeventhAuditFound", "test_a_link_of_any_other_shape_is_said_to_exist_and_not_carried"},
		{"WhatDevinFoundOnTheLastHeads", "test_a_product_name_the_ledger_accepts_is_one_a_notice_carries"},
	} {
		t.Run(tc[1], func(t *testing.T) { noticeReplay(t, tc[0], tc[1]) })
	}
}

func Test22_FN_3_DecisionWholeOutput(t *testing.T) {
	noticeReplay(t, "ABrokenFaultReachesTheLevelAboveOnce", "test_a_decision_goes_up_as_a_decision")
	ctx, gd, pd, id := fnReplayFault(t)
	n := fnRaise(t, ctx, gd, pd, id, "operator-choice")
	r := fnReserve(t, ctx, gd, pd)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-ack", "--notification", n["notificationId"].(string), "--token", r["token"].(string), "--ref", "decision-message"})
}

func Test22_FN_4_InvalidIdentifiersWholeOutput(t *testing.T) {
	noticeReplay(t, "WhatTheThirdAuditFound", "test_a_project_key_that_is_not_an_identifier_is_never_sent")
	noticeReplay(t, "WhatTheFifthFinalReviewFound", "test_a_stored_product_with_a_newline_is_never_carried")
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_ledger SET product='bad' || char(10) WHERE fault_id='" + id + "'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "1"})
}

func Test22_FN_5_IneligibleLevelSpendsNothingWholeOutput(t *testing.T) {
	for _, method := range []string{"test_an_archived_supervisor_is_not_woken_and_nothing_is_spent", "test_no_supervisor_bound_keeps_it_pending_with_the_reason_and_writes_nothing", "test_a_paused_assignment_is_never_reserved", "test_a_parent_the_host_reports_archived_withholds_it_without_a_reservation"} {
		t.Run(method, func(t *testing.T) { noticeReplay(t, "TheLevelAboveThatCannotBeToldIsNotWoken", method) })
	}
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "1"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-attention"})
}

func Test22_FN_6_UncertainSettlementWholeOutput(t *testing.T) {
	noticeReplay(t, "WhatTheFirstAuditFound", "test_a_real_send_is_never_settled_as_not_sent")
	for _, method := range []string{"test_a_lost_answer_lapses_to_uncertain_and_a_readback_settles_it", "test_a_claim_that_died_before_its_transport_is_sent_once_after_recovery"} {
		t.Run(method, func(t *testing.T) { noticeReplay(t, "WhatMayHaveBeenSentIsNeverSentAgain", method) })
	}
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	n := fnReserve(t, ctx, gd, pd)
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_notifications SET lease_until=0 WHERE notification_id='" + n["notificationId"].(string) + "'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications", "--notification-state", "uncertain"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reconcile", "--notification", n["notificationId"].(string), "--delivered", "yes", "--ref", "host-read"})
	// FN-6 / m10: another caller cannot attest arrival for the daemon's
	// transport when the supervisor channel has no recorded send.
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_notifications SET state='uncertain',owner='relay-daemon',delivered_at=NULL,ack_ref=NULL WHERE notification_id='" + n["notificationId"].(string) + "'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reconcile", "--notification", n["notificationId"].(string), "--delivered", "yes", "--ref", "somebody says it arrived"})
}

func Test22_FN_7_BudgetAndRefundWholeOutput(t *testing.T) {
	noticeReplay(t, "ABudgetHoldsANotificationAndNeverDropsIt", "test_a_spent_budget_holds_the_second_until_its_window_passes")
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "one")
	fnRaise(t, ctx, gd, pd, id, "two")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-limit", "--product", "crw", "--kind", "notification", "--max-count", "1", "--window", "3600"})
	r := fnReserve(t, ctx, gd, pd)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-fail", "--notification", r["notificationId"].(string), "--token", r["token"].(string), "--error", "not sent"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "2"})
}

func Test22_FN_8_ReservationGatesNoticeWholeOutput(t *testing.T) {
	for _, tc := range [][2]string{
		{"ANoticeGoesOnlyWhileItsNotificationIsReserved", "test_a_notice_that_waits_neither_blocks_a_report_nor_goes_without_a_reservation"},
		{"WhatTheArchitectReviewAsked", "test_a_queued_notice_whose_reservation_lapsed_does_not_hold_a_report_back"},
		{"WhatTheArchitectReviewAsked", "test_the_channel_readers_take_a_notice_row"},
		{"WhatDevinFoundOnTheFinalHeads", "test_a_notice_moved_where_nobody_is_above_holds_no_report_back"},
		{"WhatDevinFoundOnTheFinalHeads", "test_a_staging_that_fails_after_the_reservation_holds_no_report_back"},
	} {
		t.Run(tc[1], func(t *testing.T) { noticeReplay(t, tc[0], tc[1]) })
	}
}

func Test22_FN_9_SharedSendCapWholeOutput(t *testing.T) {
	for _, tc := range [][2]string{
		{"WhatTheFourthAuditFound", "test_a_notice_takes_only_what_the_reports_left_of_the_tick_cap"},
		{"WhatTheFifthAuditFound", "test_an_attempt_that_raised_after_its_transport_spends_the_tick_cap"},
	} {
		t.Run(tc[1], func(t *testing.T) { noticeReplay(t, tc[0], tc[1]) })
	}
}

func Test22_FN_10_WithdrawnNoticeWholeOutput(t *testing.T) {
	noticeReplay(t, "WhatTheArchitectReviewAsked", "test_a_blocking_notice_whose_fault_withdrew_before_it_left_never_goes")
	noticeReplay(t, "WhatTheFirstAuditFound", "test_a_cause_that_comes_back_raises_the_withdrawn_notification_again")
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_ledger SET state='withdrawn' WHERE fault_id='" + id + "'", "UPDATE fault_notifications SET state='withdrawn' WHERE fault_id='" + id + "'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications", "--notification-state", "withdrawn"})
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
}

func Test22_FN_11_CurrentProjectAddressWholeOutput(t *testing.T) {
	noticeReplay(t, "WhatTheFirstAuditFound", "test_a_notice_follows_what_its_fault_is_about_now")
	noticeReplay(t, "WhatDevinFoundOnTheSecondHead", "test_a_fault_moved_to_another_project_is_not_told_to_the_one_it_left")
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-move", "--fault", id, "--scope", `{"projectKey":"OTHER"}`})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
}

func Test22_FN_12_ProjectFaultWholeOutput(t *testing.T) {
	noticeReplay(t, "WhatTheSeventhAuditFound", "test_a_fault_about_a_project_and_no_relationship_reaches_its_supervisor_once")
	ctx, gd, pd := f1ReplayStores(t)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
	a := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"managed_start_failed","severity":"broken","signature":{"issueKey":"PROJECT","receiptStatus":"failed"},"occurrenceKey":"managed:one:failed","scope":{"projectKey":"P"}}`})
	fnRaise(t, ctx, gd, pd, a["faultId"].(string), "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
}

func Test22_FN_13_AnchorWishesWholeOutput(t *testing.T) {
	noticeReplay(t, "WhatTheThirdFinalReviewFound", "test_a_routed_defect_on_a_paused_assignment_is_held_back")
	noticeReplay(t, "WhatTheThirdFinalReviewFound", "test_a_completion_check_whose_parent_cannot_be_contacted_is_held_back")
	ctx, gd, pd, id := fnReplayFault(t)
	f1SeedBoth(t, ctx, gd, pd, []string{f1Relationship, "INSERT INTO relationship_scope VALUES('rel','P','stamp')", "UPDATE relationships SET status='paused' WHERE relationship_id='rel'"})
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "1"})
}

func Test22_FN_14_OneBlockedRecipientDoesNotHideOthersWholeOutput(t *testing.T) {
	noticeReplay(t, "WhatDevinFoundOnTheFirstHead", "test_a_capped_notice_goes_to_the_supervisor_who_took_over")
	noticeReplay(t, "WhatDevinFoundOnTheFirstHead", "test_a_parent_the_host_cannot_observe_does_not_hold_other_notices_back")
	noticeReplay(t, "WhatDevinFoundOnTheLastHeads", "test_a_no_contact_reading_dated_in_the_future_is_measured_again")
	ctx, gd, pd, id := fnReplayFault(t)
	for i := 0; i < 3; i++ {
		fnRaise(t, ctx, gd, pd, id, fmt.Sprintf("decision-%d", i))
	}
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-limit", "--product", "crw", "--kind", "notification", "--max-count", "2", "--window", "3600"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "3"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
}
