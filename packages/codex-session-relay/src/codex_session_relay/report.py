"""What the recipient actually needs, put where they will read it first.

The delivered message used to open with an event id, a relationship id, a generation and a
revision hash, and then list file paths and their digests. All of that is true and none of
it tells a parent which pull request to open, what was checked, what is still unresolved,
or what to do next. A recipient had to go and reconstruct the work before it could act.

A work report is the part contract v1 has no room for. The five schemas are frozen with
additionalProperties false, so a pull request cannot be added to a completion receipt; this
is a relay-owned record, like the criteria set and the revision request, bound to the exact
event, relationship, generation and revision it describes. A repository slug is stored
beside the pull request number and the two are never rendered apart, so the same number on
two projects stays two different pull requests.

An event with no work report renders exactly what it rendered before. That is not a
fallback bolted on for safety: a receipt from before this contract has no pull request to
centre a report on, and inventing one would be the same guess this package refuses
everywhere else.
"""

import json
from datetime import datetime

from . import cxc, mergeevidence, restoration
from .errors import DeliveryRefused, ReceiptRefused, RefusalReason
from .identity import request_id as derive_request_id
from .transport import (
    DEFERRED_BUSY, HELD_UNCERTAIN, INBOX_ONLY, QUEUED, SENDING, WITHHELD_PRE_SEND,
)

NEWLINE = chr(10)
VERSION = "relay-report/1"
LEGACY = "relay-message/legacy"
REVISION_OUTCOME = "revision_request"
READY_OUTCOME = "ready_for_review"

# UTF-8 BYTES, not characters. A transport limit is a byte limit, and a report written in
# Korean costs roughly three bytes per character, so measuring characters would let exactly
# the messages this workflow actually sends overrun a budget that looked comfortable.
# Generous enough that an ordinary report is never touched, small enough that a runaway one
# is elided here, on purpose and in the open, rather than cut by whatever reads it later.
BUDGET = 6000

# Per-field byte ceilings, enforced when a report is RECORDED. The composer can drop an
# optional section and shorten a list, but it cannot shorten a single required line without
# losing the thing that line exists to say. An oversized summary therefore used to pass
# record() and then fail every render, and because rendering happens inside the delivery
# claim, every claim rolled back and the delivery never went out. Refusing here keeps the
# failure where the caller can still fix it.
SUMMARY_MAX = 1200
ACTION_MAX = 1200
REASON_MAX = 600
# The short identifying fields. Each one lands on a line the composer cannot shorten, so an
# unbounded value there is the same undeliverable report by a different route.
LABEL_MAX = 300
# A url sits on a line the composer CAN drop, so it only needs a storage ceiling rather than
# a fits-on-one-line ceiling. Bounding it like a label rejected real forge urls that would
# have rendered perfectly well.
URL_MAX = 2000
# A manifestRef arrives from the receipt, where the frozen contract sets no length. It lands
# on an essential line now, so it is given a bounded REPRESENTATION here rather than a limit
# imposed on a contract field this layer does not own.
REF_SHOWN = 240
# Acceptance confirmations cannot be elided, because dropping one hides a decision the parent
# is credited with and never made. Something unelidable needs an aggregate ceiling or it
# becomes an undeliverable report instead: four acceptances with individually legal 300-byte
# fields render past the whole budget, and rendering happens inside the delivery claim, so the
# claim rolls back unsent every time and the report is stored forever and delivered never.
# Bounding the fields one at a time cannot catch that, since each one is fine and the sum is
# not. A candidate whose confirmations do not fit the message the parent reads is a candidate
# carrying too many accepted defects to hand over in one piece.
ACCEPTANCE_SHOWN = 2400
# SQLite stores a signed 64-bit integer and raises OverflowError above it.
SQLITE_MAX_INT = 2 ** 63 - 1
# Wider than any real exit status or signal, and far inside what can be serialised.
EXIT_CODE_MAX = 2 ** 31


def _size(text) -> int:
    return len(text.encode("utf-8"))


def show_command(event_id: str) -> str:
    return f"codex-session-relay show --event {event_id}"


# ------------------------------------------------------------------------ recording

def record(store, clock, *, event_id, repository, cxc_status, cxc_reason, summary, next_action,
           pr_number=None, pr_url=None, pr_state=None, base_ref=None, base_sha=None,
           head_sha=None, criteria_digest=None, evidence=None, unresolved=None, review=None,
           restore=None, submission_no=1, handoff=None) -> dict:
    """Store one report, validated, bound to the revision it describes.

    Identity is READ from the stored event, never accepted from the caller. Taking the
    relationship, generation, revision and outcome as arguments meant a mistyped or stale
    caller could file a report whose metadata described a different execution entirely, and
    delivery would then hand the recipient another pull request and another instruction. The
    event row is the only thing that knows what this event is.

    The CXC status is then checked against the outcome that event actually carries. It is not
    allowed to choose that outcome: the receipt is the only thing the frozen contract lets
    assert one, and a status that picked it could quietly overrule the evidence.
    """
    event = store.one("SELECT * FROM events WHERE event_id = ?", (event_id,))
    if event is None:
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"no event {event_id!r} is recorded, so there is nothing for this report to be "
            "about; a report is written against an accepted event, not ahead of one",
        )
    relationship_id = event["relationship_id"]
    execution_generation = event["execution_generation"]
    revision_hash = event["revision_hash"]
    outcome = event["outcome"]
    if outcome == REVISION_OUTCOME:
        # The parent-to-child direction carries no child receipt, so there is no asserted
        # outcome to pair the status with. Demanding one would make the caller invent it.
        cxc.check_known(cxc_status)
    else:
        cxc.check_status(cxc_status, outcome)
    # Shape before meaning. Reading review.get() ahead of _check_review assumed every truthy
    # review was a mapping, so a revision report carrying a bare string raised AttributeError
    # out of the validator instead of coming back as a named refusal.
    review = _check_review(review)
    if outcome == REVISION_OUTCOME:
        if (review or {}).get("kind") == cxc.PASS:
            # A revision event exists because the parent ruled needs_changes. A PASS verdict
            # on it would tell the child its work was approved in the same message that
            # demands changes, and the review half of a report is caller-supplied even now
            # that identity is not.
            raise ReceiptRefused(
                RefusalReason.DISPOSITION_CONFLICT,
                "a revision request cannot carry a PASS verdict: this event exists because "
                "the parent ruled needs_changes, and the message would approve and demand "
                "changes at the same time",
            )
    elif review is not None:
        # Only a correction renders a verdict line. Storing a review on a completion would
        # accept a PASS or FAIL that the delivered message never shows and never says it
        # dropped, which is the silent loss the omission notice exists to prevent.
        raise ReceiptRefused(
            RefusalReason.DISPOSITION_CONFLICT,
            f"a {outcome!r} report carries no review: a verdict line belongs to a correction, "
            "so this judgment would be stored and never delivered. Put the reviewers findings "
            "in the unresolved items, or record the review on the revision request",
        )
    repository = _bounded(_required(repository, "repository"), "repository", 200)
    summary = _bounded(_required(summary, "summary"), "summary", SUMMARY_MAX)
    next_action = _bounded(_required(next_action, "next_action"), "next_action", ACTION_MAX)
    reason = _bounded(_required(cxc_reason, "cxc_reason"), "cxc_reason", REASON_MAX)
    pr_state = _bounded_optional(pr_state, "pr_state")
    pr_url = _bounded_optional(pr_url, "pr_url", URL_MAX)
    base_ref = _bounded_optional(base_ref, "base_ref")
    base_sha = _bounded_optional(base_sha, "base_sha")
    head_sha = _bounded_optional(head_sha, "head_sha")
    criteria_digest = _bounded_optional(criteria_digest, "criteria_digest")
    if pr_number is not None:
        if isinstance(pr_number, bool) or not isinstance(pr_number, int) or pr_number < 1:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                # The value is named by TYPE, never printed. Past the integer-to-string
                # digit limit, formatting it raises ValueError out of the refusal itself,
                # and a huge negative number reaches this branch.
                "a pull request number is a positive integer the store can hold, not a "
                f"{type(pr_number).__name__} outside that range",
            )
        if pr_number > SQLITE_MAX_INT:
            # Past this the insert raises OverflowError, a host exception escaping the
            # refusal path rather than a producer being told what it got wrong.
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                # The value is deliberately not interpolated: past Python integer-to-string
                # digit limit, rendering it raises ValueError out of the refusal itself.
                "a pull request number is outside what the store can hold",
            )
        if not head_sha:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a report naming a pull request names the head commit it is about; without "
                "one, a later push silently inherits this report",
            )
    else:
        # Without a number there is no pull request line to hang these on, and the no-PR
        # branch renders neither, so accepting them would store values nobody ever sees and
        # say nothing about having dropped them.
        supplied = [
            name for name, value in (("pr_url", pr_url), ("pr_state", pr_state)) if value
        ]
        if supplied:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"{' and '.join(supplied)} describes a pull request, but none is named. "
                "Give the pull request number, or leave these out",
            )
    evidence = _check_evidence(evidence)
    unresolved = _check_unresolved(unresolved)
    restore = _check_restore(restore)
    submission_no = _submission(submission_no)
    handoff = _check_handoff(handoff, pr_number, head_sha, base_sha, outcome)
    row = {
        "eventId": event_id,
        "relationshipId": relationship_id,
        "executionGeneration": int(execution_generation),
        "revisionHash": revision_hash,
        "submissionNo": submission_no,
        "repository": repository,
        "prNumber": pr_number,
        "prUrl": pr_url,
        "prState": pr_state,
        "baseRef": base_ref,
        "baseSha": base_sha,
        "headSha": head_sha,
        "criteriaDigest": criteria_digest,
        "cxcStatus": cxc_status,
        "cxcReason": reason,
        "contractVersion": cxc.VERSION,
        "summary": summary,
        "evidence": list(evidence or []),
        "unresolved": list(unresolved or []),
        "nextAction": next_action,
        "review": review,
        "restore": dict(restore or {}),
    }
    now = clock.iso()
    with store.transaction() as db:
        # Re-read inside the write lock. Two recorders can both pass a preflight check and
        # both claim the same next submission, and a delivery can open an attempt between a
        # preflight read and this commit. Everything this decides can move, which is why the
        # rest of this package reads inside the caller transaction rather than before it.
        _assert_resubmission(db, event_id, row["submissionNo"])
        # Measured INSIDE the write lock, for the reason the comment above gives: the attempt
        # count this projection sizes itself against can move. A delivery that claims and
        # settles a retry-safe attempt between a preflight read and this commit leaves the
        # projection describing an attempt already consumed, and at a request-id digit
        # boundary - projecting a9 while the send renders a10 - a report resting on the byte
        # budget is recorded as carrying a block the real message drops.
        #
        # This is also the last moment at which refusing costs nothing. Recording a report is
        # exactly what switches this event from the plain renderer to the composer and its
        # byte budget, so a block that survived the verdict-time projection can still be
        # squeezed out here, and nothing has frozen any bytes yet. Raising rolls this
        # transaction back with nothing written.
        projection = _project_restoration(store, event, row)
        if projection is not None and projection["outcome"] in restoration.UNDELIVERABLE:
            raise ReceiptRefused(
                RefusalReason.RESTORATION_UNDELIVERABLE,
                f"this report would push the restoration block on "
                f"{projection['criterion']!r} out of the correction: {projection['detail']}. "
                "Shorten the report or move the block and record it again. Afterwards there "
                "is no supported way to send the block: the verdict does not resend and a "
                "second channel is not allowed",
            )
        # Delete then insert, rather than an upsert with a conflict target. A conflict target
        # has to match a constraint in the schema the database was actually created with, and
        # CREATE TABLE IF NOT EXISTS never changes an existing one, so naming one here made
        # the write depend on which version of this table a store happened to start life on.
        db.execute(
            "DELETE FROM work_reports WHERE event_id = ? AND submission_no = ?",
            (event_id, row["submissionNo"]),
        )
        db.execute(
            "INSERT INTO work_reports (event_id, relationship_id, execution_generation,"
            " revision_hash, submission_no, repository, pr_number, pr_url, pr_state, base_ref,"
            " base_sha, head_sha, criteria_digest, cxc_status, cxc_reason, contract_version,"
            " summary, evidence, unresolved, next_action, review, restore, recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                event_id, relationship_id, row["executionGeneration"], revision_hash,
                row["submissionNo"], repository, pr_number, pr_url, pr_state, base_ref,
                base_sha, head_sha, criteria_digest, cxc_status, reason, cxc.VERSION, summary,
                json.dumps(row["evidence"]), json.dumps(row["unresolved"]), next_action,
                json.dumps(review) if review else None, json.dumps(row["restore"]), now,
            ),
        )
        store.journal(
            "work_report_recorded", event_id,
            {"repository": repository, "prNumber": pr_number, "cxcStatus": cxc_status,
             "headSha": head_sha, "submissionNo": row["submissionNo"]},
            at=now,
        )
        # Unconditional, for the same reason the work_reports delete is: a resubmission that
        # carries no handoff must not inherit the previous one. Leaving the old row behind
        # would let a report that says nothing about its review be read back as though it had
        # said what the earlier one did.
        db.execute(
            "DELETE FROM work_report_handoffs WHERE event_id = ? AND submission_no = ?",
            (event_id, row["submissionNo"]),
        )
        if handoff is not None:
            db.execute(
                "INSERT INTO work_report_handoffs (event_id, submission_no, is_draft,"
                " base_verified_at, required_declared, checks, review_coverage,"
                " thread_dispositions, criterion_evidence, limitations, recorded_at)"
                " VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                (
                    event_id, row["submissionNo"], 1 if handoff["isDraft"] else 0,
                    handoff["baseVerifiedAt"], json.dumps(handoff["requiredDeclared"]),
                    json.dumps(handoff["checks"]), json.dumps(handoff["reviewCoverage"]),
                    json.dumps(handoff["threadDispositions"]),
                    json.dumps(handoff["criterionEvidence"]),
                    json.dumps(handoff["limitations"]), now,
                ),
            )
            row["handoff"] = handoff
            store.journal(
                "handoff_recorded", event_id,
                {"headSha": head_sha, "prNumber": pr_number,
                 "threadsSeen": handoff["reviewCoverage"]["totalCount"],
                 "submissionNo": row["submissionNo"]},
                at=now,
            )
        if projection is not None:
            store.journal("restoration_rendered", event_id, projection, at=now)
    row["recordedAt"] = now
    if projection is not None:
        row["restoration"] = projection
    return row


