# The supervisor channel, and what a readback is worth

Two relations carry messages here and they have never had the same machinery. A parent and its
child exchange a completion and a correction, with a queue, a receipt, an acknowledgement and a
verdict. A supervisor and a parent exchange instructions and reports, and until now the upward
half had nothing at all. [The shared envelope](envelope.md) decided what was owed upward and
said in data that nothing could carry it, so a parent that had decided it owed a report had
nowhere to put one.

`supervisorchannel.py` is that missing half. It is deliberately the smaller one.

## relay-supervisor-channel/1

One direction. A report travels parent to supervisor. An instruction travelling the other way
is a `scope_directives` row that linkage already owns, and nothing here sends one.

### Who may emit one

The live parent of the project, addressed to the live supervisor of the initiative above it,
both read from the linkage walk rather than from a frozen row - the rule
`delivery.resolve_recipient` already applies to a completion, for the same reason: an
assignment registered before a handover names the owner that stepped down.

The three answers stay three. An unreadable store has said nothing about the owner, a contested
one has said two things, and neither becomes a recipient. A caller may name the supervisor it
believes in; a disagreement with the linkage is the finding and is refused, because substituting
either value for the other files a report with the wrong task.

A project no initiative supervises refuses as `unregistered_scope`. That is not the same as
owing nothing: the obligation stays standing, and having nowhere to send a report is a different
state from having none to send.

### What may be sent

Only a standing obligation `supervision.select` reports reportable: a completion, a new real
block, a decision only the user can make, or a turn that settled without reporting. Those are
the three things CRW-148 decided are news, plus the absence. Nothing else has an entry point,
which is what makes a heartbeat unsendable rather than merely discouraged.

Besides those, one other thing: a fault notification the relay's fault ledger has reserved
(CRW-205 criterion 7), carried as a message of its own kind - see a fault notification, below.

### What it carries

`relay-packet/1`, gaining three rows rather than a second vocabulary:

| Purpose | Kind | Required beyond the envelope |
|---|---|---|
| completion | notification | issue, generation, evidence |
| blocked | notification | issue, evidence |
| decision_request | decision | issue, decision, evidence |

The restraint is the restraint the parent-child table already shows. A completion upward is not
required to carry an artifact, because a noop completion has none and a research assignment may
have only a locator; it carries the pull request when the work report names one whole, and a
partial one is left out rather than reported in pieces. A block and a decision carry evidence
for the reason a child's do: a level above told there is a problem and not where to look at it
has been told half of it.

A report is a pointer with a kind rather than a copy of the record. What the child actually said
is read through the evidence line, which is `show --event` for anything with an event and
`supervisor-show --message` for an omission, which has none. Each points at a version that
cannot change under the bytes that went up: an event's work report stops changing once a message
about that event has been sent, and until then the send carries whatever report stands; an
omission's reading is frozen on the message row when it is staged. The command that
produced that reading, `reporting-show`, re-reads the turn NOW, so a report that reached the
turn afterwards made it answer `reported` under a packet saying `unreported`; `supervisor-show`
prints it as `stagedFrom.recheck` beside the frozen `stagedFrom.reading`, and the two are
allowed to disagree. A second, different reading of an omission that is already staged is
refused as `contradictory_observation`, because one message keeps one reading.

An omission's envelope says what it is in its basis. The envelope has no purpose of its own for a
turn that ended without reporting - it travels as `blocked`, the purpose that asks the level
above to look - and with no revision its basis used to read unknown, so the one line a supervisor
reads first said a block. The live run showed the cost: the real supervisor summarised a
deliberately omitted report as the issue being blocked (CRW-215 live finding F4). The basis now
names it - `unreported: turn <id> ended without a report, reading <reason>, generation <n>` -
so the delivered bytes carry the obligation kind, the reason the reading gave and the generation,
and `supervisor-show` still carries the whole frozen reading.

### Every line it writes selects the store it was written from

Every command line the channel writes for a later step - the evidence pointer in the packet,
the readback line and the `supervisor-show` line in the bytes - reproduces the store selection
of the invocation that wrote it: `--state` with the directory that invocation's store is in,
whether it got there by `--state`, `CODEX_SESSION_RELAY_STATE` or the socket-scoped default.
A bare `--socket` selects the socket-scoped default, which is not the store at all whenever it
was chosen by `--state` or the environment, so the recipient's readback opened another database
and found no such message. The readback line also carries the socket the send went through,
canonicalised - that is the host the recipient's thread is on - and keeps
`YOUR_RELAY_SOCKET` only when the writer had no socket, which a CLI send never lacks. The
`reporting-show` recheck carries the reading's own `--state`, the selection that produced the
reading, and staging refuses a reading taken against any other store than the one it is staged
in - so that line selects this store as well, and the packet and its recheck read one record.

Every such line also names the relay that wrote it. It used to start with the bare program name,
which left a later reader's PATH to choose, and on the live host PATH named a relay installed
before this channel existed, so the rendered readback failed with
`invalid choice: 'supervisor-read'` (CRW-215 live finding F3). `relay_program()` renders the
console script installed beside the writing interpreter, found through the real path of its
environment so that the service's worker and a parent's CLI started through a `current` link
write the same bytes - a staged packet is compared byte for byte where its transport starts -
and the interpreter running the module where no console script is installed. A line is therefore
runnable as rendered with no PATH at all, for as long as that installation exists. After it is
replaced and removed, a line already delivered names an executable that is gone and fails as
rendered; its arguments still apply, and whoever runs them names the relay that now reads the
store. Nothing re-points a delivered line.
`tests/test_supervisor_live_findings.py` runs one rendered line as a real process with PATH
pointing nowhere.

**status_response is deliberately absent, and that is the one named gap in this contract.**
`BODY` is a dispatch instruction checked against DISPATCH-TASK-01, so requiring it of an answer
would refuse every real answer, and `supervision.status_answer` returns a structured reading
rather than prose. An occasion with no field it cannot do without would be a row that admits
anything, which is what that table exists not to be. So a parent answering a midpoint check is
carried by the envelope alone until somebody decides what an answer cannot do without.

### Ordering against parent-child traffic

1. The rows are disjoint and so are the claims. A supervisor message lives in its own table
   keyed by message id; the parent-child claim names `deliveries` by event id. Neither engine
   can claim the other's row. A supervisor message has a transport receipt like any send, and
   it has no completion receipt, no acknowledgement and no verdict, which is the difference
   that matters: nothing here is answered by the recipient's own record.
