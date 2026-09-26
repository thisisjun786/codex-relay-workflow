"""Runs every test of one Python test module, each over its own tree the Go tests share, and
records every value each test asserts.

argv: <root> <module>. Each test runs with tempfile.mkdtemp returning <root>/<Class>.<method>, so
paths, revision hashes, event and request ids are the ones the Go test derives in the same tree.
Every assertion still runs; each also appends the value the Python code produced (the first
argument of assertEqual, the expression of assertTrue, ...) to the test's capture list. The Go
mirror of the test performs the same steps and must produce the same list, so every asserted
value is compared with what Python computed rather than with a constant. <tree>/capture.json
holds {captures, problems, tables, sends}.
"""
import json
import os
import shutil
import sqlite3
import sys
import tempfile
import unittest

ROOT, MODULE = sys.argv[1:3]
_rmtree = shutil.rmtree


def _keep_trees(path, *args, **kwargs):
    if os.path.abspath(path).startswith(os.path.abspath(ROOT)):
        return None
    return _rmtree(path, *args, **kwargs)


shutil.rmtree = _keep_trees
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


def flatten(suite):
    for item in suite:
        if isinstance(item, unittest.TestSuite):
            yield from flatten(item)
        else:
            yield item


suite = unittest.defaultTestLoader.loadTestsFromName(f"tests.{MODULE}")
for case in flatten(suite):
    name = f"{type(case).__name__}.{case._testMethodName}"
    tree = os.path.join(ROOT, name)
    os.makedirs(tree)
    tempfile.mkdtemp = lambda prefix=None, tree=tree: tree
    captures.clear()
    result = unittest.TestResult()
    case.run(result)
    problems = [tb for _t, tb in result.failures + result.errors]
    tables = {}
    path = os.path.join(tree, "state", "relay.sqlite3")
    if os.path.exists(path):
        db = sqlite3.connect(path)
        db.row_factory = sqlite3.Row
        for (table,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
            rows = [dict(r) for r in db.execute(f"SELECT * FROM {table} ORDER BY rowid")]
            if rows:
                tables[table] = rows
        db.close()
    adapter = getattr(case, "adapter", None)
    sends = [list(s) for s in adapter.sends] if adapter is not None else []
    with open(os.path.join(tree, "capture.json"), "w") as handle:
        json.dump({"captures": list(captures), "problems": problems, "tables": tables,
                   "sends": sends}, handle, default=repr)
