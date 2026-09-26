"""Record the Python edit-region module's caller-visible answers for internal/relay/capacity tests.

Run from the repository root with HOME/XDG_*/CODEX_HOME/TMPDIR in a scratch directory:
  uv run --no-sync python internal/relay/capacity/testdata/gen_editregion.py \
      > internal/relay/capacity/testdata/python_editregion.json
One scenario per test of test_edit_regions.py that has an observable answer, each on a fresh
store with that file's setUp (FakeClock(1_700_000_000); PRJ-A/B/Z parents; peers A-B and A-Z).
Each step is {"ok": <json.dumps(value, indent=2)>} or {"refused": {"reason", "detail"}}.
"""
import json
import os
import sys
import tempfile

from codex_session_relay.clock import FakeClock
from codex_session_relay.editregion import EditRegions
from codex_session_relay.errors import RelayError
from codex_session_relay.linkage import PARENT, Linkage
from codex_session_relay.models import Endpoint
from codex_session_relay.store import Store

REPO, REV = "owner/repo", "rev-1"
ALPHA = Endpoint("task-alpha", "host-a", cwd="/alpha")
BETA = Endpoint("task-beta", "host-b", cwd="/beta")
ZETA = Endpoint("task-zeta", "host-z", cwd="/zeta")
BETA_CONDITION = "beta publishes parse() and restates this before renaming it"
CONSTRAINT = "keep parse() at src/a.py lines 12-13 at rev-1"
SCENARIOS = {}


def scenario(fn):
    SCENARIOS[fn.__name__] = fn
    return fn


class Env:
    def __init__(self):
        directory = tempfile.mkdtemp(dir=os.environ["TMPDIR"])
        self.store = Store(os.path.join(directory, "relay.sqlite3"))
        self.clock = FakeClock()
        self.linkage = Linkage(self.store, self.clock)
        self.regions = EditRegions(self.store, self.clock, self.linkage)
        for key, endpoint in (("PRJ-A", ALPHA), ("PRJ-B", BETA), ("PRJ-Z", ZETA)):
            self.linkage.bind_scope(role=PARENT, scope_key=key, endpoint=endpoint)
        self.pair = self.linkage.register_peer(
            left_project="PRJ-A", left_parent=ALPHA, right_project="PRJ-B",
            right_parent=BETA)["linkId"]
        self.other = self.linkage.register_peer(
            left_project="PRJ-A", left_parent=ALPHA, right_project="PRJ-Z",
            right_parent=ZETA)["linkId"]
        self.steps = []

    def step(self, call):
        try:
            value = call()
        except RelayError as error:
            self.steps.append({"refused": {
                "reason": error.reason.value if error.reason else None, "detail": error.detail}})
            return None
        self.steps.append({"ok": json.dumps(value, indent=2)})
        return value

    def propose(self, path, kind="file", key="", region_class="source", regenerate_from=None,
                link=None, right="PRJ-B", revision=REV, task=None, next_owner="task-beta"):
        return self.step(lambda: self.regions.propose(
            repository=REPO, base_revision=revision, path=path, region_kind=kind,
            region_key=key, region_class=region_class, regenerate_from=regenerate_from,
            left_project="PRJ-A", right_project=right, peer_link_id=link or self.pair,
            proposer_task_id=task or ALPHA.task_id,
            constraint_text="keep the public signature", issue_key="CRW-1",
            next_owner=next_owner))

    def by_beta(self, condition=BETA_CONDITION):
        return self.step(lambda: self.regions.propose(
            repository=REPO, base_revision=REV, path="src/a.py", region_kind="file",
            left_project="PRJ-B", right_project="PRJ-A", peer_link_id=self.pair,
            proposer_task_id=BETA.task_id, constraint_text=CONSTRAINT,
            condition=condition, issue_key="CRW-1", next_owner=ALPHA.task_id))

    def settle(self, agreement, actor, disposition, condition=None, reason=None):
        return self.step(lambda: self.regions.settle(
            agreement, actor=actor, disposition=disposition, condition=condition,
            reason=reason))

    def agreed(self, path="src/a.py"):
        record = self.propose(path)
        self.settle(record["agreementId"], BETA.task_id, "accepted")
        return record

    def restate(self, frm, to, actor=ALPHA.task_id):
        return self.step(lambda: self.regions.restate_revision(
            repository=REPO, from_revision=frm, to_revision=to, actor=actor))

    def moved(self, *revisions):
        previous = REV
        for revision in revisions:
            self.restate(previous, revision, BETA.task_id)
            previous = revision

    def reaffirm(self, agreement, actor, revision, condition=None):
        return self.step(lambda: self.regions.reaffirm(
            agreement, actor=actor, base_revision=revision, condition=condition))

    def agreement(self, identifier):
        return self.step(lambda: self.regions.agreement(identifier))

    def show(self, **kw):
        return self.step(lambda: self.regions.show(repository=REPO, **kw))

    def current(self, revision):
        return self.step(lambda: self.regions.current_revision(REPO, revision))

    def followup(self, agreement, trigger="t", acceptance="a", by=ALPHA.task_id, **kw):
        return self.step(lambda: self.regions.followup(
            agreement, trigger_text=trigger, acceptance_text=acceptance, recorded_by=by, **kw))

    def accept(self, item, actor, project):
        return self.step(lambda: self.regions.accept_followup(
            item, actor=actor, assignee_project=project))

    def settle_followup(self, item, actor, disposition):
        return self.step(lambda: self.regions.settle_followup(
            item, actor=actor, disposition=disposition))

    def rows(self, sql):
        return self.step(lambda: [dict(r) for r in self.store.all(sql, ())])

    def handover(self, key, expect, task, host):
        # Not a step: linkage.handover is todo 26's answer. The Go test applies its row writes.
        self.linkage.handover(
            role=PARENT, scope_key=key, expect_task_id=expect,
            endpoint=Endpoint(task, host), acknowledged=[],
            evidence="changed hands", actor="test")

    def racing(self, concurrent):
        original = self.regions.propose

        def interleaved(**arguments):
            self.regions.propose = original
            concurrent()
            return original(**arguments)

        self.regions.propose = interleaved


