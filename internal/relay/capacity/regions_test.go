package capacity

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// test_edit_regions.py properties EDR-1..EDR-14 (.omo/ulw-execute/todo27-properties.md). Each
// subtest replays one scenario of testdata/gen_editregion.py call for call and compares every
// answer and refusal whole with Python's (python_editregion.json); the assertions after it pin
// the fields the property names.

func reasons(e *regionEnv, want map[int]string) {
	e.t.Helper()
	for i, reason := range want {
		if got := e.reason(i); got != reason {
			e.t.Errorf("step %d: %s, want %s", i, got, reason)
		}
	}
}

func Test27_EDR1_a_region_is_a_place(t *testing.T) {
	t.Run("two symbols in one file", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("src/a.py", proposeOpt{kind: "symbol", key: "parse"})
		e.propose("src/a.py", proposeOpt{kind: "symbol", key: "render"})
		e.propose("src/b.py", proposeOpt{})
		e.sameAsPython("two_symbols_in_one_file")
		if e.ok(0)["regionId"] == e.ok(1)["regionId"] || e.ok(2)["state"] != "proposed" {
			t.Fatal("two symbols share a region")
		}
	})
	t.Run("whole file overlaps a symbol", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("src/a.py", proposeOpt{kind: "symbol", key: "parse"})
		e.propose("src/a.py", proposeOpt{right: "PRJ-Z", link: e.other})
		e.show(ShowFilter{})
		e.sameAsPython("whole_file_overlaps_symbol")
		reasons(e, map[int]string{1: "region_overlap"})
	})
	t.Run("same symbol in another file", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("src/a.py", proposeOpt{kind: "symbol", key: "parse"})
		e.propose("src/b.py", proposeOpt{kind: "symbol", key: "parse", right: "PRJ-Z", link: e.other})
		e.sameAsPython("same_symbol_other_file")
	})
	t.Run("tree contains by component", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("src/a", proposeOpt{kind: "tree"})
		e.propose("src/ab/x.py", proposeOpt{right: "PRJ-Z", link: e.other})
		e.propose("src/a/x.py", proposeOpt{right: "PRJ-Z", link: e.other})
		e.sameAsPython("tree_contains_by_component")
		reasons(e, map[int]string{2: "region_overlap"})
	})
	t.Run("classified once", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("scripts/components.json", proposeOpt{class: "generated", regen: "derive"})
		e.propose("scripts/components.json", proposeOpt{right: "PRJ-Z", link: e.other})
		e.show(ShowFilter{})
		e.sameAsPython("classified_once")
		reasons(e, map[int]string{1: "region_overlap"})
	})
}

func Test27_EDR2_region_shape_is_refused_region_too_broad(t *testing.T) {
	e := newRegionEnv(t)
	for _, root := range []string{".", ""} {
		e.propose(root, proposeOpt{kind: "tree"})
	}
	for _, spelling := range []string{"src//a.py", "/src/a.py", "src/../a.py", "src/a.py/", "./a.py", ".."} {
		e.propose(spelling, proposeOpt{})
	}
	e.propose("src/a|b.py", proposeOpt{})
	e.propose("src/a.py", proposeOpt{kind: "file", key: "parse"})
	e.propose("src/a.py", proposeOpt{kind: "symbol"})
	e.propose("scripts/components.json", proposeOpt{class: "generated"})
	e.propose("src/a.py", proposeOpt{kind: "galaxy"})
	e.propose("src/a.py", proposeOpt{class: "weird"})
	e.propose("src/a.py", proposeOpt{right: "PRJ-A"})
	e.rows("SELECT * FROM edit_regions")
	e.show(ShowFilter{})
	e.sameAsPython("region_shapes")
	for i := range 14 {
		if got := e.reason(i); got != "region_too_broad" {
			t.Errorf("step %d: %s", i, got)
		}
	}
	reasons(e, map[int]string{14: "scope_cycle"})
}

