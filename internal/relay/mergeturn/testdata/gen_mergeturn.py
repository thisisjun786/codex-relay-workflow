"""Record Python merge-turn, merge-target and wake answers for todo 26.

Run from the repository root with HOME/XDG_*/CODEX_HOME/TMPDIR in a scratch directory:
  uv run --no-sync python internal/relay/mergeturn/testdata/gen_mergeturn.py \
      > internal/relay/mergeturn/testdata/python_mergeturn.json
Every scenario is a fresh store with FakeClock(1_700_000_000) and the MergeTurnTestCase fixture
(task-alpha on PRJ-A, task-beta on PRJ-B, task-supervisor above both, FakeTarget owner/repo dev
reading base-0). Each step is {"ok": <json>} or {"refused": {"reason", "detail"}}.
"""
import json
import os
import io
import re
import subprocess
from contextlib import redirect_stdout
import sys
import tempfile

sys.path.insert(0, "packages/codex-session-relay")
from codex_session_relay.clock import FakeClock  # noqa: E402
from codex_session_relay.coordination import DOMAIN_MERGE_TARGET, Conflicts  # noqa: E402
from codex_session_relay.errors import RelayError  # noqa: E402
from codex_session_relay.linkage import Linkage, PARENT  # noqa: E402
from codex_session_relay.mergeturn import MergeTurn, target_key  # noqa: E402
from codex_session_relay.mergetarget import TargetReader, TargetUnreadable, same_commit  # noqa: E402
from codex_session_relay.models import Endpoint  # noqa: E402
from codex_session_relay.delivery import DeliveryService  # noqa: E402
from codex_session_relay.fakehost import FakeHostAdapter  # noqa: E402
from codex_session_relay.registry import Registry, record_settings  # noqa: E402
from codex_session_relay import rolepolicy  # noqa: E402
from tests.support import task_settings  # noqa: E402
from codex_session_relay.receipts import ReceiptIntake  # noqa: E402
from codex_session_relay.store import Store  # noqa: E402
from tests.support import FakeTarget  # noqa: E402

REPO, BASE, PROJECT_A, PROJECT_B, POST = "owner/repo", "dev", "PRJ-A", "PRJ-B", "base-1"
GREEN = {"hasNextPage": False, "pagesRead": 1, "totalCount": 1,
         "threadsSeen": ["thread-1"], "unresolved": 0}
CROSSED = json.load(open(
    "packages/codex-session-relay/tests/fixtures/merge_turn_crossed_handoff.json",
    encoding="utf-8"))
SCENARIOS = {}


def scenario(fn):
    SCENARIOS[fn.__name__] = fn
    return fn


def run_checks(head, conclusion="success", attempt=1, name="dev-gate", run="run-1"):
    return [{"runId": run, "name": name, "headSha": head,
             "conclusion": conclusion, "attempt": attempt}]


class World:
    def __init__(self):
        directory = tempfile.mkdtemp(dir=os.environ["TMPDIR"])
        self.store = Store(os.path.join(directory, "relay.sqlite3"))
        self.clock = FakeClock()
        self.linkage = Linkage(self.store, self.clock)
        self.target = FakeTarget()
        self.target.set(REPO, BASE, "base-0")
        self.turns = MergeTurn(self.store, self.clock, self.linkage, target_reader=self.target)
        self.alpha = Endpoint("task-alpha", "host-a", cwd="/alpha")
        self.beta = Endpoint("task-beta", "host-b", cwd="/beta")
        self.supervisor = Endpoint("task-supervisor", "host-s", cwd="/sup")
        self.linkage.bind_scope(role=PARENT, scope_key=PROJECT_A, endpoint=self.alpha)
        self.linkage.bind_scope(role=PARENT, scope_key=PROJECT_B, endpoint=self.beta)
        for project, parent in ((PROJECT_A, self.alpha), (PROJECT_B, self.beta)):
            self.linkage.register_supervision(
                initiative_key="INIT-1", project_key=project,
                supervisor=self.supervisor, parent=parent)
        self.out = []

    def step(self, call):
        try:
            value = call()
        except RelayError as error:
            self.out.append({"refused": {"reason": error.reason.value if error.reason else None,
                                         "detail": error.detail}})
            return None
        self.out.append({"ok": value})
        return value

    def rows(self, sql, params=()):
        self.out.append({"ok": [dict(row) for row in self.store.all(sql, params)]})

    def reads(self):
        self.out.append({"ok": len(self.target.reads)})

    def turn(self, turn):
        self.step(lambda: self.turns.turn(turn))

    def contests(self):
        self.step(lambda: Conflicts(self.store).all(DOMAIN_MERGE_TARGET, target_key(REPO, BASE)))

    def claim(self, endpoint, project, head, base=BASE, ready=True):
        return self.turns.request(repository=REPO, base_ref=base, project_key=project,
                                  holder=endpoint, candidate_head=head, ready=ready)

    def answer(self, turn, actor):
        grant = self.turns.turn(turn)["grant"]
        if grant is not None:
            self.turns.acknowledge_grant(turn, actor=actor, grant=grant["grantId"],
                                         evidence="read the grant and re-checked the record")

    def held(self, endpoint=None, project=PROJECT_A, head="head-a", base=BASE):
        endpoint = endpoint or self.alpha
        held = self.claim(endpoint, project, head, base=base)
        self.answer(held["turnId"], endpoint.task_id)
        return held["turnId"]

    def begin(self, turn, **overrides):
        arguments = {"actor": self.alpha.task_id, "head_sha": "head-a", "base_sha": "base-0",
                     "checks": run_checks("head-a"), "review": dict(GREEN),
                     "required": ["dev-gate"]}
        arguments.update(overrides)
        return self.turns.begin_merge(turn, **arguments)

    def check(self, turn, head="head-a", base="base-0", actor=None):
        return self.turns.begin_merge(
            turn, actor=actor or self.alpha.task_id, head_sha=head, base_sha=base,
            checks=run_checks(head), review=dict(GREEN), required=["dev-gate"])

    def merging(self, head="head-a", base="base-0"):
        turn = self.held(head=head)
        self.check(turn, head=head, base=base)
        return turn

    def merged(self, tip=POST):
        self.target.set(REPO, BASE, tip)

    def land(self, turn, landed="merge-1", observed=None, actor=None):
        arguments = {"actor": actor or self.alpha.task_id, "landed_sha": landed,
                     "evidence": "merged; the base branch read afterwards"}
        if observed is not None:
            arguments["observed_base_sha"] = observed
        return self.turns.land(turn, **arguments)

    def landed(self, head="head-a", tip=POST):
        turn = self.merging(head=head)
        self.merged(tip)
        self.land(turn)
        return turn

    def restate(self, turn, actor=None, observed=None, evidence="read the branch again"):
        arguments = {"actor": actor or self.alpha.task_id, "evidence": evidence}
        if observed is not None:
            arguments["observed_base_sha"] = observed
        return self.turns.restate_base(turn, **arguments)

    def unknown(self):
        turn = self.merging()
        self.turns.report_unknown(turn, actor=self.alpha.task_id, reason="lost the connection")
        return turn

    def resolve(self, turn, observed, state, evidence, actor=None):
        return self.turns.resolve_unknown(
            turn, actor=actor or self.supervisor.task_id, observed_base_sha=observed,
            pr_state=state, evidence=evidence)

    def r3_landing(self, turn):
        self.store.db.execute(
            "UPDATE merge_turns SET observed_base_sha = checked_base_sha WHERE turn_id = ?",
            (turn,))

    def legacy_row(self, turn, kind, key, evidence_kind, evidence, states=("landed", None)):
        self.store.db.execute(
            "INSERT INTO merge_turn_ledger (entry_id, turn_id, kind, from_state, to_state,"
            " evidence_kind, actor_task_id, evidence, idempotency_key, recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?)",
            ("legacy-" + key, turn, kind, states[0], states[1], evidence_kind,
             self.alpha.task_id, evidence, key, self.clock.iso()))

    def checks_rows(self, turn):
        self.rows("SELECT * FROM merge_turn_checks WHERE turn_id = ? ORDER BY recorded_at",
                  (turn,))


# ------------------------------------------------------------------ part F: MTN-1..9, 16..17, 20 and CCT-2


def status(w, project, value):
    w.store.db.execute("UPDATE scope_bindings SET status=? WHERE scope_key=? AND role='parent'",
                       (value, project))


def f_claim(w, endpoint=None, project=PROJECT_A, head="head-a", ready=True):
    return w.claim(endpoint or w.alpha, project, head, ready=ready)["turnId"]


def f_ready(w, turn, actor=None, ready=True, cause=None, head=None):
    kw = {"actor": actor or w.alpha.task_id, "ready": ready}
    if cause is not None:
        kw["cause"] = cause
    if head is not None:
        kw["candidate_head"] = head
    return w.turns.declare_ready(turn, **kw)


def f_release(w, turn, actor=None, reason="done", disposition="returned", evidence=""):
    return w.turns.release(turn, actor=actor or w.alpha.task_id,
                           disposition=disposition, reason=reason, evidence=evidence)


def f_attest(w, turn, kind="transport_accepted", key="delivery-1", evidence="accepted"):
    return w.turns.attest(turn, evidence_kind=kind, idempotency_key=key,
                          actor=w.beta.task_id, evidence=evidence)


@scenario
def mtn3_holder_paused(w):
    f_claim(w)
    f_claim(w, w.beta, PROJECT_B, "head-b")
    status(w, PROJECT_A, "paused")
    w.step(lambda: w.turns.target(REPO, BASE))


@scenario
def mtn3_merge_in_flight(w):
    turn = w.held()
    w.step(lambda: w.begin(turn))
    w.step(lambda: w.turns.target(REPO, BASE))


@scenario
def mtn3_causes(w):
    for i, cause in enumerate(("required", "review", "unready", "disagree", "lost",
                                "withheld", "old", "moved", "repeat")):
        base = "cause-" + str(i)
        w.target.set(REPO, base, "base-0")
        held = w.claim(w.alpha, PROJECT_A, "head-a", base=base,
                       ready=cause != "unready")["turnId"]
        if cause in ("required", "lost", "withheld"):
            w.claim(w.beta, PROJECT_B, "head-b", base=base)
        if cause in ("required", "review", "disagree", "old", "moved", "repeat"):
            w.answer(held, w.alpha.task_id)
        if cause in ("required", "disagree", "old", "repeat"):
            w.step(lambda: w.begin(held, checks=run_checks("head-a", "failure")))
        if cause in ("review", "disagree"):
            w.step(lambda: w.begin(held, review={"hasNextPage": True, "pagesRead": 1,
                                    "totalCount": 4, "threadsSeen": ["thread-1"], "unresolved": 0}))
        if cause == "lost":
            w.store.db.execute("UPDATE scope_bindings SET task_id='task-alpha-2' "
                               "WHERE scope_key=? AND role='parent'", (PROJECT_A,))
        if cause == "withheld":
            status(w, PROJECT_B, "paused")
        if cause == "old":
            w.step(lambda: f_ready(w, held, head="head-b"))
            w.step(lambda: f_ready(w, held))
        if cause == "moved":
            w.step(lambda: w.begin(held, head_sha="head-b", checks=run_checks("head-b")))
        if cause == "repeat":
            w.step(lambda: w.begin(held, checks=run_checks("head-a", "failure")))
        w.step(lambda: w.turns.target(REPO, base))
        if cause == "lost":
            w.store.db.execute("UPDATE scope_bindings SET task_id='task-alpha' "
                               "WHERE scope_key=? AND role='parent'", (PROJECT_A,))
        if cause == "withheld":
            status(w, PROJECT_B, "active")
    w.step(lambda: w.turns.target(REPO, "empty"))


