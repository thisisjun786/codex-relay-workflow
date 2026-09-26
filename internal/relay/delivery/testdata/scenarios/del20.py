_r, e = c.queued_event()
if sys.argv[3] == "resume":
    c.registry.set_status(c._rid, "paused", actor="user")
    c.attempt(e)
    rec = c.registry.get(c._rid)
    c.registry.resume(c._rid, expect_generation=rec["executionGeneration"], expect_artifact_roots=rec["authorizedScope"]["artifactRoots"], expect_allowed_recipients=rec["authorizedScope"]["allowedRecipients"], actor="user")
    c.clock.advance(c.delivery.policy.lifecycle_recheck_seconds + 1)
    out["record"] = c.attempt(e, now=c.clock.now())
else:
    out["record"] = c.attempt(e)
