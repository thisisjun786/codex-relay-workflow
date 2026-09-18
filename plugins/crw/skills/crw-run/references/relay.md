# codex-session-relay

Read when an assignment is actually HELD BY A RELAY. Ordinary `crw-run` does not need any of
this: without a relay the existing dispatch, review, and correction rules stand unchanged, and
nothing below becomes a prerequisite.

`codex-session-relay` is a separate installed package that records one issue's assignment durably:
the relationship, its execution generations, each delivery attempt, the parent's acknowledgement
and verdict, and the coordination summary owed to a Linear document. It owns none of that
workflow's authority. It does not read or write any task's CXC state, and CXC startup and phase
procedure stay with the installed `cxc-loop` and `cxc-pabcd` skills; this reference never restates
them.

Verified against `codex-session-relay` 0.1.0 on 2026-09-15: the commands, their arguments, the
refusal reasons quoted here and the record shapes were read from that installed package and
exercised against temporary stores. A different installed version may not share them, so check
the version before relying on a refusal reason or a field name.

Global options come BEFORE the subcommand:

    codex-session-relay [--state DIR] [--socket PATH] <command> [options]

Every command prints JSON. Exit 0 success, 2 a refusal carrying a machine-readable `reason`,
3 a host problem, 4 usage.

## Discover capability before relying on it

    codex-session-relay --state "$RELAY_STATE" doctor

`doctor` reports `actorReachability`: whether the state directory is writable, whether a socket is
configured, and what connecting to it actually returned. Read it from the process that will run
the work. What a given task can do is a property of that task's profile on that host, not a fixed
fact about the relay, so discover it rather than assuming it.

Its `offlineCommands` and `hostRequiredCommands` lists are curated capability GUIDANCE, not an
exhaustive index. `hostRequiredCommands` is the authoritative set; `offlineCommands` currently
omits `assignment-find`, `assignment-mark` and all ten synchronisation commands, every one of
which works from the store alone. Treat anything outside `hostRequiredCommands` as store-only.

On the host and profile measured during JUN-92, a workspace-write task could not write the default
state directory under the user state home and could not connect to the App Server control socket,
which is why the split below existed there. Reuse that as a recorded observation, not as a rule
that holds everywhere.

## One shared state directory

Every process in one assignment must pass the same `--state`. The child emitting, the parent
verifying, and whichever process delivers all read one store; a mismatched `--state` simply means
they do not see each other.

There are TWO selectors and they are set separately. `--state` chooses the relay's own store, and
falls back to `state_dir()` when omitted. The adapter's ledger, which is where duplicate
suppression and delivery recovery live, always comes from `state_dir()`: it reads
`CODEX_SESSION_RELAY_STATE`, and otherwise `$XDG_STATE_HOME/codex-session-relay` or
`~/.local/state/codex-session-relay/<endpoint hash>`. So passing only `--state` moves the store
and leaves the ledger behind. Measured: `--state /tmp/assigned` reported that store while the
adapter's root resolved under `~/.local/state`. Recovery then reads a ledger that never saw the
attempts, so it can miss one or repeat a delivery.

Set both to the same resolved absolute path, before any command that reaches the socket:

    export CODEX_SESSION_RELAY_STATE="$RELAY_STATE"
    codex-session-relay --state "$RELAY_STATE" --socket "$SOCK" deliver

A process that cannot set both keeps the assigned store and hands the socket work to one that
can. It never falls back to the default, because that silently opens a second, empty store.

Agree that path up front and put it in the packet. It depends on nothing that task creation
returns, so it can travel in a newly created child's first prompt where a relationship id cannot,
and a participant told only that issue identity resolves the rest with `assignment-find --issue`. The
receipt is emitted when the work is done rather than when the turn starts, so registration has to
land before the child's first emit, not before its first token.

Keep the store OUTSIDE every participant's checkout, somewhere they can all write. It holds
private receipts and generated runtime state, which do not belong in a repository, and a
workspace-write task that cannot write the default state home makes a checkout the tempting
answer. Where no shared writable location outside the checkouts exists, the relay is unavailable
for that assignment rather than relocated into one.