@scenario
def mtn1_paused(w):
    status(w, PROJECT_A, "paused")
    w.step(lambda: w.claim(w.alpha, PROJECT_A, "head-a"))
    w.step(lambda: w.turns.target(REPO, BASE))


@scenario
def mtn1_waiter(w):
    held = f_claim(w)
    waiter = f_claim(w, w.beta, PROJECT_B, "head-b", False)
    status(w, PROJECT_B, "paused")
    w.step(lambda: f_release(w, held))
    w.step(lambda: f_ready(w, waiter, w.beta.task_id))
    w.step(lambda: w.turns.target(REPO, BASE))
    w.turn(waiter)
    status(w, PROJECT_B, "active")
    w.step(lambda: w.turns.outstanding(w.beta.task_id))
    w.step(lambda: f_ready(w, waiter, w.beta.task_id))
    status(w, PROJECT_B, "paused")
    grant = w.turns.turn(waiter)["grant"]["grantId"]
    w.step(lambda: w.turns.acknowledge_grant(waiter, actor=w.beta.task_id,
                                              grant=grant, evidence="back now"))
    w.step(lambda: w.begin(waiter, actor=w.beta.task_id, head_sha="head-b",
                            checks=run_checks("head-b")))


@scenario
def mtn1_skip(w):
    held = f_claim(w)
    first = f_claim(w, w.beta, PROJECT_B, "head-b")
    gamma = Endpoint("task-gamma", "host-g", cwd="/gamma")
    w.linkage.bind_scope(role=PARENT, scope_key="PRJ-C", endpoint=gamma)
    w.linkage.register_supervision(initiative_key="INIT-1", project_key="PRJ-C",
                                   supervisor=w.supervisor, parent=gamma)
    second = f_claim(w, gamma, "PRJ-C", "head-c")
    status(w, PROJECT_B, "paused")
    w.step(lambda: w.turns.target(REPO, BASE))
    w.step(lambda: f_release(w, held))
    w.turn(first)
    w.turn(second)


@scenario
def mtn2_readiness(w):
    held = f_claim(w)
    waiter = f_claim(w, w.beta, PROJECT_B, "head-b")
    w.step(lambda: f_ready(w, held, ready=False, cause="a new finding arrived on the pull request"))
    w.step(lambda: f_release(w, held, reason="my readiness died; handing it on"))
    w.turn(waiter)
    next_turn = f_claim(w, head="head-c")
    w.step(lambda: f_ready(w, next_turn, head="head-c2"))
    w.turn(next_turn)


@scenario
def mtn4_outstanding(w):
    held = w.step(lambda: w.claim(w.alpha, PROJECT_A, "head-a"))["turnId"]
    w.step(lambda: w.turns.outstanding(w.alpha.task_id))
    w.step(lambda: w.claim(w.alpha, PROJECT_A, "head-a"))
    other_store = Store(w.store.path)
    after = MergeTurn(other_store, FakeClock(), Linkage(other_store, FakeClock()))
    w.step(lambda: after.outstanding(w.alpha.task_id))
    other_store.close()
    w.answer(held, w.alpha.task_id)
    w.step(lambda: w.begin(held))
    w.step(lambda: w.turns.report_unknown(held, actor=w.alpha.task_id, reason="the host stopped answering"))
    w.clock.advance(1_000_000)
    w.step(lambda: w.turns.outstanding(w.alpha.task_id))


@scenario
def mtn5_claims(w):
    w.step(lambda: w.claim(w.alpha, PROJECT_A, "head-a"))
    waiter = w.step(lambda: w.claim(w.beta, PROJECT_B, "head-b"))["turnId"]
    w.step(lambda: w.turns.target(REPO, BASE))
    w.step(lambda: w.turns.attest(waiter, evidence_kind="claimed_turn", idempotency_key="chat-1",
                                  actor=w.beta.task_id, evidence="I said in chat that it is my turn"))
    w.turn(waiter)
    w.step(lambda: w.claim(w.alpha, PROJECT_A, "head-a"))
    w.rows("SELECT * FROM merge_turns WHERE target_key=?", (target_key(REPO, BASE),))
    w.step(lambda: w.claim(w.beta, PROJECT_A, "head-x"))
    w.contests()
    w.step(lambda: w.claim(w.alpha, "PRJ-UNKNOWN", "head-x"))
    w.step(lambda: w.turns.request(repository="owner/other", base_ref=BASE, project_key=PROJECT_B,
                                    holder=w.beta, candidate_head="head-b", ready=True))


@scenario
def mtn6_alpha_first(w):
    w.step(lambda: w.claim(w.alpha, PROJECT_A, "head-a"))
    w.step(lambda: w.claim(w.beta, PROJECT_B, "head-b"))
    w.step(lambda: w.turns.target(REPO, BASE))
    w.rows("SELECT turn_id,state FROM merge_turns WHERE target_key=? ORDER BY turn_id",
           (target_key(REPO, BASE),))


@scenario
def mtn6_beta_first(w):
    w.step(lambda: w.claim(w.beta, PROJECT_B, "head-b"))
    w.step(lambda: w.claim(w.alpha, PROJECT_A, "head-a"))
    w.step(lambda: w.turns.target(REPO, BASE))
    w.rows("SELECT turn_id,state FROM merge_turns WHERE target_key=? ORDER BY turn_id",
           (target_key(REPO, BASE),))


@scenario
def mtn7_transport(w):
    held = f_claim(w)
    waiter = f_claim(w, w.beta, PROJECT_B, "head-b")
    w.step(lambda: w.turns.request_return(held, actor=w.beta.task_id,
                                           evidence="I have a ready candidate"))
    for _ in range(3):
        w.step(lambda: f_attest(w, held, evidence="the relay accepted the message"))
    w.step(lambda: w.turns.target(REPO, BASE))
    w.turn(waiter)
    w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id=? ORDER BY recorded_at,entry_id", (held,))
    w.step(lambda: f_release(w, held, reason="candidate is not ready"))


@scenario
def mtn8_promotion(w):
    held = f_claim(w)
    waiter = f_claim(w, w.beta, PROJECT_B, "head-b")
    w.step(lambda: f_release(w, held, reason="checks are not green yet"))
    w.turn(waiter)


@scenario
def mtn8_unready(w):
    held = f_claim(w)
    waiter = f_claim(w, w.beta, PROJECT_B, "head-b", False)
    w.step(lambda: f_release(w, held))
    w.step(lambda: w.turns.target(REPO, BASE))
    w.step(lambda: f_ready(w, waiter, w.beta.task_id))


@scenario
def mtn8_occupied(w):
    f_claim(w)
    waiter = f_claim(w, w.beta, PROJECT_B, "head-b", False)
    w.step(lambda: f_ready(w, waiter, w.beta.task_id))


@scenario
def mtn8_lost_owner(w):
    held = f_claim(w)
    waiter = f_claim(w, w.beta, PROJECT_B, "head-b")
    status(w, PROJECT_B, "archived")
    w.step(lambda: f_release(w, held))
    w.turn(waiter)
    w.contests()


@scenario
def mtn8_cancel(w):
    held = f_claim(w)
    w.step(lambda: f_release(w, held, actor=w.supervisor.task_id, disposition="cancelled",
                             reason="parent stopped answering", evidence="no turn for two hours, host checked"))
    w.step(lambda: f_release(w, held, reason="I am back"))
    w.turn(held)


@scenario
def mtn8_evidence(w):
    held = f_claim(w)
    w.step(lambda: f_release(w, held, actor=w.supervisor.task_id, disposition="cancelled",
                             reason="it stopped"))


@scenario
def mtn9_no_clock_release(w):
    held = w.merging()
    waiter = f_claim(w, w.beta, PROJECT_B, "head-b")
    w.step(lambda: f_release(w, held, actor=w.supervisor.task_id, disposition="cancelled",
                             reason="host stopped", evidence="host checked"))
    w.step(lambda: w.turns.report_unknown(held, actor=w.alpha.task_id, reason="lost"))
    w.clock.advance(1_000_000)
    w.step(lambda: w.turns.target(REPO, BASE))
    w.turn(waiter)
    w.step(lambda: w.resolve(held, "", "merged", "I looked"))
    w.merged()
    w.step(lambda: w.resolve(held, POST, "merged", "the pull request reads merged"))
    w.turn(waiter)


@scenario
def mtn16_grants(w):
    held = w.step(lambda: w.claim(w.alpha, PROJECT_A, "head-a"))["turnId"]
    grant = w.turns.turn(held)["grant"]["grantId"]
    for _ in range(2):
        w.step(lambda: w.turns.acknowledge_grant(held, actor=w.alpha.task_id, grant=grant,
                                                 evidence="read and checked"))
    w.step(lambda: f_ready(w, held, head="head-a2"))
    w.turn(held)
    w.step(lambda: w.turns.acknowledge_grant(held, actor=w.alpha.task_id, grant=grant,
                                             evidence="old head"))
    w.step(lambda: w.turns.acknowledge_grant(held, actor=w.alpha.task_id,
                  grant=w.turns.turn(held)["grant"]["grantId"], evidence=""))
    w.step(lambda: w.turns.acknowledge_grant(held, actor=w.beta.task_id,
                  grant=w.turns.turn(held)["grant"]["grantId"], evidence="read"))
    w.step(lambda: f_release(w, held))
    w.step(lambda: w.turns.acknowledge_grant(held, actor=w.alpha.task_id, grant=grant,
                                             evidence="late"))
    w.step(lambda: w.claim(w.alpha, PROJECT_A, "head-a"))