func Test27_EDR3_agreement_identity(t *testing.T) {
	t.Run("either side converges", func(t *testing.T) {
		e := newRegionEnv(t)
		first := e.propose("src/a.py", proposeOpt{})
		e.step(e.regions.Propose(ctx(), Proposal{Repository: repo, BaseRevision: rev, Path: "src/a.py", RegionKind: "file",
			LeftProject: "PRJ-B", RightProject: "PRJ-A", PeerLinkID: e.pair, ProposerTaskID: beta, ConstraintText: "keep the signature"}))
		e.rows("SELECT region_id, repository, base_revision, path, region_kind, region_key, region_class, regenerate_from FROM edit_regions")
		e.rows("SELECT kind, subject, detail FROM journal WHERE kind LIKE 'edit_%' ORDER BY seq")
		e.agreement(first)
		e.agreement("agr-missing")
		e.sameAsPython("either_side_converges")
		again := e.ok(1)
		if again["agreementId"] != e.ok(0)["agreementId"] || again["alreadyProposed"] != true {
			t.Fatal(again)
		}
		if a := e.ok(0); a["baseRevision"] != rev || a["repository"] != repo || !(a["leftProject"].(string) < a["rightProject"].(string)) {
			t.Fatal(a)
		}
	})
	t.Run("never peers", func(t *testing.T) {
		e := newRegionEnv(t)
		e.step(e.regions.Propose(ctx(), Proposal{Repository: repo, BaseRevision: rev, Path: "src/c.py", RegionKind: "file",
			LeftProject: "PRJ-B", RightProject: "PRJ-Z", PeerLinkID: e.pair, ProposerTaskID: beta, ConstraintText: "x"}))
		e.propose("src/c.py", proposeOpt{link: "lnk-nothing"})
		e.propose("src/c.py", proposeOpt{link: e.other})
		e.show(ShowFilter{})
		e.sameAsPython("never_peers")
		reasons(e, map[int]string{0: "unregistered_scope", 1: "unregistered_scope", 2: "unregistered_scope"})
	})
	t.Run("refused proposal leaves no region", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("scripts/components.json", proposeOpt{class: "generated", task: zeta, regen: "derive"})
		e.rows("SELECT * FROM edit_regions")
		e.propose("scripts/components.json", proposeOpt{})
		e.sameAsPython("refused_leaves_no_region")
		if e.steps[1]["ok"] != "[]" || e.ok(2)["state"] != "proposed" {
			t.Fatal(e.steps)
		}
	})
}

func Test27_EDR4_generated_metadata_is_rederived(t *testing.T) {
	t.Run("two generated claims do not conflict", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("scripts/components.json", proposeOpt{class: "generated", regen: "runtime_install.py verify-definition"})
		e.propose("scripts/components.json", proposeOpt{class: "generated", regen: "runtime_install.py verify-definition", right: "PRJ-Z", link: e.other})
		e.sameAsPython("generated_do_not_conflict")
	})
	t.Run("reported under regenerate", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("scripts/components.json", proposeOpt{class: "generated", regen: "derive"})
		e.propose("src/a.py", proposeOpt{kind: "symbol", key: "parse"})
		e.show(ShowFilter{})
		e.show(ShowFilter{BaseRevision: ns(rev), Project: ns("PRJ-B"), Path: ns("src/a.py")})
		e.show(ShowFilter{BaseRevision: ns("rev-0")})
		e.sameAsPython("generated_is_regenerate")
		shown := e.ok(2)
		regenerate := shown["regenerate"].([]any)
		if len(regenerate) != 1 || len(shown["exclusive"].([]any)) != 1 || regenerate[0].(map[string]any)["resolution"] != "rederive" {
			t.Fatal(shown)
		}
	})
}

func Test27_EDR5_settlement(t *testing.T) {
	t.Run("needs both sides", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.propose("src/a.py", proposeOpt{})
		e.settle(record, beta, "accepted", null(), null())
		e.agreement(record)
		e.sameAsPython("needs_both_sides")
		if e.ok(0)["state"] != "proposed" || e.ok(2)["state"] != "agreed" {
			t.Fatal(e.steps)
		}
	})
	t.Run("conditional decline keeps the condition", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.propose("src/a.py", proposeOpt{})
		e.settle(record, beta, "declined", ns("only if the old name keeps forwarding"), ns("too broad"))
		e.agreement(record)
		e.sameAsPython("conditional_decline")
		if a := e.ok(2); a["state"] != "declined" || a["rightCondition"] != "only if the old name keeps forwarding" {
			t.Fatal(a)
		}
	})
	t.Run("withdrawal frees the region", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.propose("src/a.py", proposeOpt{})
		e.settle(record, alpha, "withdrawn", null(), null())
		e.propose("src/a.py", proposeOpt{right: "PRJ-Z", link: e.other})
		e.sameAsPython("withdrawal_frees")
	})
	t.Run("authority and closed agreements", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.propose("src/a.py", proposeOpt{})
		e.settle(record, beta, "withdrawn", null(), null())
		e.settle(record, zeta, "accepted", null(), null())
		e.propose("src/b.py", proposeOpt{task: zeta})
		e.settle(record, beta, "maybe", null(), null())
		e.settle(record, alpha, "accepted", ns("alpha's terms"), null())
		e.settle("agr-missing", alpha, "accepted", null(), null())
		e.agreement(record)
		e.settle(record, alpha, "withdrawn", null(), null())
		e.settle(record, beta, "accepted", null(), null())
		e.settle(record, beta, "released", null(), ns("late"))
		e.show(ShowFilter{})
		e.sameAsPython("settlement_authority")
		reasons(e, map[int]string{1: "scope_role_mismatch", 2: "scope_role_mismatch", 3: "scope_role_mismatch", 4: "link_not_active",
			5: "link_not_active", 6: "unregistered_scope", 9: "agreement_not_open", 10: "agreement_not_open"})
	})
	t.Run("released closes", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.agreed("src/a.py")
		e.settle(record, beta, "released", null(), ns("merged"))
		e.sameAsPython("released_closes")
	})
}