# The delivery states another attempt can still be claimed from, INCLUDING the two that are
# merely unresolved. A delivery in sending has frozen one attempt's bytes without the
# transport having answered, and held_uncertain means reconciliation has not established
# whether that attempt reached anybody; a confirmed pre-send rejection is retry-safe and
# returns either one to a claimable state. Treating them as finished would let a report
# commit that the retry then renders with the block dropped, after the generation has already
# opened. Erring toward measuring is the cheap direction: its cost is a refusal the
# coordinator can act on, and the other mistake's cost is a correction sent without its block.
_STILL_ATTEMPTABLE = (
    QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND, SENDING, HELD_UNCERTAIN,
)


def _delivery_of(store, event_id):
    """The delivery row this report would be rendered for, or None.

    Read for the state, the hold and the attempt count in one statement, because all three
    are answers about the same row and reading them apart invites them to disagree. The hold
    matters as much as the state: the claim predicate requires hold_reason IS NULL, so a
    delivery parked at its attempt cap keeps a claimable-looking state while being unable to
    produce another message.

    The last three columns are the half of _claim's predicate that no later event can make
    true again. A superseded relationship has been replaced and nothing returns it. A
    generation the assignment has already moved past cannot be claimed. And a correction the
    child has already answered is annotated in delivery_supersession, which _claim consults
    before anything else - that row is written precisely for the outstanding deliveries whose
    state CANNOT be rewritten, so such a delivery sits at `sending` or `held_uncertain` with
    its relationship and generation still current and reads as retryable from the state alone.
    All three leave the claim refusing forever.

    Being inactive is NOT one of them, which is the distinction this query turns on. paused,
    cancelled and archived are statuses `resume` can lift, and a report recorded while the
    assignment was paused stays attached to the queued delivery and is rendered by the first
    claim after it comes back. Treating inactivity as permanent would accept an oversized
    report during the pause and send it, without the block, once the assignment resumed.
    """
    return store.one(
        "SELECT d.state, d.hold_reason, d.attempt_count,"
        "       EXISTS (SELECT 1 FROM relationships r"
        "                WHERE r.relationship_id = d.relationship_id"
        "                  AND r.superseded_by IS NULL) AS relationship_live,"
        "       NOT EXISTS (SELECT 1 FROM delivery_supersession s"
        "                    WHERE s.event_id = d.event_id) AS correction_open,"
        "       EXISTS (SELECT 1 FROM events e"
        "                JOIN relationships rr ON rr.relationship_id = e.relationship_id"
        "               WHERE e.event_id = d.event_id AND e.stage = 'final'"
        "                 AND e.execution_generation >= rr.execution_generation)"
        "           AS event_current"
        "  FROM deliveries d WHERE d.event_id = ?",
        (event_id,),
    )


def _project_restoration(store, event, row):
    """What becomes of a declared restoration block once this report shapes the message.

    Only a revision request carries one. It is the parent-to-child correction, and the block
    travels inside the findings the verdict recorded; a completion receipt's criteria are the
    child's own claims about its work, which is a different thing wearing the same key.

    Composed rather than reasoned about, because the budget is spent by every section at once
    and no rule about the findings alone can predict what a long evidence list will leave for
    them. The composed message reports which findings it kept and this asks it.
    """
    if event["outcome"] != REVISION_OUTCOME:
        return None
    receipt = json.loads(event["receipt"]) if event["receipt"] else {}
    findings = receipt.get("criteria") or []
    block = restoration.declared(findings)
    if block is None:
        return restoration.not_carried(
            basis=restoration.COMPOSED_BASIS,
            detail="no finding declared a restoration block",
        )
    delivery = _delivery_of(store, event["event_id"])
    unclaimable = (
        "no delivery is queued for it" if delivery is None
        else f"the delivery is held: {delivery['hold_reason']!r}"
        if delivery["hold_reason"] is not None
        else "its relationship has been superseded"
        if not delivery["relationship_live"]
        else "the correction it belongs to has already been answered and superseded"
        if not delivery["correction_open"]
        else "the event is behind the assignment's current generation"
        if not delivery["event_current"]
        else f"the delivery is {delivery['state']!r}"
        if delivery["state"] not in _STILL_ATTEMPTABLE else None
    )
    if unclaimable is not None:
        # Nothing will render this report, so what its composition would have done to the
        # block is not a delivery question and refusing it would reject a supported update on
        # the strength of a message that will never be built. The correction has already gone
        # out, or has stopped being sendable for a reason this layer does not own; either way
        # the bytes that went are recorded against the attempt that froze them. Unmeasured in
        # its own sense: a fact that was never established, and here never arises.
        return restoration.unmeasured(
            f"no further attempt will render this report; {unclaimable}",
            basis=restoration.COMPOSED_BASIS,
        )
    # The request id sits on a line of the message and a10 is longer than a1, so a report
    # resting on the byte budget would be measured against a length the next send does not
    # have. Retries are ordinary here: a busy or archived recipient defers before anything is
    # sent.
    attempt = delivery["attempt_count"] + 1
    try:
        composed = compose_revision(
            {"event_id": event["event_id"], "relationship_id": event["relationship_id"]},
            receipt, derive_request_id(event["event_id"], attempt), row, budget=BUDGET,
        )
    except (ValueError, KeyError, TypeError, DeliveryRefused, ReceiptRefused) as error:
        # Every failure reachable here is deterministic and recurs at delivery, because
        # rendering happens again inside the claim transaction. Calling it unmeasured would
        # commit a report whose own delivery can never succeed: each attempt would raise the
        # same way, roll the claim back, and leave the correction queued forever. The block
        # would then be stranded by a message nobody can compose rather than by one that
        # dropped it, which is the same silence arriving by a longer route. Unmeasured is for
        # a fact that was never established, not for one established as failing.
        raise ReceiptRefused(
            RefusalReason.RESTORATION_UNDELIVERABLE,
            f"this report cannot be composed into a revision message, so the restoration "
            f"block on {block['id']!r} cannot travel and every delivery attempt would fail "
            f"the same way inside its claim: {error}",
        ) from error
    projected = restoration.project_survivors(findings, composed.survivors)
    # Which attempt these bytes belong to. A projection is preflight: it describes the
    # message the NEXT attempt would render, and the bytes an attempt actually froze are
    # recorded per attempt and returned by show.
    projected["attempt"] = attempt
    # The workflow-restore SECTION is a second carrier of resumption context and the composer
    # may drop it whole, since it is not marked essential. That is reported, never refused: it
    # is already named in the omission notice, every ordinary long report uses this path, and
    # refusing here would start rejecting reports that have always been accepted.
    projected["restoreSection"] = (
        "absent" if not (row.get("restore") or {})
        else "budget_dropped" if "workflow restore" in composed.omitted
        else "carried"
    )
    return projected


