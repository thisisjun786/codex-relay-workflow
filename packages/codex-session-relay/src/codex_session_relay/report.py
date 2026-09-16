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

from . import cxc
from .errors import DeliveryRefused, ReceiptRefused, RefusalReason

NEWLINE = chr(10)
VERSION = "relay-report/1"
LEGACY = "relay-message/legacy"

# Generous enough that an ordinary report is never touched, small enough that a runaway one
# is elided here, on purpose and in the open, rather than cut by whatever reads it later.
BUDGET = 6000


def show_command(event_id: str) -> str:
    return f"codex-session-relay show --event {event_id}"


# ------------------------------------------------------------------------ recording

def record(store, clock, *, event_id, relationship_id, execution_generation, revision_hash,
           outcome, repository, cxc_status, cxc_reason, summary, next_action,
           pr_number=None, pr_url=None, pr_state=None, base_ref=None, base_sha=None,
           head_sha=None, criteria_digest=None, evidence=None, unresolved=None, review=None,
           restore=None, submission_no=1) -> dict:
    """Store one report, validated, bound to the revision it describes.

    The CXC status is checked against the outcome the child's receipt already asserted. It
    is not allowed to choose that outcome: the receipt is the only thing the frozen contract
    lets assert one, and a status that picked it could quietly overrule the evidence.
    """
    cxc.check_status(cxc_status, outcome)
    repository = _required(repository, "repository")
    summary = _required(summary, "summary")
    next_action = _required(next_action, "next_action")
    reason = _required(cxc_reason, "cxc_reason")
    if pr_number is not None:
        if isinstance(pr_number, bool) or not isinstance(pr_number, int) or pr_number < 1:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"a pull request number is a positive integer, not {pr_number!r}",
            )
        if not head_sha:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a report naming a pull request names the head commit it is about; without "
                "one, a later push silently inherits this report",
            )
    review = _check_review(review)
    row = {
        "eventId": event_id,
        "relationshipId": relationship_id,
        "executionGeneration": int(execution_generation),
        "revisionHash": revision_hash,
        "submissionNo": int(submission_no),
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
        db.execute(
            "INSERT INTO work_reports (event_id, relationship_id, execution_generation,"
            " revision_hash, submission_no, repository, pr_number, pr_url, pr_state, base_ref,"
            " base_sha, head_sha, criteria_digest, cxc_status, cxc_reason, contract_version,"
            " summary, evidence, unresolved, next_action, review, restore, recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
            " ON CONFLICT(event_id) DO UPDATE SET"
            "   submission_no = excluded.submission_no, repository = excluded.repository,"
            "   pr_number = excluded.pr_number, pr_url = excluded.pr_url,"
            "   pr_state = excluded.pr_state, base_ref = excluded.base_ref,"
            "   base_sha = excluded.base_sha, head_sha = excluded.head_sha,"
            "   criteria_digest = excluded.criteria_digest, cxc_status = excluded.cxc_status,"
            "   cxc_reason = excluded.cxc_reason, contract_version = excluded.contract_version,"
            "   summary = excluded.summary, evidence = excluded.evidence,"
            "   unresolved = excluded.unresolved, next_action = excluded.next_action,"
            "   review = excluded.review, restore = excluded.restore,"
            "   recorded_at = excluded.recorded_at",
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
    row["recordedAt"] = now
    return row


def read(store, event_id: str):
    row = store.one("SELECT * FROM work_reports WHERE event_id = ?", (event_id,))
    if row is None:
        return None
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
    text = str(value or "").strip()
    if not text:
        raise ReceiptRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"a work report states its {field}; a report the recipient cannot act on is the "
            "thing this record exists to replace",
        )
    return text


def _check_review(review):
    if review is None:
        return None
    kind = review.get("kind")
    blockers = review.get("blockers")
    # Raises on an unknown kind, on GO-WITH-FIXES without a count, and on a count attached
    # to PASS or FAIL. Rendering the line here is what makes those refusals reachable.
    cxc.verdict_line(kind, blockers)
    findings = []
    for item in review.get("findings") or []:
        identifier = str(item.get("id") or "").strip()
        if not identifier:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, "each review finding names a criterion id"
            )
        findings.append({
            "id": identifier,
            "verdict": item.get("verdict"),
            "note": str(item.get("note") or "").strip(),
            "anchor": str(item.get("anchor") or "").strip(),
        })
    return {"kind": kind, "blockers": blockers, "findings": findings}


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

    def __init__(self, name, lines, rank, *, essential=False, keep=0):
        self.name = name
        self.lines = [line for line in lines if line is not None]
        self.rank = rank
        self.essential = essential
        self.keep = keep


def _compose(sections, event_id, *, budget) -> str:
    """Fit the message, and say out loud whatever did not fit.

    Silence is the failure mode being designed against. A message that quietly loses its
    unresolved items reads exactly like a message that had none, and the recipient acts on
    the wrong one. Every removal here leaves a mark and a command that shows the whole
    record.
    """
    blocks = [list(section.lines) for section in sections]
    removed = []

    def rendered(extra_note=True):
        body = [line for block in blocks for line in block]
        if removed and extra_note:
            body += ["", _omission_line(removed, event_id)]
        return NEWLINE.join(body)

    order = sorted(range(len(sections)), key=lambda i: -sections[i].rank)
    for index in order:
        if len(rendered()) <= budget:
            break
        section = sections[index]
        if section.essential or not blocks[index]:
            continue
        removed.append(section.name)
        blocks[index] = []

    for index in order:
        section = sections[index]
        if not section.essential or len(section.lines) <= section.keep:
            continue
        # kept shrinks by one every pass and the marker is rebuilt from it rather than
        # appended to it. Popping a line and then appending a marker leaves the block the
        # same length, which is a loop that never ends.
        kept = list(section.lines)
        while len(rendered()) > budget and len(kept) > section.keep:
            kept.pop()
            dropped = len(section.lines) - len(kept)
            blocks[index] = kept + [f"  ... {dropped} more, see the full record"]
            if section.name not in removed:
                removed.append(section.name)

    out = rendered()
    if len(out) > budget:
        raise ValueError(
            f"a message budget of {budget} cannot hold this report even reduced to its "
            "required parts; raise the budget rather than shipping a message that lost them"
        )
    return out


