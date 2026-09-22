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
   can claim the other's row, and a supervisor message has no receipt, no acknowledgement and
   no verdict.
2. A report is staged only from a fact the store already holds as final - the obligation
   derivation requires `stage = 'final'` and no suppression - so an upward report can never
   precede the record it reports.
3. The per-recipient hourly bound and the per-recipient transport lock are SHARED, on purpose.
   They bound how often one task may be woken, and two queues feeding one task must not each
   get their own budget. The consequence, said rather than implied: where one task is both a
   parent and a supervisor, a report can wait behind parent-child traffic to that same task.
4. Within one recipient the oldest staged message is claimed first, so two facts reach the
   level above in the order they arose.

### Durable staging

Staging freezes the packet before any transport call, so a crash between deciding and sending
loses the send and not the decision. The message id is the envelope's, derived from the
direction, the relation, the purpose and the subject, so one fact staged twice converges on one
row. In the same transaction it records the report in the existing `supervisor_report` journal,
which is what makes the next reading of that fact converge instead of waking the level above
again - and what keeps a report that exists from being invisible to the thing that decides
whether to produce one.

### The readback, and exactly what it establishes

A delivered message is not a read one. The recipient answers from inside its own turn with
`sha256(messageId|<its own turn id>)`. The delivered bytes carry the message id, because a
recipient has to be able to quote it, and they cannot carry the turn id, because that turn does
not exist until the message arrives. So an echo of every delivered field still cannot produce
the proof - the property `ack.acknowledge` already rests on.

A verified readback says three things. The bytes this send froze are in the recipient's own
transcript, found by scanning it for the request id - evidence independent of our own receipt. A
turn the host lists on the recipient's thread answered. And that turn did not begin before the
send.

It does not say who wrote the answer. This transport carries opaque text and no authenticated
caller, and the turn a send opens is a turn the sender already knows the id of, so a readback
from that turn rests on nothing the sender could not have produced alone. That is also the
ORDINARY case, because the message is what wakes the supervisor - so which turn answered is
recorded as `relay_opened` or `recipient_opened` rather than averaged into one word, and a
reader can see how much was established instead of being told a number.

A readback that does not verify is still recorded. Hiding it would lose the fact that somebody
answered; what it does not do is move the message to read.

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

# One attempt, through the same host rules a delivery obeys.
codex-session-relay supervisor-send --message <id>

# The recipient confirming it read one, from inside its own turn.
codex-session-relay supervisor-read --message <id> --turn <your turn id> --proof <proof>

# What was staged, every attempt, and what came back.
codex-session-relay supervisor-show --message <id>
```

`supervisor-stage` and `supervisor-show` reach no host. `supervisor-send` resumes the
supervisor's thread and `supervisor-read` checks the host's turn list and transcript, so both
are host-required.

## What this does not do

Nothing here wakes anybody on a timer. There is no daemon pass behind these commands: a report
goes out inside the parent's own turn. An uncertain send is never retried automatically either,
because there is no reconciler for this queue and a second send that lands is a second wake for
one fact; it stays `held_uncertain`, which is not claimable, and says so.

## Scope of these claims

Source-implemented and covered by `tests/test_supervisor_channel.py`, which stages, sends and
reads back against this package's own fake host. That is evidence about this source and about
that host. It is not an installed runtime, an activated service, or any report reaching any
supervisor on any machine.
