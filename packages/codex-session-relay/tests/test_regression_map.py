"""The regression map's own inventories, derived from source rather than asserted in prose.

Two independent review rounds on the plan behind this work re-opened the same two
categories: evidence claimed as new that already existed, and clock provenance stated
wrongly. Patching the instances a third time would have been the third round of one
mistake. This package already has the answer to that shape - guard.EVALUATION_STAGES is
checked against the injections that exercise it, so a stage added without being listed
fails here rather than in review - and this applies it to the map.

What this closes: a citation that no longer resolves, and a criterion whose clock label
disagrees with the modules it names.

What it does NOT close, stated so nobody reads more into a green run than is there:
whether a new test asserts something an existing test already asserts. No scan can answer
that. The map's reuse column is where that judgement is written down, not where it is
enforced.
"""

import ast
import pathlib
import re
import unittest

TESTS = pathlib.Path(__file__).resolve().parent
MAP = TESTS.parent / "docs" / "contention-regression.md"

# Modules that read a real clock or start a process, so they spend wall time rather than
# moving an injected one. Derived below and compared against this tuple; the reasons are in
# the map. A new test that waits on anything real fails here until it is named.
REAL_TIME_MODULES = (
    "test_bridge_adapter.py",
    "test_cli.py",
    "test_daemon_cadence.py",
    "test_failure_recovery.py",
    "test_management_cli.py",
    "test_operational_scale.py",
    "test_service.py",
    "test_wp1_regressions.py",
)

CLOCK_NAMES = {"sleep", "monotonic", "time", "perf_counter"}
SUBPROCESS_NAMES = {"Popen", "run", "check_output", "call"}
# Spelled rather than typed, because the map is Markdown and a code span delimiter inside a
# pattern here would be one more thing to escape in two languages at once.
TICK = "\u0060"
MODULE = re.compile(TICK + r"(test_[a-z0-9_]+\.py)" + TICK)
SPANNED = re.compile(TICK + "[^" + TICK + "]*" + TICK)
CLASS = re.compile(r"\b([A-Z][A-Za-z0-9]+)\b")


def criterion_rows():
    """The criterion table, as (number, reused cell, new cell, clock cell)."""
    rows = []
    for line in MAP.read_text(encoding="utf-8").splitlines():
        if not line.startswith("|"):
            continue
        cells = [cell.strip() for cell in line.strip().strip("|").split("|")]
        if len(cells) == 5 and cells[0].isdigit():
            rows.append((int(cells[0]), cells[2], cells[3], cells[4]))
    return rows


def spends_real_time(path):
    """Why this module spends wall time, or an empty set if it only moves an injected clock.

    The boundary is deliberate: reading a clock or starting a process is what produces
    elapsed-time evidence, and that is what the map's column is about. A barrier, a thread
    join or a lock acquisition blocks until another thread arrives rather than until a
    duration passes, so it synchronises without measuring. A test that did assert on how long
    one of them took would have to read a clock, and would be caught here. Modules that only
    synchronise stay injected, and the map records the boundary rather than leaving it to be
    inferred from this function.
    """
    reasons = set()
    for node in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
        if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)):
            continue
        function = node.func
        owner = function.value.id if isinstance(function.value, ast.Name) else None
        if owner == "time" and function.attr in CLOCK_NAMES:
            reasons.add("time." + function.attr)
        if owner == "subprocess" and function.attr in SUBPROCESS_NAMES:
            reasons.add("subprocess." + function.attr)
        if function.attr == "spawn_worker":
            reasons.add("spawn_worker")
    return reasons


class TheMapCitesOnlyEvidenceThatExists(unittest.TestCase):
    def setUp(self):
        self.rows = criterion_rows()
        self.assertTrue(self.rows, "no criterion table was found in " + str(MAP))

    def classes_in(self, name):
        path = TESTS / name
        self.assertTrue(path.exists(), "the map names " + name + ", which is not in this suite")
        return {
            node.name
            for node in ast.walk(ast.parse(path.read_text(encoding="utf-8")))
            if isinstance(node, ast.ClassDef)
        }

    def test_every_reused_class_is_a_class_in_the_module_it_is_attributed_to(self):
        checked = 0
        for number, reused, _new, _clock in self.rows:
            if reused.lower().startswith("none"):
                continue
            for segment in reused.split(";"):
                modules = MODULE.findall(segment)
                if not modules:
                    continue
                prose = SPANNED.sub(" ", segment)
                for name in CLASS.findall(prose):
                    self.assertIn(
                        name, self.classes_in(modules[0]),
                        f"criterion {number} reuses {modules[0]} {name}, which that module"
                        " does not define; the citation is stale",
                    )
                    checked += 1
        self.assertGreater(
            checked, 20, f"only {checked} citations were checked, so the parser stopped seeing"
            " the table it is supposed to be reading",
        )

    def test_every_module_the_map_names_exists(self):
        for number, reused, new, _clock in self.rows:
            for cell in (reused, new):
                if cell.lower().startswith("none"):
                    continue
                for name in MODULE.findall(cell):
                    self.assertTrue(
                        (TESTS / name).exists(),
                        f"criterion {number} names {name}, which is not in this suite",
                    )


class TheMapNamesEveryTestThatSpendsRealTime(unittest.TestCase):
    def test_the_declared_real_time_modules_are_exactly_the_modules_that_spend_real_time(self):
        derived = {
            path.name for path in sorted(TESTS.glob("test_*.py")) if spends_real_time(path)
        }
        declared = set(REAL_TIME_MODULES)
        self.assertEqual(
            derived, declared,
            "the map's real-time inventory and the source disagree."
            f" Only in source: {sorted(derived - declared)}."
            f" Only declared: {sorted(declared - derived)}."
            " Name the new one in REAL_TIME_MODULES and in the map, or stop it waiting.",
        )

    def test_a_criterion_naming_a_real_time_module_is_not_labelled_injected(self):
        """The check the first draft of this module was missing.

        A global module inventory can be perfectly correct while a criterion resting on one of
        those modules still says its evidence came off an injected clock. The label is per
        module, deliberately coarse: a criterion sharing a module with one real-time case
        inherits the label rather than claiming the half it likes.
        """
        real_time = set(REAL_TIME_MODULES)
        for number, reused, new, clock in criterion_rows():
            named = set()
            for cell in (reused, new):
                if not cell.lower().startswith("none"):
                    named.update(MODULE.findall(cell))
            waiting = named & real_time
            if waiting:
                self.assertNotEqual(
                    clock, "injected",
                    f"criterion {number} rests on {sorted(waiting)}, which spend real time,"
                    f" but its clock is recorded as {clock!r}",
                )
            else:
                self.assertEqual(
                    clock, "injected",
                    f"criterion {number} is recorded as {clock!r} but names no module that"
                    f" spends real time: {sorted(named)}",
                )


if __name__ == "__main__":
    unittest.main()