LIVE = ("SELECT agreement_id FROM edit_agreements"
        "  WHERE state IN ('proposed','agreed','reopened') AND superseded_by IS NULL")


# ---------------------------------------------------------------- EDR-1 / EDR-2 / EDR-3 / EDR-4

@scenario
def two_symbols_in_one_file(e):
    e.propose("src/a.py", kind="symbol", key="parse")
    e.propose("src/a.py", kind="symbol", key="render")
    e.propose("src/b.py")


@scenario
def whole_file_overlaps_symbol(e):
    e.propose("src/a.py", kind="symbol", key="parse")
    e.propose("src/a.py", right="PRJ-Z", link=e.other)
    e.show()


@scenario
def same_symbol_other_file(e):
    e.propose("src/a.py", kind="symbol", key="parse")
    e.propose("src/b.py", kind="symbol", key="parse", right="PRJ-Z", link=e.other)


@scenario
def tree_contains_by_component(e):
    e.propose("src/a", kind="tree")
    e.propose("src/ab/x.py", right="PRJ-Z", link=e.other)
    e.propose("src/a/x.py", right="PRJ-Z", link=e.other)


@scenario
def classified_once(e):
    e.propose("scripts/components.json", region_class="generated", regenerate_from="derive")
    e.propose("scripts/components.json", right="PRJ-Z", link=e.other)
    e.show()


@scenario
def region_shapes(e):
    for root in (".", ""):
        e.propose(root, kind="tree")
    for spelling in ("src//a.py", "/src/a.py", "src/../a.py", "src/a.py/", "./a.py", ".."):
        e.propose(spelling)
    e.propose("src/a|b.py")
    e.propose("src/a.py", kind="file", key="parse")
    e.propose("src/a.py", kind="symbol")
    e.propose("scripts/components.json", region_class="generated")
    e.propose("src/a.py", kind="galaxy")
    e.propose("src/a.py", region_class="weird")
    e.propose("src/a.py", right="PRJ-A")
    e.rows("SELECT * FROM edit_regions")
    e.show()