func Test27_EDR6_a_proposer_pre_accepts_its_own_side(t *testing.T) {
	t.Run("whichever argument it used", func(t *testing.T) {
		e := newRegionEnv(t)
		reversed := e.step(e.regions.Propose(ctx(), Proposal{Repository: repo, BaseRevision: rev, Path: "src/a.py", RegionKind: "file",
			LeftProject: "PRJ-B", RightProject: "PRJ-A", PeerLinkID: e.pair, ProposerTaskID: alpha, ConstraintText: "keep the signature"}))
		e.settle(reversed, alpha, "accepted", null(), null())
		e.settle(reversed, beta, "accepted", null(), null())
		e.sameAsPython("proposer_pre_accepts")
		if a := e.ok(0); a["leftAcceptedAt"] == nil || a["rightAcceptedAt"] != nil {
			t.Fatal(a)
		}
		if e.ok(1)["state"] != "proposed" || e.ok(2)["state"] != "agreed" {
			t.Fatal(e.steps)
		}
	})
	t.Run("losing ownership mid proposal", func(t *testing.T) {
		e := newRegionEnv(t)
		calls := 0
		e.regions.ownedSide = func(ctx context.Context, low, high, actor string) (string, error) {
			calls++
			if calls == 1 {
				return e.regions.realOwnedSide(ctx, low, high, actor)
			}
			return "", nil
		}
		e.propose("src/a.py", proposeOpt{})
		e.regions.ownedSide = nil
		e.rows("SELECT * FROM edit_agreements")
		e.rows("SELECT * FROM edit_regions")
		e.show(ShowFilter{})
		e.sameAsPython("losing_ownership_mid_proposal")
		reasons(e, map[int]string{0: "scope_role_mismatch"})
	})
}

func followupFixture(e *regionEnv, extra Followup) (any, any) {
	record := e.propose("src/a.py", proposeOpt{})
	extra.Agreement, extra.Trigger = id(record), "a temporary duplicate implementation"
	extra.Acceptance, extra.IssueRef = "the duplicate is gone and one caller remains", ns("CRW-200")
	return record, e.followup(extra)
}

