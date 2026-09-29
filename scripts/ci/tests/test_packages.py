"""Negative tests for the packages check's own reporting rules.

These need no uv, no network and no package environment: they drive the report
reader directly, because the failures worth catching are the ones where the job
would otherwise print a success line over an empty or skipped run.
"""

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest import mock

SCRIPT = Path(__file__).resolve().parents[1] / "packages.py"
spec = importlib.util.spec_from_file_location("packages_check", SCRIPT)
packages = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packages)


def report(body):
    return f"<testsuites>{body}</testsuites>"


def suite(tests, failures=0, errors=0, cases=""):
    return (
        f'<testsuite tests="{tests}" failures="{failures}" errors="{errors}">{cases}</testsuite>'
    )


def case(message):
    return f'<testcase classname="tests.test_x" name="t"><skipped message="{message}"/></testcase>'


class ReportTests(unittest.TestCase):
    def setUp(self):
        self.directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.directory, True)

    def write(self, text):
        path = self.directory / "report.xml"
        path.write_text(text, encoding="utf-8")
        return path

    def test_a_successful_run_reports_its_test_count(self):
        self.assertEqual(packages.read_report(self.write(report(suite(12))), "pkg"), 12)

    def test_a_single_testsuite_root_is_read_too(self):
        self.assertEqual(packages.read_report(self.write(suite(4)), "pkg"), 4)

    def test_an_empty_collection_is_not_a_pass(self):
        with self.assertRaises(packages.Failure) as raised:
            packages.read_report(self.write(report(suite(0))), "pkg")
        self.assertIn("collected no tests", str(raised.exception))

    def test_a_missing_report_is_not_a_pass(self):
        with self.assertRaises(packages.Failure):
            packages.read_report(self.directory / "absent.xml", "pkg")

    def test_failures_and_errors_are_rejected(self):
        for failures, errors in ((1, 0), (0, 1), (2, 3)):
            with self.subTest(failures=failures, errors=errors):
                document = self.write(report(suite(5, failures=failures, errors=errors)))
                with self.assertRaises(packages.Failure):
                    packages.read_report(document, "pkg")

    def test_the_bridge_import_skip_is_named_in_the_failure(self):
        document = self.write(report(suite(2, cases=case(packages.BRIDGE_SKIP))))
        with self.assertRaises(packages.Failure) as raised:
            packages.read_report(document, "codex-session-relay")
        self.assertIn("codex_thread_bridge", str(raised.exception))

    def test_any_other_skip_also_fails_the_check(self):
        document = self.write(report(suite(2, cases=case("this filesystem is unusual"))))
        with self.assertRaises(packages.Failure) as raised:
            packages.read_report(document, "pkg")
        self.assertIn("skipped 1 tests", str(raised.exception))

    def test_counts_accumulate_across_suites(self):
        self.assertEqual(packages.read_report(self.write(report(suite(3) + suite(4))), "pkg"), 7)


class EnclosingCheckoutTests(unittest.TestCase):
    """Bounded so the result comes from the tree the test builds, not from the host."""

    def tree(self, repository):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, root, True)
        if repository:
            (root / ".git").mkdir()
        nested = root / "a" / "b"
        nested.mkdir(parents=True)
        return root, nested

    def test_a_surrounding_repository_is_found(self):
        root, nested = self.tree(repository=True)
        self.assertEqual(packages.enclosing_checkout(nested, stop=root.parent), root)

    def test_a_clean_tree_reports_nothing(self):
        root, nested = self.tree(repository=False)
        self.assertIsNone(packages.enclosing_checkout(nested, stop=root.parent))

    def test_the_directory_itself_counts(self):
        root, _ = self.tree(repository=True)
        self.assertEqual(packages.enclosing_checkout(root, stop=root.parent), root)


