"""The durable outbox between a verification decision and its Linear summary.

Nothing here performs a network call. The relay records WHAT must be written and WHICH decision
it belongs to; the caller that already holds an authenticated connector does the writing and
hands the result back. That separation is what lets a synchronisation failure be retried on its
own without re-running the verification it describes.

Two things this deliberately does NOT claim.

It does not make the external write idempotent. The connector's document save takes no
idempotency key, so a write can succeed and lose its response. A deterministic local row stops
US from queueing the same summary twice; it cannot stop a second append from landing in the
document. That is what reconcile() is for, and it runs BEFORE any rewrite rather than after a
hopeful retry.

It does not confuse two different failures. Enqueueing is a LOCAL write inside the verdict's own
transaction: if it fails, that transaction rolls back and the caller sees it, because swallowing
it would contradict the durability this design rests on. The EXTERNAL write happens later, in
another process, and its failure never touches a committed verdict. Only the second one is meant
by "a synchronisation failure never fails a verification".
"""

import hashlib
import json
import secrets

from .errors import AckRefused, RefusalReason

PENDING = "pending"
CLAIMED = "claimed"
WRITTEN = "written"
CONFIRMED = "confirmed"
FAILED = "failed"

VERDICT = "verdict"
PROGRESS = "progress"

COORDINATION_DOCUMENT = "coordination_document"

MAX_ATTEMPTS = 8
LEASE_SECONDS = 300.0
BASE_BACKOFF = 30.0
MAX_BACKOFF = 900.0

# EVERY header the pre-v2 renderer emitted, each matched exactly. Substring matching is unsafe in
# both directions: "verified" occurs inside "unverified", and an event id in one block plus a
# revision hash in another block is not one matching record.
#
# issueKey and relationshipId belong here because that renderer wrote them, which the real
# readback shows, and validating a subset accepted a block wearing another issue's label as this
# job's record. identityDigest does not close that: the block carries it as a copied header that
# nothing recomputes, and issueKey is not in the canonical payload at all.
IDENTITY_FIELDS = (
    "syncId", "subjectKind", "issueKey", "relationshipId", "eventId", "executionGeneration",
    "revisionHash", "disposition", "identityDigest",
)


def _canonical(target, target_ref, subject_kind, relationship_id, event_id, generation,
               revision, verdict, criteria_digest=None, ruling=None) -> str:
    def render(value):
        return "null" if value is None else str(value)

    payload = "|".join(render(v) for v in (
        target, target_ref, subject_kind, relationship_id, event_id, generation, revision,
        verdict,
    ))
    # Appended only for values actually supplied. With neither, this is byte-for-byte the string
    # this function has always produced, so every id computed without them - progress jobs, and
    # verdicts on relationships with no canonical criteria - is exactly the id it was before.
    # Nothing recomputes identity for a stored row, so those rows stay addressable either way;
    # what this preserves is that a caller which has not changed keeps producing the same id.
    #
    # Labelled rather than positional, because an unlabelled trailing segment could not be told
    # from the other one when only the second is present. This is labelling and NOT escaping: it
    # is sound because both producers are relay-generated - a sha256 hexdigest and an integer -
    # so no delimiter can reach here from a caller. A producer of arbitrary text would need a
    # real encoding, for the reason set_digest gives for canonical JSON over joined fields.
    for label, value in (("criteria", criteria_digest), ("ruling", ruling)):
        if value is not None:
            payload += f"|{label}={render(value)}"
    return payload


def sync_id(target, target_ref, subject_kind, relationship_id, event_id=None, generation=None,
            revision=None, verdict=None, criteria_digest=None, ruling=None) -> str:
    """Identity includes the actual target DOCUMENT, not just the target kind.

    The same verdict written to two different documents is two different jobs, and a completion
    validated against the wrong document validated nothing.

    It also includes WHICH criteria the ruling rests on and WHICH ruling it was, because the
    other seven values are all unchanged by a re-review: same event, same generation, same
    revision, and - when the second reading reaches the same conclusion - the same verdict. With
    only those, a re-review that ruled against newly edited criteria produced the id that already
    existed, INSERT OR IGNORE dropped it, and the document kept the summary written against the
    earlier wording. The digest alone is not enough either: it names the criteria SET, not the
    occasion, so a set edited away and then back would recompute the first ruling's id. The
    ordinal is what distinguishes the occasions.
    """
    payload = _canonical(target, target_ref, subject_kind, relationship_id, event_id,
                         generation, revision, verdict, criteria_digest, ruling)
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()[:32]