@scenario
def mtn17_namespace(w):
    held = f_claim(w)
    for kind, key in (("grant", "mine-1"), ("landing_base_restated", "mine-2"),
                      ("transport_accepted", "close:landed"),
                      ("transport_accepted", "restate-base:1")):
        w.step(lambda: f_attest(w, held, kind, key))
    w.step(lambda: f_attest(w, held, key="delivery-9"))
    waiter = f_claim(w, w.beta, PROJECT_B, "head-b")
    tenure = w.turns.turn(waiter)["tenure"]
    key = "promote:" + str(tenure)
    w.store.db.execute("INSERT INTO merge_turn_ledger (entry_id,turn_id,kind,from_state,to_state,"
                       "evidence_kind,actor_task_id,evidence,idempotency_key,recorded_at)"
                       " VALUES (?,?,?,?,?,?,?,?,?,?)",
                       ("squatted-1", waiter, "attestation", None, None, "transport_accepted",
                        w.beta.task_id, "not a promotion", key, "2026-01-01T00:00:00Z"))
    w.step(lambda: f_release(w, held, reason="handing it on"))
    w.turn(held)
    w.turn(waiter)
    w.step(lambda: w.turns.target(REPO, BASE))


def legacy_grant(w, turn, kind, key, evidence):
    w.store.db.execute("INSERT INTO merge_turn_ledger (entry_id,turn_id,kind,from_state,to_state,"
                       "evidence_kind,actor_task_id,evidence,idempotency_key,recorded_at)"
                       " VALUES (?,?,?,?,?,?,?,?,?,?)",
                       ("legacy-"+key, turn, "attestation", None, None, kind, w.beta.task_id,
                        evidence, key, "2026-01-01T00:00:00Z"))


@scenario
def mtn17_legacy(w):
    held = f_claim(w)
    grant = w.turns.turn(held)["grant"]["grantId"]
    for key, text in (("chat-note-1", "approved in chat"), ("grant:not-json", "{oops"),
                      ("grant:no-sequence", '{"grantId": "mtg-forged"}')):
        legacy_grant(w, held, "grant", key, text)
    w.turn(held)
    w.step(lambda: w.turns.target(REPO, BASE))
    w.step(lambda: w.turns.outstanding(w.alpha.task_id))
    w.step(lambda: w.turns.acknowledge_grant(held, actor=w.alpha.task_id, grant=grant,
                                             evidence="read the grant past the rows nobody can read"))
    w.step(lambda: w.begin(held))


@scenario
def mtn17_foreign_grants(w):
    from codex_session_relay.mergeturn import grant_id
    held = f_claim(w)
    mine = w.turns.turn(held)["grant"]["grantId"]
    tenure = w.turns.turn(held)["tenure"]
    foreign = [("grant:mtg-forged", {"grantId": "mtg-forged", "sequence": 9999,
                "turnId": held, "tenure": tenure, "recipientTaskId": w.beta.task_id,
                "candidateHead": "head-x"}),
               ("grant:" + grant_id(held, tenure, 9998),
                {"grantId": grant_id(held, tenure, 9998), "sequence": 9998,
                 "turnId": "some-other-turn", "tenure": tenure,
                 "recipientTaskId": w.beta.task_id, "candidateHead": "head-x"}),
               ("wrong-key", {"grantId": grant_id(held, tenure, 9997),
                               "sequence": 9997, "turnId": held, "tenure": tenure,
                               "recipientTaskId": w.beta.task_id, "candidateHead": "head-x"})]
    for key, envelope in foreign:
        legacy_grant(w, held, "grant", key, json.dumps(envelope, sort_keys=True, separators=(",", ":")))
    w.turn(held)
    w.step(lambda: w.turns.acknowledge_grant(held, actor=w.alpha.task_id, grant=mine,
                         evidence="the impersonating rows are not this turn's grant"))
    w.turn(held)


@scenario
def mtn17_only_legacy(w):
    held = f_claim(w)
    w.store.db.execute("DELETE FROM merge_turn_ledger WHERE turn_id=? AND evidence_kind='grant'", (held,))
    legacy_grant(w, held, "grant", "chat-note-1", "approved in chat")
    w.turn(held)
    w.step(lambda: w.begin(held))


@scenario
def mtn20_readings(w):
    for i, mode in enumerate(("reads", "stale", "own", "unreadable", "early", "mark", "abbrev")):
        base = "read-" + str(i)
        w.target.set(REPO, base, "base-0")
        held = w.held(base=base)
        if mode == "reads":
            w.target.set(REPO, base, "c" * 40)
            w.step(lambda: w.check(held, base="C" * 40))
        elif mode in ("stale", "own"):
            w.step(lambda: w.check(held, base="base-x" if mode == "stale" else "head-a"))
            w.turn(held)
        elif mode == "unreadable":
            w.target.forget(REPO, base)
            w.step(lambda: w.check(held))
            w.checks_rows(held)
            w.step(lambda: w.turns.target(REPO, base))
        elif mode == "early":
            w.reads()
            w.step(lambda: w.check(held, head="head-z"))
            w.step(lambda: w.check(held, actor=w.beta.task_id))
            w.reads()
        elif mode == "mark":
            w.step(lambda: w.check(held))
            w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id=? AND evidence_kind='currency_confirmed'", (held,))
        else:
            w.target.set(REPO, base, "a" * 40)
            w.step(lambda: w.check(held, base="a" * 7))
            w.step(lambda: w.check(held, base="a" * 40))
            w.step(lambda: w.turns.land(held, actor=w.alpha.task_id, landed_sha="merge-1",
                                       observed_base_sha="a" * 7, evidence="merged"))


@scenario
def cct2_counts(w):
    # Record the actual Python transaction openings, not a restatement of expected counts.
    def count(call):
        original = w.store.transaction
        opened = 0
        def shim():
            nonlocal opened
            opened += 1
            return original()
        w.store.transaction = shim
        try:
            call()
        finally:
            w.store.transaction = original
        w.step(lambda: opened)

    held = w.held()
    count(lambda: f_ready(w, held))
    count(lambda: f_attest(w, held))
    count(lambda: w.begin(held))
    def refused(call):
        try:
            call()
        except RelayError:
            pass
    count(lambda: refused(lambda: w.turns.land(held, actor="task-stranger",
                                                landed_sha="merge-1", evidence="merged")))
    w.merged()
    count(lambda: w.land(held, observed=POST))
    other = w.held(head="head-c")
    count(lambda: w.turns.turn(other))
    count(lambda: w.turns.ledger(other))
    count(lambda: w.turns.target(REPO, BASE))
    count(lambda: refused(lambda: w.check(other, head="head-moved")))
    count(lambda: w.claim(w.beta, PROJECT_B, "head-d", base="another"))
    w.answer(other, w.alpha.task_id)
    w.check(other, head="head-c", base=POST)
    w.turns.report_unknown(other, actor=w.alpha.task_id, reason="lost")
    count(lambda: w.resolve(other, POST, "merged", "the pull request reads merged",
                            actor=w.alpha.task_id))
    w.merged("base-2")
    count(lambda: w.restate(other))
    before = w.turns.target(REPO, BASE)
    w.clock.advance(1_000_000)
    w.step(lambda: before == w.turns.target(REPO, BASE))


# ------------------------------------------------------------------ MTN-11

MALFORMED = [
    dict(GREEN, threadsSeen=1), dict(GREEN, threadsSeen="ab", totalCount=2),
    dict(GREEN, threadsSeen=[1]), dict(GREEN, unresolved=[]),
    dict(GREEN, unresolved=["thread-1"]), dict(GREEN, pagesRead="1"),
    dict(GREEN, hasNextPage="false"), [], "review",
    dict(GREEN, pagesRead=-1), dict(GREEN, totalCount=1.5, hasNextPage=None),
]


@scenario
def mtn11_malformed(w):
    for index, review in enumerate(MALFORMED):
        branch = "dev-" + str(index)
        w.target.set(REPO, branch, "base-0")
        turn = w.held(base=branch)
        w.reads()
        w.step(lambda: w.begin(turn, review=review))
        w.reads()
        w.checks_rows(turn)
        w.step(lambda: w.begin(turn))
    w.checks_rows(w.turns.target(REPO, "dev-0")["holder"]["turnId"])


# ------------------------------------------------------------------ MTN-10 remainder / MTN-12

def refusal_then(w, **overrides):
    turn = w.held()
    w.step(lambda: w.begin(turn, **overrides))
    w.checks_rows(turn)
    w.turn(turn)


@scenario
def mtn12_required_missing(w):
    refusal_then(w, required=["dev-gate", "devin"])


@scenario
def mtn12_newest_failing(w):
    refusal_then(w, checks=run_checks("head-a", attempt=1)
                 + run_checks("head-a", conclusion="failure", attempt=2))


@scenario
def mtn12_newest_passing(w):
    refusal_then(w, checks=run_checks("head-a", conclusion="failure", attempt=1)
                 + run_checks("head-a", attempt=2))


@scenario
def mtn12_optional_failing(w):
    refusal_then(w, checks=run_checks("head-a")
                 + run_checks("head-a", conclusion="failure", name="lint", run="run-2"))


@scenario
def mtn12_nothing_required(w):
    refusal_then(w, required=[],
                 checks=run_checks("head-a", conclusion="failure", name="lint", run="run-2"))


@scenario
def mtn12_other_head(w):
    refusal_then(w, checks=run_checks("head-other"))


@scenario
def mtn12_no_checks(w):
    refusal_then(w, checks=[], required=[])


@scenario
def mtn12_no_identity(w):
    turn = w.held()
    for entry in ({"runId": "", "name": "dev-gate"}, {"runId": "run-1", "name": ""}):
        check = dict(entry, headSha="head-a", conclusion="success", attempt=1)
        w.step(lambda: w.begin(turn, checks=[check], required=[]))


@scenario
def mtn12_ci_unfinished(w):
    refusal_then(w, checks=run_checks("head-a", conclusion=None))


@scenario
def mtn12_base_moved_since_landing(w):
    first = w.held()
    w.begin(first)
    w.merged(POST)
    w.step(lambda: w.turns.land(first, actor=w.alpha.task_id, landed_sha="merge-1",
                                observed_base_sha=POST, evidence="landed"))
    second = w.held(head="head-c")
    w.step(lambda: w.begin(second, head_sha="head-c", checks=run_checks("head-c"),
                           base_sha="base-0"))
    w.step(lambda: w.begin(second, head_sha="head-c", checks=run_checks("head-c"),
                           base_sha=POST))


