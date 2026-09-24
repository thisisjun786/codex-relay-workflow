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

## Determine whether this store holds the assignment

    codex-session-relay --state "$RELAY_STATE" doctor --issue <the exact issue identity>

Adds an `issue` block to the doctor report: `holds`, `responsibleChild`,
`responsibleRelationship`, the `storeId` the rows actually came from, and `storeAgreement`.

It exists because the proof was a conjunction split across two commands with nothing ordering
them. `doctor` said which store this process reached; `assignment-find` said who owns the issue;
and running the second one first against a mistyped state directory CREATES an empty store, which
then answers "no assignment" perfectly honestly. A coordinator that believes that answer opens a
second writer for an issue that already has one. Asking both halves of one read-only connection is
what removes the gap, because a single read cannot disagree with itself about which file it read.

`storeAgreement` compares the store id returned by that read, and the device and inode the read
itself measured, against what the probe measured. `same` is the only value to act on. `changed`
means the rows came from a file this process did not measure, and it returns `holds: null` and
exits 2 rather than reporting a relationship you would then adopt out of an unverified store.
`unknown` means one side could not be established. None of this is proof of store identity on its
own: an id travels with a copy of the bytes, and an agreeing device and inode does not say the
peer opened the same pathname for it, which is why `--expect-store`, `--expect-inode`,
`--expect-log` and `--expect-nonce` still exist and still refuse anything short of `proven`.
`--expect-log` is the newest and carries the part a name count cannot: where the peer's
write-ahead log is written, since one inode reached at a second pathname keeps a log of its own.

`unknown` is refused the same way `changed` is, whenever the database WAS readable: a caller that
asked whether these rows came from its store and got no proof must not read exit 0 as yes, which
is the rule the `--expect-*` comparison already applies one level up. An unreadable database is a
different answer rather than a weaker one — there is no relay here to agree with — so it keeps
exit 0, and determining before anything exists is not an error.

`holds: false` before registration is the expected reading, not a verdict that this assignment is
direct. Registration needs the task id creation returns, so at managed start there is genuinely
no relationship yet. What establishes relay-managed mode at that point is the agreed state
directory plus the declared intent naming that store, which `intent-show` reads back; `holds`
becomes true at step 6, once registration has landed. Treating step 1's `false` as "no relay" is
the misreading that keeps execution on the direct path, and it is why the determination is
recorded rather than re-derived from a single lookup.

An unreadable database answers `readable: false` and `holds: null`, never `holds: false`. Those
are different answers and only one of them is safe to act on. Like the rest of `doctor`, this
constructs no store: it reads through the probe's own read-only connection, so a diagnosis cannot
create the database it was asked to look at, and a rename during the read returns no rows rather
than rows attributed to the wrong file.

`assignment-find --issue` carries the same provenance under `relay`: `holds`, and a `store`
block with `storeId`, `dbPath`, `realPath`, `device`, `inode` and `recordedSocket`.
`recordedSocket` is the socket the store recorded when it was created, first write wins, and
deliberately not the socket this process resolved — so a participant pointing somewhere else can
see the two disagree instead of the later one quietly winning.

### The managed start sequence

