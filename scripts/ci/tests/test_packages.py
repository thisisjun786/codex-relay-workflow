"""Negative tests for the packages check's own reporting rules.

These need no uv, no network and no package environment: they drive the report
reader directly, because the failures worth catching are the ones where the job
would otherwise print a success line over an empty or skipped run.
"""

import importlib.util
import os
from pathlib import Path
import shutil
import tempfile
import unittest

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

    def partition(self, items, index, total):
        namespace = {}
        exec(packages.SHARD_PLUGIN, namespace)
        deselected = []

        class Hook:
            def pytest_deselected(self, items):
                deselected.extend(items)

        class Config:
            hook = Hook()

        kept = list(items)
        old = os.environ.get("CRW_PACKAGES_SHARD")
        os.environ["CRW_PACKAGES_SHARD"] = f"{index}/{total}"
        try:
            namespace["pytest_collection_modifyitems"](Config(), kept)
        finally:
            if old is None:
                del os.environ["CRW_PACKAGES_SHARD"]
            else:
                os.environ["CRW_PACKAGES_SHARD"] = old
        return kept, deselected

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
        for total in (1, 2, 3, 5):
            with self.subTest(total=total):
                seen, modules = [], {}
                for index in range(1, total + 1):
                    kept, dropped = self.partition(items, index, total)
                    self.assertEqual(sorted(map(id, kept + dropped)), sorted(map(id, items)))
                    self.assertEqual(kept, [item for item in items if item in kept])
                    for item in kept:
                        module = item.nodeid.split("::")[0]
                        self.assertEqual(modules.setdefault(module, index), index)
                    seen += kept
                self.assertEqual(sorted(map(id, seen)), sorted(map(id, items)))

    def test_largest_modules_are_spread_across_shards(self):
        items = self.items([("a", 7), ("b", 1), ("c", 4), ("d", 4), ("e", 2)])
        sizes = [len(self.partition(items, index, 2)[0]) for index in (1, 2)]
        self.assertEqual(sorted(sizes), [9, 9])

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
        finally:
            packages.run = original
        self.assertEqual(calls, [])

if __name__ == "__main__":
    unittest.main()