def read(store, event_id: str):
    """The current submission. Earlier ones are still there; see read_all."""
    row = store.one(
        "SELECT * FROM work_reports WHERE event_id = ?"
        " ORDER BY submission_no DESC LIMIT 1",
        (event_id,),
    )
    if row is None:
        return None
    return _with_handoff(store, _row(row))[0]


def read_all(store, event_id: str) -> list:
    """Every submission, oldest first.

    A message that had to elide part of its report points its recipient at the full record.
    If a later submission replaced the only stored copy, that promise would break for anyone
    still holding the older message, so the rows are kept and this is how they are read.
    """
    rows = store.all(
        "SELECT * FROM work_reports WHERE event_id = ? ORDER BY submission_no", (event_id,)
    )
    return _with_handoff(store, *[_row(row) for row in rows])


def _with_handoff(store, *records: dict) -> list:
    """Attach the merge-readiness evidence, when this submission recorded any.

    Written and never read is a record nobody can act on, and the parent is the reader this
    exists for: it restates these exact vectors to the merge turn rather than going back to
    the forge to rebuild them. Absent stays absent rather than becoming an empty shape,
    because a report with no handoff and a handoff with nothing in it are different facts.

    Every submission is fetched in one read. A history is read whole by `show`, and a query
    per row turns one read into as many as the event has submissions.
    """
    if not records:
        return []
    by_submission = {
        row["submission_no"]: {
            "isDraft": bool(row["is_draft"]),
            "baseVerifiedAt": row["base_verified_at"],
            "requiredDeclared": json.loads(row["required_declared"]),
            "checks": json.loads(row["checks"]),
            "reviewCoverage": json.loads(row["review_coverage"]),
            "threadDispositions": json.loads(row["thread_dispositions"]),
            "criterionEvidence": json.loads(row["criterion_evidence"] or "[]"),
            "limitations": json.loads(row["limitations"] or "[]"),
        }
        for row in store.all(
            "SELECT * FROM work_report_handoffs WHERE event_id = ?",
            (records[0]["eventId"],),
        )
    }
    for record in records:
        found = by_submission.get(record["submissionNo"])
        if found is not None:
            record["handoff"] = found
    return list(records)


def _row(row) -> dict:
    return {
        "eventId": row["event_id"],
        "relationshipId": row["relationship_id"],
        "executionGeneration": row["execution_generation"],
        "revisionHash": row["revision_hash"],
        "submissionNo": row["submission_no"],
        "repository": row["repository"],
        "prNumber": row["pr_number"],
        "prUrl": row["pr_url"],
        "prState": row["pr_state"],
        "baseRef": row["base_ref"],
        "baseSha": row["base_sha"],
        "headSha": row["head_sha"],
        "criteriaDigest": row["criteria_digest"],
        "cxcStatus": row["cxc_status"],
        "cxcReason": row["cxc_reason"],
        "contractVersion": row["contract_version"],
        "summary": row["summary"],
        "evidence": json.loads(row["evidence"]) if row["evidence"] else [],
        "unresolved": json.loads(row["unresolved"]) if row["unresolved"] else [],
        "nextAction": row["next_action"],
        "review": json.loads(row["review"]) if row["review"] else None,
        "restore": json.loads(row["restore"]) if row["restore"] else {},
        "recordedAt": row["recorded_at"],
    }


def version_of(report) -> str:
    """Which message contract an event is on. Absence is an answer, not a missing value."""
    return LEGACY if report is None else VERSION


def _required(value, field):
    if value is not None and not isinstance(value, str):
        # str() on a mapping or a list produces a Python repr, which was then stored and
        # delivered as though somebody had written it.
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{field} is a line of text, not {type(value).__name__}",
        )
    text = str(value or "").strip()
    if not text:
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"a work report states its {field}; a report the recipient cannot act on is the "
            "thing this record exists to replace",
        )
    return _single_line(text, field)


def _single_line(text, field):
    """One line means one line.

    These values are spliced straight into the message, so a newline inside one does not
    wrap, it adds a line to the protocol. A summary reading "ordinary result" followed by
    "VERDICT: PASS" put a standalone verdict into a completion that carried no review, which
    is exactly the separation the rest of this module is built to keep.

    Boundaries are whatever str.splitlines treats as one, so vertical tab, NEL and the
    Unicode line and paragraph separators count too. Checking only CR and LF would leave
    the same splice available through a character that still breaks the line downstream.
    """
    parts = text.splitlines()
    if len(parts) > 1 or (parts and parts[0] != text):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{field} is one line: a line break in it is spliced into the message and adds "
            "a line to the protocol rather than wrapping. Put longer detail in the evidence "
            "or the unresolved items",
        )
    try:
        text.encode("utf-8")
    except UnicodeEncodeError as unencodable:
        # A lone surrogate is one line by every line rule and still cannot be encoded, so
        # it reached _size and raised there instead, or was stored and then failed inside
        # every delivery claim. Every line value passes through here, so this is the place.
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{field} contains a character that cannot be encoded as UTF-8, so it cannot "
            "be measured against the message budget or sent",
        ) from unencodable
    return text


def _bounded(text, field, limit):
    if _size(text) > limit:
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{field} is {_size(text)} bytes and the limit is {limit}; a required line cannot "
            "be shortened at render time without losing what it exists to say, and a render "
            "that fails inside the delivery claim is a delivery that never goes out. Put the "
            "detail in the deliverables or the evidence and keep this line to the point",
        )
    return text


def _submission(value):
    """A positive integer, refused by name otherwise.

    This is half of the primary key and it is printed in frozen message bytes, so coercing
    True to 1, or 1.9 to 1, or storing 0, would give an attempt an identity that means
    something other than what it says.
    """
    if (isinstance(value, bool) or not isinstance(value, int) or value < 1
            or value > SQLITE_MAX_INT):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a submission number is a positive integer the store can hold; it is part of "
            "this report identity and is printed in the bytes that get frozen",
        )
    return value


def _bounded_optional(value, field, limit=LABEL_MAX):
    """The short fields, bounded too. They sit on lines that cannot be shortened either."""
    if value is None:
        return None
    if not isinstance(value, str):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{field} is a string when it is given at all, not {type(value).__name__}",
        )
    return _bounded(_single_line(value, field), field, limit)


def _assert_resubmission(db, event_id, submission_no) -> None:
    """Once a message has gone out, a changed report is a new submission and says so.

    The bytes of every attempt stay frozen in attempt_messages, so history is never lost.
    What this stops is the quieter thing: replacing a report in place after a delivery has
    already been attempted, so a retry carries different instructions under the same event
    and the same stated submission. The recipient would have no way to tell which one it was
    answering. Correcting a report before anything is sent stays free.

    Takes the transaction handle rather than the store, so this cannot be satisfied by a
    read that was already stale by the time the row was written.
    """
    attempted = db.execute(
        "SELECT a.record, a.state, s.submission_no"
        "  FROM attempts a"
        "  LEFT JOIN attempt_report_submissions s ON s.request_id = a.request_id"
        " WHERE a.event_id = ?",
        (event_id,),
    ).fetchall()
    delivered = 0
    legacy = False
    for row in attempted:
        if not _may_have_reached([row]):
            continue
        if row["submission_no"] is None:
            # A pre-contract message reached the recipient. The first report is therefore
            # also a change to what a retry will say, so it counts as submission 1.
            legacy = True
            delivered = max(delivered, 1)
        else:
            delivered = max(delivered, row["submission_no"])
    stored = db.execute(
        "SELECT MAX(submission_no) AS highest FROM work_reports WHERE event_id = ?",
        (event_id,),
    ).fetchone()
    highest = (stored["highest"] if stored else None) or 0
    # Two independent floors, and both apply whatever the other one says. A submission at or
    # below the delivered one would rewrite bytes somebody already has. A submission below
    # the highest stored one would be accepted and then never selected, because reading and
    # delivery both take the highest, so the caller would be told a write landed that nobody
    # will ever see. Checking only one of them left the band between them open.
    if submission_no >= highest and submission_no > delivered:
        return
    if submission_no < highest:
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"submission {highest} of this report already exists, and both reading and "
            f"delivery take the highest, so recording {submission_no} would report success "
            f"and change nothing anyone sees. Correct submission {highest}, or record "
            f"{max(highest, delivered) + 1}",
        )
    if legacy and delivered == 1:
        detail = (
            "a pre-contract message has already been delivered for this event, so a first "
            "report would change what a retry says without changing what it calls itself; "
            "record it as submission 2 or higher"
        )
    else:
        detail = (
            f"submission {delivered} of this report has already been frozen into a delivered "
            f"attempt, so replacing it in place would change what a retry says without "
            f"changing what it calls itself; record this as submission "
            f"{max(highest, delivered) + 1} or "
            "higher. A submission that has never been sent can still be corrected in place"
        )
    raise ReceiptRefused(RefusalReason.MALFORMED_RECEIPT, detail)


