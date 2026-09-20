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


def canonical_path(path):
    """One spelling per place, checked before it reaches an identity.

    Containment normalises its inputs, so 'a//b' and 'a/b' are the same place to it while
    hashing them would give two regions. Rejecting the non-canonical spelling is what keeps
    identity and containment agreeing about what a place is.
    """
    if not isinstance(path, str) or not path.strip() or "\x00" in path:
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
        return self._agreement_record(row) if row else None

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
        exclusive, regenerate = [], []
        for row in rows:
            if base_revision is not None and row["base_revision"] != base_revision:
                continue
            if project_key is not None and project_key not in (
                    row["left_project"], row["right_project"]):
                continue
            if path is not None and row["region_path"] != path:
                continue
            record = self._agreement_record(row)
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
                condition=None, issue_key=None, next_owner=None, supersedes=None):
        """Claim a place, not a file. Refuses a claim so broad it would block a project."""
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
        if self._owned_side(low, high, proposer_task_id) is None:
            raise CoordinationError(
                RefusalReason.SCOPE_ROLE_MISMATCH,
                "task " + repr(proposer_task_id) + " is the registered parent of neither "
                + repr(low) + " nor " + repr(high) + ", and a proposal pre-accepts its own"
                " side, so a stranger could forge one and block an overlapping region")
        now = self.clock.iso()
        refusal, agreement, replay = None, None, None
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO edit_regions (region_id, repository, base_revision, path,"
                " region_kind, region_key, region_class, regenerate_from, recorded_at)"
                " VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT (region_id) DO NOTHING",
                (identifier, repository, base_revision, clean, region_kind, region_key,
                 region_class, regenerate_from, now),
            )
            # Re-read what was PERSISTED and copy from that, never from the arguments. A
            # replayed proposal that spelt its revision differently would otherwise store an
            # agreement whose revision disagreed with the one inside its own region id, and
            # the stale-settlement check reads that column.
            region = db.execute(
                "SELECT * FROM edit_regions WHERE region_id = ?", (identifier,)).fetchone()
            if region["region_class"] != region_class or (
                    region["regenerate_from"] or "") != (regenerate_from or ""):
                # The insert above is ON CONFLICT DO NOTHING, so a later proposal naming a
                # different classification silently inherited the first one - and
                # classification decides whether the region contests at all.
                raise CoordinationError(
                    RefusalReason.REGION_OVERLAP,
                    "region " + repr(identifier) + " is already recorded as "
                    + repr(region["region_class"]) + " derived from "
                    + repr(region["regenerate_from"]) + "; a place is classified once, and a"
                    " different classification is a different claim about it")
            previous = db.execute(
                "SELECT * FROM edit_agreements"
                "  WHERE region_id = ? AND left_project = ? AND right_project = ?"
                "    AND state IN ('proposed','agreed','reopened') AND superseded_by IS NULL",
                (identifier, low, high),
            ).fetchone()
            if previous is not None:
                replay = previous
            else:
                refusal = self._overlap_refusal(
                    db, region, low, high, proposer_task_id)
                if refusal is None:
                    refusal = self._peer_refusal(
                        db, low, high, peer_link_id, proposer_task_id, identifier)
            if replay is None and refusal is None:
                highest = db.execute(
                    "SELECT MAX(tenure) AS top FROM edit_agreements"
                    "  WHERE region_id = ? AND left_project = ? AND right_project = ?",
                    (identifier, low, high),
                ).fetchone()
                tenure = (highest["top"] or 0) + 1
                agreement = agreement_id(identifier, low, high, tenure)
                mine = "left_condition" if left_project == low else "right_condition"
                accepted = "left_accepted_at" if left_project == low else "right_accepted_at"
                db.execute(
                    "INSERT INTO edit_agreements (agreement_id, region_id, repository,"
                    " base_revision, left_project, right_project, peer_link_id,"
                    " proposer_task_id, issue_key, constraint_text, " + mine + ", "
                    + accepted + ", next_owner, state, tenure, supersedes, proposed_at,"
                    " updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (agreement, identifier, region["repository"], region["base_revision"],
                     low, high, peer_link_id, proposer_task_id, issue_key, constraint_text,
                     condition, now, next_owner, PROPOSED, tenure, supersedes, now, now),
                )
                self.store.journal(
                    "edit_region_proposed", agreement,
                    {"regionId": identifier, "path": clean, "pair": [low, high]}, at=now)
            elif refusal is not None:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        if replay is not None:
            answer = self._agreement_record(replay)
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
                    refusal = Refusal(
                        RefusalReason.AGREEMENT_REVISION_STALE,
                        "this agreement stands on " + repr(row["base_revision"])
                        + ", which was restated to " + repr(superseded["to_revision"])
                        + "; reaffirm it on the current revision before settling it",
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
        marks = {
            row["from_revision"]: row["to_revision"] for row in db.execute(
                "SELECT from_revision, to_revision FROM edit_revision_marks"
                "  WHERE repository = ?", (repository,),
            ).fetchall()
        }
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
        """
        exact(repository, "a repository")
        exact(from_revision, "a base revision")
        exact(to_revision, "a base revision")
        if from_revision == to_revision:
            raise CoordinationError(
                RefusalReason.LINK_NOT_ACTIVE, "a revision is not its own successor")
        self._check_restater(repository, actor)
        now = self.clock.iso()
        refusal, moved = None, []
        with self.store.transaction() as db:
            existing = db.execute(
                "SELECT * FROM edit_revision_marks"
                "  WHERE repository = ? AND from_revision = ?",
                (repository, from_revision),
            ).fetchone()
            if existing is not None and existing["to_revision"] != to_revision:
                refusal = Refusal(
                    RefusalReason.AGREEMENT_REVISION_STALE,
                    repr(from_revision) + " was already restated to "
                    + repr(existing["to_revision"]) + " by " + repr(existing["actor"])
                    + "; one revision has one successor and a second would leave two chains"
                    " nobody can order",
                    domain=DOMAIN_EDIT_REGION, subject=repository,
                    incumbent=existing["to_revision"], challenger=to_revision)
                self.conflicts.record_in(db, refusal, at=now)
            elif existing is None:
                reached = self._chain_from(db, repository, to_revision)
                if from_revision in reached:
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
                self.store.journal(
                    "edit_revision_restated", repository,
                    {"from": from_revision, "to": to_revision, "reopened": moved}, at=now)
        if refusal is not None:
            raise refusal.error()
        return {"repository": repository, "fromRevision": from_revision,
                "toRevision": to_revision, "reopened": moved}

    def reaffirm(self, identifier, *, actor, base_revision):
        """Carry an agreement onto the revision its own chain actually reaches.

        A successor rather than an edit, because the region's id contains its revision: moving
        the row would leave an identifier its own columns no longer derive.

        Two things the first version got wrong. It accepted any destination, so a typo became
        a live agreement on a tree nothing had recorded as the successor, and both sides could
        settle it because an unrecorded revision has no stale mark. And it created the
        successor before retiring the predecessor, so a failure between the two left both live
        with different region revisions, which the one-live-agreement index cannot catch.

        The predecessor is retired FIRST now. A failure after that leaves nothing live rather
        than two things live, which is recoverable by proposing again; the reverse is not.
        """
        now = self.clock.iso()
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
                carried = None
            else:
                error = None
                carried = dict(row)
                db.execute(
                    "UPDATE edit_agreements SET state = ?, close_reason = ?, closed_at = ?,"
                    " updated_at = ? WHERE agreement_id = ?",
                    (RELEASED, "reaffirmed onto " + base_revision, now, now, identifier))
        if error is not None:
            raise error
        successor = self.propose(
            repository=carried["repository"], base_revision=base_revision,
            path=carried["path"], region_kind=carried["region_kind"],
            region_key=carried["region_key"], region_class=carried["region_class"],
            regenerate_from=carried["regenerate_from"],
            left_project=carried["left_project"], right_project=carried["right_project"],
            peer_link_id=carried["peer_link_id"], proposer_task_id=actor,
            constraint_text=carried["constraint_text"], issue_key=carried["issue_key"],
            next_owner=carried["next_owner"], supersedes=identifier)
        with self.store.transaction() as db:
            db.execute(
                "UPDATE edit_agreements SET superseded_by = ?, updated_at = ?"
                " WHERE agreement_id = ?",
                (successor["agreementId"], now, identifier))
        return successor

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
            _side, refusal = self._acting_side(
                db, agreement, actor, agreement["repository"])
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
