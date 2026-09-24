"""What two peer parents agreed about a shared edit region, and what that does not mean.

The unit is a REGION, never a file name. A region is a repository, a base revision, a path and
optionally a symbol or data key inside that path, so two parents with business in different
parts of one file are not in conflict and one shared file cannot block a project. That is the
whole reason the key is shaped this way rather than as a path.

base_revision is part of the region's identity, which makes revision invalidation mechanical
instead of a rule somebody has to remember: a new revision derives a different region, so an
agreement made against an older tree cannot silently stand for the newer one. Restating a
revision is recorded as an append-only mark, one successor per revision, and a settlement asks
whether its OWN revision has an outgoing mark. Asking which mark is newest would have no answer
when two share an injected clock's instant.

A base move reaches an agreement only when a party records it (CRW-237). Nothing here watches a
branch, and merge-turn-land records no revision mark: a mark is append-only with one successor
per revision, while a landing's recorded base can still be corrected, so a mark written from a
wrong reading could never be taken back. A registered parent of a project with an agreement in the
repository, open or closed (_check_restater reads that history, not the agreement being moved),
records the move from the end of the recorded chain; until somebody does, a late acceptance on the
older tree stands.

Carrying an agreement onto the newer tree keeps what was agreed. The successor keeps its proposer,
its constraint and both sides' conditions, whichever side carries it; the carrying side may
restate only its own condition, and carrying accepts its side on the new tree. The other side's
acceptance was given on the older tree, so it is asked for again - named, with its reason and the
command that gives it, never cleared without a word. Text carried from an older tree says which
tree it was written against, because a line number inside it points there.

Generated metadata is classified rather than contested. Two parents both touching a file that
is re-derived from the integration tree are not disagreeing; they are both going to regenerate
it. Such a region never refuses another proposal and is reported under regenerate with the
command it comes from.

An agreement confers NOTHING. It is not merge permission, it is not authority to instruct, and
it does not widen anybody's artifact scope. Nothing here writes an authorization of any kind
and the merge-turn module never reads this table, which is the form that claim takes: a
property of what the code does not do, checked by a test, rather than a sentence in a record.

scope.is_within arrives under an alias. The suite's regression map declares it a folded
boolean and tracks the bare name, so calling it by that name would trip an inventory declared
in a file this work may not edit.
"""

import posixpath

from .coordination import DOMAIN_EDIT_REGION, Conflicts, Refusal, derive, exact
from .errors import CoordinationError, RefusalReason
from .identity import sha256_hex
from .scope import is_within as region_contains

PROJECT = "project"
PARENT = "parent"
PEER = "peer"

TREE = "tree"
FILE = "file"
SYMBOL = "symbol"
DATA = "data"
KINDS = (TREE, FILE, SYMBOL, DATA)
KEYED = (SYMBOL, DATA)

SOURCE = "source"
GENERATED = "generated"
CLASSES = (SOURCE, GENERATED)

PROPOSED = "proposed"
AGREED = "agreed"
REOPENED = "reopened"
DECLINED = "declined"
WITHDRAWN = "withdrawn"
RELEASED = "released"
LIVE = (PROPOSED, AGREED, REOPENED)
DISPOSITIONS = ("accepted", "declined", "withdrawn", "released")

OPEN = "open"
ACCEPTED = "accepted"
DONE = "done"
DROPPED = "dropped"
FOLLOWUP_DISPOSITIONS = (DONE, DROPPED)

TOTAL = "total"
PARTIAL = "partial"
DISJOINT = "disjoint"

# Why a carried agreement still waits for one side, said in the answer rather than left to a
# cleared column to explain. An acceptance stands on the tree it was given on.
ACCEPTANCE_ON_PRIOR_REVISION = "acceptance_on_prior_revision"
NOT_YET_ACCEPTED = "not_yet_accepted"

MARKS = ("SELECT from_revision, to_revision FROM edit_revision_marks"
         "  WHERE repository = ?")
# The destination a carry must share with the agreement it retires, named as the columns are.
CARRIED_PLACE = (
    "repository", "path", "region_kind", "region_key", "region_class", "regenerate_from",
    "left_project", "right_project", "peer_link_id",
)


def canonical_path(path):
    """One spelling per place, checked before it reaches an identity.

    Containment normalises its inputs, so 'a//b' and 'a/b' are the same place to it while
    hashing them would give two regions. Rejecting the non-canonical spelling is what keeps
    identity and containment agreeing about what a place is.
    """
    if not isinstance(path, str) or not path.strip() or "\x00" in path:
        return None
    if "|" in path:
        # The field separator. Without this 'a|b' as a path and 'a' with key 'b' derive one
        # identity, so two distinct places would replay or contest each other.
        return None
    if path.startswith("/") or path in (".", "..") or path != posixpath.normpath(path):
        return None
    if any(part == ".." for part in path.split("/")):
        return None
    return path


def region_id(repository, base_revision, path, region_kind, region_key):
    return derive(
        "rgn", exact(repository, "a repository"), exact(base_revision, "a base revision"),
        path, region_kind, region_key)


def agreement_id(region, left_project, right_project, tenure):
    """Sorted, so either side proposing converges on one record rather than two mirrors."""
    low, high = sorted((left_project, right_project))
    return derive("agr", region, low, high, str(tenure))


def followup_id(agreement, trigger_text):
    return derive("fup", agreement, sha256_hex(trigger_text))


def mark_id(repository, from_revision, to_revision):
    return derive("rvm", repository, from_revision, to_revision)


def overlap(left, right):
    """total, partial or disjoint, decided on the place rather than on who asked.

    Two symbol or data regions overlap only when the PATH and the key both match. Comparing
    keys alone would have made a function of one name in one file collide with the same name
    in another, which is not a shared region by any reading.
    """
    if left["region_id"] == right["region_id"]:
        return TOTAL
    if left["repository"] != right["repository"]:
        return DISJOINT
    for upper, lower in ((left, right), (right, left)):
        if upper["region_kind"] == TREE and region_contains(upper["path"], lower["path"]):
            return PARTIAL
    if left["path"] != right["path"]:
        return DISJOINT
    kinds = {left["region_kind"], right["region_kind"]}
    if FILE in kinds:
        return PARTIAL
    if left["region_kind"] in KEYED and right["region_kind"] in KEYED:
        return PARTIAL if left["region_key"] == right["region_key"] else DISJOINT
    return DISJOINT