2. An event-derived report is staged only from a fact the store already holds as final - that
   derivation requires `stage = 'final'` and no suppression - so it cannot precede the record
   it reports. An omission is the other path and has no event in this store at all: it comes
   from a reporting observation - one a caller passes in, or one this store derives from the
   declarations the child's relay recorded here (see an omission this store derives) - and what
   vouches for it is that reading, which cannot precede the settlement it reads.
3. The per-recipient hourly bound is SHARED between the two queues on purpose. It bounds how
   often one task may be woken, and two queues feeding one task must not each get their own
   budget. The consequence, said rather than implied: where one task is both a parent and a
   supervisor, a report can wait behind parent-child traffic to that same task. The transport
   lock is shared in the same way within one adapter worker, which is where it lives; it is
   not a process-wide or durable lock.
   Both halves of the bound - the hourly count and the minimum gap between two sends - are
   decided by ONE predicate inside the write that spends the send: `delivery.reserve_send`,
   which the parent-child claim calls in its claim and this channel calls in the write that
   stamps its transport start. This channel's claim asks the same predicate
   (`delivery.send_refusal`) first and spends nothing. A bound that only one of its writers
   re-checks inside its write is a bound the other does not obey: a delivery claimed straight
   after a report never re-read the gap, and the two woke one task inside it.
   The gap is read across hour windows, because `last_send_at` lives on the hour's row and
   reading one hour let two sends a second apart straddle the boundary. Each sender also reads
   the same predicate before the host, and that is a preflight: two callers pass it at the
   same moment, so on its own it paces nothing.
   A claim refused on the budget is deferred by the same gap the preflight would have applied,
   in both queues. It is never recorded as a failure and never held, because nothing about the
   message or the recipient is wrong: another send simply got there first.
   This channel charges the budget at the later of its caller's instant and the clock read
   inside the transport-start write. The caller reads its instant before the host checks, and a
   charge dated that early let the gap and the hourly window lapse before the transport had
   even started. A send the budget refuses in that write is deferred the same way (see who
   moves a message out of sending). The parent-child claim charges its own instant and sends
   straight after the claim commits.
4. Within one recipient the oldest claimable message is claimed first, and an older message
   that is IN FLIGHT blocks the one behind it too - that is the moment ordering matters most.
   It is still weaker than arrival order, deliberately: an older message that is held, inside
   its backoff, or stranded with an expired lease does not block the one behind it, because a
   permanently held report would otherwise stop a project reporting anything at all.

### Durable staging

Staging freezes the packet before any transport call, so a crash between deciding and sending
loses the send and not the decision. The message id is the envelope's, derived from the
direction, the relation, the purpose and the subject, so one fact staged twice converges on one
row. In the same transaction it records the report in the existing `supervisor_report` journal,
which is what makes the next reading of that fact converge instead of waking the level above
again - and what keeps a report that exists from being invisible to the thing that decides
whether to produce one.

The packet and the evidence it points at are ONE fact, bound at staging and immutable after
it. A completion's packet freezes its work report's pull request and decision, and its evidence
pointer reads that event's report, so the message records the event and the report submission
it was composed from. The obligation handed in has to be the one its event raises in every
field `supervision._obligation` derives - its id alone let a caller's copy with another
generation or issue through - or staging refuses as `contradictory_observation`. Under the
staging lock the report is read again and has to be the same report WHOLE, because a
correction made in place keeps its submission number; the obligation is re-derived and compared
field by field; and the packet is composed again from what the store now says and has to be
the packet about to be frozen. Any difference refuses as `superseded_revision` with nothing
written. Once a message about the event has reached the transport - its attempt's transport
start is stamped - `report.record` refuses to change that event's report at all, in place, as a
new submission or as a first report, because the bytes that went up would otherwise say one
thing, or say there was no report, while their evidence reads another. Before that, the staged row is a proposal: a correction lands, and
the send carries it (see a staged row is a proposal). The stamp is written in the same write
that lets the transport start, so a correction and that write serialize: one committing first
is restated and sent, one committing after is refused.

A message staged from its event before any report existed froze no report, so the first report
on that event is recorded. Refusing it lost the report outright: a block staged from its outcome
could never be told it was a decision only the user can make. What goes out is decided again
where the transport starts (see a staged row is a proposal, below): a packet composed without
the report that now stands is restated in place and sent as that report, journalled as
`supervisor_message_restated` with both submissions. An event that now raises a different
obligation - the block that turned out to be a decision - holds its old
message as `superseded_by_report`, and the new obligation is staged as its own message. That
hold is derived, so it is not terminal. What it was derived from can move back - a report
corrected away from a block and then back to it, a confirmed Linear record whose target is
repointed - so the hold is re-derived whenever the message is staged or attempted, and released
on the same message once the obligation is owed through it again.

An omission's reading is checked against its obligation before anything is composed: the
schema is `reporting-observation/1`, the state is `unreported`, the relationship and the turn in
its selectors are the obligation's, and the obligation the reading raises is the one handed in,
field by field - the id omits the generation, so a reading from another generation of the same
turn passes every other check. Otherwise staging refuses as `contradictory_observation`.
`supervisor-stage --project` indexes the readings it is given by the obligation each raises,
and an obligation whose readings disagree about what it is or where it can be read is refused
by name under `refused` rather than staged with one of them.

### An omission this store derives

A turn that ended without a report writes no event, and the relay daemon reads no marker file
(hook-contract.md gives it nothing there), so from the store alone a turn that declared its
outcome looked exactly like one that declared nothing. Two records close that, both written
by the command that writes the marker fact, after it, mirroring the fact the marker then stands
on (`declarations.py`):

- `intent-claim` records in `reporting_sessions` that this session's relay writes its
  declarations into this store, with the marker root and workspace the claim used and the issue
  the intent was declared for - and only from a marker the marker reader could read at that
  moment: every fact readable and the right shape, the claim standing and correlated with an
  intent declared for that workspace. A marker `reporting-show` would answer `unmeasured` about
  records nothing, and the session stays a legacy admission;
- `intent-disposition` records the turn's declared outcome in `turn_declarations`,
  create-once like the marker file, holding the store's write lock across the marker
  publication and the record (`declarations.Held`): a staging, claim or transport start that
  could act on the turn's omission takes the same lock, so it waits and sees the declaration
  instead of reading the moment between the two.

The store to write is the one the coordinator recorded in the intent (`dbPath`), the same one
the Stop hook reads receipts from. Both commands answer with a `storeRecord` beside the marker
answer: `recorded`, `unchanged`, `conflict` (the first record stands, as it does in the
marker), `not_recorded` when the intent names no store or the store does not exist - there
is then nothing to derive from either - and `failed` when the store could not be written,
which exits 2 with the whole answer, because the marker fact was published and the caller has
to see both. Running the command again retries only the store record.