func Test27_EDR7_follow_ups(t *testing.T) {
	t.Run("unassigned is nobody's work", func(t *testing.T) {
		e := newRegionEnv(t)
		_, item := followupFixture(e, Followup{})
		e.show(ShowFilter{})
		e.show(ShowFilter{Project: ns("PRJ-A")})
		e.show(ShowFilter{Project: ns("PRJ-B")})
		e.settleFollowup(item, alpha, "done")
		e.settleFollowup(item, alpha, "finished")
		e.settleFollowup(item, alpha, "dropped")
		e.settleFollowup(item, alpha, "dropped")
		e.settleFollowup(item, alpha, "done")
		e.accept(item, beta, "PRJ-B")
		e.show(ShowFilter{})
		e.sameAsPython("followup_unassigned")
		reasons(e, map[int]string{5: "followup_unassigned", 6: "link_not_active", 9: "agreement_not_open", 10: "agreement_not_open"})
		for _, i := range []int{2, 3, 4} {
			if accepted := e.ok(i)["followups"].(map[string]any)["accepted"].([]any); len(accepted) != 0 {
				t.Fatalf("step %d counts it for a parent", i)
			}
		}
		if e.ok(7)["state"] != "dropped" {
			t.Fatal(e.steps[7])
		}
	})
	t.Run("acceptance and authority over it", func(t *testing.T) {
		e := newRegionEnv(t)
		record, item := followupFixture(e, Followup{})
		e.accept(item, beta, "PRJ-A")
		e.accept(item, beta, "PRJ-B")
		e.accept(item, beta, "PRJ-B")
		e.accept(item, alpha, "PRJ-A")
		e.agreement(record)
		e.settleFollowup(item, alpha, "done")
		e.settleFollowup(item, beta, "done")
		e.show(ShowFilter{})
		e.followup(Followup{Agreement: id(record), Trigger: "a temporary duplicate implementation", Acceptance: "restated differently"})
		e.settleFollowup("fup-missing", alpha, "done")
		e.accept("fup-missing", alpha, "PRJ-A")
		e.sameAsPython("followup_accepted")
		reasons(e, map[int]string{2: "scope_role_mismatch", 5: "scope_role_mismatch", 7: "scope_role_mismatch", 11: "unregistered_scope", 12: "unregistered_scope"})
		taken := e.ok(3)
		if taken["assigneeTaskId"] != beta || taken["acceptedAt"] == nil || taken["issueRef"] != "CRW-200" || e.ok(6)["nextOwner"] != "task-beta" {
			t.Fatal(taken)
		}
		if again := e.ok(10); again["followupId"] != e.ok(1)["followupId"] || again["acceptance"] != "the duplicate is gone and one caller remains" {
			t.Fatal(again)
		}
	})
	t.Run("who may append work", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.propose("src/a.py", proposeOpt{})
		e.followup(Followup{Agreement: id(record), RecordedBy: zeta})
		e.followup(Followup{Agreement: id(record), AssigneeTask: ns(beta)})
		e.followup(Followup{Agreement: id(record), Trigger: " "})
		e.followup(Followup{Agreement: id(record), Acceptance: "a|b"})
		e.followup(Followup{Agreement: "agr-missing"})
		e.followup(Followup{Agreement: id(record), Trigger: "mine", AssigneeTask: ns(alpha), AssigneeProject: ns("PRJ-A")})
		e.show(ShowFilter{})
		e.sameAsPython("followup_authority")
		reasons(e, map[int]string{1: "scope_role_mismatch", 2: "scope_role_mismatch", 3: "unregistered_scope", 4: "unregistered_scope", 5: "unregistered_scope"})
	})
}

func Test27_EDR8_an_agreement_is_not_permission(t *testing.T) {
	e := newRegionEnv(t)
	e.propose("src/a.py", proposeOpt{})
	e.sameAsPython("authorizes_nothing")
	if a := e.ok(0); len(a["authorizes"].([]any)) != 0 || a["grantsMergePermission"] != false {
		t.Fatal(a)
	}
	// The merge-turn half (the same claim answered with and without an agreed region) is a
	// property of what this package does not do: nothing here reads or writes merge_turns.
	for _, file := range []string{"editregion_read.go", "editregion_write.go", "editregion_revision.go"} {
		source, err := readSource(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(source, "merge_turn") || strings.Contains(source, "MergeTurn") {
			t.Fatalf("%s touches the merge turn", file)
		}
	}
}

func Test27_EDR9_revision_moves(t *testing.T) {
	t.Run("restating reopens proposed and agreed", func(t *testing.T) {
		e := newRegionEnv(t)
		settled := e.agreed("src/a.py")
		open := e.propose("src/b.py", proposeOpt{})
		e.restate(rev, "rev-2", alpha)
		e.agreement(settled)
		e.agreement(open)
		e.restate(rev, rev, alpha)
		e.restate(rev, "a|b", alpha)
		e.sameAsPython("restating_reopens")
		for _, i := range []int{4, 5} {
			if a := e.ok(i); a["state"] != "reopened" || a["baseRevision"] != rev {
				t.Fatal(a)
			}
		}
		reasons(e, map[int]string{6: "link_not_active", 7: "unregistered_scope"})
	})
	t.Run("settling a superseded revision anywhere in a chain", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.propose("src/a.py", proposeOpt{})
		e.restate(rev, "rev-2", alpha)
		e.restate("rev-2", "rev-3", alpha)
		e.settle(record, beta, "accepted", null(), null())
		e.current("rev-2")
		e.current("rev-3")
		e.current(rev)
		e.sameAsPython("superseded_settlement")
		reasons(e, map[int]string{3: "agreement_revision_stale"})
		if e.steps[4]["ok"] != "false" || e.steps[5]["ok"] != "true" {
			t.Fatal(e.steps)
		}
	})
	t.Run("one successor", func(t *testing.T) {
		e := newRegionEnv(t)
		e.restate(rev, "rev-2", alpha)
		e.restate(rev, "rev-2", alpha)
		e.restate(rev, "rev-9", beta)
		e.rows("SELECT * FROM edit_revision_marks")
		e.show(ShowFilter{})
		e.sameAsPython("one_successor")
		reasons(e, map[int]string{2: "agreement_revision_stale"})
		if e.ok(1)["toRevision"] != "rev-2" {
			t.Fatal(e.steps[1])
		}
	})
	t.Run("a cycle is refused", func(t *testing.T) {
		e := newRegionEnv(t)
		e.agreed("src/a.py")
		e.restate(rev, "rev-2", alpha)
		e.restate("rev-2", rev, alpha)
		e.current("rev-2")
		e.show(ShowFilter{})
		e.sameAsPython("revision_cycle")
		reasons(e, map[int]string{3: "agreement_revision_stale"})
	})
	t.Run("a stranger cannot restate", func(t *testing.T) {
		e := newRegionEnv(t)
		e.agreed("src/a.py")
		e.restate(rev, "rev-2", zeta)
		e.step(e.regions.RestateRevision(ctx(), "other/repo", "x-1", "x-2", alpha))
		e.step(e.regions.RestateRevision(ctx(), "empty/repo", "x-1", "x-2", "task-nobody"))
		e.sameAsPython("stranger_restates")
		reasons(e, map[int]string{2: "scope_role_mismatch", 4: "scope_role_mismatch"})
	})
}

