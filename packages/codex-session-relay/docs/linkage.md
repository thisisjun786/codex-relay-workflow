# Three-level linkage

Relay-owned records, not contract records. The five schemas under
`src/codex_session_relay/schema/` are byte copies of the frozen cross-session communication
contract and none of them describes anything on this page. Nothing here is validated against
them, and nothing here changed them.

## Why these are separate tables

`relationships` binds one parent task to one child task for one issue. That is the whole of the
registered hierarchy it can express, and three separate contracts break if a supervision is
pushed into it:

- identity is `sha256(parentTaskId|childTaskId|issueKey)`, and a supervision has no issue
- the rival check enforces one live assignment per issue key, so a project id used as an issue
  key would collide with itself once the project had two issues
- `relationship.json` sets `additionalProperties: false` and its bytes are digest-pinned, so a
  role or scope field would change a frozen schema

A supervision is also not an assignment in the first place. It carries no receipt, no
acknowledgement, no verdict, no generation and no artifact scope, and a peer link carries less
than that. The package already had the pattern for a fact the frozen contract has no room for:
`verdict_context`, `claim_context`, `attempt_messages` and `assignment_settlements` all exist
for that reason.

New tables reach an existing database and new columns do not. `Store.__init__` runs
`executescript(DDL)` on every open and the DDL is all `CREATE TABLE IF NOT EXISTS`, so a
deployed store gains these five tables the next time it is opened. There is no migration step
and no second database.

## The records

| Table | Holds |
|---|---|
| `scope_bindings` | which task owns which scope, at which level, with its status and revision |
| `scope_links` | the edges: `execution`, `reference` and `peer` |
| `relationship_scope` | which project an issue assignment belongs to |
| `scope_directives` | an instruction that reached a scope, by digest and origin |
| `linkage_conflicts` | a contested attempt, retained after it was refused |

## Identity

    bindingId   = "bnd-" + sha256(role|scopeKind|scopeKey|taskId)[:32]
    linkId      = "lnk-" + sha256(kind|upperKind|upperKey|lowerKind|lowerKey)[:32]
    peer linkId = "lnk-" + sha256("peer"|"project"|lower|"project"|higher)[:32]
    directiveId = "dir-" + sha256(scopeKind|scopeKey|fromScopeKey|digest|revision)[:32]

128 bits, not the 64 the contract's `relationshipId` keeps. That one is frozen and cannot be
widened; these are relay-owned, and a collision here would silently MERGE two scopes or two
edges rather than fail loudly, so the cheap width is the right one.

A **link** is keyed by its two scopes and never by a task id. Keying on the owner would mean a
handover changed the identity of an unchanged relationship, so a later re-registration would
derive a different id and create a duplicate. The task columns on `scope_links` are the owners
as they stood when the edge was written; they are kept for drift detection and are deliberately
outside identity.

A **binding** is keyed WITH its task id, because a binding is one task's claim on one scope.
Replacing the owner is supposed to produce a new binding rather than rewrite who the old one
was, so the old row stays, archived, with `supersededBy` pointing at its replacement.

A **peer** id sorts its two scope keys before hashing. A peer relation is symmetric, so two
parents registering it from opposite ends have to converge on one record rather than on two
mirror images.

A **directive** carries the revision of the link it arrived on, which is the one identity here
that deliberately does NOT converge across a handover. Replaying an instruction to the same
scope is meant to land on the same record, and a replacement supervisor re-issuing a
byte-identical instruction is a different fact: a handover advances the edge's revision, so
without it the second directive derived its predecessor's id and the two became
indistinguishable. Derive the id with the revision the edge carries when the directive is
recorded, not the one it had when the instruction was written.

## The hierarchy, and what cannot enter it

Only `(initiative, project)` and `(project, issue)` are execution edges. Every walk filters
`link_kind = 'execution'` in its SQL, which is why a `reference` or a `peer` row can neither
lengthen a chain nor introduce a second execution owner. That is a property of the query rather
than a convention, and a test asserts the walk's output is unchanged by adding a peer row.

One task holds one role. That single rule is also what makes mutual supervision unreachable: a
task cannot be both a parent and somebody else's supervisor. The reachability walk exists for
the case the role rule does not cover, a task that owns a scope BELOW the project it is being
asked to supervise.