`omitted.derive` then reads one relationship's newest admitted turn from this store: the relay's
own settlement, the admission, the recorded declaration, the receipt where readiness was
declared, and whether a later turn was admitted. It hands those facts to `omitted.classify`,
the same predicate `reporting-show`'s reader (`omitted.observe`) hands its facts to; neither
reader classifies anything itself, and neither classifies a turn the other would refuse to place: the registration's identity - child, binding, issue, working directory and dispatch - is one predicate (`_identity_problem`) both apply first, the store reader against the workspace and issue its claim record carries. `tests/test_supervisor_omission_store.py` feeds one
fact set through both and compares the facts and the answer. The diagnosis is CRW-180's,
unchanged. Beside it the predicate answers whether a report is still OWED: not when the turn's
own final receipt exists (it goes upward as its own fact), not when a later turn was admitted
(the work went on; admissions are ordered by when the bound admission was made, to the
microsecond), and not inside `omission_grace_seconds` after the settlement (300 by
default), which gives the parent, or the child it steers, the chance to answer first. A
reading that owes nothing raises no obligation and is not a gap.

How soon an omission is owed depends on when the relay settles the turn, and that is not the
grace. A turn that ended with no receipt has no staged event to find it by, so the daemon reaches
it through its scan of admitted turns, which reads at most `share - 1` rows of
`generation_turns` - the whole table, every relationship's rows - on each visit to one
relationship. With `max_turn_reads_per_tick` 8 and `min_relationship_share` 2 that is four
relationships a tick and one row a visit, and an admission written after a pass fixed its last
row waits for the next pass. Measured on the live host on 2026-09-23, with 27 rows and 20 active
relationships (about 100 seconds a row): two turns that ended at about 16:56 UTC settled at 17:36
and 17:38, one that ended at about 17:05 settled at 18:24, and its omission was owed at 18:29 -
about 83 minutes after the turn ended, of which the grace is 5. A turn that carried a receipt
settled within 90 seconds, because its staged event puts it in the ring directly. The scan belongs
to the observation pass and not to this channel; making it cheaper - reading one relationship's
own admissions through an index, or serving eligible admissions before rotating - is the named
follow-up "supervisor omission latency", and until then an omission's wake is bounded by that scan
rather than by its grace.

The readers still read different sources, and where the marker and the store disagree about
the assignment itself each answers from its own. A marker that never received
`intent-register` leaves the marker reader without the relationship id it reads the store
with, so it answers `unmeasured`; the store reader finds the registration through the dispatch
the claim recorded, and the Stop hook already reads that turn as `managed_unregistered`, an
omission. A marker registration naming another generation than the store's is read by the
marker reader as a receipt that cannot count, and by the store reader against the registration
in this store; that direction wakes nobody.

A marker that stops reading AFTER the claim - a fact rewritten out of shape, a file that can
no longer be read - is one the store reader cannot see: the daemon reads no marker file. The
store reader then answers from the registration and the declarations recorded here, which the
marker's health does not change, while `reporting-show` answers `unmeasured`. The Stop hook
releases a turn it cannot read the marker for rather than prompting the child to declare, so an
omission derived then is a turn that really ended without a declaration.

The cut-over is the claim record. A turn whose session has no `reporting_sessions` row -
every child that claimed before its relay wrote here, or through a relay that does not - is a
legacy admission: its declarations may exist only in the marker, so the store's silence proves
nothing, `derive` answers `unmeasured` / `declarations_not_recorded`, and nothing is staged
or sent for it automatically. It stays owed and visible exactly as before, through a reading
somebody passes in.

What the store derives is staged like any omission: with its reading, frozen on the row, now
carrying `source: relay_store`, and `supervisor-show` prints `reporting-derive` as its
recheck. `supervisor-standing`, `supervisor-stage --project` and the daemon's pass all add
the store's owed readings beside a caller's; a caller's reading of the same obligation is kept
and the store's left out, because one omission travels with one reading. The two readings of
one omission share its message id, and where they agree field for field they are one reading
as far as staging is concerned, so a parent's `reporting-show` staging and the daemon's
derivation converge on one message.

Where the turn's session records its declarations here, the store decides for EVERY omission,
whatever reading it arrives with: staging asks it under the write lock and the transport start
asks it again (`_omission_withdrawn`, from `_proposal_now`). A parent's `reporting-show`
reading taken before the child declared the turn in progress is a proposal like any other; the
staging refuses as `not_claimable`, or the message is held `superseded_by_report` at its
transport start, and nobody is woken. Where the session records nothing here - a legacy
admission - the store cannot see the declaration, and a caller's frozen reading stands as it
always has, checked against the turn's final receipt.

### A staged row is a proposal

A staged row is a proposal, not a commitment. Transport start re-derives the obligation's
CURRENT content inside the same write that lets the transport start, and sends only if the
staged row equals it; otherwise the attempt is voided, recorded with its reason, and the row is
restated or held. Stale bytes never go out, and one obligation never wakes anybody twice. One
function asks it, `_proposal_now`, and it asks what staging would stage for this obligation
now:

- who it is for: the live hierarchy, through `resolve()`;
- what it is: the obligation the row's event raises from that event's current work report, or
  the one the reading frozen on an omission's row raises - and, for a reading this store
  derived, the store's derivation of that same turn now, so a declaration recorded since, a
  later admitted turn or the turn's own receipt leaves nothing owed through the message;
- which statement: the newest event raising the obligation, found by the obligation's key
  rather than by asking the event the row was staged from first - that event can stop raising
  it while a newer statement of the same block still does - and that event's current report;
- whether it is still owed: a confirmed Linear record discharges it, and a final receipt for
  an omitted turn means the turn reported;
- the bytes: the packet composed from all of that with the observation time the row was staged
  with, compared with the row byte for byte, so anything composition reads that the list above
  does not name cannot drift past it either.

The claim asks it inside its write and the write that stamps `transport_started_at` asks it
again; that write is the last one before the bytes go out, and nothing reaches the transport
without passing it. What it answers decides one of three things. When the hierarchy moved,
nothing is sent, the attempt is recorded as sending nothing, and staging again re-addresses the
report (below). When nothing is owed through this message any more - the event now raises
another obligation or none, the record discharges it, the omitted turn reported - nothing is
sent, the message is held as `superseded_by_report`, and the send refuses as
`superseded_revision`. When the obligation says something newer, the never-sent row is
restated in place - same message id, same journal entry, `supervisor_message_restated` naming
both events and submissions - in the claim before its bytes are rendered, or at the transport
start with the voided attempt recorded as sending nothing, after which the same call claims it
again, at most twice, and sends what is owed now.

