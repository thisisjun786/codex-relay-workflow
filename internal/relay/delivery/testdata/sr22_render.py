"""Capture SR-22's observable completion messages through Python production rendering.

argv: <sqlite snapshot> [<fixture tree>]. The baseline fixture has a relationship and project
scope. The other cases remove only the project scope or present a delivery row naming an absent
relation. A fixture tree, when given, is the fixture's directory instead of a random temporary one,
so the artifact paths (and the revision and event id derived from them) are the same on every run.
"""
import json
import sqlite3
import sys
import tempfile
from pathlib import Path

if len(sys.argv) > 2:
    _real_mkdtemp = tempfile.mkdtemp

    def _fixture_mkdtemp(*args, **kwargs):
        caller = sys._getframe(1).f_globals.get("__name__", "")
        if caller == "__main__" or caller.split(".")[0] == "tests":
            return sys.argv[2]
        return _real_mkdtemp(*args, **kwargs)

    tempfile.mkdtemp = _fixture_mkdtemp

from codex_session_relay import report
from tests.support import DeliveryTestCase


def main():
    case = DeliveryTestCase()
    case.setUp()
    try:
        _relationship, event_id = case.queued_event()
        stored = report.record(
            case.store,
            case.clock,
            event_id=event_id,
            repository="repo/project",
            cxc_status="DONE",
            cxc_reason="proved",
            summary="the work is done",
            next_action="review",
        )
        row = case.delivery.get(event_id)
        receipt = case.intake.get(event_id)
        request = "del-sr22-a1"

        def render(current):
            return report.render_completion(
                current,
                receipt,
                request,
                stored,
                context=case.delivery.envelope_context(current),
            )

        case.store.db.execute(
            "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,?)",
            (row["relationship_id"], "PRJ-1", case.clock.iso()),
        )
        messages = {"project": render(row)}
        case.store.db.execute(
            "DELETE FROM relationship_scope WHERE relationship_id = ?",
            (row["relationship_id"],),
        )
        messages["issue_only"] = render(row)
        absent = dict(row, relationship_id="rel-0000000000000000")
        messages["absent_relationship"] = render(absent)

        destination = sqlite3.connect(Path(sys.argv[1]))
        case.store.db.backup(destination)
        destination.close()
        print(json.dumps({"eventId": event_id, "requestId": request, "messages": messages}))
    finally:
        case.doCleanups()


if __name__ == "__main__":
    main()