func Test27_EDR10_base_moves_chain_from_the_last_recorded_revision(t *testing.T) {
	t.Run("consecutive moves", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.propose("src/a.py", proposeOpt{})
		e.restate(rev, "rev-2", alpha)
		e.restate(rev, "rev-3", alpha)
		e.restate("rev-2", "rev-3", alpha)
		e.restate(rev, "rev-3", beta)
		e.rows("SELECT * FROM edit_revision_marks ORDER BY mark_id")
		e.show(ShowFilter{})
		e.settle(record, beta, "accepted", null(), null())
		e.reaffirm(record, beta, "rev-2", null())
		e.reaffirm(record, beta, "rev-3", null())
		e.sameAsPython("consecutive_moves")
		reasons(e, map[int]string{2: "agreement_revision_stale", 7: "agreement_revision_stale", 8: "agreement_revision_stale"})
		if !strings.Contains(e.detail(2), "--from-revision rev-2 --to-revision rev-3") || !strings.Contains(e.detail(7), "--revision rev-3") {
			t.Fatal(e.detail(2), e.detail(7))
		}
		if a := e.ok(4); a["alreadyRecorded"] != true || a["currentRevision"] != "rev-3" {
			t.Fatal(a)
		}
	})
	t.Run("a move from nowhere", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("src/a.py", proposeOpt{})
		e.restate(rev, "rev-2", alpha)
		e.restate("rev-9", "rev-10", alpha)
		e.rows("SELECT * FROM edit_revision_marks")
		e.show(ShowFilter{})
		e.sameAsPython("move_from_nowhere")
		if !strings.Contains(e.detail(2), "'rev-2'") {
			t.Fatal(e.detail(2))
		}
	})
	t.Run("another revision starts its own chain", func(t *testing.T) {
		e := newRegionEnv(t)
		e.propose("src/a.py", proposeOpt{})
		e.restate(rev, "rev-2", alpha)
		e.propose("src/b.py", proposeOpt{revision: "other-1"})
		e.restate("other-1", "other-2", alpha)
		e.sameAsPython("another_revision_own_chain")
	})
	t.Run("a closed agreement is not a start", func(t *testing.T) {
		e := newRegionEnv(t)
		closed := e.propose("src/old.py", proposeOpt{revision: "closed-1"})
		e.settle(closed, alpha, "withdrawn", null(), null())
		live := e.propose("src/a.py", proposeOpt{})
		e.restate(rev, "rev-2", alpha)
		successor := e.reaffirm(live, alpha, "rev-2", null())
		e.restate("closed-1", "rev-3", alpha)
		e.rows("SELECT * FROM edit_revision_marks")
		e.agreement(successor)
		e.sameAsPython("closed_not_a_start")
		reasons(e, map[int]string{5: "agreement_revision_stale"})
	})
}