@scenario
def mtn10_review_rows(w):
    turn = w.held()
    for review in ({"pagesRead": 1}, {"hasNextPage": True, "pagesRead": 1, "totalCount": 2,
                                      "threadsSeen": ["one"], "unresolved": 0},
                   {"hasNextPage": False, "pagesRead": 2, "totalCount": 1,
                    "threadsSeen": ["one"], "unresolved": 1},
                   {"hasNextPage": False, "pagesRead": 2, "totalCount": 2,
                    "threadsSeen": ["thread-1", "thread-1"], "unresolved": 0},
                   {"hasNextPage": False, "pagesRead": 0, "totalCount": 3,
                    "threadsSeen": ["a", " ", ""], "unresolved": 0},
                   None):
        w.step(lambda: w.begin(turn, review=review))
    w.checks_rows(turn)
    w.step(lambda: w.begin(turn, review={"hasNextPage": False, "pagesRead": 2,
                                         "totalCount": 2, "threadsSeen": ["t-1", "t-2"],
                                         "unresolved": 0}))


# ------------------------------------------------------------------ MTN-13

@scenario
def mtn13_former_parent(w):
    turn = w.held()
    w.store.db.execute("UPDATE scope_bindings SET status = 'archived'"
                       "  WHERE scope_key = ? AND task_id = ?", (PROJECT_A, w.alpha.task_id))
    w.step(lambda: w.begin(turn, required=[]))
    w.contests()


@scenario
def mtn13_unready(w):
    turn = w.claim(w.alpha, PROJECT_A, "head-a", ready=False)["turnId"]
    w.answer(turn, w.alpha.task_id)
    w.step(lambda: w.begin(turn, required=[]))


@scenario
def mtn13_not_holder(w):
    turn = w.held()
    w.step(lambda: w.begin(turn, actor=w.beta.task_id))
    w.contests()


@scenario
def mtn13_unanswered_and_paused(w):
    turn = w.claim(w.alpha, PROJECT_A, "head-a")["turnId"]
    w.step(lambda: w.begin(turn))
    w.store.db.execute("UPDATE scope_bindings SET status = 'paused' WHERE scope_key = ?",
                       (PROJECT_A,))
    w.step(lambda: w.begin(turn, head_sha="head-z"))


# ------------------------------------------------------------------ MTN-14

@scenario
def mtn14_authority(w):
    held = w.claim(w.alpha, PROJECT_A, "head-a")["turnId"]
    w.step(lambda: w.turns.release(held, actor="task-stranger", disposition="cancelled",
                                   reason="I want it", evidence="none of my business"))
    w.turn(held)
    other = w.claim(w.beta, PROJECT_B, "head-b", base="dev-b")["turnId"]
    w.step(lambda: w.turns.release(other, actor=w.supervisor.task_id, disposition="cancelled",
                                   reason="stuck", evidence="the parent stopped answering"))
    w.answer(held, w.alpha.task_id)
    w.check(held)
    w.step(lambda: w.turns.report_unknown(held, actor="task-stranger", reason="I say so"))
    w.step(lambda: w.turns.report_unknown(held, actor=w.alpha.task_id,
                                          reason="lost the connection"))
    w.step(lambda: w.resolve(held, "base-9", "merged", "I looked", actor="task-stranger"))
    w.turn(held)
    w.step(lambda: w.resolve(held, "base-0", "open",
                             "the pull request is still open on the same base",
                             actor=w.alpha.task_id))
    w.contests()


# ------------------------------------------------------------------ MTN-15

@scenario
def mtn15_open_unchanged(w):
    turn = w.unknown()
    w.step(lambda: w.resolve(turn, "base-0", "open", "still open, base unchanged"))


@scenario
def mtn15_open_moved(w):
    turn = w.unknown()
    w.merged("base-9")
    w.step(lambda: w.resolve(turn, "base-9", "open",
                             "somebody else pushed; this one is still open"))


@scenario
def mtn15_unreadable_state(w):
    turn = w.unknown()
    w.step(lambda: w.resolve(turn, "base-9", "unknown", "could not read the pull request"))
    for observed in ("base-0", "base-9"):
        w.step(lambda: w.resolve(turn, observed, "mergd", "I think it merged"))
    w.turn(turn)


@scenario
def mtn15_merged(w):
    turn = w.unknown()
    w.step(lambda: w.resolve(turn, "base-0", "merged", "the pull request reads merged"))


# ------------------------------------------------------------------ MTN-18

@scenario
def mtn18_crossed_handoff(w):
    repository, base = CROSSED["repository"], CROSSED["baseRef"]
    w.target.set(repository, base, "base-0")
    for entry in CROSSED["parents"]:
        endpoint = Endpoint(entry["task"], entry["host"], cwd="/" + entry["task"])
        w.linkage.bind_scope(role=PARENT, scope_key=entry["project"], endpoint=endpoint)
        w.linkage.register_supervision(initiative_key="INIT-1", project_key=entry["project"],
                                       supervisor=w.supervisor, parent=endpoint)

    def claim(entry):
        return w.turns.request(
            repository=repository, base_ref=base, project_key=entry["project"],
            holder=Endpoint(entry["task"], entry["host"], cwd="/" + entry["task"]),
            candidate_head=entry["head"], pr_number=entry["pr"], ready=True)

    turns = [w.step(lambda: claim(entry))["turnId"] for entry in CROSSED["parents"]]
    w.answer(turns[0], "task-hierarchy")
    w.step(lambda: w.turns.begin_merge(
        turns[0], actor="task-hierarchy", head_sha="head-73", base_sha="base-0",
        required=["dev-gate"], checks=run_checks("head-73", conclusion=None),
        review=dict(GREEN)))
    w.step(lambda: w.turns.target(repository, base))
    w.step(lambda: w.turns.request_return(turns[0], actor="task-status",
                                          evidence="my candidate is green"))
    w.step(lambda: w.turns.attest(turns[0], evidence_kind="transport_accepted",
                                  idempotency_key="msg-69", actor="task-status",
                                  evidence="the relay accepted the message"))
    w.step(lambda: w.turns.target(repository, base))
    w.step(lambda: w.turns.release(turns[0], actor="task-hierarchy", disposition="returned",
                                   reason="required CI has not finished and a peer is ready"))
    for turn in turns[2:]:
        w.turn(turn)


# ------------------------------------------------------------------ MTN-21

@scenario
def mtn21_landing_records_reading(w):
    turn = w.merging()
    w.merged()
    w.step(lambda: w.land(turn))


@scenario
def mtn21_stated_agrees(w):
    turn = w.merging()
    w.merged("d" * 40)
    w.step(lambda: w.land(turn, observed="D" * 40))


@scenario
def mtn21_trial_mistake(w):
    turn = w.merging()
    w.merged("head-a")
    w.step(lambda: w.land(turn, landed="head-a", observed="base-0"))
    w.turn(turn)
    w.contests()


@scenario
def mtn21_not_advanced(w):
    turn = w.merging()
    w.step(lambda: w.land(turn))
    w.turn(turn)
    w.merged()
    w.step(lambda: w.land(turn))


@scenario
def mtn21_already_base(w):
    turn = w.held()
    w.target.set(REPO, BASE, "head-a")
    w.step(lambda: w.check(turn, base="head-a"))
    w.step(lambda: w.land(turn, landed="head-a"))


@scenario
def mtn21_unreadable_and_blind(w):
    turn = w.merging()
    w.target.forget(REPO, BASE)
    w.step(lambda: w.land(turn))
    blind = MergeTurn(w.store, w.clock, w.linkage)
    w.step(lambda: blind.land(turn, actor=w.alpha.task_id, landed_sha="merge-1",
                              evidence="merged"))
    w.turn(turn)


@scenario
def mtn21_stranger_never_reads(w):
    turn = w.merging()
    w.reads()
    w.step(lambda: w.land(turn, actor="task-stranger"))
    w.reads()


@scenario
def mtn21_r3_turn_lands_through_resolve(w):
    turn = w.merging()
    w.store.db.execute("UPDATE merge_turns SET checked_base_sha = 'base-y' WHERE turn_id = ?",
                       (turn,))
    w.store.db.execute("UPDATE merge_turn_ledger SET evidence = 'chk-legacy'"
                       "  WHERE turn_id = ? AND evidence_kind = 'currency_confirmed'", (turn,))
    w.step(lambda: w.land(turn))
    w.step(lambda: w.turns.report_unknown(turn, actor=w.alpha.task_id,
                                          reason="checked before the relay read its base"))
    w.merged()
    w.step(lambda: w.resolve(turn, POST, "merged", "the pull request reads merged",
                             actor=w.alpha.task_id))


@scenario
def mtn21_forged_mark(w):
    turn = w.merging()
    w.store.db.execute("UPDATE merge_turn_ledger SET kind = 'attestation'"
                       "  WHERE turn_id = ? AND evidence_kind = 'currency_confirmed'", (turn,))
    w.merged()
    w.step(lambda: w.land(turn))


# ------------------------------------------------------------------ MTN-22

@scenario
def mtn22_merged_records_reading(w):
    turn = w.unknown()
    w.merged()
    w.step(lambda: w.resolve(turn, POST, "merged", "merged"))


@scenario
def mtn22_already_contained(w):
    turn = w.unknown()
    w.step(lambda: w.resolve(turn, "base-0", "merged", "merged; the base already contained it"))


@scenario
def mtn22_disagrees(w):
    turn = w.unknown()
    w.step(lambda: w.resolve(turn, "base-9", "open", "still open"))
    w.turn(turn)


@scenario
def mtn22_unreadable_open(w):
    turn = w.unknown()
    w.target.forget(REPO, BASE)
    w.step(lambda: w.resolve(turn, "base-0", "open", "still open"))


@scenario
def mtn22_unreadable_merged(w):
    turn = w.unknown()
    w.target.forget(REPO, BASE)
    w.step(lambda: w.resolve(turn, POST, "merged", "merged"))
    w.turn(turn)


@scenario
def mtn22_after_unreadable_return(w):
    turn = w.unknown()
    w.target.forget(REPO, BASE)
    w.step(lambda: w.resolve(turn, "base-0", "closed", "closed unmerged"))
    second = w.held(head="head-c")
    w.step(lambda: w.check(second, head="head-c"))
    w.target.set(REPO, BASE, "base-0")
    w.step(lambda: w.check(second, head="head-c"))


# ------------------------------------------------------------------ MTN-23

@scenario
def mtn23_trial_shape(w):
    first = w.landed()
    w.r3_landing(first)
    second = w.held(head="head-c")
    w.target.set(REPO, BASE, POST)
    w.step(lambda: w.check(second, head="head-c", base=POST))
    w.step(lambda: w.restate(first, observed=POST))
    w.step(lambda: w.check(second, head="head-c", base=POST))
    w.merged("merge-2")
    w.step(lambda: w.land(second, landed="merge-2"))


@scenario
def mtn23_keeps_original(w):
    first = w.landed()
    w.r3_landing(first)
    w.step(lambda: w.restate(first, evidence="git rev-parse main reads base-1"))
    w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id = ? AND"
           " evidence_kind = 'landing_base_restated'", (first,))


@scenario
def mtn23_nothing_to_write(w):
    first = w.landed()
    w.step(lambda: w.restate(first))
    w.r3_landing(first)
    w.step(lambda: w.restate(first))
    w.step(lambda: w.restate(first))


