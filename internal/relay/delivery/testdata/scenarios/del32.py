import json
_r, e = c.queued_event()
row = c.store.one("SELECT receipt FROM events WHERE event_id = ?", (e,))
receipt = json.loads(row["receipt"])
receipt["criteria"] = [{"id": "c1", "verdict": "verified", "restoration": "false"}, {"id": "c2", "verdict": "verified", "restoration": True}]
c.store.db.execute("UPDATE events SET receipt = ? WHERE event_id = ?", (json.dumps(receipt), e))
out["message"] = c.delivery.preview_message(e)