func Test27_EDR11_reaffirm_carries_onto_the_current_revision(t *testing.T) {
	t.Run("owner carries, stranger cannot, one live agreement", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.agreed("src/a.py")
		e.restate(rev, "rev-2", alpha)
		e.reaffirm(record, zeta, "rev-2", null())
		successor := e.reaffirm(record, alpha, "rev-2", null())
		e.agreement(record)
		e.rows(liveQuery)
		e.rows("SELECT * FROM edit_reaffirmations")
		e.rows("SELECT kind, subject, detail FROM journal WHERE kind LIKE 'edit_%' ORDER BY seq")
		e.reaffirm("agr-missing", alpha, "rev-2", null())
		e.sameAsPython("owner_carries")
		reasons(e, map[int]string{3: "scope_role_mismatch", 9: "unregistered_scope"})
		s := e.ok(4)
		if s["baseRevision"] != "rev-2" || s["supersedes"] != id(record) || s["state"] != "proposed" || e.ok(5)["supersededBy"] != id(successor) {
			t.Fatal(s)
		}
		if e.steps[6]["ok"] != "[\n  {\n    \"agreement_id\": \""+id(successor)+"\"\n  }\n]" {
			t.Fatal(e.steps[6])
		}
	})
	t.Run("closed is proposed again", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.propose("src/a.py", proposeOpt{})
		e.settle(record, alpha, "withdrawn", null(), null())
		e.reaffirm(record, alpha, rev, null())
		e.sameAsPython("closed_is_proposed_again")
		reasons(e, map[int]string{2: "agreement_not_open"})
	})
	t.Run("never reached", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.agreed("src/a.py")
		e.restate(rev, "rev-2", alpha)
		e.reaffirm(record, alpha, "rev-typo", null())
		e.show(ShowFilter{})
		e.sameAsPython("reaffirm_never_reached")
		reasons(e, map[int]string{3: "agreement_revision_stale"})
	})
	t.Run("onto the revision it stands on", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.settle(original, alpha, "accepted", null(), null())
		e.reaffirm(original, alpha, rev, null())
		e.agreement(original)
		e.sameAsPython("reaffirm_same_revision")
		reasons(e, map[int]string{2: "link_not_active"})
		if a := e.ok(3); a["state"] != "agreed" || a["supersededBy"] != nil || a["rightAcceptedAt"] == nil {
			t.Fatal(a)
		}
	})
}