@scenario
def mtn23_moved_twice(w):
    first = w.landed()
    w.target.set(REPO, BASE, "base-2")
    w.step(lambda: w.restate(first))
    w.target.set(REPO, BASE, POST)
    w.step(lambda: w.restate(first))


@scenario
def mtn23_supervisor_and_stranger(w):
    first = w.landed()
    w.r3_landing(first)
    w.reads()
    w.step(lambda: w.restate(first, actor="task-stranger"))
    w.reads()
    w.step(lambda: w.restate(first, actor=w.supervisor.task_id))


@scenario
def mtn23_value_not_read(w):
    first = w.landed()
    w.r3_landing(first)
    w.step(lambda: w.restate(first, observed="base-0"))
    w.turn(first)


@scenario
def mtn23_only_landed(w):
    turn = w.merging()
    w.step(lambda: w.restate(turn))


@scenario
def mtn23_only_latest_landing(w):
    first = w.landed()
    second = w.held(head="head-c")
    w.check(second, head="head-c", base=POST)
    w.merged("base-2")
    w.land(second)
    w.step(lambda: w.restate(first))


@scenario
def mtn23_in_flight(w):
    first = w.landed()
    second = w.held(head="head-c")
    w.check(second, head="head-c", base=POST)
    w.target.set(REPO, BASE, "base-2")
    w.step(lambda: w.restate(first))
    w.turn(first)


@scenario
def mtn23_states_why(w):
    first = w.landed()
    w.step(lambda: w.restate(first, evidence=" "))


# ------------------------------------------------------------------ MTN-24

@scenario
def mtn24_same_instant_order(w):
    first = w.landed()
    second = w.held(head="head-c")
    w.check(second, head="head-c", base=POST)
    w.merged("base-2")
    w.land(second)
    third = w.held(head="head-d")
    w.step(lambda: w.check(third, head="head-d", base="base-2"))
    w.step(lambda: w.restate(first))


@scenario
def mtn24_latest_restated(w):
    first = w.landed()
    second = w.held(head="head-c")
    w.check(second, head="head-c", base=POST)
    w.merged("base-2")
    w.land(second)
    w.r3_landing(second)
    w.step(lambda: w.restate(second))
    w.step(lambda: w.restate(first))


@scenario
def mtn24_returned_does_not_gate(w):
    w.landed()
    second = w.held(head="head-c")
    w.check(second, head="head-c", base=POST)
    w.turns.report_unknown(second, actor=w.alpha.task_id, reason="lost")
    w.step(lambda: w.resolve(second, POST, "open", "still open", actor=w.alpha.task_id))
    w.target.set(REPO, BASE, "base-3")
    third = w.held(head="head-d")
    w.step(lambda: w.check(third, head="head-d", base="base-3"))


@scenario
def mtn24_too_long_key(w):
    first = w.landed()
    w.r3_landing(first)
    for key in ("restate-base:" + "9" * 4301, "restate-base:" + "9" * 18,
                "restate-base:1" + "0" * 18):
        w.legacy_row(first, "attestation", key, "transport_accepted", "old caller row")
    w.step(lambda: w.restate(first))


@scenario
def mtn24_nineteen_digits(w):
    first = w.landed()
    w.legacy_row(first, "attestation", "restate-base:" + str(10 ** 18), "transport_accepted",
                 "old caller row")
    w.legacy_row(first, "transition", "restate-base:" + str(10 ** 18 + 1),
                 "landing_base_restated",
                 json.dumps({"turnId": first, "sequence": 10 ** 18 + 1, "from": "base-x",
                             "to": POST, "evidence": "earlier", "source": "fake"}),
                 states=("landed", "landed"))
    w.r3_landing(first)
    w.step(lambda: w.restate(first))


@scenario
def mtn24_legacy_rows(w):
    first = w.landed()
    w.r3_landing(first)
    w.legacy_row(first, "attestation", "restate-base:1", "transport_accepted", "squatted")
    w.legacy_row(first, "attestation", "mine", "landing_base_restated", "free text")
    w.legacy_row(first, "attestation", "restate-base:7", "landing_base_restated",
                 json.dumps({"turnId": first, "sequence": 7, "from": "x", "to": "y",
                             "evidence": "forged"}))
    w.step(lambda: w.restate(first))


# ------------------------------------------------------------------ merge target (MTG-1..4)

def target_repo():
    path = tempfile.mkdtemp(dir=os.environ["TMPDIR"])
    repo = os.path.join(path, "repo.git")
    subprocess.run(["git", "init", "--bare", repo], check=True, capture_output=True)
    def commit(text):
        payload = ("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n"
                   "author Test <test@example.org> 0 +0000\n"
                   "committer Test <test@example.org> 0 +0000\n\n" + text + "\n")
        return subprocess.run(["git", "-C", repo, "hash-object", "-w", "-t", "commit",
                               "--stdin"], input=payload, text=True, capture_output=True,
                              check=True).stdout.strip()
    first, second = commit("first"), commit("second")
    subprocess.run(["git", "-C", repo, "update-ref", "refs/heads/main", first], check=True)
    return repo, first, second


def target_step(w, reader, repo, branch, paths=()):
    try:
        result = {"ok": reader.tip(repo, branch)}
    except TargetUnreadable as error:
        result = {"refused": {"reason": "merge_target_unreadable", "detail": error.detail}}
    # The checkout path is incidental; compare the complete answer with its path canonicalized.
    raw = json.dumps(result)
    for path in sorted(paths, key=len, reverse=True):
        raw = raw.replace(path, "<repo>")
    w.out.append(json.loads(raw))


@scenario
def mtg1_local(w):
    repo, first, second = target_repo()
    reader = TargetReader()
    target_step(w, reader, repo, "main", (repo,))
    subprocess.run(["git", "-C", repo, "update-ref", "refs/heads/main", second], check=True)
    target_step(w, reader, repo, "main", (repo,))
    work = os.path.join(os.path.dirname(repo), "work")
    subprocess.run(["git", "init", "-b", "main", work], check=True, capture_output=True)
    subprocess.run(["git", "-C", work, "fetch", repo, "main"], check=True,
                   capture_output=True)
    subprocess.run(["git", "-C", work, "update-ref", "refs/heads/main", second], check=True)
    target_step(w, reader, work, "main", (work,))
    linked = os.path.join(os.path.dirname(repo), "linked")
    subprocess.run(["git", "-C", work, "worktree", "add", "-b", "side", linked],
                   check=True, capture_output=True)
    target_step(w, reader, linked, "main", (linked,))


@scenario
def mtg2_refs(w):
    repo, _, second = target_repo()
    reader = TargetReader()
    for branch in ("main~1", "main^", "main@{1}", "-main", "main.lock", "ma*in", " main",
                   "a..b", "x/.hidden", "a//b", "main.", "@", "ma in", "ma\tin", "x/"):
        target_step(w, reader, repo, branch, (repo,))
    for branch in ("topic#42", "50%off", "release/1.2"):
        subprocess.run(["git", "-C", repo, "update-ref", "refs/heads/" + branch, second],
                       check=True)
        target_step(w, reader, repo, branch, (repo,))


@scenario
def mtg3_unreadable(w):
    repo, _, _ = target_repo()
    reader = TargetReader()
    root = os.path.dirname(repo)
    for path in ("relative", os.path.join(root, "missing"), root,
                 os.path.join(repo, "objects")):
        target_step(w, reader, path, "main", (repo, root))
    target_step(w, reader, repo, "nope", (repo, root))
    odd = os.path.join(root, "odd")
    os.mkdir(odd)
    with open(os.path.join(odd, ".git"), "w", encoding="utf-8") as handle:
        handle.write("not a pointer\n")
    target_step(w, reader, odd, "main", (repo, root))
    previous = {key: os.environ.get(key) for key in ("GIT_DIR", "GIT_NAMESPACE")}
    try:
        os.environ.update(GIT_DIR=repo, GIT_NAMESPACE="x")
        target_step(w, reader, repo, "main", (repo, root))
        target_step(w, reader, root, "main", (repo, root))
    finally:
        for key, value in previous.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


@scenario
def mtg4_forge(w):
    from codex_session_relay import forge
    sha = "a" * 40
    valid = {"ref": "refs/heads/dev", "object": {"type": "commit", "sha": sha}}
    answers = [(0, json.dumps(valid), ""), (1, "", "gh: Not Found (HTTP 404)"),
               (1, "", "gh: Git Repository is empty. (HTTP 409)"),
               (0, json.dumps([{"ref": "refs/heads/dev"}]), ""),
               (0, json.dumps(dict(valid, ref="refs/heads/dev-2")), ""),
               (0, json.dumps(dict(valid, object={"type": "tag", "sha": sha})), ""),
               (0, json.dumps(dict(valid, object={"type": "commit", "sha": sha.upper()})), ""),
               FileNotFoundError("fork/exec <missing-gh>: no such file or directory")]
    for answer in answers:
        calls = []
        def run(argv, timeout):
            calls.append(argv)
            if isinstance(answer, Exception):
                raise answer
            return answer
        reader = TargetReader(forge_factory=lambda: forge.Forge(run=run))
        target_step(w, reader, "owner/repo", "dev")
        w.out.append({"ok": [call[1:] for call in calls]})
    for repo, branch in [("owner/repo", "topic#42"), ("-owner/repo", "dev"),
                         ("owner", "dev"), ("owner/repo/extra", "dev"),
                         ("https://x/y", "dev")]:
        calls = []
        target_step(w, TargetReader(forge_factory=lambda: forge.Forge(
            run=lambda argv, timeout: calls.append(argv))), repo, branch)
        w.out.append({"ok": calls})


# ------------------------------------------------------------------ merge target (MTG-5..8)

