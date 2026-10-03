package dagsched

import (
	"strings"
	"testing"
)

// CRW-410: the page, the skill and the command specs say the same things about the sweep and the merge-order constraint, so a command, trigger, status, reason, lane, head source, table or key that is added or
// renamed in the code cannot go undescribed.
func TestSweepPageSkillAndSpecsNameTheSameThings(t *testing.T) {
	page := readText(t, "../../../docs/relay/dag-scheduler.md")
	plans := readText(t, "../../../docs/relay/dag-plans.md")
	skill := readText(t, "../../../plugins/crw/skills/crw-run/references/region-grades.md")
	skillIndex := readText(t, "../../../plugins/crw/skills/crw-run/SKILL.md")
	specs := readText(t, "../argparse/specs.json")
	for where, text := range map[string]string{"docs/relay/dag-scheduler.md": page, "the crw-run skill (region-grades.md)": skill} {
		if !strings.Contains(text, "dag-conflict-sweep") {
			t.Errorf("%s does not name dag-conflict-sweep", where)
		}
	}
	if !strings.Contains(specs, "{\"name\":\"dag-conflict-sweep\"") {
		t.Error("argparse/specs.json has no spec for dag-conflict-sweep")
	}
	for _, name := range []string{TriggerLanding, TriggerReceipt, TriggerManual, MemberPair, MemberTip, MemberObserved, MemberReplayed, MemberUnmeasured, ReasonHeadUnknown, ReasonCheckoutMismatch, ReasonCommitMissing,
		ReasonNoCommonAncestor, ReasonTipUnreadable, HeadExplicit, HeadAcceptance, HeadChildCheckout, SweepRecorded, SweepAlready, SweepSkipped, SweepFailed, LaneTurn, LaneAccepted, LaneWorking,
		HeadsCurrentYes, HeadsCurrentNo, HeadsCurrentUnknown} {
		if !strings.Contains(page, name) {
			t.Errorf("the page does not describe %s", name)
		}
	}
	for _, name := range []string{"dag_tip_conflict_observations", "dag_tip_conflict_observation_files", "dag_conflict_drift", "dag_conflict_sweeps", "dag_conflict_sweeps_trigger", "dag_conflict_sweep_members"} {
		if !strings.Contains(page, name) {
			t.Errorf("the page does not name the zone object %s", name)
		}
	}
	for _, name := range []string{"dag_tip_conflict_observations", "dag_conflict_drift", "dag_conflict_sweeps", "dag_conflict_sweep_members"} {
		if !strings.Contains(plans, name) {
			t.Errorf("the plan page does not list the zone table %s", name)
		}
	}
	// the keys of the printed object, in the page and in what the parent is told to read
	for _, key := range []string{"merge_order", "after", "before", "tip", "heads_current", "drift_nodes", "unattributed", "order_constraints", "conflict_sweep", "observation_id"} {
		if !strings.Contains(page, key) {
			t.Errorf("the page does not describe the key %s", key)
		}
	}
	for _, say := range []string{"merge_order", "order_constraints", "dag-conflict-sweep", "--trigger receipt", "git fetch", "drift", "heads_current", "sibling channel"} {
		if !strings.Contains(skill, say) {
			t.Errorf("the skill does not say %q", say)
		}
	}
	if !strings.Contains(skillIndex, "references/region-grades.md#read-the-merge-order") || !strings.Contains(skill, "## At merge time") || !strings.Contains(skill, "### Read the merge order") {
		t.Error("the skill's procedure or the link to it is missing")
	}
	// the approximation of receipt arrival is stated where the parent reads it, and the constraint is said to stop nothing
	if !strings.Contains(page, "Receipt arrival is approximated") || !strings.Contains(skill, "approximated") || !strings.Contains(skill, "Nothing running is stopped") {
		t.Error("the page or the skill does not state the approximation or that nothing is stopped")
	}
}
