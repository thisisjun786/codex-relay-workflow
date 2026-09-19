"""Three-level execution linkage: who owns a scope, and which scopes are linked.

The two-level tables answer one question: which child is responsible for one issue, under which
parent. They cannot answer the level above it. relationships is keyed by issue and by the pair of
task ids, so a supervisor that owns an initiative has no row to be, and a project has no owner of
its own - only whichever parent happens to appear on an issue's assignment.

Nothing here is an assignment. A supervision carries no receipt, no acknowledgement, no verdict,
no generation and no artifact scope, and a peer link carries less than that. These records exist
so that a message can ask who the real counterpart is and whether the relationship it quotes is
still current; they authorize nothing on the delivery path.

Identity is derived, so re-registering the same input converges on the same record rather than
opening a second one. A link is keyed by its two SCOPES and never by a task id: keying on the
owner would mean a handover changed the identity of an unchanged relationship, and a later
re-registration would derive a different id and create a duplicate. A binding is keyed WITH its
task id, because a binding is one task's claim on one scope and replacing the owner is supposed
to produce a new binding rather than rewrite who the old one was.

Every write path validates completely before its first mutation. That ordering is what lets one
transaction commit either the conflict row alone or the whole operation, and never part of one:
at the moment a refusal is decided nothing has been written, so the contest is recorded in that
same transaction and the exception is raised after it closes.
"""

from .errors import LinkageError, RefusalReason
from .identity import sha256_hex

SUPERVISOR = "supervisor"
PARENT = "parent"
CHILD = "child"
INITIATIVE = "initiative"
PROJECT = "project"
ISSUE = "issue"
ROLE_SCOPE = {SUPERVISOR: INITIATIVE, PARENT: PROJECT, CHILD: ISSUE}

EXECUTION = "execution"
REFERENCE = "reference"
PEER = "peer"
SUPERVISION_KINDS = (EXECUTION, REFERENCE)
# The only hierarchy this registry admits, upper to lower. A peer edge is not in it, which is
# why no walk can ever lengthen a chain through one.
EXECUTION_EDGES = ((INITIATIVE, PROJECT), (PROJECT, ISSUE))

ACTIVE = "active"
PAUSED = "paused"
CANCELLED = "cancelled"
ARCHIVED = "archived"
STATUSES = (ACTIVE, PAUSED, CANCELLED, ARCHIVED)
LIVE = (ACTIVE, PAUSED)
# What AssignmentView.state() calls done. Read from there rather than re-derived, because a
# merge counts only where it matches the CURRENT head event, generation and revision.
FINISHED_STATES = ("merged", "closed", "abandoned")

CHOSEN = "chosen"
# What the absence of an owner is called at each level, so a gap names the role that is
# missing rather than repeating the scope back at the reader.
ROLE_SCOPE_OWNER = {INITIATIVE: "supervisor", PROJECT: "parent", ISSUE: "child"}
SUPERSEDED = "superseded"
DISPOSITIONS = (CHOSEN, SUPERSEDED)


def _exact(value, what):
    """A scope or task identifier is a non-empty string that cannot corrupt a derivation."""
    if not isinstance(value, str) or not value.strip():
        raise LinkageError(
            RefusalReason.UNREGISTERED_SCOPE,
            what + " must be a non-empty string, not " + repr(value),
        )
    if "|" in value:
        raise LinkageError(
            RefusalReason.UNREGISTERED_SCOPE,
            what + " must not contain '|', which is the field separator",
        )
    return value


def binding_id(role, scope_kind, scope_key, task_id):
    return "bnd-" + sha256_hex("|".join((role, scope_kind, scope_key, task_id)))[:16]


def link_id(kind, upper_kind, upper_key, lower_kind, lower_key):
    """Scope-only, and symmetric for a peer.

    A peer relation is symmetric, so two parents registering it from opposite ends have to
    converge on one record rather than on two mirror images. Sorting the scope keys before
    hashing is what makes that true regardless of which side calls first.
    """
    if kind == PEER:
        low, high = sorted((upper_key, lower_key))
        return "lnk-" + sha256_hex("|".join((PEER, PROJECT, low, PROJECT, high)))[:16]
    return "lnk-" + sha256_hex(
        "|".join((kind, upper_kind, upper_key, lower_kind, lower_key))
    )[:16]


def directive_id(scope_kind, scope_key, from_scope_key, digest):
    return "dir-" + sha256_hex(
        "|".join((scope_kind, scope_key, from_scope_key, digest))
    )[:16]


class _Refusal:
    """A decided refusal, carried from validation to the single place that records it.

    Returned rather than raised, so the caller can write the contest inside the transaction that
    decided it and raise afterwards. Raising at the decision point would roll the evidence back
    with the operation it is evidence about.
    """

    def __init__(self, reason, detail, *, scope_kind, scope_key, incumbent="", challenger=""):
        self.reason = reason
        self.detail = detail
        self.scope_kind = scope_kind
        self.scope_key = scope_key
        self.incumbent = incumbent or ""
        self.challenger = challenger or ""

    def error(self):
        return LinkageError(self.reason, self.detail)


