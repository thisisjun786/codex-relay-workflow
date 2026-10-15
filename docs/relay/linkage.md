# Three-level linkage

Relay-owned records, not contract records. The five schemas under
`contract/schema/` (`src/codex_session_relay/schema/` until todo 44) are byte copies of the frozen cross-session communication
contract and none of them describes anything on this page. Nothing here is validated against
them, and nothing here changed them.

The sessions in this document are named by [the level table](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels): the parent is the repository session, the child is the project session, and a `child` session (a code id, not a layer) is no layer. The role ids that the commands, flags and stored rows keep (`supervisor`, `parent` and `child`) are read through [Names used only in code](../../plugins/crw/skills/crw-canon/references/hierarchy.md#names-used-only-in-code).

## Why these are separate tables

`relationships` binds the task of one child to the task of one `child` session for one issue. That is the whole of the
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
children registering it from opposite ends have to converge on one record rather than on two
mirror images.

A **directive** carries the revision of the link it arrived on, which is the one identity here
that deliberately does NOT converge across a handover. Replaying an instruction to the same
scope is meant to land on the same record, and a replacement parent re-issuing a
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
task cannot be both a child and somebody else's parent. The reachability walk exists for
the case the role rule does not cover, a task that owns a scope BELOW the project it is being
asked to supervise.

A project has at most one live `execution` edge, whichever initiative or parent it names. Every
other initiative uses a `reference`, and a reference must agree about who the child of the project is. That is
what stops a shared project from acquiring a second execution parent.

## The store seat

The `supervisor` role's own level is an initiative.
`linkage-bind --role supervisor --scope-kind store --scope store` claims the store instead: one key, one live owner, and the same identity rule, so
the binding is `bindingID("supervisor","store","store",task)`. The store is the one scope no
Linear level owns, so a task bound there holds a seat that is not an initiative execution
binding. `--scope-kind` is accepted for that one combination and no other: another role, or
another key, is a named refusal rather than a second scope, and the seat writes no `scope_links`
row, because a supervision edge joins an initiative to a project and the store has no initiative
above it.

The seat is for a session that manages the store rather than one project. The parent (repository session, [the level table](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels)) holds it, and LINA holds the same seat the same way — a task with one parent binding at the
store — so neither is tied to a single initiative's execution. One task holds one such
seat, so a session registered here cannot also supervise an initiative; that is the rule the role
model already had, not a limit of the command. The seat is also what such a session needs to
speak for the store scope: `limit-declare --scope-kind store` reads its declarer from a live
parent binding, and the seat is the binding a session that manages the store holds.

The seat changes hands like any other:
`linkage-handover --role supervisor --scope-kind store --scope store --expect-task <incumbent> --task <successor> ...` archives the incumbent's binding
and binds the successor. The scope kind is the same `--scope-kind` decision `linkage-bind` makes,
so it is accepted for the parent's store seat and no other combination; the handover rules are
the ordinary ones, which is what makes the seat recoverable when the parent is
replaced. `--expect-task` must name the live owner, and a successor that already supervises an
initiative is refused `role_already_bound`, because one task holds one role. Without
`--scope-kind` the handover takes the role's own level as it always did, so
`--role supervisor --scope store` still looks for an initiative named `store` and refuses `unregistered_scope`.

## Management seats

A parent holds a seat of its own:
`linkage-bind --role supervisor --scope-kind management --scope <seat id> --task <thread>`
binds role `supervisor` at scope kind `management`,
keyed by the seat id, which is the thread that first registered it; `task_id` names the seat's
current owner. Two parents have two keys, so neither overwrites the other, and a third
task asking for a seat another one holds is refused `duplicate_scope_owner`. `crw manage supervisor register`
is that line for the calling thread (`--task`, else `CODEX_THREAD_ID`, else the
`supervisor` section of the configuration; `--host`, else the section) followed by the settings record.
A thread that inherited a seat by handover and registers its own seat again (`--scope` its own
thread) is answered with the seat it holds; one task holds one such seat. A thread that
holds the store seat lays it down when it takes its management seat: the store binding is archived
with `superseded_by` naming the management binding, and a `scope_seat_moved` journal row records
the move, in the same transaction. The store scope's capacity has one declarer
(`limit-declare --scope-kind store`, and `usage-observe` at the store scope): the holder
`StoreScopeSupervisor` answers, that is the live store seat or the management seat that took it
over. Any other live management seat holder still states a store-wide figure, because a declaration
is reversible and fails open, but the answer warns under `declarerWarning` (`reason`
`scope_role_mismatch`, `declaredBy`, `storeScopeSupervisor`, `detail`) and a
`store_scope_declarer_warned` journal row records it; a task with no live store or management seat
is refused `scope_role_mismatch`. Every reader of the store seat reads a seat laid down
this way through the management seat that took it over: with no live store binding, the store
seat's holder is the live owner of the management seat that superseded the most recently archived
store binding, so it follows that seat's handovers. A user-grade or delegated-management-grade
`decision-answer` on a record with no management ref (see below), the recipient of a
`jun_decision_request` and the capacity declarer keep finding the parent after it registers. The
supervisor channel's store-seat fallback does so only for a message whose decision the user owes
(CRW-1486, [grades](supervisor-channel.md#the-level-above-a-project)): a completion, a block, a
report or an escalation of a project or an initiative no management edge stands above finds nobody
above (`unregistered_scope`) instead of reaching the parent through the seat it laid down, and one
staged while the seat was live is held `hierarchy_unresolved` until an edge or a live store seat
names someone. A parent hears its own projects through the edges it writes when it binds their
children (below). A store
seat bound again afterwards is live and answers for itself; the thread that laid it down binding it
again after handing its management seat over reactivates its old row with `superseded_by` cleared
(`scope_rebound`), so that row is the store seat again.

A lead (the `supervisor` role at an initiative) registered by a thread that holds a live management seat gets the
execution edge from that seat to its initiative in the same transaction:
`linkage-bind --role supervisor --scope <INIT> --task <lead> --host ... --creator <management thread>`, where
`--creator` defaults to the caller's `CODEX_THREAD_ID`. The answer is the lead's binding with the
edge under `managementLink`. A creator that holds no management seat (a lead registering itself)
writes no edge. An initiative has one upward execution edge, and a second parent takes
an initiative only by the handover of the seat above it. A registration is reversible, so one whose
creator holds another seat is not refused: the lead's binding stands, the standing edge stays, no
second edge is written, the answer carries the warning under `managementLinkSkipped` (`reason`
`link_conflict`, `detail`, the `standing` link id, the creator's seat and the creator) and a
`management_link_skipped` journal row records it once. The edge's
link id is made from the two scopes, so
`linkage-handover --role supervisor --scope-kind management --scope <seat id> --expect-task <old owner> --task <new owner> --evidence ...`
keeps the edge and
moves its `upper_task_id` to the new owner, archives the old binding (`superseded_by`), gives the new
one the evidence as `handover_note`, and writes `scope_handover`.

On the 1.0 path the sessions are two layers, the parent and the child (project session, [the level table](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels)),
and a parent binds its child itself. A child (role `parent` at its project) bound by a thread
that holds a live management seat gets the execution edge from that seat to the project in the same
transaction: `linkage-bind --role parent --scope <PROJECT> --task <owner> --host ... --creator
<management thread>`, `--creator` again defaulting to `CODEX_THREAD_ID`, answered with the edge under
`managementLink`. The rules are the initiative's: a creator with no management seat (a child binding
itself) writes no edge; a project has one upward execution edge, so a binding whose creator holds
another seat writes no second edge and is warned and recorded the same way (`managementLinkSkipped`,
`management_link_skipped`), not refused; another child is refused by the ordinary duplicate-owner rule,
which is the store's own invariant (one live owner per scope and role); and the seat's handover moves
the edge's `upper_task_id` with the
`scope_handover` record, as a child's handover moves its `lower_task_id`. A project a
lead already supervises (the legacy configuration, `linkage-supervise`) keeps that edge:
the binding is not refused, no management edge is written, the answer carries the warning under
`managementLinkSkipped` (`reason` `link_conflict`, `detail`, the `standing` link id, the seat and the
creator) and a `management_link_skipped` journal row records it once. In the other direction
`linkage-supervise` refuses an initiative over a project a management edge already stands above
(`duplicate_scope_owner`, naming the management seat), because the project has its execution
parent. A thread that holds a management seat and still keeps a legacy initiative over a project
therefore runs `linkage-supervise` before it binds the child: the binding then keeps the
initiative's edge with the skip warning.

The walk up from an issue or a project reaches whatever stands above the project. A management level
directly above the project is the recipient of the project's upward messages: a relationship's
report, a `project:<key>` notice and the direct project kinds go to the owner of that seat (see [the
supervisor channel](supervisor-channel.md#the-level-above-a-project)). Without one, a legacy
lead is the recipient, and without that the single store seat (a store seat laid
down for a management seat only for a decision the user owes, above). A management level above an
initiative is not a project's recipient: the lead stands between and stays the recipient of the
project's reports, while the lead's own messages go to the management seat through
`initiative:<key>` (see [direct messages](supervisor-channel.md#direct-messages)). An initiative with
no management edge falls back to the single store seat, on the same terms. No schema changes: the seats and edges are
new values in `scope_bindings.scope_kind` and `scope_links.upper_kind`, which carry no CHECK.

The answer to a decision reserved for Jun follows the same seat (CRW-1371). A user-grade or
delegated-management-grade `decision-raise` names in the record's `authority.ref` the management
seat whose live execution edge stands directly above the origin project (`--origin-project`, read
as a project's key and, when no edge stands above that key, as a flow plan's id through the project
its header names, which is how `crw manage batch decide` raises). The ref is the seat's key, not its
owner. `decision-answer` accepts such a record's answer only from the seat's current owner: the
successor after a handover, and nobody while the seat has no live owner, each other answerer refused
`authority_unverified`; an answer that cites the class without a ref takes the record's. A record
without a management ref (a project with no management edge, or a row raised before the edge stood)
keeps the store seat's rule above. The grade and via rules are unchanged, and a parent-class ref is
the raiser's. What the raise cannot settle is not refused: a ref the raiser named that is not the
seat is replaced by the seat, and a seat that cannot be read or two seats above one project leave
the authority as given; each is answered under `authorityWarning` (`reason` `ref_replaced`,
`seat_unreadable` or `seat_contested`) and recorded by one `decision_authority_warning` journal row.
With no management edge above the project, a ref the raiser named that is a known management seat is
kept, so that seat's owner answers and the store seat does not; it is warned (`ref_without_edge`) and
journaled the same way (CRW-1378). No schema change: the seat is a value of the record's existing
authority object.

A repeated question folds into the open record of its fingerprint only when that record is held
under the same seat (the same `authority.ref` of a user-grade or delegated-management-grade record,
or none). Raised under another management seat it is a record of its own, with that seat as its ref,
and the answer carries `foldWarning` (`reason` `seat_differs`, the record it did not fold into),
journaled as a `decision_authority_warning` row (CRW-1378). The warning's detail names the seat the
open record is held under and the rule that answers this raise: its own seat's owner, the store
seat's rule for a user-grade or delegated-management-grade question with no seat above its project,
or the raiser's own authority for a parent-class question (CRW-1390). A record raised before this
rule reads and answers as it did.

**A record whose seat has no owner** - the parent that held the seat ended or released
it, so `decision-answer` refuses every answerer `authority_unverified` - is recovered in one of three
ways, none of which edits the record:

1. a new parent thread binds the seat's key again:
   `linkage-bind --role supervisor --scope-kind management --scope <seat id> --task <new thread> --host ...`; the seat's new owner answers;
2. while the old parent's binding is still live,
   `linkage-handover --role supervisor --scope-kind management --scope <seat id> --expect-task <old owner> --task <new thread> --evidence ...` moves
   the seat, and its edges, to the new thread, which then answers;
3. the question is withdrawn (`decision-withdraw --decision ID --reason ...`) and raised again from
   the project: the new record names the seat that stands above the project then, or none (the store
   seat's rule) when no edge does.

The parent's procedure over these records - registering its seat, binding the child of each project it
starts, handing the seat over - is
[the parent's section of crw-run](../../plugins/crw/skills/crw-run/SKILL.md#hold-the-seat-bind-the-children-hand-the-seat-over);
what the project's messages then reach is
[the level above a project](supervisor-channel.md#the-level-above-a-project), and what a flow run's
reports record of their delivery is [reporting](../flow/reporting.md#through-the-supervisor-channel).

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
lead binding alongside the conflict row: the lead owned an initiative no accepted
operation ever created. That was measured, not predicted, and
`test_a_refused_supervision_leaves_neither_binding_behind` pins it.

`linkage_conflicts` carries a logical unique key and is written with `ON CONFLICT DO UPDATE`,
so a losing caller that retries converges on one row instead of accumulating one per attempt.
`incumbent` and `challenger` are `NOT NULL` because SQLite treats NULLs as distinct in a
unique index.

## Attachment is one operation

`attach_in` is the only path that writes the lower level, and its idempotence is computed over
all THREE of its facts: the `relationship_scope` row, the `child` session's binding, and the project to
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
deliveries name the child's thread. So a handover cannot carry that endpoint across: rewriting
`parent_task_id` in place would leave a row whose stored identity no longer derives from its
own columns, and a replacement child that cannot receive the work it just accepted. Both were
tried and both were wrong.

Each assignment is moved by registering its successor with `supersedes`, which mints a correct
new identity, carries the new child's host and cwd, and repoints the project-to-issue edge.

Merged work is the exception, and it is closed rather than moved. An assignment is *settled* when it reads
`merged` (its merge mark matches its current head, generation and revision, and the criteria are still current) and
nothing of it is still owed: no delivery of it that has not reached the child (`queued`, `deferred_busy`,
`withheld_pre_send`, `sending`, `held_uncertain` or `inbox_only`), no message of it on the supervisor channel that has not left (the same
states without `inbox_only`), and, where it is the execution of a plan node, an active acceptance of this very head under
the criteria now in force. A handover does not refuse for a settled row. It classifies the attached rows first, refuses on
the others exactly as before, and only when every refusal check has passed (the binding plan included) archives each
settled row that does not name the incoming child, in the same transaction and before any binding or link moves. The row
is journalled as `status_changed` with the reason `merged_settled`, and the answer lists the ids as `closedMerged` (the
key is absent when none was closed). A row left active would keep naming the child that stepped down and could reopen
under it. Archived, it comes back only through `relationship-resume`, which is refused `foreign_scope` once the project
has changed hands, so a fix after a handover is a new assignment under the current child (the issue is free).
`Attached` and `Outstanding` keep their meaning: `linkage-completion` reads `attached` as the live assignments, so a
project whose assignments are all merged still reads `complete_candidate` until they are closed.

Handing a project BACK is the case where "a correct new identity" is not available. The id is
`sha256(parentTaskId|childTaskId|issueKey)`, so returning to an earlier child with the same
`child` session derives the id that child already used. That is not a collision: the same triple is the
same relationship, and the return is a second TENURE of it, recorded as a new generation with
`supersedes` and `supersededBy` carrying the lineage in both directions. Registering a dead
identity without naming a live predecessor is refused rather than answered with the retired
row, and the refusal says whether to pass `supersedes` or to use `relationship-resume`.
Once no live assignment still names the child that is stepping down, the scope changes hands
and nothing is left answering to it.

That is the condition, and it is not "no unfinished work". Work re-registered under the
INCOMING child may still be unfinished and no longer blocks, because it already names the
child taking over. `linkage-outstanding --project` prints the PROJECT's unfinished set rather
than one child's, so work inherited from a previous child still counts — and that set is
frequently not empty at the moment a valid handover runs. `--acknowledge` has to equal
whatever it prints then: restating it is the requirement, emptying it is not. Passing less is
`handover_unconfirmed`, and waiting for every assignment to finish first is not required.
`test_the_escape_route_a_stranding_refusal_prescribes_is_reachable` walks that sequence.

The acknowledgement is still required and still means what the acceptance criterion asks: the
incoming owner restates the outgoing owner and the exact unfinished set, so nothing is taken
over silently. It is now a check the caller passes on the way to a refusal that explains the
rest, rather than a licence to proceed.

## Closing merged assignments

`relationship-close-merged (--project KEY | --all) --actor ID [--apply]` archives the live assignments that are settled
(see Handover) and no others. Without `--apply` it only reads: it never creates a store and writes no relationship, link,
binding or journal row (opening a store this runtime may write runs the idempotent settlement backfill that every
writable open runs). The answer names `closable` (what is settled), `closed` (what `--apply` archived; empty without it)
and `kept` (every other live assignment, with its state word and why it was left: not merged, or what is still owed).
With `--apply` the decision and the writes share one transaction, so a mark that stopped counting meanwhile (a new
generation, a new head, changed criteria) is not closed, and a second `--apply` closes nothing. `--project` takes the
project's assignments; an assignment with no project is reached only by `--all`.

A closed assignment reads `closed` and keeps its merge mark in `assignment-show`. Its issue scope is released, so the
issue is free for a new assignment. The way back is `relationship-resume`, which restates the generation, the roots and
the recipients; the row then reads `merged` again and `generation-open` reopens it for a fix. While it is closed
`generation-open` is refused `relationship_not_active`. `linkage-completion` reads `unregistered` once nothing in a
project is live, which says that nothing is attached rather than that everything finished, so it is read first and kept as
the completion evidence.

What the registry cannot decide is a completion report of a supervised project that was never staged. The `supervisor`
package derives it from the events and keeps it standing when the project has no coordination target, so it cannot be told
from a report nobody will ever stage. It stays listed by `supervisor-standing` after the close, but staging and sending it
are refused `unregistered_scope`. For a supervised project `supervisor-standing --project` is read, and neither a bulk apply nor a handover runs while
an entry for an assignment about to be closed (its `relationId`) lacks a `decision.priorReport`; entries of assignments already
closed are history and a discharged obligation is not listed, so neither blocks, and a project with nothing above it stages
nothing, so there the list is not a stop condition.

## Where an instruction stands

An instruction to a project from above is recorded by digest (`linkage-directive`), and with
`--purpose` its envelope pointer says what it is for. The purpose decides what the instruction
competes with (`envelope.directive_place`). Reading every two live instructions of different
digests as contradictory made a relayed decision contest the assignment it was answering
inside; on the installed relay that read as `instruction_conflict` for both of one parent's
projects and held every report they owed upward for half an hour (CRW-124 G1, F-G1-2; CRW-230).

| Purpose | Relation to the others |
|---|---|
| `project_assignment` | Sole: one live per scope. A later one replaces it explicitly |
| `scope_correction` | Sole: one live per scope, whether or not it answers a message. It amends the assignment and stands beside it; a later correction replaces it explicitly |
| `relayed_decision` | Stands beside the assignment and the correction. Two answering the same message with different digests compete |
| `midpoint_check`, `resume`, `user_stop` | Stand beside everything. Two answering the same message with different digests compete |
| none recorded | Cannot be placed, so it competes with every live instruction of another digest: the rule every row followed before |

Two live instructions contest when their digests differ and they claim the same place: the same
sole purpose, the same purpose answering the same message, or either with no recorded purpose.
Every place holds one live row. The same instruction restated is not a contest - one digest is
one instruction - but after a handover it would reach the store as a new row, because the id
carries the link revision (see Identity). A purposed record of it is therefore refused as already
in force: the predecessor's row stays live through the handover, so nothing needs recording, and a
second live copy would leave a later replacement settling one of two. A successor that wants it
in its own name settles the predecessor's row superseded first. A different instruction from the
successor is refused like any competitor and names the predecessor's row. Reading a pair an older
writer left, the walks treat one digest as one instruction and hold nothing for it.
Replacing one is always explicit: settle the one in force `superseded` (`linkage-settle`),
restate in the new instruction whatever of it still applies, and record the new one. `chosen`
also takes a directive out of the live set, but it records the winner of a contest rather than
a replacement. Two corrections are not merged, because the store keeps no order between them
that the receiving session could apply: directives are read in recorded order and a tie falls to the id.

Priority is guidance for the session that receives the instruction and the store never reads it: a user stop comes first; the
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
what the outgoing owner of the edge said, and the successor settles it when it replaces it.

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
assignment is moved to the incoming child before the scope is, and during that window the
project-to-issue edge names the new child while the project binding still names the old one.
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
| `foreign_scope` | an issue whose assigning child does not own the project, an issue already scoped elsewhere, or an assignment reactivated into a project its child no longer holds |
| `duplicate_scope_owner` | a second owner for one scope, or a second execution edge for one project |
| `role_already_bound` | one task asked for a second live scope of a role it already holds |
| `handover_unconfirmed` | no evidence, the wrong outgoing owner, or an outstanding set that does not match |
| `handover_would_strand` | live work the handover cannot carry across, named row by row in the refusal. A merged assignment with nothing owed is closed by the handover and is not named |
| `link_conflict` | the same link or binding asserted with different endpoints or another host; a supervising initiative also referencing its own project; a settled directive decided again differently; a purposed directive that would contest a live one for its place ([Where an instruction stands](#where-an-instruction-stands)) |
| `link_not_active` | a status or disposition outside its vocabulary |

## What this is not

It registers and queries. It delivers no message: a peer link is not a channel, and
`scope_directives` records that an instruction exists and its digest rather than carrying one.
Delivery, acknowledgement and merge order between children belong to CRW-122 and CRW-123.

Nothing here is evidence about an installed runtime. A green suite in this repository says the
source builds, imports and behaves as its tests describe; it says nothing about any host.
