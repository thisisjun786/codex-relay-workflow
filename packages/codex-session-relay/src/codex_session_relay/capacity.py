"""How much concurrent execution a scope is using, against what somebody declared.

Two different things live here and keeping them apart is the point rather than a tidiness
preference.

The first is a COUNT this store can take for itself: how many execution subjects a parent, or
a whole initiative, is holding a slot for right now. That number is always COUNT(*) over held
rows and never a stored counter, so there is nothing to decrement twice and nothing to leak
when a process dies between a decrement and the row that was meant to explain it.

The second is a declared RESOURCE or COST ceiling - file descriptors, model spend - and this
store cannot measure one. Those are facts about a host and an account that no number of rows
here observes. So a limit on such a dimension takes its current value only from an explicit
observation, and with none recorded the answer is unmeasured rather than zero. Treating an
absent measurement as no usage would be exactly the inference this separation exists to
prevent, reached by a longer route: a task count is not evidence about a file descriptor
limit, however many tasks there are.

The module is Capacity.reserve rather than Capacity.acquire because the suite's regression map
tracks calls by bare name and scope.acquire is already a declared folded boolean; a test
calling capacity.acquire would be classified as a summary read of that one and fail an
inventory declared in a file this work may not edit.
"""

from .coordination import DOMAIN_EXECUTION, Conflicts, Refusal, derive, exact
from .errors import CoordinationError, RefusalReason

PROJECT = "project"
PARENT = "parent"
INITIATIVE = "initiative"
STORE = "store"

HELD = "held"
RELEASED = "released"

RUNS = "runs"
SCOPES = (INITIATIVE, PROJECT, STORE)

DERIVED_FROM_SLOTS = "derived_from_slots"
OBSERVED = "observed"
UNMEASURED = "unmeasured"

WITHIN = "within"
OVER_CEILING = "over_ceiling"


def slot_id(subject_kind, subject_key, tenure):
    """One subject holds one slot, and a later tenure is a different one.

    Without the tenure a re-reservation would either collide with the retained released row or
    have to overwrite it, and that row is the evidence a duplicate or contradictory release is
    detected against.
    """
    return derive(
        "slt", exact(subject_kind, "a subject kind"), exact(subject_key, "a subject key"),
        str(tenure))


def limit_id(scope_kind, scope_key, dimension):
    """One ceiling per dimension per scope, so re-declaring updates rather than accumulates."""
    return derive(
        "lim", exact(scope_kind, "a scope kind"), exact(scope_key, "a scope key"),
        exact(dimension, "a dimension"))