A project has at most one live `execution` edge, whichever initiative or parent it names. Every
other initiative uses a `reference`, and a reference must agree about who the parent is. That is
what stops a shared project from acquiring a second execution parent.

## The transaction protocol

Every write path validates completely before its first mutation:

```python
with self.store.transaction() as db:
    ...every competition, role, scope and cycle read...
    if problem is not None:
        self._record_conflict_in(db, problem)   # the only write this transaction makes
        refusal = LinkageError(problem.reason, problem.detail)
    else:
        refusal = None
        ...every mutation...
if refusal is not None:
    raise refusal
```

One transaction commits either the conflict row alone or the whole operation, never both and
never part of one. A refused operation therefore leaves no state, and the contest it lost is
still durable, because the conflict is written inside the transaction that decided it rather
than in a second one after a rollback.

This is why `binding_plan` is separate from `apply_binding_plan`. An operation that binds two
scopes and then links them has to be able to refuse on the second scope without having written
the first. With the decision and the write folded together, a refused supervision committed its
supervisor binding alongside the conflict row: the supervisor owned an initiative no accepted
operation ever created. That was measured, not predicted, and
`test_a_refused_supervision_leaves_neither_binding_behind` pins it.

`linkage_conflicts` carries a logical unique key and is written with `ON CONFLICT DO UPDATE`,
so a losing caller that retries converges on one row instead of accumulating one per attempt.
`incumbent` and `challenger` are `NOT NULL` because SQLite treats NULLs as distinct in a
unique index.

## Attachment is one operation

`attach_in` is the only path that writes the lower level, and its idempotence is computed over
all THREE of its facts: the `relationship_scope` row, the child binding, and the project to
issue edge. A relationship attached by an older writer, or by a run that stopped between two of
them, is completed rather than reported as already done.

`registry.register(project_key=...)` calls it inside the transaction that inserts the
relationship, so an assignment and its whole lower level are one atomic fact. An assignment that
already exists takes a separate path that attaches only, because the insert statements below it
are unconditional and falling through would collide on its own primary key.

Assignment lifecycle moves with it. `registry._write_status`, `resume`, `supersede` and the
`supersedes` branch of `register` each call `apply_relationship_status_in` inside the
transaction they already own, so archiving an assignment releases its issue scope and resuming
reclaims it. It is an explicit no-op for an assignment with no recorded project, which is every
assignment registered before this existed.

## Handover

Replacing a scope's owner requires restating both the outgoing owner and the outstanding work,
which is `registry.resume`'s discipline. The outstanding set is derived through
`AssignmentView.state()` rather than by asking whether a merge mark exists, because a mark
counts only where it matches the CURRENT head event, generation and revision: a mark left from
an earlier generation would otherwise hide work that is still unfinished. A caller that has not
read the outstanding work cannot produce the list, which is the point.

`linkage-outstanding` prints exactly the set `--acknowledge` has to equal.

### What a handover refuses to do

It moves the scope. It refuses outright while the scope still has unfinished work, and says
which assignments it could not move.

An assignment's identity is `sha256(parentTaskId|childTaskId|issueKey)` and its queued
deliveries name the parent's thread. So a handover cannot carry that endpoint across: rewriting
`parent_task_id` in place would leave a row whose stored identity no longer derives from its
own columns, and a replacement parent that cannot receive the work it just accepted. Both were
tried and both were wrong.

Each assignment is moved by registering its successor with `supersedes`, which mints a correct
new identity, carries the new parent's host and cwd, and repoints the project-to-issue edge.

Handing a project BACK is the case where "a correct new identity" is not available. The id is
`sha256(parentTaskId|childTaskId|issueKey)`, so returning to an earlier parent with the same
child derives the id that parent already used. That is not a collision: the same triple is the
same relationship, and the return is a second TENURE of it, recorded as a new generation with
`supersedes` and `supersededBy` carrying the lineage in both directions. Registering a dead
identity without naming a live predecessor is refused rather than answered with the retired
row, and the refusal says whether to pass `supersedes` or to use `relationship-resume`.
Once no live assignment still names the parent that is stepping down, the scope changes hands
and nothing is left answering to it.

