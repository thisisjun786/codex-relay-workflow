"""Run one original test_omitted method and capture every complete observe result.

Each call retains its input tree and a SQLite backup, including the WAL. Registry writes made
inside observe are captured separately for deterministic read-race replay. argv: <root> <method>.
"""
import json
import os
import shutil
import sqlite3
import stat
import sys
import tempfile
import unittest

ROOT, METHOD = sys.argv[1:3]
_real_rmtree = shutil.rmtree


def keep(path, *args, **kwargs):
    if os.path.abspath(path).startswith(os.path.join(os.path.abspath(ROOT), "tree")):
        return None
    return _real_rmtree(path, *args, **kwargs)


shutil.rmtree = keep
tree = os.path.join(ROOT, "tree")
os.makedirs(tree, exist_ok=True)
_real_mkdtemp = tempfile.mkdtemp


def under_tree(*args, **kwargs):
    kwargs["dir"] = tree
    return _real_mkdtemp(*args, **kwargs)


tempfile.mkdtemp = under_tree


def plain(value):
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if isinstance(value, os.PathLike):
        return os.fspath(value)
    if isinstance(value, dict):
        return {str(k): plain(v) for k, v in value.items()}
    try:
        return {str(k): plain(value[k]) for k in value.keys()}
    except (AttributeError, TypeError):
        pass
    if isinstance(value, (list, tuple)):
        return [plain(v) for v in value]
    if hasattr(value, "path"):
        return {"path": os.fspath(value.path), "source": getattr(value, "source", "explicit")}
    return repr(value)


def main():
    from codex_session_relay import omitted
    import tests.test_omitted as module

    calls = []
    writes = []
    original = omitted.observe
    original_setup = module.Reporting.setUp

    def setup(case):
        original_setup(case)
        case.store.db.set_trace_callback(
            lambda sql: writes.append(sql) if sql.lstrip().upper().startswith("UPDATE ") else None)

    module.Reporting.setUp = setup

    def copy_fact(source, target):
        if stat.S_ISFIFO(os.stat(source).st_mode):
            os.mkfifo(target)
            return target
        return shutil.copy2(source, target)

    def observe(selection, marker_root, workspace, assignment, session, turn, now, **kwargs):
        number = len(calls)
        snapshot = os.path.join(ROOT, "call-%d" % number)
        shutil.copytree(tree, snapshot, symlinks=True, copy_function=copy_fact)
        source_path = os.fspath(selection.db_path)
        if os.path.isfile(source_path):
            relative = os.path.relpath(source_path, tree)
            copied = os.path.join(snapshot, relative)
            for suffix in ("", "-wal", "-shm"):
                try:
                    os.unlink(copied + suffix)
                except FileNotFoundError:
                    pass
            source = sqlite3.connect(source_path)
            target = sqlite3.connect(copied)
            source.backup(target)
            target.close()
            source.close()
        writes.clear()
        try:
            result = original(selection, marker_root, workspace, assignment, session, turn, now,
                              **kwargs)
        except Exception as error:
            calls.append({"args": plain([selection, marker_root, workspace, assignment, session,
                                         turn, now, kwargs]),
                          "error": type(error).__name__ + ": " + str(error)})
            raise
        calls.append({"args": plain([selection, marker_root, workspace, assignment, session,
                                     turn, now, kwargs]), "result": plain(result),
                      "snapshot": snapshot, "writes": list(writes)})
        return result

    omitted.observe = observe
    suite = unittest.defaultTestLoader.loadTestsFromName("tests.test_omitted.Reporting." + METHOD)
    result = unittest.TestResult()
    suite.run(result)
    problems = [tb for _case, tb in result.failures + result.errors]
    with open(os.path.join(ROOT, "capture.json"), "w") as handle:
        json.dump({"calls": calls, "problems": problems}, handle, sort_keys=True)
    if problems:
        print("\n".join(problems), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