def _omission_line(removed, event_id) -> str:
    return f"omitted: {', '.join(removed)} - read in full with {show_command(event_id)}"


# ------------------------------------------------------------------------ rendering

def render_completion(row, receipt, request, report, *, budget=BUDGET) -> str:
    """Child to parent, led by what the parent has to decide.

    Result, then the pull request, then the evidence, then what is still open, then what to
    do. The identifiers keep their place at the bottom: they are how the parent answers, not
    how it decides.
    """
    event_id = row["event_id"]
    sections = [
        _Section("header", [
            "[codex-session-relay] verification request",
            f"result: {report['summary']}",
            f"cxc: {report['cxcStatus']} - {report['cxcReason']}",
            f"  meaning: {cxc.MEANING[report['cxcStatus']]}",
            "  this is the child reporting on its own work. It is not a verification:"
            f" {cxc.refuse_promotion('cxc_done')}",
        ], rank=0, essential=True, keep=5),
        # keep counts from the top of the block, and these blocks open with a blank line, so
        # a floor of two is what keeps the heading attached to whatever survives under it.
        _Section("pull request", _pr_lines(report), rank=1, essential=True, keep=2),
        _Section("verification", _evidence_lines(report), rank=4),
        _Section("unresolved", _unresolved_lines(report), rank=2, essential=True, keep=2),
        _Section("next", [f"next: {report['nextAction']}"], rank=0, essential=True, keep=1),
        _Section("workflow restore", _restore_lines(report), rank=3),
        _Section("deliverables", _manifest_lines(receipt, event_id), rank=6),
        _Section("relay record", [
            "",
            "relay record:",
            f"  contract: {version_of(report)}",
            f"  requestId: {request}",
            f"  eventId: {event_id}",
            f"  relationshipId: {row['relationship_id']}",
            f"  executionGeneration: {receipt.get('executionGeneration')}",
            f"  attempt: {receipt.get('attempt')}  submission: {report['submissionNo']}",
            f"  outcome: {receipt.get('outcome')}",
            f"  revisionHash: {receipt.get('revisionHash')}",
        ], rank=5),
        _Section("respond", _ack_lines(event_id), rank=0, essential=True, keep=7),
    ]
    return _compose(sections, event_id, budget=budget)


def render_revision(row, receipt, request, report, *, budget=BUDGET) -> str:
    """Parent to child, shaped as an instruction the child can execute.

    DISPATCH-TASK-01 fixes the fields. What leads is the thing that was violated and the
    anchor that reproduces it, because a correction whose first line is an identifier is a
    correction the child has to go and research before it can start.
    """
    event_id = row["event_id"]
    generation = receipt.get("executionGeneration")
    review = report.get("review")
    head = [
        "[codex-session-relay] revision request",
    ]
    if review:
        cxc.assert_reviewed(True)
        head.append(cxc.verdict_line(review["kind"], review.get("blockers")))
    head.append(f"TASK: {report['summary']}")

    sections = [
        _Section("header", head, rank=0, essential=True, keep=2),
        _Section("violated criteria", _finding_lines(review), rank=0, essential=True, keep=2),
        _Section("SCOPE", _scope_lines(report, generation), rank=1, essential=True, keep=2),
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
            "",
            f"relay record: requestId {request}, eventId {event_id},"
            f" contract {version_of(report)}",
            f"Full record: {show_command(event_id)}",
        ], rank=0, essential=True, keep=9),
    ]
    return _compose(sections, event_id, budget=budget)


def _pr_lines(report):
    reference = pr_ref(report)
    if not reference:
        return [
            "",
            f"repository: {report['repository']}",
            "pull request: none recorded for this event",
        ]
    lines = [
        "",
        f"pull request: {reference}"
        + (f"  ({report['prState']})" if report.get("prState") else ""),
    ]
    if report.get("prUrl"):
        lines.append(f"  url: {report['prUrl']}")
    base = report.get("baseRef") or ""
    if report.get("baseSha"):
        lines.append(f"  base: {base} {report['baseSha']}".rstrip())
    lines.append(f"  head: {report.get('headSha')}")
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


def _finding_lines(review):
    if not review or not review.get("findings"):
        return ["", "violated criteria: no per-criterion findings were recorded"]
    lines = ["", "violated criteria:"]
    for item in review["findings"]:
        rendered = f"  {item['id']}: {item.get('verdict')}"
        if item.get("note"):
            rendered += f" - {item['note']}"
        lines.append(rendered)
        if item.get("anchor"):
            lines.append(f"    anchor: {item['anchor']}")
    return lines


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
    lines.append("  preserve: everything outside the findings above, including work this")
    lines.append("    request does not mention and any other task in-flight beside it")
    return lines


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