def _may_have_reached(attempt_rows) -> bool:
    """Could any of these attempts have put bytes in front of the recipient.

    An attempt row is not a delivery. A settled attempt whose record says sendAttempted is
    no was refused before the send, so its frozen bytes never reached anyone, and counting
    it as a delivered submission would leave a gap in the numbering for nothing. Anything
    else, including an attempt still in flight or one whose record cannot be read, is
    treated as possibly delivered, which is the reading reconciliation already uses: an
    unfinished receipt is never proof of non-delivery.

    inbox_only is the exception that looks like the rule. It carries sendAttempted no,
    because the push was refused before any resume, but its frozen message IS the durable
    inbox item and the recipient can read it. Protocol v1 section 3 calls that channel the
    guarantee. So it reached someone, and the submission behind it is not rewritable.
    """
    for row in attempt_rows or []:
        if _state_of(row) == INBOX_ONLY:
            return True
        if row["record"] is None:
            return True
        try:
            record = json.loads(row["record"])
        except (TypeError, ValueError):
            return True
        if record.get("sendAttempted") != "no":
            return True
    return False


def _state_of(row):
    """The delivery state this attempt settled at, when the row carries one."""
    try:
        return row["state"]
    except (IndexError, KeyError):
        return None


RESTORE_FIELDS = ("mode", "scope", "phase", "phaseObservedAt", "plan", "evidence", "remaining")


def _check_restore(restore):
    """A skill pointer nobody owns is another render-time failure inside the claim.

    _restore_lines resolves each named activity through cxc.skill_pointer, which refuses an
    unknown name on purpose, so an unvalidated name recorded here would surface as a failed
    render and a rolled-back delivery rather than as a typo somebody could fix.

    Shape is checked before membership. Testing an unhashable entry against a dict raises
    TypeError, which reaches the caller as a host failure instead of a named refusal, and a
    validator that crashes on malformed input is not validating it.
    """
    if restore is None:
        return {}
    if not isinstance(restore, dict):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"a restore section is an object of named fields, not {type(restore).__name__}",
        )
    skills = restore.get("skills")
    if skills is None:
        names = []
    elif isinstance(skills, (list, tuple)):
        names = list(skills)
    else:
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"restore skills is a list of activity names, not {type(skills).__name__}",
        )
    unknown = []
    for name in names:
        if not isinstance(name, str):
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"each restore skill is the name of a recorded activity, not {name!r}",
            )
        if name not in cxc.SKILL_POINTERS:
            unknown.append(name)
    if unknown:
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"no recorded skill owner for {unknown}; known activities are "
            f"{sorted(cxc.SKILL_POINTERS)}. Naming an owner nobody has would fail at render "
            "time, inside the delivery claim, instead of here",
        )
    checked = {"skills": names} if names else {}
    for key, value in restore.items():
        if key == "skills":
            continue
        if key not in RESTORE_FIELDS:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"{key!r} is not a restore field this build renders; supported fields are "
                f"{list(RESTORE_FIELDS)} plus skills. An unrendered field is one the "
                "recipient never sees and is never told was dropped",
            )
        if not isinstance(value, str):
            # A mapping here reached the message as a Python repr, and an arbitrary object
            # failed later inside json.dumps as a host exception rather than a refusal.
            if value is None:
                continue
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"restore {key} is a single line of text, not {type(value).__name__}",
            )
        value = value.strip()
        if not value:
            # Dropped rather than stored empty. A key with nothing behind it left the render
            # emitting a workflow-restore heading with no fields under it.
            continue
        checked[key] = _bounded(_single_line(value, f"restore {key}"),
                                f"restore {key}", LABEL_MAX)
    return checked


def _check_review(review):
    if review is None:
        return None
    if not isinstance(review, dict):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"a review is an object with a kind and its findings, not "
            f"{type(review).__name__}; a bare verdict word is not a review",
        )
    findings_in = review.get("findings")
    if findings_in is not None and not isinstance(findings_in, (list, tuple)):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"review findings is a list, not {type(findings_in).__name__}",
        )
    kind = review.get("kind")
    blockers = review.get("blockers")
    # Raises on an unknown kind, on GO-WITH-FIXES without a count, and on a count attached
    # to PASS or FAIL. Rendering the line here is what makes those refusals reachable.
    # Translated, because everything else a producer can get wrong here comes back as a
    # named refusal and a bare ValueError would be the one typo that escapes as a host
    # exception instead.
    try:
        cxc.verdict_line(kind, blockers)
    except ValueError as invalid:
        raise ReceiptRefused(RefusalReason.MALFORMED_RECEIPT, str(invalid)) from invalid
    findings = []
    for item in review.get("findings") or []:
        if not isinstance(item, dict):
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"each review finding is an object naming a criterion, not {item!r}",
            )
        if not isinstance(item.get("id"), str) or not item["id"].strip():
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, "each review finding names a criterion id"
            )
        identifier = item["id"].strip()
        if identifier in {finding["id"] for finding in findings}:
            # The renderer keys enrichment on the id, so a second entry replaced the first
            # and its note or anchor vanished with no omission notice.
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"two findings name criterion {identifier!r}; one criterion carries one "
                "finding, so the second would silently replace the first",
            )
        for field in ("note", "anchor"):
            value = item.get(field)
            if value is not None and not isinstance(value, str):
                raise ReceiptRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    f"a finding {field} is a line of text, not {type(value).__name__}",
                )
        findings.append({
            "id": _single_line(identifier, "a finding id"),
            "verdict": _disposition(item.get("verdict")),
            "note": _single_line((item.get("note") or "").strip(), "a finding note"),
            "anchor": _single_line((item.get("anchor") or "").strip(), "a finding anchor"),
        })
    return {"kind": kind, "blockers": blockers, "findings": findings}


def _disposition(value):
    """One of the frozen criteria dispositions, checked rather than passed through.

    It is rendered onto the violated-criteria line, so an unchecked value carried the same
    line-splicing route as the fields beside it, and a word outside the enum would describe
    a judgment the contract has no room for.

    Absence is allowed. A report finding is also used purely to enrich an authoritative
    finding from the revision receipt with a note or a source anchor, and that use has no
    disposition of its own to state; requiring one refused exactly the enrichment case
    _finding_lines exists for.
    """
    from .criteria import DISPOSITIONS

    if value is None:
        return None
    if value not in DISPOSITIONS:
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{value!r} is not one of {DISPOSITIONS}; the criteria enum is frozen",
        )
    return value


def _sequence(entries, field):
    """A mapping iterates as its keys and a scalar does not iterate at all.

    Without this, a dict quietly became a list of its own key strings and a number raised
    TypeError out of the validator, so the one guarantee this layer makes, that every
    malformed shape comes back as a named refusal, did not hold at the top level.
    """
    if entries is None:
        return []
    if not isinstance(entries, (list, tuple)):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{field} is a list of entries, not {type(entries).__name__}",
        )
    return list(entries)


def _check_evidence(entries):
    """Reject a malformed entry HERE, where a caller can fix it.

    Rendering happens inside the delivery claim transaction, so an entry that only blows up
    at render time rolls the claim back and the delivery never goes out. A shape that cannot
    be rendered must therefore be refused at the point it is recorded.
    """
    checked = []
    for item in _sequence(entries, "evidence"):
        if isinstance(item, str):
            checked.append(
                _single_line(_required(item, "an evidence entry"), "an evidence entry")
            )
            continue
        if not isinstance(item, dict) or not isinstance(item.get("check"), str) \
                or not item["check"].strip():
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"each verification entry is a string or an object naming its check, not "
                f"{item!r}",
            )
        checked.append({
            "check": _single_line(item["check"].strip(), "an evidence check"),
            "exitCode": _exit_code(item.get("exitCode")),
            "detail": _single_line(_text_or_none(item.get("detail"), "an evidence detail"),
                                   "an evidence detail") or None,
        })
    return checked


def _text_or_none(value, field) -> str:
    """A string or nothing. Coercion here turned a mapping into a repr and 0 into absence."""
    if value is None:
        return ""
    if not isinstance(value, str):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{field} is a line of text when it is given at all, not {type(value).__name__}",
        )
    return value.strip()


def _exit_code(value):
    """An integer or nothing. Anything else is either not proof or not storable.

    A mapping here rendered as an exit code nobody can interpret, which is verification
    evidence that says nothing, and an arbitrary object failed later inside json.dumps as a
    host exception rather than a named refusal.
    """
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"an exit code is an integer or absent, not {value!r}; evidence a reader cannot "
            "interpret is not evidence",
        )
    if not -EXIT_CODE_MAX <= value <= EXIT_CODE_MAX:
        # Past the integer-to-string digit limit, json.dumps raises ValueError while
        # serialising the row, which is a host exception rather than a named refusal, and a
        # process never exited with a number this size anyway.
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "an exit code is a number a process could actually have exited with",
        )
    return value


def _check_unresolved(entries):
    checked = []
    for item in _sequence(entries, "unresolved"):
        if isinstance(item, str):
            checked.append(
                _single_line(_required(item, "an unresolved entry"), "an unresolved entry")
            )
            continue
        if (not isinstance(item, dict) or not isinstance(item.get("id"), str)
                or not item["id"].strip()):
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"each unresolved entry is a string or an object naming its id, not {item!r}",
            )
        note = item.get("note")
        if note is not None and not isinstance(note, str):
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"an unresolved note is a line of text, not {type(note).__name__}",
            )
        checked.append({
            "id": _single_line(item["id"].strip(), "an unresolved id"),
            "note": _single_line((note or "").strip(), "an unresolved note"),
        })
    return checked


# -------------------------------------------------------------------- identity guards

def pr_ref(report) -> str:
    """Always repository-qualified. A bare number belongs to whoever reads it first."""
    if report is None or not report.get("prNumber"):
        return ""
    return f"{report['repository']}#{report['prNumber']}"