That is the condition, and it is not "no unfinished work". Work re-registered under the
INCOMING parent may still be unfinished and no longer blocks, because it already names the
parent taking over. `linkage-outstanding --project` prints the PROJECT's unfinished set rather
than one parent's, so work inherited from a previous parent still counts — and that set is
frequently not empty at the moment a valid handover runs. `--acknowledge` has to equal
whatever it prints then: restating it is the requirement, emptying it is not. Passing less is
`handover_unconfirmed`, and waiting for every assignment to finish first is not required.
`test_the_escape_route_a_stranding_refusal_prescribes_is_reachable` walks that sequence.

The acknowledgement is still required and still means what the acceptance criterion asks: the
incoming owner restates the outgoing owner and the exact unfinished set, so nothing is taken
over silently. It is now a check the caller passes on the way to a refusal that explains the
rest, rather than a licence to proceed.

## Where an instruction stands

A supervisor's instruction to a project is recorded by digest (`linkage-directive`), and with
`--purpose` its envelope pointer says what it is for. The purpose decides what the instruction
competes with (`envelope.directive_place`). Reading every two live instructions of different
digests as contradictory made a relayed decision contest the assignment it was answering
inside; on the installed relay that read as `instruction_conflict` for both of a supervisor's
projects and held every report they owed upward for half an hour (CRW-124 G1, F-G1-2; CRW-230).

| Purpose | Relation to the others |
|---|---|
| `project_assignment` | Sole: never two different live ones per scope. A later version replaces it explicitly |
| `scope_correction` | Sole: never two different live ones per scope, whether or not they answer a message. It amends the assignment and stands beside it; a later correction replaces it explicitly |
| `relayed_decision` | Stands beside the assignment and the correction. Two answering the same message with different digests compete |
| `midpoint_check`, `resume`, `user_stop` | Stand beside everything. Two answering the same message with different digests compete |
| none recorded | Cannot be placed, so it competes with every live instruction of another digest: the rule every row followed before |

Two live instructions contest when their digests differ and they claim the same place: the same
sole purpose, the same purpose answering the same message, or either with no recorded purpose.
The same instruction restated is not a contest: one digest is one instruction. After a handover a
successor re-issuing it records its own row on the new link revision (the id carries the revision
so the two stay distinguishable, see Identity), and the two rows stand together as the same
instruction said twice; a successor issuing a DIFFERENT one is refused and names the
predecessor's row.
Replacing one is always explicit: settle the one in force `superseded` (`linkage-settle`),
restate in the new instruction whatever of it still applies, and record the new one. `chosen`
also takes a directive out of the live set, but it records the winner of a contest rather than
a replacement. Two corrections are not merged, because the store keeps no order between them
that a parent could apply: directives are read in recorded order and a tie falls to the id.

Priority is guidance for the parent and the store never reads it: a user stop comes first; the
live correction amends the live assignment; a relayed decision applies within both; a
midpoint check or a resume asks for an answer or a restart and changes nothing about the work.