The writers that can change an obligation after staging are the ones it reads: `report.record`
(a first, corrected or kind-changing work report), the receipt intake (a newer statement of the
same block or decision, or a final receipt for an omitted turn), the linkage (a handover, an
archived assignment, a project re-linked, a contested edge) and the sync outbox confirming a
verdict. The authorized settings the send carries are asked again in the same write too: a
change since the claim sends nothing and queues the message, and the next attempt reads them as
they stand.

### When the hierarchy moves under a staged report

The message id is the fact's and the endpoints are the hierarchy's, so a handover moves the
second from under the first. Returning the frozen row as it stood left a report addressed to a
supervisor who had stepped down: `attempt()` refused it as drift, and the `supervisor_report`
entry kept a second report from being produced, so the successor was never told.

Staging again recovers it, under one rule. A message none of whose attempts can have sent
anything has not been seen by anybody - its bytes are rendered inside the claim, and an attempt
recorded as `sendAttempted: no` and retry-safe put them nowhere - so it is re-addressed in
place: the same id and the same journal entry, with the live sender, recipient, project key and
a recomposed packet, and a `supervisor_message_readdressed` entry naming both hierarchies. The
condition is a predicate inside the write, and the hierarchy is resolved again under that same
lock, so a claim that commits first wins and the re-address becomes a refusal. What the former
recipient's state decided goes with it: a lifecycle recheck, a backoff and a busy cap were
bounds about that task, and the busy count restarts from the re-address.

A report goes to whoever supervises at its TRANSPORT INSTANT, and the write that stamps that
instant is where the question is asked last. It checks, under the lock, that the send's claim
still holds the row, that the row still names the task the caller is about to send to, and that
the hierarchy the message names is still the live one - by asking `resolve()` itself inside that
write, the same question the claim asks inside its own. A predicate written beside the resolver
compared only the two owner bindings, and an assignment archived meanwhile, a project moved
under another initiative or a drifting edge each passed it while `resolve()` refuses them. The claim itself refuses a row whose
endpoints are not the ones its caller observed, so a re-address landing between the reads and
the claim sends nothing, and the next attempt reads the row as it stands. A hierarchy
that moved after the claim and before that write finds nothing sent: the attempt is recorded as
one that sent nothing, the message goes back to `queued`, the send raises the refusal
`resolve()` gave - `relation_owner_drift` for a handover, `unregistered_scope` for an assignment
archived meanwhile - and staging again re-addresses the report to whoever the linkage names
then. A handover that commits after the stamp finds a
report already on its way to the supervisor who was live when it started, and that report stays
with the task it went to: its attempts describe bytes that went there, and moving the row would
have them describe a recipient they were never sent to. Staging it again refuses as
`relation_owner_drift` and names both hierarchies, which is how `supervisor-stage --project`
reports it under `refused`; the successor has not been told through this channel, and the
obligation stands until the Linear record confirms it. Sending never re-addresses anything;
which task a report is for is decided where it is staged.

A message the hierarchy gives no addressee does not hold the queue. The claim keeps
per-recipient order - a message waits while an older one to the same task can go - so an
unsent message whose `resolve()` refuses (an archived assignment) or names other endpoints than
the row does was, left claimable, the oldest message to its recipient for good, and every later
report to that task waited behind it. `attempt()` holds it as `hierarchy_unresolved` when it
meets either refusal. The hold is derived: staging and the next attempt release it on the same
message once `resolve()` names its endpoints again, and staging re-addresses one that was never
sent. Order is kept among the messages that have an addressee.

### Who moves a message out of sending

A message leaves `sending` only through the claim that holds it: this message, this attempt
number and this lease owner, all three in the predicate of the write that moves it. `_settle`
records the transport's answer that way, and the transport-start write that finds the hierarchy
moved, the fact the packet was composed from moved, the settings changed, or the send budget spent, releases the
message the same way. The one other way out is
recovery, which takes the
row from a claim whose lease expired with no receipt, and what it does depends on a durable
fact. `transport_started_at` is stamped only by the claim holding the row, inside the write
that checks it still does, and committed before the transport is called. The stamp is taken as the last thing that write does, after the lock is granted and the
hierarchy asked, because a time read before waiting for the lock dated the start early. An
attempt with no stamp therefore sent nothing and never will - its owner's own transport-start write now finds
the row is not its own - so recovery records it as sending nothing and queues the report again.
An attempt with a stamp may have sent, so recovery moves the message to `held_uncertain`,
because nothing observed what that send did.

No exit between a claim and its transport has anything to give back, because nothing before
the transport start spends the recipient's budget. The claim only asks the budget it shares
with parent-child deliveries; the transport-start write spends one send, in the same write as
the stamp and after the lock and the hierarchy check. A hierarchy that moved, or a lease
recovered before the transport started, therefore leaves the budget as it was. If the budget
refuses in that write - a parent-child delivery spent it after this claim - the attempt is
recorded as sending nothing, the message is queued again past the gap under this claim's own
state, attempt and lease owner, and `supervisor_message_paced` is journalled: deferred, never
failed and never held. Reserving at the claim and giving the send back on each exit could not
be done exactly: two claims given back out of order left a send time behind with no send under
it, and the next real send waited for it.

Once recovery has declared an attempt uncertain, nothing that attempt's late receipt says moves
the message. The receipt is still recorded on its attempt row - the attempt history keeps what
the transport said - and the `supervisor_message_attempted` journal entry says
`messageMoved: false` and the state the message stayed in; `supervisor-send` answers the same
beside the receipt as `messageState`. Only a verified readback moves `held_uncertain`. Matching
`held_uncertain` in the settlement write let a late retry-safe refusal make the message
claimable again, so a second attempt could wake the recipient while the first one's outcome was
still unknown, and let a late success promote it without anybody reading anything back.

Expiry alone ends nothing. A claim whose lease ran out and that nobody has recovered still
holds the row, and its own receipt is still the best fact there is about its send, so it
settles normally; whichever of the settlement and a recovery commits first decides.

### The readback, and exactly what it establishes

A delivered message is not a read one. The message asks the recipient to answer from inside its
own turn with `sha256(messageId|<its own turn id>)`. That is an INSTRUCTION and not an enforced
property: the command receives a deterministic hash, nothing authenticates the caller, and
nothing establishes that the named turn produced it. What the proof does rule out is an echo:
the delivered bytes carry the message id, because a recipient has to be able to quote it, and
they cannot carry the turn id, because that turn does not exist until the message arrives. That
is the whole of what it rules out, and it is the property `ack.acknowledge` already rests on.