## Who runs what

Exactly five commands reach the App Server and need `--socket`: `deliver`, `reconcile`, `recover`,
`daemon` and `verify-acks`. Every other command works from the store alone. Where a task cannot
reach the socket, it uses the store-only subset and a host-capable process owns delivery.

`ack` sits between the two. It does not REQUIRE a socket, which is why it is not in that list, but
it uses one when given: with `--socket` and a reachable host it verifies the acknowledging turn
immediately, and without one it records the parent's intent for `verify-acks` to complete later.

## Register the assignment

    codex-session-relay --state "$RELAY_STATE" register \
      --parent-task <coordinator task id> --parent-host <host id> \
      --child-task <child task id> --child-host <host id> \
      --issue <globally stable issue identity> --scope-ref <canonical workspace/project document> \
      --artifact-root /abs/path \
      --allowed-recipient <parent task id> --allowed-recipient <child task id> \
      --dispatch-request-id <stable id> --dispatch-turn-id <the dispatch turn> \
      --parent-settings @parent.json --child-settings @child.json

Name BOTH sides as allowed recipients: completions travel to the parent and corrections travel
back to the child, and a `needs_changes` verdict is refused when the child is not an allowed
recipient.

`--issue` is the assignment's identity, not a label. `assignment-find --issue` matches that exact
string, the duplicate-assignment guard keys on it, and the relationship id is derived from it with
the two task ids. A displayed key like `JUN-00` is unique inside one workspace and not across two,
so one store serving two workspaces would conflate them and refuse the second legitimate child.
Pass an identifier that is stable and unique across every workspace the store serves, and record
the workspace or project it belongs to in `--scope-ref`, which the relationship keeps and reports.
For a standalone issue, the scope reference names its workspace and stable issue/document
URL instead of a project. Project membership is absent, not fabricated; the real coordinator
and child IDs and both allowed recipients remain required. `--scope-ref` is descriptive
context, not a checked Linear project binding.
Use that same issue string everywhere afterwards, including in the packet the child is given: a lookup
by a different spelling of the same issue finds nothing, and a child that follows it reports a
perfectly good completion as UNEMITTED.

BOTH sides' authorized execution settings come from the actual host creation receipts the
coordinator already holds. A settings record has seven required fields:

    {"sandbox": {...}, "approvalPolicy": "...", "cwd": "/abs/path",
     "runtimeWorkspaceRoots": ["/abs/path"], "model": "...", "reasoningEffort": "...",
     "environments": [...]}

Six sit at the top level of the creation response. `environments` does not: on the current response
it is nested at `creation.thread.environments`, so read it from there. When the response reports
`activePermissionProfile`, carry it into the record's `expectedPermissionProfile` field, which is
the key a later resume is checked against. Never ask a worker to echo its own settings back, and
never widen a task's permissions to make a later send connect.

Carry that value WHOLE. The later check is object equality against what the resume reports, so a
record holding only the profile's id can never match: the comparison sees an id-shaped object
against the full one and reports `UNVERIFIABLE_PERMISSION_PROFILE` with both sides, on a task
whose permissions never changed at all. Copy the object the response gave you, including fields
that look like they carry nothing, because an absent `extends` and an `extends` that is null are
different objects to an equality check. The bridge offers no help here and is not meant to: it
passes the profile through without interpreting it and leaves it out of the settings it compares,
so it arrives raw at `creation.activePermissionProfile` or `resumed.activePermissionProfile`, and
inside `permissionReceipt` on the worktree path. Measured against the version named above.

`--parent-settings` and `--child-settings` are optional. Leaving them off still registers the
relationship, and either side can be recorded afterwards with
`settings-record --task <id> --settings @file.json`. What is validated is the RECORD, so an
incomplete one is refused when it is written, and a recipient with no record at all has its send
withheld rather than sent under a host default. `settings-show --task <id>` reports what is held
and what is missing.

