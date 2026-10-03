package dagsched

import (
	"strings"
	"testing"
)

// CRW-411: the page, the skill and the command specs say the same things about narrowing, the release policy, the results and the measurements, so a command, table, key, state, kind or reason that
// is added or renamed in the code cannot go undescribed.
func TestPolicyPageSkillAndSpecsNameTheSameThings(t *testing.T) {
	page := readText(t, "../../../docs/relay/dag-scheduler.md")
	plans := readText(t, "../../../docs/relay/dag-plans.md")
	skill := readText(t, "../../../plugins/crw/skills/crw-run/references/region-grades.md")
	specs := readText(t, "../argparse/specs.json")
	for _, command := range []string{"dag-release-policy-record", "dag-landing-result-record", "dag-measurements"} {
		for where, text := range map[string]string{"docs/relay/dag-scheduler.md": page, "the crw-run skill (region-grades.md)": skill} {
			if !strings.Contains(text, command) {
				t.Errorf("%s does not name %s", where, command)
			}
		}
		if !strings.Contains(specs, "{\"name\":\""+command+"\"") {
			t.Errorf("argparse/specs.json has no spec for %s", command)
		}
	}
	for _, name := range []string{"dag_release_policy", "dag_landing_results", "dag_pass_release_policy"} {
		if !strings.Contains(page, name) {
			t.Errorf("the page does not name the zone table %s", name)
		}
		if !strings.Contains(plans, name) {
			t.Errorf("the plan page does not list the zone table %s", name)
		}
	}
	names := []string{OptimismOn, OptimismOff, AbsentNoRecordedPass, AbsentNoLandings, AbsentNoObservations, AbsentNotRecorded, AbsentNoneRecorded, AbsentTableMissing}
	names = append(names, ResultKinds...)
	for _, name := range names {
		if !strings.Contains(page, name) {
			t.Errorf("the page does not describe %s", name)
		}
	}
	// the keys of the printed objects and the settings, in the page and in the skill the parent reads
	for _, key := range []string{"release_policy", "held_by_policy", "local_optimistic", "transitions", "transitions_omitted", "window", "handling_seconds", "red_merges", "clean_run", "narrowed", "policy_seq",
		"conflict_handling_seconds", "post_merge_red", "stale_base_judgements", "returns_to_child", "round_trips", "by_grade", "against_tip", "unattributed_observations", "hunks", "cancelled_after_release",
		"duplicate_nodes", "discarded_nodes", "with_a_result", "limited_by", "held_slots"} {
		if !strings.Contains(page, key) {
			t.Errorf("the page does not describe the key %s", key)
		}
	}
	for _, say := range []string{"narrowed", "disposition_conflict", "held_by_policy", "release_policy", "dag-landing-result-record", "dag-measurements", "absent", "drift", "Keep optimism honest"} {
		if !strings.Contains(skill, say) {
			t.Errorf("the skill does not say %q", say)
		}
	}
	// what the policy is, said where the parent reads it: advice, not a gate, and what it cannot see
	for _, say := range []string{"stops no child", "revokes no release", "changes no merge gate"} {
		if !strings.Contains(page, say) || !strings.Contains(skill, say) {
			t.Errorf("the page and the skill both say %q", say)
		}
	}
	if !strings.Contains(page, "Narrowing a held declaration") || !strings.Contains(page, "#narrowing-a-held-declaration") || !strings.Contains(skill, "#keep-optimism-honest") {
		t.Error("the narrowing rule or the link to it is missing")
	}
}