An instruction recorded WITH a purpose is placed where it is recorded. One that would contest a
live instruction is refused `link_conflict` before anything is written, naming the one in force
(its id, purpose, answered message, origin, link revision and recording time) and the
`linkage-settle` that replaces it, and the contest is retained in `linkage_conflicts` with both
directive ids, so a retry converges on one row. An instruction recorded WITHOUT a purpose cannot
say what it competes with, so it is recorded as it always was and any contest it makes is
reported by the walks as `instruction_conflict`, which holds the reports above it and is named
in `supervisor-standing` ([supervisor channel](supervisor-channel.md#a-report-the-hierarchy-is-holding)).
A previous link revision's instruction counts like any other: a handover does not dispose of
what the outgoing supervisor said, and the successor settles it when it replaces it.

One limit stays. A directive's id is derived from its scope, origin, digest and link revision,
so an instruction recorded without a purpose cannot be recorded again with one under the same
digest: the second record is the same id, and the pointer check refuses it whether or not the
first has been settled. Record the purposed instruction under a digest of its own (one computed
over the purpose and the text, for example), after settling the old row if it is to stop being
live.

## Reading

`up()`, `down()` and `counterpart()` return a record and never raise for an absence, and they
separate three answers:

| | meaning |
|---|---|
| `readable: true`, something found | the hierarchy, as recorded |
| `readable: true`, nothing found | the store answered, and there is nothing |
| `readable: true`, state `ambiguous` | the store answered with more than one candidate, and the reader will not choose between them. The candidates are returned |
| `readable: false` | the store did not answer |

The last is never reported as the second, and neither is reported as completion. An unreadable
answer carries empty `levels`, `gaps` and `contention`, because a store that could not be read
has no findings to report. This is the shape `intent.dispatch_generation_state` already uses to
separate stale from absent.

Four things make an answer ambiguous. One scope pair joined by several live edges, and one
message whose two endpoints can be paired in more than one way because a task keeps the
bindings of scopes it used to hold: `counterpart()` reports both in `candidates`, each entry
naming a `linkId`, its `kind` and the counterpart `scopeKey`. One scope with several live
execution edges into it: `up()` reports `competing_parents` and stops the walk there rather
than following the highest revision, and `down()` reports it too, separating the chain it is
walking from the scopes it has already visited so that a second parent is not mistaken for a
cycle. And one scope with two live owners, which only a store whose `scope_bindings` unique
index could not be installed can hold: that level's `owner` is null, `competing_owners` names
both tasks, and it is not counted as a gap, because two owners is not nobody.

Quoting `from_scope` or `quoted_scope` resolves the message cases, which is what OPS-7.4
already says a message carries. Nothing resolves the store cases except repairing the store;
the reader's job is to say so rather than to pick.

`owner_drift` is reported at BOTH ends of an edge, by both walks. It is not only a corruption
symptom: the sequence a stranding refusal prescribes passes through it on purpose, because each
assignment is moved to the incoming parent before the scope is, and during that window the
project-to-issue edge names the new parent while the project binding still names the old one.
A walk that checked one end called that window internally consistent.

When the store could not install one of its partial unique indexes, `linkage-up`,
`linkage-down` and `status` carry `unenforcedIndexes` naming it. An ambiguous answer and a
missing index are the same fact from two sides: the second says the database is no longer
refusing a second owner either, which is what a caller deciding whether to act on a contested
scope has to know. A healthy store carries no such key.

`linkage-up` takes exactly one of `--task`, `--issue` or `--relationship`, and `--scope` only
with `--task`. Naming several used to resolve by an internal precedence the caller never saw.

`gaps` name what is missing instead of omitting the level: `initiative_without_supervisor`,
`project_without_parent`, `issue_without_child`, `unscoped_assignment` and `no_supervisor`.
An unscoped assignment is the compatibility case and is reported, never dropped.

`counterpart()` findings are independent, so one message can carry several: `wrong_role`,
`foreign_scope`, `foreign_sender_scope`, `stale_owner` with `currentOwner`, `stale_sender`,
`stale_revision`, `owner_drift`, `instruction_conflict`, `unregistered_link` and
`link_contention`, plus `competing_owners` when the scope it would name has two live holders.
`link_contention` accompanies state `ambiguous`, and answering `unlinked` instead would deny
a linkage that demonstrably exists.

## Refusals

| Reason | Means |
|---|---|
| `unregistered_scope` | the scope, or the link a caller named, does not exist or is not live |
| `scope_role_mismatch` | a task already holds another role, or a pair may not address each other |
| `scope_cycle` | a self-link at either edge, or a chain that reaches itself |
| `foreign_scope` | an issue whose parent does not own the project, an issue already scoped elsewhere, or an assignment reactivated into a project its parent no longer holds |
| `duplicate_scope_owner` | a second owner for one scope, or a second execution edge for one project |
| `role_already_bound` | one task asked for a second live scope of a role it already holds |
| `handover_unconfirmed` | no evidence, the wrong outgoing owner, or an outstanding set that does not match |
| `handover_would_strand` | live work the handover cannot carry across, named row by row in the refusal |
| `link_conflict` | the same link or binding asserted with different endpoints or another host; a supervising initiative also referencing its own project; a settled directive decided again differently; a purposed directive that would contest a live one for its place ([Where an instruction stands](#where-an-instruction-stands)) |
| `link_not_active` | a status or disposition outside its vocabulary |

## What this is not

It registers and queries. It delivers no message: a peer link is not a channel, and
`scope_directives` records that an instruction exists and its digest rather than carrying one.
Delivery, acknowledgement and merge order between parents belong to CRW-122 and CRW-123.

Nothing here is evidence about an installed runtime. A green suite in this repository says the
source builds, imports and behaves as its tests describe; it says nothing about any host.