One rule decides every verification: **a readback verifies only on evidence bound to THIS
attempt** - measured from the instant this attempt's transport started, and found in a turn
the host names on the recipient's thread. A link in that chain the host cannot establish does
not verify. `host_read` needs all of the following:

- the host can read the named turn on the recipient's thread and it carries a start time. The
  read is thread-scoped, so it establishes membership as well as existence; asking a bounded
  LISTING first would have answered `turn_not_found` for a real turn a busy recipient had
  pushed off the end of it.
- the named turn is not CERTAINLY earlier than this attempt's `transport_started_at`, the
  instant stamped immediately before the transport was called - a start that precedes it by
  more than the host's timestamp precision is refused. The claim time is not that instant,
  because settings, lock contention and scheduling separate the two, and an attempt with no
  transport instant at all, claimed and never sent, verifies nothing. This applies to every
  candidate, including the turn the attempt names, because a send can STEER an existing turn
  rather than open one.
- a bounded scan of the recipient's items - at most 200 - found THIS attempt's delivery token,
  in a turn the host names. The token is the request id and a random part drawn inside the
  claim and rendered into that attempt's bytes alone. The request id by itself is derived from
  the message and the attempt number, so a copy of it could be written into the recipient's
  thread ahead of the send - half a second ahead passed even the chronology below - and after
  a lost response that copy verified a readback for bytes that never arrived. Nothing written
  before the claim can contain the token; whoever holds the store can read it once the claim
  commits, which is the authority bound this readback already records. The frozen message is never compared, and what the scan is good for is
  that it does not come from our own send receipt. A token the host places in no turn is tied
  to nothing and answers `transcript_unconfirmed`.
- the named turn is that turn, or does not certainly begin before it. The transport can hold a
  message after it is called, so a turn opened in that interval followed the stamp and still
  came before the bytes: where the message landed is the later bound. If the host gives no
  start for the turn it landed in, the readback does not verify.
- where the named turn is the one the SEND opened, the token is in that same turn: what the
  token is IN is where the message landed, so a disagreement answers
  `transcript_turn_mismatch`. A turn the recipient opened afterwards is not expected to carry
  the token, and is the stronger reading anyway, because the sender never knew its id.
- the turn the token is in does not certainly begin before this attempt's transport started,
  unless it is the turn the transport itself reported - a steered turn is older than the send,
  and the receipt says the bytes went there. An older token answers `turn_predates_send`, and
  an unknown start is not verified.

Every answer to one readback has one shape - the first, a later one answered from the settled
row, and one that lost the race to settle it - built from the stored row, with `recorded` and
`raced` saying which it was.

Wherever the channel says a message was read, the turn's origin stands beside it, because the
state's name is shorter than what it proves. The readback answer carries `turnOrigin` and
`establishes`; `supervisor-show` carries `turnOrigin` and `readEstablishes` next to a `read`
state and the same two on the readback; the reach ladder's `received` detail names the origin.
For `relay_opened` what is established is arrival only. A `recipient_opened` readback is the
only one that involves a turn the recipient's thread opened after the send, and even that does
not say who wrote the answer.

It does not say who wrote the answer, that the turn answered anything, or that the supervisor
acted. The turn a send opens is one the sender already knows the id of, so a readback from that
turn rests on nothing the sender could not have produced alone - and that is the ORDINARY case,
because the message is what wakes the supervisor. So which turn was named is recorded as
`relay_opened`, `recipient_opened` or `unknown` rather than averaged into one word, and a
reader can see how much was established instead of being told a number: `relay_opened` shows
arrival and nothing more, and not even `recipient_opened` shows who wrote the answer.
`unknown` is reachable: the origin is named only when the attempt carries a turn id, and a send
nobody heard back from carries none, so a readback that settles one answers `unknown`.

There are six verification answers: `host_read`, `transcript_unconfirmed` for a real turn whose
transcript scan did not confirm the message, `transcript_turn_mismatch` where the readback
names the turn the send opened and the token is in another, `turn_not_found`,
`turn_predates_send`, and `unverified_turn` where no host could be read at all.

The four facts are read at different moments. The turn is read first and the transcript
scanned after it, each through its own paged host calls, and the host offers no snapshot or
revision that could tie the two to one observation. The join is sound because both facts are
about append-only history - a turn keeps its id and start time once it has them, and an item
keeps its text and its turn - and it is blind to a host that rewrites history between the two
reads, such as a rollback that removes the turn or the item. `host_read` does not claim more.

A readback that does not verify is written down, on the path that inserts or updates a row -
the one taken when no settled `host_read` row exists yet. Hiding it would lose the fact that
somebody answered; what it does not do is move the message to read. Once a settled row exists,
a later readback is answered from it on a fast path BEFORE the proof is checked, so it is
neither verified nor recorded, and a settled verdict cannot be replaced - the guard for that is
the re-read inside the write transaction, not the check before it.

The write also asks whether the message is still the one the checks were made against. They
run outside the lock, so a message whose state or attempt count moved in between refuses and
records nothing, and the answer is to ask again.

A message held uncertain is read back as well. Its send's response was lost, or its sender died
between the claim and the receipt, and nothing else reconciles this queue, so refusing it here
made the one check that could prove this attempt's delivery token reached the recipient's thread
unreachable. It is verified against THAT attempt, the one numbered by the message's attempt
count, found by its number because a late receipt may have been recorded on it. Verified means a
real turn on the recipient's thread that did not begin before the send, and that attempt's
delivery token in the recipient's own transcript, in a turn - the named one included - that
began no earlier than the transport, measured without the precision allowance the other
chronologies get. Settling is the one verdict that turns an unknown outcome into `read`, so it
takes no benefit of the doubt: a token placed half a second ahead of the transport does not
settle it, and a genuine turn the host dates just before the stamp leaves the report held for
manual settlement. Only then does the readback settle it
to `read`, with a `supervisor_message_reconciled` journal entry and a `reconciled` field on
the readback saying so. The answer alone never settles it; an unverified readback is recorded
and the message stays `held_uncertain`. With no receipt the attempt keeps `held_uncertain`,
so the reach ladder reads `transport_accepted` as unmeasured - the transport never answered -
while `received` is answered from the readback; a receipt that arrived after the recovery is
what the attempt then says, and the ladder reads it.

### The five stages

`envelope.REACH_SOURCES` changes for this direction, because the table is data precisely so it
can say what is true once a channel exists:

| Stage | Answered by |
|---|---|
| transport_accepted | `supervisor_attempts` |
| received | `supervisor_readbacks` |
| agreed | nothing |
| applied | nothing |
| verified | nothing |

The three with nothing stay `not_applicable` whatever happens on this channel, and
`check_reach` still refuses a caller that hands the contract a supervisor acknowledgement
sourced from `acks`. What discharges a reporting obligation is unchanged: the Linear record the
supervisor reads for itself, confirmed.

### A fault notification

`stage_notice` freezes a notification the fault ledger reserved as one message of kind
`fault_notification`: its obligation id is the notification id, its subject the notification's
`deliveryKey` and its envelope relation the fault (`fault:<id>`), so its message id is the
notification's own. A second staging - another process, a restart, another relationship
addressing it - finds the same row, and the store refuses a second one by a unique index
(`supervisor_messages_one_notice`). It is addressed by `resolve()` from the relationship the
notification is about now (`anchor_relationship`, never one registered under another project
than the fault's scope names), like a report, or - for a fault no relationship holds, or one scoped to another project -
from the project its scope names: `resolve("project:<key>")` walks up from that project's one live owner,
and refuses as it refuses a relationship (no owner, two owners, no supervisor, a contested
walk). Such a row's `relationship_id` holds that `project:<key>` anchor; a relationship id never
takes that shape. A row none of whose attempts can have sent is
restated, re-addressed (to another relationship or project too) or released from its park by the next
staging; one that may have been sent is returned as it is. A re-address to another sender,
supervisor or project releases the former recipient's bounds (a cap, a recheck, a backoff), as
`_readdress` does for a report; a notice whose anchor moved under the same hierarchy keeps them.
The issue is optional in both of its
purposes (`fault_notice`, `fault_decision`): a fault about a project and no issue still goes.

A notice is owed only while its notification is reserved under a live lease, because that
reservation is where the ledger decided its eligibility and spent its budget. That is its I-247
check (`_notice_now`, through `_proposal_now` at the claim and again where the transport starts):
a notice whose notification is pending, uncertain, delivered or lapsed is held
(`superseded_by_report`), never sent, as is a blocking notice whose fault withdrew and one the
ledger's eligibility (`faults.notification_eligibility`, the rule its reservation asked) no longer
allows - a pause or no-contact committed after the reservation; one whose fault
is about another relationship now is not claimed (and a parked one stays parked) until its next
staging re-addresses it; and a reserved one is recomposed from what the ledger says now and
restated if its fault moved. A notice whose attempt sent nothing is parked under the same
hold (`park_notice`), so it is never the oldest message holding its recipient's later reports back,
and the next reservation's staging releases it. Every other rule here holds unchanged: lifecycle
withholding, the busy backoff and caps, the recipient's send budget, one head per recipient in
staging order, uncertain sends held until a readback. The deliverer that reserves and settles it
lives in `faultnotice` and is described in [faults](faults.md).

## The surface

```bash
# Freeze what is owed upward. Staging is not sending.
codex-session-relay supervisor-stage --event <id> [--recipient <supervisor task>]
codex-session-relay supervisor-stage --project <key> [--observation <file>]...
codex-session-relay supervisor-stage --observation <file>

# One attempt, through the same host rules a delivery obeys. --socket and --state are global,
# so they come before the subcommand; after it argparse refuses.
codex-session-relay [--state <dir>] --socket <path> supervisor-send --message <id>

# The recipient answering, with the line the message carries: it names the store the report
# was staged in and the socket it was sent through. The message ASKS for a turn id of the
# recipient's own; nothing enforces that. --as is required and checked against the recipient.
codex-session-relay --state <dir> --socket <path> supervisor-read --message <id> \
  --turn <turn> --proof <p> --as <your task id>

# What was staged, every attempt, and what came back.
codex-session-relay --state <dir> supervisor-show --message <id>
```

`supervisor-stage` and `supervisor-show` reach no host. The other two are host-required and
refuse without `--socket`, exiting 4 with usage JSON and writing nothing to the channel - the
list of host-required commands is what `doctor` reports and enforces nothing, so the refusal
is its own check. Supplying the option proves an argument was supplied, not that a host is
reachable.

The line the message itself renders is this one with everything the relay already knows filled
in - the program that runs this relay (see every line it writes selects the store it was written
from), the store directory and the socket included - leaving two placeholders, `YOUR_TURN_ID`
and `YOUR_PROOF`, and a third, `YOUR_RELAY_SOCKET`, only when it was rendered without a socket.
It asks the recipient to record that the report reached its thread, and says that a readback
never records that anybody read, agreed to or acted on anything. Every command a report
carries - that line, the evidence pointer and the `supervisor-show` line - is built from its
arguments with `shlex.quote` rather than by concatenation, because the evidence selectors are
paths and names a caller chose and a workspace such as `/tmp/My Project` was two arguments.
`tests/test_supervisor_channel.py` splits each one with `shlex.split`, parses it with the real
parser, selects with the real `Services` under a default that points somewhere else, and runs
it against the row it names.

Neither reaches the host unconditionally. `supervisor-send` returns `sent: false` without
touching the adapter when the message is held, inside its backoff, or already sent, and only
resumes the supervisor's thread past those. `supervisor-read` checks the host's turn list and
the recipient's transcript only when the message has no settled readback; with one it answers
from the stored row before the proof is checked and without using an adapter at all.

`sent` is read off the transport receipt rather than asserted. An attempt that was made and
refused answers `attempted: true` with `sent: false` and the receipt's own `sendAttempted`,
because reporting a refusal as a delivery is the reading this command exists to prevent.

A recipient the host says cannot receive - archived, paused, usage-limited - gets a recheck
time and a journal entry, never a hold: that state is one somebody can undo, and a held row is
skipped by every later attempt, so recording it as a hold would mean recovering the recipient
never released the report. A busy one is deferred with a backoff that grows and a cap that is
counted in the journal, because the attempt counter only moves inside the claim a busy
recipient never reaches.

A supervisor the host has unloaded is loaded, not skipped. Its pair is the user's own selection,
so it is never transmitted: a resume can apply what it transmits while the host materializes a
thread, which would restore a model the user has since changed. That used to mean it was never
sent to either - the delivery gate refused an unloaded record-based recipient before the
transport, and the live host unloads an idle thread within about a minute, so a report waited
until somebody else loaded the supervisor (CRW-215 live finding F1). The transport now resumes
such a recipient, loaded or not, with nothing requested - `{threadId, excludeTurns}`, the
bridge's own nothing-requested form - so an unloaded thread loads under its own persisted state
and a loaded one reports it, and that answer is compared with the recorded authorization before
any turn. The model, the effort, the whole sandbox policy, the approval policy, the cwd and the
environment selection must equal the record. A difference refuses as
`settings_differ_after_load`, retry-safe and with no turn started, and since nothing was
transmitted that could have caused it, the remedy is to re-record the supervisor from a reading
the user stands behind. The workspace roots are the one relaxation: a load restores only what the
host persists, and on the live host it brought the supervisor back with its roots reduced to its
cwd while every other field held (CRW-215 live finding F2), so after such a resume the roots - at
the top level and in each environment - may be narrower than recorded, never wider.
"Narrower" is a question about lists of paths and agreement is a question about values, so both
sides are held to the shapes the comparison reads. The recorder refuses as `settings_mistyped`
roots that are not a list of text, and environments that are not a list of objects with a text
id, a text cwd and roots that are absent (they default to the cwd) or a list of text - null is
neither; and it refuses as `unsupported_sandbox_type` a sandbox any of whose declared fields does
not hold its declared default's type - a flag that is not a boolean, `writableRoots` that are
not a list of text. A record holding its roots as the text "/a/bc" used to pass and was compared
as its characters, so a host root "/" read as one it names. A host answer of any other shape is a
refusal before any turn on both resume routes (`setting_unobservable`, or a difference for an
unreadable sandbox), where it used to raise and be recorded as an unknown outcome. Every
comparison between the record and the answer is made on JSON values, where 0 and false differ;
Python's equality said they agreed. A key the pinned contract does not declare - in the sandbox
policy or in an environment, which is compared whole - and the permission profile, which is the
host's own value carried whole, have no type to hold them to and are compared exactly; a
recorded profile the answer does not report, or reports as null, is a refusal like any other
absent answer. What this verifies is the record's host settings, the fields the resume contract
defines: the sandbox, the approval policy, the cwd, the roots, the model, the effort, the
environments and the permission profile. The relay's own keys in a record (`citedRole`,
`citedException`) are checked by the role policy, and any other top-level key is neither
transmitted nor compared. A parent's or a child's pair, which policy derived, is
resumed exactly as before, carrying its settings. The bridge's own tool path still refuses the unloaded case, because it reads no binding and does not
load a thread without transmitting, so an operator message to an unloaded supervisor through it
still waits for the host to load that thread.

