"""Print the complete SQLite query-plan detail for the managed creation lookup."""
import json
import sqlite3
import sys

with sqlite3.connect(sys.argv[1]) as db:
    rows = db.execute(
        "EXPLAIN QUERY PLAN SELECT seq, detail FROM journal WHERE subject = ? "
        "AND CASE WHEN kind = 'managed_start_observed' AND json_valid(detail) "
        "THEN json_extract(detail, '$.stage') = 'creation' END "
        "ORDER BY seq DESC LIMIT 1",
        ("managed-1",),
    )
    print(json.dumps([row[3] for row in rows]))
