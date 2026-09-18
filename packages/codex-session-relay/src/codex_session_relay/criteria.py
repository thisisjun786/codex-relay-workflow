"""The canonical criteria a delivery is judged against, and the set a review was bound to.

Held here rather than in the verdict record because the five contract schemas are frozen with
additionalProperties false, so contract v1 has no slot for a criteria set. This is a relay-owned
record, like the revision request.

Two modes, named rather than implied. A MANAGED assignment is one somebody registered criteria
for, or declared managed: it cannot be completed as verified against nothing, because "verified
against nothing" is the claim this workflow exists to prevent. A LEGACY relationship is the
generic relay with no criteria contract, and it keeps the delivered behaviour exactly. The mode
is stored rather than inferred from whether a set happens to exist, because an absent set on a
managed assignment is precisely the case that must refuse.
"""

import hashlib
import json

from . import restoration
from .errors import AckRefused, RefusalReason

DISPOSITIONS = ("verified", "needs_changes", "unverified")
MANAGED = "managed"
LEGACY = "legacy"
MODES = (MANAGED, LEGACY)

COVERED = "covered"
LEGACY_UNREGISTERED = "legacy_unregistered"


def set_digest(entries) -> str:
    """sha256 over a canonical JSON serialisation, not over joined text.

    Delimiter-joined id|title|required cannot be made collision-free by escaping rules nobody
    will remember: a title containing the delimiter silently becomes a different set with the
    same digest. JSON quoting already escapes every delimiter, so two different sets cannot
    serialise identically.
    """
    payload = json.dumps(
        [
            {"id": e["id"], "title": e["title"], "required": bool(e.get("required", True))}
            for e in sorted(entries, key=lambda e: e["id"])
        ],
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def normalise_findings(criteria=None, findings=None) -> list:
    """One shape for what the CLI and the API both call findings.

    criteria is the delivered core's simple form, [{"id", "verdict"}]. findings adds a note.
    Both end up here, and a disposition outside the frozen enum is refused before anything is
    written, because a record that would fail conformance must never reach the store.

    A finding may also declare that it carries the correction's restoration block. That is the
    one place the block can travel, so which finding holds it has to survive this
    normalisation rather than being dropped with every other unrecognised key.
    """
    merged = []
    for source in (criteria or [], findings or []):
        for item in source:
            if not isinstance(item, dict):
                raise AckRefused(
                    RefusalReason.DISPOSITION_CONFLICT, "each finding is an object"
                )
            identifier = str(item.get("id") or "").strip()
            if not identifier:
                raise AckRefused(
                    RefusalReason.DISPOSITION_CONFLICT, "each finding names a criterion id"
                )
            disposition = item.get("verdict") or "verified"
            if disposition not in DISPOSITIONS:
                raise AckRefused(
                    RefusalReason.DISPOSITION_CONFLICT,
                    f"{disposition!r} is not one of {DISPOSITIONS}; the contract's criteria "
                    "enum is frozen and a finding outside it cannot be recorded",
                )
            entry = {"id": identifier, "verdict": disposition}
            note = str(item.get("note") or "").strip()
            if note:
                entry["note"] = note
            flag = item.get(restoration.FIELD)
            if flag is not None and not isinstance(flag, bool):
                raise AckRefused(
                    RefusalReason.DISPOSITION_CONFLICT,
                    f"a finding declares its restoration block with true or false, not "
                    f"{type(flag).__name__}",
                )
            previous = next((e for e in merged if e["id"] == identifier), None)
            declared_before = bool(previous and previous.get(restoration.FIELD))
            if flag is False and declared_before:
                # Last-entry-wins is right for a disposition and a note and wrong for this.
                # The two inputs are merged by id, so a caller that declared the block in
                # criteria and then described it in findings would have the declaration
                # cancelled by the entry that was only supposed to add the note. A caller
                # that means to cancel it cannot be told apart from one that forgot, so the
                # contradiction is refused rather than guessed either way.
                raise AckRefused(
                    RefusalReason.DISPOSITION_CONFLICT,
                    f"{identifier!r} both declares and disclaims the restoration block; one "
                    "correction carries one block and says so once",
                )
            if flag or declared_before:
                entry[restoration.FIELD] = True
            merged = [e for e in merged if e["id"] != identifier] + [entry]
    carriers = [e["id"] for e in merged if e.get(restoration.FIELD)]
    if len(carriers) > 1:
        raise AckRefused(
            RefusalReason.DISPOSITION_CONFLICT,
            f"{carriers} each declare the restoration block. One correction carries one "
            "block, and two candidates is a block nobody can locate",
        )
    # After the merge, so a note supplied through the other input still counts. The block
    # travels in the note and nowhere else, so a declaration with no note marks an empty
    # carrier: coverage is satisfied by some other actionable finding, the renderer labels the
    # empty one, and the relay reports it carried. Every check downstream tests the boolean,
    # so this is the only place that can tell an intention from a delivery.
    empty = [e["id"] for e in merged if e.get(restoration.FIELD) and not e.get("note")]
    if empty:
        raise AckRefused(
            RefusalReason.DISPOSITION_CONFLICT,
            f"{empty[0]!r} declares the restoration block and carries no note. The block is "
            "the note; a declaration without one names a carrier with nothing in it",
        )
    return merged


class CriteriaService:
    def __init__(self, store, clock):
        self.store = store
        self.clock = clock

    # ------------------------------------------------------------- registration

    def register(self, relationship_id, entries, *, source_ref=None) -> dict:
        normalised, seen = [], set()
        for entry in entries:
            identifier = str(entry.get("id") or "").strip()
            title = str(entry.get("title") or "").strip()
            if not identifier or not title:
                raise AckRefused(
                    RefusalReason.CRITERIA_UNREGISTERED,
                    "each criterion needs a non-empty id and title",
                )
            if identifier in seen:
                raise AckRefused(
                    RefusalReason.CRITERIA_UNREGISTERED,
                    f"duplicate criterion id {identifier!r}",
                )
            seen.add(identifier)
            normalised.append(
                {"id": identifier, "title": title, "required": bool(entry.get("required", True))}
            )
        if not normalised:
            raise AckRefused(
                RefusalReason.CRITERIA_UNREGISTERED,
                "a criteria set needs at least one criterion",
            )
        digest = set_digest(normalised)
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "DELETE FROM canonical_criteria WHERE relationship_id = ?", (relationship_id,)
            )
            for entry in normalised:
                db.execute(
                    "INSERT INTO canonical_criteria (relationship_id, criterion_id, title,"
                    " required, source_ref, set_digest, recorded_at) VALUES (?,?,?,?,?,?,?)",
                    (
                        relationship_id, entry["id"], entry["title"], int(entry["required"]),
                        source_ref, digest, now,
                    ),
                )
            self._write_mode(db, relationship_id, MANAGED, now)
            self.store.journal(
                "criteria_registered", relationship_id,
                {"setDigest": digest, "count": len(normalised)}, at=now,
            )
        return {
            "relationshipId": relationship_id, "setDigest": digest, "sourceRef": source_ref,
            "criteria": normalised, "mode": MANAGED,
        }

    def get(self, relationship_id):
        rows = self.store.all(
            "SELECT * FROM canonical_criteria WHERE relationship_id = ? ORDER BY criterion_id",
            (relationship_id,),
        )
        if not rows:
            return None
        return {
            "relationshipId": relationship_id,
            "setDigest": rows[0]["set_digest"],
            "sourceRef": rows[0]["source_ref"],
            "criteria": [
                {"id": r["criterion_id"], "title": r["title"], "required": bool(r["required"])}
                for r in rows
            ],
        }

    def mode(self, relationship_id) -> str:
        row = self.store.one(
            "SELECT mode FROM verification_mode WHERE relationship_id = ?", (relationship_id,)
        )
        return row["mode"] if row else LEGACY

    def set_mode(self, relationship_id, mode) -> dict:
        if mode not in MODES:
            raise AckRefused(
                RefusalReason.DISPOSITION_CONFLICT, f"unknown verification mode {mode!r}"
            )
        now = self.clock.iso()
        with self.store.transaction() as db:
            self._write_mode(db, relationship_id, mode, now)
        return {"relationshipId": relationship_id, "mode": mode}

    def _write_mode(self, db, relationship_id, mode, now) -> None:
        db.execute(
            "INSERT INTO verification_mode (relationship_id, mode, recorded_at) VALUES (?,?,?)"
            " ON CONFLICT(relationship_id) DO UPDATE SET mode = excluded.mode,"
            " recorded_at = excluded.recorded_at",
            (relationship_id, mode, now),
        )

    # ------------------------------------------------------------------ binding

    def bind_review(self, db, relationship_id, event_id):
        """Record the digest of the set as it stands AT REVIEW START.

        This is what makes editing a criterion's text while keeping its id invalidate the
        review instead of silently passing it: findings made against one wording cannot be
        used to certify a different wording.
        """
        current = self.get(relationship_id)
        digest = current["setDigest"] if current else None
        db.execute(
            "INSERT OR IGNORE INTO claim_context (event_id, set_digest, bound_at) VALUES (?,?,?)",
            (event_id, digest, self.clock.iso()),
        )
        return digest

    def bound_digest(self, event_id):
        row = self.store.one(
            "SELECT set_digest FROM claim_context WHERE event_id = ?", (event_id,)
        )
        return row["set_digest"] if row else None

    # ----------------------------------------------------------------- coverage

    def coverage(self, relationship_id, event_id, verdict, findings, *, reason=None,
                 expected_digest=None) -> dict:
        registered = self.get(relationship_id)
        mode = self.mode(relationship_id)
        findings = findings or []

        if registered is None:
            if mode == MANAGED:
                raise AckRefused(
                    RefusalReason.CRITERIA_UNREGISTERED,
                    f"{relationship_id!r} is a managed assignment with no canonical criteria; "
                    "a managed assignment cannot be completed against nothing",
                )
            return {
                "coverage": LEGACY_UNREGISTERED, "setDigest": None, "boundDigest": None,
                "findings": findings,
            }

        digest = registered["setDigest"]
        bound = self.bound_digest(event_id)
        if (mode == MANAGED and bound is None and expected_digest is None
                and verdict in ("verified", "needs_changes")):
            # Without a bound review or a stated digest there is nothing to compare the
            # current set against, so skipping the claim would quietly skip the currency
            # protection entirely. Claiming the review is what pins the set it was made
            # against; stating the digest is the explicit alternative for a caller that has
            # one. Legacy relationships are unaffected and keep the delivered behaviour.
            raise AckRefused(
                RefusalReason.REVIEW_NOT_BOUND,
                f"this managed review is not bound to a criteria set: claim the event first, "
                f"or pass the reviewed digest explicitly. Current set is {digest}",
            )
        if bound is not None and bound != digest:
            raise AckRefused(
                RefusalReason.CRITERIA_SET_CHANGED,
                f"the criteria set changed after this review was claimed: bound {bound}, "
                f"current {digest}. Findings made against the previous wording cannot certify "
                "the current one; claim the review again",
            )
        if expected_digest is not None and expected_digest != digest:
            raise AckRefused(
                RefusalReason.CRITERIA_SET_CHANGED,
                f"expected criteria set {expected_digest}, but the current set is {digest}",
            )

        known = {c["id"] for c in registered["criteria"]}
        required = {c["id"] for c in registered["criteria"] if c["required"]}
        by_id = {}
        for finding in findings:
            if finding["id"] not in known:
                raise AckRefused(
                    RefusalReason.UNKNOWN_CRITERION,
                    f"{finding['id']!r} is not in this assignment's canonical criteria",
                )
            by_id[finding["id"]] = finding

        if verdict == "verified":
            passed = {i for i, f in by_id.items() if f["verdict"] == "verified"}
            missing = sorted(required - passed)
            if missing:
                raise AckRefused(
                    RefusalReason.CRITERIA_NOT_COVERED,
                    f"these required criteria are not recorded as verified: {missing}",
                )
        elif verdict == "needs_changes":
            actionable = [
                f for f in by_id.values()
                if f["verdict"] == "needs_changes" and f.get("note")
            ]
            if not actionable:
                raise AckRefused(
                    RefusalReason.FINDINGS_REQUIRED,
                    "a needs_changes verdict needs at least one criterion marked "
                    "needs_changes with a note; a correction with no findings is one nobody "
                    "can act on",
                )
        elif verdict == "unverified":
            stated = [
                f for f in by_id.values() if f["verdict"] == "unverified" and f.get("note")
            ]
            if not stated:
                raise AckRefused(
                    RefusalReason.FINDINGS_REQUIRED,
                    "an unverified verdict names at least one criterion it could not verify, "
                    "with the reason",
                )
        elif verdict == "aborted":
            if not str(reason or "").strip():
                raise AckRefused(
                    RefusalReason.FINDINGS_REQUIRED, "an aborted verdict states why"
                )

        return {
            "coverage": COVERED, "setDigest": digest, "boundDigest": bound, "findings": findings,
        }
