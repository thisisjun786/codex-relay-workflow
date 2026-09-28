package supervisor_test

import "testing"

var rcfCases = map[int][]string{
	1:  {"TheVerdictPathCarriesTheCorrectionForm.test_the_plain_request_carries_the_five_sections", "TheVerdictPathCarriesTheCorrectionForm.test_the_composed_request_carries_both_forms"},
	2:  {"OnlyWhatIsRuledViolatedIsInScope.test_a_mixed_verdict_scopes_the_one_finding_marked_needs_changes"},
	3:  {"OnlyWhatIsRuledViolatedIsInScope.test_every_composed_instruction_reads_the_findings_through_fix_scope"},
	4:  {"OnlyWhatIsRuledViolatedIsInScope.test_the_task_line_quotes_the_parent_summary_within_fix_scope"},
	5:  {"WhatTheVerdictRecordCannotAnswer.test_a_verdict_that_named_no_criterion_names_the_gap"},
	6:  {"WhatTheVerdictRecordCannotAnswer.test_a_finding_without_a_note_says_its_reason_is_not_recorded"},
	7:  {"WhatTheVerdictRecordCannotAnswer.test_a_review_is_the_source_when_the_verdict_recorded_none"},
	8:  {"ARecordMissingItsFields.test_the_plain_request_names_what_the_record_lacks", "ARecordMissingItsFields.test_the_composed_request_names_what_the_record_lacks"},
	9:  {"ARecordMissingItsFields.test_a_finding_without_its_disposition_says_so_on_both_paths"},
	10: {"NothingRuledViolatedOrOwed.test_the_plain_request", "NothingRuledViolatedOrOwed.test_the_composed_request"},
	11: {"ManyFindings.test_many_long_findings_drop_no_section_and_keep_their_counts"},
	12: {"TheWorstLegalCorrectionStillRenders.test_a_mixed_verdict_with_a_review", "TheWorstLegalCorrectionStillRenders.test_a_review_naming_every_disposition", "TheWorstLegalCorrectionStillRenders.test_nothing_named_at_all"},
	13: {"TheParentsTextStaysOnItsLine.test_the_plain_request", "TheParentsTextStaysOnItsLine.test_the_composed_request"},
	14: {"TheParentsTextStaysOnItsLine.test_a_turn_id_stays_on_its_line"},
	15: {"NoParentTextOpensASection.test_the_plain_request", "NoParentTextOpensASection.test_the_composed_request", "NoParentTextOpensASection.test_a_bare_repository_name_on_the_scope_line"},
}

func rcfRun(t *testing.T, id int) {
	t.Helper()
	rrRun(t, "test_revision_correction_form", rcfCases[id])
}
func Test24_RCF_1_Replay(t *testing.T)  { rcfRun(t, 1) }
func Test24_RCF_2_Replay(t *testing.T)  { rcfRun(t, 2) }
func Test24_RCF_3_Replay(t *testing.T)  { rcfRun(t, 3) }
func Test24_RCF_4_Replay(t *testing.T)  { rcfRun(t, 4) }
func Test24_RCF_5_Replay(t *testing.T)  { rcfRun(t, 5) }
func Test24_RCF_6_Replay(t *testing.T)  { rcfRun(t, 6) }
func Test24_RCF_7_Replay(t *testing.T)  { rcfRun(t, 7) }
func Test24_RCF_8_Replay(t *testing.T)  { rcfRun(t, 8) }
func Test24_RCF_9_Replay(t *testing.T)  { rcfRun(t, 9) }
func Test24_RCF_10_Replay(t *testing.T) { rcfRun(t, 10) }
func Test24_RCF_11_Replay(t *testing.T) { rcfRun(t, 11) }
func Test24_RCF_12_Replay(t *testing.T) { rcfRun(t, 12) }
func Test24_RCF_13_Replay(t *testing.T) { rcfRun(t, 13) }
func Test24_RCF_14_Replay(t *testing.T) { rcfRun(t, 14) }
func Test24_RCF_15_Replay(t *testing.T) { rcfRun(t, 15) }