`supervisor-stage` refuses `--event` with `--observation` and `--project` with `--recipient`
rather than ignoring the one it cannot use.

It also refuses an omission staged without the reading that found it. The obligation names a
relationship and a turn; `reporting-show` needs a state directory, a marker root, a workspace,
an assignment and a session too, so a line built from the obligation alone looked like a
command and could not be run. With the reading, the reading is frozen on the row and the
recheck line in `supervisor-show` is rendered whole.

## What a readback is not

It carries no supervisor authority, and the bound on that is worth stating exactly rather than
in general. `host_read` takes four answers from the host about the RECIPIENT's thread and
nothing else: the named turn is readable there, its start is not certainly before the send,
this attempt's delivery token is in the thread's transcript, and - where the named turn is the one
the send opened - the token is in that turn. A caller with this store and no host gets
`unverified_turn`, which is recorded and leaves the message where it was.

A caller who can read this store and reach the host needs nothing more. The turn a send opens
is on the recipient's thread, holds the token, and has its id in `supervisor_attempts`, and the
proof is computed from that id and the message id. So a readback naming that turn -
`turnOrigin: relay_opened`, the ordinary case - verifies without any act of the supervisor's,
and what it establishes is that the message ARRIVED where the recipient reads, not that anybody
read it. A readback naming a turn the relay did not open - `recipient_opened` - needs a turn
opened on the recipient's thread after the send, which the supervisor does by answering and
which an operator who can drive that thread could do as well. Neither says who computed the
proof. Anyone able to do either can already write the row directly, because the store is a
file and not a service with callers to authenticate, so nothing on this side can tell them
apart; closing that takes an authenticated caller, which is a relay-wide change and not this
channel's to make. What the channel does inside its own reach is record who ASSERTED the
readback and refuse an assertion that does not name the message's recipient - a declaration,
written down as one, and calling it anything stronger would be the kind of claim the rest of
this document exists to avoid.

Read `received` on the reach ladder as "a readback was recorded, the named turn is real on the
recipient's thread, and its transcript holds this attempt's delivery token", with `turnOrigin`
read beside it. It is not "the supervisor read it" and not "the supervisor acted", and the
obligation is not discharged by it.

## What this does not do

Nothing here wakes anybody on a timer. The daemon's supervisor pass runs on the daemon's tick
and sends a message once, for an owed fact, under the pacing and lifecycle rules above; a tick
with nothing owed sends nothing and writes nothing. An uncertain send is never retried automatically either,
because there is no reconciler for this queue and a second send that lands is a second wake for
one fact; it stays `held_uncertain`, which is not claimable, and says so, until a verified
readback of that attempt settles it.

Nothing else settles it. There is no reconciler for this queue, so a held message whose
supervisor never reads it back stays held until somebody opens `supervisor-show` and decides
what to do. That is manual settlement, and it is deliberate: the only automatic way to find out
whether an unanswered send landed would be to send it again.

A send interrupted between its transport start and its receipt is the same answer reached a
different way. The claim commits first, so a process that stops existing in between leaves the
row in `sending` with a lease nobody will settle. An expired lease moves it to
`held_uncertain`, which authorises no resend, because nothing observed what that send did. A
process that stopped between its claim and its transport start sent nothing, and the recovery
queues that report again; it spent no send, because the budget is spent in the same write as
the transport-start stamp.
`stranded()` lists such rows and is a READ: there is no sweep behind it, and a row is moved
when `attempt()` or `read_back()` is called for that message again. The lease is re-checked
inside that move, and a receipt that arrives after it is recorded on the attempt without moving
a message its sender no longer owns (see who moves a message out of sending, above).

Staging decides whether anything is owed a second time under the write lock. The reading
`supervision.select` takes before it can be overtaken by a `supervisor-report-recorded`
committing in between, and the insert then stood beside a journal entry that already said the
report existed. Under the lock, an obligation already reported or discharged refuses as
`not_claimable` and nothing is written.

A block or a decision goes up as its newest statement. Both are keyed on their generation and
their cause, so the child stating the same one again - with its evidence corrected, say - raises
the same obligation from a newer event. Staging composes from the newest event that raises it -
newest by the order this store accepted them, because two accepted at one instant tie on time -
whichever statement the caller held, and a message staged from an earlier statement and never
sent is restated in place, inside the same lock the staging checks ran in: same message, same journal entry, a `supervisor_message_restated`
entry naming both events, and the recipient's own bounds kept. Once an attempt may have sent,
the newer statement is the same fact said again and is not reported again; staging answers
that, naming the newer event's own evidence. A completion is keyed on its event, so a corrected
completion is always its own message.