@scenario
def mtg6_bare_cli(w):
    from codex_session_relay import cli
    from tests.test_merge_target import Repository
    from unittest.mock import patch
    root = tempfile.mkdtemp(dir=os.environ["TMPDIR"])
    path = os.path.join(root, "R-A.git")
    repo = Repository(path)
    base = repo.commit("skeleton")
    a1 = repo.commit("A1", base)
    a2 = repo.commit("A2", a1)
    repo.point("main", base)
    state = os.path.join(root, "state")

    def call(*argv):
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = cli.main(["--state", state, *argv])
        w.out.append({"ok": {"exit": code, "payload": json.loads(buffer.getvalue())}})
        return json.loads(buffer.getvalue())

    def claim(head):
        result = call("merge-turn-request", "--repository", path, "--base-ref", "main",
                      "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a",
                      "--head", head, "--ready")
        call("merge-turn-acknowledge", "--turn", result["turnId"], "--actor", "task-alpha",
             "--grant", result["grant"]["grantId"], "--evidence", "read the grant")
        return result["turnId"]

    def check(turn, head, base_sha):
        checks = json.dumps([{"runId": "run-" + head[:7], "name": "required",
                              "headSha": head, "conclusion": "success", "attempt": 1}])
        return ("merge-turn-check", "--turn", turn, "--actor", "task-alpha",
                "--head-sha", head, "--base-sha", base_sha, "--checks", checks,
                "--review", json.dumps(GREEN), "--required", "required")

    # Each invocation opens a new Services, so patch its clock construction throughout.
    with patch.object(cli, "SystemClock", FakeClock):
        call("linkage-bind", "--role", "parent", "--scope", "PRJ-A", "--task", "task-alpha",
             "--host", "host-a")
        first = claim(a1)
        call(*check(first, a1, base))
        repo.point("main", a1)
        call("merge-turn-land", "--turn", first, "--actor", "task-alpha",
             "--landed-sha", a1, "--observed-base-sha", base,
             "--evidence", "fast-forwarded main to A1")
        call("merge-turn-land", "--turn", first, "--actor", "task-alpha",
             "--landed-sha", a1, "--evidence", "fast-forwarded main to A1")
        import sqlite3
        with sqlite3.connect(os.path.join(state, "relay.sqlite3")) as db:
            db.execute("UPDATE merge_turns SET observed_base_sha=? WHERE turn_id=?", (base, first))
        second = claim(a2)
        call(*check(second, a2, a1))
        call("merge-turn-restate-base", "--turn", first, "--actor", "task-alpha",
             "--observed-base-sha", a1, "--evidence", "main after A1's fast-forward")
        call(*check(second, a2, a1))
        repo.point("main", a2)
        call("merge-turn-land", "--turn", second, "--actor", "task-alpha",
             "--landed-sha", a2, "--observed-base-sha", a2,
             "--evidence", "fast-forwarded main to A2")
    with sqlite3.connect(os.path.join(state, "relay.sqlite3")) as db:
        db.row_factory = sqlite3.Row
        for sql in ("SELECT * FROM merge_turns ORDER BY candidate_head",
                    "SELECT * FROM merge_turn_checks ORDER BY check_id",
                    "SELECT * FROM merge_turn_ledger ORDER BY turn_id,idempotency_key"):
            w.out.append({"ok": [dict(row) for row in db.execute(sql)]})
    normalize_cli_steps(w.out, path)


def normalize_cli_steps(steps, repository):
    # IDs derive from the absolute temporary repository path. Replace every occurrence,
    # including nested ledger evidence JSON, in first-seen order, preserving the full shape.
    names = {}
    for i, step in enumerate(steps):
        text = json.dumps(step, sort_keys=True, ensure_ascii=True).replace(repository, "<repo>")
        def replace(match):
            key = match.group()
            if key not in names:
                names[key] = "<" + key[:3] + "-" + str(sum(v.startswith("<" + key[:3] + "-")
                                                      for v in names.values()) + 1) + ">"
            return names[key]
        text = re.sub(r"(?:tgt|mtn|mtg|mte|chk)-[0-9a-f]{32}", replace, text)
        steps[i] = json.loads(text)
        def stable(value):
            if isinstance(value, dict):
                for key, item in value.items():
                    stable(item)
                    if key == "ledger" and isinstance(item, list):
                        item.sort(key=lambda row: row["idempotencyKey"])
            elif isinstance(value, list):
                for item in value:
                    stable(item)
        stable(steps[i])
    # Entry identifiers are path-derived; their lexical ordering changes with the temp path.
    # Assign stable aliases by the ledger row's turn and idempotency key, not incidental order.
    entries = {}
    def collect(value):
        if isinstance(value, dict):
            entry = value.get("entryId", value.get("entry_id"))
            key = value.get("idempotencyKey", value.get("idempotency_key"))
            if entry and key:
                entries[entry] = (value.get("turnId", value.get("turn_id", "")), key)
            for child in value.values():
                collect(child)
        elif isinstance(value, list):
            for child in value:
                collect(child)
    collect(steps)
    aliases = {entry: "<entry-" + str(i) + ">" for i, (entry, _) in
               enumerate(sorted(entries.items(), key=lambda pair: pair[1]), 1)}
    for i, step in enumerate(steps):
        steps[i] = json.loads(re.sub(r"<mte-\d+>", lambda match: aliases.get(
            match.group(), match.group()), json.dumps(step)))
    for step in steps:
        rows = step.get("ok")
        if isinstance(rows, list) and rows and isinstance(rows[0], dict):
            if "check_id" in rows[0]:
                rows.sort(key=lambda row: row["turn_id"])
            elif "entry_id" in rows[0]:
                rows.sort(key=lambda row: (row["turn_id"], row["idempotency_key"]))


@scenario
def mtg5_commit(w):
    sha = "abcdef1234" + "0" * 30
    for a, b in ((sha.upper(), sha), ("abcdef1", sha), (sha, "abcdef1"),
                 ("base-0", "base-00"), ("base-0", "base-0"), ("", "")):
        w.step(lambda: same_commit(a, b))


@scenario
def mtg6_trial(w):
    first = w.landed()
    w.r3_landing(first)
    second = w.held(head="head-c")
    w.merged(POST)
    w.step(lambda: w.check(second, head="head-c", base=POST))
    w.step(lambda: w.restate(first, observed=POST))
    w.step(lambda: w.check(second, head="head-c", base=POST))
    w.merged("merge-2")
    w.step(lambda: w.land(second, landed="merge-2"))
    w.turn(second)


@scenario
def mtg7_help(w):
    from codex_session_relay import cli
    from argparse import _HelpAction
    parser = cli.build_parser()
    subparser = next(a for a in parser._actions if getattr(a, "choices", None))
    command = subparser.choices["merge-turn-restate-base"]
    w.step(lambda: {"helpExitsZero": any(isinstance(a, _HelpAction) for a in command._actions),
                    "hasObservedBaseSha": "--observed-base-sha" in command.format_help()})


@scenario
def mtg8_review(w):
    turn = w.held()
    for review in (dict(GREEN, threadsSeen=1), dict(GREEN, unresolved=[])):
        w.checks_rows(turn)
        w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id = ?", (turn,))
        w.step(lambda: w.begin(turn, review=review))
        w.checks_rows(turn)
        w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id = ?", (turn,))
    partial = dict(GREEN)
    del partial["threadsSeen"]
    w.step(lambda: w.begin(turn, review=partial))
    w.step(lambda: w.begin(turn))


# ------------------------------------------------------------------ merge turn wake (MTW-1..10)

class WakeWorld(World):
    def __init__(self):
        super().__init__()
        self.registry = Registry(self.store, self.clock)
        self.intake = ReceiptIntake(self.store, self.registry, self.clock)
        self.delivery = DeliveryService(self.store, self.registry, self.intake, self.clock)
        self.adapter = FakeHostAdapter(self.clock)
        self.adapter.add_thread(self.alpha.task_id)
        self.adapter.add_thread("task-child")
        record_settings(self.store, self.clock, self.alpha.task_id,
                        task_settings("/alpha"), source="creation_result")
        policy_path = os.path.join(os.environ["TMPDIR"], "wake-policy.json")
        with open(policy_path, "w", encoding="utf-8") as handle:
            json.dump({"roles": {"parent": {"model": "anthropic/claude-opus-5",
                                             "reasoningEffort": "xhigh"}}}, handle)
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = policy_path
        rolepolicy.reset()
        self.turns = MergeTurn(self.store, self.clock, self.linkage, delivery=self.delivery,
                               target_reader=self.target)
        self.store.db.execute(
            "INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,"
            "parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,"
            "allowed_recipients,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
            ("rel-a", "ISS-1", "active", self.alpha.task_id, self.alpha.host_id,
             "task-child", "host-child", 3, "[]", '["task-alpha"]',
             self.clock.iso(), self.clock.iso()))
        self.store.db.execute(
            "INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,"
            "anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES (?,?,?,?,?,?,?,?)",
            ("rel-a", 3, "dispatch-1", "bound", "turn-dispatch-1", "initial_assignment",
             self.clock.iso(), self.clock.iso()))

    def notices(self):
        self.rows("SELECT d.event_id, d.recipient_task_id, d.state, e.receipt FROM deliveries d"
                  " JOIN events e ON e.event_id=d.event_id WHERE d.kind='merge_turn_grant'"
                  " ORDER BY d.created_at,d.event_id")

    def promote(self):
        rival = self.claim(self.beta, PROJECT_B, "head-b")
        waiter = self.turns.request(repository=REPO, base_ref=BASE, project_key=PROJECT_A,
                                    holder=self.alpha, candidate_head="head-a", ready=True,
                                    relationship_id="rel-a")
        self.turns.release(rival["turnId"], actor=self.beta.task_id,
                           disposition="returned", reason="done")
        return waiter["turnId"]


@scenario
def mtw1_two_grants(w):
    first = w.promote()
    old = w.turns.turn(first)["grant"]
    w.step(lambda: w.turns.acknowledge_grant(first, actor=w.alpha.task_id,
                                              grant=old["grantId"], evidence="read it"))
    w.step(lambda: w.turns.release(first, actor=w.alpha.task_id,
                                    disposition="returned", reason="handing it back"))
    rival = w.claim(w.beta, PROJECT_B, "head-b2")
    waiter = w.turns.request(repository=REPO, base_ref=BASE, project_key=PROJECT_A,
                             holder=w.alpha, candidate_head="head-a2", ready=True,
                             relationship_id="rel-a")
    w.step(lambda: w.turns.release(rival["turnId"], actor=w.beta.task_id,
                                    disposition="returned", reason="done"))
    w.turn(waiter["turnId"])
    w.notices()


@scenario
def mtw3_second_dispatch(w):
    first = w.promote()
    old = w.turns.turn(first)["grant"]
    w.turns.acknowledge_grant(first, actor=w.alpha.task_id,
                              grant=old["grantId"], evidence="read it")
    w.turns.release(first, actor=w.alpha.task_id, disposition="returned",
                    reason="handing it back")
    rival = w.claim(w.beta, PROJECT_B, "head-b2")
    waiter = w.turns.request(repository=REPO, base_ref=BASE, project_key=PROJECT_A,
                             holder=w.alpha, candidate_head="head-a2", ready=True,
                             relationship_id="rel-a")
    w.turns.release(rival["turnId"], actor=w.beta.task_id,
                    disposition="returned", reason="done")
    new = w.turns.turn(waiter["turnId"])["grant"]
    w.step(lambda: w.delivery.attempt(new["wake"]["eventId"], w.adapter))
    w.step(lambda: len(w.adapter.threads[w.alpha.task_id].turns))
    text = w.adapter.threads[w.alpha.task_id].items[-1][1]
    w.step(lambda: {"newGrant": new["grantId"] in text,
                    "notOldGrant": old["grantId"] not in text})


