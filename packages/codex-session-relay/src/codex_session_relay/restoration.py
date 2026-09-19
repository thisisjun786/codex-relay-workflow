"""Whether the restoration block a correction carries can actually reach the child.

A needs-changes verdict is the only correction channel this workflow allows, so the context a
compacted child needs in order to resume travels inside that verdict's own findings. Three
different mechanisms can take it back out on the way to the child - the legacy renderer's
finding cap, the composer's byte budget, and simply never having carried one - and none of
them used to leave a name behind. Nine findings delivered whole and ten findings with the
tenth cut read identically, to the recipient and to this package's own record.

So the three are separated here and each one is given a name. A truncation is a result, not
the absence of one, and "no block was declared" is a different fact from "a block was
declared and did not fit".

What is named here is what the relay will put IN THE BYTES of the message it renders. That is
not a claim about what the child read. Delivery, acknowledgement and reading are established
elsewhere and separately, and a rendering outcome stands for none of them.
"""

# The key a finding sets to declare that it carries the block. Declared, never inferred: a
# heuristic over note text would make this package guess which finding mattered, and a guess
# is what it refuses everywhere else. verification-verdict.json freezes additionalProperties
# on the verdict record but NOT on a criteria item, so this key rides inside the frozen
# contract and needs no schema change and no store migration.
FIELD = "restoration"

CARRIED = "carried"
TRUNCATED = "truncated"
BUDGET_DROPPED = "budget_dropped"
NOT_CARRIED = "not_carried"
UNMEASURED = "unmeasured"
OUTCOMES = (CARRIED, TRUNCATED, BUDGET_DROPPED, NOT_CARRIED, UNMEASURED)

# A block WAS carried and will not arrive. Both callers refuse on these, and neither should
# have to re-derive which outcomes those are.
UNDELIVERABLE = (TRUNCATED, BUDGET_DROPPED)

# Which renderer an outcome was measured against. A projection made before a work report
# exists was measured against a different renderer from the one that runs once one has been
# recorded, and saying which is the whole difference between a measurement and an assumption.
LEGACY_BASIS = "relay-message/legacy"
COMPOSED_BASIS = "relay-report/1"


def declared_index(findings):
    """Where the declared block sits and what it is; (None, None) when none was declared.

    Position matters to every caller here, because both mechanisms that remove a block remove
    it from the END of the list, so where it sits IS whether it survives.
    """
    for index, finding in enumerate(findings or []):
        if finding.get(FIELD):
            return index, finding
    return None, None


def declared(findings):
    return declared_index(findings)[1]


def result(outcome, *, basis, criterion=None, detail="") -> dict:
    """One shape, so every surface reports the same keys and a reader learns them once."""
    if outcome not in OUTCOMES:
        raise ValueError(f"{outcome!r} is not one of {OUTCOMES}")
    return {"outcome": outcome, "basis": basis, "criterion": criterion, "detail": detail}


def not_carried(*, basis, detail) -> dict:
    return result(NOT_CARRIED, basis=basis, detail=detail)


def unmeasured(detail, *, basis=None) -> dict:
    """Deliverability could not be established here. Never a synonym for delivered."""
    return result(UNMEASURED, basis=basis, detail=detail)


def project_cap(findings, *, cap, basis=LEGACY_BASIS) -> dict:
    """For a renderer that shows the first cap findings and nothing after them.

    Arithmetic over the list rather than a rendering, which is what lets this run BEFORE the
    transaction that opens the next execution generation. A projection that needed the message
    could only run after the generation had already advanced, which is the position this
    package is trying to get out of.
    """
    index, block = declared_index(findings)
    if block is None:
        return not_carried(basis=basis, detail="no finding declared a restoration block")
    where = f"finding {index + 1} of {len(findings or [])}"
    if index < cap:
        return result(CARRIED, basis=basis, criterion=block["id"],
                      detail=f"{where}, within the {cap} this renderer shows")
    return result(TRUNCATED, basis=basis, criterion=block["id"],
                  detail=f"{where}, past the {cap} this renderer shows")


def project_survivors(findings, survivors, *, basis=COMPOSED_BASIS,
                      cause=BUDGET_DROPPED) -> dict:
    """For a renderer that reports which findings its own shortening left standing."""
    index, block = declared_index(findings)
    if block is None:
        return not_carried(basis=basis, detail="no finding declared a restoration block")
    where = f"finding {index + 1} of {len(findings or [])}"
    if block["id"] in set(survivors or ()):
        return result(CARRIED, basis=basis, criterion=block["id"],
                      detail=f"{where}, still present after the message was fitted")
    return result(cause, basis=basis, criterion=block["id"],
                  detail=f"{where}, removed while fitting the message to its byte budget")


def label(finding) -> str:
    """How a carried block is marked in the rendered bytes.

    Inline on the finding's own first line, never on a line of its own. A separate line can be
    shortened away from underneath the finding it belongs to, which would leave the bytes
    disagreeing with the record about the one thing both exist to report.
    """
    return " [restoration block]" if finding.get(FIELD) else ""


def overflow_detail(hidden) -> str:
    """What a cap took, named when it took the block with it.

    "5 more" and "5 more, one of which was the restoration block" are different news for a
    recipient deciding whether it still knows what it was doing.
    """
    block = declared(hidden)
    return f", including the restoration block on {block['id']}" if block else ""
