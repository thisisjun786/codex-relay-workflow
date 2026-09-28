package supervisor_test

import "testing"

var resCases = map[int][]string{
	1:  {"test_a_correction_says_what_its_finding_cap_removed", "test_a_cap_that_takes_the_block_says_so_by_name"},
	2:  {"test_a_block_the_message_cannot_carry_refuses_before_a_generation_opens"},
	3:  {"test_a_correction_carrying_no_block_records_that_as_a_result", "test_a_carried_block_is_named_in_the_record_and_in_the_bytes", "test_a_verdict_that_opens_no_correction_says_the_block_had_nowhere_to_go", "test_a_replayed_ruling_reports_what_it_recorded_rather_than_measuring_again"},
	4:  {"test_a_report_that_would_push_the_block_out_is_refused_naming_the_budget", "test_an_ordinary_report_records_what_became_of_the_block", "test_a_report_that_cannot_be_composed_is_refused_rather_than_called_unmeasured"},
	5:  {"test_the_report_projection_names_the_attempt_it_measured", "test_the_projection_is_measured_inside_the_write_lock"},
	6:  {"test_the_command_reports_the_outcome_without_breaking_the_frozen_record"},
	7:  {"test_the_carrier_is_matched_the_way_the_findings_are_normalised", "test_a_padded_restoration_argument_names_the_same_finding", "test_an_empty_restoration_argument_is_refused_rather_than_skipped"},
	8:  {"test_the_option_does_not_overwrite_a_findings_own_disclaimer", "test_the_option_does_not_make_an_invalid_declaration_valid"},
	9:  {"test_each_attempt_records_what_its_own_bytes_carried", "test_an_attempt_that_drops_the_block_records_that_against_its_own_bytes"},
	10: {"test_show_returns_the_outcome_measured_against_the_sent_bytes", "test_show_on_the_revision_the_child_is_sent_to_reports_the_ruling"},
	11: {"test_a_report_no_attempt_will_render_is_not_refused", "test_a_held_delivery_cannot_claim_again_so_its_report_is_not_refused", "test_a_superseded_relationship_can_never_claim_again", "test_an_answered_correction_can_never_claim_again"},
	12: {"test_a_delivery_still_sending_is_measured", "test_a_delivery_held_uncertain_is_measured", "test_a_paused_assignment_is_still_measured_because_resume_exists"},
	13: {"test_two_findings_cannot_both_declare_the_block", "test_a_carrier_with_no_note_is_refused_because_the_note_is_the_block", "test_a_declaration_that_is_not_a_boolean_is_refused_rather_than_dropped"},
	14: {"test_a_note_supplied_through_the_other_input_still_counts", "test_a_declaration_in_one_input_is_not_erased_by_the_other", "test_repeating_the_same_declaration_is_not_a_contradiction"},
	15: {"test_declaring_and_disclaiming_the_same_block_is_refused", "test_the_same_contradiction_is_refused_in_either_order"},
}

func resRun(t *testing.T, id int) {
	t.Helper()
	methods := []string{}
	for _, m := range resCases[id] {
		methods = append(methods, "RestorationDelivery."+m)
	}
	rrRun(t, "test_restoration_visibility", methods)
}
func Test24_RES_1_Replay(t *testing.T)  { resRun(t, 1) }
func Test24_RES_2_Replay(t *testing.T)  { resRun(t, 2) }
func Test24_RES_3_Replay(t *testing.T)  { resRun(t, 3) }
func Test24_RES_4_Replay(t *testing.T)  { resRun(t, 4) }
func Test24_RES_5_Replay(t *testing.T)  { resRun(t, 5) }
func Test24_RES_6_Replay(t *testing.T)  { resRun(t, 6) }
func Test24_RES_7_Replay(t *testing.T)  { resRun(t, 7) }
func Test24_RES_8_Replay(t *testing.T)  { resRun(t, 8) }
func Test24_RES_9_Replay(t *testing.T)  { resRun(t, 9) }
func Test24_RES_10_Replay(t *testing.T) { resRun(t, 10) }
func Test24_RES_11_Replay(t *testing.T) { resRun(t, 11) }
func Test24_RES_12_Replay(t *testing.T) { resRun(t, 12) }
func Test24_RES_13_Replay(t *testing.T) { resRun(t, 13) }
func Test24_RES_14_Replay(t *testing.T) { resRun(t, 14) }
func Test24_RES_15_Replay(t *testing.T) { resRun(t, 15) }