@scenario
def either_side_converges(e):
    first = e.propose("src/a.py")
    e.step(lambda: e.regions.propose(
        repository=REPO, base_revision=REV, path="src/a.py", region_kind="file",
        left_project="PRJ-B", right_project="PRJ-A", peer_link_id=e.pair,
        proposer_task_id=BETA.task_id, constraint_text="keep the signature"))
    e.rows("SELECT region_id, repository, base_revision, path, region_kind, region_key,"
           " region_class, regenerate_from FROM edit_regions")
    e.rows("SELECT kind, subject, detail FROM journal WHERE kind LIKE 'edit_%' ORDER BY seq")
    e.agreement(first["agreementId"])
    e.agreement("agr-missing")


@scenario
def never_peers(e):
    e.step(lambda: e.regions.propose(
        repository=REPO, base_revision=REV, path="src/c.py", region_kind="file",
        left_project="PRJ-B", right_project="PRJ-Z", peer_link_id=e.pair,
        proposer_task_id=BETA.task_id, constraint_text="x"))
    e.propose("src/c.py", link="lnk-nothing")
    e.propose("src/c.py", link=e.other)
    e.show()


@scenario
def refused_leaves_no_region(e):
    e.propose("scripts/components.json", region_class="generated", task=ZETA.task_id,
              regenerate_from="derive")
    e.rows("SELECT * FROM edit_regions")
    e.propose("scripts/components.json")


@scenario
def generated_do_not_conflict(e):
    e.propose("scripts/components.json", region_class="generated",
              regenerate_from="runtime_install.py verify-definition")
    e.propose("scripts/components.json", region_class="generated",
              regenerate_from="runtime_install.py verify-definition", right="PRJ-Z",
              link=e.other)


@scenario
def generated_is_regenerate(e):
    e.propose("scripts/components.json", region_class="generated", regenerate_from="derive")
    e.propose("src/a.py", kind="symbol", key="parse")
    e.show()
    e.show(base_revision=REV, project_key="PRJ-B", path="src/a.py")
    e.show(base_revision="rev-0")


# ---------------------------------------------------------------- EDR-5 / EDR-6

@scenario
def needs_both_sides(e):
    record = e.propose("src/a.py")
    e.settle(record["agreementId"], BETA.task_id, "accepted")
    e.agreement(record["agreementId"])


@scenario
def conditional_decline(e):
    record = e.propose("src/a.py")
    e.settle(record["agreementId"], BETA.task_id, "declined",
             condition="only if the old name keeps forwarding", reason="too broad")
    e.agreement(record["agreementId"])


@scenario
def withdrawal_frees(e):
    record = e.propose("src/a.py")
    e.settle(record["agreementId"], ALPHA.task_id, "withdrawn")
    e.propose("src/a.py", right="PRJ-Z", link=e.other)


@scenario
def settlement_authority(e):
    record = e.propose("src/a.py")
    e.settle(record["agreementId"], BETA.task_id, "withdrawn")
    e.settle(record["agreementId"], ZETA.task_id, "accepted")
    e.propose("src/b.py", task=ZETA.task_id)
    e.settle(record["agreementId"], BETA.task_id, "maybe")
    e.settle(record["agreementId"], ALPHA.task_id, "accepted", condition="alpha's terms")
    e.settle("agr-missing", ALPHA.task_id, "accepted")
    e.agreement(record["agreementId"])
    e.settle(record["agreementId"], ALPHA.task_id, "withdrawn")
    e.settle(record["agreementId"], BETA.task_id, "accepted")
    e.settle(record["agreementId"], BETA.task_id, "released", reason="late")
    e.show()


