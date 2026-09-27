"""Capture SR-22's observable completion messages through Python production rendering.

argv: <sqlite snapshot>. The baseline fixture has a relationship and project scope. The
other cases remove only the project scope or present a delivery row naming an absent relation.
"""
import json
import sqlite3
import sys
from pathlib import Path

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