def identity_digest(target, target_ref, subject_kind, relationship_id, event_id=None,
                    generation=None, revision=None, verdict=None, criteria_digest=None,
                    ruling=None) -> str:
    """The same payload as sync_id, hashed whole rather than truncated.

    They take the same arguments on purpose: the 32-hex id and the 64-hex digest the block
    carries must never describe different inputs.
    """
    payload = _canonical(target, target_ref, subject_kind, relationship_id, event_id,
                         generation, revision, verdict, criteria_digest, ruling)
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def start_marker(identifier: str) -> str:
    return f"<!-- relay-sync:{identifier} -->"


def end_marker(identifier: str) -> str:
    return f"<!-- /relay-sync:{identifier} -->"


def container_start(relationship_id: str) -> str:
    return f"<!-- relay-sync-container:{relationship_id} -->"


def container_end(relationship_id: str) -> str:
    return f"<!-- /relay-sync-container:{relationship_id} -->"


SUMMARY_SEPARATOR = ""

BLOCK_FORMAT = "v2"
FENCE_CHARACTER = "`"
MINIMUM_FENCE = 3

# Exactly what render_block emits, in order. v2 validates the WHOLE section against this, so a
# header the renderer writes can never go unchecked and a header it does not write can never
# appear. issueKey and relationshipId were rendered but not validated before.
RENDERED_FIELDS = (
    "blockFormat", "syncId", "subjectKind", "issueKey", "relationshipId", "eventId",
    "executionGeneration", "revisionHash", "disposition", "identityDigest", "summarySha256",
)

V2 = "v2"
FENCED_LEGACY = "fenced-legacy"
LEGACY = "legacy"
# A block that names a format this renderer does not know. It is not revalidated under the
# pre-v2 rules: reading a block leniently BECAUSE its declared version is unreadable would be
# the silent downgrade that declaring a version exists to prevent.
UNSUPPORTED = "unsupported"


def canonical_summary(text) -> str:
    """The exact bytes a block carries: newlines normalised, trailing blank lines dropped.

    The digest describes THIS, so a round trip either reproduces it exactly or fails loudly.
    """
    if not text:
        return ""
    return str(text).replace("\r\n", "\n").replace("\r", "\n").rstrip("\n")


def summary_fence(summary: str) -> str:
    """A fence longer than any backtick run inside the summary, so nothing can close it early.

    Findings in this project quote inline code and whole fenced blocks, so a fixed three-backtick
    fence would be terminated by its own content. CommonMark closes a fence only on a run at
    least as long as the opener, which is exactly the property being used here.
    """
    longest = current = 0
    for character in summary:
        current = current + 1 if character == FENCE_CHARACTER else 0
        longest = max(longest, current)
    return FENCE_CHARACTER * max(MINIMUM_FENCE, longest + 1)


def render_block(row) -> str:
    """One job owns one block, delimited by a marker derived from its stable sync id.

    The summary is written INSIDE a fenced literal block. It used to be plain body text, and the
    connector normalised it: a bare filename became an autolink, brackets and asterisks were
    escaped, and the confirmation was correctly refused because the record no longer matched. A
    fence preserves it byte-for-byte, which was verified against the real document.
    """
    summary = canonical_summary(row["summary"])
    fence = summary_fence(summary)
    fields = [
        ("blockFormat", BLOCK_FORMAT),
        ("syncId", row["sync_id"]),
        ("subjectKind", row["subject_kind"]),
        ("issueKey", row["issue_key"]),
        ("relationshipId", row["relationship_id"]),
        ("eventId", row["event_id"]),
        ("executionGeneration", row["execution_generation"]),
        ("revisionHash", row["revision_hash"]),
        ("disposition", row["verdict"]),
        ("identityDigest", row["identity_digest"]),
        ("summarySha256", hashlib.sha256(summary.encode("utf-8")).hexdigest()),
    ]
    lines = [start_marker(row["sync_id"])]
    lines += [f"{key}: {'' if value is None else value}" for key, value in fields]
    lines.append("")
    lines.append(fence + "text")
    lines.extend(summary.split("\n"))
    lines.append(fence)
    lines.append(end_marker(row["sync_id"]))
    return "\n".join(lines)


def _fence_length(line: str):
    """How many backticks open or close a fence on this line, or None if it is not a fence."""
    stripped = line.strip()
    count = 0
    while count < len(stripped) and stripped[count] == FENCE_CHARACTER:
        count += 1
    if count < MINIMUM_FENCE:
        return None
    info = stripped[count:]
    if FENCE_CHARACTER in info:
        return None
    return count, info