@scenario
def released_closes(e):
    record = e.agreed("src/a.py")
    e.settle(record["agreementId"], BETA.task_id, "released", reason="merged")


@scenario
def proposer_pre_accepts(e):
    reversed_order = e.step(lambda: e.regions.propose(
        repository=REPO, base_revision=REV, path="src/a.py", region_kind="file",
        left_project="PRJ-B", right_project="PRJ-A", peer_link_id=e.pair,
        proposer_task_id=ALPHA.task_id, constraint_text="keep the signature"))
    e.settle(reversed_order["agreementId"], ALPHA.task_id, "accepted")
    e.settle(reversed_order["agreementId"], BETA.task_id, "accepted")


@scenario
def losing_ownership_mid_proposal(e):
    original = e.regions._owned_side
    calls = {"n": 0}

    def vanishing(low, high, actor):
        calls["n"] += 1
        return original(low, high, actor) if calls["n"] == 1 else None

    e.regions._owned_side = vanishing
    e.propose("src/a.py")
    e.regions._owned_side = original
    e.rows("SELECT * FROM edit_agreements")
    e.rows("SELECT * FROM edit_regions")
    e.show()


# ---------------------------------------------------------------- EDR-7

def with_followup(e, **kw):
    record = e.propose("src/a.py")
    return record, e.followup(
        record["agreementId"], trigger="a temporary duplicate implementation",
        acceptance="the duplicate is gone and one caller remains", issue_ref="CRW-200", **kw)


@scenario
def followup_unassigned(e):
    record, item = with_followup(e)
    e.show()
    e.show(project_key="PRJ-A")
    e.show(project_key="PRJ-B")
    e.settle_followup(item["followupId"], ALPHA.task_id, "done")
    e.settle_followup(item["followupId"], ALPHA.task_id, "finished")
    e.settle_followup(item["followupId"], ALPHA.task_id, "dropped")
    e.settle_followup(item["followupId"], ALPHA.task_id, "dropped")
    e.settle_followup(item["followupId"], ALPHA.task_id, "done")
    e.accept(item["followupId"], BETA.task_id, "PRJ-B")
    e.show()


@scenario
def followup_accepted(e):
    record, item = with_followup(e)
    e.accept(item["followupId"], BETA.task_id, "PRJ-A")
    e.accept(item["followupId"], BETA.task_id, "PRJ-B")
    e.accept(item["followupId"], BETA.task_id, "PRJ-B")
    e.accept(item["followupId"], ALPHA.task_id, "PRJ-A")
    e.agreement(record["agreementId"])
    e.settle_followup(item["followupId"], ALPHA.task_id, "done")
    e.settle_followup(item["followupId"], BETA.task_id, "done")
    e.show()
    e.followup(record["agreementId"], trigger="a temporary duplicate implementation",
               acceptance="restated differently")
    e.settle_followup("fup-missing", ALPHA.task_id, "done")
    e.accept("fup-missing", ALPHA.task_id, "PRJ-A")


@scenario
def followup_authority(e):
    record = e.propose("src/a.py")
    e.followup(record["agreementId"], by=ZETA.task_id)
    e.followup(record["agreementId"], assignee_task_id=BETA.task_id)
    e.followup(record["agreementId"], trigger=" ")
    e.followup(record["agreementId"], acceptance="a|b")
    e.followup("agr-missing")
    e.followup(record["agreementId"], trigger="mine", assignee_task_id=ALPHA.task_id,
               assignee_project="PRJ-A")
    e.show()


# ---------------------------------------------------------------- EDR-9 / EDR-10

@scenario
def restating_reopens(e):
    settled = e.agreed("src/a.py")
    open_one = e.propose("src/b.py")
    e.restate(REV, "rev-2")
    e.agreement(settled["agreementId"])
    e.agreement(open_one["agreementId"])
    e.restate(REV, REV)
    e.restate(REV, "a|b")