Registering a different child for an issue that already has an active or paused assignment is
refused with `duplicate_assignment`, inside the same transaction that would have inserted it, so
two concurrent registrations cannot both win. That prevents a duplicate REGISTERED owner. It does
not make native task creation atomically impossible: a task created outside the relay is still a
real task, so look the assignment up before creating anything.

    codex-session-relay --state "$RELAY_STATE" assignment-find --issue <the identity register was given>

returns each relationship for that issue with its child, state, and next expected action, plus
`responsibleChild` when one owns it. A paused assignment still owns its issue.

## Bind the criteria

    codex-session-relay --state "$RELAY_STATE" criteria-register --relationship <rel> \
      --source-ref <canonical document url> \
      --criterion 'c1=the obligation, in words' --criterion 'c2=...' [--optional c2]

This marks the assignment MANAGED, and a managed assignment cannot be completed as `verified`
against no criteria. `criteria-show --relationship <rel>` returns the set and its digest.

## The child emits

    codex-session-relay --state "$RELAY_STATE" emit --relationship <rel> --generation <n> \
      --outcome ready_for_review --turn-thread <own task id> --turn-id <own turn> \
      --artifact /abs/path [--supersedes-revision <hash>]

For non-PR work, `--artifact` is still required. Freeze the result and verification
evidence under an authorized artifact root, including any source and delivered
document identities. A linked document alone has no manifest and cannot produce
a `ready_for_review` receipt. Keep the snapshot private when its source is private.

Without `--socket` the receipt is STAGED: recorded and visible, deliverable only once an
independent observation sees that turn end normally. Staged is real progress; it is not delivery
and a report must not call it one.

`--supersedes-revision` is scoped to ONE generation, because lineage is read per generation. When
re-emitting inside a generation it states which revision this one replaces; without it, two
revisions in one generation are a fork and neither is current. The FIRST receipt of a fresh
generation omits it: a `needs_changes` correction opens the next generation, and naming the
previous generation's revision there declares a predecessor that generation does not contain, which
reads `unknown_predecessor` and leaves the whole generation with no current head.

## A staged receipt needs a host-capable process

Two different things stop a receipt short. This one is delivery. The other is that the assignment
is not in the store yet, which the next section covers.

Staged is not deliverable. Whoever holds the socket moves it, from the same state directory:

    codex-session-relay --state "$RELAY_STATE" --socket "$SOCK" deliver [--event <id>] [--limit 4]
    codex-session-relay --state "$RELAY_STATE" --socket "$SOCK" reconcile --request-id <id>
    codex-session-relay --state "$RELAY_STATE" --socket "$SOCK" recover
    codex-session-relay --state "$RELAY_STATE" --socket "$SOCK" daemon [--max-ticks <n>] [--deadline <s>]

Each of those needs `CODEX_SESSION_RELAY_STATE` exported to the same path, as above, or the
ledger they reconcile against is not the assignment's.

`deliver` takes the eligible receipts, or one named event, and attempts the send. An attempt whose
outcome is uncertain is held rather than retried blindly; `reconcile --request-id` settles that one
attempt from the operation receipt and the recipient's own items, and `recover` does the same sweep
for everything unresolved after a restart. Neither ever sends. `daemon` runs the same loop on a
cadence when a process can stay up.

Until one of those runs, a staged receipt is real progress that the parent cannot claim, and the
child has not failed. This is the split described above: the child emits from the store, a
host-capable process owns delivery.

## When the assignment is not in the store yet

Registration needs the task id that creation returns, so a child which finishes quickly can reach
its completion before the parent's `register` lands. The lookup then answers honestly and
unhelpfully: `assignments: 0` and `responsibleChild: null`. Guessing a relationship id instead is
refused with `unregistered_relationship`.

