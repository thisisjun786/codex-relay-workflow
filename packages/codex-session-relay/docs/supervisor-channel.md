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
cannot change under the packet: an event's work report stops changing once a message names it,
and an omission's reading is frozen on the message row when it is staged. The command that
produced that reading, `reporting-show`, re-reads the turn NOW, so a report that reached the
turn afterwards made it answer `reported` under a packet saying `unreported`; `supervisor-show`
prints it as `stagedFrom.recheck` beside the frozen `stagedFrom.reading`, and the two are
allowed to disagree. A second, different reading of an omission that is already staged is
refused as `contradictory_observation`, because one message keeps one reading.

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
   from a reporting observation the caller passes in, and what vouches for it is that reading.
3. The per-recipient hourly bound is SHARED between the two queues on purpose. It bounds how
   often one task may be woken, and two queues feeding one task must not each get their own
   budget. The consequence, said rather than implied: where one task is both a parent and a
   supervisor, a report can wait behind parent-child traffic to that same task. The transport
   lock is shared in the same way within one adapter worker, which is where it lives; it is
   not a process-wide or durable lock.
   Both halves of the bound - the hourly count and the minimum gap between two sends - are
   decided inside each claim's own transaction by ONE predicate, `delivery.reserve_send`,
   which the parent-child claim and this channel's claim both call. A bound that only one of
   its writers re-checks inside its write is a bound the other does not obey: a delivery
   claimed straight after a report never re-read the gap, and the two woke one task inside it.
   The gap is read across hour windows, because `last_send_at` lives on the hour's row and
   reading one hour let two sends a second apart straddle the boundary. Each sender also reads
   the same predicate before the host, and that is a preflight: two callers pass it at the
   same moment, so on its own it paces nothing.
   A claim refused on the budget is deferred by the same gap the preflight would have applied,
   in both queues. It is never recorded as a failure and never held, because nothing about the
   message or the recipient is wrong: another send simply got there first.
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
written. Once the message commits, `report.record` refuses to change that report at all, in
place or as a new submission, because the packet would otherwise name one pull request while
its evidence reads another.

An omission's reading is checked against its obligation before anything is composed: the
schema is `reporting-observation/1`, the state is `unreported`, the relationship and the turn in
its selectors are the obligation's, and the obligation the reading raises is the one handed in,
field by field - the id omits the generation, so a reading from another generation of the same
turn passes every other check. Otherwise staging refuses as `contradictory_observation`.
`supervisor-stage --project` indexes the readings it is given by the obligation each raises,
and an obligation whose readings disagree about what it is or where it can be read is refused
by name under `refused` rather than staged with one of them.

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

### Who moves a message out of sending

A message leaves `sending` only through the claim that holds it: this message, this attempt
number and this lease owner, all three in the predicate of the write that moves it. `_settle`
records the transport's answer that way, and the transport-start write that finds the hierarchy
moved releases the message the same way. The one other way out is recovery, which takes the
row from a claim whose lease expired with no receipt, and what it does depends on a durable
fact. `transport_started_at` is stamped only by the claim holding the row, inside the write
that checks it still does, and committed before the transport is called. The stamp is taken as the last thing that write does, after the lock is granted and the
hierarchy asked, because a time read before waiting for the lock dated the start early. An
attempt with no stamp therefore sent nothing and never will - its owner's own transport-start write now finds
the row is not its own - so recovery records it as sending nothing and queues the report again.
An attempt with a stamp may have sent, so recovery moves the message to `held_uncertain`,
because nothing observed what that send did.

Every exit between a claim and its transport gives back what the claim took. The claim spends
one of the recipient's sends from the budget it shares with parent-child deliveries, and
remembers on the attempt what the recipient's row held before; the transport-start refusal and
the recovery of an attempt that never started both give that send back in the same write. Where
nobody has reserved since, the row goes back to exactly what it was; where somebody has, their
send time stays, because it is theirs, and only this attempt's count comes off. A claim that
reaches its transport keeps what it spent, whatever the transport answers.

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
`unknown` is reachable: the origin is named only when the delivered attempt carries a turn id,
and `inbox_only` carries none.

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
delivery token in the recipient's own transcript. Only then does the readback settle it
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
in - the store directory and the socket included - leaving two placeholders, `YOUR_TURN_ID`
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

Nothing here wakes anybody on a timer. There is no daemon pass behind these commands: a report
goes out inside the parent's own turn. An uncertain send is never retried automatically either,
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
queues that report again with the reserved send given back.
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

A report still owed for an archived assignment has nobody to go to. `resolve()` finds the
project by walking up the edge the assignment holds on its issue, and archiving the assignment
releases that edge, so staging and sending refuse as `unregistered_scope` from then on. The
obligation keeps standing and `supervisor-standing` keeps listing it; through this channel it
goes out only if it is staged and sent before the assignment is archived. A superseded
assignment is not affected, because its successor holds the edge.

## Who calls it today

Nothing in the relay calls this channel on its own. The four commands are the whole entry
surface: no daemon pass, Stop hook, MCP tool or service stages or sends a report. In live
operation the caller is the project parent, from inside its own turn - `supervisor-stage
--project` for what the project owes, then `supervisor-send` for each staged message - and the
supervisor answers with `supervisor-read` from a turn of its own. crw-run's relay reference
states that as an instruction to the parent and the relay does not enforce it, so a parent that
does not run the commands reports nothing through this channel. The reporting duty is carried
by that instruction, which is weaker than an automatic one, and making it automatic is not part
of this change. What IS independent of the parent is the obligation: it is derived from the
event row the child's final receipt wrote, so a parent that never stages or sends leaves the
report owed and unreported in `supervisor-standing --project`, across a restart, until somebody
reports it or the Linear record confirms it. Nothing notices that on its own; it is visible to
whoever asks.

## Scope of these claims

Source-implemented and covered by `tests/test_supervisor_channel.py`, which stages, sends and
reads back against this package's own fake host. That is evidence about this source and about
that host. It is not an installed runtime, an activated service, or any report reaching any
supervisor on any machine. Whether a real parent stages and sends a report, a real supervisor
thread receives it, and a real supervisor reads it back is the live round trip, which is run
after installation and is not shown by this source or its CI.