def parse_document(text: str) -> dict:
    """Every relay block, plus what is WRONG with the document.

    Read line by line, because three things depend on structure rather than on searching the
    whole body. Identity fields come ONLY from the header region, so a finding that happens to
    write "eventId: something" cannot supply or overwrite one. The summary is taken from the
    fenced region when there is one. And the end marker is looked for only AFTER that fence
    closes, so a finding quoting relay marker text does not truncate the block.

    Blank lines before the end marker are tolerated because the connector inserts one; that is
    not a nicety, it is what the document that actually passed looks like.

    Duplicate and unterminated blocks stay observable: a duplicate is not one unique record, and
    a partial marker is not proof that nothing landed.
    """
    blocks, duplicates, malformed = {}, [], []
    if not text:
        return {"blocks": blocks, "duplicates": duplicates, "malformed": malformed}

    lines = text.split("\n")
    index = 0
    while index < len(lines):
        stripped = lines[index].strip()
        if not (stripped.startswith("<!-- relay-sync:") and stripped.endswith("-->")):
            index += 1
            continue
        identifier = stripped[len("<!-- relay-sync:"):-len("-->")].strip()
        closing_line = end_marker(identifier)
        start_index = index
        index += 1

        fields, problems, seen = {}, [], set()
        while index < len(lines) and lines[index].strip() and lines[index].strip() != closing_line:
            key, separator, value = lines[index].partition(":")
            key = key.strip()
            if separator and key and " " not in key:
                if key in seen:
                    problems.append(f"duplicate header {key!r}")
                seen.add(key)
                fields[key] = value.strip()
            else:
                problems.append("the header region contains a line that is not a header")
            index += 1

        while index < len(lines) and not lines[index].strip():
            index += 1

        summary, block_format = None, LEGACY
        declared = fields.get("blockFormat")
        fence = _fence_length(lines[index]) if index < len(lines) else None
        if fence is not None:
            length, _info = fence
            index += 1
            collected = []
            closed = False
            while index < len(lines):
                candidate = _fence_length(lines[index])
                if candidate is not None and candidate[0] >= length and not candidate[1]:
                    closed = True
                    index += 1
                    break
                collected.append(lines[index])
                index += 1
            summary = "\n".join(collected) if closed else None
            if not closed:
                problems.append("the fenced summary is not closed")
            block_format = FENCED_LEGACY

        # A block that names its own format is held to THAT format. Inferring the shape and then
        # picking rules to match it means the more structure a block loses, the less it is
        # checked, which is backwards. An undeclared block keeps its existing treatment, because
        # the pre-v2 renderer wrote no blockFormat header and one such block is already confirmed.
        if declared is not None:
            if declared != BLOCK_FORMAT:
                block_format = UNSUPPORTED
                problems.append(f"unsupported block format {declared!r}")
            else:
                block_format = V2
                if summary is None:
                    problems.append("a v2 block must carry a closed fenced summary")

        while index < len(lines) and not lines[index].strip():
            index += 1

        if index >= len(lines) or lines[index].strip() != closing_line:
            # Fall back to the flat search so a legacy block, whose summary is plain body text,
            # is still found and still inspectable.
            tail = "\n".join(lines[start_index:])
            offset = tail.find(closing_line)
            if offset == -1:
                if identifier not in malformed:
                    malformed.append(identifier)
                index = start_index + 1
                continue
            consumed = tail[:offset + len(closing_line)]
            body_text = consumed[consumed.find("-->") + 3:-len(closing_line)]
            if summary is not None:
                # A fenced summary closed, yet the end marker was not the next thing after it.
                # Only whitespace may sit in between, or text smuggled in behind the fence would
                # ride along unread while the block still validated.
                problems.append(
                    "the block carries content between its fenced summary and its end marker"
                )
            if summary is None:
                summary = body_text.strip("\n")
            record_text = consumed
            index = start_index + consumed.count("\n") + 1
        else:
            if summary is None:
                summary = "\n".join(lines[start_index + 1:index]).strip("\n")
            record_text = "\n".join(lines[start_index:index + 1])
            index += 1

        record = {
            "fields": fields,
            "text": record_text,
            "body": record_text,
            "summary": summary,
            "format": block_format,
            "problems": problems,
        }
        if identifier in blocks and identifier not in duplicates:
            duplicates.append(identifier)
        blocks[identifier] = record
    return {"blocks": blocks, "duplicates": duplicates, "malformed": malformed}


def parse_blocks(text: str) -> dict:
    """Just the blocks, for a caller that has already checked the document is sound."""
    return parse_document(text)["blocks"]