A send never goes out as an older statement either. The transport start re-derives the newest
statement (see a staged row is a proposal), and a message staged from an earlier one is
restated in place and sent as the newer one. Without that, a restatement accepted after staging
and before the send went nowhere: the older statement was sent, and the newer one was then
answered as a fact already reported.

A report still owed for an archived assignment has nobody to go to. `resolve()` finds the
project by walking up the edge the assignment holds on its issue, and archiving the assignment
releases that edge, so staging and sending refuse as `unregistered_scope` from then on, and
the unsent message is held as `hierarchy_unresolved` so later reports to the same supervisor
do not wait behind it. The
obligation keeps standing and `supervisor-standing` keeps listing it; through this channel it
goes out only if it is staged and sent before the assignment is archived. A superseded
assignment is not affected, because its successor holds the edge.

A push the recipient's policy refuses is not a send here. The transport answers `inbox_only`
when the recipient's thread reports an approval policy it cannot serve, after the resume and
before any turn, so nothing reached the supervisor's thread. For parent-child traffic that
answer names the durable inbox item the child reads; this channel has no inbox anybody is told
to read, so the attempt is recorded as a refusal before sending, retry-safe, with the
transport's own answer beside it, `supervisor-send` says `sent: false`, and the message waits
out its backoff so a policy restored on the recipient lets the next attempt send it.

A report goes through the socket its caller names, whatever host the supervisor is registered
on - the rule parent-child delivery follows too. The relay records each endpoint's `hostId` for
routing and audit and has no map from a host id to a socket, so the supervisor has to be
reachable through the socket the sender uses, which today is the one App Server the relay runs
against. A socket whose App Server does not know the recipient's thread reads its lifecycle as
unknown, and the send is withheld with no hold, so the next attempt through a socket that can
see the recipient sends it; nothing is recorded as sent, and a readback through such a socket
cannot verify, because it needs the host to read the named turn on the recipient's thread.

An omission derived from the store rests on the child's relay having recorded its declarations
there. A store record that failed is reported and exits 2, and a child that carries on
regardless leaves the store without the declaration its marker holds; so does a child that
claimed through a relay carrying this change and later declared through one without it. Either
way the store can derive an omission the marker would not, and the grace is all that stands
between that and a wake. The store also has nothing like the marker's Stop record: its witness
that the turn ended is the relay's own settlement read beside the recorded declaration, so a
child whose Stop hook never fired is derived where `reporting-show` answers `stop_unobserved`.

## Who calls it today

The relay daemon does, on every tick, with nobody asking. Its supervisor pass
(`RelayDaemon._report_upward`) stages what each project owes - the same staging a parent runs
by hand, restricted to obligations whose message is absent or still unsent - and attempts each
recipient's oldest eligible message through `SupervisorChannel.attempt`, a page of them at a time
starting after the last one the previous tick considered, so neither one recipient's backlog nor
a page of recipients that are never sendable can keep a later recipient's report unread; an attempt that raises
is deferred by the recheck interval, and only an attempt that reached the claim and the
transport spends the send budget, so a recipient that is never sendable cannot take every tick's
budget however far apart ticks are. So every rule above holds
for it unchanged: one obligation is one message and one wake, what goes out is re-derived where
the transport starts (I-247), the recipient's budget is shared with parent-child traffic and
spent only at the transport start, a paused, archived or unreachable supervisor is withheld
rather than woken and keeps the obligation, an unloaded one is loaded with nothing transmitted
and compared with its record before any turn, and a message another caller has claimed is left
alone. It is bounded like the daemon's other passes, by `max_supervisor_projects_per_tick` and
`max_supervisor_sends_per_tick`, project keys are read a page at a time, and a project whose
messages have all gone out costs reads and no write. Within a project it reads the project's
whole history, the read `supervisor-standing` makes, so it is bounded in projects and sends per
tick and not in the length of one project's history.

The commands stay valid and are what a parent runs when it wants an answer now:
`supervisor-stage`, `supervisor-send`, `supervisor-read` and `supervisor-show`, beside the
readings `supervisor-select`, `supervisor-standing` and `supervisor-report-recorded`. A parent
that stages or sends by hand converges on the same message ids as the daemon, and whichever
claims a message first sends it; the other finds nothing to send. The supervisor still answers
with `supervisor-read` from a turn of its own - nothing reads back on its behalf.

An omission is staged by the pass too, from this store (see an omission this store derives):
once its grace has passed, for a turn whose child's relay recorded its claim here. The daemon
still reads no marker file; what it reads is what the child's own relay wrote into the store
beside the marker. A legacy admission - a child that claimed without that record - is never
staged automatically, and stays owed and visible in `supervisor-standing` whenever a
`reporting-show` reading of it is passed in, as before.

The same pass then delivers fault notifications, when the daemon holds a fault ledger: after
the reports, so a notice staged now is younger than every report already waiting for the same
supervisor, and within the same per-tick send cap: the notices get what the reports left of
`max_supervisor_sends_per_tick`. `faultnotice.NoticeDeliverer` reserves what the ledger says may
go, stages each as a notice and attempts it through the same `attempt`, and settles the notification from what the
channel recorded (see [faults](faults.md), who tells the level above).

## Scope of these claims

Source-implemented and covered by `tests/test_supervisor_channel.py`, which stages, sends and
reads back against this package's own fake host, and by `tests/test_supervisor_autosend.py` and
`tests/test_supervisor_omission_store.py`, which run the daemon tick and the child's commands
against it. That is evidence about this source and about
that host. It is not an installed runtime, an activated service, or any report reaching any
supervisor on any machine. Whether a real parent stages and sends a report, a real supervisor
thread receives it, and a real supervisor reads it back is the live round trip, which is run
after installation and is not shown by this source or its CI.

It has been run once, on the installed runtime at b19b3fd2 (CRW-215, second live window): a
completion and a deliberately omitted report went up on the daemon's own ticks, reached a real
supervisor task, and were read back `recipient_opened` from a later turn of that supervisor's
own. It got there only past the four limits this revision addresses - an unloaded supervisor
never sent to (F1), a load that narrowed the roots (F2), a rendered line PATH resolved to another
relay (F3), an omission delivered as a block (F4) - and it measured the latency bound recorded
above. What this revision changes is source and fake-host evidence until it is installed and
those live steps are run again.