@scenario
def superseded_settlement(e):
    record = e.propose("src/a.py")
    e.restate(REV, "rev-2")
    e.restate("rev-2", "rev-3")
    e.settle(record["agreementId"], BETA.task_id, "accepted")
    e.current("rev-2")
    e.current("rev-3")
    e.current(REV)


@scenario
def one_successor(e):
    e.restate(REV, "rev-2")
    e.restate(REV, "rev-2")
    e.restate(REV, "rev-9", BETA.task_id)
    e.rows("SELECT * FROM edit_revision_marks")
    e.show()


@scenario
def revision_cycle(e):
    e.agreed("src/a.py")
    e.restate(REV, "rev-2")
    e.restate("rev-2", REV)
    e.current("rev-2")
    e.show()


@scenario
def stranger_restates(e):
    e.agreed("src/a.py")
    e.restate(REV, "rev-2", ZETA.task_id)
    e.step(lambda: e.regions.restate_revision(
        repository="other/repo", from_revision="x-1", to_revision="x-2", actor=ALPHA.task_id))
    e.step(lambda: e.regions.restate_revision(
        repository="empty/repo", from_revision="x-1", to_revision="x-2", actor="task-nobody"))


@scenario
def consecutive_moves(e):
    record = e.propose("src/a.py")
    e.restate(REV, "rev-2")
    e.restate(REV, "rev-3")
    e.restate("rev-2", "rev-3")
    e.restate(REV, "rev-3", BETA.task_id)
    e.rows("SELECT * FROM edit_revision_marks ORDER BY mark_id")
    e.show()
    e.settle(record["agreementId"], BETA.task_id, "accepted")
    e.reaffirm(record["agreementId"], BETA.task_id, "rev-2")
    e.reaffirm(record["agreementId"], BETA.task_id, "rev-3")


@scenario
def move_from_nowhere(e):
    e.propose("src/a.py")
    e.restate(REV, "rev-2")
    e.restate("rev-9", "rev-10")
    e.rows("SELECT * FROM edit_revision_marks")
    e.show()


@scenario
def another_revision_own_chain(e):
    e.propose("src/a.py")
    e.restate(REV, "rev-2")
    e.propose("src/b.py", revision="other-1")
    e.restate("other-1", "other-2")


@scenario
def closed_not_a_start(e):
    closed = e.propose("src/old.py", revision="closed-1")
    e.settle(closed["agreementId"], ALPHA.task_id, "withdrawn")
    live = e.propose("src/a.py")
    e.restate(REV, "rev-2")
    successor = e.reaffirm(live["agreementId"], ALPHA.task_id, "rev-2")
    e.restate("closed-1", "rev-3")
    e.rows("SELECT * FROM edit_revision_marks")
    e.agreement(successor["agreementId"])


# ---------------------------------------------------------------- EDR-11 / EDR-12

@scenario
def owner_carries(e):
    record = e.agreed("src/a.py")
    e.restate(REV, "rev-2")
    e.reaffirm(record["agreementId"], ZETA.task_id, "rev-2")
    e.reaffirm(record["agreementId"], ALPHA.task_id, "rev-2")
    e.agreement(record["agreementId"])
    e.rows(LIVE)
    e.rows("SELECT * FROM edit_reaffirmations")
    e.rows("SELECT kind, subject, detail FROM journal WHERE kind LIKE 'edit_%' ORDER BY seq")
    e.reaffirm("agr-missing", ALPHA.task_id, "rev-2")


@scenario
def closed_is_proposed_again(e):
    record = e.propose("src/a.py")
    e.settle(record["agreementId"], ALPHA.task_id, "withdrawn")
    e.reaffirm(record["agreementId"], ALPHA.task_id, REV)


@scenario
def reaffirm_never_reached(e):
    record = e.agreed("src/a.py")
    e.restate(REV, "rev-2")
    e.reaffirm(record["agreementId"], ALPHA.task_id, "rev-typo")
    e.show()


@scenario
def reaffirm_same_revision(e):
    original = e.by_beta()
    e.settle(original["agreementId"], ALPHA.task_id, "accepted")
    e.reaffirm(original["agreementId"], ALPHA.task_id, REV)
    e.agreement(original["agreementId"])