There is nothing there for the child to fix, and two things it must not do. It preserves the
artifact exactly as produced and records its own task id, the issue identity it was given and the
state directory it
was given, then reports the completion as UNEMITTED. It does not claim a receipt it could not
write, and it does not adopt another relationship or nominate a different owner.

The coordinator recovers it after registering, from that SAME child, in a later turn on that same
task. A turn which is not the generation's anchor is refused with `unassigned_turn` unless it
carries a continuation claim naming the anchor it continues:

    codex-session-relay --state "$RELAY_STATE" emit --relationship <rel> --generation <n> \
      --outcome ready_for_review --turn-thread <own task id> --turn-id <the later turn> \
      --artifact /abs/path --continues-anchor <that generation's dispatch turn> \
      --continuation-actor <own task id> --continuation-reason 'recovered after late registration'

That anchor is checked against the registry, so a claimant which does not know which execution it
is continuing cannot produce one, and naming the wrong anchor is refused the same way. Where an
operator has to record the admission out of band instead, `admit-turn --relationship <rel>
--generation <n> --turn <id> --actor <who> --reason <why>` does it.

Recovery lands the receipt that was always owed rather than a second one. For `ready_for_review`
the event's identity is the relationship, generation, revision and outcome, NOT the turn, so the
recovered receipt carries the same event id and revision hash the on-time emit would have
produced. That is why this is a recovery and not a duplicate writer.

A lookup that returns an assignment whose child is some other task is the other shape of the same
moment, and it is not recoverable this way: a second registration for that issue is refused with
`duplicate_assignment`, and emitting into another child's relationship is refused with
`unassigned_turn`, because the thread check is absolute. Report the conflict with the owner the
lookup actually named, and write nothing into it.

A completed turn carrying no receipt is an ordinary turn end, never success. What a managed session
must record so that an ordinary turn end can be told apart from a finished one, and what a hook may
conclude from a Stop event, are decided in
[Managed marker, completion state, and hook adjudication](hook-contract.md).

## The parent verifies

    codex-session-relay --state "$RELAY_STATE" claim     --event <id> --turn <own turn>
    codex-session-relay --state "$RELAY_STATE" ack-proof --event <id> --turn <own turn>
    codex-session-relay --state "$RELAY_STATE" [--socket "$SOCK"] ack --event <id> --ack-turn <own turn> \
      --ack-proof <proof>
    # only when that ack was offline: a host-capable process completes it before any verdict
    codex-session-relay --state "$RELAY_STATE" --socket "$SOCK" verify-acks [--limit 8]
    codex-session-relay --state "$RELAY_STATE" verdict   --event <id> --verdict verified \
      --verdict-turn <own turn> --finding 'c1=verified' --finding 'c2=verified'