class SyncOutbox:
    def __init__(self, store, clock):
        self.store = store
        self.clock = clock

    # ------------------------------------------------------------------ target

    def set_target(self, relationship_id, target, target_ref) -> dict:
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO sync_targets (relationship_id, target, target_ref, recorded_at)"
                " VALUES (?,?,?,?)"
                " ON CONFLICT(relationship_id, target) DO UPDATE SET"
                "   target_ref = excluded.target_ref, recorded_at = excluded.recorded_at",
                (relationship_id, target, target_ref, now),
            )
        return {"relationshipId": relationship_id, "target": target, "targetRef": target_ref}

    def target_for(self, relationship_id, target=COORDINATION_DOCUMENT):
        row = self.store.one(
            "SELECT * FROM sync_targets WHERE relationship_id = ? AND target = ?",
            (relationship_id, target),
        )
        return dict(row) if row else None

    # ----------------------------------------------------------------- enqueue

    def enqueue_in(self, db, *, relationship_id, issue_key, subject_kind, summary,
                   event_id=None, generation=None, revision=None, verdict=None,
                   target=COORDINATION_DOCUMENT, criteria_digest=None, ruling=None):
        """Inside the caller's transaction, so the decision and its owed summary commit together.

        Returns the sync id, or None when no target is registered: an unconfigured target is not
        an error. A local failure here propagates and rolls the caller's transaction back, which
        is the honest behaviour for a write this design promises to be durable.

        criteria_digest and ruling are not validated here, deliberately. Refusing a malformed one
        would let a synchronisation concern refuse a verification, which is the one thing this
        module promises never to do; the labelled payload is what keeps the shapes apart instead.
        """
        configured = self.target_for(relationship_id, target)
        if configured is None:
            return None
        target_ref = configured["target_ref"]
        identifier = sync_id(target, target_ref, subject_kind, relationship_id, event_id,
                             generation, revision, verdict, criteria_digest, ruling)
        digest = identity_digest(target, target_ref, subject_kind, relationship_id, event_id,
                                 generation, revision, verdict, criteria_digest, ruling)
        now = self.clock.iso()
        cursor = db.execute(
            "INSERT OR IGNORE INTO sync_outbox (sync_id, relationship_id, issue_key, target,"
            " target_ref, subject_kind, event_id, execution_generation, revision_hash, verdict,"
            " identity_digest, summary, state, attempts, next_attempt_at, created_at,"
            " updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0,NULL,?,?)",
            (
                identifier, relationship_id, issue_key, target, target_ref, subject_kind,
                event_id, generation, revision, verdict, digest, summary, PENDING, now, now,
            ),
        )
        # "inserted" because INSERT OR IGNORE can do nothing, and an entry that says enqueued
        # either way claims a row that may not exist. The criteria digest is recorded here for
        # the same reason it is not recorded in a column: the store has no migration path, and
        # verdict_context - the other place it lives - is overwritten by the next re-review,
        # while the journal is append-only. It cannot be recovered from the identity hash.
        self.store.journal(
            "sync_enqueued", identifier,
            {"subjectKind": subject_kind, "eventId": event_id, "criteriaDigest": criteria_digest,
             "ruling": ruling, "inserted": cursor.rowcount == 1},
            at=now,
        )
        return identifier

    def enqueue_verdict_in(self, db, *, relationship, event, verdict, findings, record,
                           criteria_digest=None, ruling=None):
        return self.enqueue_in(
            db,
            relationship_id=relationship["relationshipId"],
            issue_key=relationship["issueKey"],
            subject_kind=VERDICT,
            summary=render_verdict_summary(relationship, event, verdict, findings, record,
                                           criteria_digest=criteria_digest),
            event_id=event["event_id"],
            generation=event["execution_generation"],
            revision=event["revision_hash"],
            verdict=verdict,
            criteria_digest=criteria_digest,
            ruling=ruling,
        )

    # ------------------------------------------------------------------ queue

    def get(self, identifier):
        row = self.store.one("SELECT * FROM sync_outbox WHERE sync_id = ?", (identifier,))
        if row is None:
            raise AckRefused(
                RefusalReason.SYNC_NOT_CLAIMABLE, f"no synchronisation job {identifier!r}"
            )
        return row

    def next(self, *, target=None, limit=4, now=None) -> list:
        now = self.clock.now() if now is None else now
        sql = (
            # CLAIMED is included on purpose: a writer that died holding a lease leaves a job
            # nobody can discover otherwise, and a restarted writer has to be able to find and
            # reconcile it. The lease condition below is what stops a live claim being stolen.
            "SELECT * FROM sync_outbox WHERE state IN (?,?,?)"
            "   AND (next_attempt_at IS NULL OR next_attempt_at <= ?)"
            "   AND (lease_until IS NULL OR lease_until <= ?)"
        )
        params = [PENDING, WRITTEN, CLAIMED, now, now]
        if target:
            sql += " AND target = ?"
            params.append(target)
        sql += " ORDER BY created_at LIMIT ?"
        params.append(limit)
        return [dict(row) for row in self.store.all(sql, tuple(params))]

    def claim(self, identifier, *, owner, now=None) -> dict:
        """A lease plus a per-claim token, which is what fences complete and fail."""
        now = self.clock.now() if now is None else now
        token = secrets.token_hex(16)
        with self.store.transaction() as db:
            row = db.execute(
                "SELECT * FROM sync_outbox WHERE sync_id = ?", (identifier,)
            ).fetchone()
            if row is None:
                raise AckRefused(
                    RefusalReason.SYNC_NOT_CLAIMABLE, f"no synchronisation job {identifier!r}"
                )
            if row["state"] == CONFIRMED:
                raise AckRefused(
                    RefusalReason.SYNC_NOT_CLAIMABLE,
                    f"{identifier!r} is already confirmed",
                )
            if row["state"] == FAILED:
                # The bounded retry policy is not advisory. A failed job is resumed by an
                # explicit retry, never by a caller that simply asks for it again.
                raise AckRefused(
                    RefusalReason.SYNC_NOT_CLAIMABLE,
                    f"{identifier!r} is failed after {row['attempts']} attempts; call retry to "
                    "resume it deliberately",
                )
            if row["next_attempt_at"] is not None and row["next_attempt_at"] > now:
                # Validated here and not only in next(), because claim is reachable directly.
                raise AckRefused(
                    RefusalReason.SYNC_NOT_CLAIMABLE,
                    f"{identifier!r} is backing off until {row['next_attempt_at']}",
                )
            if row["lease_until"] is not None and row["lease_until"] > now:
                raise AckRefused(
                    RefusalReason.SYNC_NOT_CLAIMABLE,
                    f"{identifier!r} is leased by {row['lease_owner']!r} until "
                    f"{row['lease_until']}",
                )
            db.execute(
                "UPDATE sync_outbox SET state = ?, lease_owner = ?, lease_until = ?,"
                " claim_token = ?, updated_at = ? WHERE sync_id = ?",
                (CLAIMED, owner, now + LEASE_SECONDS, token, self.clock.iso(), identifier),
            )
        return {"syncId": identifier, "claimToken": token, "owner": owner,
                "leaseUntil": now + LEASE_SECONDS}

    def _fenced(self, db, identifier, claim_token):
        row = db.execute(
            "SELECT * FROM sync_outbox WHERE sync_id = ?", (identifier,)
        ).fetchone()
        if row is None:
            raise AckRefused(
                RefusalReason.SYNC_NOT_CLAIMABLE, f"no synchronisation job {identifier!r}"
            )
        if row["claim_token"] != claim_token:
            # An old claimant must not be able to undo what a newer one already confirmed.
            raise AckRefused(
                RefusalReason.SYNC_NOT_CLAIMABLE,
                f"this claim token is not the one currently held for {identifier!r}",
            )
        return row

    # ------------------------------------------------------------- the packet

    def operation(self, identifier) -> dict:
        """The exact thing to execute, including the protocol, not an abstract verb."""
        row = self.get(identifier)
        return {
            "syncId": row["sync_id"],
            "target": row["target"],
            "targetRef": row["target_ref"],
            "issueKey": row["issue_key"],
            "subjectKind": row["subject_kind"],
            "identity": {
                "eventId": row["event_id"],
                "executionGeneration": row["execution_generation"],
                "revisionHash": row["revision_hash"],
                "disposition": row["verdict"],
            },
            "identityDigest": row["identity_digest"],
            "startMarker": start_marker(row["sync_id"]),
            "endMarker": end_marker(row["sync_id"]),
            "containerStartMarker": container_start(row["relationship_id"]),
            "containerEndMarker": container_end(row["relationship_id"]),
            "block": render_block(row),
            "protocol": [
                "read the target document and locate this relationship's CONTAINER markers",
                "container absent: this is initialisation. Create it once, with an empty body,"
                " before any job writes. If that response is lost, do not retry blindly: read"
                " again and reconcile, because a negative read is not proof of non-delivery"
                " while an earlier write can still land",
                "container present: every write, INCLUDING this job's first block, is a"
                " conditional replacement of the container's exact current text with its new"
                " text. Never a bare append: an append is unconditional, so a slow writer whose"
                " lease expired can still land a second copy after another writer appended",
                "this job's block present with a matching record: the write already landed;"
                " complete from that observation and do NOT write again",
                "present with a different record, or duplicated, or unterminated: reconcile"
                " reports duplicate or malformed; repair the container in one conditional"
                " replacement rather than appending over it",
                "read the document back and pass the full text to complete",
            ],
            "note": "the connector's document save takes no idempotency key, so the marker read"
                    " plus conditional replacement of an owned container is what makes a retry"
                    " safe, not a request id. Local claim fencing cannot retract a remote write"
                    " that was already issued, which is why insertion is conditional too.",
            "formatEvidence": "HTML relay-sync markers survive a real Linear document write and"
                              " readback byte-for-byte; see JUN-92-linear-format-probe.json",
        }

    # --------------------------------------------------------- reconciliation

    def reconcile(self, identifier, observed_text) -> dict:
        """Did this job's write already land? Answered from the document, before rewriting."""
        row = self.get(identifier)
        document = parse_document(observed_text)
        if row["sync_id"] in document["malformed"]:
            # A partial marker is not proof that nothing landed. Appending over it would write
            # this summary twice.
            return {"syncId": identifier, "outcome": "malformed",
                    "detail": "an unterminated block for this job is present; the previous "
                              "write's effect is uncertain and must not be appended over"}
        if row["sync_id"] in document["duplicates"]:
            return {"syncId": identifier, "outcome": "duplicate",
                    "detail": "more than one block for this job is present; that is not one "
                              "successful unique record and must be repaired before confirming"}
        found = document["blocks"].get(row["sync_id"])
        if found is None:
            return {"syncId": identifier, "outcome": "absent",
                    "detail": "no block for this job was observed. A negative read is not "
                              "affirmative non-delivery while an earlier write may still land, "
                              "so write only through the conditional container replacement"}
        mismatch = self._payload_mismatch(row, found)
        if mismatch:
            return {"syncId": identifier, "outcome": "stale", "mismatch": mismatch,
                    "previousBlock": found["text"],
                    "detail": "a block exists but describes something else; replace it"}
        return {"syncId": identifier, "outcome": "already_written",
                "previousBlock": found["text"],
                "detail": "this job's write landed; complete it without writing again"}

    @staticmethod
    def _identity_mismatch(row, fields) -> list:
        expected = {
            "syncId": row["sync_id"],
            "subjectKind": row["subject_kind"],
            "issueKey": row["issue_key"],
            "relationshipId": row["relationship_id"],
            "eventId": "" if row["event_id"] is None else row["event_id"],
            "executionGeneration": (
                "" if row["execution_generation"] is None
                else str(row["execution_generation"])
            ),
            "revisionHash": "" if row["revision_hash"] is None else row["revision_hash"],
            "disposition": "" if row["verdict"] is None else row["verdict"],
            "identityDigest": row["identity_digest"],
        }
        problems = []
        for key in IDENTITY_FIELDS:
            actual = fields.get(key)
            if actual is None:
                problems.append(f"{key} is missing")
            elif actual != expected[key]:
                problems.append(f"{key} is {actual!r}, expected {expected[key]!r}")
        return problems

    def _payload_mismatch(self, row, found) -> list:
        """Identity AND the payload it claims to carry.

        Exact identity headers do not establish that the summary actually arrived: a block
        carrying the right digests and none of the findings is not the record this job owes.
        """
        problems = list(found.get("problems") or [])
        if found.get("format") == UNSUPPORTED:
            # Nothing further can be checked honestly: the relay does not know what this block
            # claims to be. reconcile still returns its exact text, so the repair is the ordinary
            # conditional replacement of the owned container.
            return problems
        if found.get("format") == V2:
            # Written by this renderer, so it is held to exactly what this renderer emits.
            return problems + self._v2_mismatch(row, found)
        # A block written before the fenced format, including the hand-repaired one the connector
        # already confirmed. Every header ITS renderer emitted is validated, so a relabelled block
        # is refused here too, and header-region parsing already stops its summary from supplying
        # one. What is not applied backwards is the exact-key-set rule and the summary digest:
        # both describe text only this renderer writes, so enforcing them on an older block would
        # refuse on a guess about headers this codebase never wrote.
        problems += self._identity_mismatch(row, found["fields"])
        summary = (row["summary"] or "").strip()
        carried = found.get("summary")
        haystack = carried if carried is not None else found["body"]
        if summary and summary not in haystack:
            problems.append("the block does not carry this job's summary text")
        return problems

    def _v2_mismatch(self, row, found) -> list:
        """The WHOLE rendered identity section, then the summary, then its digest.

        Validating a subset was its own hazard: issueKey and relationshipId were rendered and
        never checked, so a block could carry the wrong ones and still pass. Both are now
        checked on every shape; what is specific to v2 is the EXACT key set, which lets no
        header this renderer writes go unchecked and no header it does not write appear, and
        the summary digest.
        """
        summary = canonical_summary(row["summary"])
        expected = {
            "blockFormat": BLOCK_FORMAT,
            "syncId": row["sync_id"],
            "subjectKind": row["subject_kind"],
            "issueKey": row["issue_key"],
            "relationshipId": row["relationship_id"],
            "eventId": "" if row["event_id"] is None else row["event_id"],
            "executionGeneration": (
                "" if row["execution_generation"] is None else str(row["execution_generation"])
            ),
            "revisionHash": "" if row["revision_hash"] is None else row["revision_hash"],
            "disposition": "" if row["verdict"] is None else row["verdict"],
            "identityDigest": row["identity_digest"],
            "summarySha256": hashlib.sha256(summary.encode("utf-8")).hexdigest(),
        }
        fields = found["fields"]
        problems = []
        for key in RENDERED_FIELDS:
            actual = fields.get(key)
            if actual is None:
                problems.append(f"{key} is missing")
            elif actual != expected[key]:
                problems.append(f"{key} is {actual!r}, expected {expected[key]!r}")
        unexpected = sorted(set(fields) - set(RENDERED_FIELDS))
        if unexpected:
            problems.append(f"the header region carries unexpected headers {unexpected}")
        carried = found.get("summary")
        if carried is None:
            problems.append("the fenced summary could not be read")
        elif carried != summary:
            problems.append("the fenced summary is not this job's summary text")
        return problems

    # ----------------------------------------------------------- settle

    def complete(self, identifier, *, claim_token, target_ref, readback, external_ref=None,
                 now=None) -> dict:
        """Confirmed only against this job's own block, in the intended document, by exact field.

        Confirmation is MONOTONIC. A later call cannot move a confirmed job backwards, even
        with the token that confirmed it: a bad readback arriving after a good one is a
        question about the document, not grounds to un-confirm what was already observed.

        One fenced transaction does everything. Recording the failure in a second transaction
        left a window in which this call's failure could overwrite a newer claimant's
        confirmation, so the write happens under the same fence and the refusal is raised only
        after it commits.
        """
        now = self.clock.now() if now is None else now
        stamp = self.clock.iso()
        problems = None
        with self.store.transaction() as db:
            row = self._fenced(db, identifier, claim_token)
            if row["state"] == CONFIRMED:
                return dict(row)
            if row["target_ref"] != target_ref:
                raise AckRefused(
                    RefusalReason.SYNC_TARGET_MISMATCH,
                    f"this job targets {row['target_ref']!r}, not {target_ref!r}; validating "
                    "the right text in the wrong document validates nothing",
                )
            document = parse_document(readback)
            found = document["blocks"].get(row["sync_id"])
            if row["sync_id"] in document["malformed"]:
                problems = ["an unterminated block for this job is present, so what landed is "
                            "uncertain"]
            elif row["sync_id"] in document["duplicates"]:
                problems = ["more than one block for this job is present, which is not one "
                            "successful unique record"]
            elif found is None:
                problems = ["the readback carries no block for this job"]
            else:
                problems = self._payload_mismatch(row, found) or None
            if problems is None:
                db.execute(
                "UPDATE sync_outbox SET state = ?, external_ref = ?, readback = ?,"
                " written_at = COALESCE(written_at, ?), confirmed_at = ?, last_error = NULL,"
                " next_attempt_at = NULL, lease_owner = NULL, lease_until = NULL,"
                " updated_at = ? WHERE sync_id = ?",
                (
                    CONFIRMED, external_ref, found["text"], stamp, stamp, stamp, identifier,
                ),
                )
                self.store.journal(
                    "sync_confirmed", identifier, {"targetRef": target_ref}, at=stamp
                )
            else:
                db.execute(
                    "UPDATE sync_outbox SET state = ?, last_error = ?, lease_owner = NULL,"
                    " lease_until = NULL, updated_at = ?"
                    " WHERE sync_id = ? AND claim_token = ? AND state != ?",
                    (
                        WRITTEN, "; ".join(problems), stamp, identifier, claim_token, CONFIRMED,
                    ),
                )
        if problems is not None:
            raise AckRefused(
                RefusalReason.READBACK_MISMATCH,
                "the readback does not carry this job's record: " + "; ".join(problems),
            )
        return dict(self.get(identifier))

    def fail(self, identifier, *, claim_token, error, now=None) -> dict:
        """Touches nothing outside this table. No verification is re-run and nothing is resent."""
        now = self.clock.now() if now is None else now
        stamp = self.clock.iso()
        with self.store.transaction() as db:
            row = self._fenced(db, identifier, claim_token)
            if row["state"] == CONFIRMED:
                raise AckRefused(
                    RefusalReason.SYNC_NOT_CLAIMABLE,
                    f"{identifier!r} is confirmed; a later failure cannot undo it",
                )
            attempts = row["attempts"] + 1
            state = FAILED if attempts >= MAX_ATTEMPTS else PENDING
            delay = min(MAX_BACKOFF, BASE_BACKOFF * (2 ** max(0, attempts - 1)))
            db.execute(
                "UPDATE sync_outbox SET state = ?, attempts = ?, last_error = ?,"
                " next_attempt_at = ?, lease_owner = NULL, lease_until = NULL,"
                " claim_token = NULL, updated_at = ? WHERE sync_id = ?",
                (state, attempts, str(error), now + delay, stamp, identifier),
            )
            self.store.journal(
                "sync_failed", identifier, {"attempts": attempts, "state": state}, at=stamp
            )
        return dict(self.get(identifier))

    def retry(self, identifier) -> dict:
        stamp = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "UPDATE sync_outbox SET state = ?, next_attempt_at = NULL, lease_owner = NULL,"
                " lease_until = NULL, claim_token = NULL, updated_at = ?"
                " WHERE sync_id = ? AND state != ?",
                (PENDING, stamp, identifier, CONFIRMED),
            )
        return dict(self.get(identifier))

    def snapshot(self, *, relationship_id=None) -> dict:
        sql = "SELECT * FROM sync_outbox"
        params = ()
        if relationship_id:
            sql += " WHERE relationship_id = ?"
            params = (relationship_id,)
        rows = self.store.all(sql + " ORDER BY created_at", params)
        return {
            "jobs": [
                {
                    "syncId": r["sync_id"], "target": r["target"], "targetRef": r["target_ref"],
                    "subjectKind": r["subject_kind"], "eventId": r["event_id"],
                    "executionGeneration": r["execution_generation"],
                    "revisionHash": r["revision_hash"], "disposition": r["verdict"],
                    "state": r["state"], "attempts": r["attempts"],
                    "lastError": r["last_error"], "nextAttemptAt": r["next_attempt_at"],
                    "confirmedAt": r["confirmed_at"],
                }
                for r in rows
            ],
            "pending": sum(1 for r in rows if r["state"] in (PENDING, CLAIMED, WRITTEN)),
            "failed": sum(1 for r in rows if r["state"] == FAILED),
            "confirmed": sum(1 for r in rows if r["state"] == CONFIRMED),
        }