func Test27_EDR12_what_a_carry_keeps(t *testing.T) {
	t.Run("answering side keeps proposer, conditions and constraint", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2", "rev-3")
		e.settle(original, alpha, "accepted", null(), null())
		successor := e.reaffirm(original, alpha, "rev-3", null())
		e.agreement(original)
		e.settle(successor, beta, "accepted", null(), null())
		e.show(ShowFilter{})
		e.sameAsPython("answering_side_reaffirms")
		reasons(e, map[int]string{3: "agreement_revision_stale"})
		s := e.ok(4)
		waiting := s["reaffirmation"].(map[string]any)["awaitingAcceptance"].(map[string]any)
		if s["proposerTaskId"] != beta || s["rightCondition"] != betaCondition || s["legacyCarry"] != nil ||
			waiting["reason"] != "acceptance_on_prior_revision" || s["nextOwner"] != beta ||
			waiting["command"] != "region-settle --agreement "+id(successor)+" --actor task-beta --disposition accepted" {
			t.Fatal(s)
		}
		if a := e.ok(6); a["state"] != "agreed" || a["reaffirmation"].(map[string]any)["awaitingAcceptance"] != nil {
			t.Fatal(a)
		}
	})
	t.Run("proposing side is the control", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		e.reaffirm(original, beta, "rev-2", null())
		e.sameAsPython("proposing_side_control")
		waiting := e.ok(2)["reaffirmation"].(map[string]any)["awaitingAcceptance"].(map[string]any)
		if waiting["reason"] != "not_yet_accepted" || waiting["priorAcceptedAt"] != nil || waiting["task"] != alpha {
			t.Fatal(waiting)
		}
	})
	t.Run("agreed carried asks again", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.settle(original, alpha, "accepted", null(), null())
		e.moved("rev-2")
		e.reaffirm(original, alpha, "rev-2", null())
		e.sameAsPython("agreed_carried_asks_again")
	})
	t.Run("restates only its own condition", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		e.reaffirm(original, alpha, "rev-2", ns("alpha's condition, lines 14-15 at rev-2"))
		e.sameAsPython("restates_own_condition")
	})
	t.Run("both conditions survive a second carry", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		first := e.reaffirm(original, alpha, "rev-2", ns("alpha's condition at rev-2"))
		e.restate("rev-2", "rev-3", alpha)
		e.reaffirm(first, beta, "rev-3", null())
		e.rows("SELECT * FROM edit_reaffirmations ORDER BY recorded_at, agreement_id")
		e.sameAsPython("second_carry")
	})
	t.Run("a decline after a carry", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		successor := e.reaffirm(original, alpha, "rev-2", null())
		e.settle(successor, beta, "declined", ns("only if parse keeps forwarding"), ns("the tree moved"))
		e.sameAsPython("decline_after_carry")
	})
	t.Run("a conditionless decline after a carry", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		successor := e.reaffirm(original, alpha, "rev-2", null())
		e.settle(successor, beta, "declined", null(), ns("changed our mind"))
		e.sameAsPython("conditionless_decline_after_carry")
	})
	t.Run("a legacy successor says where its text was written", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		legacy := e.step(e.regions.Propose(ctx(), Proposal{Repository: repo, BaseRevision: "rev-2", Path: "src/a.py", RegionKind: "file",
			LeftProject: "PRJ-A", RightProject: "PRJ-B", PeerLinkID: e.pair, ProposerTaskID: alpha, ConstraintText: constraintAt,
			IssueKey: ns("CRW-1"), Supersedes: ns(id(original))}))
		e.show(ShowFilter{})
		e.restate("rev-2", "rev-3", beta)
		e.reaffirm(legacy, alpha, "rev-3", null())
		e.sameAsPython("legacy_successor")
		if carried := e.ok(5); carried["proposerTaskId"] != beta || carried["rightCondition"] != betaCondition || carried["legacyCarry"] != nil {
			t.Fatal(carried)
		}
	})
	t.Run("next owner follows a handover", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		successor := e.reaffirm(original, alpha, "rev-2", null())
		e.handover("PRJ-B", beta, "task-beta-next", "host-b")
		e.agreement(successor)
		e.settle(successor, "task-beta-next", "accepted", null(), null())
		e.sameAsPython("next_owner_follows_handover")
		waiting := e.ok(3)["reaffirmation"].(map[string]any)["awaitingAcceptance"].(map[string]any)
		if e.ok(3)["nextOwner"] != "task-beta-next" || !strings.Contains(waiting["command"].(string), "--actor task-beta-next") {
			t.Fatal(waiting)
		}
	})
	t.Run("no single parent to name", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		successor := e.reaffirm(original, alpha, "rev-2", null())
		if _, err := e.store.DB.ExecContext(ctx(), "UPDATE scope_bindings SET status = 'archived' WHERE scope_key = 'PRJ-B'"); err != nil {
			t.Fatal(err)
		}
		e.agreement(successor)
		e.sameAsPython("awaiting_without_single_parent")
	})
}

