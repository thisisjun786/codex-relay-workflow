"""Record the Python capacity module's caller-visible answers for internal/relay/capacity tests.

Run from the repository root with HOME/XDG_*/CODEX_HOME/TMPDIR in a scratch directory:
  uv run --no-sync python internal/relay/capacity/testdata/gen_capacity.py \
      > internal/relay/capacity/testdata/python_capacity.json
Every scenario is test_capacity.py's setUp (FakeClock(1_700_000_000), two parents under one
supervisor) followed by the calls of the property it covers. Each step is
{"ok": <json.dumps(value, indent=2)>} or {"refused": {"reason", "detail"}}; the Go tests emit
their own answers with contract.Emit and compare the text byte for byte.
"""
import json
import os
import sys
import tempfile

from codex_session_relay.capacity import Capacity
from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import RelayError
from codex_session_relay.linkage import PARENT, Linkage
from codex_session_relay.models import Endpoint
from codex_session_relay.store import Store

PROJECT_A, PROJECT_B, ASSIGNMENT = "PRJ-A", "PRJ-B", "assignment"
ALPHA = Endpoint("task-alpha", "host-a", cwd="/alpha")
BETA = Endpoint("task-beta", "host-b", cwd="/beta")
SUPERVISOR = Endpoint("task-supervisor", "host-s", cwd="/sup")
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
        self.capacity = Capacity(self.store, self.clock, self.linkage)
        self.linkage.bind_scope(role=PARENT, scope_key=PROJECT_A, endpoint=ALPHA)
        self.linkage.bind_scope(role=PARENT, scope_key=PROJECT_B, endpoint=BETA)
        for project, parent in ((PROJECT_A, ALPHA), (PROJECT_B, BETA)):
            self.linkage.register_supervision(
                initiative_key="INIT-1", project_key=project, supervisor=SUPERVISOR,
                parent=parent)
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

    def take(self, subject, endpoint=ALPHA, project=PROJECT_A):
        return self.step(lambda: self.capacity.reserve(
            subject_kind=ASSIGNMENT, subject_key=subject, parent_task_id=endpoint.task_id,
            project_key=project, reserved_by=endpoint.task_id))

    def give_back(self, subject, reason="completed", endpoint=ALPHA, tenure=None):
        return self.step(lambda: self.capacity.release(
            subject_kind=ASSIGNMENT, subject_key=subject, released_by=endpoint.task_id,
            reason=reason, tenure=tenure))

    def ceiling(self, dimension, amount, scope=PROJECT_A, kind="project", unit="runs",
                enforce=True, by=None):
        owner = by or (SUPERVISOR.task_id if kind != "project" else ALPHA.task_id)
        return self.step(lambda: self.capacity.declare_limit(
            scope_kind=kind, scope_key=scope, dimension=dimension, unit=unit,
            ceiling=amount, declared_by=owner, source="operator", enforce=enforce))

    def observe(self, dimension, observed, kind="project", scope=PROJECT_A, by=ALPHA.task_id,
                method="probe"):
        return self.step(lambda: self.capacity.observe(
            scope_kind=kind, scope_key=scope, dimension=dimension, observed=observed,
            observed_by=by, method=method))

    def report(self):
        return self.step(lambda: self.capacity.report())

    def headroom(self, kind="project", scope=PROJECT_A):
        return self.step(lambda: self.capacity.headroom(kind, scope))

    def rows(self, sql, params=()):
        return self.step(lambda: [dict(r) for r in self.store.all(sql, params)])

    def conflicts(self, subject):
        return self.step(lambda: self.capacity.conflicts.all("execution_subject", subject))


SLOTS = "SELECT tenure, state, release_reason FROM execution_slots WHERE subject_key = ? ORDER BY tenure"


@scenario
def cap1(e):
    e.take("REL-1"), e.take("REL-1"), e.report()
    e.give_back("REL-1"), e.give_back("REL-1"), e.report()
    e.take("REL-2")
    for _ in range(3):
        e.give_back("REL-2", "completed")
    e.rows("SELECT * FROM execution_slots WHERE subject_key = ? AND state = 'released'", ("REL-2",))
    e.rows("SELECT kind, subject, detail FROM journal WHERE kind LIKE 'slot_%' ORDER BY seq")


@scenario
def cap2(e):
    e.take("REL-1"), e.give_back("REL-1", "completed"), e.give_back("REL-1", "failed")
    e.step(lambda: e.capacity.slot(ASSIGNMENT, "REL-1"))
    e.conflicts("REL-1")
    e.give_back("REL-NEVER")
    e.take("REL-2"), e.take("REL-2", endpoint=BETA, project=PROJECT_B), e.conflicts("REL-2")
    e.take("REL-1"), e.give_back("REL-1"), e.report()
    e.give_back("REL-1", tenure=1), e.report()
    e.give_back("REL-1", tenure=2), e.report()
    e.give_back("REL-1", tenure=7)


