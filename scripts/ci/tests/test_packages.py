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

if __name__ == "__main__":
    unittest.main()
