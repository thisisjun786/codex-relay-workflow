exec(open(os.path.join(os.path.dirname(__file__), "scenarios", "_vcu.py")).read())
m = sys.argv[3]
if m == "advanced":
    e = acknowledged(); advance(); out["r"] = verdict(e, "verified")
elif m == "ambiguous":
    e = acknowledged(); second(); out["r"] = verdict(e, "verified")
elif m == "superseded":
    e = acknowledged(); supersede(e, "the corrected revision", c.intake.row(e)["revision_hash"]); out["r"] = verdict(e, "verified")
elif m == "paused":
    e = acknowledged(); c.registry.set_status(c._rid, "paused", actor=PARENT)
    out["r"] = [verdict(e, v, reason="stopping") for v in ("verified", "needs_changes", "unverified", "aborted")]
elif m == "nc_stale":
    e = acknowledged(); advance(); out["r"] = verdict(e, "needs_changes", findings=[{"id": "c1", "verdict": "needs_changes", "note": "still wrong"}])
elif m == "unv_stale":
    e = acknowledged(); advance(); out["r"] = verdict(e, "unverified", reason="could not reach it")
elif m == "replay":
    e = acknowledged()
    out["first"] = verdict(e, "needs_changes", findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix it"}])
    out["r"] = verdict(e, "verified", turn="v2")
elif m == "sole":
    acknowledged(); out["r"] = head()
elif m == "chain":
    e = acknowledged(); supersede(e, "second revision", c.intake.row(e)["revision_hash"]); out["r"] = head()
elif m == "fork_undeclared":
    acknowledged(); second(); out["r"] = head()
elif m == "fork_shared":
    e = acknowledged(); h = c.intake.row(e)["revision_hash"]
    supersede(e, "branch one", h); supersede(e, "branch two", h); out["r"] = head()
elif m == "unknown_pred":
    e = acknowledged(); supersede(e, "claims to replace something we never saw", "f" * 64); out["r"] = head()
elif m == "cycle":
    e = acknowledged(); s = supersede(e, "second revision", c.intake.row(e)["revision_hash"])
    with c.store.transaction() as db:
        db.execute("UPDATE revision_lineage SET supersedes_hash = ? WHERE event_id = ?", (s["revisionHash"], e))
    out["r"] = head()
elif m == "reversed":
    acknowledged(text="a different revision"); second(text="the deliverable"); out["r"] = head()
elif m == "self":
    rel = c.register(); c._rid = rel["relationshipId"]
    p = c.ready_payload(rel, [c.artifact("out.txt", "self referential")])
    out["r"] = refusal(c.intake.accept_child_receipt, p, observation=c.assigned_turn(), supersedes_revision=p["revisionHash"])
elif m == "managed_none":
    e = acknowledged(); crit.set_mode(c._rid, "managed"); out["r"] = verdict(e, "verified")
elif m == "legacy":
    e = acknowledged(); out["r"] = verdict(e, "verified")
else:
    e = acknowledged()
    if m == "no_claim" or m == "explicit":
        reg = register_criteria()
    else:
        register_criteria(); c.ack.claim_verification(e, turn_id="ack-turn")
    both = [{"id": "c1", "verdict": "verified"}, {"id": "c2", "verdict": "verified"}]
    if m == "missing": out["r"] = verdict(e, "verified", findings=[{"id": "c1", "verdict": "verified"}])
    elif m == "covered": out["r"] = verdict(e, "verified", findings=both)
    elif m == "no_note": out["r"] = verdict(e, "needs_changes", findings=[{"id": "c1", "verdict": "needs_changes"}])
    elif m == "unknown_id": out["r"] = verdict(e, "verified", findings=[{"id": "nope", "verdict": "verified"}])
    elif m == "bad_disposition": out["r"] = verdict(e, "verified", findings=[{"id": "c1", "verdict": "regressed"}])
    elif m == "edited":
        register_criteria([{"id": "c1", "title": "the endpoint returns a COMPLETELY different shape"}, {"id": "c2", "title": "a malformed request is refused"}])
        out["r"] = verdict(e, "verified", findings=both)
    elif m == "wrong_digest": out["r"] = verdict(e, "verified", findings=both, expect_criteria_digest="0" * 64)
    elif m == "no_claim": out["r"] = verdict(e, "verified", findings=both)
    elif m == "explicit": out["r"] = verdict(e, "verified", findings=both, expect_criteria_digest=reg["setDigest"])
