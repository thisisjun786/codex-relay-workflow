"""Replay original RES/RCF unittest methods; preserve every output and raw SQLite cell.

Snapshots are operation inputs, never expected rows copied over a Go result. Nested calls
are replayed through their public outer operation, so a missing write cannot be hidden.
"""
import functools
import importlib
import json
from pathlib import Path
import shutil
import sqlite3
import sys
import tempfile
import unittest

from typing import Any

ROOT = Path(sys.argv[1])
MODULE, METHOD = sys.argv[2:4]
TREE = ROOT / "tree"
TREE.mkdir()
tempfile.mkdtemp = lambda *a, **k: str(TREE)
real_rmtree = shutil.rmtree
shutil.rmtree = lambda path, *a, **k: None if Path(path) == TREE else real_rmtree(path, *a, **k)
operations = []
active = False
depth = 0
# The class is selected by name from the original test module at runtime.
case: Any


def plain(v):
    if v is None or isinstance(v, (str, int, float, bool)):
        return v
    if isinstance(v, dict):
        return {str(k): plain(x) for k, x in v.items()}
    if isinstance(v, (list, tuple)):
        return [plain(x) for x in v]
    if hasattr(v, "keys"):
        return {k: plain(v[k]) for k in v.keys()}
    raise TypeError(type(v))


def encoded(v):
    return json.dumps(plain(v), sort_keys=True, indent=2) + "\n"


def tables():
    # Include empty tables and metadata. Embedded JSON remains text, byte for byte.
    db = case.store.db
    return {name: [dict(row) for row in db.execute(f'SELECT * FROM "{name}" ORDER BY rowid')]
            for name, in db.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")}


def capture(owner, name, kind, arguments):
    original = getattr(owner, name)
    @functools.wraps(original)
    def wrapped(*a, **k):
        global depth
        if not active or depth:
            return original(*a, **k)
        n = len(operations)
        pre = ROOT / f"pre-{n}.sqlite3"
        dst = sqlite3.connect(pre)
        case.store.db.backup(dst)
        dst.close()
        args = arguments(a, k)
        op = {"kind": kind, "args": plain(args), "now": case.clock.now(), "pre": str(pre)}
        operations.append(op)
        depth += 1
        try:
            value = original(*a, **k)
            output = {"value": plain(value)}
        except Exception as error:
            from codex_session_relay.errors import RelayError
            from codex_session_relay import cli
            if isinstance(error, RelayError):
                assert error.reason is not None
                output = {"error": "refused", "reason": error.reason.value, "detail": error.detail}
            elif isinstance(error, cli.SystemExit2):
                output = {"error": "usage", "detail": str(error)}
            else:
                raise
            op["output"] = (json.dumps(output, indent=2) + "\n") if kind.startswith("cli-") else encoded(output)
            raise
        finally:
            depth -= 1
            op["tables"] = encoded(tables())
        op["output"] = (json.dumps(plain(value), indent=2) + "\n") if kind in ("cli-verdict", "show") else encoded(output)
        return value
    setattr(owner, name, wrapped)


def main():
    global case, active
    from codex_session_relay import ack, cli, delivery, report
    from codex_session_relay.clock import SystemClock
    SystemClock.iso = lambda self: "2023-11-14T22:13:25.000000+00:00"
    module = importlib.import_module("tests." + MODULE)
    cls, method = METHOD.split(".")
    case = getattr(module, cls)(method)
    acknowledge = ack.AckService.acknowledge
    def acknowledged(*a, **k):
        global active
        out = acknowledge(*a, **k)
        active = True
        return out
    ack.AckService.acknowledge = acknowledged
    capture(ack.AckService, "record_verdict", "verdict", lambda a,k: {"event":a[1], **k})
    capture(ack.AckService, "restoration_of", "restoration", lambda a,k: {"event":a[1]})
    capture(delivery.DeliveryService, "render_message", "render", lambda a,k: {"event":a[1], **k})
    capture(delivery.DeliveryService, "preview_message", "preview", lambda a,k: {"event":a[1], **k})
    capture(delivery.DeliveryService, "attempt", "attempt", lambda a,k: {"event":a[1], **k})
    capture(report, "record", "report", lambda a,k: {**k, "budget":report.BUDGET,
            "settleFirst": METHOD.endswith("test_the_projection_is_measured_inside_the_write_lock")})
    capture(report, "read", "read", lambda a,k: {"event":a[1]})
    capture(report, "render_revision", "tight", lambda a,k: {"event":a[0]["event_id"], "request":a[2], **k})
    capture(cli, "cmd_verdict", "cli-verdict", lambda a,k: vars(a[1]))
    capture(cli, "cmd_show", "show", lambda a,k: vars(a[1]))
    if METHOD.endswith(("test_a_note_supplied_through_the_other_input_still_counts",
                        "test_a_declaration_in_one_input_is_not_erased_by_the_other",
                        "test_repeating_the_same_declaration_is_not_a_contradiction")):
        original_method = getattr(case, method)
        def merged_carrier_sent():
            original_method()
            row = case.store.one("SELECT event_id FROM deliveries WHERE kind='revision_request'")
            assert row is not None
            case.attempt(row["event_id"])
        setattr(case, method, merged_carrier_sent)
    result = unittest.TestResult()
    case.run(result)
    problems = [text for _, text in result.errors + result.failures]
    (ROOT / "capture.json").write_text(json.dumps({"operations":operations, "problems":problems}))
    if problems:
        print("\n".join(problems), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
