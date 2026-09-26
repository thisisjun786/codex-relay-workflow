_r, e = c.queued_event()
c.adapter.script("in_progress")
record = c.attempt(e)
turn = c.adapter.start_turn(PARENT, status="inProgress")
c.adapter.ledger[record["requestId"]] = {"requestId": record["requestId"], "status": "accepted", "resumed": {"approvalPolicy": "never"}, "turnId": turn.turn_id}
out["record"] = record
out["reconciled"] = c.reconciler.reconcile_attempt(record["requestId"], c.adapter)
