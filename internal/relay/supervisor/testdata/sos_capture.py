"""Capture operation boundaries from one original supervisor omission-store test.

argv: ROOT Class.method.  Each real relay command and omitted.derive call records its complete
input, output, pre-operation tree, and all populated SQLite rows after the operation.
"""
import contextlib
import io
import json
import os
import shutil
import sqlite3
import sys
import tempfile
import unittest

ROOT, TEST = sys.argv[1:3]
TREE = os.path.join(ROOT, "tree")
os.makedirs(TREE, exist_ok=True)
_real_rmtree = shutil.rmtree
_real_mkdtemp = tempfile.mkdtemp


def keep(path, *args, **kwargs):
    if os.path.abspath(path).startswith(os.path.abspath(TREE)):
        return None
    return _real_rmtree(path, *args, **kwargs)


def tree_mkdtemp(*args, **kwargs):
    """A test's own temporary directory is the captured tree; nothing else's is.

    The fence reads a store's ownership stamp through a disposable copy in a TemporaryDirectory
    (ownership.metadata). Sending that one into the tree too would leave a stale second copy of
    the store there, which find_db and the Go replay would take for the store itself.
    """
    if sys._getframe(1).f_globals.get("__name__", "").startswith("tests."):
        return TREE
    return _real_mkdtemp(*args, **kwargs)


shutil.rmtree = keep
tempfile.mkdtemp = tree_mkdtemp


def plain(value):
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if isinstance(value, os.PathLike):
        return os.fspath(value)
    if isinstance(value, dict):
        return {str(k): plain(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [plain(v) for v in value]
    try:
        return {str(k): plain(value[k]) for k in value.keys()}
    except (AttributeError, TypeError):
        pass
    if hasattr(value, "path"):
        return {"path": os.fspath(value.path), "source": getattr(value, "source", "explicit")}
    return repr(value)


def db_rows(path):
    if not os.path.isfile(path):
        return {}
    try:
        db = sqlite3.connect("file:" + path + "?mode=ro", uri=True)
        db.row_factory = sqlite3.Row
        tables = {}
        for (name,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' "
                                  "AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
            rows = [dict(row) for row in db.execute("SELECT * FROM " + name + " ORDER BY rowid")]
            if rows:
                tables[name] = plain(rows)
        db.close()
        return tables
    except sqlite3.DatabaseError:
        return {}


def find_db():
    for base, _dirs, files in os.walk(TREE):
        for name in files:
            if name.endswith(".sqlite3") and name != "not-a-store.sqlite3":
                return os.path.join(base, name)
    return None


def snapshot(number):
    target = os.path.join(ROOT, "pre-%03d" % number)
    shutil.copytree(TREE, target, symlinks=True)
    path = find_db()
    if path and os.path.isfile(path):
        relative = os.path.relpath(path, TREE)
        copied = os.path.join(target, relative)
        try:
            os.unlink(copied)
        except FileNotFoundError:
            pass
        source = sqlite3.connect(path)
        out = sqlite3.connect(copied)
        source.backup(out)
        out.close()
        source.close()
    return target


def main():
    from codex_session_relay import cli, omitted
    import tests.test_supervisor_omission_store as module

    operations = []
    sequence = [0]

    def reserve():
        number = sequence[0]
        sequence[0] += 1
        return number

    original_relay = module.relay
    from codex_session_relay.clock import SystemClock
    fixed_times = []
    original_iso = SystemClock.iso

    def fixed_iso(self):
        if fixed_times:
            return fixed_times[-1]
        return original_iso(self)

    SystemClock.iso = fixed_iso

    def relay(*argv):
        number = reserve()
        fixed_times.append(original_iso(SystemClock()))
        pre = snapshot(number)
        code, answer = original_relay(*argv)
        operations.append({"kind": "command", "args": plain(argv), "pre": pre,
                           "clock": fixed_times[-1], "code": code, "output": plain(answer),
                           "tables": db_rows(find_db() or "")})
        fixed_times.pop()
        return code, answer

    module.relay = relay
    original_derive = omitted.derive

    def derive(store, relationship_id, *args, **kwargs):
        pre = snapshot(reserve())
        answer = original_derive(store, relationship_id, *args, **kwargs)
        operations.append({"kind": "derive", "relationship": relationship_id,
                           "args": plain(args), "kwargs": plain(kwargs), "pre": pre,
                           "output": plain(answer),
                           "tables": db_rows(os.fspath(store.path))})
        return answer

    omitted.derive = derive
    from codex_session_relay import supervisorchannel
    original_stage_unsent = supervisorchannel.SupervisorChannel.stage_unsent

    def stage_unsent(self, project_key):
        pre = snapshot(reserve())
        answer = original_stage_unsent(self, project_key)
        operations.append({"kind": "supervisor_step", "project": project_key,
                           "at": self.clock.iso(), "pre": pre, "output": plain(answer),
                           "tables": db_rows(os.fspath(self.store.path))})
        return answer

    supervisorchannel.SupervisorChannel.stage_unsent = stage_unsent

    # Capture the send window itself, not only the staging nested inside a tick.
    # The archived pass withholds; the following pass must select and claim that
    # withheld row after the supervisor is unarchived.
    from codex_session_relay.daemon import RelayDaemon, TickReport
    supervisorchannel.secrets.token_hex = lambda n=None: "00" * (32 if n is None else n)
    original_send = RelayDaemon._send_upward

    def send_upward(self, channel, report, now):
        if not TEST.startswith("ASupervisorWhoCannotBeWoken"):
            return original_send(self, channel, report, now)
        pre = snapshot(reserve())
        captured = TickReport()
        archived = self.adapter.threads[module.SUPERVISOR].archived
        next_turn = self.adapter._turn_counter + 1
        answer = original_send(self, channel, captured, now)
        report.supervisorSent += captured.supervisorSent
        report.deferred += captured.deferred
        report.skipped += captured.skipped
        report.notes.extend(captured.notes)
        operations.append({"kind": "send_window", "pre": pre, "at": self.clock.iso(),
                           "kwargs": {"now": now, "archived": archived, "nextTurn": next_turn},
                           "output": {"supervisorStaged": 0,
                                      "supervisorSent": captured.supervisorSent,
                                      "deferred": captured.deferred, "skipped": captured.skipped,
                                      "notes": captured.notes},
                           "tables": db_rows(os.fspath(self.store.path))})
        return answer

    RelayDaemon._send_upward = send_upward
    suite = unittest.defaultTestLoader.loadTestsFromName(
        "tests.test_supervisor_omission_store." + TEST)
    result = unittest.TestResult()
    suite.run(result)
    problems = [tb for _case, tb in result.failures + result.errors]
    with open(os.path.join(ROOT, "capture.json"), "w") as handle:
        json.dump({"operations": operations, "problems": problems,
                   "tables": db_rows(find_db() or "")}, handle, sort_keys=True)
    if problems:
        print("\n".join(problems), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
