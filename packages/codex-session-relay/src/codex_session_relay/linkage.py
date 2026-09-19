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


# How much of the digest a relay-owned identifier keeps. 128 bits rather than the 64 the
# contract's relationship id uses: that one is frozen and cannot be widened, these are not,
# and a collision here would silently MERGE two scopes or two edges rather than fail loudly.
ID_WIDTH = 32


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
    return "bnd-" + sha256_hex("|".join((role, scope_kind, scope_key, task_id)))[:ID_WIDTH]


def link_id(kind, upper_kind, upper_key, lower_kind, lower_key):
    """Scope-only, and symmetric for a peer.

    A peer relation is symmetric, so two parents registering it from opposite ends have to
    converge on one record rather than on two mirror images. Sorting the scope keys before
    hashing is what makes that true regardless of which side calls first.
    """
    if kind == PEER:
        low, high = sorted((upper_key, lower_key))
        return "lnk-" + sha256_hex("|".join((PEER, PROJECT, low, PROJECT, high)))[:ID_WIDTH]
    return "lnk-" + sha256_hex(
        "|".join((kind, upper_kind, upper_key, lower_kind, lower_key))
    )[:ID_WIDTH]


def directive_id(scope_kind, scope_key, from_scope_key, digest, revision):
    """Includes the link revision the instruction arrived on.

    A handover advances that revision, so without it a replacement supervisor re-issuing an
    identical instruction derived its predecessor's id, silently replayed that record, and the
    two were indistinguishable. Same instruction, different relationship, different fact.
    """
    return "dir-" + sha256_hex(
        "|".join((scope_kind, scope_key, from_scope_key, digest, str(revision)))
    )[:ID_WIDTH]


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

    def owners(self, scope_kind, scope_key):
        """EVERY live owner of a scope. Exactly one wherever the index could be installed.

        owner() has to answer with a row, and a store that already broke the live-owner index
        can hold two. Ordering those and returning the first is the failure this exists to
        prevent: it turns an unknown into a settled-looking answer, which is worse than an
        error because nothing downstream can tell it was a guess. The reading paths ask here
        and report the candidates; the writing paths keep owner(), because they run against a
        store whose index is installed and they refuse a second owner before it exists.
        """
        return [self._binding_record(row) for row in self.store.all(
            "SELECT * FROM scope_bindings"
            "  WHERE scope_kind = ? AND scope_key = ? AND status IN ('active','paused')"
            "    AND superseded_by IS NULL"
            "  ORDER BY revision DESC, binding_id",
            (scope_kind, scope_key),
        )]

    def _sole_owner(self, scope_kind, scope_key, contention):
        """The one live owner, or None with the contest appended to contention.

        Answering None and saying why is the whole point: a level with no owner is a gap, and
        a level with two is a contest, and neither is a level with an owner picked by sorting.
        """
        held = self.owners(scope_kind, scope_key)
        if len(held) > 1:
            contention.append(
                {"contention": "competing_owners", "scopeKind": scope_kind,
                 "scopeKey": scope_key,
                 "candidates": sorted(record["taskId"] for record in held)})
            return None
        return held[0] if held else None

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
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            # The lookup, the decision and the write are all inside BEGIN IMMEDIATE. Read
            # beforehand, two identical calls could both miss the row and the loser collided
            # on the primary key with a database error instead of converging; and an ARCHIVED
            # binding was returned as success without ever asking who holds the scope now.
            plan, refusal = self.binding_plan(
                db, role=role, scope_key=scope_key, endpoint=endpoint)
            if refusal is None:
                self.apply_binding_plan(db, plan, status=status, at=now)
            else:
                self._record_conflict_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.binding(bid)

    def binding_plan(self, db, *, role, scope_key, endpoint, replacing=None):
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
            if current["status"] in LIVE:
                # Two hosts claiming one LIVE binding is the contradiction worth refusing:
                # the scope is held right now, and the record cannot describe both machines.
                # An ARCHIVED binding is a different question - the task has since moved, and
                # the reactivation below revalidates the claim from scratch and writes the
                # endpoint it is given. Refusing there made a cross-host handback unreachable
                # while the branch that exists to record the new host sat just past it.
                if current["host_id"] != endpoint.host_id:
                    return None, _Refusal(
                        RefusalReason.LINK_CONFLICT,
                        bid + " is already bound on another host",
                        scope_kind=scope_kind, scope_key=scope_key,
                        incumbent=current["task_id"], challenger=endpoint.task_id,
                    )
                return (bid, "present", role, scope_kind, scope_key, endpoint), None
            # Reactivating is a claim on a scope somebody else may hold by now, and a task
            # that has since taken another role must not get one back this way. So an
            # archived binding is revalidated exactly like a new one rather than restored on
            # the strength of having existed.
            refusal = self._binding_refusal(db, role, scope_kind, scope_key, endpoint,
                                            replacing=replacing)
            if refusal is not None:
                return None, refusal
            return (bid, "reactivate", role, scope_kind, scope_key, endpoint), None
        refusal = self._binding_refusal(db, role, scope_kind, scope_key, endpoint,
                                        replacing=replacing)
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
            # The endpoint comes with it. A task that gets a scope back is not the task that
            # left it: a replacement was in between, and the returning one is running from
            # whatever cwd and CXC session it has NOW. Moving only the status kept the
            # metadata from its previous tenure, so routing and audit read a cwd and a session
            # that no longer existed while the binding said it was live.
            db.execute(
                "UPDATE scope_bindings SET status = ?, updated_at = ?, host_id = ?,"
                "  cwd = ?, cxc_session = ? WHERE binding_id = ?",
                # The requested status, not ACTIVE. The insert branch has always honoured it,
                # so restoring an equivalent paused claim quietly activated it instead - the
                # two branches of one call disagreeing about what the caller asked for.
                (status, at, endpoint.host_id, endpoint.cwd, endpoint.cxc_session, bid),
            )
            self.store.journal("scope_rebound", bid, {"scopeKey": scope_key}, at=at)
        return bid

    def _binding_refusal(self, db, role, scope_kind, scope_key, endpoint, *, replacing=None):
        """Every competition and role read, before anything is written.

        replacing names the outgoing owner of a handover. That task is the incumbent by
        definition, so counting it as a rival would refuse the very operation that replaces it;
        excluding it keeps the rival check meaningful for everybody else, including a SECOND
        handover racing the first, which still sees a live owner it did not name.
        """
        rival = db.execute(
            "SELECT binding_id, task_id, status FROM scope_bindings"
            "  WHERE scope_kind = ? AND scope_key = ? AND role = ?"
            "    AND status IN ('active','paused') AND superseded_by IS NULL"
            "    AND task_id != ? AND task_id IS NOT ?",
            (scope_kind, scope_key, role, endpoint.task_id, replacing),
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
        # One task, ONE scope at that role. Each level is one Codex task bound to one Linear
        # level by stable id - a supervisor to an initiative, a parent to a project, a child to
        # an issue - so several ready projects mean several parents, not one parent holding
        # several projects. Told apart from the refusal above because they are different
        # mistakes: that one is a task wearing two hats, this one is a task wearing the same
        # hat twice.
        held = db.execute(
            "SELECT scope_kind, scope_key FROM scope_bindings"
            "  WHERE task_id = ? AND role = ? AND scope_key != ?"
            "    AND status IN ('active','paused') AND superseded_by IS NULL",
            (endpoint.task_id, role, scope_key),
        ).fetchone()
        if held is not None:
            return _Refusal(
                RefusalReason.ROLE_ALREADY_BOUND,
                "task " + repr(endpoint.task_id) + " is already the " + role + " of "
                + held["scope_kind"] + " " + repr(held["scope_key"])
                + "; one task is bound to one Linear level, so a second "
                + scope_kind + " needs its own " + role,
                scope_kind=scope_kind, scope_key=scope_key,
                incumbent=held["scope_key"], challenger=endpoint.task_id,
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
        for who, endpoint in (("supervisor", supervisor), ("parent", parent)):
            _exact(endpoint.task_id, "the " + who + "'s task id")
            _exact(endpoint.host_id, "the " + who + "'s host id")
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
                # The HOSTS have to agree too: a replay is the same registration repeated, and
                # the same tasks reached on a different host is a different claim, which
                # binding_plan refuses rather than silently keeping the old host.
                for role_name, scope, endpoint in (
                    (SUPERVISOR, initiative_key, supervisor), (PARENT, project_key, parent),
                ):
                    _plan, host_refusal = self.binding_plan(
                        db, role=role_name, scope_key=scope, endpoint=endpoint)
                    if host_refusal is not None:
                        self._record_conflict_in(db, host_refusal, at=now)
                        refusal = host_refusal
                        break
                if refusal is None:
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
                and owning["upper_key"] == initiative_key:
            # The initiative that SUPERVISES this project cannot also merely reference it.
            # Both edges would be live for one scope pair - the unique index is per kind, so
            # it permits that - and a message between the two would then have two answers to
            # "what relationship is this", differing in the one thing a reference exists to
            # say: that it carries no directive authority.
            return _Refusal(
                RefusalReason.LINK_CONFLICT,
                "initiative " + repr(initiative_key) + " already supervises project "
                + repr(project_key) + " under " + owning["link_id"] + ", so it cannot also "
                "reference it; a reference is how a DIFFERENT initiative reads this outcome",
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
        if link_kind == REFERENCE and owning is None:
            # A reference points at an outcome that already has an owner. Allowed to go first,
            # it would BIND the nominated task as the project's parent - which is the one
            # thing a secondary initiative must not be able to do, since it would choose the
            # execution parent by referencing rather than by supervising.
            return _Refusal(
                RefusalReason.UNREGISTERED_SCOPE,
                "project " + repr(project_key) + " has no execution supervisor yet, so there "
                "is no outcome to reference. Register its execution supervision first",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent="", challenger=initiative_key,
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

    def _owns_its_issue(self, db, relationship_id, issue_key):
        """Is this relationship the live assignment for its issue?

        Keyed on the RELATIONSHIP rather than on the child task. Comparing tasks was enough
        while a replacement used a different child, and wrong the moment the same child was
        registered again for the same issue after a handover: the ids matched, the archived
        predecessor looked like the owner, and its status writes reached the successor's
        binding and edge.
        """
        live = db.execute(
            "SELECT relationship_id FROM relationships"
            "  WHERE issue_key = ? AND status IN ('active','paused')"
            "    AND superseded_by IS NULL"
            "  ORDER BY created_at DESC LIMIT 1",
            (issue_key,),
        ).fetchone()
        return live is None or live["relationship_id"] == relationship_id

    def replaceable_child_in(self, db, supersedes):
        """The outgoing child a replacement may take the issue binding from, or None.

        Only a LIVE predecessor that STILL HOLDS that binding qualifies. A predecessor which
        was cancelled or archived released its scope already, and naming its child as the
        outgoing owner then excused a rival that was not the predecessor at all: the same task
        can have claimed the issue directly in the meantime, and the successor's insert met
        the one-live-owner index - a database error - instead of the domain refusal the caller
        was owed. Read on the caller's connection so the answer is taken under the same lock
        as the write it authorizes.
        """
        if not supersedes:
            return None
        row = db.execute(
            "SELECT r.child_task_id AS child_task_id FROM relationships r"
            "  JOIN scope_bindings b ON b.scope_kind = ? AND b.scope_key = r.issue_key"
            "   AND b.role = ? AND b.task_id = r.child_task_id"
            " WHERE r.relationship_id = ? AND r.status IN ('active','paused')"
            "   AND r.superseded_by IS NULL"
            "   AND b.status IN ('active','paused') AND b.superseded_by IS NULL",
            (ISSUE, CHILD, supersedes),
        ).fetchone()
        return row["child_task_id"] if row else None

    def attach_in(self, db, relationship_row, project_key, *, at=None, replacing=None):
        """The ONE path that writes the lower level. Returns a refusal or None.

        Split into a decision and a write for the reason every other path is: a caller that
        inserts a relationship and then discovers its project is foreign has already written.
        attach_refusal is pure reads, so registry.register can take the decision BEFORE its own
        inserts and record the contest alone.
        """
        at = at or self.clock.iso()
        plan, refusal = self.attach_refusal(db, relationship_row, project_key,
                                            replacing=replacing)
        if refusal is not None:
            return refusal
        self.attach_apply(db, relationship_row, project_key, plan, at=at)
        return None

    def attach_refusal(self, db, relationship_row, project_key, *, replacing=None):
        """Decide an attachment without writing. Returns (plan, refusal).

        The plan carries what attach_apply must do, so idempotence is decided here over all
        THREE facts - the relationship_scope row, the child binding and the project to issue
        edge - rather than over the scope row alone. A relationship attached by an older writer,
        or by a run that stopped between two of them, is COMPLETED rather than reported done.
        """
        from .models import Endpoint

        rid = relationship_row["relationship_id"]
        issue_key = relationship_row["issue_key"]
        parent_task = relationship_row["parent_task_id"]
        child_task = relationship_row["child_task_id"]
        _exact(project_key, "a project key")
        if relationship_row["status"] != ACTIVE or relationship_row["superseded_by"]:
            return None, _Refusal(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                "relationship " + repr(rid) + " is " + repr(relationship_row["status"])
                + ", so its issue cannot be attached to a project",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=rid, challenger=project_key)
        if parent_task == child_task:
            return None, _Refusal(
                RefusalReason.SCOPE_CYCLE,
                "relationship " + repr(rid) + " has the same task as parent and child, which "
                "is a self-link rather than a level",
                scope_kind=ISSUE, scope_key=issue_key,
                incumbent=parent_task, challenger=child_task)
        holder = db.execute(
            "SELECT task_id FROM scope_bindings"
            "  WHERE scope_kind = ? AND scope_key = ? AND role = ?"
            "    AND status IN ('active','paused') AND superseded_by IS NULL",
            (PROJECT, project_key, PARENT),
        ).fetchone()
        if holder is None:
            return None, _Refusal(
                RefusalReason.UNREGISTERED_SCOPE,
                "project " + repr(project_key) + " has no registered parent, so an issue "
                "cannot be attached to it yet",
                scope_kind=PROJECT, scope_key=project_key, challenger=parent_task)
        # Relaxed ONLY for a genuine successor: this relationship must supersede one that was
        # itself scoped to this project AND assigned to this same issue. Accepting any
        # assignment whose issue merely has project history let an unrelated registration
        # under a foreign parent repoint the issue edge away from the project's live owner,
        # which is the guard's whole job; checking the project alone then still allowed a
        # relationship for issue B to borrow a predecessor belonging to issue A.
        predecessor = relationship_row["supersedes"] if "supersedes" in \
            relationship_row.keys() else None
        # And the predecessor has to still BE one. A scope row outlives the assignment that
        # wrote it, so a dead predecessor's history alone was enough to relax the check: a
        # successor could register under parent B while project P was still owned by A and
        # repoint P's issue edge to B. replaceable_child_in asks the same question the write
        # side asks - is this predecessor live and still holding the issue binding.
        #
        # Or this registration is the one that just took it over. register() archives the
        # predecessor before inserting the successor and calls here again afterwards, by which
        # time "still live" is false by its own doing. superseded_by naming THIS relationship
        # is what tells those two apart, and only the transaction that wrote it can see it, so
        # it relaxes nothing for a caller whose earlier check did not already pass.
        moving_within = None
        if predecessor:
            taken_over = db.execute(
                "SELECT 1 FROM relationships WHERE relationship_id = ? AND superseded_by = ?",
                (predecessor, rid),
            ).fetchone()
            if taken_over is not None \
                    or self.replaceable_child_in(db, predecessor) is not None:
                moving_within = db.execute(
                    "SELECT 1 FROM relationship_scope s"
                    "  JOIN relationships r ON r.relationship_id = s.relationship_id"
                    " WHERE s.relationship_id = ? AND s.project_key = ? AND r.issue_key = ?",
                    (predecessor, project_key, issue_key),
                ).fetchone()
        if holder["task_id"] != parent_task and moving_within is None:
            # The issue has never belonged to this project, so this is a foreign attachment.
            # When it HAS, a differing parent is an assignment being moved to a new one, which
            # is exactly how a project's work is handed over before its scope is.
            return None, _Refusal(
                RefusalReason.FOREIGN_SCOPE,
                "issue " + repr(issue_key) + " is assigned under parent " + repr(parent_task)
                + ", but project " + repr(project_key) + " is executed by "
                + repr(holder["task_id"]) + "; an issue belongs to its own project",
                scope_kind=PROJECT, scope_key=project_key,
                incumbent=holder["task_id"], challenger=parent_task)
        recorded = db.execute(
            "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", (rid,)
        ).fetchone()
        if recorded is not None and recorded["project_key"] != project_key:
            return None, _Refusal(
                RefusalReason.FOREIGN_SCOPE,
                "issue " + repr(issue_key) + " is already scoped to project "
                + repr(recorded["project_key"]) + ", not " + repr(project_key),
                scope_kind=ISSUE, scope_key=issue_key,
                incumbent=recorded["project_key"], challenger=project_key)
        # An ISSUE belongs to one project, whichever relationship asks. Keyed on the
        # relationship alone, two relationships naming the same issue could attach it to two
        # projects and leave the issue with an execution edge from each, which an upward walk
        # then chose between silently.
        elsewhere = db.execute(
            "SELECT s.project_key FROM relationship_scope s"
            "  JOIN relationships r ON r.relationship_id = s.relationship_id"
            " WHERE r.issue_key = ? AND s.project_key != ? AND s.relationship_id != ?"
            " LIMIT 1",
            (issue_key, project_key, rid),
        ).fetchone()
        if elsewhere is not None:
            return None, _Refusal(
                RefusalReason.FOREIGN_SCOPE,
                "issue " + repr(issue_key) + " is already scoped to project "
                + repr(elsewhere["project_key"]) + " through another assignment, so it cannot "
                "also belong to " + repr(project_key),
                scope_kind=ISSUE, scope_key=issue_key,
                incumbent=elsewhere["project_key"], challenger=project_key)
        plan, refusal = self.binding_plan(
            db, role=CHILD, scope_key=issue_key,
            endpoint=Endpoint(child_task, relationship_row["child_host_id"],
                              cwd=relationship_row["child_cwd"],
                              cxc_session=relationship_row["child_cxc_session"]),
            replacing=replacing,
        )
        if refusal is not None:
            return None, refusal
        return (plan, recorded is None), None

    def attach_apply(self, db, relationship_row, project_key, plan, *, at):
        """Perform an attachment decided earlier. Every refusal is already behind us."""
        binding, scope_row_missing = plan
        rid = relationship_row["relationship_id"]
        issue_key = relationship_row["issue_key"]
        parent_task = relationship_row["parent_task_id"]
        child_task = relationship_row["child_task_id"]
        self.apply_binding_plan(db, binding, at=at)
        if scope_row_missing:
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

    def record_conflict_in(self, db, refusal, *, at):
        """Public name for the conflict writer, for callers in other modules."""
        self._record_conflict_in(db, refusal, at=at)

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
        contested = []
        record = {
            "relationshipId": relationship_id,
            "projectKey": row["project_key"],
            "issueKey": issue_key,
            # Same rule as the walks: a scope with two live owners has no owner to report,
            # and saying which is contested beats naming whichever sorts first.
            "child": self._sole_owner(ISSUE, issue_key, contested) if issue_key else None,
            "parent": self._sole_owner(PROJECT, row["project_key"], contested),
            "link": self.link(link_id(EXECUTION, PROJECT, row["project_key"], ISSUE, issue_key))
            if issue_key else None,
        }
        record["contention"] = contested
        return record

    def apply_relationship_status_in(self, db, relationship_id, status,
                                     *, previous_status=None):
        """Move an assignment's lower level with the assignment itself.

        Called from registry._write_status, from resume's write transaction and from
        supersede's - not from set_status, which delegates and owns no transaction. A
        relationship and its lower level therefore move together or not at all.

        previous_status is what the relationship was BEFORE this write. The caller has to
        supply it because by the time this runs the row already says the new status, and a
        relationship that had already stopped being live has already given up its scope.

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
            "SELECT issue_key, child_task_id, parent_task_id FROM relationships"
            "  WHERE relationship_id = ?",
            (relationship_id,),
        ).fetchone()
        if row is None:
            return "unscoped"
        at = self.clock.iso()
        # Only the relationship that currently OWNS the issue scope may move it. A superseded
        # one still gets status writes - archiving is how supersession records itself - and
        # acting on them would archive the binding and edge its successor has already taken
        # over, disabling the replacement's whole lower level.
        current = db.execute(
            "SELECT task_id FROM scope_bindings"
            "  WHERE scope_kind = ? AND scope_key = ? AND role = ?"
            "    AND status IN ('active','paused') AND superseded_by IS NULL",
            (ISSUE, row["issue_key"], CHILD),
        ).fetchone()
        # paused is LIVE ownership on both sides of this boundary. The relationship model
        # treats a paused assignment as still responsible - it blocks a replacement
        # registration - and LIVE here is active or paused for the same reason. Collapsing
        # paused into archived released the issue scope while registry still reported the
        # child as responsible, so one store answered two ways about the same assignment.
        lower = status if status in LIVE else ARCHIVED
        if lower not in LIVE and previous_status is not None and previous_status not in LIVE:
            # Already released. A relationship gives up its issue scope ONCE, at the moment it
            # stops being live, and a later deactivation of an already-dead row must not reach
            # the binding again. In between, that scope can have been claimed directly through
            # bind_scope by the same child - which derives the SAME binding id - and a second
            # archive from a relationship holding nothing would take the new claim down.
            # _owns_its_issue cannot see this: a direct claim writes no relationship row for it
            # to find, and "no live assignment anywhere" is exactly what it reads as its own.
            return "released"
        if lower != ACTIVE and not self._owns_its_issue(
                db, relationship_id, row["issue_key"]):
            # Deactivating, and this relationship no longer owns the scope. A superseded one
            # still receives status writes - archiving is how supersession records itself -
            # and acting on them would archive the binding and the edge its SUCCESSOR has
            # already taken over, disabling the replacement's whole lower level.
            return "superseded"
        if lower in LIVE:
            # Any transition INTO a live status is a reactivation, paused as much as active.
            # Testing for active alone let an archived assignment be paused straight back into
            # a live relationship while its binding stayed archived, so AssignmentView reported
            # a responsible child and linkage reported issue_without_child - one store
            # answering two ways, which is the thing this whole path exists to prevent.
            #
            # First, the PROJECT must still be in the hands this assignment names. Archiving or
            # cancelling releases the issue and leaves nothing live for attached() to see, so
            # the project can be handed to a new parent with no work to strand and no refusal.
            # Coming back afterwards is the stale-owner case: the relationship and the delivery
            # authorization derived from it still name the OLD parent, while linkage names the
            # new one, which is precisely the split routing a handover exists to prevent.
            holder = db.execute(
                "SELECT task_id FROM scope_bindings"
                "  WHERE scope_kind = ? AND scope_key = ? AND role = ?"
                "    AND status IN ('active','paused') AND superseded_by IS NULL"
                "  ORDER BY revision DESC LIMIT 1",
                (PROJECT, scoped["project_key"], PARENT),
            ).fetchone()
            if holder is None or holder["task_id"] != row["parent_task_id"]:
                raise LinkageError(
                    RefusalReason.FOREIGN_SCOPE,
                    "project " + repr(scoped["project_key"]) + " is now parented by "
                    + (repr(holder["task_id"]) if holder else "nobody")
                    + ", not by " + repr(row["parent_task_id"]) + ", so restoring "
                    + repr(relationship_id) + " would reattach its issue under an owner the "
                    "project no longer has; re-register the assignment under the current "
                    "parent instead",
                )
            # Reactivating must not produce a second owner either. Cancelling RELEASES an
            # issue, so another child can be bound to it directly in the meantime; resume
            # checks relationships and never looked at who holds the scope now.
            rival = db.execute(
                "SELECT task_id FROM scope_bindings"
                "  WHERE scope_kind = ? AND scope_key = ? AND role = ?"
                "    AND status IN ('active','paused') AND superseded_by IS NULL"
                "    AND task_id != ?",
                (ISSUE, row["issue_key"], CHILD, row["child_task_id"]),
            ).fetchone()
            if rival is not None:
                raise LinkageError(
                    RefusalReason.DUPLICATE_SCOPE_OWNER,
                    "issue " + repr(row["issue_key"]) + " is now held by "
                    + repr(rival["task_id"]) + ", so restoring "
                    + repr(row["child_task_id"]) + " would leave it with two owners",
                )
            # And the child must still be ELIGIBLE to hold it. The unique index is scoped by
            # issue, and resume only checks for a competing relationship on the same issue, so
            # a child that took another role or another issue while this one was archived
            # could be reactivated into a second live scope by this direct update.
            blocked = db.execute(
                "SELECT role, scope_kind, scope_key FROM scope_bindings"
                "  WHERE task_id = ? AND status IN ('active','paused')"
                "    AND superseded_by IS NULL"
                "    AND (role != ? OR scope_key != ?)",
                (row["child_task_id"], CHILD, row["issue_key"]),
            ).fetchone()
            if blocked is not None:
                raise LinkageError(
                    RefusalReason.ROLE_ALREADY_BOUND if blocked["role"] == CHILD
                    else RefusalReason.SCOPE_ROLE_MISMATCH,
                    "task " + repr(row["child_task_id"]) + " has since become the "
                    + blocked["role"] + " of " + blocked["scope_kind"] + " "
                    + repr(blocked["scope_key"]) + ", so it cannot be restored as the child "
                    "of issue " + repr(row["issue_key"]) + " as well",
                )
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
        # The id needs the link revision, which is only known once the edge is read inside the
        # transaction, so it is derived there rather than up front.
        did = None
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
            elif edge["link_kind"] != EXECUTION \
                    or edge["upper_key"] != from_scope_key \
                    or edge["lower_key"] != scope_key \
                    or edge["lower_kind"] != scope_kind:
                refusal = _Refusal(
                    RefusalReason.UNREGISTERED_SCOPE,
                    "link " + repr(link_id_value) + " does not join " + repr(from_scope_key)
                    + " down to " + scope_kind + " " + repr(scope_key)
                    + " by execution. A reference carries no authority to instruct: a "
                    "secondary initiative references a project's outcome instead of issuing "
                    "it work",
                    scope_kind=scope_kind, scope_key=scope_key,
                    incumbent=str(link_id_value), challenger=from_scope_key,
                )
            elif edge["upper_task_id"] != from_task_id:
                refusal = _Refusal(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "link " + repr(link_id_value) + " records " + repr(edge["upper_task_id"])
                    + " as its upper endpoint, not " + repr(from_task_id),
                    scope_kind=scope_kind, scope_key=scope_key,
                    incumbent=edge["upper_task_id"], challenger=from_task_id,
                )
            if refusal is None:
                did = directive_id(scope_kind, scope_key, from_scope_key, digest,
                                   edge["revision"])
                # The replay check runs AFTER validation, not before it. Returning an existing
                # row first made the derived id a way past every check: the same digest
                # replayed with a task that owns nothing and a link that joins nothing was
                # accepted, because only the id had to match.
                replay = db.execute(
                    "SELECT * FROM scope_directives WHERE directive_id = ?", (did,)
                ).fetchone()
                if replay is not None:
                    return self._directive_record(replay)
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
        now = self.clock.iso()
        already = None
        refusal = None
        with self.store.transaction() as db:
            # Read the disposition inside the write transaction. Read before it, two deciders
            # could both see an unsettled directive and the second would still overwrite the
            # first; BEGIN IMMEDIATE serialises them so the second one sees the decision here.
            row = db.execute(
                "SELECT * FROM scope_directives WHERE directive_id = ?",
                (directive_id_value,),
            ).fetchone()
            if row is None:
                raise LinkageError(
                    RefusalReason.UNREGISTERED_SCOPE,
                    "no directive " + repr(directive_id_value))
            if row["disposition"] is not None:
                # A settlement that can be silently overwritten is not a decision. The second
                # writer would take the record from the first and leave nothing saying the two
                # disagreed, which is the opposite of retaining a contested instruction.
                # Restating the SAME disposition converges, like every other registration
                # here; changing it is refused.
                #
                # Refused AND retained. Raising from inside the transaction rolled it back and
                # took the evidence with it, so the disagreement this module exists to preserve
                # was the one thing the refusal destroyed. The contest is written first and the
                # error is raised after the commit, which is what every other write path here
                # does.
                if row["disposition"] != disposition:
                    refusal = _Refusal(
                        RefusalReason.LINK_CONFLICT,
                        "directive " + repr(directive_id_value) + " was already settled as "
                        + repr(row["disposition"]) + " by " + repr(row["decided_by"])
                        + " at " + str(row["decided_at"]) + "; the decision stands and a "
                        "later instruction is recorded as its own directive",
                        scope_kind=row["scope_kind"], scope_key=row["scope_key"],
                        incumbent=row["decided_by"] or "", challenger=decided_by,
                    )
                    self._record_conflict_in(db, refusal, at=now)
                else:
                    already = row
            else:
                db.execute(
                    "UPDATE scope_directives SET disposition = ?, decided_by = ?,"
                    "  decided_at = ? WHERE directive_id = ?",
                    (disposition, decided_by, now, directive_id_value),
                )
                self.store.journal(
                    "directive_settled", directive_id_value,
                    {"disposition": disposition, "decidedBy": decided_by, "reason": reason},
                    at=now,
                )
        if refusal is not None:
            raise refusal.error()
        if already is not None:
            return self._directive_record(already)
        return self._directive_record(self.store.one(
            "SELECT * FROM scope_directives WHERE directive_id = ?", (directive_id_value,)))

    def contested_directives(self, scope_kind, scope_key):
        """Undisposed directives of differing digests from different origins.

        Two supervisors can agree about who the parent is and still instruct it differently, so
        this is decided on the digest rather than on ownership.
        """
        open_ones = [d for d in self.directives(scope_kind, scope_key)
                     if d["disposition"] is None]
        # Decided on the digest alone. Only the execution supervisor can instruct, so two
        # conflicting instructions are usually ITS successive ones rather than two origins;
        # requiring two origins meant the ordinary conflict was never reported.
        if len({d["digest"] for d in open_ones}) > 1:
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
        for side, endpoint in (("left", left_parent), ("right", right_parent)):
            _exact(endpoint.task_id, "the " + side + " parent's task id")
            _exact(endpoint.host_id, "the " + side + " parent's host id")
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
                # The host is part of the claim, not decoration. binding_plan refuses a task
                # already bound on a different host, so routing the endpoint through it is
                # what stops a peer link recording a host nothing agrees with.
                _plan, host_refusal = self.binding_plan(
                    db, role=PARENT, scope_key=scope_key, endpoint=endpoint)
                if host_refusal is not None:
                    refusal = host_refusal
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

    def counterpart(self, from_task, to_task, *, quoted_revision=None, quoted_scope=None,
                    from_scope=None):
        """What a message can establish about whom it is addressing.

        Both endpoints are resolved as CANDIDATE SETS and the pair an actual live link joins
        is the one answered about. One task holds one live scope per role, so in a healthy
        store each set has one live member and the choice is trivial; the sets exist because a
        task's ARCHIVED bindings are candidates too - that is how a message naming a replaced
        owner is told so - and because a reader must not pick a branch if a store somehow
        holds two live ones. OPS-7.4 says a message binds both Linear scopes; from_scope and
        quoted_scope are how a caller supplies them, and they narrow the candidates rather
        than being checked after a pick.

        Returns a record and never raises for an absence, and never reports a failure to READ
        as an absence: an empty answer with readable True means the store said there is
        nothing, and one with readable False means the store did not answer.
        """
        import sqlite3

        try:
            senders = self._bindings_for(from_task)
            recipients = self._bindings_for(to_task)
            findings = []
            if from_scope is not None:
                narrowed = [b for b in senders if b["scopeKey"] == from_scope]
                if narrowed:
                    senders = narrowed
                else:
                    # Symmetric with quoted_scope below. Falling back to the sender's other
                    # scopes could answer linked with no findings about a scope the message
                    # never named, which is the opposite of what quoting a source scope means.
                    findings.append("foreign_sender_scope")
                    # So nothing is answered about, for the same reason the recipient side
                    # clears its own: a finding beside a state that contradicts it is worse
                    # than either alone.
                    senders = []
            wrong_scope = None
            if quoted_scope is not None:
                narrowed = [b for b in recipients if b["scopeKey"] == quoted_scope]
                if narrowed:
                    recipients = narrowed
                else:
                    wrong_scope = "foreign_scope"
                    # And nothing else is answered about. Keeping the endpoint's OTHER
                    # bindings let a message that named a scope its recipient does not hold be
                    # returned as linked, against a scope nobody mentioned, with the finding
                    # sitting beside a state that contradicted it.
                    recipients = []
            sender = senders[0] if senders else None
            recipient = recipients[0] if recipients else None
            edge = None
            # EVERY candidate pair is evaluated before anything is chosen. Stopping at the
            # first pair that had a link was its own arbitrary pick: a task accumulates
            # archived bindings for the scopes it used to hold, so two historical pairs can
            # both be joined by a live edge, and the one that sorted first was answered about
            # as though the other did not exist. Contention covers both shapes - several
            # edges for one pair, and several pairs - because the question is the same one:
            # which relationship is this message about.
            matches = []
            for candidate_sender in senders:
                for candidate_recipient in recipients:
                    joined = self._joining_links(candidate_sender, candidate_recipient)
                    if joined:
                        matches.append((candidate_sender, candidate_recipient, joined))
            contention = None
            if len(matches) == 1 and len(matches[0][2]) == 1:
                sender, recipient = matches[0][0], matches[0][1]
                edge = matches[0][2][0]
            elif matches:
                # Reporting it as unlinked would be worse than any single answer: it would
                # deny a linkage that demonstrably exists. from and counterpart stay as the
                # endpoints' own live-first bindings so the staleness findings still apply;
                # what is withheld is the claim that one particular edge is the answer.
                contention = [
                    {"linkId": record["linkId"], "kind": record["kind"],
                     "scopeKey": candidate_recipient["scopeKey"]}
                    for _s, candidate_recipient, joined in matches
                    for record in joined
                ]
            current = None
            if recipient is not None and (recipient["status"] not in LIVE
                                          or recipient["supersededBy"]):
                findings.append("stale_owner")
                held = self.owners(recipient["scopeKind"], recipient["scopeKey"])
                if len(held) > 1:
                    # Two live owners, so there is no "who holds it now" to answer with.
                    # Naming one of them would tell a stale sender to re-address a message to
                    # a task picked by sorting.
                    findings.append("competing_owners")
                else:
                    current = held[0] if held else None
            # The sender can be stale too. A message FROM a task that no longer owns its scope
            # is exactly as misrouted as one addressed to a replaced owner.
            if sender is not None and (sender["status"] not in LIVE
                                       or sender["supersededBy"]):
                findings.append("stale_sender")
            if wrong_scope is not None:
                findings.append(wrong_scope)
            if contention is not None:
                findings.append("link_contention")
                if self._role_pair_is_wrong(sender, recipient):
                    findings.append("wrong_role")
                return {
                    "state": "ambiguous", "readable": True, "link": None,
                    "from": sender, "counterpart": recipient, "currentOwner": current,
                    "candidates": contention, "findings": sorted(set(findings)),
                }
            if sender is None or recipient is None:
                if recipient is None:
                    findings.append("unregistered_link")
                return {
                    "state": "unlinked", "readable": True, "link": None,
                    "from": sender, "counterpart": recipient, "currentOwner": current,
                    "findings": sorted(set(findings)),
                }
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
            # Not "older than". A revision that does not exist yet is not current either, and
            # reporting only the lower side let a message quoting a future linkage read clean.
            if quoted_revision is not None and quoted_revision != edge["revision"]:
                findings.append("stale_revision")
            for side in ("upper", "lower"):
                held = self.owners(edge[side]["scopeKind"], edge[side]["scopeKey"])
                if len(held) > 1:
                    findings.append("competing_owners")
                    continue
                live = held[0] if held else None
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
        except sqlite3.Error as fault:
            return {
                "state": "unreadable", "readable": False, "link": None,
                "from": None, "counterpart": None, "currentOwner": None, "findings": [],
                # findings stays empty - a store that could not be read has nothing to report
                # ABOUT the linkage - but the fault itself is a diagnostic worth keeping, and
                # collapsing corruption, schema drift and a query fault into one word threw it
                # away.
                "detail": type(fault).__name__ + ": " + str(fault),
            }

    def _bindings_for(self, task_id):
        """Every scope this task holds, live ones first, newest revision first.

        Deliberately not filtered to live rows: a message naming a replaced owner has to be
        told that it did, and the only way to say so is to find the archived binding. Returned
        as a LIST rather than one row, because a task may hold several scopes of one role and
        choosing between them belongs to whoever knows which scope the message is about.
        """
        return [
            self._binding_record(row)
            for row in self.store.all(
                "SELECT * FROM scope_bindings WHERE task_id = ?"
                "  ORDER BY CASE WHEN status IN ('active','paused') THEN 0 ELSE 1 END,"
                "           revision DESC, scope_key",
                (task_id,),
            )
        ]

    def _joining_links(self, sender, recipient):
        """EVERY live link joining these two scopes, in either direction.

        A list rather than one row. The partial unique index allows one live edge per KIND,
        so one scope pair can still hold an execution edge and a reference edge at once -
        register_supervision now refuses to create that, but a store written before it did,
        or by hand, can contain it. Taking the highest revision answered a message about
        whichever relationship sorted first, and equal revisions made that an arbitrary pick
        between two different meanings. The caller reports the contention instead.
        """
        return [
            self._link_record(row)
            for row in self.store.all(
                "SELECT * FROM scope_links"
                "  WHERE status IN ('active','paused') AND superseded_by IS NULL"
                "    AND ((upper_kind = ? AND upper_key = ? AND lower_kind = ? AND lower_key = ?)"
                "      OR (upper_kind = ? AND upper_key = ? AND lower_kind = ? AND lower_key = ?))"
                "  ORDER BY revision DESC, link_id",
                (sender["scopeKind"], sender["scopeKey"],
                 recipient["scopeKind"], recipient["scopeKey"],
                 recipient["scopeKind"], recipient["scopeKey"],
                 sender["scopeKind"], sender["scopeKey"]),
            )
        ]

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
            # An unknown scope is unregistered, not a resolved tree of one empty level.
            # _descend appends a level unconditionally, so without this the branch below was
            # unreachable and a scope nobody ever registered read as resolved.
            if not self.owners(scope_kind, scope_key) and not self.store.one(
                "SELECT 1 FROM scope_links"
                "  WHERE ((upper_kind = ? AND upper_key = ?) OR (lower_kind = ? AND"
                "         lower_key = ?)) AND status IN ('active','paused')",
                (scope_kind, scope_key, scope_kind, scope_key),
            ):
                return {"state": "unregistered", "readable": True, "levels": [],
                        "gaps": [{"gap": scope_kind + "_without_"
                                  + ROLE_SCOPE_OWNER[scope_kind],
                                  "scopeKind": scope_kind, "scopeKey": scope_key}],
                        "contention": self.conflicts(scope_kind, scope_key)}
            # A visited set, for the reason _reaches has a budget: a cyclic edge set is not
            # reachable through the write paths, but a reader that recursed forever on a
            # corrupt or hand-edited store would be worse than one that answers.
            self._descend(scope_kind, scope_key, levels, gaps, contention, depth=0,
                          seen=set(), path=())
            if not levels:
                return {"state": "unregistered", "readable": True,
                        "levels": [], "gaps": gaps, "contention": contention}
            # Contested ownership anywhere in the tree makes the whole answer ambiguous, the
            # same word up() uses. Reporting the finding beside a state that called the walk
            # resolved left a consumer free to read past it.
            contested = any(row.get("contention") in ("competing_parents",
                                                      "competing_owners")
                            for row in contention)
            return {"state": "ambiguous" if contested else "resolved", "readable": True,
                    "levels": levels, "gaps": gaps, "contention": contention}
        except sqlite3.Error as fault:
            return {"state": "unreadable", "readable": False,
                    "levels": [], "gaps": [], "contention": [],
                    "detail": type(fault).__name__ + ": " + str(fault)}

    def _incoming_execution(self, scope_kind, scope_key):
        """The live execution edges INTO a scope, by id. One in a healthy store."""
        return sorted(row["link_id"] for row in self.store.all(
            "SELECT link_id FROM scope_links"
            "  WHERE lower_kind = ? AND lower_key = ? AND link_kind = 'execution'"
            "    AND status IN ('active','paused') AND superseded_by IS NULL",
            (scope_kind, scope_key),
        ))

    def _descend(self, scope_kind, scope_key, levels, gaps, contention, *, depth, seen,
                 path=()):
        if (scope_kind, scope_key) in path:
            # A back edge INTO the chain currently being walked. That is a cycle.
            contention.append({"contention": "scope_cycle", "scopeKind": scope_kind,
                               "scopeKey": scope_key})
            return
        if (scope_kind, scope_key) in seen:
            # Reached again from a different branch, which is not a cycle at all. One global
            # visited set could not tell the two apart, so a scope with two execution parents
            # was reported as a corrupt back edge rather than as the contested ownership it
            # is - and the walk still called itself resolved. It is not descended into twice.
            contention.append(
                {"contention": "competing_parents", "scopeKind": scope_kind,
                 "scopeKey": scope_key,
                 "candidates": self._incoming_execution(scope_kind, scope_key)})
            return
        seen.add((scope_kind, scope_key))
        # A contest is recorded by _sole_owner, and a scope with two owners is not a scope
        # with none: the gap below would say nobody holds it, which is the opposite of true.
        marked = len(contention)
        owner = self._sole_owner(scope_kind, scope_key, contention)
        levels.append({"scopeKind": scope_kind, "scopeKey": scope_key, "owner": owner,
                       "depth": depth})
        if owner is None and len(contention) == marked:
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
                          levels, gaps, contention, depth=depth + 1, seen=seen,
                          path=path + ((scope_kind, scope_key),))

    def up(self, *, task_id=None, issue_key=None, relationship_id=None, scope_key=None):
        """Child to parent to supervisor, reporting a missing upper level as a gap."""
        import sqlite3

        try:
            # Only when the task is the ONLY selector. A caller that also named an issue or a
            # relationship has already said which hierarchy it means, and answering it with an
            # ambiguity it did not have would ignore the selector it supplied.
            if (task_id is not None and scope_key is None
                    and issue_key is None and relationship_id is None):
                held = [b for b in self._bindings_for(task_id) if b["status"] in LIVE]
                if len({b["scopeKey"] for b in held}) > 1:
                    # Unreachable through the write paths, which refuse a second live scope
                    # of one role for one task. Kept because a reader must answer safely on a
                    # store that contains one anyway - an older writer, a hand edit, a future
                    # bug - and the rule for that case is to report the ambiguity rather than
                    # pick a branch. This is a defence, not a supported shape.
                    return {
                        "state": "ambiguous", "readable": True, "levels": [], "gaps": [],
                        "contention": [{"contention": "ambiguous_scope", "taskId": task_id,
                                        "candidates": sorted({b["scopeKey"] for b in held})}],
                    }
            start = self._starting_scope(task_id, issue_key, relationship_id,
                                         scope_key=scope_key)
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
                marked = len(contention)
                owner = self._sole_owner(scope_kind, scope_key, contention)
                levels.append({"scopeKind": scope_kind, "scopeKey": scope_key,
                               "owner": owner, "depth": len(levels)})
                if owner is None and len(contention) == marked:
                    gaps.append({"gap": scope_kind + "_without_"
                                 + ROLE_SCOPE_OWNER[scope_kind],
                                 "scopeKind": scope_kind, "scopeKey": scope_key})
                contention.extend(self.conflicts(scope_kind, scope_key))
                for directive in self.contested_directives(scope_kind, scope_key):
                    contention.append({"contention": "instruction_conflict",
                                       "scopeKind": scope_kind, "scopeKey": scope_key,
                                       "directiveId": directive["directiveId"],
                                       "fromScopeKey": directive["fromScopeKey"],
                                       "digest": directive["digest"]})
                incoming = self.store.all(
                    "SELECT * FROM scope_links"
                    "  WHERE lower_kind = ? AND lower_key = ? AND link_kind = 'execution'"
                    "    AND status IN ('active','paused') AND superseded_by IS NULL"
                    "  ORDER BY revision DESC, link_id",
                    (scope_kind, scope_key),
                )
                if len(incoming) > 1:
                    # Several live execution edges INTO one scope. Registration refuses to
                    # create that, but this reader answers about stores older writers or a
                    # hand edit produced, and taking the highest revision would walk one
                    # arbitrary hierarchy and call it resolved. Same rule as the ambiguous
                    # start above: report the competing edges, do not choose a parent.
                    contention.append(
                        {"contention": "competing_parents", "scopeKind": scope_kind,
                         "scopeKey": scope_key,
                         "candidates": sorted(link["link_id"] for link in incoming)})
                    return {"state": "ambiguous", "readable": True, "levels": levels,
                            "gaps": gaps, "contention": contention}
                row = incoming[0] if incoming else None
                if row is None:
                    if scope_kind == PROJECT:
                        gaps.append({"gap": "no_supervisor", "scopeKind": PROJECT,
                                     "scopeKey": scope_key})
                    break
                edge = self._link_record(row)
                live = self.owner(edge["lower"]["scopeKind"], edge["lower"]["scopeKey"])
                if live is not None and live["taskId"] != edge["lower"]["taskId"]:
                    contention.append({"contention": "owner_drift", "linkId": edge["linkId"],
                                       "recorded": edge["lower"]["taskId"],
                                       "live": live["taskId"]})
                scope_kind, scope_key = row["upper_kind"], row["upper_key"]
            # Same rule the downward walk follows: a contest anywhere makes the answer
            # ambiguous rather than a resolved chain with one candidate quietly chosen.
            return {"state": "ambiguous" if any(
                        row.get("contention") == "competing_owners" for row in contention
                    ) else "resolved",
                    "readable": True, "levels": levels,
                    "gaps": gaps, "contention": contention}
        except sqlite3.Error as fault:
            return {"state": "unreadable", "readable": False, "levels": [], "gaps": [],
                    "contention": [],
                    "detail": type(fault).__name__ + ": " + str(fault)}

    def _starting_scope(self, task_id, issue_key, relationship_id, *, scope_key=None):
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
            if scope_key is not None:
                held = [b for b in self._bindings_for(task_id)
                        if b["status"] in LIVE and b["scopeKey"] == scope_key]
                return (held[0]["scopeKind"], held[0]["scopeKey"]) if held else None
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

    def outstanding(self, project_key, task_id=None):
        """Live assignments in this project that are not finished.

        Scoped to the PROJECT, not to whichever task currently parents each row. Filtering by
        the parent task meant that after one handover the replacement appeared to have no
        outstanding work at all, so a second replacement could take the project without
        acknowledging anything. task_id narrows it when a caller genuinely wants one parent's
        rows, and handover does not use that narrowing.

        Derived through AssignmentView.state() rather than by asking whether a merge mark
        exists, because a mark counts only where it matches the CURRENT head event, generation
        and revision. A mark left over from an earlier generation would otherwise hide work that
        is still unfinished, which is exactly what a handover must not do.
        """
        from .assignment import AssignmentView
        from .registry import Registry

        view = AssignmentView(self.store, Registry(self.store, self.clock), self.clock)
        unfinished_ids = []
        rows = self.store.all(
            "SELECT r.relationship_id AS rid, r.parent_task_id AS parent FROM relationships r"
            "  JOIN relationship_scope s ON s.relationship_id = r.relationship_id"
            " WHERE s.project_key = ?"
            "   AND r.status IN ('active','paused') AND r.superseded_by IS NULL"
            " ORDER BY r.created_at",
            (project_key,),
        )
        for row in rows:
            if task_id is not None and row["parent"] != task_id:
                continue
            if view.state(row["rid"])["state"] not in FINISHED_STATES:
                unfinished_ids.append(row["rid"])
        return unfinished_ids

    def attached(self, project_key, task_id=None, *, other_than=None):
        """Every LIVE assignment in this project, settled or not.

        task_id narrows to one parent's rows. other_than does the opposite and is what a
        handover asks: everything NOT named by the incoming owner. Asking only about the
        outgoing one let assignments parked on a third parent slip through, and the project
        owner and its issue edges then named different tasks.

        Wider than outstanding on purpose. A merged assignment is still a live relationship
        whose next generation opens under the parent named on its own row, so a handover that
        only looked at unfinished work let settled work reopen under the owner that had
        already stepped down. Reopening is ordinary - a needs_changes verdict does it - so
        "finished for now" is not the same as "cannot come back".
        """
        return [
            row["relationship_id"] for row in self.store.all(
                "SELECT r.relationship_id FROM relationships r"
                "  JOIN relationship_scope s ON s.relationship_id = r.relationship_id"
                " WHERE s.project_key = ? AND r.status IN ('active','paused')"
                "   AND r.superseded_by IS NULL"
                "   AND (? IS NULL OR r.parent_task_id = ?)"
                "   AND (? IS NULL OR r.parent_task_id != ?)"
                " ORDER BY r.created_at",
                (project_key, task_id, task_id, other_than, other_than),
            )
        ]

    def handover(self, *, role, scope_key, expect_task_id, endpoint, acknowledged, evidence,
                 actor):
        """Replace a scope's owner, only from a caller that has read what it is taking on.

        The caller restates both the outgoing owner AND the outstanding work, which is
        registry.resume's discipline: it has to say what it believes it is taking over, and be
        right. A caller that has not read the outstanding work cannot produce the list, which is
        the point - an owner is never taken over silently.

        Every read that decides the outcome happens INSIDE the write transaction. Read
        beforehand, two callers could each see the same outgoing owner, each find the
        outstanding set unchanged, and both write, leaving one scope with two live owners. That
        was measured rather than predicted: a deterministic race produced exactly it before
        these reads moved inside.

        The incoming owner goes through binding_plan like every other binding, so a handover is
        not a route around the one-task-one-role rule.
        """
        scope_kind = ROLE_SCOPE.get(role)
        if scope_kind is None:
            raise LinkageError(
                RefusalReason.SCOPE_ROLE_MISMATCH, "unknown role " + repr(role))
        if role == CHILD:
            # A child is owned by its assignment, not by this table. Replacing one here would
            # move the binding and the edge while the relationships row kept naming the
            # outgoing task, so delivery and assignment state would go on targeting it, and a
            # later pause could leave both bindings live. The supported path is
            # register(supersedes=...), which moves both together.
            raise LinkageError(
                RefusalReason.SCOPE_ROLE_MISMATCH,
                "a child is replaced by registering its successor with supersedes, which moves "
                "the assignment and its issue scope together; handover covers a supervisor or "
                "a parent",
            )
        if not str(evidence or "").strip():
            raise LinkageError(
                RefusalReason.HANDOVER_UNCONFIRMED, "a handover carries its evidence")
        _exact(scope_key, "a scope key")
        _exact(endpoint.task_id, "the replacement owner's task id")
        _exact(endpoint.host_id, "the replacement owner's host id")
        claimed = sorted(set(acknowledged or ()))
        now = self.clock.iso()
        new_id = binding_id(role, scope_kind, scope_key, endpoint.task_id)
        refusal = None
        with self.store.transaction() as db:
            row = db.execute(
                "SELECT * FROM scope_bindings"
                "  WHERE scope_kind = ? AND scope_key = ? AND role = ?"
                "    AND status IN ('active','paused') AND superseded_by IS NULL"
                "  ORDER BY revision DESC LIMIT 1",
                (scope_kind, scope_key, role),
            ).fetchone()
            current = self._binding_record(row) if row else None
            plan = None
            if current is None:
                refusal = _Refusal(
                    RefusalReason.UNREGISTERED_SCOPE,
                    scope_kind + " " + repr(scope_key) + " has no live owner to replace",
                    scope_kind=scope_kind, scope_key=scope_key,
                    challenger=endpoint.task_id)
            elif current["taskId"] != expect_task_id:
                refusal = _Refusal(
                    RefusalReason.HANDOVER_UNCONFIRMED,
                    "this handover expects " + repr(expect_task_id) + " to hold "
                    + repr(scope_key) + ", but it is held by " + repr(current["taskId"])
                    + "; re-read the scope before replacing its owner",
                    scope_kind=scope_kind, scope_key=scope_key,
                    incumbent=current["taskId"], challenger=endpoint.task_id)
            elif endpoint.task_id == expect_task_id:
                # Same task in and out. The binding id derives from the task, so this
                # superseded a binding with itself: one row pointing at its own id, archived
                # and live at once, with the revision advanced over nothing. Decided AFTER the
                # scope is read, so an unregistered or differently owned scope gets its own
                # answer rather than this one, and the contest is recorded like any other.
                refusal = _Refusal(
                    RefusalReason.HANDOVER_UNCONFIRMED,
                    "task " + repr(endpoint.task_id) + " already holds " + repr(scope_key)
                    + "; a handover replaces the owner with a different one",
                    scope_kind=scope_kind, scope_key=scope_key,
                    incumbent=expect_task_id, challenger=endpoint.task_id)
            else:
                unfinished = self.outstanding(scope_key) if scope_kind == PROJECT else []
                if claimed != sorted(set(unfinished)):
                    refusal = _Refusal(
                        RefusalReason.HANDOVER_UNCONFIRMED,
                        "the outstanding work restated by this handover is " + repr(claimed)
                        + " but the store says it is " + repr(sorted(set(unfinished)))
                        + "; a replacement owner confirms the unfinished work it takes on",
                        scope_kind=scope_kind, scope_key=scope_key,
                        incumbent=expect_task_id, challenger=endpoint.task_id)
                elif scope_kind == PROJECT and self.attached(
                        scope_key, other_than=endpoint.task_id):
                    # Everything that does NOT already name the incoming owner blocks. Asking
                    # only about the outgoing one let assignments parked on a third parent
                    # through; asking about all of them made the escape route this refusal
                    # prescribes impossible to finish. This is the rule that says what the
                    # handover is for: afterwards every live assignment names the new parent.
                    still_here = self.attached(scope_key, other_than=endpoint.task_id)
                    # Refuse, and say what it could not move. An assignment's identity is
                    # sha256(parentTaskId|childTaskId|issueKey) and its queued deliveries name
                    # the parent's thread, so a handover cannot carry the endpoint across: it
                    # would leave a replacement parent that cannot receive the work it just
                    # accepted. Each of these is moved by registering its successor with
                    # supersedes, which mints a correct new identity and repoints the edge.
                    refusal = _Refusal(
                        RefusalReason.HANDOVER_WOULD_STRAND,
                        scope_kind + " " + repr(scope_key) + " still has unfinished work that "
                        "this handover cannot move: " + repr(sorted(set(still_here)))
                        + " (of which " + repr(sorted(set(unfinished))) + " is unfinished; a "
                        "settled one still reopens under the parent named on its own row)"
                        + ". An assignment's identity and its queued deliveries name its "
                        "parent, so each one is moved by registering its successor with "
                        "supersedes before the scope changes hands",
                        scope_kind=scope_kind, scope_key=scope_key,
                        incumbent=expect_task_id, challenger=endpoint.task_id)
            if refusal is None:
                plan, refusal = self.binding_plan(
                    db, role=role, scope_key=scope_key, endpoint=endpoint,
                    replacing=expect_task_id)
            if refusal is not None:
                self._record_conflict_in(db, refusal, at=now)
            else:
                db.execute(
                    "UPDATE scope_bindings SET status = ?, superseded_by = ?, updated_at = ?"
                    "  WHERE binding_id = ?",
                    (ARCHIVED, new_id, now, current["bindingId"]),
                )
                if plan[1] == "insert":
                    self._insert_binding(
                        db, new_id, role, scope_kind, scope_key, endpoint, ACTIVE,
                        revision=current["revision"] + 1, at=now,
                        supersedes=current["bindingId"], note=evidence,
                    )
                else:
                    # The endpoint comes with it, exactly as it does on the insert branch. A
                    # task taking a scope BACK is not the task that left it: a replacement was
                    # in between and the returning one is running from whatever cwd and CXC
                    # session it has now. Moving only the status kept the metadata from its
                    # previous tenure, which routing and audit then read as current.
                    db.execute(
                        "UPDATE scope_bindings SET status = ?, revision = ?, supersedes = ?,"
                        " handover_note = ?, superseded_by = NULL, updated_at = ?,"
                        " host_id = ?, cwd = ?, cxc_session = ?"
                        "  WHERE binding_id = ?",
                        (ACTIVE, current["revision"] + 1, current["bindingId"], evidence,
                         now, endpoint.host_id, endpoint.cwd, endpoint.cxc_session, new_id),
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
        if refusal is not None:
            raise refusal.error()
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
