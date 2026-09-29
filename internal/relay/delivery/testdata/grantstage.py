"""Stages the merge-turn half of test_unknown_send_lost's AnUncertainGrantIsReadAsStatusReadsIt in
Python, over a store the Go test then reads and writes itself.

argv: <tree>. The merge turn's request, promotion, grant notice, acknowledgement and regrant are
mergeturn.py's (todo 26 ports them); reconciliation and the status reading, which the property is
about, run in Go on the same file. The first line printed is the staged state; after it the script
reads one command per line (advance <s> | answer | regrant | quit) and answers each with a line.
"""
import json
import os
import sys
import tempfile

TREE = sys.argv[1]
_real_mkdtemp = tempfile.mkdtemp


def _fixture_mkdtemp(*args, **kwargs):
    # Only the Python tests' own fixture directories are the tree Go shares. The relay package's
    # scratch directories (the ownership fence's schema_meta snapshot) stay real temporary ones.
    caller = sys._getframe(1).f_globals.get("__name__", "")
    if caller == "__main__" or caller.split(".")[0] == "tests":
        return TREE
    return _real_mkdtemp(*args, **kwargs)


tempfile.mkdtemp = _fixture_mkdtemp

from tests.support import PARENT  # noqa: E402
from tests.test_merge_turn_wake import MergeTurnWakeTestCase  # noqa: E402


class Case(MergeTurnWakeTestCase):
    def runTest(self):
        pass


c = Case()
c.setUp()
# promoted(), keeping the state of the claim it queued (the value the Python test asserts).
from tests.test_merge_turn_wake import PROJECT_A, PROJECT_B  # noqa: E402
held = c.claim(c.beta, PROJECT_B, "head-b")
waiter = c.claim(c.alpha, PROJECT_A, "head-a")
waiting = waiter["state"]
c.turns.release(held["turnId"], actor=c.beta.task_id, disposition="returned",
                reason="not ready after all")
turn = waiter["turnId"]
event = c.wakes()[0]["event_id"]
c.adapter.script("transport_unknown")
record = c.attempt(event)
c.store.db.commit() if c.store.db.in_transaction else None
# One runtime writes the store at a time: Python stops writing it here, and the Go test takes
# it over. Python writes again (answer, regrant) only once the Go test has handed it back, on
# the same Store object reopened - every component of the case keeps holding that object.
c.store.close()
print(json.dumps({
    "waiting": waiting, "turn": turn, "event": event, "record": record,
    "receipt": c.adapter.ledger[record["requestId"]],
    "sends": [list(s) for s in c.adapter.sends], "now": c.clock.now(),
    "threads": sorted(c.adapter.threads), "store": str(c.store.path),
}), flush=True)
for line in sys.stdin:
    words = line.split()
    if not words or words[0] == "quit":
        break
    if words[0] == "advance":
        c.clock.advance(float(words[1]))
    elif words[0] in ("answer", "regrant"):
        c.store.__init__(c.store.path)
        try:
            if words[0] == "answer":
                c.answer_grant(turn, PARENT)
            else:
                c.turns.declare_ready(turn, actor=PARENT, ready=True, candidate_head="head-a2")
            c.store.db.commit() if c.store.db.in_transaction else None
        finally:
            c.store.close()
    print(json.dumps({"ok": words[0]}), flush=True)
