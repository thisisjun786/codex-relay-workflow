"""Run one Python supervisor test under an isolated HOME and capture assertions and DB rows.

argv: <root> <Class.method>. Writes <root>/capture.json. The caller provides a fresh
root and HOME/XDG/CODEX_HOME; the test's tempfile tree is <root>/tree.
"""
import importlib
import json
import os
import shutil
import sqlite3
import sys
import tempfile
import unittest

ROOT, TEST = sys.argv[1:3]
assert os.path.isdir(ROOT)
_rmtree = shutil.rmtree


def keep_tree(path, *args, **kwargs):
    if os.path.abspath(path).startswith(os.path.join(os.path.abspath(ROOT), "tree")):
        return None
    return _rmtree(path, *args, **kwargs)


shutil.rmtree = keep_tree
captures = []


def plain(value):
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if isinstance(value, dict):
        return {str(k): plain(v) for k, v in value.items()}
    if isinstance(value, (set, frozenset)):
        return sorted((plain(v) for v in value), key=lambda v: json.dumps(v, sort_keys=True))
    if isinstance(value, (list, tuple)):
        return [plain(v) for v in value]
    try:
        return {k: plain(value[k]) for k in value.keys()}
    except Exception:  # noqa: BLE001
        return repr(value)


def recorder(name, pick):
    original = getattr(unittest.TestCase, name)

    def method(self, *args, **kwargs):
        captures.append(plain(pick(*args)))
        return original(self, *args, **kwargs)
    return method


for _name, _pick in {
    "assertEqual": lambda a, b, *r: a, "assertNotEqual": lambda a, b, *r: a,
    "assertTrue": lambda a, *r: bool(a), "assertFalse": lambda a, *r: bool(a),
    "assertIsNone": lambda a, *r: a, "assertIsNotNone": lambda a, *r: a is not None,
    "assertIn": lambda a, b, *r: a in b, "assertNotIn": lambda a, b, *r: a in b,
    "assertLessEqual": lambda a, b, *r: a <= b, "assertLess": lambda a, b, *r: a < b,
}.items():
    setattr(unittest.TestCase, _name, recorder(_name, _pick))


def snapshot(case, name):
    source = sqlite3.connect(case.store.path)
    target = sqlite3.connect(os.path.join(ROOT, name))
    source.backup(target)
    target.close()
    source.close()