def pr_key(report):
    """The identity two reports are compared on. The number alone is not one."""
    if report is None or not report.get("prNumber"):
        return None
    return (report["relationshipId"], report["repository"], report["prNumber"])


def assert_current(report, *, execution_generation, head_sha=None) -> None:
    """A report describes one revision of one head. It cannot answer for a later one.

    Without this, the cheapest way to pass a review is to push again and let the previous
    report stand for the new bytes.
    """
    if report is None:
        return
    if report["executionGeneration"] != execution_generation:
        raise DeliveryRefused(
            RefusalReason.STALE_GENERATION,
            f"this report is generation {report['executionGeneration']} and the assignment "
            f"is on generation {execution_generation}; it cannot answer for the current one",
        )
    if head_sha and report.get("headSha") and report["headSha"] != head_sha:
        raise DeliveryRefused(
            RefusalReason.STALE_MARK_CONTEXT,
            f"this report is about head {report['headSha']} and the current head is "
            f"{head_sha}; re-report against the head under review",
        )


# ------------------------------------------------------------------------ composing

class _Section:
    """One block of the message, with how badly it is needed.

    rank orders removal, lowest number last to go. essential blocks are never dropped
    whole; when they have to shrink they keep their heading and say how much is missing.
    """

    def __init__(self, name, lines, rank, *, essential=False, keep=0, last=False, owners=None):
        owners = list(owners) if owners is not None else []
        owners += [None] * (len(lines) - len(owners))
        # Filtered together, so an owner cannot drift onto a line other than the one it was
        # recorded against.
        pairs = [(line, owner) for line, owner in zip(lines, owners) if line is not None]
        self.name = name
        self.lines = [line for line, _ in pairs]
        # Which finding each line belongs to, None for headings and markers. _one_finding can
        # emit two lines for a single finding, so a surviving line COUNT cannot be mapped back
        # to the findings that survived without this.
        self.owners = [owner for _, owner in pairs]
        self.rank = rank
        self.essential = essential
        self.keep = keep
        # A section that must stay at the end of the message even when something was
        # elided. The omission notice goes BEFORE it, not after.
        self.last = last


class _Composed:
    """The fitted message, and what fitting it cost.

    Returning the text alone said what the recipient would read and nothing about what came
    out to make it fit, so a caller that needed to know had to re-parse the bytes it had just
    been handed. omitted names the sections that were dropped or shortened; survivors names
    the findings still standing afterwards, which is what lets a restoration block be reported
    as delivered or not instead of being left to silence.
    """

    __slots__ = ("text", "omitted", "survivors")

    def __init__(self, text, omitted, survivors):
        self.text = text
        self.omitted = tuple(omitted)
        self.survivors = frozenset(survivors)


def _survivors(sections, kept_counts) -> set:
    """Which owned lines are still in the message, named by owner.

    Counted rather than searched. A shortened section keeps a PREFIX of its lines plus a
    marker, so the owners line up by index for exactly the kept count and the marker belongs
    to nobody.
    """
    out = set()
    for section, count in zip(sections, kept_counts):
        for owner in section.owners[:count]:
            if owner is not None:
                out.add(owner)
    return out


def _compose(sections, event_id, *, budget) -> _Composed:
    """Fit the message, and say out loud whatever did not fit.

    Silence is the failure mode being designed against. A message that quietly loses its
    unresolved items reads exactly like a message that had none, and the recipient acts on
    the wrong one. Every removal here leaves a mark and a command that shows the whole
    record.
    """
    blocks = [list(section.lines) for section in sections]
    kept_counts = [len(block) for block in blocks]
    removed = []
    tail = next((i for i, section in enumerate(sections) if section.last), None)

    def rendered(extra_note=True):
        body = [
            line for index, block in enumerate(blocks) if index != tail for line in block
        ]
        if removed and extra_note:
            body += ["", _omission_line(removed, event_id)]
        if tail is not None:
            # Appending the notice after this would make "omitted: ..." the final line, and
            # a consumer following the final-line verdict contract would stop finding the
            # verdict in exactly the messages that had to drop something.
            body += blocks[tail]
        return NEWLINE.join(body)

    def over_budget():
        """Byte length from a running total, never recounted.

        Shortening pops one line at a time. Summing every remaining line on each pass was
        still quadratic in the length of the list being shortened, and this runs inside the
        claim transaction, where a long report would hold the single SQLite writer for the
        duration. The totals below are adjusted by each mutation instead.
        """
        count, total = state["lines"], state["bytes"]
        if removed:
            count += 2
            total += _size(_omission_line(removed, event_id))
        return total + max(count - 1, 0) > budget

    sizes = [[_size(line) for line in block] for block in blocks]
    state = {
        "lines": sum(len(block) for block in blocks),
        "bytes": sum(size for block in sizes for size in block),
    }

    order = sorted(range(len(sections)), key=lambda i: -sections[i].rank)
    for index in order:
        if not over_budget():
            break
        section = sections[index]
        if section.essential or not blocks[index]:
            continue
        removed.append(section.name)
        state["lines"] -= len(blocks[index])
        state["bytes"] -= sum(sizes[index])
        blocks[index] = []
        sizes[index] = []
        kept_counts[index] = 0

    for index in order:
        section = sections[index]
        if not section.essential or len(section.lines) <= section.keep:
            continue
        # kept shrinks by one every pass and the marker is rebuilt from it rather than
        # appended to it. Popping a line and then appending a marker leaves the block the
        # same length, which is a loop that never ends.
        kept = list(section.lines)
        kept_sizes = list(sizes[index])
        marker_size = 0
        while over_budget() and len(kept) > section.keep:
            kept.pop()
            state["bytes"] -= kept_sizes.pop()
            state["lines"] -= 1
            dropped = len(section.lines) - len(kept)
            marker = f"  ... {dropped} more, see the full record"
            state["bytes"] += _size(marker) - marker_size
            state["lines"] += 0 if marker_size else 1
            marker_size = _size(marker)
            blocks[index] = kept + [marker]
            sizes[index] = kept_sizes + [marker_size]
            kept_counts[index] = len(kept)
            if section.name not in removed:
                removed.append(section.name)

    out = rendered()
    if _size(out) > budget:
        raise ValueError(
            f"a message budget of {budget} bytes cannot hold this report even reduced to its "
            "required parts; raise the budget rather than shipping a message that lost them"
        )
    return _Composed(out, removed, _survivors(sections, kept_counts))


def _omission_line(removed, event_id) -> str:
    return f"omitted: {', '.join(removed)} - read in full with {show_command(event_id)}"


def _non_verification(status: str) -> str:
    """The reason this particular report is not a verdict.

    Reusing the DONE sentence for every status told a BLOCKED report it was proving its own
    criteria, which is both false and the opposite of what BLOCKED means.
    """
    return cxc.refuse_promotion("cxc_done" if status == cxc.DONE else "cxc_report")


# ------------------------------------------------------------------------ rendering

def compose_completion(row, receipt, request, report, *, budget=BUDGET) -> _Composed:
    """Child to parent, led by what the parent has to decide.

    Result, then the pull request, then the evidence, then what is still open, then what to
    do. The identifiers keep their place at the bottom: they are how the parent answers, not
    how it decides.
    """
    event_id = row["event_id"]
    assert_current(report, execution_generation=receipt.get("executionGeneration")
                   or report["executionGeneration"])
    readiness = _handoff_lines(report)
    readiness_keep = len(readiness) if _accepted_dispositions(report.get("handoff")) else 2
    sections = [
        _Section("header", [
            "[codex-session-relay] verification request",
            f"result: {report['summary']}",
            f"cxc: {report['cxcStatus']} - {report['cxcReason']}",
            f"  meaning: {cxc.MEANING[report['cxcStatus']]}",
            "  this is the child reporting on its own work. It is not a verification:"
            f" {_non_verification(report['cxcStatus'])}",
        ], rank=0, essential=True, keep=5),
        # keep counts from the top of the block, and these blocks open with a blank line, so
        # a floor of two is what keeps the heading attached to whatever survives under it.
        _Section("pull request", _pr_lines(report), rank=1, essential=True, keep=2),
        # keep is a floor on what survives shrinking, and 2 left only the heading. That was
        # survivable while this section carried counts the parent could re-read for itself,
        # and it is not now: an acceptance line is the ONLY place the parent learns it is
        # credited with a decision it may never have made, so dropping it silently restores
        # exactly the forgery this rendering exists to catch. Naming the whole block makes
        # the composer shorten something else instead.
        _Section("merge readiness", readiness, rank=1, essential=True, keep=readiness_keep),
        _Section("verification", _evidence_lines(report), rank=4),
        _Section("unresolved", _unresolved_lines(report), rank=2, essential=True, keep=2),
        _Section("next", [f"next: {report['nextAction']}"], rank=0, essential=True, keep=1),
        _Section("workflow restore", _restore_lines(report), rank=3),
        _Section("deliverables", _manifest_lines(receipt, event_id), rank=6),
        _Section("manifest reference", _manifest_ref_lines(receipt), rank=2, essential=True,
                 keep=2),
        _Section("relay record", [
            "",
            "relay record:",
            f"  requestId: {request}",
            f"  eventId: {event_id}",
            f"  submission: {report['submissionNo']}  contract: {version_of(report)}",
            f"  relationshipId: {row['relationship_id']}",
            f"  executionGeneration: {receipt.get('executionGeneration')}",
            f"  attempt: {receipt.get('attempt')}",
            f"  outcome: {receipt.get('outcome')}",
            f"  revisionHash: {receipt.get('revisionHash')}",
        # The first five lines are ordered so the floor protects exactly what lets a
        # recipient pick its own submission out of the several that show returns. The rest
        # of the record can shorten.
        ], rank=5, essential=True, keep=5),
        _Section("respond", _ack_lines(event_id), rank=0, essential=True, keep=7),
    ]
    return _compose(sections, event_id, budget=budget)