class Linkage:
    """Scope bindings, scope links, issue attachment, directives and handover."""

    def __init__(self, store, clock):
        self.store = store
        self.clock = clock

    # ---------------------------------------------------------------- reading

    def binding(self, bid):
        row = self.store.one("SELECT * FROM scope_bindings WHERE binding_id = ?", (bid,))
        return self._binding_record(row) if row else None

    def owner(self, scope_kind, scope_key):
        """The live owner of a scope, or None. Never guesses past a deactivation."""
        row = self.store.one(
            "SELECT * FROM scope_bindings"
            "  WHERE scope_kind = ? AND scope_key = ? AND status IN ('active','paused')"
            "    AND superseded_by IS NULL"
            "  ORDER BY revision DESC LIMIT 1",
            (scope_kind, scope_key),
        )
        return self._binding_record(row) if row else None

    def link(self, lid):
        row = self.store.one("SELECT * FROM scope_links WHERE link_id = ?", (lid,))
        return self._link_record(row) if row else None

    def conflicts(self, scope_kind, scope_key):
        return [
            {
                "at": row["at"], "scopeKind": row["scope_kind"], "scopeKey": row["scope_key"],
                "reason": row["reason"], "incumbent": row["incumbent"] or None,
                "challenger": row["challenger"] or None, "detail": row["detail"],
            }
            for row in self.store.all(
                "SELECT * FROM linkage_conflicts"
                "  WHERE scope_kind = ? AND scope_key = ? ORDER BY id",
                (scope_kind, scope_key),
            )
        ]

    def directives(self, scope_kind, scope_key):
        return [self._directive_record(row) for row in self.store.all(
            "SELECT * FROM scope_directives"
            "  WHERE scope_kind = ? AND scope_key = ? ORDER BY recorded_at, directive_id",
            (scope_kind, scope_key),
        )]

    # ---------------------------------------------------------------- writing

    def bind_scope(self, *, role, scope_key, endpoint, status=ACTIVE):
        """Claim a scope for a task. Deterministic and idempotent on the same claim."""
        scope_kind = ROLE_SCOPE.get(role)
        if scope_kind is None:
            raise LinkageError(
                RefusalReason.SCOPE_ROLE_MISMATCH,
                "a role is one of " + ", ".join(sorted(ROLE_SCOPE)) + ", not " + repr(role),
            )
        if status not in STATUSES:
            raise LinkageError(RefusalReason.LINK_NOT_ACTIVE, "bad status " + repr(status))
        _exact(scope_key, "a scope key")
        _exact(endpoint.task_id, "a task id")
        _exact(endpoint.host_id, "a host id")
        bid = binding_id(role, scope_kind, scope_key, endpoint.task_id)
        existing = self.store.one("SELECT * FROM scope_bindings WHERE binding_id = ?", (bid,))
        if existing is not None:
            if existing["host_id"] != endpoint.host_id:
                raise LinkageError(
                    RefusalReason.LINK_CONFLICT,
                    bid + " is already bound on host " + repr(existing["host_id"])
                    + ", not " + repr(endpoint.host_id),
                )
            return self._binding_record(existing)
        now = self.clock.iso()
        with self.store.transaction() as db:
            refusal = self._binding_refusal(db, role, scope_kind, scope_key, endpoint)
            if refusal is None:
                self._insert_binding(db, bid, role, scope_kind, scope_key, endpoint, status,
                                     revision=1, at=now)
            else:
                self._record_conflict_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.binding(bid)

    def binding_plan(self, db, *, role, scope_key, endpoint):
        """Decide a binding WITHOUT writing it. Returns (plan, refusal).

        Split from the write deliberately. An operation that binds two scopes and then links
        them has to be able to refuse on the SECOND scope without having already written the
        first: with validation and mutation folded together, a refused supervision committed
        its supervisor binding alongside the conflict row, which is the exact failure the
        transaction protocol exists to prevent. Measured, not theorised - it leaked until this
        split, and test_a_refused_supervision_leaves_neither_binding_behind pins it.
        """
        scope_kind = ROLE_SCOPE[role]
        bid = binding_id(role, scope_kind, scope_key, endpoint.task_id)
        current = db.execute(
            "SELECT * FROM scope_bindings WHERE binding_id = ?", (bid,)
        ).fetchone()
        if current is not None:
            if current["host_id"] != endpoint.host_id:
                return None, _Refusal(
                    RefusalReason.LINK_CONFLICT,
                    bid + " is already bound on another host",
                    scope_kind=scope_kind, scope_key=scope_key,
                    incumbent=current["task_id"], challenger=endpoint.task_id,
                )
            action = "present" if current["status"] in LIVE else "reactivate"
            return (bid, action, role, scope_kind, scope_key, endpoint), None
        refusal = self._binding_refusal(db, role, scope_kind, scope_key, endpoint)
        if refusal is not None:
            return None, refusal
        return (bid, "insert", role, scope_kind, scope_key, endpoint), None

    def apply_binding_plan(self, db, plan, *, status=ACTIVE, at):
        """Perform a decision taken earlier. Every refusal is already behind us."""
        bid, action, role, scope_kind, scope_key, endpoint = plan
        if action == "insert":
            self._insert_binding(db, bid, role, scope_kind, scope_key, endpoint, status,
                                 revision=1, at=at)
        elif action == "reactivate":
            db.execute(
                "UPDATE scope_bindings SET status = ?, updated_at = ? WHERE binding_id = ?",
                (ACTIVE, at, bid),
            )
            self.store.journal("scope_rebound", bid, {"scopeKey": scope_key}, at=at)
        return bid

    def _binding_refusal(self, db, role, scope_kind, scope_key, endpoint):
        """Every competition and role read, before anything is written."""
        rival = db.execute(
            "SELECT binding_id, task_id, status FROM scope_bindings"
            "  WHERE scope_kind = ? AND scope_key = ? AND role = ?"
            "    AND status IN ('active','paused') AND superseded_by IS NULL"
            "    AND task_id != ?",
            (scope_kind, scope_key, role, endpoint.task_id),
        ).fetchone()
        if rival is not None:
            return _Refusal(
                RefusalReason.DUPLICATE_SCOPE_OWNER,
                scope_kind + " " + repr(scope_key) + " is already owned by "
                + repr(rival["task_id"]) + " under " + rival["binding_id"] + " ("
                + rival["status"] + "); hand it over deliberately instead of opening a "
                "second owner",
                scope_kind=scope_kind, scope_key=scope_key,
                incumbent=rival["task_id"], challenger=endpoint.task_id,
            )
        # One task, one role. This single rule is also what makes mutual supervision
        # unreachable: a task cannot be both a parent and somebody else's supervisor.
        confused = db.execute(
            "SELECT role, scope_kind, scope_key FROM scope_bindings"
            "  WHERE task_id = ? AND role != ? AND status IN ('active','paused')"
            "    AND superseded_by IS NULL",
            (endpoint.task_id, role),
        ).fetchone()
        if confused is not None:
            return _Refusal(
                RefusalReason.SCOPE_ROLE_MISMATCH,
                "task " + repr(endpoint.task_id) + " is already the " + confused["role"]
                + " of " + confused["scope_kind"] + " " + repr(confused["scope_key"])
                + ", so it cannot also be a " + role,
                scope_kind=scope_kind, scope_key=scope_key,
                incumbent=confused["scope_key"], challenger=endpoint.task_id,
            )
        return None

    def _insert_binding(self, db, bid, role, scope_kind, scope_key, endpoint, status,
                        *, revision, at, supersedes=None, note=None):
        db.execute(
            "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"
            " host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"
            " handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?)",
            (
                bid, role, scope_kind, scope_key, endpoint.task_id, endpoint.host_id,
                endpoint.cwd, endpoint.cxc_session, status, revision, supersedes, note, at, at,
            ),
        )
        self.store.journal(
            "scope_bound", bid,
            {"role": role, "scopeKind": scope_kind, "scopeKey": scope_key,
             "taskId": endpoint.task_id, "revision": revision},
            at=at,
        )

    def _record_conflict_in(self, db, refusal, *, at):
        """Retain a contest. Written in the transaction that decided it, which has no mutations.

        ON CONFLICT DO UPDATE rather than a plain insert, so a losing caller that retries
        converges on one row instead of accumulating one per attempt.
        """
        db.execute(
            "INSERT INTO linkage_conflicts (at, scope_kind, scope_key, reason, incumbent,"
            " challenger, detail) VALUES (?,?,?,?,?,?,?)"
            " ON CONFLICT(scope_kind, scope_key, reason, incumbent, challenger)"
            "   DO UPDATE SET at = excluded.at, detail = excluded.detail",
            (
                at, refusal.scope_kind, refusal.scope_key, refusal.reason.value,
                refusal.incumbent, refusal.challenger, refusal.detail,
            ),
        )
        self.store.journal(
            "linkage_refused", refusal.scope_key,
            {"reason": refusal.reason.value, "scopeKind": refusal.scope_kind,
             "incumbent": refusal.incumbent, "challenger": refusal.challenger},
            at=at,
        )

    # ------------------------------------------------------------ supervision

    def register_supervision(self, *, initiative_key, project_key, supervisor, parent,
                             link_kind=EXECUTION):
        """An initiative supervisor over a project parent. Never an assignment.

        Deterministic and idempotent: the same two scopes and the same kind derive the same
        link, so an uncertain response can simply be repeated.
        """
        if link_kind not in SUPERVISION_KINDS:
            raise LinkageError(
                RefusalReason.SCOPE_ROLE_MISMATCH,
                "a supervision is " + " or ".join(SUPERVISION_KINDS) + ", not "
                + repr(link_kind) + "; a peer link is registered with register_peer",
            )
        _exact(initiative_key, "an initiative key")
        _exact(project_key, "a project key")
        lid = link_id(link_kind, INITIATIVE, initiative_key, PROJECT, project_key)
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            replay = db.execute(
                "SELECT * FROM scope_links WHERE link_id = ?", (lid,)
            ).fetchone()
            if replay is not None and replay["lower_task_id"] == parent.task_id \
                    and replay["upper_task_id"] == supervisor.task_id:
                # Runs BEFORE the ownership check below, which would otherwise refuse a link
                # being re-registered against itself and turn convergence into a refusal.
                return self._link_record(replay)
            refusal = self._supervision_refusal(
                db, lid, initiative_key, project_key, supervisor, parent, link_kind, replay,
            )
            supervisor_plan = parent_plan = None
            if refusal is None:
                supervisor_plan, refusal = self.binding_plan(
                    db, role=SUPERVISOR, scope_key=initiative_key, endpoint=supervisor)
            if refusal is None:
                parent_plan, refusal = self.binding_plan(
                    db, role=PARENT, scope_key=project_key, endpoint=parent)
            if refusal is None:
                # Both decisions are made; only now does anything get written.
                self.apply_binding_plan(db, supervisor_plan, at=now)
                self.apply_binding_plan(db, parent_plan, at=now)
                self._insert_link(
                    db, lid, link_kind, (INITIATIVE, initiative_key, supervisor.task_id),
                    (PROJECT, project_key, parent.task_id), at=now)
            else:
                self._record_conflict_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.link(lid)

    def _supervision_refusal(self, db, lid, initiative_key, project_key, supervisor, parent,
                             link_kind, replay):
        if supervisor.task_id == parent.task_id:
            return _Refusal(
                RefusalReason.SCOPE_CYCLE,
                "task " + repr(supervisor.task_id) + " cannot supervise itself",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=supervisor.task_id, challenger=parent.task_id,
            )
        if initiative_key == project_key:
            return _Refusal(
                RefusalReason.SCOPE_CYCLE,
                "an initiative and a project cannot be the same scope " + repr(project_key),
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=initiative_key, challenger=project_key,
            )
        if self._reaches(db, PROJECT, project_key, supervisor.task_id):
            return _Refusal(
                RefusalReason.SCOPE_CYCLE,
                "task " + repr(supervisor.task_id) + " already owns a scope below project "
                + repr(project_key) + ", so supervising it would close a loop",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=project_key, challenger=supervisor.task_id,
            )
        if replay is not None:
            return _Refusal(
                RefusalReason.LINK_CONFLICT,
                lid + " already joins these scopes with different endpoints: "
                + repr(replay["upper_task_id"]) + " over " + repr(replay["lower_task_id"]),
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=replay["lower_task_id"], challenger=parent.task_id,
            )
        # At most ONE execution edge per project, whichever initiative or parent it names. A
        # rule that refused only a DIFFERENT parent let a second initiative naming the same
        # parent derive another link id and take a second execution edge.
        owning = db.execute(
            "SELECT link_id, upper_key, lower_task_id FROM scope_links"
            "  WHERE lower_kind = ? AND lower_key = ? AND link_kind = 'execution'"
            "    AND status IN ('active','paused') AND superseded_by IS NULL",
            (PROJECT, project_key),
        ).fetchone()
        if link_kind == EXECUTION and owning is not None:
            return _Refusal(
                RefusalReason.DUPLICATE_SCOPE_OWNER,
                "project " + repr(project_key) + " already has its execution supervisor under "
                + owning["link_id"] + " from initiative " + repr(owning["upper_key"])
                + "; another initiative references it instead of supervising it again",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=owning["upper_key"], challenger=initiative_key,
            )
        if link_kind == REFERENCE and owning is not None \
                and owning["lower_task_id"] != parent.task_id:
            return _Refusal(
                RefusalReason.DUPLICATE_SCOPE_OWNER,
                "project " + repr(project_key) + " is executed by parent "
                + repr(owning["lower_task_id"]) + ", so a reference naming "
                + repr(parent.task_id) + " would clone its execution parent",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=owning["lower_task_id"], challenger=parent.task_id,
            )
        return None

    def _reaches(self, db, scope_kind, scope_key, task_id):
        """Does a live EXECUTION chain from this scope reach a scope that task owns?

        Bounded by the number of live links, so a corrupted store cannot spin here. Only
        execution edges are walked, which is why a peer or a reference row can never make a
        cycle out of two ordinary relationships.
        """
        frontier = [(scope_kind, scope_key)]
        seen = set()
        budget = db.execute(
            "SELECT count(*) AS n FROM scope_links WHERE link_kind = 'execution'"
        ).fetchone()["n"] + 1
        while frontier and budget > 0:
            budget -= 1
            kind, key = frontier.pop()
            if (kind, key) in seen:
                continue
            seen.add((kind, key))
            held = db.execute(
                "SELECT 1 FROM scope_bindings"
                "  WHERE scope_kind = ? AND scope_key = ? AND task_id = ?"
                "    AND status IN ('active','paused') AND superseded_by IS NULL",
                (kind, key, task_id),
            ).fetchone()
            if held is not None:
                return "reaches"
            for row in db.execute(
                "SELECT lower_kind, lower_key FROM scope_links"
                "  WHERE upper_kind = ? AND upper_key = ? AND link_kind = 'execution'"
                "    AND status IN ('active','paused') AND superseded_by IS NULL",
                (kind, key),
            ).fetchall():
                frontier.append((row["lower_kind"], row["lower_key"]))
        return ""

    def _insert_link(self, db, lid, kind, upper, lower, *, at, revision=1):
        db.execute(
            "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"
            " upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"
            " superseded_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,NULL,?,?)",
            (lid, kind, upper[0], upper[1], upper[2], lower[0], lower[1], lower[2],
             ACTIVE, revision, at, at),
        )
        self.store.journal(
            "scope_linked", lid,
            {"kind": kind, "upper": upper[1], "lower": lower[1], "revision": revision},
            at=at,
        )

    # ------------------------------------------------------------- attachment

    def attach_issue(self, relationship_id, project_key):
        """Bind an existing assignment's issue to its project."""
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            row = db.execute(
                "SELECT * FROM relationships WHERE relationship_id = ?", (relationship_id,)
            ).fetchone()
            if row is None:
                raise LinkageError(
                    RefusalReason.UNREGISTERED_RELATIONSHIP,
                    "no relationship " + repr(relationship_id),
                )
            refusal = self.attach_in(db, row, project_key, at=now)
            if refusal is not None:
                self._record_conflict_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.attachment(relationship_id)

    def attach_in(self, db, relationship_row, project_key, *, at=None):
        """The ONE path that writes the lower level. Returns a refusal or None.

        Idempotence is computed over all THREE facts - the relationship_scope row, the child
        binding and the project to issue edge - rather than over the scope row alone. A
        relationship attached by an older writer, or by a run that stopped between two of them,
        is COMPLETED here instead of being reported as already done. There is no second,
        thinner path that writes only some of them.
        """
        at = at or self.clock.iso()
        rid = relationship_row["relationship_id"]
        issue_key = relationship_row["issue_key"]
        parent_task = relationship_row["parent_task_id"]
        child_task = relationship_row["child_task_id"]
        _exact(project_key, "a project key")
        if relationship_row["status"] != ACTIVE or relationship_row["superseded_by"]:
            return _Refusal(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                "relationship " + repr(rid) + " is " + repr(relationship_row["status"])
                + ", so its issue cannot be attached to a project",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=rid, challenger=project_key,
            )
        if parent_task == child_task:
            return _Refusal(
                RefusalReason.SCOPE_CYCLE,
                "relationship " + repr(rid) + " has the same task as parent and child, which "
                "is a self-link rather than a level",
                scope_kind=ISSUE, scope_key=issue_key,
                incumbent=parent_task, challenger=child_task,
            )
        holder = db.execute(
            "SELECT task_id FROM scope_bindings"
            "  WHERE scope_kind = ? AND scope_key = ? AND role = ?"
            "    AND status IN ('active','paused') AND superseded_by IS NULL",
            (PROJECT, project_key, PARENT),
        ).fetchone()
        if holder is None:
            return _Refusal(
                RefusalReason.UNREGISTERED_SCOPE,
                "project " + repr(project_key) + " has no registered parent, so an issue "
                "cannot be attached to it yet",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent="", challenger=parent_task,
            )
        if holder["task_id"] != parent_task:
            return _Refusal(
                RefusalReason.FOREIGN_SCOPE,
                "issue " + repr(issue_key) + " is assigned under parent " + repr(parent_task)
                + ", but project " + repr(project_key) + " is executed by "
                + repr(holder["task_id"]) + "; an issue belongs to its own project",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=holder["task_id"], challenger=parent_task,
            )
        recorded = db.execute(
            "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", (rid,)
        ).fetchone()
        if recorded is not None and recorded["project_key"] != project_key:
            return _Refusal(
                RefusalReason.FOREIGN_SCOPE,
                "issue " + repr(issue_key) + " is already scoped to project "
                + repr(recorded["project_key"]) + ", not " + repr(project_key),
                scope_kind=ISSUE, scope_key=issue_key,
                incumbent=recorded["project_key"], challenger=project_key,
            )
        from .models import Endpoint

        plan, refusal = self.binding_plan(
            db, role=CHILD, scope_key=issue_key,
            endpoint=Endpoint(child_task, relationship_row["child_host_id"],
                              cwd=relationship_row["child_cwd"],
                              cxc_session=relationship_row["child_cxc_session"]),
        )
        if refusal is not None:
            return refusal
        self.apply_binding_plan(db, plan, at=at)
        if recorded is None:
            db.execute(
                "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at)"
                " VALUES (?,?,?) ON CONFLICT(relationship_id) DO NOTHING",
                (rid, project_key, at),
            )
        lid = link_id(EXECUTION, PROJECT, project_key, ISSUE, issue_key)
        edge = db.execute("SELECT * FROM scope_links WHERE link_id = ?", (lid,)).fetchone()
        if edge is None:
            self._insert_link(db, lid, EXECUTION, (PROJECT, project_key, parent_task),
                              (ISSUE, issue_key, child_task), at=at)
        elif edge["status"] not in LIVE or edge["lower_task_id"] != child_task:
            # Reactivate AND repoint. The edge's identity is scope-only, so after a replacement
            # archived the old one the row still exists: writing only absent facts would leave
            # the new child with an archived edge pointing at its predecessor forever.
            db.execute(
                "UPDATE scope_links SET status = ?, lower_task_id = ?, upper_task_id = ?,"
                " revision = revision + 1, superseded_by = NULL, updated_at = ?"
                "  WHERE link_id = ?",
                (ACTIVE, child_task, parent_task, at, lid),
            )
            self.store.journal(
                "scope_link_repointed", lid, {"lowerTaskId": child_task}, at=at)
        return None

    def attachment(self, relationship_id):
        """What the lower level says about one assignment, or None when it is unscoped."""
        row = self.store.one(
            "SELECT project_key FROM relationship_scope WHERE relationship_id = ?",
            (relationship_id,),
        )
        if row is None:
            return None
        relationship = self.store.one(
            "SELECT issue_key FROM relationships WHERE relationship_id = ?", (relationship_id,)
        )
        issue_key = relationship["issue_key"] if relationship else None
        return {
            "relationshipId": relationship_id,
            "projectKey": row["project_key"],
            "issueKey": issue_key,
            "child": self.owner(ISSUE, issue_key) if issue_key else None,
            "parent": self.owner(PROJECT, row["project_key"]),
            "link": self.link(link_id(EXECUTION, PROJECT, row["project_key"], ISSUE, issue_key))
            if issue_key else None,
        }

    def apply_relationship_status_in(self, db, relationship_id, status):
        """Move an assignment's lower level with the assignment itself.

        Called from registry._write_status, from resume's write transaction and from
        supersede's - not from set_status, which delegates and owns no transaction. A
        relationship and its lower level therefore move together or not at all.

        An explicit NO-OP when the relationship has no relationship_scope row. That is what
        keeps every relationship registered without a project exactly as it was, which is every
        relationship the existing suite creates.
        """
        scoped = db.execute(
            "SELECT project_key FROM relationship_scope WHERE relationship_id = ?",
            (relationship_id,),
        ).fetchone()
        if scoped is None:
            return "unscoped"
        row = db.execute(
            "SELECT issue_key, child_task_id FROM relationships WHERE relationship_id = ?",
            (relationship_id,),
        ).fetchone()
        if row is None:
            return "unscoped"
        at = self.clock.iso()
        lower = ACTIVE if status == ACTIVE else ARCHIVED
        db.execute(
            "UPDATE scope_bindings SET status = ?, updated_at = ?"
            "  WHERE scope_kind = ? AND scope_key = ? AND role = ? AND task_id = ?",
            (lower, at, ISSUE, row["issue_key"], CHILD, row["child_task_id"]),
        )
        db.execute(
            "UPDATE scope_links SET status = ?, updated_at = ? WHERE link_id = ?",
            (lower, at, link_id(EXECUTION, PROJECT, scoped["project_key"], ISSUE,
                                row["issue_key"])),
        )
        self.store.journal(
            "scope_lifecycle", relationship_id,
            {"status": status, "lower": lower, "issueKey": row["issue_key"]}, at=at,
        )
        return lower

    # ------------------------------------------------------------- directives

    def record_directive(self, *, scope_kind, scope_key, from_task_id, from_scope_key,
                         link_id_value, digest, reference=None):
        """An instruction that reached a scope, by digest and origin.

        The caller names the EXACT link, because link_kind is part of link identity and an
        execution link and a reference link can both join one scope pair, so "a live link joins
        these scopes" does not choose one. The kind is copied onto the row, which is what lets a
        reader tell an instruction from the execution supervisor apart from one from a merely
        referencing initiative.
        """
        _exact(digest, "a directive digest")
        did = directive_id(scope_kind, scope_key, from_scope_key, digest)
        existing = self.store.one(
            "SELECT * FROM scope_directives WHERE directive_id = ?", (did,))
        if existing is not None:
            return self._directive_record(existing)
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            holder = db.execute(
                "SELECT task_id FROM scope_bindings"
                "  WHERE scope_key = ? AND task_id = ? AND status IN ('active','paused')"
                "    AND superseded_by IS NULL",
                (from_scope_key, from_task_id),
            ).fetchone()
            edge = db.execute(
                "SELECT * FROM scope_links WHERE link_id = ?", (link_id_value,)
            ).fetchone()
            if holder is None:
                refusal = _Refusal(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "task " + repr(from_task_id) + " does not own scope "
                    + repr(from_scope_key) + ", so it cannot instruct from it",
                    scope_kind=scope_kind, scope_key=scope_key,
                    incumbent=from_scope_key, challenger=from_task_id,
                )
            elif edge is None or edge["status"] not in LIVE or edge["superseded_by"]:
                refusal = _Refusal(
                    RefusalReason.UNREGISTERED_SCOPE,
                    "link " + repr(link_id_value) + " is not a live link",
                    scope_kind=scope_kind, scope_key=scope_key,
                    incumbent=str(link_id_value), challenger=from_task_id,
                )
            elif edge["link_kind"] not in SUPERVISION_KINDS \
                    or edge["upper_key"] != from_scope_key \
                    or edge["lower_key"] != scope_key:
                refusal = _Refusal(
                    RefusalReason.UNREGISTERED_SCOPE,
                    "link " + repr(link_id_value) + " does not join " + repr(from_scope_key)
                    + " down to " + repr(scope_key) + " by execution or reference",
                    scope_kind=scope_kind, scope_key=scope_key,
                    incumbent=str(link_id_value), challenger=from_scope_key,
                )
            if refusal is None:
                db.execute(
                    "INSERT INTO scope_directives (directive_id, scope_kind, scope_key,"
                    " from_task_id, from_scope_key, link_id, link_kind, digest, reference,"
                    " revision, disposition, decided_by, decided_at, recorded_at)"
                    " VALUES (?,?,?,?,?,?,?,?,?,?,NULL,NULL,NULL,?)",
                    (did, scope_kind, scope_key, from_task_id, from_scope_key, link_id_value,
                     edge["link_kind"], digest, reference, edge["revision"], now),
                )
                self.store.journal(
                    "directive_recorded", did,
                    {"scopeKey": scope_key, "fromScopeKey": from_scope_key,
                     "linkKind": edge["link_kind"]}, at=now,
                )
            else:
                self._record_conflict_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.store.one(
            "SELECT * FROM scope_directives WHERE directive_id = ?", (did,)
        ) and self._directive_record(
            self.store.one("SELECT * FROM scope_directives WHERE directive_id = ?", (did,)))

    def settle_directive(self, directive_id_value, disposition, *, decided_by, reason=None):
        """Settle one directive without rewriting it, so the instruction that lost stays read."""
        if disposition not in DISPOSITIONS:
            raise LinkageError(
                RefusalReason.LINK_NOT_ACTIVE,
                "a disposition is " + " or ".join(DISPOSITIONS) + ", not " + repr(disposition),
            )
        row = self.store.one(
            "SELECT * FROM scope_directives WHERE directive_id = ?", (directive_id_value,))
        if row is None:
            raise LinkageError(
                RefusalReason.UNREGISTERED_SCOPE, "no directive " + repr(directive_id_value))
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "UPDATE scope_directives SET disposition = ?, decided_by = ?, decided_at = ?"
                "  WHERE directive_id = ?",
                (disposition, decided_by, now, directive_id_value),
            )
            self.store.journal(
                "directive_settled", directive_id_value,
                {"disposition": disposition, "decidedBy": decided_by, "reason": reason},
                at=now,
            )
        return self._directive_record(self.store.one(
            "SELECT * FROM scope_directives WHERE directive_id = ?", (directive_id_value,)))

    def contested_directives(self, scope_kind, scope_key):
        """Undisposed directives of differing digests from different origins.

        Two supervisors can agree about who the parent is and still instruct it differently, so
        this is decided on the digest rather than on ownership.
        """
        open_ones = [d for d in self.directives(scope_kind, scope_key)
                     if d["disposition"] is None]
        digests = {d["digest"] for d in open_ones}
        origins = {d["fromScopeKey"] for d in open_ones}
        if len(digests) > 1 and len(origins) > 1:
            return open_ones
        return []

    # ------------------------------------------------------------------- peer

    def register_peer(self, *, left_project, left_parent, right_project, right_parent):
        """Two project parents collaborating. Symmetric, and not hierarchy.

        A peer relation confers nothing: no receipt, no acknowledgement, no verdict, no
        instruction. It is recorded so a message can ask who the real counterpart is and
        whether the relationship it quotes is current. Every walk filters
        link_kind = 'execution', so this row can neither lengthen a chain nor produce a second
        execution owner.
        """
        _exact(left_project, "a project key")
        _exact(right_project, "a project key")
        if left_project == right_project:
            raise LinkageError(
                RefusalReason.SCOPE_CYCLE,
                "project " + repr(left_project) + " is not its own peer",
            )
        lid = link_id(PEER, PROJECT, left_project, PROJECT, right_project)
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            sides = sorted(
                ((left_project, left_parent), (right_project, right_parent)),
                key=lambda side: side[0],
            )
            replay = db.execute(
                "SELECT * FROM scope_links WHERE link_id = ?", (lid,)
            ).fetchone()
            for scope_key, endpoint in sides:
                holder = db.execute(
                    "SELECT task_id FROM scope_bindings"
                    "  WHERE scope_kind = ? AND scope_key = ? AND role = ?"
                    "    AND status IN ('active','paused') AND superseded_by IS NULL",
                    (PROJECT, scope_key, PARENT),
                ).fetchone()
                if holder is None or holder["task_id"] != endpoint.task_id:
                    refusal = _Refusal(
                        RefusalReason.SCOPE_ROLE_MISMATCH,
                        "task " + repr(endpoint.task_id) + " is not the registered parent of "
                        "project " + repr(scope_key)
                        + (", which is held by " + repr(holder["task_id"]) if holder
                           else ", which has no registered parent")
                        + "; a peer link joins two project parents",
                        scope_kind=PROJECT, scope_key=scope_key,
                        incumbent=holder["task_id"] if holder else "",
                        challenger=endpoint.task_id,
                    )
                    break
            if refusal is None and replay is not None:
                return self._link_record(replay)
            if refusal is None:
                self._insert_link(
                    db, lid, PEER,
                    (PROJECT, sides[0][0], sides[0][1].task_id),
                    (PROJECT, sides[1][0], sides[1][1].task_id), at=now)
            else:
                self._record_conflict_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.link(lid)

    def counterpart(self, from_task, to_task, *, quoted_revision=None, quoted_scope=None):
        """What a message can establish about whom it is addressing.

        Returns a record and never raises for an absence, and never reports a failure to READ
        as an absence: an empty answer with readable True means the store said there is nothing,
        and one with readable False means the store did not answer. This is the shape
        intent.dispatch_generation_state uses, which separates stale from absent for the same
        reason.
        """
        import sqlite3

        blank = {
            "state": "unreadable", "readable": False, "link": None,
            "from": None, "counterpart": None, "currentOwner": None, "findings": [],
        }
        try:
            sender = self._any_binding(from_task)
            recipient = self._any_binding(to_task)
            findings = []
            current = None
            if recipient is not None and (recipient["status"] not in LIVE
                                          or recipient["supersededBy"]):
                findings.append("stale_owner")
                current = self.owner(recipient["scopeKind"], recipient["scopeKey"])
            if sender is None or recipient is None:
                if recipient is None:
                    findings.append("unregistered_link")
                return {
                    "state": "unlinked", "readable": True, "link": None,
                    "from": sender, "counterpart": recipient, "currentOwner": current,
                    "findings": sorted(set(findings)),
                }
            if quoted_scope is not None and quoted_scope != recipient["scopeKey"]:
                findings.append("foreign_scope")
            edge = self._joining_link(sender, recipient)
            if edge is None:
                findings.append("unregistered_link")
                if self._role_pair_is_wrong(sender, recipient):
                    findings.append("wrong_role")
                return {
                    "state": "unlinked", "readable": True, "link": None,
                    "from": sender, "counterpart": recipient, "currentOwner": current,
                    "findings": sorted(set(findings)),
                }
            if self._role_pair_is_wrong(sender, recipient):
                findings.append("wrong_role")
            if quoted_revision is not None and quoted_revision < edge["revision"]:
                findings.append("stale_revision")
            for side in ("upper", "lower"):
                live = self.owner(edge[side]["scopeKind"], edge[side]["scopeKey"])
                if live is not None and live["taskId"] != edge[side]["taskId"]:
                    findings.append("owner_drift")
            if self.contested_directives(recipient["scopeKind"], recipient["scopeKey"]):
                findings.append("instruction_conflict")
            return {
                "state": "linked", "readable": True,
                "link": {"linkId": edge["linkId"], "kind": edge["kind"],
                         "revision": edge["revision"], "status": edge["status"]},
                "from": sender, "counterpart": recipient, "currentOwner": current,
                "findings": sorted(set(findings)),
            }
        except sqlite3.Error:
            return blank

    def _any_binding(self, task_id):
        """The most recent binding for a task, live or not.

        Deliberately not filtered to live rows: a message naming a replaced owner has to be
        told that it did, and the only way to say so is to find the archived binding.
        """
        row = self.store.one(
            "SELECT * FROM scope_bindings WHERE task_id = ?"
            "  ORDER BY CASE WHEN status IN ('active','paused') THEN 0 ELSE 1 END,"
            "           revision DESC LIMIT 1",
            (task_id,),
        )
        return self._binding_record(row) if row else None

    def _joining_link(self, sender, recipient):
        row = self.store.one(
            "SELECT * FROM scope_links"
            "  WHERE status IN ('active','paused') AND superseded_by IS NULL"
            "    AND ((upper_key = ? AND lower_key = ?) OR (upper_key = ? AND lower_key = ?))"
            "  ORDER BY revision DESC LIMIT 1",
            (sender["scopeKey"], recipient["scopeKey"],
             recipient["scopeKey"], sender["scopeKey"]),
        )
        return self._link_record(row) if row else None

    @staticmethod
    def _role_pair_is_wrong(sender, recipient):
        """Which role pairs may address each other at all.

        A supervisor instructs the parents of its projects and a parent instructs its own
        children, so a supervisor addressing a child directly skips the owner that is supposed
        to decide. Two parents may address each other, and that is what a peer link is for.
        """
        pair = (sender["role"], recipient["role"])
        return pair not in (
            (SUPERVISOR, PARENT), (PARENT, SUPERVISOR),
            (PARENT, CHILD), (CHILD, PARENT),
            (PARENT, PARENT),
        )

    # ---------------------------------------------------------------- queries

    def down(self, scope_kind, scope_key):
        """Initiative to projects to issues, with what is missing and what is contended.

        A level is never omitted for being incomplete: a project with no parent is a gap that
        says so. An empty answer with readable True means the store said there is nothing; an
        empty answer with readable False means the store did not answer. Neither is completion.
        """
        import sqlite3

        try:
            levels, gaps, contention = [], [], []
            self._descend(scope_kind, scope_key, levels, gaps, contention, depth=0)
            if not levels:
                return {"state": "unregistered", "readable": True,
                        "levels": [], "gaps": gaps, "contention": contention}
            return {"state": "resolved", "readable": True,
                    "levels": levels, "gaps": gaps, "contention": contention}
        except sqlite3.Error:
            return {"state": "unreadable", "readable": False,
                    "levels": [], "gaps": [], "contention": []}

    def _descend(self, scope_kind, scope_key, levels, gaps, contention, *, depth):
        owner = self.owner(scope_kind, scope_key)
        levels.append({"scopeKind": scope_kind, "scopeKey": scope_key, "owner": owner,
                       "depth": depth})
        if owner is None:
            gaps.append({"gap": scope_kind + "_without_" + ROLE_SCOPE_OWNER[scope_kind],
                         "scopeKind": scope_kind, "scopeKey": scope_key})
        contention.extend(self.conflicts(scope_kind, scope_key))
        for directive in self.contested_directives(scope_kind, scope_key):
            contention.append({"contention": "instruction_conflict",
                               "scopeKind": scope_kind, "scopeKey": scope_key,
                               "directiveId": directive["directiveId"],
                               "fromScopeKey": directive["fromScopeKey"],
                               "digest": directive["digest"]})
        for row in self.store.all(
            "SELECT * FROM scope_links"
            "  WHERE upper_kind = ? AND upper_key = ? AND link_kind = 'execution'"
            "    AND status IN ('active','paused') AND superseded_by IS NULL"
            "  ORDER BY lower_key",
            (scope_kind, scope_key),
        ):
            edge = self._link_record(row)
            live = self.owner(edge["lower"]["scopeKind"], edge["lower"]["scopeKey"])
            if live is not None and live["taskId"] != edge["lower"]["taskId"]:
                contention.append({"contention": "owner_drift", "linkId": edge["linkId"],
                                   "recorded": edge["lower"]["taskId"],
                                   "live": live["taskId"]})
            self._descend(edge["lower"]["scopeKind"], edge["lower"]["scopeKey"],
                          levels, gaps, contention, depth=depth + 1)

    def up(self, *, task_id=None, issue_key=None, relationship_id=None):
        """Child to parent to supervisor, reporting a missing upper level as a gap."""
        import sqlite3

        try:
            start = self._starting_scope(task_id, issue_key, relationship_id)
            if start is None:
                return {"state": "unregistered", "readable": True, "levels": [],
                        "gaps": [{"gap": "unscoped_assignment", "relationshipId":
                                  relationship_id, "issueKey": issue_key,
                                  "taskId": task_id}],
                        "contention": []}
            levels, gaps, contention = [], [], []
            scope_kind, scope_key = start
            seen = set()
            while (scope_kind, scope_key) not in seen:
                seen.add((scope_kind, scope_key))
                owner = self.owner(scope_kind, scope_key)
                levels.append({"scopeKind": scope_kind, "scopeKey": scope_key,
                               "owner": owner, "depth": len(levels)})
                if owner is None:
                    gaps.append({"gap": scope_kind + "_without_"
                                 + ROLE_SCOPE_OWNER[scope_kind],
                                 "scopeKind": scope_kind, "scopeKey": scope_key})
                contention.extend(self.conflicts(scope_kind, scope_key))
                row = self.store.one(
                    "SELECT * FROM scope_links"
                    "  WHERE lower_kind = ? AND lower_key = ? AND link_kind = 'execution'"
                    "    AND status IN ('active','paused') AND superseded_by IS NULL"
                    "  ORDER BY revision DESC LIMIT 1",
                    (scope_kind, scope_key),
                )
                if row is None:
                    if scope_kind == PROJECT:
                        gaps.append({"gap": "no_supervisor", "scopeKind": PROJECT,
                                     "scopeKey": scope_key})
                    break
                scope_kind, scope_key = row["upper_kind"], row["upper_key"]
            return {"state": "resolved", "readable": True, "levels": levels,
                    "gaps": gaps, "contention": contention}
        except sqlite3.Error:
            return {"state": "unreadable", "readable": False, "levels": [], "gaps": [],
                    "contention": []}

    def _starting_scope(self, task_id, issue_key, relationship_id):
        if relationship_id is not None:
            row = self.store.one(
                "SELECT issue_key FROM relationships WHERE relationship_id = ?",
                (relationship_id,))
            if row is None:
                return None
            scoped = self.store.one(
                "SELECT project_key FROM relationship_scope WHERE relationship_id = ?",
                (relationship_id,))
            if scoped is None:
                return None
            return (ISSUE, row["issue_key"])
        if issue_key is not None:
            if self.owner(ISSUE, issue_key) is None:
                return None
            return (ISSUE, issue_key)
        if task_id is not None:
            binding = self.owner_of_task(task_id)
            if binding is None:
                return None
            return (binding["scopeKind"], binding["scopeKey"])
        return None

    def owner_of_task(self, task_id):
        row = self.store.one(
            "SELECT * FROM scope_bindings WHERE task_id = ? AND status IN ('active','paused')"
            "  AND superseded_by IS NULL ORDER BY revision DESC LIMIT 1",
            (task_id,),
        )
        return self._binding_record(row) if row else None

    # ---------------------------------------------------------------- handover

    def outstanding(self, project_key, task_id):
        """Live assignments under this parent in this project that are not finished.

        Derived through AssignmentView.state() rather than by asking whether a merge mark
        exists, because a mark counts only where it matches the CURRENT head event, generation
        and revision. A mark left over from an earlier generation would otherwise hide work that
        is still unfinished, which is exactly what a handover must not do.
        """
        from .assignment import AssignmentView
        from .registry import Registry

        view = AssignmentView(self.store, Registry(self.store, self.clock), self.clock)
        unfinished_ids = []
        for row in self.store.all(
            "SELECT r.relationship_id AS rid FROM relationships r"
            "  JOIN relationship_scope s ON s.relationship_id = r.relationship_id"
            " WHERE s.project_key = ? AND r.parent_task_id = ?"
            "   AND r.status IN ('active','paused') AND r.superseded_by IS NULL"
            " ORDER BY r.created_at",
            (project_key, task_id),
        ):
            if view.state(row["rid"])["state"] not in FINISHED_STATES:
                unfinished_ids.append(row["rid"])
        return unfinished_ids

    def handover(self, *, role, scope_key, expect_task_id, endpoint, acknowledged, evidence,
                 actor):
        """Replace a scope's owner, only from a caller that has read what it is taking on.

        The caller restates both the outgoing owner AND the outstanding work, which is
        registry.resume's discipline: it has to say what it believes it is taking over, and be
        right. A caller that has not read the outstanding work cannot produce the list, which is
        the point - an owner is never taken over silently.
        """
        scope_kind = ROLE_SCOPE.get(role)
        if scope_kind is None:
            raise LinkageError(
                RefusalReason.SCOPE_ROLE_MISMATCH, "unknown role " + repr(role))
        if not str(evidence or "").strip():
            raise LinkageError(
                RefusalReason.HANDOVER_UNCONFIRMED, "a handover carries its evidence")
        current = self.owner(scope_kind, scope_key)
        if current is None:
            raise LinkageError(
                RefusalReason.UNREGISTERED_SCOPE,
                scope_kind + " " + repr(scope_key) + " has no live owner to replace")
        if current["taskId"] != expect_task_id:
            raise LinkageError(
                RefusalReason.HANDOVER_UNCONFIRMED,
                "this handover expects " + repr(expect_task_id) + " to hold " + repr(scope_key)
                + ", but it is held by " + repr(current["taskId"])
                + "; re-read the scope before replacing its owner")
        unfinished = self.outstanding(scope_key, expect_task_id) if scope_kind == PROJECT else []
        claimed = sorted(set(acknowledged or ()))
        if claimed != sorted(set(unfinished)):
            raise LinkageError(
                RefusalReason.HANDOVER_UNCONFIRMED,
                "the outstanding work restated by this handover is " + repr(claimed)
                + " but the store says it is " + repr(sorted(set(unfinished)))
                + "; a replacement owner confirms the unfinished work it is taking on",
            )
        now = self.clock.iso()
        new_id = binding_id(role, scope_kind, scope_key, endpoint.task_id)
        with self.store.transaction() as db:
            db.execute(
                "UPDATE scope_bindings SET status = ?, superseded_by = ?, updated_at = ?"
                "  WHERE binding_id = ?",
                (ARCHIVED, new_id, now, current["bindingId"]),
            )
            self._insert_binding(
                db, new_id, role, scope_kind, scope_key, endpoint, ACTIVE,
                revision=current["revision"] + 1, at=now,
                supersedes=current["bindingId"], note=evidence,
            )
            db.execute(
                "UPDATE scope_links SET lower_task_id = ?, revision = revision + 1,"
                " updated_at = ? WHERE lower_kind = ? AND lower_key = ?"
                "   AND lower_task_id = ? AND status IN ('active','paused')",
                (endpoint.task_id, now, scope_kind, scope_key, expect_task_id),
            )
            db.execute(
                "UPDATE scope_links SET upper_task_id = ?, revision = revision + 1,"
                " updated_at = ? WHERE upper_kind = ? AND upper_key = ?"
                "   AND upper_task_id = ? AND status IN ('active','paused')",
                (endpoint.task_id, now, scope_kind, scope_key, expect_task_id),
            )
            self.store.journal(
                "scope_handover", new_id,
                {"scopeKey": scope_key, "from": expect_task_id, "to": endpoint.task_id,
                 "actor": actor, "acknowledged": claimed},
                at=now,
            )
        return self.binding(new_id)

    # ---------------------------------------------------------------- records

    @staticmethod
    def _binding_record(row):
        return {
            "bindingId": row["binding_id"],
            "role": row["role"],
            "scopeKind": row["scope_kind"],
            "scopeKey": row["scope_key"],
            "taskId": row["task_id"],
            "hostId": row["host_id"],
            "cwd": row["cwd"],
            "status": row["status"],
            "revision": row["revision"],
            "supersedes": row["supersedes"],
            "supersededBy": row["superseded_by"],
            "handoverNote": row["handover_note"],
            "createdAt": row["created_at"],
            "updatedAt": row["updated_at"],
            # Harness bindings are carried for audit and never written back to any harness,
            # exactly as registry._row_to_record already treats them.
            "_bindings": {"cxcSession": row["cxc_session"]},
        }

    @staticmethod
    def _link_record(row):
        return {
            "linkId": row["link_id"],
            "kind": row["link_kind"],
            "upper": {"scopeKind": row["upper_kind"], "scopeKey": row["upper_key"],
                      "taskId": row["upper_task_id"]},
            "lower": {"scopeKind": row["lower_kind"], "scopeKey": row["lower_key"],
                      "taskId": row["lower_task_id"]},
            "status": row["status"],
            "revision": row["revision"],
            "supersededBy": row["superseded_by"],
            "createdAt": row["created_at"],
            "updatedAt": row["updated_at"],
        }

    @staticmethod
    def _directive_record(row):
        return {
            "directiveId": row["directive_id"],
            "scopeKind": row["scope_kind"],
            "scopeKey": row["scope_key"],
            "fromTaskId": row["from_task_id"],
            "fromScopeKey": row["from_scope_key"],
            "linkId": row["link_id"],
            "linkKind": row["link_kind"],
            "digest": row["digest"],
            "reference": row["reference"],
            "revision": row["revision"],
            "disposition": row["disposition"],
            "decidedBy": row["decided_by"],
            "decidedAt": row["decided_at"],
            "recordedAt": row["recorded_at"],
        }