@scenario
def answering_side_reaffirms(e):
    original = e.by_beta()
    e.moved("rev-2", "rev-3")
    e.settle(original["agreementId"], ALPHA.task_id, "accepted")
    successor = e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-3")
    e.agreement(original["agreementId"])
    e.settle(successor["agreementId"], BETA.task_id, "accepted")
    e.show()


@scenario
def proposing_side_control(e):
    original = e.by_beta()
    e.moved("rev-2")
    e.reaffirm(original["agreementId"], BETA.task_id, "rev-2")


@scenario
def agreed_carried_asks_again(e):
    original = e.by_beta()
    e.settle(original["agreementId"], ALPHA.task_id, "accepted")
    e.moved("rev-2")
    e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-2")


@scenario
def restates_own_condition(e):
    original = e.by_beta()
    e.moved("rev-2")
    e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-2",
               condition="alpha's condition, lines 14-15 at rev-2")


@scenario
def second_carry(e):
    original = e.by_beta()
    e.moved("rev-2")
    first = e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-2",
                       condition="alpha's condition at rev-2")
    e.restate("rev-2", "rev-3")
    e.reaffirm(first["agreementId"], BETA.task_id, "rev-3")
    e.rows("SELECT * FROM edit_reaffirmations ORDER BY recorded_at, agreement_id")


@scenario
def decline_after_carry(e):
    original = e.by_beta()
    e.moved("rev-2")
    successor = e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-2")
    e.settle(successor["agreementId"], BETA.task_id, "declined",
             condition="only if parse keeps forwarding", reason="the tree moved")


@scenario
def conditionless_decline_after_carry(e):
    original = e.by_beta()
    e.moved("rev-2")
    successor = e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-2")
    e.settle(successor["agreementId"], BETA.task_id, "declined", reason="changed our mind")


@scenario
def legacy_successor(e):
    original = e.by_beta()
    e.moved("rev-2")
    legacy = e.step(lambda: e.regions.propose(
        repository=REPO, base_revision="rev-2", path="src/a.py", region_kind="file",
        left_project="PRJ-A", right_project="PRJ-B", peer_link_id=e.pair,
        proposer_task_id=ALPHA.task_id, constraint_text=CONSTRAINT,
        issue_key="CRW-1", supersedes=original["agreementId"]))
    e.show()
    e.restate("rev-2", "rev-3", BETA.task_id)
    e.reaffirm(legacy["agreementId"], ALPHA.task_id, "rev-3")


@scenario
def next_owner_follows_handover(e):
    original = e.by_beta()
    e.moved("rev-2")
    successor = e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-2")
    e.handover("PRJ-B", BETA.task_id, "task-beta-next", "host-b")
    e.agreement(successor["agreementId"])
    e.settle(successor["agreementId"], "task-beta-next", "accepted")


@scenario
def awaiting_without_single_parent(e):
    original = e.by_beta()
    e.moved("rev-2")
    successor = e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-2")
    e.store.db.execute("UPDATE scope_bindings SET status = 'archived' WHERE scope_key = 'PRJ-B'")
    e.agreement(successor["agreementId"])


# ---------------------------------------------------------------- EDR-13

@scenario
def failed_reaffirmation_overlap(e):
    record = e.agreed("src/a.py")
    e.restate(REV, "rev-2")
    e.propose("src/a.py", revision="rev-2", right="PRJ-Z", link=e.other)
    e.reaffirm(record["agreementId"], ALPHA.task_id, "rev-2")
    e.agreement(record["agreementId"])
    e.show()


@scenario
def carry_onto_held_place(e):
    original = e.by_beta()
    e.moved("rev-2")
    e.propose("src/a.py", revision="rev-2")
    e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-2")
    e.agreement(original["agreementId"])
    e.rows("SELECT * FROM edit_reaffirmations")