For new assignments, prefer the relay's `managed-start` entry when that installed version exposes
it. It owns pre-creation reservation, standby creation, registration and business dispatch as one
recoverable operation; see the [request and recovery contract](../../../../../packages/codex-session-relay/README.md#recoverable-managed-start).
Use the same request id, exact request contents and state/socket/marker paths when recovering.
A refused or incomplete result is not permission to create a replacement. `admitted` means
dispatch only: the child's claim, actual hook firing, receipt and parent judgment still need
their own evidence. Verify the installed command surface before selecting it; source presence
does not make an older installed relay support it.

The lower-level sequence below remains available for supported explicit coordination and
documents the existing owners. Run in this order. The determination comes FIRST, because that is the
step whose absence left the question unasked; running the lookup first against the wrong path is
what creates an empty store and reports a real assignment as absent.

1. `doctor --issue <issue>` — determine. Expect `holds` false or null here.
2. `intent-declare --dispatch-request-id <id> --issue <issue>` — the management marker, published
   BEFORE the child exists. `dbPath` is recorded from the resolved `--state`; there is no
   `--db-path` on this command, only `--no-db-path` to suppress it.
3. create the child, then `register` with both endpoints and both allowed recipients.
4. `intent-register --assignment <id> --relationship <rel> --dispatch-request-id <id> --db-path <store>`
   — join the marker to the relationship registration actually produced.
5. `criteria-register` — the canonical set, so a later verdict rules on agreed obligations.
6. `doctor --issue <issue>` again — expect `holds` true, the responsible child, and
   `storeAgreement` `same`.

`tests/test_managed_execution.py` in the relay package runs exactly this sequence against a real
store and asserts the chain, so an instruction that stopped producing it fails there. It covers
managed START only: an offline `emit` stages and a staged receipt has no delivery row, and
`deliver` needs the socket, so the delivery and correction legs are asserted against a real store
through the fake host instead. Note that the doc and the test are not yet compared automatically —
keep them in step by hand when either changes.

## Managed execution boundaries

A healthy delivery worker is one prerequisite for a managed assignment, not an assignment.
`managed-start` sequences the [managed start](#the-managed-start-sequence) owners with a durable
issue reservation and retained bridge receipts. The lower-level calls remain separate;
`doctor --require-worker-policy` alone neither runs admission nor prevents raw task creation.
The managed entry rechecks authorization after resume, but an external UI change can race its
last read and turn start. Do not describe that observation as an atomic host permission guard.
The source package's [exact-turn reporting observation](../../../../../packages/codex-session-relay/README.md#exact-turn-reporting-observation)
is a separate manual diagnosis: `reporting-show` reads one named turn offline. A Stop observation
alone is not a terminal assignment, and missing or unreadable evidence stays unknown rather than
success. The command does not wake a parent, write a queue, or publish a report. Source presence
does not mean the installed relay exposes it.

The durable owners are separate:

| Boundary | Owner | Evidence it supplies |
| --- | --- | --- |
| Dispatch intent and task correlation | [intent.py](../../../../../packages/codex-session-relay/src/codex_session_relay/intent.py) | Published intent, creation observations, identity binding and relationship reference |
| Authorized assignment and execution | [registry.py](../../../../../packages/codex-session-relay/src/codex_session_relay/registry.py) and [criteria.py](../../../../../packages/codex-session-relay/src/codex_session_relay/criteria.py) | Endpoints, scope, generation, exact dispatch anchor and canonical criteria |
| Actual worker policy | [service.py](../../../../../packages/codex-session-relay/src/codex_session_relay/service.py) and [rolepolicy.py](../../../../../packages/codex-session-relay/src/codex_session_relay/rolepolicy.py) | The serving process's policy snapshot and point-in-time readiness |
| Child's declared result and observed turn ending | [receipts.py](../../../../../packages/codex-session-relay/src/codex_session_relay/receipts.py) | Staged versus final result, or failure/interruption/ordinary turn end |
| Stop-time omission | [guard.py](../../../../../packages/codex-session-relay/src/codex_session_relay/guard.py) | Bounded missing-declaration/receipt observations, never an invented result |
| Delivery, acknowledgement and judgment | [delivery.py](../../../../../packages/codex-session-relay/src/codex_session_relay/delivery.py) and [ack.py](../../../../../packages/codex-session-relay/src/codex_session_relay/ack.py) | Separate queued, dispatched, received and judged states |

A registered generation can still lack its first dispatch anchor. Receipt intake refuses it
until an exact turn is bound. The [live-trial startup order](../../../../../docs/live-trial.md#the-order)
materializes a standby turn before business dispatch; that host preparation is not completion
evidence. An unknown creation or dispatch response must be reconciled against its retained
bridge operation rather than retried with a new request id.

An ordinary turn end without a child receipt is not success. The Stop guard records omissions
only for the assignment and session it can establish, and its bounded holds do not guarantee
that an agent will submit a report. Raw bridge calls are not themselves intercepted by the
guard. Tasks without a managed marker remain outside its observation. The worker's send-time
policy and lifecycle checks remain
necessary even when an earlier readiness check passed.

## One shared state directory

Every process in one assignment must pass the same `--state`. The child emitting, the parent
verifying, and whichever process delivers all read one store; a mismatched `--state` simply means
they do not see each other.

There are TWO selectors and they are set separately. `--state` chooses the relay's own store, and
falls back to `state_dir()` when omitted. For ordinary delivery commands, the adapter's ledger,
which is where duplicate suppression and delivery recovery live, comes from `state_dir()`: it reads
`CODEX_SESSION_RELAY_STATE`, and otherwise `$XDG_STATE_HOME/codex-session-relay` or
`~/.local/state/codex-session-relay/<endpoint hash>`. So passing only `--state` moves the store
and leaves the ledger behind. Measured: `--state /tmp/assigned` reported that store while the
adapter's root resolved under `~/.local/state`. Recovery then reads a ledger that never saw the
attempts, so it can miss one or repeat a delivery.

`managed-start` pins its ledger to its required explicit `--state` instead. Its request
fingerprint includes the observed ledger path, device and inode, and mutation boundaries
revalidate that identity. A retry after ledger replacement refuses. This exception does not
change the shared environment requirement for the delivery worker and other socket commands.

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

Commands that require the App Server and `--socket` include `managed-start`, `deliver`,
`reconcile`, `recover`, `daemon` and `verify-acks`. Inspect `doctor`
`actorReachability.hostRequiredCommands` for the installed command inventory;
`reporting-show` reads only the explicitly selected marker and store. Where a task cannot
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

Record the role the creation cited alongside them, through `--parent-role`/`--child-role` on
registration or `--role` on a settings record, taken from the receipt's `executionPolicy.role`.
The relay compares it with the role the task is actually bound to, in whichever order those two
facts arrive, and refuses a disagreement as `role_binding_mismatch` rather than recording it:
the recovery for a mismatch is not re-recording, because that would write one side's answer over
the other.

After a user changes an existing task's model, re-record that task's authorization from a
user-attributed source before the next send, with `--source user_transition`. The record is what
a send verifies against, and a value observed on the host is evidence of what the task is running
rather than a new approval, so nothing adopts a drifting setting on its own. Until it is
re-recorded the send is refused as `settings_record_stale_for_role`, before any transport call.
A process that cannot read a role policy refuses role-bound sends as `role_policy_unconfigured`
rather than skipping the check; that hold is retry-safe, so declaring the policy and restarting
resumes the held deliveries. `doctor` reports the policy digest this process resolved and
compares it with the bridge's, because two processes reading two different files is a second
source of truth that neither one can see on its own.


A message to a task states the RECIPIENT's authorized pair, not the sender's. A child reporting
to its parent states the parent's, and its own settings are unaffected by what the parent runs
on; copying the recipient's pair into the sender is how a correction to one level spreads to
another. The pair to state is the one RECORDED as that task's authorization, which is what a send
verifies against. For most tasks that is the pair its role's policy declares, and it is read at
the time of the send rather than from a memory of what the role used to run on. It is not always
that pair: an exception authorizes one specific model and effort for one role and directory, and
a task created under a currently valid one legitimately differs from the ordinary declared pair.
Stating the declared pair for such a task would either fail verification or ask the host to
change what the task runs on.

The recipient's runtime state decides the mechanism as well as the settings, and it does not
decide it alone. An ACTIVE recipient is steered into the turn it is already running, and a steer
carries no model or effort at all, so there is nothing to state and nothing that could be
applied. An IDLE recipient whose pair derives from its role's declared pair is resumed and
carries its settings, and so is one the host reports as `notLoaded`: a host that applies what it
was sent while materializing the thread lands it where policy says it belongs. A record-based pair
— a supervisor's — or an exception-authorized one is never transmitted, loaded or not, because a
resume could restore a value the user has since changed. The relay's transport loads such a
recipient with a resume that requests nothing, compares what the host reports with the record
before any turn, and refuses a difference as `settings_differ_after_load`, retry-safe with
nothing started; re-record from a reading the user stands behind rather than retrying. The
workspace roots may come back narrower than recorded, because a load does not restore them, and
never wider. The bridge's own tool path still refuses the unloaded case, so an operator message
to an unloaded supervisor through the bridge waits until the host has it loaded.

Reading the state before choosing is what keeps those apart, and it is also what a report must
not skip: a send accepted on a resumed recipient, a steer accepted into a live turn, and a
correction still held because its recipient was not loaded are three different facts.

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

Read `deliverable` rather than `usable` before a send. They answer different questions: `usable`
is about the record alone, whether the required fields are there, and it is the field `missing`
pairs with. `deliverable` also accounts for the role the task is actually bound to, so a complete
record that contradicts its binding or has gone stale against the current role policy reports
`usable: true` and `deliverable: false`, with `roleFinding` saying which. A preflight that reads
only `usable` will approve a send the relay then withholds.

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

Where the work is a pull request, the work report recorded against that event also carries the
merge-readiness handoff: the head this evidence is about, the base you verified, the declared
required check names, the runs as `{runId, name, headSha, conclusion, attempt}`, the review
coverage as `{hasNextPage, pagesRead, totalCount, threadsSeen, unresolved}`, and a judged
disposition with evidence for every thread in `threadsSeen`. Recording it is refused while the
review is unenumerated, while anything is unresolved, while a thread seen has no disposition,
while a declared required check is not successful at its highest attempt on that head, or while
the pull request is a draft. State every field; an unstated one is refused rather than read as
zero. If the review is not finished, the turn ends `blocked_needs_input` and says so, which is
not a lesser outcome than pretending it did.

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

## Which children stopped, and whether anyone was told

    codex-session-relay --state "$RELAY_STATE" dispositions-show --project <the project key>
    codex-session-relay --state "$RELAY_STATE" dispositions-show --relationship <the id>

Read-only, offline, and it constructs no store: a mistyped state directory answers `readable`
false instead of creating an empty database that then honestly reports nothing. One selector is
required and the two are exclusive. `--project` answers about live work, so an archived or
superseded assignment is absent from it. `--relationship` answers about the assignment it names
whatever its status, and carries that status.

It exists because `assignment-find` and the verification reads answer about `ready_for_review` and
nothing else. A child that records `blocked_needs_input` is invisible to them, so a coordinator
reading only those concludes nothing is wrong while a child waits for a person. Use this before
deciding that a quiet project is a healthy one.

Per child it reports `turnDisposition` over the execution-only outcomes — `blocked_needs_input`,
`failed`, `interrupted` — with `basis` saying how that was decided: `sole`,
`latest_of_same_outcome` when several events agree, `contested` when two final dispositions
disagree, and `none` when this generation holds no execution-only disposition at all. `none` is
not "the child is fine": read `reviewable` beside it, which counts and lists the reviewable events
and says `head: not_derived_here`, because which revision a generation stands on is answered by
`assignment-show` and `revision-head`, not here. `contested` names no winner on purpose.

Per event it reports the `workReport` that separates `BLOCKED` from `UNSAFE` from `NEEDS_HUMAN`.
All three collapse onto `blocked_needs_input` in the frozen outcome enum, so when
`workReport.recorded` is false those three cannot be told apart for that event, and nothing infers
one of them from the outcome.

Delivery is two questions, reported separately, beside the store's own `state`.

| `delivery.observation` | What the records say |
| --- | --- |
| `unmeasured` | a final event with no delivery row and no delivery intent. Nothing establishes whether a delivery was ever attempted or even wanted; absence also covers `--no-enqueue` and an event stranded by an old generation, so it is never read as "not delivered" |
| `refused_pre_queue` | delivery was wanted and refused for a reason that may not last, with its attempts and last error |
| `not_sent` | the delivery exists and nothing has been sent |
| `send_uncertain` | a send is in flight or answered unusably, which is not evidence of non-delivery |
| `stored_not_woken` | left where the recipient reads it with no turn woken |
| `dispatched` | the transport accepted a send and there is a turn id for it |
| `superseded` | no longer what the assignment stands on; the state is left alone so reconciliation can still settle it |
| `not_deliverable:<stage>` | a staged claim, which is real progress and never delivery |
| `suppressed` | the claim was invalidated, so there is no delivery obligation to measure |
| `state_unrecognised` / `records_disagree` | the store holds a state this reader has no word for, or rows that cannot coexist. Read it with `status` |

`delivery.recipientObservation` is the other question, and the one to check before concluding a
parent knows: `observed_host_read` when an acknowledgement rests on an App Server read of the
recipient's own turn list, `observed_claimed` when an acknowledgement is recorded without that
read, and `unmeasured` when neither an acknowledgement nor its evidence exists — including behind a
`dispatched` send, because a dispatch proves the send was accepted and never that anybody read it.

Exit codes differ from `doctor` deliberately. `doctor` keeps exit 0 for an unreadable database
because determining before anything exists is not an error. This command exits 2 and prints the
whole payload, because a coordinator asking which children are blocked and checking only the exit
code must not read an unreadable store as "nobody". A readable store with no matching children
exits 0 with an empty list, which is a different answer.

For the per-delivery phase of a delivery that exists, and for pending intents across the whole
store, `status` remains the reader; this command does not restate its vocabulary.

These readers are also what a parent runs on entry rather than only when something looks wrong.
[OPS-8.5](operations.md#ops-85-the-goal-free-parents-wake-path) makes that a contract: a parent
woken from idle re-reads its own outstanding work instead of acting on the payload that woke it,
which is what carries the events that arrived while it was mid-turn. There is no recipient-scoped
reader that answers it in one call, so the parent reaches it by relationship — the project's
outstanding assignments, then each relationship's dispositions and status.

One state these readers surface deserves naming, because it looks like waiting and is not. A
delivery that exhausts its attempt budget is held, and a held delivery is excluded from
eligibility: it is not retried again, and no supported command clears the hold. Its recovery is a
fresh execution generation, which creates a new delivery rather than reviving the held one. Read
`holdReason` beside the delivery state before concluding that a quiet assignment is merely slow,
and where a parent was never woken at all, nothing surfaces this automatically.

## The four readers a candidate pass also uses

    codex-session-relay --state "$RELAY_STATE" merge-turn-show --turn <id>
    codex-session-relay --state "$RELAY_STATE" merge-turn-show --repository <repo> --base-ref <ref>
    codex-session-relay --state "$RELAY_STATE" merge-turn-show --parent-task <task>
    codex-session-relay --state "$RELAY_STATE" merge-turn-acknowledge --turn <id> --actor <task> --grant <id> --evidence <text>
    codex-session-relay --state "$RELAY_STATE" capacity-show [--project <key>] [--parent-task <id>] [--initiative <key>] [--scope <key> --scope-kind initiative|project|store]
    codex-session-relay --state "$RELAY_STATE" region-show --repository <repo> [--revision <rev>] [--project <key>] [--path <path>]
    codex-session-relay --state "$RELAY_STATE" linkage-outstanding --project <key> [--task <id>]

[Re-evaluating the candidate set](reevaluation.md) names these as the reads behind an integration
landing, a capacity change, a peer's region and a new attachment. What each answers is here; what
a pass does with it is there.

`merge-turn-show` takes exactly one selector, and naming two is refused as `bad_invocation`
rather than answered about whichever one won: a recovery read that quietly changed scope is
worse than one that stops. `--turn` names one turn, and an id the store does not hold answers
`ok` false with reason `unregistered_scope` rather than an empty record, so a mistyped turn
is not read as a released window. `--repository` with `--base-ref` answers about that
target's window instead of one turn, and the two are given together or not at all.

`--parent-task` is the one a parent uses on entry, because after a restart or a compaction
its own task id is the only identifier it still has: every other selector needs a turn or a
target it no longer remembers. It answers every live claim that task holds across targets,
each with its grant, its state, and `targetFree` — whether the target could be taken right
now. Nothing acquires on a parent's behalf, so a waiting claim on a free target is taken by
declaring readiness again, and an unresolved outcome is reported as unresolved rather than as
a target anybody may claim.

A turn that reached holding carries a grant addressed to its owner, and
`merge-turn-acknowledge` is how that owner says it re-read the record rather than acting on
what it remembered. It is refused when the grant is not the current one — a returned tenure or
a restated candidate each issue their own — when the turn is no longer holding, when the caller
is not its holder, or when the owning binding is paused. A granted turn that has not
acknowledged cannot begin a merge. A claim made before grants were recorded has none, needs
none, and is not held to this.

`capacity-show` reports the held slots, their total, the count per parent, and any whose recorded
parent no longer owns the project. `--project`, `--parent-task` and `--initiative` narrow that
list. `headroom` is added only when `--scope` is given, and it carries a dimension only where a
limit was declared: a scope with no declared limit is unbounded by the store rather than known to
have room, and those are different answers.

`region-show` needs `--repository`; `--revision`, `--project` and `--path` narrow it. A region
nobody proposed has no row at all, so an absent overlap is unknown rather than none.

`linkage-outstanding` lists the live assignments in a project that are not finished, scoped to
the **project** rather than to whichever task currently parents each row. `--task` narrows it to
one parent's rows and is deliberately left off for a handover: filtering by the parent made a
replacement appear to have no outstanding work at all, which let a second replacement take the
project without acknowledging anything.

`merge-turn-show`, `capacity-show` and `region-show` carry `unenforcedIndexes` where the store
could not install a guard index. An ambiguous answer and a missing index are the same fact seen
from two sides, so a reader deciding whether to act on a contested owner is being told that the
database is not stopping a second one either.

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

When the child's report came with a `relay-packet/1` packet, check it first against your own
store reading, exactly as a child checks what it receives
([the typed form these fields travel in](task-packet.md#the-typed-form-these-fields-travel-in)):

    codex-session-relay --state "$RELAY_STATE" packet-check --packet <file> \
      --receiver <own task id> --ledger <own reception ledger> --observation <observation file>

The observation is where you read the pull request's repository, number and head, with its
source, or, for a deliverable that is not a pull request, its path and the digest you computed
yourself in the form the packet states it; the store never holds either. Without it the answer
is unavailable on the artifact, which means read it and check again, not go on. Go on only where
the answer is accepted and `act` is true, and once the steps below have run, record that you
acted on it with the same `packet-check` and `--applied`, without `--observation`, so a
report arriving again is not acted on twice. Then:

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

Read the handoff the report carries rather than collecting its contents again. The child has
already paginated the review and enumerated the check runs, and the values are the ones the merge
turn expects to be restated. What this side adds is currency: re-read the head and the base
immediately before merging and compare the counts to the record. A disagreement is a fail-closed
return to the same child, through the needs-changes verdict below, not a repair made here.

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

A build carrying this renders that request in the correction form a packet revision request is
refused without: VIOLATED CRITERION, WHAT CHANGED, FIX SCOPE, PRESERVE and REVERIFY AND RETURN.
FIX SCOPE is exactly the findings ruled `needs_changes`; one ruled `verified` is preserved, one
ruled `unverified` is evidence to show, and what the verdict did not record is written as not
recorded rather than omitted. So give each finding its disposition as well as its note.

What the child actually reads is the RENDERED revision request, which is not the whole verdict.
A build carrying the behaviour below still shows only the first ten findings, but it says how
many it dropped and names the restoration block when the cut took it. Say which finding carries
that block:

    codex-session-relay --state "$RELAY_STATE" verdict --event <id> --verdict needs_changes \
      --verdict-turn <own turn> --restoration c2 \
      --finding 'c2=needs_changes:what to change, and the context to resume from'

Ask the installed command whether it has this before relying on it, the same way capability is
discovered everywhere else here: `verdict --help` lists `--restoration` on a build that carries
it, and a build without it rejects the flag as unknown. The package version does not answer the
question: both stay `0.1.0` while their contents change, which is why
[OPS-1.2](operations.md#ops-12-the-integrity-digest) makes the integrity digest the thing that
detects drift. An operator reading a version alone cannot tell this source from an older
installation, and would get a usage error where the paragraph below promises a refusal before
the generation opens.

A declared block the message would not carry refuses the verdict with
`restoration_undeliverable` BEFORE any generation is opened, so nothing is superseded and
nothing is queued: move the finding and rule again. Recording a work report on the revision
request moves it onto the composer, which has a byte budget the plain renderer does not, so
that command re-measures and refuses there too, while there is still nothing sent to undo. The
outcome is named either way — `carried`, `truncated`, `budget_dropped`, `not_carried` or
`unmeasured` — and is reported beside the verdict record, as `_restoration`, and by
`show --event <id>`.

Those two are preflight: each describes the message the NEXT attempt would render. What an
attempt actually froze is recorded by that attempt, in the transaction that froze its bytes,
and `show` returns it as `restoration_attempted` beside the bytes themselves. A retry that
never sent leaves the following render one request-id digit longer, so a measurement taken
earlier is a good reason to act and never evidence of what went out.

All of that is about what the relay will put in the bytes, and none of it is a claim that the
child read them. Confirmation still comes from a dispatched attempt and what the child actually
received. A queued rendering is the bytes the next attempt would send, not evidence that any
send happened, and reading it settles nothing about delivery. Where the content did not arrive
there is no second correction to send: the verdict does not resend, and this workflow forbids
the route around it. So it is recorded as an undelivered correction on the assignment and
handed to the coordinator to decide, rather than repaired here. Treat all of this as this
version's behaviour rather than constants, and whether an installed relay behaves this way as
a separate fact from what this source does.

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

The message form both relations share is one of them. `relay-envelope/1` is defined in the
package's own `docs/envelope.md`, with the workflow rule in
[the message both relations are read by](../../crw-plan/references/integrations.md#the-message-both-relations-are-read-by).
Two facts from it decide what a run may claim, so they are repeated here and nowhere else: the
a report upward is transported and read back and nothing further is measured on that relation -
a supervisor agreeing, acting or confirming is not a fact the relay holds; and what discharges a
reporting obligation is the Linear record the supervisor reads, confirmed, rather than a report
having been written, sent or even read.

Three commands make that readable rather than remembered. None of them sends anything and none
wakes anybody; four more below do the sending, and the relay daemon runs the same staging and
sending on its own tick.

```bash
# Is this event news for the level above? A read: it sends, queues and records nothing.
codex-session-relay supervisor-select --event <id> [--recipient <supervisor task>]

# What does this project still owe upward? An explicit question, never suppressed.
# A turn that ended without reporting writes no event. The store derives it where the child's
# relay recorded its claim here; for a child that claimed without that, pass its reporting-show
# reading.
codex-session-relay supervisor-standing --project <key> [--observation <file>]...

# Record that a report was produced for this event's obligation, once.
codex-session-relay supervisor-report-recorded --event <id> [--message <messageId>]

# The same, for an obligation a turn left by ending without reporting. It has no event.
codex-session-relay supervisor-report-recorded --observation <file> [--message <messageId>]
```

`supervisor-select` answers `report: false` with a reason far more often than it answers true,
and the reason is the part to read: `no_meaningful_transition` for an ordinary event,
`already_reported_under_this_obligation` for a fact already reported,
`already_in_the_record_the_supervisor_reads` once Linear has it confirmed,
`recipient_is_not_contactable` for a paused or archived supervisor, and
`recipient_contactability_unmeasured` when nobody has looked recently. Every one of them
preserves the obligation; none of them discards it.

Without `--recipient` the answer is about the obligation alone and says so with
`deliverability_was_not_asked_about`: it does not claim anybody is reachable, and it does not
suppress on a question it was not given the means to ask. Name a recipient and deliverability
becomes part of the answer, which means a recipient nobody has observed recently - or an
observation dated in the future, from a clock that went backwards - suppresses the wake rather
than authorizing one.

`supervisor-report-recorded` says a report was COMPOSED. Whether a turn was created, whether it
ran, and whether the supervisor acted are three further facts, and no row here carries any of
them. Recording it is what makes the next reading of the same fact converge instead of waking
the level above again, so record it when the report actually goes out.

Four more are the channel CRW-215 added. A running relay daemon stages and sends what each
project owes on its own tick, so a parent does not have to remember to; the commands are for an
answer now, and they converge on the same messages. Only `supervisor-send` attempts a transport
from the command line, and it answers `sent: false` without touching the host when the message
is held, inside its backoff or already sent - including one the daemon sent first.
`supervisor-stage` records the report itself, so a run that stages does not also call
`supervisor-report-recorded`. A turn that ended without a receipt is staged by the daemon too,
once `omission_grace_seconds` have passed since the relay settled it, from what the child's own
relay recorded in the store: `intent-claim` records there that the session writes its
declarations into it, and `intent-disposition` records each turn's declaration beside the
marker file. Both answer a `storeRecord`, and `failed` exits 2 with the marker answer beside
it; run the same command again, which retries only the store record. `reporting-derive
--relationship <id>` prints the reading the daemon acts on. A child that claimed without that
record - through an older relay - is never staged automatically; stage its omission with its
`reporting-show` reading.

After a supervisor handover, stage the project again. A report staged for the former
supervisor and never sent is re-addressed to the live one under the same message; one that was
already sent stays with the task it went to and is listed under `refused` as
`relation_owner_drift`, which means the successor has not been told through this channel.

A send whose response never came back is held as `held_uncertain` and is never resent. The
supervisor's `supervisor-read` can still settle it, and does only when its transcript holds
that attempt's delivery token - its request id and a random part drawn when the send was claimed - in a turn that began no earlier than the transport; `supervisor-show` then records how it was settled.

The daemon also carries the relay's fault notifications up this channel (CRW-205 criterion 7),
only while the fault ledger has reserved them, so the assignment's pause, archive and no-contact
and the product's notification budget still decide what goes. One arrives with purpose
`fault_notice` - a broken fault opened, or a fault resolved; no answer is owed - or
`fault_decision`, a decision about a fault that is the user's to make, relayed like any
`decision_request`. It names the fault's class, severity and state and the issue once published,
and points at `fault-show --fault <id>` on the sender's store; the fault's recorded evidence does
not travel. The same `supervisor-read` records that one arrived. A fault the relay cannot place
under a project with a supervisor is not sent anywhere else; it waits, with the reason on the
notification (`fault-notifications`).

Once a message about an event has been sent upward, that event's work report no longer
changes: the relay refuses a correction to it, in place, as a new submission or as a first
report, because the bytes that went up described it and point at it for evidence. Before the send a
correction lands and the send carries it, so send after the report is final. A message
staged from the event before any report existed froze nothing, so the first report is still
recorded; the send restates the message to the report that stands and sends that, and when the
report turned the block into a decision, the block is held and staging the project stages the
decision. Whatever a send carries is re-derived where its transport starts, so a staged message
never goes out stale.

```bash
# Freeze what is owed upward as a message. Staging is not sending.
codex-session-relay supervisor-stage --event <id> [--recipient <supervisor task>]
codex-session-relay supervisor-stage --project <key> [--observation <file>]...
codex-session-relay supervisor-stage --observation <file>

# One attempt at one staged message. --socket is global and goes before the subcommand;
# without it the command exits 4 and writes nothing.
codex-session-relay --socket <path> supervisor-send --message <id>

# The recipient answering, with the line the message carries: it names the store the report
# was staged in (--state) and the socket it was sent through, and a bare --socket would open
# another store. The message asks for a turn id of its own; nothing enforces it. --as names
# the asserting task and is required; it is checked against the message's recipient.
codex-session-relay --state <dir> --socket <path> supervisor-read --message <id> \
  --turn <turn> --proof <p> --as <your task id>

# What was staged, every attempt, and what came back. An omission's report points here: it
# prints the observation the report was staged from, frozen, beside a line that re-reads it now.
codex-session-relay --state <dir> supervisor-show --message <id>
```

The proof is `sha256(messageId|<your own turn id>)`, which the delivered bytes cannot contain,
so quoting the message back does not produce it. That is the whole of what it rules out:
nothing authenticates the caller and nothing establishes that the named turn produced it.

A verified readback says a bounded scan of at most 200 of the recipient's items found that
attempt's delivery token, and that the host can read the named turn on the recipient's thread and
it carries a start time which is not CERTAINLY earlier than the send. That comparison applies
to every candidate, including the turn the attempt names, because a send can steer an existing
turn rather than open one. Where the readback names the turn the send itself opened, the
delivered bytes have to be in that same turn: what the token is in is where the message
landed. It does not say the turn answered, who computed the proof, or that anybody acted, and
it discharges nothing: the obligation stands until Linear confirms. For the turn the send
opened (`turnOrigin: relay_opened`, the ordinary case) a readback needs nothing from the
supervisor, since that turn's id is already in the store, so it shows the report arrived
rather than that it was read. Staging and sending are automatic - the relay daemon stages and
sends what each project owes on its own tick - but the readback is not: only the supervisor,
from a turn of its own, runs `supervisor-read`, and nothing reads back on its behalf.

`linkage-directive` takes `--purpose` now, which derives the envelope pointer from the link and
the digest rather than leaving it to be written by hand. A pointer belonging to another
instruction is refused with the contest retained, and one digest cannot carry two purposes.

The packet a parent and a child exchange sits on that same envelope and adds what the
occasion requires: `relay-packet/1`, in the package's own `docs/packets.md`, with the
workflow rule in [the typed form these fields travel in](task-packet.md#the-typed-form-these-fields-travel-in).
One command reads it, and the receiver runs it against its own store reading.

```bash
# Does this packet carry what its purpose requires, and does it agree with what YOUR store says?
# Opens the store read-only, reaches no host, writes only your own reception ledger.
# A packet naming an artifact (a correction, resume, acceptance, integration result or report)
# needs --observation: your own reading of it, which the store never holds (see below).
codex-session-relay --state "$RELAY_STATE" packet-check --packet <file> \
  --receiver <your task id> --ledger <your reception ledger> [--observation <file>]

# Offline, against a reading you supply yourself. The answer says recordSource: supplied.
codex-session-relay packet-check --packet <file> --record <file>

# After you acted on an accepted packet: record it applied in your ledger. Reads no store,
# and refuses --observation.
codex-session-relay --state "$RELAY_STATE" packet-check --packet <file> \
  --receiver <your task id> --ledger <your reception ledger> --applied
```

With `--receiver` the record is built from the store: the receiver's own relationship row
(its relation, the other task, the issue, whether it is still live, the current generation and
the dispatch that opened it), the project link's revision while that link is live and still
joins the relationship's two tasks, the registered criteria digest, the recorded settings of
the child (its model, effort, sandbox and approval) and of the parent being answered, the role
policy's verdict on them (the policy this store's service declares, which its daemon reads when
it launches, so the check does not depend on your shell's environment; two different policy
files refuse the check, and a daemon still running on an earlier policy shows it in
`service status` as `launchPolicy.runningDigest`), and the execution mode from the
receiver's own ledger. `--applied` reads neither the store nor the policy. The answer says
`recordSource: store` and gives each field's `provenance`. No artifact is a store fact: a pull
request's repository, number and head are a forge reading, and a deliverable's path and digest
are a reading of the file. Either comes only from `--observation`, a JSON file with its own
`source` - `{"source": ..., "artifactPath": ..., "artifactDigest": ...}` for a locator, with
the digest in the form the packet states it, or `{"source": ..., "repository": ...,
"prNumber": ..., "headSha": ...}` for a pull request. Without one those fields are gaps and the
answer is unavailable: read the artifact yourself and check the same packet again with it,
rather than acting on it or reporting it as refused. The ledger is how a repeat is applied once: `act`
is true while today's answer is accepted and the ledger holds no application for that packet,
and the relationship is not paused (then `actHeld` says it waits for `relationship-resume`);
`--applied` records one after the receiver acted, and without a ledger `act` is always false.

`--record` is the offline form: the reading the receiver did for ITSELF, supplied as a file.
The answer says `recordSource: supplied`, and that qualifier is the honest part: handing it an
agreeing record proves only that two files agree. A key the file leaves out is unread; null
for `relationRevision` is the one definite answer and says the relationship is unscoped.

Three dispositions come back and the middle one is the one to read. `accepted` means every
field the record could answer agreed with it. `refused` names the field and both values.
`unavailable` means the record could not answer, which is not acceptance: a field the
receiver could not check is unchecked, and a run that proceeded on it proceeded on nobody's
authority. A field the record omits produces `unavailable` rather than a pass, so a thin
record makes less pass rather than more.

A non-PR audit is a first-class shape here. Its artifact is a locator and a digest, it is
never compared against a head, and it is never asked for a pull request to fill the field.

The answer also carries the seven handover states as the store answers them for the packet's
subject event, the states the packet claims that the store does not back (`unbackedClaims`)
or cannot answer (`unmeasurableClaims`), and `coordinationSync` for the Linear summary block,
which is not the issue reaching Done. This check runs where the receiver runs it: no relay
code path carries a parent-child packet, so a receiver that skips the step has checked
nothing.