def render_completion(row, receipt, request, report, *, budget=BUDGET) -> str:
    """The bytes alone, for a caller that only has to send them."""
    return compose_completion(row, receipt, request, report, budget=budget).text


def compose_revision(row, receipt, request, report, *, budget=BUDGET) -> _Composed:
    """Parent to child, shaped as an instruction the child can execute.

    DISPATCH-TASK-01 fixes the fields. What leads is the thing that was violated and the
    anchor that reproduces it, because a correction whose first line is an identifier is a
    correction the child has to go and research before it can start.
    """
    event_id = row["event_id"]
    generation = receipt.get("executionGeneration")
    assert_current(report, execution_generation=generation or report["executionGeneration"])
    review = report.get("review")
    head = [
        "[codex-session-relay] revision request",
    ]
    head.append(f"TASK: {report['summary']}")
    # The status and its reason belong here too. The contract map says a report carries them
    # and the correction direction was dropping both without saying it had.
    head.append(f"cxc: {report['cxcStatus']} - {report['cxcReason']}")
    head.append(f"  meaning: {cxc.MEANING[report['cxcStatus']]}")

    # Kept beside its lines rather than recomputed later. The composer reports which findings
    # its shortening left standing, and it can only do that if it was told which line belonged
    # to which finding before it started removing them.
    finding_lines, finding_owners = _finding_lines(receipt, review)

    sections = [
        _Section("header", head, rank=0, essential=True, keep=4),
        _Section("violated criteria", finding_lines, rank=0, essential=True, keep=2,
                 owners=finding_owners),
        _Section("SCOPE", _scope_lines(report, generation), rank=1, essential=True, keep=2),
        _Section("preserve", _preserve_lines(), rank=0, essential=True, keep=2),
        # A correction that hides the dependencies and risks the report marked open sends the
        # child at the findings without telling it what else is in the way.
        _Section("unresolved", _unresolved_lines(report), rank=2, essential=True, keep=2),
        _Section("MUST DO", [
            "MUST DO:",
            f"  {report['nextAction']}",
            "  answer every finding above with a fix, a reasoned rebuttal, or an explicit"
            " out-of-scope split, and reply on its thread",
        ], rank=0, essential=True, keep=2),
        _Section("MUST NOT", [
            "MUST NOT:",
            "  discard work outside the findings above, rewrite another task history, or"
            " force-push a shared branch",
            "  treat this request as an acknowledgeable message; see the note below",
        ], rank=1, essential=True, keep=2),
        _Section("PROOF", _proof_lines(report), rank=2, essential=True, keep=2),
        _Section("RETURN FORMAT", [
            "RETURN FORMAT:",
            "  result summary, repository and pull request, base and head SHA, verification"
            " evidence, unresolved items, next action, and the CXC report status",
        ], rank=2, essential=True, keep=2),
        _Section("DECISION BOUNDARY", [
            "DECISION BOUNDARY:",
            "  fix what the findings name. Anything wider, anything that would discard"
            " preserved work, and anything needing authority you were not given comes back"
            " here instead of being decided locally",
        ], rank=1, essential=True, keep=2),
        _Section("workflow restore", _restore_lines(report), rank=3),
        _Section("answer", [
            "",
            "There is nothing to acknowledge. Contract v1 defines no acknowledgement for this",
            "direction and the relay refuses one by kind, so there is no proof to compute and",
            "no acknowledgement to send.",
            "Answer with your next completion receipt under the new generation:",
            f"  emit --relationship {row['relationship_id']}"
            f" --generation {generation} --attempt <n>",
            "       --outcome ready_for_review --turn-thread <your task id>"
            " --turn-id <your turn>",
            "       --artifact <path> [--continues-anchor <this generation dispatch turn>]",
        ], rank=0, essential=True, keep=8),
        # Its own section, with a floor that covers every line in it. Left at the end of the
        # answer block, the submission identifier was the first thing shortening removed, and
        # it is what tells a recipient which of several stored submissions produced the bytes
        # it is holding.
        _Section("relay record", [
            "",
            f"relay record: requestId {request}, eventId {event_id},"
            f" submission {report['submissionNo']}, contract {version_of(report)}",
            f"Full record: {show_command(event_id)}",
        ], rank=0, essential=True, keep=3),
    ]
    if review:
        # REVIEW-OUTPUT-01 puts the machine-scannable judgment on the FINAL line, so a
        # scanner reading the tail finds it. assert_reviewed is what stops an ordinary
        # progress notice from reaching this branch at all.
        cxc.assert_reviewed(True)
        sections.append(_Section(
            "verdict", ["", cxc.verdict_line(review["kind"], review.get("blockers"))],
            rank=0, essential=True, keep=2, last=True,
        ))
    return _compose(sections, event_id, budget=budget)


def render_revision(row, receipt, request, report, *, budget=BUDGET) -> str:
    """The bytes alone, for a caller that only has to send them."""
    return compose_revision(row, receipt, request, report, budget=budget).text


def _pr_lines(report):
    reference = pr_ref(report)
    if not reference:
        # No pull request does not mean no revision context. A blocked or budget-exhausted
        # report can still name the branch point and the commit it got to, and dropping
        # those silently left the parent without what it needed to act.
        return [
            "",
            f"repository: {report['repository']}",
            "pull request: none recorded for this event",
        ] + _commit_lines(report)
    lines = [
        "",
        f"pull request: {reference}"
        + (f"  ({report['prState']})" if report.get("prState") else ""),
    ]
    if report.get("prUrl"):
        lines.append(f"  url: {report['prUrl']}")
    return lines + _commit_lines(report)


def _commit_lines(report):
    """The branch point, the commit and the criteria set, whenever they are known."""
    lines = []
    base = report.get("baseRef") or ""
    if report.get("baseSha") or base:
        lines.append(f"  base: {base} {report.get('baseSha') or ''}".rstrip())
    if report.get("headSha"):
        lines.append(f"  head: {report['headSha']}")
    if report.get("criteriaDigest"):
        lines.append(f"  criteria: {report['criteriaDigest']}")
    return lines


def _evidence_lines(report):
    entries = report.get("evidence") or []
    if not entries:
        return ["", "verification: none recorded"]
    lines = ["", "verification:"]
    for item in entries:
        if isinstance(item, str):
            lines.append(f"  {item}")
            continue
        check = item.get("check", "")
        code = item.get("exitCode")
        detail = item.get("detail")
        rendered = f"  {check}"
        if code is not None:
            rendered += f" -> exit {code}"
        if detail:
            rendered += f"  {detail}"
        lines.append(rendered)
    return lines


def _unresolved_lines(report):
    entries = report.get("unresolved") or []
    if not entries:
        return ["", "unresolved: none"]
    lines = ["", "unresolved:"]
    for item in entries:
        if isinstance(item, str):
            lines.append(f"  - {item}")
        else:
            note = item.get("note") or ""
            lines.append(f"  - {item.get('id')}: {note}".rstrip(": "))
    return lines


def _finding_lines(receipt, review):
    """The recorded verdict decides WHICH criteria; the review only enriches them.

    A revision event already carries the parent findings in its own receipt, written by
    record_verdict in the transaction that opened the new generation. Rendering only the work
    report review meant a report with no review, or one naming a different set, replaced the
    authoritative findings with nothing or with something else, and the child was corrected
    against instructions the parent never gave. So the receipt leads, the review adds notes
    and source anchors by id, and anything the review raises on its own is kept but labelled
    as not part of the recorded verdict.

    Returns the lines and, beside them, which finding each line belongs to. One finding can
    occupy two lines, so the composer cannot work out which findings its shortening left
    standing from a line count alone, and a restoration block would go back to being
    unobservable.
    """
    authoritative = [item for item in (receipt.get("criteria") or []) if item.get("id")]
    enrichment = {}
    for item in (review or {}).get("findings") or []:
        enrichment[item["id"]] = item
    if not authoritative and not enrichment:
        return ["", "violated criteria: no per-criterion findings were recorded"], [None, None]

    lines = ["", "violated criteria:"]
    owners = [None, None]
    seen = set()
    if not authoritative:
        # A legacy relationship can reach needs_changes with no registered criteria, so the
        # review findings are all there is. Saying where they came from still matters: the
        # child should not read them as a recorded verdict it can look up.
        lines.append("  from the review; this assignment has no recorded criteria set:")
        owners.append(None)
    for item in authoritative or list(enrichment.values()):
        extra = enrichment.get(item["id"], {})
        seen.add(item["id"])
        rendered = _one_finding(item, extra)
        lines += rendered
        owners += [item["id"]] * len(rendered)
    unrecorded = [item for key, item in enrichment.items() if key not in seen]
    if authoritative and unrecorded:
        lines.append("  also raised in review, not part of the recorded verdict:")
        owners.append(None)
        for item in unrecorded:
            rendered = _one_finding(item, {})
            lines += rendered
            owners += [item["id"]] * len(rendered)
    return lines, owners


def _one_finding(item, extra):
    note = item.get("note") or extra.get("note")
    # An enrichment-only finding has no disposition of its own, and rendering the absence as
    # "None" told the reader a criterion had a judgment named None.
    disposition = item.get("verdict")
    name = f"{item['id']}{restoration.label(item)}"
    rendered = f"  {name}: {disposition}" if disposition else f"  {name}"
    if note:
        rendered += f" - {note}"
    out = [rendered]
    anchor = extra.get("anchor") or item.get("anchor")
    if anchor:
        out.append(f"    anchor: {anchor}")
    return out


def _scope_lines(report, generation):
    lines = ["", "SCOPE:"]
    reference = pr_ref(report)
    lines.append(f"  {reference}" if reference else f"  {report['repository']}")
    if report.get("baseSha"):
        lines.append(f"  base {report.get('baseRef') or ''} {report['baseSha']}".rstrip())
    if report.get("headSha"):
        lines.append(f"  head {report['headSha']}")
    if report.get("criteriaDigest"):
        lines.append(f"  criteria {report['criteriaDigest']}")
    lines.append(f"  execution generation {generation} (new)")
    return lines