@scenario
def mtw3_dispatch(w):
    turn = w.promote()
    grant = w.turns.turn(turn)["grant"]
    event = grant["wake"]["eventId"]
    w.step(lambda: w.delivery.attempt(event, w.adapter))
    w.step(lambda: len(w.adapter.threads[w.alpha.task_id].turns))
    text = w.adapter.threads[w.alpha.task_id].items[-1][1]
    w.step(lambda: {"grant": grant["grantId"] in text,
                    "granted": "merge turn granted" in text,
                    "repository": REPO in text,
                    "acknowledge": "merge-turn-acknowledge" in text,
                    "noAckProof": "ack-proof" not in text})
    w.step(lambda: next(one for one in w.delivery.snapshot()["deliveries"]
                        if one["eventId"] == event)["phase"])
    w.step(lambda: w.turns.acknowledge_grant(turn, actor=w.alpha.task_id,
                                              grant=grant["grantId"], evidence="read it"))
    w.step(lambda: next(one for one in w.delivery.snapshot()["deliveries"]
                        if one["eventId"] == event)["phase"])
    w.step(lambda: w.delivery.attempt(event, w.adapter))
    w.step(lambda: len(w.adapter.sends))


@scenario
def mtw6_unaddressable(w):
    rival = w.claim(w.beta, PROJECT_B, "head-b")
    waiter = w.claim(w.alpha, PROJECT_A, "head-a")
    w.step(lambda: w.turns.release(rival["turnId"], actor=w.beta.task_id,
                                    disposition="returned", reason="done"))
    w.turn(waiter["turnId"])
    w.notices()


@scenario
def mtw6_other_parent(w):
    held = w.claim(w.alpha, PROJECT_A, "head-a")
    waiter = w.turns.request(repository=REPO, base_ref=BASE, project_key=PROJECT_B,
                             holder=w.beta, candidate_head="head-b", ready=True,
                             relationship_id="rel-a")
    w.step(lambda: w.turns.release(held["turnId"], actor=w.alpha.task_id,
                                    disposition="returned", reason="done"))
    w.turn(waiter["turnId"])
    w.notices()


@scenario
def mtw6_recipient_and_scope(w):
    w.store.db.execute("UPDATE relationships SET allowed_recipients=? WHERE relationship_id=?",
                       ('["task-child"]', "rel-a"))
    turn = w.promote()
    w.turn(turn)
    w.notices()
    w.store.db.execute("UPDATE relationships SET allowed_recipients=? WHERE relationship_id=?",
                       ('["task-alpha"]', "rel-a"))
    w.store.db.execute("INSERT INTO relationship_scope (relationship_id,project_key,recorded_at)"
                       " VALUES (?,?,?)", ("rel-a", "PRJ-C", w.clock.iso()))
    held = w.turns.request(repository=REPO, base_ref="release", project_key=PROJECT_B,
                           holder=w.beta, candidate_head="head-b2", ready=True)
    waiter = w.turns.request(repository=REPO, base_ref="release", project_key=PROJECT_A,
                             holder=w.alpha, candidate_head="head-a2", ready=True,
                             relationship_id="rel-a")
    w.turns.release(held["turnId"], actor=w.beta.task_id,
                    disposition="returned", reason="done")
    w.turn(waiter["turnId"])
    w.notices()


@scenario
def mtw7_optional(w):
    plain = MergeTurn(w.store, w.clock, w.linkage)
    held = plain.request(repository=REPO, base_ref="release", project_key=PROJECT_B,
                         holder=w.beta, candidate_head="head-b", ready=True,
                         relationship_id="rel-a")
    waiter = plain.request(repository=REPO, base_ref="release", project_key=PROJECT_A,
                           holder=w.alpha, candidate_head="head-a", ready=True,
                           relationship_id="rel-a")
    w.step(lambda: plain.release(held["turnId"], actor=w.beta.task_id,
                                 disposition="returned", reason="done"))
    w.step(lambda: plain.turn(waiter["turnId"]))
    w.notices()


@scenario
def mtw9_notice(w):
    turn = w.promote()
    grant = w.turns.turn(turn)["grant"]
    event = grant["wake"]["eventId"]
    message = w.delivery.preview_message(event)
    w.step(lambda: {"notRecorded": "requiredDeclared: not recorded (" in message,
                    "mergeEvidence": "merge-evidence --repository " + REPO in message,
                    "requiredPlaceholder": " --required=<" in message})
    w.step(lambda: w.delivery.attempt(event, w.adapter))
    sent = w.adapter.threads[w.alpha.task_id].items[-1][1]
    w.step(lambda: {"notRecorded": "requiredDeclared: not recorded (" in sent,
                    "mergeEvidence": "merge-evidence --repository " + REPO in sent})


@scenario
def mtw9_reading(w):
    from codex_session_relay import report
    def read():
        return report.required_for_candidate(w.store, relationship_id="rel-a",
                                             repository=REPO, base_ref=BASE,
                                             head_sha="head-a")
    w.step(read)
    def entry(event, head, required):
        w.store.db.execute(
            "INSERT INTO work_reports (event_id,submission_no,relationship_id,"
            "execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,"
            "cxc_reason,contract_version,summary,next_action,recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (event, 1, "rel-a", 3, "rev", REPO, BASE, head, "done", "r", "1", "s",
             "n", w.clock.iso()))
        if required is not None:
            w.store.db.execute(
                "INSERT INTO work_report_handoffs (event_id,submission_no,is_draft,"
                "required_declared,checks,review_coverage,thread_dispositions,recorded_at)"
                " VALUES (?,?,?,?,?,?,?,?)",
                (event, 1, 0, json.dumps(required), "[]", "{}", "[]", w.clock.iso()))
    entry("event-a", "head-a", ["dev-gate"])
    w.step(read)
    entry("event-b", "head-a", ["other-gate"])
    w.step(read)


@scenario
def mtw10_gate(w):
    rid = "rel-a"
    def report(event, submission, head):
        w.store.db.execute(
            "INSERT INTO work_reports (event_id,submission_no,relationship_id,"
            "execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,"
            "cxc_reason,contract_version,summary,next_action,recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (event, submission, rid, 3, "rev", REPO, BASE, head, "done", "r", "1", "s",
             "n", w.clock.iso()))
    report("event-a", 1, "head-old")
    report("event-a", 2, "head-a")
    report("event-b", 1, "head-b")
    turn = w.turns.request(repository=REPO, base_ref=BASE, project_key=PROJECT_A,
                           holder=w.alpha, candidate_head="head-a", ready=True,
                           relationship_id=rid)["turnId"]
    w.answer(turn, w.alpha.task_id)
    w.step(lambda: w.check(turn, head="head-a"))
    w.turn(turn)
    w.store.db.execute("DELETE FROM work_reports WHERE event_id='event-b'")
    w.step(lambda: w.check(turn, head="head-old"))
    report("event-a", 3, None)
    w.turn(turn)
    w.turns.release(turn, actor=w.alpha.task_id, disposition="returned",
                    reason="try historical head")
    historical = w.turns.request(repository=REPO, base_ref=BASE, project_key=PROJECT_A,
                                 holder=w.alpha, candidate_head="head-old", ready=True,
                                 relationship_id=rid)["turnId"]
    w.answer(historical, w.alpha.task_id)
    w.step(lambda: w.check(historical, head="head-old"))
    w.turn(historical)


@scenario
def mtw5_correction(w):
    w.store.db.execute("UPDATE relationships SET allowed_recipients=? WHERE relationship_id=?",
                       ('["task-alpha","task-child"]', "rel-a"))
    record_settings(w.store, w.clock, "task-child", task_settings("/child"),
                    source="creation_result")
    from codex_session_relay import NO_DELIVERABLE
    from codex_session_relay.delivery import REVISION
    event = "f" * 32
    now = w.clock.iso()
    receipt = json.dumps({"eventId": event, "relationshipId": "rel-a",
                          "executionGeneration": 3, "kind": REVISION, "criteria": []})
    w.store.db.execute(
        "INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,"
        "outcome,producer,attempt,turn_thread_id,turn_id,turn_status,receipt,stage,"
        "first_seen_at,last_seen_at,observation_count)"
        " VALUES (?,?,?,?,?,?,NULL,?,?,?,?,'final',?,?,1)",
        (event, "rel-a", 3, NO_DELIVERABLE, REVISION, "relay", w.alpha.task_id,
         "turn-verdict-1", "completed", receipt, now, now))
    with w.store.transaction() as db:
        w.delivery.enqueue_in(db, event, relationship_id="rel-a", kind=REVISION,
                              recipient_task_id="task-child")
    w.promote()
    with w.store.transaction() as db:
        w.step(lambda: w.delivery._supersession_reason(db, event))
    w.rows("SELECT reason FROM delivery_supersession WHERE event_id=?", (event,))
    w.step(lambda: w.delivery.attempt(event, w.adapter))
    w.step(lambda: len(w.adapter.threads["task-child"].turns))


@scenario
def mtw8_states(w):
    def item(event):
        return next(one for one in w.delivery.snapshot()["deliveries"]
                    if one["eventId"] == event)
    turn = w.promote()
    grant = w.turns.turn(turn)["grant"]
    event = grant["wake"]["eventId"]
    w.step(lambda: w.delivery.attempt(event, w.adapter))
    w.step(lambda: item(event))
    w.turns.acknowledge_grant(turn, actor=w.alpha.task_id,
                              grant=grant["grantId"], evidence="read it")
    w.step(lambda: item(event))
    w.turns.release(turn, actor=w.alpha.task_id, disposition="returned", reason="done")
    w.step(lambda: item(event))


@scenario
def mtw8_damage(w):
    def item(event):
        return next(one for one in w.delivery.snapshot()["deliveries"]
                    if one["eventId"] == event)
    turn = w.promote()
    event = w.turns.turn(turn)["grant"]["wake"]["eventId"]
    w.delivery.attempt(event, w.adapter)
    w.store.db.execute("DELETE FROM merge_turns WHERE turn_id=?", (turn,))
    w.step(lambda: item(event))
    w.store.db.execute("UPDATE events SET receipt=? WHERE event_id=?", ("not a grant", event))
    w.step(lambda: item(event))


@scenario
def mtw8_currency(w):
    from codex_session_relay.mergeturn import grant_supersession_in
    turn = w.promote()
    grant = w.turns.turn(turn)["grant"]
    event = grant["wake"]["eventId"]
    w.step(lambda: grant_supersession_in(w.store.db, turn, grant["grantId"]))
    w.step(lambda: next(one for one in w.delivery.snapshot()["deliveries"]
                        if one["eventId"] == event))
    w.turns.declare_ready(turn, actor=w.alpha.task_id, ready=True, candidate_head="head-a2")
    w.step(lambda: grant_supersession_in(w.store.db, turn, grant["grantId"]))
    w.step(lambda: next(one for one in w.delivery.snapshot()["deliveries"]
                        if one["eventId"] == event))