`claim` binds the review to the criteria set in force at that moment, so editing a criterion's
text afterwards invalidates the review rather than passing it. That review is not lost: see
[Re-reviewing after the criteria change](#re-reviewing-after-the-criteria-change) for claiming it
again against the set now in force.

The proof is over the parent's OWN acknowledging turn, which the delivered message cannot carry:
the child does not know which turn will acknowledge, and quoting the delivered fields back cannot
produce the value. `ack-proof` is how the parent derives it, from the event id and its own turn id,
and its `ackProof` output field is what `ack --ack-proof` expects. `ack` derives the same value only
to compare against the one you supplied, refusing a mismatch with `ack_proof_mismatch`; no flag
makes it produce the proof for you, because a proof the relay supplied would say nothing about who
sent it. Use the same turn id for both commands.

Without a host the acknowledgement is RECORDED as the parent's authored intent and reported
unverified: it does not close the attempt and cannot yet produce a verdict. A host-capable process
completes it with `verify-acks`, re-checking disposition as it goes, so an intent whose generation
advanced or whose relationship paused meanwhile stays exactly as the parent wrote it.

That is why `verify-acks` sits in the sequence above rather than being left implied: a parent
working from the store alone cannot run `ack` into `verdict`, because the verdict is refused
against an acknowledgement that is still only an intent. A parent that passed `--socket` to its own
`ack`, and whose host answered, already has a verified acknowledgement and skips the line; that is
the only case that skips it, and it is visible in the `ack` result rather than assumed.

`--verdict` is `verified`, `needs_changes`, `unverified` or `aborted`; per-criterion dispositions
are `verified`, `needs_changes` or `unverified`, and `aborted` is not among them. A `verified`
verdict needs every REQUIRED criterion recorded `verified`; one finding
left at `needs_changes` refuses it with `criteria_not_covered`. Against registered criteria,
`needs_changes` and `unverified` each need at least one criterion marked that way WITH a note, and
`aborted` states why in `--reason`; without that the verdict is refused with `findings_required`.
The correction is the other verdict:

    codex-session-relay --state "$RELAY_STATE" verdict --event <id> --verdict needs_changes \
      --verdict-turn <own turn> --finding 'c2=needs_changes:what to change and why'

A `needs_changes` verdict IS the correction: it opens the next generation and queues the revision
request to the same registered child, carrying the superseded event, its digest and the findings.
Give every finding a note.

What the child actually reads is the RENDERED revision request, which is not the whole verdict. On
the version named above the renderer emits only the first ten findings and adds no notice that it
dropped the rest, so a finding past that point is delivered nowhere and looks delivered from the
parent's side. A size budget can go further and drop the findings section outright, first entry
included. So putting what the child must receive in the first finding is a precaution and not a
guarantee, and no arrangement of the verdict makes one: the verdict is a single transaction that
has already opened the next generation before anything can be read back.

Confirmation therefore comes from a dispatched attempt and what the child actually received. A
queued rendering is the bytes the next attempt would send, not evidence that any send happened,
and reading it settles nothing about delivery. Where the content did not arrive there is no second
correction to send: the verdict does not resend, and this workflow forbids the route around it. So
it is recorded as an undelivered correction on the assignment and handed to the coordinator to
decide, rather than repaired here. Treat all of these limits as this version's behaviour rather
than constants.

Queued is not sent. That revision request travels the same way a completion does, so the
host-capable `deliver`, or a `daemon` already running, is what puts it in front of the child. A
correction nobody delivered is not a correction the child can act on, and the answer is that step,
never a second message sent around the relay.

## Verify the current revision

    codex-session-relay --state "$RELAY_STATE" revision-head --relationship <rel>
    codex-session-relay --state "$RELAY_STATE" assignment-show --relationship <rel>

`verified` and `needs_changes` are refused on anything that is not the current head:
`stale_generation`, `superseded_revision`, `revision_ambiguous`, or `relationship_not_active`.
Head evidence reads `sole_revision` or `declared_chain`; `fork`, `cycle`, `unknown_predecessor`
and `disconnected` are ambiguous and withhold completion rather than picking a winner.

`assignment-show` returns the state and what it is waiting for: `requested`, `received`,
`verifying`, `needs_changes`, `corrected`, `verified`, `merged`, plus `paused`, `ambiguous`
and `re_review_needed`. Record an integration with:

    codex-session-relay --state "$RELAY_STATE" assignment-mark --relationship <rel> \
      --mark merged --evidence '<what landed>' --actor <id> \
      --expected-event <the event you integrated>

Naming a revision that is no longer current is refused rather than silently rebound, and so is a
revision whose criteria set has moved since it was verified: the mark is refused with
`criteria_set_changed` until the re-review below lands. The mark is about the revision rather
than the wording, so once that re-review records `verified` an integration recorded earlier reads
as the current mark again and does not have to be recorded twice.

## Re-reviewing after the criteria change

Once the criteria change, `assignment-show` reports `re_review_needed` and asks the parent to
verify again. There are two routes. Which one applies turns on whether the artifact has to change.

An inactive relationship gates both routes rather than choosing between them. While it is paused,
cancelled or archived, claiming cannot reopen the review and `generation-open` refuses with
`relationship_not_active`. Bring the relationship back first and then pick a route on the ordinary
grounds below. Reactivating is not a status flip, because `relationship-status` accepts only
`paused`, `cancelled` and `archived`. Resume restates the generation and the scope being
re-authorized, and the restatement is compared for exact equality, so repeat
`--expect-artifact-root` and `--expect-allowed-recipient` once for every root and recipient the
relationship was registered with:

    codex-session-relay --state "$RELAY_STATE" relationship-resume --relationship <rel> \
      --expect-generation <n> --expect-artifact-root /abs/path \
      --expect-allowed-recipient <parent task id> \
      --expect-allowed-recipient <child task id> --actor <id>

A restatement that does not match is refused with nothing written. A superseded relationship does
not come back at all: its successor owns the issue. Cancelling or archiving RELEASES the issue, so
one whose issue another child has since been registered for is refused with
`duplicate_assignment` rather than resumed into a second owner; replacing that assignment is a
deliberate act of its own.

### Judging the same revision again

When the deliverable is fine and only the wording moved, claim the same event again and rule on
it. A claim on an event whose criteria set moved out from under its review returns `proceed`
instead of `already_claimed`, and rebinds the review to the set now in force:

    codex-session-relay --state "$RELAY_STATE" claim   --event <id> --turn <own turn>
    codex-session-relay --state "$RELAY_STATE" verdict --event <id> --verdict verified \
      --verdict-turn <own turn> --finding 'c1=verified' --finding 'c2=verified' \
      --expect-criteria-digest <the digest of the set you read>

`--expect-criteria-digest` is required only where a verdict has ALREADY settled on that event,
and it is the `setDigest` `criteria-show` reports for the set actually read. Without it the call
cannot be told apart from re-submitting the ruling being replaced, which would clear
`re_review_needed` with nobody having read the new wording, so it is refused with
`criteria_set_changed`. A review that was claimed but never ruled needs no digest: the re-claim
rebinds it and the ordinary `verdict` line above works unchanged.

A re-review records `verified` or `needs_changes`. Those are the two dispositions the assignment
has a state for. `unverified` and `aborted` are refused here with `disposition_conflict`, because
replacing a certification with a disposition the assignment cannot act on leaves it waiting on a
verification that can no longer be given. Both remain available on an event that has not been
ruled on yet, and an assignment nobody intends to finish is stopped on the relationship rather
than annotated onto one of its events.

Re-claiming stays idempotent: it reopens once per criteria edit. A second claim after it returns
`already_claimed`, and so does a claim once the re-review has landed, so a duplicate delivery
still cannot obtain the claim.

The replaced ruling is kept. `verdict_context` carries the set actually ruled on, and the journal
records `verdict_superseded` with the replaced record and both digests. What this does NOT do is
refresh an already-published coordination summary: the outbox identity does not include the
criteria digest, so a re-review landing on the SAME disposition enqueues no second job and the
document keeps the summary written against the earlier wording. Rewrite it yourself, the same way
you wrote it the first time.

### A fresh execution generation

That route is closed and a new generation is the one to use whenever the same event cannot be the
answer:

  - the artifact itself has to change, which is what a `needs_changes` verdict is for;
  - the event was already ruled `needs_changes`, `unverified` or `aborted`. Re-claiming it
    returns `already_claimed` and `verdict` returns the settled record marked as a replay. After
    `unverified` the state reads `verifying` rather than `re_review_needed`, so the assignment
    does not announce this one;
  - the event is no longer the revision this generation stands on, because a newer revision
    arrived, the head is ambiguous, or the generation advanced.

A `needs_changes` verdict opens the generation itself. Open one by hand when nothing ruled it:

    codex-session-relay --state "$RELAY_STATE" generation-open --relationship <rel> \
      --dispatch-request-id <new stable id> --reason needs_changes_revision \
      --dispatch-turn-id <the turn that dispatched the re-review>

`--reason` takes `initial_assignment` or `needs_changes_revision`; anything else is refused with
`unknown_generation`. Opening SENDS nothing, because the command is store-only and a generation is
not an event the relay can deliver. The child learns about the re-review the same way it learned
about the original assignment, through whatever transport dispatched it, and the turn that resume
creates is the new generation's anchor. Supply that turn when opening, as above, or afterwards:

    codex-session-relay --state "$RELAY_STATE" generation-bind --relationship <rel> \
      --generation <n> --dispatch-turn-id <the turn that dispatched the re-review> \
      --source dispatch_receipt

Until it is bound, a receipt in that generation is refused with `unbound_generation`: the
generation stays pending and reportable rather than accepting a receipt anchored to nothing.
Neither ordering removes that window, since a quick child can finish before the open or before
the bind. A child that meets the refusal is not stuck and does not wait for binding to appear: it
preserves the artifact and reports the completion unemitted, exactly as for an assignment that is
not registered yet, and the coordinator binds the anchor and recovers the receipt from that same
child. Polling for the binding would be a readiness loop, and this workflow does not have one.
If the child completes on a later turn than the anchor, that is the continuation claim above.

The child then emits under the new generation. Identical artifact bytes are fine: event identity
includes the generation, so the receipt is a new event, its claim binds the CURRENT criteria set,
and a fresh verdict records against the obligations now in force. This is a new review context,
not a duplicate delivery, and it needs no overwrite and no second verification path.

## The Linear summary is the coordinator's own write

The relay never calls Linear and holds no credential. There is no automatic Linear transport: the
relay's automatic delivery is task-to-task only. A verdict enqueues the summary it owes, and the
coordinator executes it with the connector it already has.

    codex-session-relay --state "$RELAY_STATE" sync-target --relationship <rel> --target-ref <doc id>
    codex-session-relay --state "$RELAY_STATE" sync-next
    codex-session-relay --state "$RELAY_STATE" sync-claim     --sync <id> --owner <who>
    codex-session-relay --state "$RELAY_STATE" sync-operation --sync <id>

`sync-operation` returns the target, the identity, the container and block markers, the exact
block, and the protocol. The connector's document save takes no idempotency key, so a write can
succeed and lose its response. Therefore read the document first and locate this relationship's
CONTAINER markers.

No container yet is initialisation, not a job write: create the container ONCE, with an empty body,
before any job goes into it. If that response is lost, read again and reconcile rather than
creating a second one, because a negative read is not proof of non-delivery while an earlier write
can still land.

With the container present, every job write, INCLUDING this job's first block, is a conditional
replacement of the container's exact current text, end marker included, so a stale writer's edit no
longer matches and is rejected. A bare append is never safe for a job block: an append is
unconditional, so a slow writer whose lease expired can still land a second copy after another
writer appended. Then read the document back and settle the job:

    codex-session-relay --state "$RELAY_STATE" sync-reconcile --sync <id> --observed @doc.txt
    codex-session-relay --state "$RELAY_STATE" sync-complete  --sync <id> --claim-token <token> \
      --target-ref <doc id> --readback @doc.txt
    codex-session-relay --state "$RELAY_STATE" sync-fail --sync <id> --claim-token <token> \
      --error '<text>'

`sync-reconcile` answers from the document before any rewrite: `already_written`, `absent`,
`stale`, `duplicate` or `malformed`. Those describe THIS JOB'S BLOCK, not the container, so
`absent` still means writing through the conditional container replacement above. A duplicated or
unterminated block is repaired by one conditional replacement, never appended over, and `stale`
returns that block's exact current text so the repair is built from what is actually there.
`sync-complete` parses the readback as a structured record and checks this job's own block, not
the presence of words, and confirmation is monotonic.
A failed summary write is retried on its own and never re-runs a verification or re-sends a
correction; `sync-status` and `sync-retry` manage the queue, and
`sync-progress --relationship <rel>` queues a progress summary between verdicts.

## Not owned here

The wire and record protocol, the invariants, and the package internals live with the package's
own documents. Read them when changing the relay, not when running an assignment.