class EditRegions:
    """Proposals, agreements, follow-ups and revision restatements over shared regions."""

    def __init__(self, store, clock, linkage):
        self.store = store
        self.clock = clock
        self.linkage = linkage
        self.conflicts = Conflicts(store)

    # ---------------------------------------------------------------- reading

    def agreement(self, identifier):
        row = self.store.one(
            "SELECT * FROM edit_agreements WHERE agreement_id = ?", (identifier,))
        return self._described(self._agreement_record(row)) if row else None

    def current_revision(self, repository, revision):
        """Whether this revision was superseded, asked of the revision itself.

        One successor per revision means a chain leaves every earlier revision with an
        outgoing mark and only the newest without one, so this needs no ordering and no
        tie-break.
        """
        return self.store.one(
            "SELECT * FROM edit_revision_marks"
            "  WHERE repository = ? AND from_revision = ?",
            (repository, revision),
        ) is None

    def show(self, *, repository, base_revision=None, project_key=None, path=None):
        rows = self.store.all(
            "SELECT a.*, r.path AS region_path, r.region_kind AS region_kind,"
            "       r.region_key AS region_key, r.region_class AS region_class,"
            "       r.regenerate_from AS regenerate_from"
            "  FROM edit_agreements a JOIN edit_regions r ON r.region_id = a.region_id"
            " WHERE a.repository = ? ORDER BY a.proposed_at, a.agreement_id",
            (repository,),
        )
        # Read once for the whole answer rather than once per agreement.
        successors = self._successor_map(self.store.all(MARKS, (repository,)))
        carries = {
            row["agreement_id"]: row for row in self.store.all(
                "SELECT c.* FROM edit_reaffirmations c"
                "  JOIN edit_agreements a ON a.agreement_id = c.agreement_id"
                " WHERE a.repository = ?", (repository,))
        }
        exclusive, regenerate = [], []
        for row in rows:
            if base_revision is not None and row["base_revision"] != base_revision:
                continue
            if project_key is not None and project_key not in (
                    row["left_project"], row["right_project"]):
                continue
            if path is not None and row["region_path"] != path:
                continue
            record = self._described(self._agreement_record(row), successors, carries)
            record["region"] = {
                "path": row["region_path"], "regionKind": row["region_kind"],
                "regionKey": row["region_key"] or None,
                "regionClass": row["region_class"],
                "regenerateFrom": row["regenerate_from"],
            }
            record["resolution"] = (
                "rederive" if row["region_class"] == GENERATED else "exclusive")
            (regenerate if row["region_class"] == GENERATED else exclusive).append(record)
        return {
            "repository": repository, "baseRevision": base_revision,
            "exclusive": exclusive, "regenerate": regenerate,
            "followups": self._followups_for(
                [r["agreementId"] for r in exclusive + regenerate]),
            "conflicts": self.conflicts.all(DOMAIN_EDIT_REGION, repository),
        }

    def _followups_for(self, agreements):
        accepted, unassigned, closed = [], [], []
        for identifier in agreements:
            for row in self.store.all(
                "SELECT * FROM edit_followups"
                "  WHERE agreement_id = ? ORDER BY recorded_at, followup_id",
                (identifier,),
            ):
                record = self._followup_record(row)
                if row["state"] in (DONE, DROPPED):
                    closed.append(record)
                elif row["assignee_task_id"]:
                    accepted.append(record)
                else:
                    # Nobody took this. It is reported on its own rather than attributed to
                    # whichever parent happens to be asking, because counting an unassigned
                    # follow-up under a parent is how work nobody owns looks owned.
                    unassigned.append(record)
        return {"accepted": accepted, "unassigned": unassigned, "closed": closed}

    @staticmethod
    def _agreement_record(row):
        return {
            "agreementId": row["agreement_id"], "regionId": row["region_id"],
            "repository": row["repository"], "baseRevision": row["base_revision"],
            "leftProject": row["left_project"], "rightProject": row["right_project"],
            "peerLinkId": row["peer_link_id"], "proposerTaskId": row["proposer_task_id"],
            "issueKey": row["issue_key"], "constraintText": row["constraint_text"],
            "leftCondition": row["left_condition"], "rightCondition": row["right_condition"],
            "leftAcceptedAt": row["left_accepted_at"],
            "rightAcceptedAt": row["right_accepted_at"],
            "nextOwner": row["next_owner"], "state": row["state"], "tenure": row["tenure"],
            "supersedes": row["supersedes"], "supersededBy": row["superseded_by"],
            "closeReason": row["close_reason"], "proposedAt": row["proposed_at"],
            "updatedAt": row["updated_at"], "closedAt": row["closed_at"],
            # Stated rather than implied. A peer agreement records what two parents said they
            # would accept; it is not permission to merge and it widens nobody's scope.
            "authorizes": [], "grantsMergePermission": False,
        }

    @staticmethod
    def _followup_record(row):
        return {
            "followupId": row["followup_id"], "agreementId": row["agreement_id"],
            "trigger": row["trigger_text"], "acceptance": row["acceptance_text"],
            "issueRef": row["issue_ref"], "assigneeTaskId": row["assignee_task_id"],
            "assigneeProject": row["assignee_project"], "acceptedAt": row["accepted_at"],
            "state": row["state"], "closeReason": row["close_reason"],
            "recordedBy": row["recorded_by"], "recordedAt": row["recorded_at"],
        }

    def _described(self, record, successors=None, carries=None):
        """What a record cannot say from its own columns (CRW-237).

        Which revision each of its texts was written against, where the recorded chain from its
        revision ends, and what a reaffirmation carried into it. A reader, so it opens no
        transaction: show() passes the repository's marks and carries read once, and a single
        record reads the two it needs.
        """
        if successors is None:
            successors = self._successor_map(self.store.all(MARKS, (record["repository"],)))
        if carries is None:
            row = self.store.one(
                "SELECT * FROM edit_reaffirmations WHERE agreement_id = ?",
                (record["agreementId"],))
            carries = {record["agreementId"]: row} if row is not None else {}
        carry = carries.get(record["agreementId"])
        base = record["baseRevision"]
        if carry is None:
            stated = {
                "constraint": base,
                "leftCondition": base if record["leftCondition"] is not None else None,
                "rightCondition": base if record["rightCondition"] is not None else None,
            }
        else:
            stated = {
                "constraint": carry["constraint_revision"],
                "leftCondition": carry["left_condition_revision"],
                "rightCondition": carry["right_condition_revision"],
            }
        record["statedOn"] = stated
        # Listed rather than rewritten: a line number in any of these points into the tree it was
        # written against, and only the side that wrote a text can restate it.
        record["textFromEarlierRevision"] = [
            field for field in ("constraint", "leftCondition", "rightCondition")
            if stated[field] is not None and stated[field] != base
        ]
        chain = self._walk(successors, base)
        record["currentRevision"] = chain[-1] if chain else base
        awaiting = None if carry is None else self._awaiting(record, carry)
        if awaiting is not None:
            # While a carried agreement waits for a side, whoever acts next is that side's
            # parent NOW. The stored value names the parent at the time of the carry, and after
            # a handover it would send the acceptance to a task that can no longer give it.
            record["nextOwner"] = awaiting["task"]
        record["reaffirmation"] = None if carry is None else {
            "predecessor": carry["predecessor_id"], "actor": carry["actor"],
            "actorProject": carry["actor_project"], "fromRevision": carry["from_revision"],
            "toRevision": carry["to_revision"], "recordedAt": carry["recorded_at"],
            "awaitingAcceptance": awaiting,
        }
        return record

    def _awaiting(self, record, carry):
        """The side a carried agreement still waits for, why, and the command that answers it.

        Only while it is proposed: once agreed nothing waits, and a closed or reopened successor
        is answered by its own state. With no single registered parent on that side there is no
        task to name, so the answer says what has to happen first instead of printing a command
        nobody can run.
        """
        if record["state"] != PROPOSED:
            return None
        for accepted, project, prior_column in (
                ("leftAcceptedAt", "leftProject", "left_accepted_at"),
                ("rightAcceptedAt", "rightProject", "right_accepted_at")):
            if record[accepted]:
                continue
            predecessor = self.store.one(
                "SELECT * FROM edit_agreements WHERE agreement_id = ?",
                (carry["predecessor_id"],))
            prior = predecessor[prior_column] if predecessor is not None else None
            key = record[project]
            parent = self._sole_parent(key)
            command = ("region-settle --agreement " + record["agreementId"] + " --actor "
                       + parent + " --disposition accepted") if parent else None
            return {
                "project": key, "task": parent,
                "reason": ACCEPTANCE_ON_PRIOR_REVISION if prior else NOT_YET_ACCEPTED,
                "priorAcceptedAt": prior, "priorRevision": carry["from_revision"],
                "command": command,
                "precondition": None if parent else (
                    repr(key) + " has no single registered parent; the parent that takes it"
                    " runs region-settle --agreement " + record["agreementId"]
                    + " --actor <that task> --disposition accepted"),
            }
        return None

    def _sole_parent(self, project):
        """The one registered parent of a project, or None when there is not exactly one."""
        owners = [
            record["taskId"] for record in self.linkage.owners(PROJECT, project)
            if record["role"] == PARENT
        ]
        return owners[0] if len(owners) == 1 else None

    # -------------------------------------------------------------- ownership

    def _acting_side(self, db, row, actor, subject):
        """The actor must own one of the two projects, and the peer link must still be live."""
        for side in ("left_project", "right_project"):
            owners = [
                record["taskId"] for record in self.linkage.owners(PROJECT, row[side])
                if record["role"] == PARENT
            ]
            if owners == [actor]:
                link = db.execute(
                    "SELECT * FROM scope_links WHERE link_id = ?", (row["peer_link_id"],)
                ).fetchone()
                if link is None or link["status"] not in ("active", "paused") \
                        or link["superseded_by"]:
                    return None, Refusal(
                        RefusalReason.UNREGISTERED_SCOPE,
                        "peer link " + repr(row["peer_link_id"]) + " is not live, so these two"
                        " projects are not registered peers",
                        domain=DOMAIN_EDIT_REGION, subject=subject, challenger=actor)
                return side, None
        return None, Refusal(
            RefusalReason.SCOPE_ROLE_MISMATCH,
            "task " + repr(actor) + " is the registered parent of neither "
            + repr(row["left_project"]) + " nor " + repr(row["right_project"]),
            domain=DOMAIN_EDIT_REGION, subject=subject, challenger=actor)
    # ---------------------------------------------------------------- writing

    def propose(self, *, repository, base_revision, path, region_kind, left_project,
                right_project, peer_link_id, proposer_task_id, constraint_text,
                region_key="", region_class=SOURCE, regenerate_from=None,
                condition=None, issue_key=None, next_owner=None, supersedes=None,
                carry=None):
        """Claim a place, not a file. Refuses a claim so broad it would block a project.

        ``carry`` is reaffirm's, and nobody else's: {"predecessor": id, "restated": text or
        None}. With it this one transaction re-decides everything reaffirm validated a
        transaction earlier, retires the predecessor, inserts the successor with the terms READ
        FROM THE PREDECESSOR here, and links the two. The arguments name where the successor
        lands and must be the predecessor's own place, pair and link on the end of its chain;
        proposer_task_id is the task carrying it, which is not the proposer it records.
        """
        clean = canonical_path(path)
        problem = self._shape_refusal(
            clean, path, region_kind, region_class, regenerate_from, repository)
        if problem is not None:
            raise problem.error()
        region_key = region_key or ""
        identifier = region_id(repository, base_revision, clean, region_kind, region_key)
        low, high = sorted((exact(left_project, "a project key"),
                            exact(right_project, "a project key")))
        if low == high:
            raise CoordinationError(
                RefusalReason.SCOPE_CYCLE, "a project is not its own peer")
        if (region_kind in KEYED) != bool(region_key):
            # A key belongs to a symbol or a data region and to nothing else. A keyed file
            # minted a second identity for one place, and an unkeyed symbol collapsed every
            # symbol in the file into one.
            raise CoordinationError(
                RefusalReason.REGION_TOO_BROAD,
                "a " + region_kind + " region "
                + ("names the symbol or data key it covers" if region_kind in KEYED
                   else "covers the whole path and takes no key"))
        if carry is None and self._owned_side(low, high, proposer_task_id) is None:
            # A carry's ownership is decided inside its transaction instead (the "no longer
            # owns" branch below), where the refusal is recorded: reaffirm validated ownership a
            # transaction earlier, and a handover in between used to be refused here, unrecorded.
            raise CoordinationError(
                RefusalReason.SCOPE_ROLE_MISMATCH,
                "task " + repr(proposer_task_id) + " is the registered parent of neither "
                + repr(low) + " nor " + repr(high) + ", and a proposal pre-accepts its own"
                " side, so a stranger could forge one and block an overlapping region")
        now = self.clock.iso()
        refusal, agreement, replay = None, None, None
        with self.store.transaction() as db:
            # Nothing is written until everything is decided. Inserting the region first left
            # an orphan behind every refusal, and because a region is classified once that
            # orphan then rejected the corrected proposal it existed to describe.
            region = db.execute(
                "SELECT * FROM edit_regions WHERE region_id = ?", (identifier,)).fetchone()
            if region is None:
                region = {
                    "region_id": identifier, "repository": repository,
                    "base_revision": base_revision, "path": clean,
                    "region_kind": region_kind, "region_key": region_key,
                    "region_class": region_class, "regenerate_from": regenerate_from,
                }
            predecessor = None
            if carry is not None:
                predecessor, refusal = self._carry_check(
                    db, carry, {
                        "repository": repository, "path": clean, "region_kind": region_kind,
                        "region_key": region_key, "region_class": region_class,
                        "regenerate_from": regenerate_from, "left_project": low,
                        "right_project": high, "peer_link_id": peer_link_id,
                    }, supersedes, base_revision, proposer_task_id)
            if refusal is None and (region["region_class"] != region_class or (
                    region["regenerate_from"] or "") != (regenerate_from or "")):
                # The insert above is ON CONFLICT DO NOTHING, so a later proposal naming a
                # different classification silently inherited the first one - and
                # classification decides whether the region contests at all. Recorded like every
                # other refusal here: raised inside the transaction it kept no trace, which a
                # carry refused this way made visible (CRW-237 review).
                refusal = Refusal(
                    RefusalReason.REGION_OVERLAP,
                    "region " + repr(identifier) + " is already recorded as "
                    + repr(region["region_class"]) + " derived from "
                    + repr(region["regenerate_from"]) + "; a place is classified once, and a"
                    " different classification is a different claim about it",
                    domain=DOMAIN_EDIT_REGION, subject=repository, incumbent=identifier,
                    challenger=region_class)
            previous = None if refusal is not None else db.execute(
                "SELECT * FROM edit_agreements"
                "  WHERE region_id = ? AND left_project = ? AND right_project = ?"
                "    AND state IN ('proposed','agreed','reopened') AND superseded_by IS NULL",
                (identifier, low, high),
            ).fetchone()
            if refusal is not None:
                pass
            elif previous is not None and predecessor is not None:
                # Carrying onto an agreement this pair already holds on the new tree would hand
                # it a lineage and terms it never had. reaffirm refuses this before it writes
                # anything; reaching it here means that agreement arrived in between.
                refusal = self._standing_refusal(
                    previous["agreement_id"], predecessor["agreement_id"], base_revision,
                    repository, proposer_task_id)
            elif previous is not None:
                replay = previous
            else:
                refusal = self._overlap_refusal(
                    db, region, low, high, proposer_task_id)
                if refusal is None:
                    refusal = self._peer_refusal(
                        db, low, high, peer_link_id, proposer_task_id, identifier)
            if replay is None and refusal is None:
                db.execute(
                    "INSERT INTO edit_regions (region_id, repository, base_revision, path,"
                    " region_kind, region_key, region_class, regenerate_from, recorded_at)"
                    " VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT (region_id) DO NOTHING",
                    (identifier, repository, base_revision, clean, region_kind, region_key,
                     region_class, regenerate_from, now),
                )
                # Copy from what is PERSISTED, never from the arguments: a replayed proposal
                # spelling its revision differently would otherwise store an agreement whose
                # revision disagreed with the one inside its own region id, and the
                # stale-settlement check reads that column.
                region = db.execute(
                    "SELECT * FROM edit_regions WHERE region_id = ?", (identifier,)).fetchone()
                highest = db.execute(
                    "SELECT MAX(tenure) AS top FROM edit_agreements"
                    "  WHERE region_id = ? AND left_project = ? AND right_project = ?",
                    (identifier, low, high),
                ).fetchone()
                tenure = (highest["top"] or 0) + 1
                agreement = agreement_id(identifier, low, high, tenure)
                # The side the proposer OWNS, not the argument slot it arrived in. Reversed
                # arguments made a proposer pre-accept the PEER's side, so it could then
                # accept the remaining one itself and hold both.
                owned = self._owned_side(low, high, proposer_task_id)
                if owned is None:
                    # Ownership can be lost between the pre-flight check and this insert. A
                    # None here used to fall through to the high side, so a proposer that had
                    # just stopped owning the low project pre-accepted the PEER's - which is
                    # the bug this line was added to fix, one race later.
                    refusal = Refusal(
                        RefusalReason.SCOPE_ROLE_MISMATCH,
                        "task " + repr(proposer_task_id) + " no longer owns either "
                        + repr(low) + " or " + repr(high) + "; the project changed hands"
                        " while this proposal was being decided",
                        domain=DOMAIN_EDIT_REGION, subject=repository,
                        challenger=proposer_task_id)
                    self.conflicts.record_in(db, refusal, at=now)
                    agreement = None
                else:
                    terms = {
                        "proposer": proposer_task_id, "constraint": constraint_text,
                        "issue": issue_key, "next_owner": next_owner,
                        "conditions": {low: None, high: None},
                    }
                    terms["conditions"][owned] = condition
                    if predecessor is not None:
                        terms = self._carried_terms(
                            db, predecessor, owned, low, high, carry["restated"],
                            region["base_revision"])
                    db.execute(
                        "INSERT INTO edit_agreements (agreement_id, region_id, repository,"
                        " base_revision, left_project, right_project, peer_link_id,"
                        " proposer_task_id, issue_key, constraint_text, left_condition,"
                        " right_condition, left_accepted_at, right_accepted_at, next_owner,"
                        " state, tenure, supersedes, proposed_at, updated_at)"
                        " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                        (agreement, identifier, region["repository"],
                         region["base_revision"], low, high, peer_link_id, terms["proposer"],
                         terms["issue"], terms["constraint"], terms["conditions"][low],
                         terms["conditions"][high], now if owned == low else None,
                         now if owned == high else None, terms["next_owner"], PROPOSED,
                         tenure, supersedes, now, now),
                    )
                    self.store.journal(
                        "edit_region_proposed", agreement,
                        {"regionId": identifier, "path": clean, "pair": [low, high]}, at=now)
                    if predecessor is not None:
                        self._retire_carried(
                            db, predecessor, agreement, proposer_task_id, owned, terms,
                            region["base_revision"], now)
            elif refusal is not None:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        if replay is not None:
            answer = self._described(self._agreement_record(replay))
            answer["alreadyProposed"] = True
            return answer
        answer = self.agreement(agreement)
        answer["alreadyProposed"] = False
        return answer

    def _shape_refusal(self, clean, path, region_kind, region_class, regenerate_from,
                       repository):
        def broad(detail):
            return Refusal(
                RefusalReason.REGION_TOO_BROAD, detail,
                domain=DOMAIN_EDIT_REGION, subject=repository, incumbent=str(path))

        if region_kind not in KINDS:
            return broad("a region kind is one of " + ", ".join(KINDS) + ", not "
                         + repr(region_kind))
        if region_class not in CLASSES:
            return broad("a region class is one of " + ", ".join(CLASSES) + ", not "
                         + repr(region_class))
        if clean is None:
            return broad(
                "a region path is repository-relative and canonical: " + repr(path)
                + " has a leading slash, a '..' component, a redundant separator or a"
                " trailing one. Two spellings of one place would derive two regions while"
                " containment treated them as the same place")
        if region_kind == TREE and clean in ("", "."):
            return broad(
                "a tree region at the repository root claims everything, which is the"
                " 'one name blocks the project' failure at its largest")
        if region_class == GENERATED and not str(regenerate_from or "").strip():
            return broad(
                "a generated region names what it is re-derived from; without that a reader"
                " cannot tell an artefact to regenerate from one to hand-merge")
        return None

    def _overlap_refusal(self, db, region, low, high, actor):
        """A live agreement of ANOTHER pair on an overlapping place refuses this one.

        A generated region never contests. Two parents both touching a file the integration
        tree re-derives are not disagreeing about it; they are both going to regenerate it,
        and refusing one of them would describe a conflict that does not exist.
        """
        if region["region_class"] == GENERATED:
            return None
        for row in db.execute(
            "SELECT a.*, r.path AS path, r.region_kind AS region_kind,"
            "       r.region_key AS region_key, r.region_class AS region_class,"
            "       r.repository AS repository, r.region_id AS region_id"
            "  FROM edit_agreements a JOIN edit_regions r ON r.region_id = a.region_id"
            " WHERE a.repository = ? AND a.base_revision = ?"
            "   AND a.state IN ('proposed','agreed','reopened') AND a.superseded_by IS NULL",
            (region["repository"], region["base_revision"]),
        ).fetchall():
            if row["region_class"] == GENERATED:
                continue
            if (row["left_project"], row["right_project"]) == (low, high):
                continue
            if overlap(region, row) == DISJOINT:
                continue
            return Refusal(
                RefusalReason.REGION_OVERLAP,
                "agreement " + repr(row["agreement_id"]) + " between "
                + repr(row["left_project"]) + " and " + repr(row["right_project"])
                + " already covers " + row["region_kind"] + " " + repr(row["path"])
                + ", which overlaps this region",
                domain=DOMAIN_EDIT_REGION, subject=region["repository"],
                incumbent=row["agreement_id"], challenger=actor)
        return None

    def _peer_refusal(self, db, low, high, peer_link_id, actor, subject):
        link = db.execute(
            "SELECT * FROM scope_links WHERE link_id = ?", (peer_link_id,)).fetchone()
        if link is None or link["link_kind"] != PEER \
                or link["status"] not in ("active", "paused") or link["superseded_by"]:
            return Refusal(
                RefusalReason.UNREGISTERED_SCOPE,
                "link " + repr(peer_link_id) + " is not a live peer link, and an agreement"
                " joins two projects that registered as peers",
                domain=DOMAIN_EDIT_REGION, subject=subject, challenger=actor)
        if sorted((link["upper_key"], link["lower_key"])) != [low, high]:
            return Refusal(
                RefusalReason.UNREGISTERED_SCOPE,
                "peer link " + repr(peer_link_id) + " joins "
                + repr(sorted((link["upper_key"], link["lower_key"])))
                + ", not " + repr([low, high]),
                domain=DOMAIN_EDIT_REGION, subject=subject, challenger=actor)
        return None

    def _carry_check(self, db, carry, place, supersedes, base_revision, actor):
        """Whether a carry may retire its predecessor and land here, decided in the write.

        reaffirm validated this one transaction earlier, and everything it read can have moved
        since: the predecessor settled or carried by the other side, the base moved again. A
        carry retires the row it names, so each fact is read again where it is acted on. The
        destination is checked against the predecessor rather than trusted, because a carry that
        landed on another place, pair or link would retire one agreement and create an
        unrelated one.
        """
        subject = place["repository"]
        predecessor = db.execute(
            "SELECT a.*, r.path AS path, r.region_kind AS region_kind,"
            "       r.region_key AS region_key, r.region_class AS region_class,"
            "       r.regenerate_from AS regenerate_from"
            "  FROM edit_agreements a JOIN edit_regions r ON r.region_id = a.region_id"
            " WHERE a.agreement_id = ?", (carry["predecessor"],)).fetchone()
        if predecessor is None:
            return None, Refusal(
                RefusalReason.UNREGISTERED_SCOPE,
                "no agreement " + repr(carry["predecessor"]) + " to carry",
                domain=DOMAIN_EDIT_REGION, subject=subject,
                incumbent=str(carry["predecessor"]), challenger=actor)
        identifier = predecessor["agreement_id"]
        for field in CARRIED_PLACE:
            if (place[field] or "") != (predecessor[field] or ""):
                return None, Refusal(
                    RefusalReason.UNREGISTERED_SCOPE,
                    "a carry of " + repr(identifier) + " lands on its own place, pair and"
                    " link; " + field + " " + repr(place[field]) + " is not its "
                    + repr(predecessor[field]),
                    domain=DOMAIN_EDIT_REGION, subject=subject, incumbent=identifier,
                    challenger=actor)
        if supersedes != identifier:
            return None, Refusal(
                RefusalReason.UNREGISTERED_SCOPE,
                "a carry of " + repr(identifier) + " supersedes it, not " + repr(supersedes),
                domain=DOMAIN_EDIT_REGION, subject=subject, incumbent=identifier,
                challenger=actor)
        if predecessor["state"] not in LIVE or predecessor["superseded_by"]:
            return None, Refusal(
                RefusalReason.AGREEMENT_NOT_OPEN,
                "agreement " + repr(identifier) + " is " + predecessor["state"]
                + (", superseded by " + repr(predecessor["superseded_by"])
                   if predecessor["superseded_by"] else "")
                + "; it was settled or carried while this carry was being decided, so there"
                " is nothing left to carry",
                domain=DOMAIN_EDIT_REGION, subject=subject, incumbent=predecessor["state"],
                challenger=actor)
        chain = self._chain_from(db, subject, predecessor["base_revision"])
        if not chain:
            return None, self._unmoved_refusal(
                identifier, predecessor["base_revision"], subject, actor)
        if chain[-1] != base_revision:
            return None, Refusal(
                RefusalReason.AGREEMENT_REVISION_STALE,
                "the recorded chain from " + repr(predecessor["base_revision"]) + " now ends"
                " at " + repr(chain[-1]) + ", not " + repr(base_revision) + ": the base moved"
                " again while this was being carried. Reaffirm it onto " + repr(chain[-1]),
                domain=DOMAIN_EDIT_REGION, subject=subject, incumbent=chain[-1],
                challenger=base_revision)
        return predecessor, None

    @staticmethod
    def _unmoved_refusal(identifier, revision, subject, actor):
        return Refusal(
            RefusalReason.LINK_NOT_ACTIVE,
            "agreement " + repr(identifier) + " already stands on " + repr(revision)
            + ", which no recorded move has superseded. A revision is not its own successor,"
            " and proposing it again in place would only clear the other side's acceptance",
            domain=DOMAIN_EDIT_REGION, subject=subject, incumbent=revision, challenger=actor)

    @staticmethod
    def _standing_refusal(standing, identifier, revision, subject, actor):
        return Refusal(
            RefusalReason.REGION_OVERLAP,
            "agreement " + repr(standing) + " between these two projects already stands on this"
            " place at " + repr(revision) + " with terms of its own. Carrying " + repr(identifier)
            + " onto it would hand it a lineage and terms it never had; agree on "
            + repr(standing) + " instead",
            domain=DOMAIN_EDIT_REGION, subject=subject, incumbent=standing, challenger=actor)

    def _carried_terms(self, db, predecessor, owned, low, high, restated, revision):
        """The terms a successor inherits, each with the revision it was written against.

        Read from the row being retired, inside the transaction retiring it, so the carried text
        is exactly the text that stops being live. Only the carrying side's own condition can be
        restated, and a restated one is stated on the successor's revision.
        """
        earlier = db.execute(
            "SELECT * FROM edit_reaffirmations WHERE agreement_id = ?",
            (predecessor["agreement_id"],)).fetchone()
        before = predecessor["base_revision"]
        conditions = {low: predecessor["left_condition"], high: predecessor["right_condition"]}
        if earlier is not None:
            revisions = {low: earlier["left_condition_revision"],
                         high: earlier["right_condition_revision"]}
            constraint_revision = earlier["constraint_revision"]
        else:
            revisions = {side: before if conditions[side] is not None else None
                         for side in (low, high)}
            constraint_revision = before
        if restated is not None:
            conditions[owned] = restated
            revisions[owned] = revision
        return {
            "proposer": predecessor["proposer_task_id"],
            "constraint": predecessor["constraint_text"], "issue": predecessor["issue_key"],
            # Whoever must act next is the side still to accept, when there is exactly one task
            # to name. The carried value was never checked against anybody's ownership.
            "next_owner": self._sole_parent(high if owned == low else low),
            "conditions": conditions, "revisions": revisions,
            "constraint_revision": constraint_revision,
        }

    def _retire_carried(self, db, predecessor, successor, actor, owned, terms, revision, now):
        """Retire, link and record the carry, in the transaction that inserted the successor."""
        db.execute(
            "UPDATE edit_agreements SET state = ?, close_reason = ?, closed_at = ?,"
            " superseded_by = ?, updated_at = ? WHERE agreement_id = ?",
            (RELEASED, "reaffirmed onto " + revision, now, successor, now,
             predecessor["agreement_id"]))
        low, high = predecessor["left_project"], predecessor["right_project"]
        db.execute(
            "INSERT INTO edit_reaffirmations (agreement_id, predecessor_id, actor, actor_project,"
            " from_revision, to_revision, constraint_revision, left_condition_revision,"
            " right_condition_revision, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
            (successor, predecessor["agreement_id"], actor, owned,
             predecessor["base_revision"], revision, terms["constraint_revision"],
             terms["revisions"][low], terms["revisions"][high], now),
        )
        self.store.journal(
            "edit_region_reaffirmed", successor,
            {"predecessor": predecessor["agreement_id"], "actor": actor,
             "from": predecessor["base_revision"], "to": revision}, at=now)

    def settle(self, identifier, *, actor, disposition, condition=None, reason=None):
        """Accept, decline, withdraw or release. A decline keeps the condition it would accept.

        A conditional refusal that discarded its condition would leave the two sides knowing
        only that they disagreed, which is the opposite of recording what they would accept.
        """
        if disposition not in DISPOSITIONS:
            raise CoordinationError(
                RefusalReason.LINK_NOT_ACTIVE,
                "a disposition is one of " + ", ".join(DISPOSITIONS) + ", not "
                + repr(disposition))
        if disposition == "accepted" and condition is not None:
            # An acceptance never wrote a condition, so one given here vanished without a trace;
            # CRW-124 G3 restated a condition in an acceptance and lost it. Refused with the
            # invocation reason this function already uses for an unknown disposition.
            raise CoordinationError(
                RefusalReason.LINK_NOT_ACTIVE,
                "an acceptance takes no condition, and one given here would be dropped. A side"
                " states its condition when it proposes, restates it with region-reaffirm"
                " --condition after a base move, or declines with the condition it would accept")
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            row = db.execute(
                "SELECT * FROM edit_agreements WHERE agreement_id = ?", (identifier,)
            ).fetchone()
            if row is None:
                raise CoordinationError(
                    RefusalReason.UNREGISTERED_SCOPE, "no agreement " + repr(identifier))
            side, refusal = self._acting_side(db, row, actor, row["repository"])
            if refusal is None and row["state"] not in LIVE:
                refusal = Refusal(
                    RefusalReason.AGREEMENT_NOT_OPEN,
                    "agreement " + repr(identifier) + " is " + row["state"]
                    + ", which admits no further settlement; a new proposal supersedes it",
                    domain=DOMAIN_EDIT_REGION, subject=row["repository"],
                    incumbent=row["state"], challenger=actor)
            if refusal is None:
                superseded = db.execute(
                    "SELECT * FROM edit_revision_marks"
                    "  WHERE repository = ? AND from_revision = ?",
                    (row["repository"], row["base_revision"]),
                ).fetchone()
                if superseded is not None:
                    chain = self._chain_from(db, row["repository"], row["base_revision"])
                    end = chain[-1] if chain else superseded["to_revision"]
                    refusal = Refusal(
                        RefusalReason.AGREEMENT_REVISION_STALE,
                        "this agreement stands on " + repr(row["base_revision"])
                        + ", which was restated to " + repr(superseded["to_revision"])
                        + "; the recorded chain from it ends at " + repr(end) + ". Reaffirm it"
                        " on the current revision before settling it: region-reaffirm"
                        " --agreement " + identifier + " --actor " + actor + " --revision "
                        + end,
                        domain=DOMAIN_EDIT_REGION, subject=row["repository"],
                        incumbent=row["base_revision"], challenger=actor)
            if refusal is None and disposition == WITHDRAWN \
                    and row["proposer_task_id"] != actor:
                refusal = Refusal(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "only " + repr(row["proposer_task_id"]) + " can withdraw its own proposal;"
                    " the other side declines instead",
                    domain=DOMAIN_EDIT_REGION, subject=row["repository"],
                    incumbent=row["proposer_task_id"], challenger=actor)
            if refusal is None:
                self._apply_settlement(db, row, side, disposition, condition, reason, now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.agreement(identifier)

    def _apply_settlement(self, db, row, side, disposition, condition, reason, now):
        identifier = row["agreement_id"]
        if disposition == "accepted":
            column = "left_accepted_at" if side == "left_project" else "right_accepted_at"
            db.execute(
                "UPDATE edit_agreements SET " + column + " = ?, updated_at = ?"
                " WHERE agreement_id = ?", (now, now, identifier))
            both = db.execute(
                "SELECT left_accepted_at, right_accepted_at FROM edit_agreements"
                "  WHERE agreement_id = ?", (identifier,)).fetchone()
            if both["left_accepted_at"] and both["right_accepted_at"]:
                db.execute(
                    "UPDATE edit_agreements SET state = ?, updated_at = ?"
                    " WHERE agreement_id = ?", (AGREED, now, identifier))
            self.store.journal("edit_region_accepted", identifier, {"side": side}, at=now)
            return
        if disposition == "declined":
            column = "left_condition" if side == "left_project" else "right_condition"
            db.execute(
                "UPDATE edit_agreements SET " + column + " = ?, state = ?, close_reason = ?,"
                " closed_at = ?, updated_at = ? WHERE agreement_id = ?",
                (condition, DECLINED, reason, now, now, identifier))
            # A carried successor records where each condition was written; a decline writes
            # this side's afresh, on this revision, or clears it. No row, nothing to update.
            db.execute(
                "UPDATE edit_reaffirmations SET " + column + "_revision = ?"
                " WHERE agreement_id = ?",
                (row["base_revision"] if condition is not None else None, identifier))
            self.store.journal(
                "edit_region_declined", identifier,
                {"side": side, "condition": condition}, at=now)
            return
        closed = WITHDRAWN if disposition == WITHDRAWN else RELEASED
        db.execute(
            "UPDATE edit_agreements SET state = ?, close_reason = ?, closed_at = ?,"
            " updated_at = ? WHERE agreement_id = ?",
            (closed, reason, now, now, identifier))
        self.store.journal("edit_region_closed", identifier, {"state": closed}, at=now)

    def _owned_side(self, low, high, actor):
        """Which of the two projects the actor is the registered parent of, or None."""
        for project in (low, high):
            owners = [
                record["taskId"] for record in self.linkage.owners(PROJECT, project)
                if record["role"] == PARENT
            ]
            if owners == [actor]:
                return project
        return None

    def _chain_from(self, db, repository, revision):
        """Every revision reachable by following successor marks, bounded by the marks read.

        A walk over rows one query returned, not a wait: the chain cannot be longer than the
        number of marks the repository has, and a repeat stops it.
        """
        return self._walk(
            self._successor_map(db.execute(MARKS, (repository,)).fetchall()), revision)

    @staticmethod
    def _successor_map(rows):
        return {row["from_revision"]: row["to_revision"] for row in rows}

    def _unrelated_start(self, db, repository, revision):
        """A first move starts where an agreement stands, and a later one where a chain ends.

        One successor per revision stops a second chain from the SAME revision and nothing
        stopped one from a revision nobody stands on: after A->B, recording C->D instead of B->D
        succeeded, and the move never reached the agreement on A (CRW-237 review). Only a LIVE
        agreement stands anywhere: counting a withdrawn one let the same wrong move start from
        the closed agreement's revision (second review). A repository with neither a live
        agreement nor a mark keeps the first restatement _check_restater allows.
        """
        bases = {row["base_revision"] for row in db.execute(
            "SELECT DISTINCT base_revision FROM edit_agreements"
            "  WHERE repository = ? AND state IN ('proposed','agreed','reopened')"
            "    AND superseded_by IS NULL",
            (repository,)).fetchall()}
        successors = self._successor_map(db.execute(MARKS, (repository,)).fetchall())
        if not bases and not successors:
            return None
        if revision in bases or revision in successors.values():
            return None
        ends = sorted({(self._walk(successors, base) or [base])[-1] for base in bases}
                      | {end for end in successors.values() if end not in successors})
        return Refusal(
            RefusalReason.AGREEMENT_REVISION_STALE,
            "no live agreement in " + repr(repository) + " stands on " + repr(revision) + " and no"
            " recorded move reaches it, so a move from it would start a chain no agreement"
            " follows. A first move starts at a live agreement's revision and a later one at the"
            " end of its recorded chain; the chains here end at " + repr(ends),
            domain=DOMAIN_EDIT_REGION, subject=repository, incumbent=",".join(ends),
            challenger=revision)

    @staticmethod
    def _walk(marks, revision):
        reached, cursor = [], revision
        for _step in range(len(marks) + 1):
            cursor = marks.get(cursor)
            if cursor is None or cursor in reached:
                break
            reached.append(cursor)
        return reached

    def _check_restater(self, repository, actor):
        """Restating a revision reopens every agreement on it, so it is not anybody's call.

        An unrelated caller could mark a repository's revision superseded and reopen or block
        every agreement standing on it. The actor has to be a registered parent on one side of
        an agreement in that repository.
        """
        rows = self.store.all(
            "SELECT DISTINCT left_project AS project FROM edit_agreements WHERE repository = ?"
            "  UNION SELECT DISTINCT right_project FROM edit_agreements WHERE repository = ?",
            (repository, repository),
        )
        for row in rows:
            owners = [
                record["taskId"] for record in self.linkage.owners(PROJECT, row["project"])
                if record["role"] == PARENT
            ]
            if owners == [actor]:
                return
        if not rows:
            # A repository with no agreements has nothing to reopen and nobody to protect, so
            # the check is only that the caller is a registered parent at all. Refusing here
            # would make the first restatement impossible.
            registered = self.store.one(
                "SELECT task_id FROM scope_bindings"
                "  WHERE task_id = ? AND role = 'parent'"
                "    AND status IN ('active','paused') AND superseded_by IS NULL",
                (actor,),
            )
            if registered is not None:
                return
        raise CoordinationError(
            RefusalReason.SCOPE_ROLE_MISMATCH,
            "task " + repr(actor) + " is the registered parent of no project holding an"
            " agreement in " + repr(repository) + ", so it cannot restate that repository's"
            " revision and reopen everybody's agreements")

    def restate_revision(self, *, repository, from_revision, to_revision, actor):
        """Record that a repository's agreements now stand on a newer tree.

        The mark is append-only and one revision has one successor, so a chain leaves every
        earlier revision marked and only the newest unmarked. Rewriting base_revision on the
        rows instead would leave each one's id no longer derivable from its own columns.

        So only the first move is recorded from a proposal's revision; every later one is
        recorded from the end of the chain, and a refusal names that end (CRW-237). A move the
        chain already holds - restating the proposal's revision to a revision the chain reaches
        - is the same fact said again, answered like a replay with alreadyRecorded, not refused.
        """
        exact(repository, "a repository")
        exact(from_revision, "a base revision")
        exact(to_revision, "a base revision")
        if from_revision == to_revision:
            raise CoordinationError(
                RefusalReason.LINK_NOT_ACTIVE, "a revision is not its own successor")
        self._check_restater(repository, actor)
        now = self.clock.iso()
        refusal, moved, reached = None, [], []
        with self.store.transaction() as db:
            existing = db.execute(
                "SELECT * FROM edit_revision_marks"
                "  WHERE repository = ? AND from_revision = ?",
                (repository, from_revision),
            ).fetchone()
            if existing is not None and existing["to_revision"] != to_revision \
                    and to_revision not in self._chain_from(db, repository, from_revision):
                end = self._chain_from(db, repository, from_revision)[-1]
                refusal = Refusal(
                    RefusalReason.AGREEMENT_REVISION_STALE,
                    repr(from_revision) + " was already restated to "
                    + repr(existing["to_revision"]) + " by " + repr(existing["actor"])
                    + "; one revision has one successor and a second would leave two chains"
                    " nobody can order. A later move is recorded from the end of the recorded"
                    " chain, which is " + repr(end) + ": region-restate-revision --repository "
                    + repository + " --from-revision " + end + " --to-revision " + to_revision,
                    domain=DOMAIN_EDIT_REGION, subject=repository,
                    incumbent=existing["to_revision"], challenger=to_revision)
                self.conflicts.record_in(db, refusal, at=now)
            elif existing is None:
                refusal = self._unrelated_start(db, repository, from_revision)
                reached = self._chain_from(db, repository, to_revision)
                if refusal is None and from_revision in reached:
                    # One successor per revision does not make the chain acyclic. A to B
                    # followed by B to A left BOTH revisions carrying an outgoing mark, so
                    # every revision read as superseded and no tree was current at all.
                    refusal = Refusal(
                        RefusalReason.AGREEMENT_REVISION_STALE,
                        repr(to_revision) + " already reaches " + repr(from_revision)
                        + " through " + repr(reached) + ", so this mark would close a cycle"
                        " and leave no current revision for anything to stand on",
                        domain=DOMAIN_EDIT_REGION, subject=repository,
                        incumbent=to_revision, challenger=from_revision)
                if refusal is not None:
                    self.conflicts.record_in(db, refusal, at=now)
            if refusal is None and existing is None:
                db.execute(
                    "INSERT INTO edit_revision_marks (mark_id, repository, from_revision,"
                    " to_revision, actor, recorded_at) VALUES (?,?,?,?,?,?)",
                    (mark_id(repository, from_revision, to_revision), repository,
                     from_revision, to_revision, actor, now),
                )
            if refusal is None:
                rows = db.execute(
                    "SELECT agreement_id FROM edit_agreements"
                    "  WHERE repository = ? AND base_revision = ?"
                    "    AND state IN ('proposed','agreed') AND superseded_by IS NULL",
                    (repository, from_revision),
                ).fetchall()
                moved = [row["agreement_id"] for row in rows]
                # Proposed rows move too. Leaving them open would let a counterpart accept a
                # proposal about a tree that no longer exists.
                db.execute(
                    "UPDATE edit_agreements SET state = ?, updated_at = ?"
                    " WHERE repository = ? AND base_revision = ?"
                    "   AND state IN ('proposed','agreed') AND superseded_by IS NULL",
                    (REOPENED, now, repository, from_revision),
                )
                reached = self._chain_from(db, repository, from_revision)
                self.store.journal(
                    "edit_revision_restated", repository,
                    {"from": from_revision, "to": to_revision, "reopened": moved,
                     "alreadyRecorded": existing is not None}, at=now)
        if refusal is not None:
            raise refusal.error()
        return {"repository": repository, "fromRevision": from_revision,
                "toRevision": to_revision, "reopened": moved,
                "alreadyRecorded": existing is not None, "currentRevision": reached[-1]}

    def reaffirm(self, identifier, *, actor, base_revision, condition=None):
        """Carry an agreement onto the revision its own chain actually reaches, keeping it.

        A successor rather than an edit, because the region's id contains its revision: moving
        the row would leave an identifier its own columns no longer derive.

        What is carried (CRW-237). The successor keeps the predecessor's proposer, constraint
        and both sides' conditions, whichever side carries it; ``condition`` restates the
        carrier's OWN side only, stated on the new revision. Carrying accepts the carrier's side
        on the new tree. The other side's acceptance was given on the older tree and is not
        carried; the successor names that wait - reason, prior acceptance, command. The first
        version made the carrier the proposer, dropped both conditions and cleared the other
        side's acceptance without a word (CRW-124 G3).

        Two transactions. The first validates and records its refusals: ownership, an open
        agreement, a revision that actually moved (a revision is not its own successor), the end
        of the recorded chain (a typo is not a successor nobody recorded), and a successor place
        free of another pair's overlap and of this pair's own live agreement. It retires
        nothing. The second is propose() with the carry, which re-decides all of that where it
        writes and then retires, inserts and links in one transaction. The earlier version
        retired first and inserted a transaction later, so a move or a same-pair proposal
        landing in between left a successor on a superseded tree or nothing live at all.
        """
        now = self.clock.iso()
        error, carried = None, None
        with self.store.transaction() as db:
            row = db.execute(
                "SELECT a.*, r.path AS path, r.region_kind AS region_kind,"
                "       r.region_key AS region_key, r.region_class AS region_class,"
                "       r.regenerate_from AS regenerate_from"
                "  FROM edit_agreements a JOIN edit_regions r ON r.region_id = a.region_id"
                " WHERE a.agreement_id = ?", (identifier,)).fetchone()
            if row is None:
                raise CoordinationError(
                    RefusalReason.UNREGISTERED_SCOPE, "no agreement " + repr(identifier))
            _side, refusal = self._acting_side(db, row, actor, row["repository"])
            if refusal is None and row["state"] not in LIVE:
                # A withdrawn, declined or released agreement was decided. Carrying one
                # forward recreated it on a new revision, which is a new proposal wearing a
                # closed agreement's history.
                refusal = Refusal(
                    RefusalReason.AGREEMENT_NOT_OPEN,
                    "agreement " + repr(identifier) + " is " + row["state"]
                    + "; a closed agreement is not carried forward, it is proposed again",
                    domain=DOMAIN_EDIT_REGION, subject=row["repository"],
                    incumbent=row["state"], challenger=actor)
            if refusal is None and base_revision == row["base_revision"]:
                refusal = self._unmoved_refusal(
                    identifier, row["base_revision"], row["repository"], actor)
            if refusal is None:
                chain = self._chain_from(db, row["repository"], row["base_revision"])
                terminal = chain[-1] if chain else row["base_revision"]
                if base_revision != terminal:
                    refusal = Refusal(
                        RefusalReason.AGREEMENT_REVISION_STALE,
                        "this agreement stands on " + repr(row["base_revision"])
                        + ", whose recorded chain reaches " + repr(terminal) + ", not "
                        + repr(base_revision)
                        + ". Restate the revision first, or name the one the chain reaches",
                        domain=DOMAIN_EDIT_REGION, subject=row["repository"],
                        incumbent=terminal, challenger=base_revision)
            if refusal is not None:
                self.conflicts.record_in(db, refusal, at=now)
                error = refusal.error()
            else:
                # Refused here, where it records, rather than discovered after a write. The
                # carry's own transaction decides all of it again before retiring anything.
                successor_region = {
                    "region_id": region_id(
                        row["repository"], base_revision, row["path"],
                        row["region_kind"], row["region_key"] or ""),
                    "repository": row["repository"], "base_revision": base_revision,
                    "path": row["path"], "region_kind": row["region_kind"],
                    "region_key": row["region_key"] or "",
                    "region_class": row["region_class"],
                }
                low, high = sorted((row["left_project"], row["right_project"]))
                blocked = self._overlap_refusal(db, successor_region, low, high, actor) \
                    or self._peer_refusal(
                        db, low, high, row["peer_link_id"], actor, row["repository"])
                if blocked is None:
                    standing = db.execute(
                        "SELECT agreement_id FROM edit_agreements"
                        "  WHERE region_id = ? AND left_project = ? AND right_project = ?"
                        "    AND state IN ('proposed','agreed','reopened')"
                        "    AND superseded_by IS NULL",
                        (successor_region["region_id"], low, high)).fetchone()
                    if standing is not None:
                        blocked = self._standing_refusal(
                            standing["agreement_id"], identifier, base_revision,
                            row["repository"], actor)
                if blocked is not None:
                    self.conflicts.record_in(db, blocked, at=now)
                    # Carried OUT of the transaction. Raising here rolls back the very row
                    # that records the contest, which is the one thing this module's write
                    # protocol exists to prevent.
                    error = blocked.error()
                else:
                    carried = dict(row)
        if error is not None:
            raise error
        return self.propose(
            repository=carried["repository"], base_revision=base_revision,
            path=carried["path"], region_kind=carried["region_kind"],
            region_key=carried["region_key"], region_class=carried["region_class"],
            regenerate_from=carried["regenerate_from"],
            left_project=carried["left_project"], right_project=carried["right_project"],
            peer_link_id=carried["peer_link_id"], proposer_task_id=actor,
            constraint_text=carried["constraint_text"], issue_key=carried["issue_key"],
            next_owner=carried["next_owner"], supersedes=identifier,
            carry={"predecessor": identifier, "restated": condition})

    # -------------------------------------------------------------- follow-ups

    def followup(self, agreement, *, trigger_text, acceptance_text, recorded_by,
                 issue_ref=None, assignee_task_id=None, assignee_project=None):
        """What the two sides agreed should happen next, with or without somebody to do it.

        Removing a temporary duplicate implementation is the shape this exists for: the
        trigger that made it necessary, what would count as done, whether anybody took it, and
        the issue it belongs to all have to survive the agreement being released, because the
        work does.
        """
        exact(trigger_text, "a follow-up trigger")
        exact(acceptance_text, "a follow-up acceptance criterion")
        agreement_row = self.store.one(
            "SELECT left_project, right_project FROM edit_agreements"
            "  WHERE agreement_id = ?", (agreement,))
        if agreement_row is not None and self._owned_side(
                agreement_row["left_project"], agreement_row["right_project"],
                recorded_by) is None:
            raise CoordinationError(
                RefusalReason.SCOPE_ROLE_MISMATCH,
                "task " + repr(recorded_by) + " owns neither side of agreement "
                + repr(agreement) + ", so it cannot append work to it")
        if assignee_task_id and assignee_task_id != recorded_by:
            # Recording a follow-up is not accepting one on another task's behalf. Trusting
            # the field attributed accepted work to a task that never agreed to it, which is
            # exactly what the unassigned rule exists to prevent.
            raise CoordinationError(
                RefusalReason.SCOPE_ROLE_MISMATCH,
                "a follow-up records its own author as the assignee or nobody; "
                + repr(recorded_by) + " cannot accept it for " + repr(assignee_task_id)
                + ", who accepts it themselves")
        identifier = followup_id(agreement, trigger_text)
        now = self.clock.iso()
        with self.store.transaction() as db:
            if db.execute(
                "SELECT 1 FROM edit_agreements WHERE agreement_id = ?", (agreement,)
            ).fetchone() is None:
                raise CoordinationError(
                    RefusalReason.UNREGISTERED_SCOPE, "no agreement " + repr(agreement))
            db.execute(
                "INSERT INTO edit_followups (followup_id, agreement_id, trigger_text,"
                " acceptance_text, issue_ref, assignee_task_id, assignee_project,"
                " accepted_at, state, recorded_by, recorded_at, updated_at)"
                " VALUES (?,?,?,?,?,?,?,?,?,?,?,?)"
                " ON CONFLICT (followup_id) DO NOTHING",
                (identifier, agreement, trigger_text, acceptance_text, issue_ref,
                 assignee_task_id, assignee_project,
                 now if assignee_task_id else None,
                 ACCEPTED if assignee_task_id else OPEN, recorded_by, now, now),
            )
        return self._followup_record(self.store.one(
            "SELECT * FROM edit_followups WHERE followup_id = ?", (identifier,)))

    def accept_followup(self, identifier, *, actor, assignee_project):
        """Somebody takes it. Until then it belongs to nobody and is counted as nobody's."""
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            row = self._followup_row_in(db, identifier)
            agreement = db.execute(
                "SELECT * FROM edit_agreements WHERE agreement_id = ?",
                (row["agreement_id"],)).fetchone()
            side, refusal = self._acting_side(
                db, agreement, actor, agreement["repository"])
            if refusal is None and assignee_project != agreement[side]:
                # Otherwise a parent could file its accepted work under the peer's project and
                # the aggregate would credit the wrong side.
                refusal = Refusal(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "task " + repr(actor) + " owns " + repr(agreement[side])
                    + ", so its acceptance is recorded under that project, not "
                    + repr(assignee_project),
                    domain=DOMAIN_EDIT_REGION, subject=agreement["repository"],
                    incumbent=agreement[side], challenger=assignee_project)
            if refusal is None and row["state"] in (DONE, DROPPED):
                # done and dropped are terminal. Accepting over one returned closed work to
                # the active list while close_reason still explained why it had ended.
                refusal = Refusal(
                    RefusalReason.AGREEMENT_NOT_OPEN,
                    "follow-up " + repr(identifier) + " is " + row["state"]
                    + ", which is terminal; a new follow-up records new work",
                    domain=DOMAIN_EDIT_REGION, subject=agreement["repository"],
                    incumbent=row["state"], challenger=actor)
            if refusal is None and row["assignee_task_id"] \
                    and row["assignee_task_id"] != actor:
                refusal = Refusal(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "follow-up " + repr(identifier) + " was already accepted by "
                    + repr(row["assignee_task_id"]) + "; taking it from them is not an"
                    " acceptance",
                    domain=DOMAIN_EDIT_REGION, subject=agreement["repository"],
                    incumbent=row["assignee_task_id"], challenger=actor)
            if refusal is None:
                db.execute(
                    "UPDATE edit_followups SET assignee_task_id = ?, assignee_project = ?,"
                    " accepted_at = ?, state = ? , updated_at = ? WHERE followup_id = ?",
                    (actor, assignee_project, now, ACCEPTED, now, identifier),
                )
                self.store.journal(
                    "edit_followup_accepted", identifier, {"actor": actor}, at=now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self._followup_record(self.store.one(
            "SELECT * FROM edit_followups WHERE followup_id = ?", (identifier,)))

    def settle_followup(self, identifier, *, actor, disposition, reason=None):
        """Finish it or drop it. Nobody can finish what nobody took.

        Completing an unassigned follow-up is the write-side form of counting it under a
        parent that never accepted it, so it is refused by name. Dropping one stays available,
        because an unassigned follow-up should not be immortal either.
        """
        if disposition not in FOLLOWUP_DISPOSITIONS:
            raise CoordinationError(
                RefusalReason.LINK_NOT_ACTIVE,
                "a follow-up disposition is " + " or ".join(FOLLOWUP_DISPOSITIONS)
                + ", not " + repr(disposition))
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            row = self._followup_row_in(db, identifier)
            agreement = db.execute(
                "SELECT * FROM edit_agreements WHERE agreement_id = ?",
                (row["agreement_id"],)).fetchone()
            _side, refusal = self._acting_side(
                db, agreement, actor, agreement["repository"])
            if refusal is None and row["state"] in (DONE, DROPPED) \
                    and row["state"] != disposition:
                # A terminal settlement is a decision. Rewriting done to dropped left the
                # history saying something nobody decided.
                refusal = Refusal(
                    RefusalReason.AGREEMENT_NOT_OPEN,
                    "follow-up " + repr(identifier) + " was already settled as "
                    + row["state"] + "; that decision stands",
                    domain=DOMAIN_EDIT_REGION, subject=agreement["repository"],
                    incumbent=row["state"], challenger=disposition)
            if refusal is None and disposition == DONE and not row["assignee_task_id"]:
                refusal = Refusal(
                    RefusalReason.FOLLOWUP_UNASSIGNED,
                    "follow-up " + repr(identifier) + " was never accepted by anybody, so"
                    " nobody can report it done. Accept it first, or drop it",
                    domain=DOMAIN_EDIT_REGION, subject=agreement["repository"],
                    challenger=actor)
            if refusal is None and disposition == DONE \
                    and row["assignee_task_id"] != actor:
                # Owning the other side of the agreement is not authority over the work.
                # Reporting somebody else's accepted work finished is the same misattribution
                # the unassigned case refuses, one step later.
                refusal = Refusal(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "follow-up " + repr(identifier) + " was accepted by "
                    + repr(row["assignee_task_id"]) + ", so only they can report it done",
                    domain=DOMAIN_EDIT_REGION, subject=agreement["repository"],
                    incumbent=row["assignee_task_id"], challenger=actor)
            if refusal is None:
                db.execute(
                    "UPDATE edit_followups SET state = ?, close_reason = ?, updated_at = ?"
                    " WHERE followup_id = ?", (disposition, reason, now, identifier))
                self.store.journal(
                    "edit_followup_settled", identifier,
                    {"disposition": disposition, "actor": actor}, at=now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self._followup_record(self.store.one(
            "SELECT * FROM edit_followups WHERE followup_id = ?", (identifier,)))

    @staticmethod
    def _followup_row_in(db, identifier):
        row = db.execute(
            "SELECT * FROM edit_followups WHERE followup_id = ?", (identifier,)).fetchone()
        if row is None:
            raise CoordinationError(
                RefusalReason.UNREGISTERED_SCOPE, "no follow-up " + repr(identifier))
        return row