@scenario
def mtw4_restated(w):
    turn = w.promote()
    grant = w.turns.turn(turn)["grant"]
    w.turns.declare_ready(turn, actor=w.alpha.task_id, ready=True, candidate_head="head-a2")
    w.step(lambda: w.delivery.attempt(grant["wake"]["eventId"], w.adapter))
    w.step(lambda: len(w.adapter.sends))


@scenario
def mtw4_generation(w):
    turn = w.promote()
    grant = w.turns.turn(turn)["grant"]
    w.store.db.execute("UPDATE relationships SET execution_generation=4 WHERE relationship_id='rel-a'")
    w.store.db.execute("INSERT INTO generations (relationship_id,execution_generation,"
                       "dispatch_request_id,anchor_state,reason,opened_at) VALUES (?,?,?,?,?,?)",
                       ("rel-a", 4, "revision-1", "pending", "needs_changes_revision",
                        w.clock.iso()))
    w.step(lambda: w.delivery.attempt(grant["wake"]["eventId"], w.adapter))
    w.step(lambda: next(one for one in w.delivery.snapshot()["deliveries"]
                        if one["eventId"] == grant["wake"]["eventId"])["phase"])


@scenario
def mtw4_answered(w):
    turn = w.promote()
    grant = w.turns.turn(turn)["grant"]
    w.turns.acknowledge_grant(turn, actor=w.alpha.task_id,
                              grant=grant["grantId"], evidence="read it")
    w.step(lambda: w.delivery.attempt(grant["wake"]["eventId"], w.adapter))
    w.step(lambda: len(w.adapter.sends))


@scenario
def mtw1_promotion(w):
    turn = w.promote()
    w.turn(turn)
    w.notices()
    grant = w.turns.turn(turn)["grant"]
    w.step(lambda: w.turns.acknowledge_grant(turn, actor=w.alpha.task_id,
                                              grant=grant["grantId"], evidence="read it"))
    w.step(lambda: w.turns.release(turn, actor=w.alpha.task_id,
                                    disposition="returned", reason="handing it back"))
    w.notices()


@scenario
def mtw2_unknown(w):
    rival = w.claim(w.beta, PROJECT_B, "head-b")
    w.answer(rival["turnId"], w.beta.task_id)
    w.begin(rival["turnId"], actor=w.beta.task_id, head_sha="head-b",
            checks=run_checks("head-b"))
    waiter = w.turns.request(repository=REPO, base_ref=BASE, project_key=PROJECT_A,
                             holder=w.alpha, candidate_head="head-a", ready=True,
                             relationship_id="rel-a")
    w.step(lambda: w.turns.report_unknown(rival["turnId"], actor=w.beta.task_id,
                                            reason="the host went away mid-merge"))
    w.clock.advance(86400 * 7)
    w.turn(waiter["turnId"])
    w.notices()


@scenario
def mtw2_conditions(w):
    held = w.claim(w.beta, PROJECT_B, "head-b")
    waiter = w.turns.request(repository=REPO, base_ref=BASE, project_key=PROJECT_A,
                             holder=w.alpha, candidate_head="head-a", ready=False,
                             relationship_id="rel-a")
    w.notices()
    w.step(lambda: w.turns.release(held["turnId"], actor=w.beta.task_id,
                                    disposition="returned", reason="done"))
    w.turn(waiter["turnId"])
    w.notices()
    w.step(lambda: w.turns.declare_ready(waiter["turnId"], actor=w.alpha.task_id,
                                          ready=True))
    w.notices()


# Each PR132-RB2 status row is independently reproducible, including raw delivery state.
MTW8_CASES = ("delivered", "regranted", "returned", "landed", "queued_regranted",
              "uncertain_regranted", "absent", "unreadable", "answered_queued",
              "uncertain_answered", "suppressed_answered", "suppressed_regranted")


def mtw8_case(w, case):
    from codex_session_relay.transport import HELD_UNCERTAIN
    turn = w.promote()
    grant = w.turns.turn(turn)["grant"]
    event = grant["wake"]["eventId"]
    if case in ("delivered", "regranted", "returned", "landed", "absent", "unreadable"):
        w.step(lambda: w.delivery.attempt(event, w.adapter))
    if case in ("uncertain_regranted", "uncertain_answered"):
        w.store.db.execute("UPDATE deliveries SET state=? WHERE event_id=?",
                           (HELD_UNCERTAIN, event))
    if case in ("regranted", "queued_regranted", "uncertain_regranted",
                "suppressed_regranted"):
        w.turns.declare_ready(turn, actor=w.alpha.task_id, ready=True,
                              candidate_head="head-a2")
    if case in ("landed", "answered_queued", "uncertain_answered", "suppressed_answered"):
        w.turns.acknowledge_grant(turn, actor=w.alpha.task_id,
                                  grant=grant["grantId"], evidence="read it")
    if case == "landed":
        w.check(turn)
        w.merged()
        w.land(turn, observed=POST)
    if case == "returned":
        w.turns.release(turn, actor=w.alpha.task_id, disposition="returned",
                        reason="cannot land it")
    if case == "absent":
        w.store.db.execute("DELETE FROM merge_turns WHERE turn_id=?", (turn,))
    if case == "unreadable":
        w.store.db.execute("UPDATE events SET receipt=? WHERE event_id=?",
                           ("not a grant", event))
    if case in ("suppressed_answered", "suppressed_regranted"):
        w.step(lambda: w.delivery.attempt(event, w.adapter))
    w.step(lambda: next(item for item in w.delivery.snapshot()["deliveries"]
                        if item["eventId"] == event))


for _case in MTW8_CASES:
    SCENARIOS["mtw8_row_" + _case] = lambda w, case=_case: mtw8_case(w, case)


# The fifteen notice rows each start with a fresh assignment and candidate reading.
MTW9_CASES = ("red_gate", "preview", "quoted", "resubmitted", "different_events",
              "headless_latest", "none_required", "no_report", "no_handoff",
              "other_head", "other_repo", "other_base", "no_base",
              "mixed_handoff", "disagree")


def mtw9_case(w, case):
    from codex_session_relay import report
    def add(event, submission=1, head="head-a", required=("dev-gate",),
            repository=REPO, base=BASE):
        w.store.db.execute(
            "INSERT INTO work_reports (event_id,submission_no,relationship_id,"
            "execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,"
            "cxc_reason,contract_version,summary,next_action,recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (event, submission, "rel-a", 3, "rev", repository, base, head, "done", "r",
             "1", "s", "n", w.clock.iso()))
        if required is not None:
            w.store.db.execute(
                "INSERT INTO work_report_handoffs (event_id,submission_no,is_draft,"
                "required_declared,checks,review_coverage,thread_dispositions,recorded_at)"
                " VALUES (?,?,?,?,?,?,?,?)",
                (event, submission, 0, json.dumps(list(required)), "[]", "{}", "[]",
                 w.clock.iso()))
    if case in ("red_gate", "preview"):
        add("event-a")
    elif case == "quoted":
        add("event-a", required=("--check", "build linux", "dev-gate", "lint'; echo x"))
    elif case == "resubmitted":
        add("event-a", head="head-old", required=("old-gate",))
        add("event-a", 2)
    elif case == "different_events":
        add("event-a")
        add("event-a", 2, required=("optional-lint",))
        add("event-b")
    elif case == "headless_latest":
        add("event-a")
        add("event-a", 2, head=None, required=None)
        add("event-b", required=("optional-lint",))
    elif case == "none_required":
        add("event-a", required=())
    elif case == "no_handoff":
        add("event-a", required=None)
    elif case == "other_head":
        add("event-a", head="head-old")
    elif case == "other_repo":
        add("event-a", repository="owner/other")
    elif case == "other_base":
        add("event-a", base="main")
    elif case == "no_base":
        add("event-a", base=None, required=())
    elif case == "mixed_handoff":
        add("event-a", required=())
        add("event-b", required=None)
    elif case == "disagree":
        add("event-a")
        add("event-b", required=("other-gate",))
    turn = w.promote()
    grant = w.turns.turn(turn)["grant"]
    event = grant["wake"]["eventId"]
    w.step(lambda: report.required_for_candidate(w.store, relationship_id="rel-a",
                                                  repository=REPO, base_ref=BASE,
                                                  head_sha="head-a"))
    text = w.delivery.preview_message(event)
    w.step(lambda: text)
    w.step(lambda: w.delivery.attempt(event, w.adapter))
    w.step(lambda: w.adapter.threads[w.alpha.task_id].items[-1][1])
    if case == "red_gate":
        w.turns.acknowledge_grant(turn, actor=w.alpha.task_id, grant=grant["grantId"],
                                  evidence="read it")
        red = run_checks("head-a", conclusion="failure") + run_checks(
            "head-a", name="optional-lint", run="run-lint")
        w.step(lambda: w.begin(turn, checks=red, required=["dev-gate"]))
        w.step(lambda: w.begin(turn))


for _case in MTW9_CASES:
    SCENARIOS["mtw9_row_" + _case] = lambda w, case=_case: mtw9_case(w, case)


# ------------------------------------------------------------------ withdraw (CCL-1 command)

@scenario
def withdraw_waiting(w):
    w.claim(w.alpha, PROJECT_A, "head-a")
    waiting = w.claim(w.beta, PROJECT_B, "head-b")["turnId"]
    w.step(lambda: w.turns.withdraw(waiting, actor=w.alpha.task_id))
    w.step(lambda: w.turns.withdraw(waiting, actor=w.beta.task_id))
    w.step(lambda: w.turns.withdraw(waiting, actor=w.beta.task_id))
    w.step(lambda: w.turns.withdraw("mtn-missing", actor=w.beta.task_id))


def main():
    out = {}
    for name, fn in SCENARIOS.items():
        w = WakeWorld() if name.startswith("mtw") else World()
        fn(w)
        out[name] = w.out
        w.store.close()
    # Preserve identity relationships, not the derived digest, in recorded answers.
    for scenario, steps in out.items():
        names = {}
        def replace(match):
            value = match.group()
            if value not in names:
                names[value] = f"<target-key-{len(names) + 1}>"
            return names[value]
        out[scenario] = json.loads(re.sub(r"tgt-[0-9a-f]{32}", replace,
                                          json.dumps(steps, sort_keys=True, ensure_ascii=True)))
    json.dump(out, sys.stdout, indent=1, sort_keys=True, ensure_ascii=True)
    print()


if __name__ == "__main__":
    main()
