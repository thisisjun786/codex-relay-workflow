from codex_session_relay.criteria import CriteriaService
crit = CriteriaService(c.store, c.clock)
SET = [{"id": " c2 ", "title": " malformed requests are refused ", "required": 0}, {"id": "c1", "title": "the endpoint returns the agreed shape"}]
SOURCE = "https://linear.app/doc/1"
m = sys.argv[3]
if m == "refused":
    out["r"] = [refusal(crit.ensure_registered, "rel-1", e, source_ref=SOURCE) for e in ([], [{"id": "c1", "title": "   "}], [{"id": "c1", "title": "one"}, "not-an-object"], [{"id": "c1", "title": "one"}, {"id": " c1 ", "title": "again"}])]
    out["mode"] = crit.mode("rel-1")
elif m == "first":
    out["r"] = crit.ensure_registered("rel-1", list(reversed(SET)), source_ref=SOURCE)
elif m == "replay":
    crit.ensure_registered("rel-1", SET, source_ref=SOURCE)
    c.clock.advance(30)
    out["r"] = crit.ensure_registered("rel-1", [{"id": "c2", "title": "malformed requests are refused", "required": False}, {"id": "c1", "title": " the endpoint returns the agreed shape "}], source_ref=SOURCE)
elif m == "changed":
    crit.ensure_registered("rel-1", SET, source_ref=SOURCE)
    out["r"] = [refusal(crit.ensure_registered, "rel-1", e, source_ref=s) for e, s in (
        ([{"id": "c1", "title": "the endpoint returns the agreed shape"}, {"id": "c2", "title": "a different refusal", "required": False}], SOURCE),
        (SET, "https://linear.app/doc/2"),
        ([{"id": "c2", "title": "malformed requests are refused", "required": True}, {"id": "c1", "title": "the endpoint returns the agreed shape"}], SOURCE),
        (SET + [{"id": "c3", "title": "one more obligation"}], SOURCE))]
elif m == "replace":
    crit.ensure_registered("rel-1", SET, source_ref=SOURCE)
    c.clock.advance(5)
    out["r"] = crit.register("rel-1", [{"id": "c9", "title": "the replacement obligation", "required": False}], source_ref="operator")
    out["r2"] = refusal(crit.ensure_registered, "rel-1", SET, source_ref=SOURCE)
else:
    from codex_session_relay.criteria import set_digest
    N = [{"id": "c1", "title": "the endpoint returns the agreed shape", "required": True}, {"id": "c2", "title": "malformed requests are refused", "required": False}]
    d, now = set_digest(N), c.clock.iso()
    with c.store.transaction() as db:
        if m == "corrupt":
            db.execute("INSERT INTO canonical_criteria VALUES (?,?,?,?,?,?,?)", ("rel-1", "c1", N[0]["title"], 1, SOURCE, d, now))
            db.execute("INSERT INTO canonical_criteria VALUES (?,?,?,?,?,?,?)", ("rel-1", "c2", N[1]["title"], 0, "other-source", "not-the-digest", now))
            db.execute("INSERT INTO verification_mode VALUES (?,?,?)", ("rel-1", "legacy", now))
        else:
            db.execute("INSERT INTO canonical_criteria VALUES (?,?,?,?,?,?,?)", ("rel-1", "c1", N[0]["title"], 2, SOURCE, d, now))
            db.execute("INSERT INTO canonical_criteria VALUES (?,?,?,?,?,?,?)", ("rel-1", "c2", N[1]["title"], 0, SOURCE, d, now))
            db.execute("INSERT INTO verification_mode VALUES (?,?,?)", ("rel-1", "managed", now))
    out["r"] = refusal(crit.ensure_registered, "rel-1", SET, source_ref=SOURCE)