def main():
    module = importlib.import_module("tests.test_supervisor_channel")
    # Deterministic per-attempt token bytes for byte-for-byte Go replay.
    import itertools
    from codex_session_relay import supervisorchannel
    token_numbers = itertools.count()
    supervisorchannel.secrets.token_hex = lambda n: next(token_numbers).to_bytes(n, "big").hex()

    original_obligation = module.ChannelTestCase.obligation
    original_completed = module.ChannelTestCase.completed

    def completed(self, *args, **kwargs):
        result = original_completed(self, *args, **kwargs)
        snapshot(self, "event.sqlite3")
        return result

    module.ChannelTestCase.completed = completed

    def obligation(self, *args, **kwargs):
        # The fixture's completed() records a real event before from_event reads it.
        # Keep the exact event fixture for the Go mirror, before the Python stage runs.
        event_id = args[0] if args else kwargs.get("event_id")
        if event_id is None:
            event_id = self.completed()
        snapshot(self, "event.sqlite3")
        return original_obligation(self, event_id)

    module.ChannelTestCase.obligation = obligation
    # SCH-68 needs the chronology before the second block, not the last
    # obligation() snapshot (which already includes that block).
    if TEST == "WhatTheTwelfthIndependentReviewFound.test_a_block_stated_again_before_its_send_goes_up_as_the_newer_statement":
        cls = module.WhatTheTwelfthIndependentReviewFound
        original_blocked = cls.blocked
        calls = []

        def blocked(self, *args, **kwargs):
            result = original_blocked(self, *args, **kwargs)
            calls.append(result)
            snapshot(self, "first-block.sqlite3" if len(calls) == 1 else "second-block.sqlite3")
            return result

        cls.blocked = blocked
    if TEST == "TheStagedRowIsAProposal.test_an_obligation_discharged_after_staging_is_not_sent":
        cls = module.TheStagedRowIsAProposal
        original_confirmed = cls.confirmed

        def confirmed(self, *args, **kwargs):
            result = original_confirmed(self, *args, **kwargs)
            snapshot(self, "discharged.sqlite3")
            return result

        cls.confirmed = confirmed
    if TEST == "TheStagedRowIsAProposal.test_an_omission_whose_turn_reported_after_staging_is_not_sent":
        original_accept_omission = module.ChannelTestCase.accept

        def accept_omission(self, *args, **kwargs):
            result = original_accept_omission(self, *args, **kwargs)
            snapshot(self, "omission-accepted.sqlite3")
            return result

        module.ChannelTestCase.accept = accept_omission
    if TEST == "WhatTheEleventhIndependentReviewFound.test_a_decision_reported_after_its_block_was_staged_still_goes_up":
        original_accept = module.ChannelTestCase.accept

        def accept(self, *args, **kwargs):
            result = original_accept(self, *args, **kwargs)
            snapshot(self, "accepted.sqlite3")
            return result

        module.ChannelTestCase.accept = accept
    if TEST == "WhatTheEleventhIndependentReviewFound.test_a_decision_reported_after_its_block_was_staged_still_goes_up":
        from codex_session_relay import report as report_module
        original_decision_report = report_module.record
        case_for_decision = []

        def decision_report(self_store, *args, **kwargs):
            result = original_decision_report(self_store, *args, **kwargs)
            snapshot(case_for_decision[0], "decision-recorded.sqlite3")
            return result

        report_module.record = decision_report
    if TEST == "WhatTheEleventhIndependentReviewFound.test_a_report_recorded_after_its_completion_was_staged_is_carried":
        from codex_session_relay import report as report_module
        original_report = report_module.record

        def report(self_store, *args, **kwargs):
            result = original_report(self_store, *args, **kwargs)
            snapshot(case_for_report[0], "report-recorded.sqlite3")
            return result

        case_for_report = []
        report_module.record = report
    if TEST == "WhatTheFifthReviewRoundFound.test_a_report_claimed_inside_the_gap_after_a_delivery_is_refused":
        original_claim = supervisorchannel.SupervisorChannel._claim

        def claim(self, *args, **kwargs):
            snapshot(self, "delivery-claimed.sqlite3")
            return original_claim(self, *args, **kwargs)

        supervisorchannel.SupervisorChannel._claim = claim
    tree = os.path.join(ROOT, "tree")
    os.makedirs(tree, exist_ok=True)
    tempfile.mkdtemp = lambda *args, **kwargs: tree
    suite = unittest.defaultTestLoader.loadTestsFromName(
        "tests.test_supervisor_channel." + TEST)
    while isinstance(suite, unittest.TestSuite):
        suite = next(iter(suite))
    case = suite
    if TEST == "WhatTheEleventhIndependentReviewFound.test_a_decision_reported_after_its_block_was_staged_still_goes_up":
        case_for_decision.append(case)
    if TEST == "WhatTheEleventhIndependentReviewFound.test_a_report_recorded_after_its_completion_was_staged_is_carried":
        case_for_report.append(case)
    result = unittest.TestResult()
    original_setup = case.setUp

    def setup():
        original_setup()
        snapshot(case, "setup.sqlite3")

    case.setUp = setup
    case.run(result)
    problems = [tb for _test, tb in result.failures + result.errors]
    tables = {}
    path = os.path.join(tree, "state", "relay.sqlite3")
    if TEST == "WhatMayBeStaged.test_an_ordinary_event_is_not_news_and_has_no_obligation_to_stage":
        source = sqlite3.connect(path)
        target = sqlite3.connect(os.path.join(ROOT, "ordinary-final.sqlite3"))
        source.backup(target)
        target.close()
        source.close()
    if os.path.exists(path):
        db = sqlite3.connect(path)
        db.row_factory = sqlite3.Row
        for (table,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
            rows = [dict(row) for row in db.execute(f"SELECT * FROM {table} ORDER BY rowid")]
            if rows:
                tables[table] = plain(rows)
        db.close()
    with open(os.path.join(ROOT, "capture.json"), "w") as handle:
        json.dump({"captures": captures, "problems": problems, "tables": tables}, handle)
    if problems:
        print("\n".join(problems), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