class TemporaryRootTests(unittest.TestCase):
    def setUp(self):
        self.previous = os.environ.get("CRW_PACKAGES_TMPDIR")
        self.addCleanup(self.restore)

    def restore(self):
        if self.previous is None:
            os.environ.pop("CRW_PACKAGES_TMPDIR", None)
        else:
            os.environ["CRW_PACKAGES_TMPDIR"] = self.previous

    def test_a_directory_inside_a_checkout_is_refused(self):
        enclosing = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, enclosing, True)
        (enclosing / ".git").mkdir()
        nested = enclosing / "scratch"
        nested.mkdir()
        os.environ["CRW_PACKAGES_TMPDIR"] = str(nested)
        with self.assertRaises(packages.Failure) as raised:
            packages.temporary_root()
        self.assertIn("CRW_PACKAGES_TMPDIR", str(raised.exception))


class ShardTests(unittest.TestCase):
    """A CI leg runs one shard; the legs together must be exactly the whole run."""

    def partition(self, items, index, total, durations=None):
        namespace = {}
        exec(packages.SHARD_PLUGIN, namespace)
        deselected = []

        class Hook:
            def pytest_deselected(self, items):
                deselected.extend(items)

        class Config:
            hook = Hook()

        kept = list(items)
        environment = {"CRW_PACKAGES_SHARD": f"{index}/{total}"}
        if durations is not None:
            environment["CRW_PACKAGES_DURATIONS"] = json.dumps(durations)
        with mock.patch.dict(os.environ, environment):
            if durations is None:
                os.environ.pop("CRW_PACKAGES_DURATIONS", None)
            namespace["pytest_collection_modifyitems"](Config(), kept)
        return kept, deselected

    def modules(self, items, index, total, durations=None):
        kept = self.partition(items, index, total, durations)[0]
        return sorted({item.nodeid.split("::")[0] for item in kept})

    def items(self, sizes):
        class Item:
            def __init__(self, nodeid):
                self.nodeid = nodeid

            def __repr__(self):
                return self.nodeid

        return [Item(f"tests/test_{module}.py::test_{n}")
                for module, size in sizes for n in range(size)]

    def test_shards_are_disjoint_ordered_complete_and_keep_modules_whole(self):
        items = self.items([("a", 7), ("b", 1), ("c", 4), ("d", 4), ("e", 2)])
        # Without a table; with one naming only some modules; with one naming every module.
        tables = (None, {"tests/test_b.py": 30.0, "tests/test_d.py": 2.5},
                  {f"tests/test_{name}.py": seconds
                   for name, seconds in zip("abcde", (1.0, 9.0, 4.0, 4.0, 0.0))})
        for durations in tables:
            for total in (1, 2, 3, 5):
                with self.subTest(durations=durations, total=total):
                    seen, modules = [], {}
                    for index in range(1, total + 1):
                        kept, dropped = self.partition(items, index, total, durations)
                        self.assertEqual(sorted(map(id, kept + dropped)), sorted(map(id, items)))
                        self.assertEqual(kept, [item for item in items if item in kept])
                        for item in kept:
                            module = item.nodeid.split("::")[0]
                            self.assertEqual(modules.setdefault(module, index), index)
                        seen += kept
                    self.assertEqual(sorted(map(id, seen)), sorted(map(id, items)))

    def test_recorded_seconds_outweigh_test_counts(self):
        # By count, `slow` (2 tests) would join the 40-test module and leave one leg at
        # 120 s and the other at 60 s. By time it is a shard's whole load.
        items = self.items([("slow", 2), ("a", 40), ("b", 30), ("c", 20)])
        durations = {"tests/test_slow.py": 90.0, "tests/test_a.py": 30.0,
                     "tests/test_b.py": 30.0, "tests/test_c.py": 30.0}
        shards = [self.modules(items, index, 2, durations) for index in (1, 2)]
        self.assertEqual(shards, [["tests/test_slow.py"],
                                  ["tests/test_a.py", "tests/test_b.py", "tests/test_c.py"]])

    def test_an_unrecorded_module_weighs_its_tests_at_the_recorded_mean(self):
        # `a` and `b` record 10 s per test, so the 15 new tests weigh 150 s: the heaviest
        # module, alone on its shard. At 1 per test (or 0) it would join `a` instead.
        items = self.items([("a", 10), ("b", 10), ("new", 15)])
        durations = {"tests/test_a.py": 100.0, "tests/test_b.py": 100.0}
        shards = [self.modules(items, index, 2, durations) for index in (1, 2)]
        self.assertEqual(shards, [["tests/test_new.py"], ["tests/test_a.py", "tests/test_b.py"]])

    def test_the_mean_counts_only_recorded_modules_in_this_collection(self):
        # `gone` is recorded but not collected, so it neither sets the rate nor takes a
        # place. At 10 s per test `new` weighs 50 s and joins `a`; counting `gone` would
        # make the rate 30 s and leave `new` alone, and placing it would load a leg with
        # 400 s of tests it never runs.
        items = self.items([("a", 10), ("b", 10), ("new", 5)])
        durations = {"tests/test_a.py": 100.0, "tests/test_b.py": 100.0,
                     "tests/test_gone.py": 400.0}
        shards = [self.modules(items, index, 2, durations) for index in (1, 2)]
        self.assertEqual(shards, [["tests/test_a.py", "tests/test_new.py"], ["tests/test_b.py"]])

    def test_without_a_recorded_module_each_test_weighs_one(self):
        # Largest first onto the lightest shard: a+e and c+d+b, 9 tests each.
        items = self.items([("a", 7), ("b", 1), ("c", 4), ("d", 4), ("e", 2)])
        for durations in (None, {}, {"tests/test_other.py": 50.0}):
            with self.subTest(durations=durations):
                sizes = [len(self.partition(items, index, 2, durations)[0]) for index in (1, 2)]
                self.assertEqual(sizes, [9, 9])

    def test_the_whole_run_deselects_nothing(self):
        items = self.items([("a", 2), ("b", 1)])
        self.assertEqual(self.partition(items, 1, 1), (items, []))

    def test_shard_arguments_are_validated(self):
        self.assertEqual(packages.parse_shard("2/3"), (2, 3))
        self.assertEqual(packages.parse_shard("1/1"), (1, 1))
        for value in ("0/2", "3/2", "1/0", "1", "a/b", "-1/2", "1/2/3", "", " 1/2"):
            with self.subTest(value=value), self.assertRaises(packages.Failure):
                packages.parse_shard(value)

    def test_bad_arguments_fail_before_anything_runs(self):
        calls = []
        original = packages.run
        packages.run = lambda *args, **kwargs: calls.append(args)
        try:
            for argv in (["--shard"], ["--shard", "0/2"], ["--other"], ["--shard", "1/2", "x"]):
                with self.subTest(argv=argv):
                    self.assertEqual(packages.main(argv), 1)
            with mock.patch.dict(os.environ, {"CRW_PACKAGES_RECORD": "unused.json"}):
                self.assertEqual(packages.main(["--shard", "1/2"]), 1)
        finally:
            packages.run = original
        self.assertEqual(calls, [])