func Test27_EDR13_a_refused_carry_retires_nothing_and_records_its_contest(t *testing.T) {
	t.Run("overlap on the new revision", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.agreed("src/a.py")
		e.restate(rev, "rev-2", alpha)
		e.propose("src/a.py", proposeOpt{revision: "rev-2", right: "PRJ-Z", link: e.other})
		e.reaffirm(record, alpha, "rev-2", null())
		e.agreement(record)
		e.show(ShowFilter{})
		e.sameAsPython("failed_reaffirmation_overlap")
		reasons(e, map[int]string{4: "region_overlap"})
	})
	t.Run("the pair already holds the place", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		standing := e.propose("src/a.py", proposeOpt{revision: "rev-2"})
		e.reaffirm(original, alpha, "rev-2", null())
		e.agreement(original)
		e.rows("SELECT * FROM edit_reaffirmations")
		e.sameAsPython("carry_onto_held_place")
		if !strings.Contains(e.detail(3), id(standing)) {
			t.Fatal(e.detail(3))
		}
	})
	t.Run("a move recorded while carrying", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.byBeta()
		e.moved("rev-2")
		e.racing(func() { e.restate("rev-2", "rev-3", beta) })
		e.reaffirm(record, alpha, "rev-2", null())
		e.agreement(record)
		e.rows("SELECT * FROM edit_reaffirmations")
		e.rows("SELECT * FROM edit_agreements WHERE base_revision = 'rev-2'")
		e.show(ShowFilter{})
		e.sameAsPython("move_while_carrying")
		reasons(e, map[int]string{3: "agreement_revision_stale"})
	})
	t.Run("a same-pair proposal while carrying", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.byBeta()
		e.moved("rev-2")
		e.racing(func() { e.propose("src/a.py", proposeOpt{revision: "rev-2"}) })
		e.reaffirm(record, alpha, "rev-2", null())
		e.agreement(record)
		e.rows("SELECT * FROM edit_reaffirmations")
		e.show(ShowFilter{})
		e.sameAsPython("same_pair_while_carrying")
		reasons(e, map[int]string{3: "region_overlap"})
	})
	t.Run("a destination that is not the predecessor's place", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.byBeta()
		e.moved("rev-2")
		carry := func(p, right, link, supersedes, predecessor string) {
			e.step(e.regions.Propose(ctx(), Proposal{Repository: repo, BaseRevision: "rev-2", Path: p, RegionKind: "file",
				LeftProject: "PRJ-A", RightProject: right, PeerLinkID: link, ProposerTaskID: alpha, ConstraintText: "elsewhere",
				Supersedes: ns(supersedes), Carry: &Carry{Predecessor: predecessor}}))
		}
		carry("src/b.py", "PRJ-B", e.pair, id(record), id(record))
		carry("src/a.py", "PRJ-Z", e.other, id(record), id(record))
		carry("src/a.py", "PRJ-B", e.pair, "agr-other", id(record))
		carry("src/a.py", "PRJ-B", e.pair, "agr-gone", "agr-gone")
		e.agreement(record)
		e.rows("SELECT * FROM edit_reaffirmations")
		e.rows("SELECT * FROM edit_agreements WHERE base_revision = 'rev-2'")
		e.show(ShowFilter{})
		e.sameAsPython("carry_elsewhere")
		reasons(e, map[int]string{2: "unregistered_scope", 3: "unregistered_scope", 4: "unregistered_scope", 5: "unregistered_scope"})
	})
	t.Run("a handover while carrying", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.byBeta()
		e.moved("rev-2")
		e.racing(func() { e.handover("PRJ-A", alpha, "task-alpha-next", "host-a") })
		e.reaffirm(record, alpha, "rev-2", null())
		e.agreement(record)
		e.rows("SELECT * FROM edit_reaffirmations")
		e.rows("SELECT * FROM edit_regions WHERE base_revision = 'rev-2'")
		e.show(ShowFilter{})
		e.sameAsPython("handover_while_carrying")
		reasons(e, map[int]string{2: "scope_role_mismatch"})
	})
	t.Run("a classification clash", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		e.propose("src/a.py", proposeOpt{revision: "rev-2", right: "PRJ-Z", link: e.other, class: "generated", regen: "derive"})
		e.reaffirm(original, alpha, "rev-2", null())
		e.agreement(original)
		e.rows("SELECT * FROM edit_reaffirmations")
		e.show(ShowFilter{})
		e.sameAsPython("classification_clash")
		reasons(e, map[int]string{3: "region_overlap"})
	})
	t.Run("settled while carrying", func(t *testing.T) {
		e := newRegionEnv(t)
		record := e.byBeta()
		e.moved("rev-2")
		e.racing(func() { e.settle(record, beta, "withdrawn", null(), null()) })
		e.reaffirm(record, alpha, "rev-2", null())
		e.show(ShowFilter{})
		e.sameAsPython("settled_while_carrying")
	})
}

// EDR-14 races two pairs on one database from two Stores; a start barrier lines them up and the
// assertions hold under every interleaving: one live agreement, one region_overlap, the contest kept.
func Test27_EDR14_one_of_two_overlapping_proposals_survives(t *testing.T) {
	e := newRegionEnv(t)
	var start, done sync.WaitGroup
	start.Add(1)
	errs := make([]error, 2)
	for i, side := range [][2]string{{"PRJ-B", e.pair}, {"PRJ-Z", e.other}} {
		other := openAgain(t, e.env)
		regions := &EditRegions{Store: other.Store, Now: e.clock.iso}
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_, errs[i] = regions.Propose(ctx(), Proposal{Repository: repo, BaseRevision: rev, Path: "src/shared.py", RegionKind: "file",
				LeftProject: "PRJ-A", RightProject: side[0], PeerLinkID: side[1], ProposerTaskID: alpha, ConstraintText: "keep the signature"})
		}()
	}
	start.Done()
	done.Wait()
	survived, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			survived++
		case reasonOf(err) == "region_overlap":
			refused++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	live := e.rows(liveQuery).([]any)
	conflicts, err := Conflicts(ctx(), e.store, domainEditRegion, repo)
	if err != nil {
		t.Fatal(err)
	}
	if survived != 1 || refused != 1 || len(live) != 1 || len(conflicts) != 1 {
		t.Fatalf("survived %d refused %d live %d contests %d", survived, refused, len(live), len(conflicts))
	}
	if reason := conflicts[0].(contract.OrderedObject)[3].Value; reason != "region_overlap" {
		t.Fatal(reason)
	}
}