def _preserve_lines():
    """Fixed protocol prose, kept out of the shortenable part of SCOPE.

    Mixed in with the variable data, the preserve boundary could be shortened away, and the
    omission marker points at show, which returns the receipt and the work report but not
    template text. So those lines were not recoverable anywhere once dropped.
    """
    return [
        "  preserve: everything outside the findings above, including work this",
        "    request does not mention and any other task in-flight beside it",
    ]


def _proof_lines(report):
    lines = ["", "PROOF:"]
    entries = report.get("evidence") or []
    if entries:
        lines.append("  re-run what this review ran, and report command, exit code and result:")
        for item in entries:
            if isinstance(item, str):
                lines.append(f"    {item}")
            else:
                lines.append(f"    {item.get('check', '')}")
    else:
        lines.append("  state the command, its exit code and what it showed, for each finding")
    lines.append("  a passing string match is not a passing behaviour")
    return lines


def _restore_lines(report):
    restore = report.get("restore") or {}
    if not restore:
        return []
    lines = ["", "workflow restore:"]
    for key, label in (
        ("mode", "mode"), ("scope", "scope"), ("phase", "phase"),
        ("phaseObservedAt", "phase observed"), ("plan", "plan"), ("evidence", "evidence"),
        ("remaining", "remaining"),
    ):
        if restore.get(key):
            lines.append(f"  {label}: {restore[key]}")
    for activity in restore.get("skills") or []:
        lines.append(f"  read: {cxc.skill_pointer(activity)}")
    return lines


def _manifest_lines(receipt, event_id):
    """Including manifestRef, which the pre-contract message always carried.

    It is the stable location a receipt points at when the live artifact paths may move, so
    dropping it would leave the recipient verifying against files that had relocated. Adding
    a work report must not quietly take it away.
    """
    manifest = receipt.get("manifest")
    if not manifest:
        return ["", "deliverables: none (execution-only outcome)"]
    lines = ["", f"deliverables: {len(manifest)}"]
    for entry in manifest:
        size = entry.get("bytes")
        lines.append(
            f"  {entry['path']}  sha256={entry['sha256']}"
            + (f"  bytes={size}" if size is not None else "")
        )
    return lines


def _manifest_ref_lines(receipt):
    """Its own section, because it is a verification pointer and not a file listing.

    Living inside the deliverables block meant the first thing the composer dropped took the
    pointer with it, so precisely the long reports most likely to be shortened lost the
    stable location their artifacts can still be verified against.
    """
    reference = receipt.get("manifestRef")
    if not reference:
        return []
    text = str(reference)
    try:
        text.encode("utf-8")
    except UnicodeEncodeError:
        # The receipt is contract-validated and this field has no encodability rule there,
        # so an unencodable one reached here and made every delivery claim raise while
        # measuring. Say it exists and where to read it rather than refusing the delivery.
        return ["", "manifestRef: present but not renderable; read it in the record"]
    if _size(text) > REF_SHOWN:
        # Truncated visibly, never quietly. A pointer nobody can read is still better than a
        # message that cannot be sent, and the whole value is in the record.
        shown = text.encode("utf-8")[:REF_SHOWN].decode("utf-8", "ignore")
        return ["", f"manifestRef: {shown}... (truncated; full value in the record)"]
    return ["", f"manifestRef: {text}"]


def _ack_lines(event_id):
    return [
        "",
        "To respond, from inside your own turn:",
        f"  claim     --event {event_id} --turn <your turn id>",
        f"  ack-proof --event {event_id} --turn <your turn id>",
        f"  ack       --event {event_id} --ack-turn <your turn id> --ack-proof <proof>",
        f"  verdict   --event {event_id} --verdict <verified|needs_changes|"
        "unverified|aborted> --verdict-turn <your turn id>",
        "",
        "The proof is sha256(eventId|<your own turn id>). This message does not and cannot",
        "contain that turn id, which is what distinguishes acknowledging from echoing.",
        f"Full record: {show_command(event_id)}",
    ]


# ------------------------------------------------------------------- merge readiness

def _accepted_dispositions(handoff) -> list:
    """The threads a handoff claims the parent agreed to leave unfixed."""
    if not handoff:
        return []
    return [
        one for one in (handoff.get("threadDispositions") or [])
        if one.get("disposition") == "accepted"
    ]


def _acceptance_lines(accepted) -> list:
    """The confirmations, formatted once.

    The record-time bound and the render read the same function on purpose. Measuring one
    string and rendering another is how a limit passes its own check and then overflows the
    thing it was protecting.
    """
    if not accepted:
        return []
    lines = [f"  accepted by your decision ({len(accepted)}) - confirm each was yours:"]
    for one in accepted:
        lines.append(f"    {one.get('threadId')}: {one.get('addressedBy')}"
                     f" - {one.get('followUpOwner')} owns it,"
                     f" reopens on {one.get('reopenTrigger')}")
    return lines


def _handoff_lines(report) -> list:
    """What the parent restates, said in the message rather than left in the store.

    A record written and never rendered is the silent loss the omission notice exists to
    prevent: the recipient reads a completion, sees nothing about the review, and has no reason
    to suspect there is a row it never fetched. These lines are that pointer, and they carry
    the two numbers worth comparing on sight.
    """
    handoff = report.get("handoff")
    if not handoff:
        return []
    coverage = handoff.get("reviewCoverage") or {}
    required = handoff.get("requiredDeclared") or []
    checks = handoff.get("checks") or []
    # An acceptance is the child asserting a decision the PARENT made, and nothing here can
    # authenticate that. Rendering each one puts the assertion in front of the only party who
    # knows whether it happened, at the moment it restates the record anyway. Left in the
    # store it would be a row nobody had a reason to fetch.
    accepted = _accepted_dispositions(handoff)
    lines = [
        "",
        "merge readiness (restate these; do not collect them again):",
        f"  head {report.get('headSha')} on base {report.get('baseSha')}"
        f" verified {handoff.get('baseVerifiedAt')}",
        f"  required: {', '.join(required) if required else 'none declared by the branch'}"
        f" - {len(checks)} run(s) restated",
        f"  review: {coverage.get('totalCount')} thread(s) seen over"
        f" {coverage.get('pagesRead')} page(s), {coverage.get('unresolved')} unresolved",
    ]
    return lines + _acceptance_lines(accepted)

#: Which refusal a problem code becomes. The next action genuinely differs for each, which is
#: why the predicate returns codes rather than prose: an undeclared required set is something
#: the child must go and read, a stale check is something it must wait for or re-run, and an
#: unenumerated review is something it must finish. Folding them into one reason would tell a
#: producer that something is wrong without telling it which thing to do.
_HANDOFF_REASONS = {
    mergeevidence.MALFORMED: RefusalReason.MALFORMED_RECEIPT,
    mergeevidence.REVIEW_UNSTATED: RefusalReason.MERGE_REVIEW_INCOMPLETE,
    mergeevidence.REVIEW_INCOMPLETE: RefusalReason.MERGE_REVIEW_INCOMPLETE,
    mergeevidence.CHECKS_STALE: RefusalReason.MERGE_CURRENCY_STALE,
    mergeevidence.REQUIRED_UNDECLARED: RefusalReason.MERGE_EVIDENCE_REQUIRED,
}

#: `accepted` is here because the other five could not say the true thing about a real
#: finding a parent decided not to fix now. `not_applicable` is false when it does apply
#: and `disputed` is false when nobody disputes it, so a child holding a minor separable
#: defect had only a false `fixed` or another round. Which findings may be accepted at all,
#: and what the acceptance has to record, belong to the impact rule in the crw-run skill;
#: this enum only keeps the judgment sayable.
DISPOSITIONS = ("fixed", "accepted", "not_applicable", "duplicate", "already_resolved",
                "disputed")


def _verified_at(value):
    """A timestamp, or None for the caller to refuse.

    Presence is not a time. A non-empty string passed, so a handoff could say the base was
    verified "not-a-date" and the delivered message would print exactly that, leaving the
    parent with a field that looks like evidence and answers nothing. An offset is required
    for the same reason: the parent compares this against its own reading, and a naive stamp
    does not say which clock it came from.
    """
    if not isinstance(value, str) or not value.strip():
        return None
    try:
        parsed = datetime.fromisoformat(value.strip().replace("Z", "+00:00"))
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return None
    return parsed.isoformat()


