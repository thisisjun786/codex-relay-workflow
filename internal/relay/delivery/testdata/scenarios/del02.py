rel = c.register()
p = c.ready_payload(rel, [c.artifact("out.txt", "still working")], turn=c.assigned_turn("inProgress"))
c.accept(p)
out["refused"] = refusal(c.delivery.enqueue, p["eventId"])