def junit(cases):
    """A JUnit report holding (classname, seconds, failed) cases."""
    body = "".join(
        f'<testcase classname="{classname}" name="t{n}" time="{seconds}">'
        + ('<failure message="no"/>' if failed else "") + "</testcase>"
        for n, (classname, seconds, failed) in enumerate(cases)
    )
    failures = sum(1 for _, _, failed in cases if failed)
    return report(f'<testsuite tests="{len(cases)}" failures="{failures}" errors="0">{body}</testsuite>')


class DurationTests(unittest.TestCase):
    """The table the shards are balanced on, and the whole run that records it."""

    def setUp(self):
        self.directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.directory, True)

    def test_a_report_is_summed_per_module_file(self):
        package = self.directory / "pkg"
        for name in ("tests/test_a.py", "tests/test_b.py", "tests/sub/test_c.py"):
            (package / name).parent.mkdir(parents=True, exist_ok=True)
            (package / name).touch()
        document = self.directory / "report.xml"
        document.write_text(junit([
            ("tests.test_a.Case", 1.25, False), ("tests.test_a", 0.3, False),
            ("tests.test_b.Outer.Inner", 2.04, False), ("tests.sub.test_c", 0.0, False),
            ("tests.test_a.Case", 0.01, False),
        ]), encoding="utf-8")
        self.assertEqual(packages.module_durations(document, package), {
            "tests/test_a.py": 1.6, "tests/test_b.py": 2.0, "tests/sub/test_c.py": 0.0})

    def test_a_case_without_a_module_file_is_refused(self):
        package = self.directory / "pkg"
        (package / "tests").mkdir(parents=True)
        document = self.directory / "report.xml"
        document.write_text(junit([("tests.test_gone.Case", 1.0, False)]), encoding="utf-8")
        with self.assertRaises(packages.Failure) as raised:
            packages.module_durations(document, package)
        self.assertIn("tests.test_gone.Case", str(raised.exception))

    def test_the_committed_table_is_well_formed(self):
        table = packages.load_durations()
        self.assertEqual(set(table), {directory for directory, _, _ in packages.PACKAGES})
        self.assertTrue(all(table.values()))

    def test_a_malformed_table_is_refused(self):
        path = self.directory / "durations.json"
        for text in ("[]", '{"pkg": []}', '{"pkg": {"t.py": -1}}', '{"pkg": {"t.py": true}}',
                     '{"pkg": {"t.py": "1"}}', "{"):
            with self.subTest(text=text):
                path.write_text(text, encoding="utf-8")
                with self.assertRaises(packages.Failure):
                    packages.load_durations(path)

    def main(self, argv, environment, failing=False):
        """Run main() with every command faked; the pytest step writes a JUnit report."""
        legs = []

        def run(command, *, cwd=packages.ROOT, env=None, capture=False):
            command = [str(part) for part in command]
            reports = [part.split("=", 1)[1] for part in command if part.startswith("--junit-xml=")]
            if reports:
                legs.append((Path(cwd).name, dict(env)))
                first, second = sorted(Path(cwd).glob("tests/test_*.py"))[:2]
                # The second module is reported first, so the table's order is its own.
                Path(reports[0]).write_text(junit([
                    (f"tests.{second.stem}", 0.24, False),
                    (f"tests.{first.stem}.Case", 1.26, failing),
                    (f"tests.{first.stem}.Case", 0.25, False),
                ]), encoding="utf-8")
            return ""

        with mock.patch.object(packages, "run", run), \
                mock.patch.object(packages, "import_locations", lambda env: None), \
                mock.patch.object(packages, "temporary_root",
                                  lambda: Path(tempfile.mkdtemp(dir=str(self.directory)))), \
                mock.patch.dict(os.environ, environment), \
                contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()):
            return packages.main(argv), legs

    def test_a_whole_run_records_the_table_from_its_reports(self):
        record = self.directory / "durations.json"
        status, _ = self.main([], {"CRW_PACKAGES_RECORD": str(record)})
        self.assertEqual(status, 0)
        first = {}
        for directory, _, _ in packages.PACKAGES:
            modules = sorted((packages.ROOT / "packages" / directory).glob("tests/test_*.py"))
            first[directory] = [path.name for path in modules[:2]]
        relay, bridge = first["codex-session-relay"], first["codex-thread-bridge"]
        self.assertEqual(record.read_text(encoding="utf-8"), (
            '{\n'
            ' "codex-session-relay": {\n'
            f'  "tests/{relay[0]}": 1.5,\n'
            f'  "tests/{relay[1]}": 0.2\n'
            ' },\n'
            ' "codex-thread-bridge": {\n'
            f'  "tests/{bridge[0]}": 1.5,\n'
            f'  "tests/{bridge[1]}": 0.2\n'
            ' }\n'
            '}\n'
        ))

    def test_a_failed_run_records_nothing(self):
        record = self.directory / "durations.json"
        status, _ = self.main([], {"CRW_PACKAGES_RECORD": str(record)}, failing=True)
        self.assertEqual(status, 1)
        self.assertFalse(record.exists())

    def test_each_shard_leg_hands_the_plugin_its_own_package_table(self):
        status, legs = self.main(["--shard", "2/2"], {})
        self.assertEqual(status, 0)
        table = packages.load_durations()
        self.assertEqual([directory for directory, _ in legs],
                         [directory for directory, _, _ in packages.PACKAGES])
        for directory, env in legs:
            with self.subTest(directory=directory):
                self.assertEqual(env["CRW_PACKAGES_SHARD"], "2/2")
                self.assertEqual(json.loads(env["CRW_PACKAGES_DURATIONS"]), table[directory])

if __name__ == "__main__":
    unittest.main()
