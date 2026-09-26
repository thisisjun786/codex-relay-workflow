rel = c.register()
p = c.ready_payload(rel, [c.artifact("out.txt", "finished after several turns")], turn=c.assigned_turn("completed", turn="turn-loop-3"))
obs = c.assigned_turn("completed", turn="turn-loop-3")
mode = sys.argv[3]
if mode == "none":
    out["result"] = refusal(c.accept, p)
elif mode == "valid":
    out["result"] = refusal(c.intake.accept_child_receipt, p, observation=obs, continuation={"anchorTurnId": "turn-dispatch-1", "actor": "child-loop", "reason": "PABCD cycle 3 completed this generation"})
elif mode == "anchor":
    out["result"] = refusal(c.intake.accept_child_receipt, p, observation=obs, continuation={"anchorTurnId": "some-other-execution", "actor": "a", "reason": "b"})
elif mode == "thread":
    p = c.ready_payload(rel, [c.artifact("out.txt", "payload")], turn=c.assigned_turn("completed", thread="someone-else", turn="turn-loop-3"))
    out["result"] = refusal(c.intake.accept_child_receipt, p, observation=c.assigned_turn("completed", thread="someone-else", turn="turn-loop-3"), continuation={"anchorTurnId": "turn-dispatch-1", "actor": "a", "reason": "b"})
elif mode == "malformed":
    out["result"] = refusal(c.intake.accept_child_receipt, p, observation=obs, continuation={"anchorTurnId": "turn-dispatch-1"})
else:
    c.intake.accept_child_receipt(p, observation=obs, continuation={"anchorTurnId": "turn-dispatch-1", "actor": "a", "reason": "b"})
    out["result"] = refusal(c.intake.accept_child_receipt, p, observation=obs)