def _check_handoff(handoff, pr_number, head_sha, base_sha, outcome):
    """The child's merge-readiness evidence, refused at the point it can still be fixed.

    This is the gate CRW-128 exists for. A child reported EQP-29 complete with fourteen
    unresolved review threads and nothing in the contract objected, because the completion
    path tested that the turn had ended and never what the review said.

    It applies ONLY to a completion report that names a pull request. A report with no pull
    request has no merge readiness to state, and a revision request travelling the other way
    is the parent's judgment rather than the child's evidence; demanding check runs from
    either would refuse every report this contract is not about.
    """
    if outcome == REVISION_OUTCOME:
        if handoff is None:
            return None
        raise ReceiptRefused(
            RefusalReason.DISPOSITION_CONFLICT,
            "a revision request carries no merge-readiness handoff: this event exists because "
            "the parent ruled needs_changes, and the evidence that a candidate is ready comes "
            "from the child on the completion it is about",
        )
    if handoff is None:
        if pr_number is None or outcome != READY_OUTCOME:
            return None
        # The gate has to be compulsory or it is not a gate. An opt-in one is satisfied by
        # saying nothing, which is exactly what the child in EQP-29 did: it reported the work
        # complete and never mentioned that fourteen review threads were open. Silence about
        # the review is the failure, so silence is what this refuses.
        #
        # Only a report CLAIMING readiness, though. A blocked, interrupted or failed turn names
        # its pull request too, and demanding a complete handoff from one would make the honest
        # outcome the only one a child could not report - which is the opposite of OPS-9.2,
        # where a missing review is blocked and blocked is reported as blocked.
        raise ReceiptRefused(
            RefusalReason.MERGE_EVIDENCE_REQUIRED,
            "this report names a pull request and states nothing about its checks or its "
            "review, so nothing here says the candidate is ready to hand over. Record the "
            "merge-readiness handoff, or report the work blocked if the review is not finished",
        )
    if not isinstance(handoff, dict):
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"a merge-readiness handoff is an object, not a {type(handoff).__name__}",
        )
    if pr_number is None:
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a merge-readiness handoff describes a pull request, but none is named; give the "
            "pull request number, or leave the handoff out",
        )
    review = handoff.get("reviewCoverage")
    checks = handoff.get("checks", [])
    required = handoff.get("requiredDeclared", mergeevidence.UNDECLARED)
    problems = mergeevidence.handoff_problems(head_sha, review, checks, required=required)
    if problems:
        raise ReceiptRefused(
            _HANDOFF_REASONS[problems[0].code],
            "this candidate is not ready to hand over: "
            + "; ".join(mergeevidence.details(problems)),
        )
    if not isinstance(handoff.get("isDraft"), bool):
        # Optional and truthy meant a draft could be handed over by not mentioning it, and an
        # integer 0 read as "not a draft" from a producer that never looked.
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "the handoff states isDraft as true or false; it is how a reviewer knows the "
            "review was actually requested, and leaving it out is not the same as false",
        )
    if handoff["isDraft"]:
        raise ReceiptRefused(
            RefusalReason.MERGE_EVIDENCE_REQUIRED,
            "the pull request is still a draft, so the review it reports was never actually "
            "requested; mark it ready for review before handing it over",
        )
    verified_at = _verified_at(handoff.get("baseVerifiedAt"))
    if verified_at is None:
        # The parent's one job here is to restate a base and compare it. A handoff that says
        # which head it is about and not which base, or not when the base was read, hands over
        # half of the comparison and leaves the other half to be guessed.
        raise ReceiptRefused(
            RefusalReason.MERGE_EVIDENCE_REQUIRED,
            "the handoff states the head it is about but not when its base was verified; the "
            "parent restates the base immediately before merging, and a base nobody dated "
            "cannot be compared against the one it reads",
        )
    if not base_sha:
        raise ReceiptRefused(
            RefusalReason.MERGE_EVIDENCE_REQUIRED,
            "a report carrying a merge-readiness handoff names the base commit it was verified "
            "against; without one there is nothing for the pre-merge re-read to disagree with",
        )
    dispositions = _check_dispositions(handoff.get("threadDispositions"), review)
    # Checked here rather than per field, because every field can be legal while the sum is
    # not, and refusing at record time is the difference between a child that is told to fix
    # some of them and a report that is accepted now and undeliverable for good.
    shown = _acceptance_lines([one for one in dispositions
                               if one["disposition"] == "accepted"])
    if shown:
        total = sum(_size(line) + 1 for line in shown)
        if total > ACCEPTANCE_SHOWN:
            raise ReceiptRefused(
                RefusalReason.MERGE_EVIDENCE_REQUIRED,
                f"the acceptance confirmations for this candidate render {total} bytes and the"
                f" reserve is {ACCEPTANCE_SHOWN}. They cannot be shortened, because a dropped"
                " confirmation hides a decision the parent is credited with and never made, so"
                " a candidate whose confirmations do not fit the message is carrying too many"
                " accepted defects to hand over at once. Fix some of them, shorten the decision"
                " and follow-up references, or split the change",
            )
    return {
        "isDraft": False,
        "baseVerifiedAt": verified_at,
        "requiredDeclared": sorted(
            _bounded(_single_line(name, "a required check name"), "a required check name",
                     LABEL_MAX)
            for name in required
        ),
        "checks": [dict(entry) for entry in checks],
        "reviewCoverage": dict(review),
        "threadDispositions": dispositions,
        "criterionEvidence": _check_evidence(handoff.get("criterionEvidence")),
        "limitations": [
            _single_line(_required(item, "a limitation"), "a limitation")
            for item in _sequence(handoff.get("limitations"), "limitations")
        ],
    }


def _check_dispositions(entries, review):
    """Every thread that was seen carries a judged disposition, and a judgment carries evidence.

    A count of unresolved threads says the buttons were pressed. It does not say anybody read
    the finding, and the criteria this implements are explicit that disputed, duplicate and
    already-resolved findings are judged with a reason rather than cleared mechanically. So a
    thread the child saw and did not account for is missing, and 'resolved' is not among the
    words it may account for it with.

    `accepted` carries the same burden as `fixed` for the same reason. A fix names the commit
    because the claim is checkable there; an acceptance names the decision and the follow-up
    it left because that is where ITS claim is checkable. An acceptance with nothing to point
    at is the shape this gate exists to refuse: it reads exactly like a weighed judgment and
    contains none.

    What this canNOT do is authenticate the parent. Nothing here has an authenticated caller,
    so a child asserting "the parent accepted this" is asserting it, exactly as `--actor` and
    `--task` are asserted everywhere else in this package. An acceptance is therefore the one
    disposition that legitimises a defect the candidate still carries, which makes an
    unnoticed forgery the real risk rather than a malformed field. The answer is not a check
    that cannot be performed and reads like one: every acceptance is rendered into the merge
    readiness lines the parent restates before merging, so a decision the parent did not make
    arrives in front of the party that would know, and OPS-9.4 already returns a record that
    disagrees with the re-read to the child fail-closed.
    """
    judged = {}
    for item in _sequence(entries, "threadDispositions"):
        if not isinstance(item, dict):
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"each thread disposition is an object naming its thread, not {item!r}",
            )
        identifier = item.get("threadId")
        if not isinstance(identifier, str) or not identifier.strip():
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "each thread disposition names the review thread it is about",
            )
        # Bounded and single-lined because these three are spliced into the acceptance
        # confirmations the parent reads. Before they were rendered, a newline in one was
        # merely ugly storage; now it adds a line to the protocol, which is the splice
        # `_single_line` exists to refuse.
        identifier = _bounded(_single_line(identifier.strip(), "a thread identifier"),
                              "a thread identifier", LABEL_MAX)
        if identifier in judged:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"two dispositions name thread {identifier!r}; one thread carries one "
                "judgment, so the second would silently replace the first",
            )
        disposition = item.get("disposition")
        if disposition not in DISPOSITIONS:
            raise ReceiptRefused(
                RefusalReason.MERGE_REVIEW_INCOMPLETE,
                f"thread {identifier!r} is recorded as {disposition!r}, which is not a "
                f"judgment. Use one of: {', '.join(DISPOSITIONS)}. Resolving a thread is a "
                "button, not a finding anybody ruled on",
            )
        note = _single_line(_required(item.get("evidence"), "a disposition evidence"),
                            "a disposition evidence")
        addressed = item.get("addressedBy")
        if isinstance(addressed, str) and addressed.strip():
            addressed = _bounded(_single_line(addressed.strip(), "a disposition addressedBy"),
                                 "a disposition addressedBy", LABEL_MAX)
        if disposition == "fixed" and not (isinstance(addressed, str) and addressed.strip()):
            raise ReceiptRefused(
                RefusalReason.MERGE_REVIEW_INCOMPLETE,
                f"thread {identifier!r} is recorded fixed without the commit that fixed it; "
                "a per-finding trail is the finding, the commit that addressed it, and the "
                "recheck",
            )
        owner = item.get("followUpOwner")
        trigger = item.get("reopenTrigger")
        if disposition == "accepted":
            if not (isinstance(addressed, str) and addressed.strip()):
                raise ReceiptRefused(
                    RefusalReason.MERGE_REVIEW_INCOMPLETE,
                    f"thread {identifier!r} is recorded accepted without naming the parent "
                    "decision that accepted it; an acceptance is a judgment somebody made and "
                    "owns, not a fix and not a cleared thread",
                )
            # Two facts, two fields. One opaque follow-up string was satisfied by naming an
            # owner and saying nothing about what brings the finding back, and an acceptance
            # nothing can reopen is a waiver wearing a follow-up's name.
            if not (isinstance(owner, str) and owner.strip()):
                raise ReceiptRefused(
                    RefusalReason.MERGE_REVIEW_INCOMPLETE,
                    f"thread {identifier!r} is recorded accepted with no follow-up owner; an "
                    "acceptance that leaves nobody holding the residue is how a known defect "
                    "stops being anybody's",
                )
            if not (isinstance(trigger, str) and trigger.strip()):
                raise ReceiptRefused(
                    RefusalReason.MERGE_REVIEW_INCOMPLETE,
                    f"thread {identifier!r} is recorded accepted with no reopen trigger; "
                    "without one the acceptance cannot be revisited by anything, which is a "
                    "waiver rather than a deferral",
                )
            owner = _bounded(_single_line(owner.strip(), "a follow-up owner"),
                             "a follow-up owner", LABEL_MAX)
            trigger = _bounded(_single_line(trigger.strip(), "a reopen trigger"),
                               "a reopen trigger", LABEL_MAX)
        elif owner is not None or trigger is not None:
            # Only an acceptance leaves a residue somebody owns. Letting these ride along on
            # a fix would make "there is a follow-up" stop meaning anything.
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"thread {identifier!r} is recorded {disposition!r} and carries follow-up "
                "fields; a follow-up owner and a reopen trigger belong to an acceptance, "
                "which is the disposition that leaves a residue for somebody to own",
            )
        judged[identifier] = {
            "threadId": identifier, "disposition": disposition, "evidence": note,
            "addressedBy": addressed.strip() if isinstance(addressed, str) else None,
            "followUpOwner": owner if isinstance(owner, str) else None,
            "reopenTrigger": trigger if isinstance(trigger, str) else None,
        }
    seen = [str(one) for one in (review.get("threadsSeen") or [])]
    unaccounted = [one for one in seen if one not in judged]
    if unaccounted:
        raise ReceiptRefused(
            RefusalReason.MERGE_REVIEW_INCOMPLETE,
            "these review threads were seen and carry no judged disposition: "
            + repr(unaccounted),
        )
    return [judged[one] for one in seen]