@scenario
def move_while_carrying(e):
    record = e.by_beta()
    e.moved("rev-2")
    e.racing(lambda: e.restate("rev-2", "rev-3", BETA.task_id))
    e.reaffirm(record["agreementId"], ALPHA.task_id, "rev-2")
    e.agreement(record["agreementId"])
    e.rows("SELECT * FROM edit_reaffirmations")
    e.rows("SELECT * FROM edit_agreements WHERE base_revision = 'rev-2'")
    e.show()


@scenario
def same_pair_while_carrying(e):
    record = e.by_beta()
    e.moved("rev-2")
    e.racing(lambda: e.propose("src/a.py", revision="rev-2"))
    e.reaffirm(record["agreementId"], ALPHA.task_id, "rev-2")
    e.agreement(record["agreementId"])
    e.rows("SELECT * FROM edit_reaffirmations")
    e.show()


@scenario
def carry_elsewhere(e):
    record = e.by_beta()
    e.moved("rev-2")
    for path, right, link in (("src/b.py", "PRJ-B", e.pair), ("src/a.py", "PRJ-Z", e.other)):
        e.step(lambda: e.regions.propose(
            repository=REPO, base_revision="rev-2", path=path, region_kind="file",
            left_project="PRJ-A", right_project=right, peer_link_id=link,
            proposer_task_id=ALPHA.task_id, constraint_text="elsewhere",
            supersedes=record["agreementId"],
            carry={"predecessor": record["agreementId"], "restated": None}))
    e.step(lambda: e.regions.propose(
        repository=REPO, base_revision="rev-2", path="src/a.py", region_kind="file",
        left_project="PRJ-A", right_project="PRJ-B", peer_link_id=e.pair,
        proposer_task_id=ALPHA.task_id, constraint_text="elsewhere",
        supersedes="agr-other", carry={"predecessor": record["agreementId"], "restated": None}))
    e.step(lambda: e.regions.propose(
        repository=REPO, base_revision="rev-2", path="src/a.py", region_kind="file",
        left_project="PRJ-A", right_project="PRJ-B", peer_link_id=e.pair,
        proposer_task_id=ALPHA.task_id, constraint_text="elsewhere",
        supersedes="agr-gone", carry={"predecessor": "agr-gone", "restated": None}))
    e.agreement(record["agreementId"])
    e.rows("SELECT * FROM edit_reaffirmations")
    e.rows("SELECT * FROM edit_agreements WHERE base_revision = 'rev-2'")
    e.show()


@scenario
def handover_while_carrying(e):
    record = e.by_beta()
    e.moved("rev-2")
    e.racing(lambda: e.handover("PRJ-A", ALPHA.task_id, "task-alpha-next", "host-a"))
    e.reaffirm(record["agreementId"], ALPHA.task_id, "rev-2")
    e.agreement(record["agreementId"])
    e.rows("SELECT * FROM edit_reaffirmations")
    e.rows("SELECT * FROM edit_regions WHERE base_revision = 'rev-2'")
    e.show()


@scenario
def classification_clash(e):
    original = e.by_beta()
    e.moved("rev-2")
    e.propose("src/a.py", revision="rev-2", right="PRJ-Z", link=e.other,
              region_class="generated", regenerate_from="derive")
    e.reaffirm(original["agreementId"], ALPHA.task_id, "rev-2")
    e.agreement(original["agreementId"])
    e.rows("SELECT * FROM edit_reaffirmations")
    e.show()


@scenario
def settled_while_carrying(e):
    record = e.by_beta()
    e.moved("rev-2")
    e.racing(lambda: e.settle(record["agreementId"], BETA.task_id, "withdrawn"))
    e.reaffirm(record["agreementId"], ALPHA.task_id, "rev-2")
    e.show()


# ---------------------------------------------------------------- EDR-8

@scenario
def authorizes_nothing(e):
    e.propose("src/a.py")


out = {}
for name, fn in SCENARIOS.items():
    env = Env()
    fn(env)
    out[name] = env.steps
    env.store.close()
json.dump(out, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