@scenario
def cap3(e):
    e.take("REL-1"), e.give_back("REL-1"), e.take("REL-1")
    e.rows(SLOTS, ("REL-1",)), e.report()


@scenario
def cap4(e):
    e.take("REL-1"), e.give_back("REL-1", endpoint=BETA), e.report()
    e.take("REL-2", endpoint=BETA, project=PROJECT_A), e.conflicts("REL-2")
    e.take("REL-3", project="PRJ-NONE"), e.conflicts("REL-3")
    e.step(lambda: e.capacity.release(
        subject_kind=ASSIGNMENT, subject_key="REL-1", released_by=SUPERVISOR.task_id,
        reason="parent stopped answering"))
    e.ceiling("runs", 99, by="task-stranger")
    e.ceiling("runs", 99, kind="initiative", scope="INIT-1", by="task-stranger")
    e.ceiling("runs", 99, kind="store", scope="store", by="task-stranger")
    e.observe("file_descriptors", 1, by="task-stranger", method="claimed")
    e.ceiling("runs", 9, kind="initiative", scope="INIT-1")
    e.headroom("initiative", "INIT-1")


@scenario
def cap5(e):
    e.observe("runs", 99.0, method="claimed")
    e.observe("file_descriptors", 1.0, kind="galaxy")
    for bad in (float("nan"), float("inf"), -1.0):
        e.ceiling("runs", bad)
    e.observe("file_descriptors", float("nan"))
    e.step(lambda: e.capacity.declare_limit(
        scope_kind="store", scope_key="global", dimension="runs", unit="runs", ceiling=1.0,
        declared_by=SUPERVISOR.task_id, source="operator"))
    e.step(lambda: e.capacity.declare_limit(
        scope_kind="galaxy", scope_key="x", dimension="runs", unit="runs", ceiling=1.0,
        declared_by=SUPERVISOR.task_id, source="operator"))
    e.observe("a|b", 1.0)
    e.observe("file_descriptors", 1.0, method=" ")
    e.ceiling("a|b", 1.0)
    e.rows("SELECT COUNT(*) AS n FROM execution_limits")


@scenario
def cap6(e):
    e.ceiling("runs", 1), e.ceiling("runs", 3, scope="store", kind="store")
    e.take("REL-1"), e.take("REL-2"), e.take("REL-3", endpoint=BETA, project=PROJECT_B)
    e.report(), e.conflicts("REL-2"), e.headroom("store", "store")


@scenario
def cap6_store(e):
    e.ceiling("runs", 1, scope="store", kind="store")
    e.take("REL-1"), e.take("REL-2", endpoint=BETA, project=PROJECT_B)


@scenario
def cap6_lowered(e):
    e.ceiling("runs", 5), e.take("REL-1"), e.take("REL-2"), e.take("REL-3")
    e.ceiling("runs", 1), e.report(), e.take("REL-4")
    e.rows("SELECT kind, subject, detail FROM journal WHERE kind = 'execution_limit_declared' ORDER BY seq")


@scenario
def cap6_unenforced(e):
    e.ceiling("model_cost", 1, unit="usd", enforce=False), e.take("REL-1"), e.headroom()


@scenario
def cap7(e):
    e.ceiling("file_descriptors", 100, unit="fds", enforce=False)
    e.headroom(), e.take("REL-1"), e.take("REL-2"), e.take("REL-3"), e.headroom()


@scenario
def cap7_unmeasured(e):
    e.ceiling("model_cost", 50, unit="usd"), e.take("REL-1"), e.conflicts("REL-1")


@scenario
def cap7_observed(e):
    e.ceiling("file_descriptors", 100, unit="fds")
    e.observe("file_descriptors", 99, method="counted /proc/<pid>/fd"), e.headroom()
    e.observe("file_descriptors", 120, method="counted /proc/<pid>/fd"), e.headroom()
    e.take("REL-1")


@scenario
def cap7_runs(e):
    e.ceiling("runs", 5), e.take("REL-1"), e.headroom()


@scenario
def cap8(e):
    e.ceiling("runs", 5), e.ceiling("file_descriptors", 100, unit="fds", enforce=False)
    e.take("REL-1"), e.report(), e.headroom()
    e.clock.advance(1_000_000)
    e.report(), e.headroom()


out = {}
for name, fn in SCENARIOS.items():
    env = Env()
    fn(env)
    out[name] = env.steps
    env.store.close()
json.dump(out, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
