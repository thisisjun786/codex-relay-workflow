package sync

import (
	"strings"
	"testing"
)

func Test23_SO_11_StructuralIntegrity(t *testing.T) {
	for _, replacement := range [][][]string{{{"blockFormat: v2", "blockFormat: v900"}}, {{"```text\n", ""}, {"\n```\n", "\n"}}, {{"\n```\n", "\n```\nsmuggled payload\n"}}, {{"\n```\n", "\n```\n\n"}}} {
		replay(t, actions(a("parse", "replace", replacement), a("reconcile", "replace", replacement)))
	}
	replay(t, actions(a("parse", "document", "fenced-legacy"), a("reconcile", "document", "fenced-legacy")))
}
func Test23_SO_12_LegacyIdentity(t *testing.T) {
	verdictReplay(t, "test_a_verdict_with_no_canonical_criteria_keeps_the_identity_it_always_had", "test_a_legacy_ruling_keeps_its_id_when_criteria_are_registered_afterwards")
	replay(t, actions(a("enqueue"), a("target", "ref", "doc-ref"), a("enqueue"), a("operation")))
	if got := IdentityDigest("coordination_document", "doc-ref", "verdict", "rel-1", "e1", 1, "r1", "verified", nil, nil); len(got) != 64 {
		t.Fatal(got)
	}
}
func Test23_SO_13_CriteriaRulingIdentity(t *testing.T) {
	verdictReplay(t, "test_a_re_review_against_edited_criteria_enqueues_its_own_job", "test_criteria_edited_away_and_back_still_enqueues_the_newest_ruling", "test_a_replay_enqueues_nothing_further", "test_the_summary_names_the_criteria_set_the_ruling_rests_on", "test_the_summary_is_what_tells_a_reader_which_ruling_stands", "test_an_older_job_retried_after_a_newer_one_still_names_its_own_ruling", "test_a_labelled_job_reconciles_and_completes_from_its_stored_identity")
	steps := []action{a("target", "ref", doc), a("enqueue", "criteria", strings.Repeat("d", 64), "summary", "criteria set dddddddddddd, ruling 1"), a("claim"), a("fail"), a("enqueue", "criteria", strings.Repeat("e", 64), "ruling", 2, "summary", "criteria set eeeeeeeeeeee, ruling 2"), a("enqueue", "criteria", strings.Repeat("d", 64), "ruling", 3, "summary", "criteria set dddddddddddd, ruling 3"), a("next"), a("claim"), a("reconcile"), a("complete"), a("retry", "job", 0), a("operation", "job", 0), a("snapshot")}
	replay(t, steps)
}
func Test23_SO_14_EnqueueJournal(t *testing.T) {
	replay(t, []action{a("target", "ref", doc), a("enqueue", "criteria", strings.Repeat("d", 64)), a("enqueue", "criteria", strings.Repeat("d", 64), "summary", "ignored summary"), a("snapshot")})
}
