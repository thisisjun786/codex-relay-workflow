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
`reporting-show --turn` for an omission, which has none.

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
   decided inside the claim's own transaction, where the counter moves. The reading taken
   before the host reads is a preflight that two callers pass at the same moment, neither
   having seen the other's send, so on its own it paces nothing.
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

### The readback, and exactly what it establishes

A delivered message is not a read one. The message asks the recipient to answer from inside its
own turn with `sha256(messageId|<its own turn id>)`. That is an INSTRUCTION and not an enforced
property: the command receives a deterministic hash, nothing authenticates the caller, and
nothing establishes that the named turn produced it. What the proof does rule out is an echo:
the delivered bytes carry the message id, because a recipient has to be able to quote it, and
they cannot carry the turn id, because that turn does not exist until the message arrives. That
is the whole of what it rules out, and it is the property `ack.acknowledge` already rests on.

A verified readback says exactly four things, in these words:

- a bounded scan of the recipient's items - at most 200 of them - found THIS attempt's request
  id. Not the frozen message, which is never compared; not necessarily in the turn that
  answered, which is never correlated. What it is good for is that it does not come from our
  own send receipt.
- the host can read the named turn on the recipient's thread and it carries a start time. The
  read is thread-scoped, so it establishes membership as well as existence; asking a bounded
  LISTING first would have answered `turn_not_found` for a real turn a busy recipient had
  pushed off the end of it.
- the named turn is not CERTAINLY earlier than the send - a start that precedes it by more
  than the host's timestamp precision is refused. This applies to every candidate, including
  the turn the attempt reports having opened, because a send can STEER an existing turn rather
  than open one: that turn predates the message, and exempting it let it verify a readback for
  a message it could not have been opened by.
- where the named turn is the one the SEND opened, the delivered bytes are in that same turn.
  Those two cannot disagree about one message: what the token is IN is where the message
  landed, so a claim to have read it where it landed has to name that turn. The tie holds for
  that case only, and deliberately - a turn the recipient opened AFTERWARDS is not expected to
  carry the token, and is the stronger reading anyway, because the sender never knew its id.

It does not say who wrote the answer, that the turn answered anything, or that the supervisor
acted. The turn a send opens is one the sender already knows the id of, so a readback from that
turn rests on nothing the sender could not have produced alone - and that is the ORDINARY case,
because the message is what wakes the supervisor. So which turn answered is recorded as
`relay_opened`, `recipient_opened` or `unknown` rather than averaged into one word, and a reader
can see how much was established instead of being told a number. `unknown` is reachable: the
origin is named only when the delivered attempt carries a turn id, and `inbox_only` carries
none.

There are six verification answers: `host_read`, `transcript_unconfirmed` for a real turn whose
transcript scan did not confirm the message, `transcript_turn_mismatch` where the readback
names the turn the send opened and the token is in another, `turn_not_found`,
`turn_predates_send`, and `unverified_turn` where no host could be read at all.

A readback that does not verify is written down, on the path that inserts or updates a row -
the one taken when no settled `host_read` row exists yet. Hiding it would lose the fact that
somebody answered; what it does not do is move the message to read. Once a settled row exists,
a later readback is answered from it on a fast path BEFORE the proof is checked, so it is
neither verified nor recorded, and a settled verdict cannot be replaced - the guard for that is
the re-read inside the write transaction, not the check before it.

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

# One attempt, through the same host rules a delivery obeys. --socket is global, so it comes
# before the subcommand; after it argparse refuses.
codex-session-relay --socket <path> supervisor-send --message <id>

# The recipient answering. The message ASKS for a turn id of its own; nothing enforces that.
# --as names the asserting task and is required; it is checked against the message's recipient.
codex-session-relay --socket <path> supervisor-read --message <id> --turn <turn> \
  --proof <p> --as <your task id>

# What was staged, every attempt, and what came back.
codex-session-relay supervisor-show --message <id>
```

`supervisor-stage` and `supervisor-show` reach no host. The other two are host-required and
refuse without `--socket`, exiting 4 with usage JSON and writing nothing to the channel - the
list of host-required commands is what `doctor` reports and enforces nothing, so the refusal
is its own check. Supplying the option proves an argument was supplied, not that a host is
reachable.

The line the message itself renders is this one with everything the relay already knows filled
in, leaving three placeholders: the socket path, the recipient's own turn id, and the proof
over it. Each is a single word, so the rendered line splits into an argv as it stands -
`tests/test_supervisor_channel.py` hands it to the real parser rather than checking it looks
like a command.

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
an assignment and a session too, so an evidence line built from the obligation alone looked
like a command and could not be run. With the reading the pointer is rendered whole.

## What a readback is not

It carries no supervisor authority, and the bound on that is worth stating exactly rather than
in general. Reaching `host_read` takes four things no caller can produce from this store
alone: a turn the host will read on the RECIPIENT's own thread, a start time for it that is
not certainly before the send, this attempt's request id in the recipient's own transcript,
and - where the readback names the turn the send opened - the token in that same turn. Every
one of those is a question put to the host about the recipient's thread. A caller holding
nothing but this database gets `unverified_turn` or `transcript_unconfirmed`, which is
recorded and leaves the message where it was.

One residual is left and this channel cannot close it. A local operator who can DRIVE the
recipient's thread - open a turn on it and get the request id into it - satisfies all four,
and so can produce a verified readback the supervisor never wrote. That operator already holds
the authority to write the row directly: the store is a file, not a service with callers to
authenticate, so nothing on this side can tell the two apart. Closing it takes an
authenticated caller, which is a relay-wide change and not this channel's to make. What the
channel does inside its own reach is record who ASSERTED the readback and refuse an assertion
that does not name the message's recipient - a declaration, written down as one, and calling
it anything stronger would be the kind of claim the rest of this document exists to avoid.

Read `received` on the reach ladder as "a readback was recorded, the host agreed its turn is
real, and the recipient's own transcript holds the message". It is not "the supervisor acted",
and the obligation is not discharged by it.

## What this does not do

Nothing here wakes anybody on a timer. There is no daemon pass behind these commands: a report
goes out inside the parent's own turn. An uncertain send is never retried automatically either,
because there is no reconciler for this queue and a second send that lands is a second wake for
one fact; it stays `held_uncertain`, which is not claimable, and says so.

A send interrupted between its claim and its transport receipt is the same answer reached a
different way. The claim commits first, so a process that stops existing in between leaves the
row in `sending` with a lease nobody will settle. An expired lease moves it to
`held_uncertain`, which authorises no resend, because nothing observed what that send did.
`stranded()` lists such rows and is a READ: there is no sweep behind it, and a row is moved
when `attempt()` is called for that message again.

## Scope of these claims

Source-implemented and covered by `tests/test_supervisor_channel.py`, which stages, sends and
reads back against this package's own fake host. That is evidence about this source and about
that host. It is not an installed runtime, an activated service, or any report reaching any
supervisor on any machine.