def render_verdict_summary(relationship, event, verdict, findings, record,
                           criteria_digest=None) -> str:
    lines = [
        f"{relationship['issueKey']} · {relationship['child']['taskId']} · {verdict}",
        f"generation {event['execution_generation']}, revision {event['revision_hash'][:12]}",
    ]
    if criteria_digest:
        # Which wording the judgment rests on. Identity already separates two rulings made
        # against different criteria, but a person reading the document sees only the summary,
        # and two blocks reaching the same disposition are indistinguishable without this.
        lines.append(f"criteria set {criteria_digest[:12]}")
    if record.get("nextExecutionGeneration"):
        lines.append(
            f"a revision request was queued to the same child under generation "
            f"{record['nextExecutionGeneration']}"
        )
    if findings:
        lines.append("findings:")
        for finding in findings:
            note = finding.get("note")
            lines.append(
                f"  {finding['id']}: {finding['verdict']}" + (f" — {note}" if note else "")
            )
    return "\n".join(lines)


def render_progress_summary(assignment) -> str:
    head = assignment["head"]
    return "\n".join([
        f"{assignment['issueKey']} · {assignment['childTaskId']} · {assignment['state']}",
        f"generation {assignment['executionGeneration']}, next: "
        f"{assignment['nextExpectedAction']}",
        f"current revision: {(head['revisionHash'] or 'none')[:12]} ({head['evidence']})",
    ])