class Capacity:
    """Execution slots, declared ceilings, and what each of them can honestly answer."""

    def __init__(self, store, clock, linkage):
        self.store = store
        self.clock = clock
        self.linkage = linkage
        self.conflicts = Conflicts(store)

    # ---------------------------------------------------------------- reading

    def slot(self, subject_kind, subject_key):
        row = self.store.one(
            "SELECT * FROM execution_slots"
            "  WHERE subject_kind = ? AND subject_key = ?"
            "  ORDER BY tenure DESC LIMIT 1",
            (subject_kind, subject_key),
        )
        return self._slot_record(row) if row else None

    def report(self, *, project_key=None, parent_task_id=None, initiative_key=None):
        """Held slots, counted per parent and in total, plus any that outlived their scope."""
        rows = self.store.all(
            "SELECT * FROM execution_slots WHERE state = ?"
            "  ORDER BY reserved_at, slot_id",
            (HELD,),
        )
        held = [self._slot_record(row) for row in rows]
        if project_key is not None:
            held = [record for record in held if record["projectKey"] == project_key]
        if parent_task_id is not None:
            held = [record for record in held if record["parentTaskId"] == parent_task_id]
        if initiative_key is not None:
            held = [record for record in held if record["initiativeKey"] == initiative_key]
        per_parent = {}
        for record in held:
            per_parent[record["parentTaskId"]] = per_parent.get(
                record["parentTaskId"], 0) + 1
        return {
            "held": held, "total": len(held), "perParent": per_parent,
            "staleSlots": [
                record for record in held
                if not self._owner_matches(record["projectKey"], record["parentTaskId"])
            ],
        }

    def _owner_matches(self, project_key, task_id):
        owners = [
            record["taskId"] for record in self.linkage.owners(PROJECT, project_key)
            if record["role"] == PARENT
        ]
        return owners == [task_id]

    def headroom(self, scope_kind, scope_key):
        """Per dimension: what is used, against what was declared, and HOW that use is known.

        proof is the field that matters. A runs figure is derived from the slots this store
        holds; anything else is observed or it is unmeasured, and the two are never mixed. A
        reader that only looked at used and ceiling could not tell a measured ninety-nine from
        a guessed one, which is the distinction the whole table split exists to preserve.
        """
        answer = {"scopeKind": scope_kind, "scopeKey": scope_key, "dimensions": []}
        for row in self.store.all(
            "SELECT * FROM execution_limits"
            "  WHERE scope_kind = ? AND scope_key = ? ORDER BY dimension",
            (scope_kind, scope_key),
        ):
            answer["dimensions"].append(self._dimension(row))
        return answer

    def _dimension(self, row):
        dimension, ceiling = row["dimension"], row["ceiling"]
        if dimension == RUNS:
            used = self._runs_in(None, row["scope_kind"], row["scope_key"])
            proof = DERIVED_FROM_SLOTS
        else:
            seen = self.store.one(
                "SELECT * FROM execution_usage"
                "  WHERE scope_kind = ? AND scope_key = ? AND dimension = ?",
                (row["scope_kind"], row["scope_key"], dimension),
            )
            used = seen["observed"] if seen else None
            proof = OBSERVED if seen else UNMEASURED
        if used is None:
            state = UNMEASURED
        elif used > ceiling:
            state = OVER_CEILING
        else:
            state = WITHIN
        return {
            "dimension": dimension, "unit": row["unit"], "ceiling": ceiling,
            "used": used, "proof": proof, "state": state,
            "enforce": row["enforce"] == 1, "revision": row["revision"],
            "declaredBy": row["declared_by"], "source": row["source"],
            "overBy": (used - ceiling) if used is not None and used > ceiling else 0,
            "note": None if dimension == RUNS else
            "a slot count is not a measurement of this dimension",
        }

    @staticmethod
    def _slot_record(row):
        return {
            "slotId": row["slot_id"], "subjectKind": row["subject_kind"],
            "subjectKey": row["subject_key"], "parentTaskId": row["parent_task_id"],
            "projectKey": row["project_key"], "initiativeKey": row["initiative_key"],
            "tenure": row["tenure"], "state": row["state"],
            "reservedBy": row["reserved_by"], "reservedAt": row["reserved_at"],
            "releasedAt": row["released_at"], "releasedBy": row["released_by"],
            "releaseReason": row["release_reason"], "detail": row["detail"],
        }
    # ---------------------------------------------------------------- counting

    def _runs_in(self, db, scope_kind, scope_key):
        """Held slots inside a scope. Always counted, never stored."""
        reader = db if db is not None else self.store.db
        if scope_kind == STORE:
            statement = "SELECT COUNT(*) AS tally FROM execution_slots WHERE state = ?"
            parameters = (HELD,)
        elif scope_kind == PROJECT:
            statement = ("SELECT COUNT(*) AS tally FROM execution_slots"
                         "  WHERE state = ? AND project_key = ?")
            parameters = (HELD, scope_key)
        else:
            statement = ("SELECT COUNT(*) AS tally FROM execution_slots"
                         "  WHERE state = ? AND initiative_key = ?")
            parameters = (HELD, scope_key)
        return reader.execute(statement, parameters).fetchone()["tally"]

    def _supervisor_of(self, project_key):
        """The initiative supervisor above a project, or None when the walk cannot say."""
        owners = [
            record["taskId"] for record in self.linkage.owners(PROJECT, project_key)
            if record["role"] == PARENT
        ]
        if len(owners) != 1:
            return None
        walk = self.linkage.up(task_id=owners[0], scope_key=project_key)
        if not walk.get("readable") or walk.get("state") == "ambiguous":
            return None
        for level in walk.get("levels", []):
            if level.get("scopeKind") == INITIATIVE:
                return (level.get("owner") or {}).get("taskId")
        return None

    def _initiative_of(self, project_key):
        """The initiative above a project, or None when the walk cannot say.

        An ambiguous or unreadable answer means no initiative ceiling is consulted, and
        headroom reports the absence rather than substituting a guess for it.
        """
        owners = [
            record["taskId"] for record in self.linkage.owners(PROJECT, project_key)
            if record["role"] == PARENT
        ]
        if len(owners) != 1:
            return None
        walk = self.linkage.up(task_id=owners[0], scope_key=project_key)
        if not walk.get("readable") or walk.get("state") == "ambiguous":
            return None
        for level in walk.get("levels", []):
            if level.get("scopeKind") == INITIATIVE:
                return level.get("scopeKey")
        return None

    # ---------------------------------------------------------------- writing
    @staticmethod
    def _check_scope(scope_kind, scope_key):
        """The store scope has exactly one key, because enforcement reads exactly one.

        _ceiling_refusal queries (STORE, STORE). A declaration under any other store key was
        accepted, stored and then never consulted, so an operator who set a global ceiling saw
        it succeed and saw it ignored.
        """
        if scope_kind == STORE and scope_key != STORE:
            raise CoordinationError(
                RefusalReason.LINK_NOT_ACTIVE,
                "the store scope has one key, " + repr(STORE) + ", not " + repr(scope_key)
                + "; enforcement reads that key and a ceiling under any other would be"
                " recorded and never applied")

    def _check_declarer(self, scope_kind, scope_key, actor):
        """A ceiling and a measurement are somebody's statements, not anybody's.

        A caller that could raise a ceiling, disable enforcement or publish a usage figure
        could admit execution the owner had bounded. So the declarer has to be the registered
        owner of the scope it is speaking for.

        The store scope is the recorded exception: it has no owner of its own, so any task
        holding a live supervisor binding may set it, and that is stated rather than
        disguised as a stronger check.
        """
        if scope_kind == STORE:
            held = self.store.one(
                "SELECT task_id FROM scope_bindings"
                "  WHERE task_id = ? AND role = 'supervisor'"
                "    AND status IN ('active','paused') AND superseded_by IS NULL",
                (actor,),
            )
            if held is None:
                raise CoordinationError(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "task " + repr(actor) + " holds no live supervisor binding, and the store"
                    " scope has no owner of its own to speak for it")
            return
        role = PARENT if scope_kind == PROJECT else "supervisor"
        owners = [
            record["taskId"] for record in self.linkage.owners(scope_kind, scope_key)
            if record["role"] == role
        ]
        if owners != [actor]:
            raise CoordinationError(
                RefusalReason.SCOPE_ROLE_MISMATCH,
                "task " + repr(actor) + " is not the registered " + role + " of "
                + scope_kind + " " + repr(scope_key)
                + (", which is held by " + repr(owners[0]) if len(owners) == 1
                   else ", which has " + str(len(owners)) + " live owners")
                + ", so it cannot state a bound for it")


    def reserve(self, *, subject_kind, subject_key, parent_task_id, project_key,
                reserved_by, detail=None):
        """Take a slot for one execution subject, inside the transaction that counts.

        Counting after the write would let two interleaved callers both pass a ceiling of one,
        which is why the count, the decision and the insert share a single BEGIN IMMEDIATE.
        """
        exact(project_key, "a project key")
        exact(parent_task_id, "a task id")
        now = self.clock.iso()
        refusal, identifier, existing = None, None, None
        initiative = self._initiative_of(project_key)
        with self.store.transaction() as db:
            owners = [
                record["taskId"] for record in self.linkage.owners(PROJECT, project_key)
                if record["role"] == PARENT
            ]
            if len(owners) > 1:
                refusal = Refusal(
                    RefusalReason.DUPLICATE_SCOPE_OWNER,
                    "project " + repr(project_key) + " has more than one live parent ("
                    + ", ".join(repr(t) for t in sorted(owners)) + "), so there is no owner to"
                    " reserve under",
                    domain=DOMAIN_EXECUTION, subject=subject_key,
                    incumbent=sorted(owners)[0], challenger=parent_task_id)
            elif not owners:
                refusal = Refusal(
                    RefusalReason.UNREGISTERED_SCOPE,
                    "project " + repr(project_key) + " has no registered parent",
                    domain=DOMAIN_EXECUTION, subject=subject_key,
                    challenger=parent_task_id)
            elif owners[0] != parent_task_id:
                refusal = Refusal(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "task " + repr(parent_task_id) + " is not the registered parent of project"
                    " " + repr(project_key) + ", which is held by " + repr(owners[0]),
                    domain=DOMAIN_EXECUTION, subject=subject_key,
                    incumbent=owners[0], challenger=parent_task_id)
            if refusal is None:
                existing = db.execute(
                    "SELECT * FROM execution_slots"
                    "  WHERE subject_kind = ? AND subject_key = ? AND state = ?",
                    (subject_kind, subject_key, HELD),
                ).fetchone()
                if existing is not None and (
                        existing["parent_task_id"] != parent_task_id
                        or existing["project_key"] != project_key):
                    # A globally held subject is not this caller's replay. Answering
                    # alreadyHeld handed another project's slot back as if it were theirs, so
                    # neither their ownership nor their ceiling was ever consulted.
                    refusal = Refusal(
                        RefusalReason.DISPOSITION_CONFLICT,
                        repr(subject_key) + " is already held by "
                        + repr(existing["parent_task_id"]) + " for project "
                        + repr(existing["project_key"]) + ", not by " + repr(parent_task_id)
                        + " for " + repr(project_key),
                        domain=DOMAIN_EXECUTION, subject=subject_key,
                        incumbent=existing["parent_task_id"], challenger=parent_task_id)
                    existing = None
            if refusal is None and existing is None:
                refusal = self._ceiling_refusal(
                    db, project_key, initiative, parent_task_id, subject_key)
            if refusal is None and existing is None:
                previous = db.execute(
                    "SELECT MAX(tenure) AS highest FROM execution_slots"
                    "  WHERE subject_kind = ? AND subject_key = ?",
                    (subject_kind, subject_key),
                ).fetchone()
                tenure = (previous["highest"] or 0) + 1
                identifier = slot_id(subject_kind, subject_key, tenure)
                db.execute(
                    "INSERT INTO execution_slots (slot_id, subject_kind, subject_key,"
                    " parent_task_id, project_key, initiative_key, tenure, state, reserved_by,"
                    " reserved_at, detail) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                    (identifier, subject_kind, subject_key, parent_task_id, project_key,
                     initiative, tenure, HELD, reserved_by, now, detail),
                )
                self.store.journal(
                    "slot_reserved", identifier,
                    {"subjectKey": subject_key, "parentTaskId": parent_task_id,
                     "tenure": tenure}, at=now)
            elif refusal is not None:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        if existing is not None:
            answer = self._slot_record(existing)
            answer["alreadyHeld"] = True
            return answer
        answer = self.slot(subject_kind, subject_key)
        answer["alreadyHeld"] = False
        return answer

    def _ceiling_refusal(self, db, project_key, initiative, parent_task_id, subject):
        """Every enforced ceiling in scope, runs counted and everything else observed."""
        scopes = [(PROJECT, project_key), (STORE, STORE)]
        if initiative is not None:
            scopes.insert(0, (INITIATIVE, initiative))
        for scope_kind, scope_key in scopes:
            for row in db.execute(
                "SELECT * FROM execution_limits"
                "  WHERE scope_kind = ? AND scope_key = ? AND enforce = 1"
                "  ORDER BY dimension",
                (scope_kind, scope_key),
            ).fetchall():
                if row["dimension"] == RUNS:
                    used = self._runs_in(db, scope_kind, scope_key)
                    if used >= row["ceiling"]:
                        return Refusal(
                            RefusalReason.CAPACITY_EXHAUSTED,
                            scope_kind + " " + repr(scope_key) + " already holds "
                            + str(used) + " of " + str(row["ceiling"]) + " runs",
                            domain=DOMAIN_EXECUTION, subject=subject,
                            incumbent=scope_key, challenger=parent_task_id)
                    continue
                seen = db.execute(
                    "SELECT * FROM execution_usage"
                    "  WHERE scope_kind = ? AND scope_key = ? AND dimension = ?",
                    (scope_kind, scope_key, row["dimension"]),
                ).fetchone()
                if seen is None:
                    return Refusal(
                        RefusalReason.CAPACITY_UNMEASURED,
                        "an enforced ceiling of " + str(row["ceiling"]) + " "
                        + row["unit"] + " is declared for " + repr(row["dimension"]) + " on "
                        + scope_kind + " " + repr(scope_key) + " and nothing has measured it."
                        " A count of running tasks is not a measurement of this dimension, so"
                        " there is no basis to say whether the bound holds",
                        domain=DOMAIN_EXECUTION, subject=subject,
                        incumbent=row["dimension"], challenger=parent_task_id)
                if seen["observed"] >= row["ceiling"]:
                    return Refusal(
                        RefusalReason.CAPACITY_EXHAUSTED,
                        repr(row["dimension"]) + " was observed at " + str(seen["observed"])
                        + " " + row["unit"] + " against a ceiling of " + str(row["ceiling"]),
                        domain=DOMAIN_EXECUTION, subject=subject,
                        incumbent=row["dimension"], challenger=parent_task_id)
        return None

    def release(self, *, subject_kind, subject_key, released_by, reason):
        """Give the slot back. Idempotent on the subject, and a changed reason is refused.

        Completion, failure, a resume and a duplicated notification all arrive here with the
        same subject, so the second one must not be a second release. Restating the SAME reason
        converges; restating a DIFFERENT one is a disagreement about what happened and is
        refused with the contest retained, rather than silently overwriting the first answer.
        """
        exact(reason, "a release reason")
        now = self.clock.iso()
        refusal, already = None, None
        with self.store.transaction() as db:
            row = db.execute(
                "SELECT * FROM execution_slots"
                "  WHERE subject_kind = ? AND subject_key = ?"
                "  ORDER BY tenure DESC LIMIT 1",
                (subject_kind, subject_key),
            ).fetchone()
            if row is None:
                raise CoordinationError(
                    RefusalReason.SLOT_UNKNOWN,
                    "no slot was ever reserved for " + subject_kind + " " + repr(subject_key))
            if row["parent_task_id"] != released_by \
                    and released_by != self._supervisor_of(row["project_key"]):
                # Freeing somebody else's slot admits work past the owner's own reservation,
                # which is the ceiling failing open through the release door.
                raise CoordinationError(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "slot " + repr(row["slot_id"]) + " is held by "
                    + repr(row["parent_task_id"]) + ", so " + repr(released_by)
                    + " cannot release it")
            if row["state"] == RELEASED:
                if row["release_reason"] != reason:
                    refusal = Refusal(
                        RefusalReason.DISPOSITION_CONFLICT,
                        "slot " + repr(row["slot_id"]) + " was released as "
                        + repr(row["release_reason"]) + " by " + repr(row["released_by"])
                        + "; a later notification restates that reason rather than replacing"
                        " it",
                        domain=DOMAIN_EXECUTION, subject=subject_key,
                        incumbent=row["release_reason"] or "", challenger=reason)
                    self.conflicts.record_in(db, refusal, at=now)
                else:
                    already = row
            else:
                # The guard is part of the UPDATE rather than a preflight read, so two
                # concurrent releases cannot both find it held and both write.
                db.execute(
                    "UPDATE execution_slots SET state = ?, released_at = ?, released_by = ?,"
                    " release_reason = ? WHERE slot_id = ? AND state = ?",
                    (RELEASED, now, released_by, reason, row["slot_id"], HELD),
                )
                self.store.journal(
                    "slot_released", row["slot_id"],
                    {"subjectKey": subject_key, "reason": reason}, at=now)
        if refusal is not None:
            raise refusal.error()
        answer = self._slot_record(already) if already is not None else self.slot(
            subject_kind, subject_key)
        answer["alreadyReleased"] = already is not None
        return answer

    def declare_limit(self, *, scope_kind, scope_key, dimension, unit, ceiling,
                      declared_by, source, enforce=True):
        """State a ceiling. Lowering one below current use revokes nothing and says so.

        This package cannot stop a running child, so a reduced ceiling that pretended to
        reclaim capacity would be a claim the record cannot support. It reports the overage and
        refuses the next reservation instead, which is what a supervisor can act on.
        """
        if scope_kind not in SCOPES:
            raise CoordinationError(
                RefusalReason.LINK_NOT_ACTIVE,
                "a limit scope is one of " + ", ".join(SCOPES) + ", not " + repr(scope_kind))
        self._check_scope(scope_kind, scope_key)
        self._check_declarer(scope_kind, scope_key, declared_by)
        identifier = limit_id(scope_kind, scope_key, dimension)
        now = self.clock.iso()
        with self.store.transaction() as db:
            previous = db.execute(
                "SELECT * FROM execution_limits WHERE limit_id = ?", (identifier,)
            ).fetchone()
            revision = (previous["revision"] + 1) if previous else 1
            db.execute(
                "INSERT INTO execution_limits (limit_id, scope_kind, scope_key, dimension,"
                " unit, ceiling, enforce, declared_by, source, revision, declared_at,"
                " updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)"
                " ON CONFLICT (limit_id) DO UPDATE SET unit = excluded.unit,"
                " ceiling = excluded.ceiling, enforce = excluded.enforce,"
                " declared_by = excluded.declared_by, source = excluded.source,"
                " revision = excluded.revision, updated_at = excluded.updated_at",
                (identifier, scope_kind, scope_key, dimension, unit, float(ceiling),
                 1 if enforce else 0, declared_by, source, revision, now, now),
            )
            self.store.journal(
                "execution_limit_declared", identifier,
                {"dimension": dimension, "ceiling": float(ceiling),
                 "previousCeiling": previous["ceiling"] if previous else None,
                 "revision": revision}, at=now)
        answer = [
            entry for entry in self.headroom(scope_kind, scope_key)["dimensions"]
            if entry["dimension"] == dimension
        ][0]
        answer["limitId"] = identifier
        return answer

    def observe(self, *, scope_kind, scope_key, dimension, observed, observed_by, method):
        """Record a measurement. The only way a non-runs dimension gets a current value."""
        exact(method, "a measurement method")
        self._check_scope(scope_kind, scope_key)
        self._check_declarer(scope_kind, scope_key, observed_by)
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO execution_usage (scope_kind, scope_key, dimension, observed,"
                " observed_by, method, observed_at) VALUES (?,?,?,?,?,?,?)"
                " ON CONFLICT (scope_kind, scope_key, dimension) DO UPDATE SET"
                " observed = excluded.observed, observed_by = excluded.observed_by,"
                " method = excluded.method, observed_at = excluded.observed_at",
                (scope_kind, scope_key, dimension, float(observed), observed_by, method, now),
            )
        return {"scopeKind": scope_kind, "scopeKey": scope_key, "dimension": dimension,
                "observed": float(observed), "observedBy": observed_by, "method": method,
                "observedAt": now}
