# What a message has to carry, once the envelope has said what it is

`relay-envelope/1` answers which relation a message belongs to, which direction it travels,
what the recipient owes because it arrived, and how far it got. It stops there on purpose.
It does not know that an assignment without a criteria digest is an instruction nobody can
be judged against, that a correction without the generation it opens produces a receipt the
relay then refuses, or that a resume omitting the workflow has dropped the one field no
transport carries.

`packets.py` is the other half. `relay-packet/1`, for the parent-child relation and for what a
parent owes the level above; a supervisor's instruction downward is still carried by the
envelope alone. `receiver.py` is what the receiver reads for itself before it acts on one.

## Fourteen occasions, not two

The envelope shipped with one purpose in each direction, so five different child reports and
six different parent messages rendered as two words. A blocked turn and a candidate offered
for review both said completion; the message that STARTS the work had no name at all.

| Direction | Purpose | Kind | Required beyond the envelope |
|---|---|---|---|
| parent to child | assignment | request | issue, criteriaDigest, policy, callback, body |
| parent to child | revision_request | request | issue, generation, criteriaDigest, callback, artifact, body, evidence |
| parent to child | resume | request | issue, policy, callback, artifact |
| parent to child | receipt_confirmation | notification | issue, correlationId |
| parent to child | acceptance | notification | issue, criteriaDigest, artifact |
| parent to child | integration_result | notification | issue, artifact |
| child to parent | completion | request | issue, generation, criteriaDigest, artifact, evidence |
| child to parent | review_ready | request | issue, generation, criteriaDigest, artifact, evidence |
| child to parent | blocked | request | issue, evidence |
| child to parent | decision_request | decision | issue, decision, evidence |
| child to parent | progress | notification | issue |
| parent to supervisor | completion | notification | issue, generation, evidence |
| parent to supervisor | blocked | notification | issue, evidence |
| parent to supervisor | decision_request | decision | issue, decision, evidence |

The restraint is load-bearing in two rows. An **assignment** does not require a generation,
because a newly created child's registration needs a task id creation has not returned yet
and the assignment is what gets sent before it exists; demanding one would refuse every
legitimate first dispatch. A **progress** note requires nothing but the issue, because
demanding a head from a child with nothing to show yet is how a plausible value gets made up.

A **resume** requires the policy for the opposite reason. Model, effort, sandbox and approval
travel as settings and a receipt reads them back. The workflow has no transport field
anywhere, so a message that does not say it has not deferred it - it has dropped it, and the
compacted child it reaches resumes under whatever it still happens to remember.

A **revision request** requires its body and its evidence, because a correction that says
nothing about what it corrects is a correction the child answers from memory. The body is read
against the correction form below rather than against DISPATCH-TASK-01, and the evidence is
where the reproduction or the review finding it rests on can be read.

The three upward rows are the ones [the supervisor channel](supervisor-channel.md) sends. A
completion upward is NOT required to carry an artifact, because a noop completion has none and
a research assignment may have only a locator; it carries the pull request when the work report
names one whole. `status_response` has no row at all, and the absence is deliberate: `body`
here is a dispatch instruction checked against DISPATCH-TASK-01, so requiring it of an answer
would refuse every real answer, and an occasion with no field it cannot do without would be a
row that admits anything. That occasion is carried by the envelope alone.

## The shapes the data takes

**The correction form.** A revision request's body carries five sections, each a line-anchored
heading with something under it: `VIOLATED CRITERION` (the original criterion the work does not
meet), `WHAT CHANGED` (what is different from what the child was working to), `FIX SCOPE` (the
bounded part to change), `PRESERVE` (the work and evidence that stay), and `REVERIFY AND RETURN`
(what to check again and when to hand back). The link to the request being corrected is the
envelope subject, and the reproduction or review basis is the evidence field. A body missing a
section is refused naming it, exactly as an assignment missing a DISPATCH-TASK-01 section is.

**The callback.** An object of `taskId`, `model` and `effort`: where to answer, and the pair the
task being answered is currently authorized to run. It is an object because the pair is what
goes stale. A parent whose model the user changed on 2026-09-23 runs another pair from then on,
and a packet still naming the old one must not pass; a string such as "parent task X" could not
be compared at all.

**The policy.** Model, effort, workflow and **mode** (`loop`, `non_loop` or `coordination`),
with sandbox and approval where stated. The mode is the machine-readable form of what the
workflow means for activation; the workflow text is never parsed to infer one. It is read only
to refuse a contradiction: a workflow naming CXC Loop with any mode but `loop` is refused.

**The first assignment.** Before creation there is no relationship to name, since its id is
derived from the child's task id. The one identity that exists before creation and that
registration binds to the relationship is the dispatch request id, so a first assignment names
that dispatch as its relation and states its recipient as an absence. A receiver compares it
with the dispatch that opened its current generation and reads the recipient from its own
registration. A first assignment arriving after a correction names a dispatch that is no
longer current and is refused as `wrong_relation`. Its relation revision is not compared, because
the packet was written before the link existed; the dispatch binding and the relationship's
liveness answer for currency instead.

## The artifact is one of two things and never a blank

A `pull_request` carries its repository beside its number, because the same number on two
projects is two different pull requests, and its head, because a report naming a pull request
and not a commit is inherited by the next push to it.

A `locator` carries a path and a digest. It exists so a research, design or verification
assignment has something true to put in the field. Before it, the only shape available was a
pull request, so a child with no repository change either invented one or left the field
empty. A locator is never compared against a head and is never asked for one.

## Three dispositions, and the middle one is the point

`reception(packet, record)` compares the packet against the record the receiver read for
**itself**. Nothing in a message is evidence of itself: these transports carry opaque text and
no authenticated caller, so a title, a self-description or a plausible task id establishes
nothing.

- **accepted** - every field the record could answer agreed with it.
- **refused** - a field contradicted it. The mismatch names the field, both values and why it
  matters: `wrong_sender`, `wrong_recipient`, `wrong_relation`, `superseded_relation`,
  `wrong_issue`, `stale_generation`, `stale_criteria_digest`, `stale_head`, `wrong_callback`,
  `stale_callback`, `stale_policy`, `wrong_mode`, `refused_settings`, `message_collision`.
- **unavailable** - the record could not answer. This is neither of the others. A receiver
  that could not check the generation has not checked it, and folding that into acceptance is
  how an unverifiable instruction becomes an applied one.

A record key that is missing, or holds nothing, is unread and produces an `unreadable` gap. One
key has a definite nothing: `relationRevision` holding null says the relationship is unscoped,
so it has no link and no revision, and a packet stating a revision for it is refused while one
stating none agrees. The relation revision is always compared except on a first assignment,
and the relationship must be live: `relationStatus` other than `active` or `paused` is refused as
`superseded_relation`. A packet on a paused relationship can be accepted, since it is current;
acting on it still waits for `relationship-resume`.

The execution mode is compared with the receiver's own reading of it. An assignment defines the
mode only where the receiver holds no reading yet; every other packet carrying a policy or an
activation reading, and any assignment where a reading exists, is compared and refused as
`wrong_mode` on a difference, or left a gap where there is no reading. So no packet can
redefine a mode the receiver already holds.

`refused_settings` is worth naming separately. A model and effort pair the record holds as
refused for this role is a settings answer, not a provider failure, and it is not worked
around by creating a second child.

## The receiver's own reading

`packet-check --receiver <task id>` builds the record from the relay store instead of taking
it from a file. It opens the store read-only and never creates or migrates one; a store it
cannot open or read gives a record holding only the receiver's own id, so every other field is
a gap. Each field in the answer's `provenance` names what answered it.

| Record key | Answered by |
|---|---|
| the receiver's own role id | the receiver (`--receiver`) |
| relationId, the other task, issue, relationStatus, generation | `relationships` |
| dispatchRequestId | `generations`, the current generation |
| relationRevision | `scope_links` through the relationship's project link; null when unscoped |
| criteriaDigest | `canonical_criteria`, a managed set with one digest |
| policy | `authorized_settings` of the child |
| callback | the relationship's parent and its `authorized_settings` |
| refusedPolicies | the role policy (`rolepolicy.check_record`) for the child's bound role |
| mode | the receiver's reception ledger (below) |
| repository, prNumber, headSha, artifactPath, artifactDigest | only `--observation` |

The relationship is chosen among the receiver's own rows: its one live relationship where it
has exactly one, otherwise the row the packet names if it is one of the receiver's, otherwise
none and the relationship keys are gaps. The packet's relation id is a selector among rows the
receiver already holds and is never copied into the record.

The artifact head is a forge reading the store does not hold. It comes only from
`--observation <file>`, a JSON object with its own `source` (and `observedAt` where known) and
any of `repository`, `prNumber`, `headSha`, `artifactPath`, `artifactDigest`. Without one, a
pull request's head is a gap and the answer is unavailable. A locator is compared with the
observed path and digest and is never asked for a head.

**The reception ledger** (`--ledger <file>`) is the receiver's own file, written by this command
and read by nothing else. It names its receiver and a version, and a ledger naming another
receiver is refused rather than read. It keeps each answered message id beside the content
digest and disposition it got, and, once an assignment is accepted, the mode and workflow that
assignment gave. Writes happen under a lock on a sidecar file and replace the ledger
atomically. A message arriving again with the same content is a `replay`: the answer is today's
reading, with `previousDisposition` beside it, and `act` is true only when today's answer is
accepted and the earlier one was not, which is the one case where the instruction has not been
applied yet. The same id asking for something else is a `message_collision` and is refused.
A stored disposition is upgraded to accepted and never downgraded. Without a ledger the answer
says `repeat: unchecked` and `act` is false whatever the disposition, because a repeat cannot be
told from a first arrival.

**Why this step is the boundary.** No relay code path carries a parent-child packet. The relay
renders its own messages from stored rows and checks their currency when they are acknowledged
and claimed; a `relay-packet/1` reaches its receiver as text inside a prompt - the creation
prompt, a steer, a send. So the reception boundary is the receiver's documented step, and that
step runs this command. The same is its limit, named plainly: a receiver that does not run the
step has not checked the packet, nothing in the relay calls it on the receiver's behalf, and
the residual risk is a packet acted on unchecked, visible only through the receiver's own
report. It is a receive-step check, not an enforcement layer; there is no final enforcement
layer.

`packet-check --record <file>` remains the offline form. The answer says `recordSource:
supplied`, and that qualifier is the honest part: two files agreeing proves only that they
agree.

## A message arriving twice

`repeat` answers **first**, **replay** or **collision**, keyed on the message id together
with a digest of what the packet actually asks for. Identity alone is not enough: the id is
derived from the direction, relation, purpose and subject, so a sender can spend one in
advance. A collision - one id asking for something else - is raised rather than given the
earlier answer. How a replay is answered is the ledger's rule above: the current reading
stands, and the earlier disposition is reported beside it.

## Seven states, and two of them are never held by the store

The envelope's five stages are about one message getting somewhere. These seven are about the
assignment, and they run past the message into what the parent then did and what Linear ended
up holding.

| State | Answered by |
|---|---|
| read | nothing |
| transport_accepted | `attempts` |
| relay_ack | `acks` |
| criteria_verdict | `verdicts` |
| parent_acceptance | `verdicts` |
| merge_landing | `merge_turns` |
| linear_done | `linear_issue_status`, a readback of the issue's own status |

`read` has no mechanism at all. No row in this store says a recipient read anything, and a
model writing "received, understood" is prose. `linear_done` has a mechanism, but not in this
store: the outbox confirms that a coordination summary block was written and read back, which
says nothing about the issue's status. A supplied ladder answering `linear_done` from
`sync_outbox` is refused for that reason, and the store-backed answer reports the outbox
separately as `coordinationSync`.

In store mode the ladder is read for the packet's subject event: `transport_accepted` holds only
for a dispatched attempt (held-uncertain or unsettled attempts leave it unmeasured, and
inbox-only is an approval failure with no send); `relay_ack` holds for an accepted and verified
acknowledgement and is conditional for one not yet verified; `criteria_verdict` holds for any
verdict and `parent_acceptance` for a verified one; `merge_landing` holds only for a landed turn
on the observed repository, pull request and head, and is unmeasured without that observation;
`linear_done` is unmeasured. `promotions` lists states standing above one that is not held.
`unbackedClaims` lists states the packet's own ladder claims that the store answers no or
conditional, and `unmeasurableClaims` those it cannot answer at all. Neither promotes anything.

## Activation is three facts, not one

The prompt naming a workflow, the child having actually armed it, and a host goal being
active are produced by three different parties and none implies another.
`activation_class` reads the triple into the classes the crw-run reference already names,
L0 through L6. Two of those are not defects: L5 is working and L6 is an unknown.

The mode decides which facts can answer, and the reading carries the mode it was read under.
A coordination parent holds a native goal and no implementation FSM **by design**, and an
authorised non-Loop assignment arms neither, so both answer `not_applicable` rather than
absent. A loop reading answering `not_applicable` is refused, a reading whose mode differs from
the packet's policy is refused, and the mode itself is compared with the receiver's reading as
above. A parent that holds no reading of a child's mode gets such a report back unavailable;
it reads the child's activation from the child's own goalplan under dispatch verification, not
from the packet.

## Scope of these claims

Source-implemented and covered by `tests/test_child_packets.py`,
`tests/test_reception_findings.py` and `tests/test_store_reception.py`, and for the upward rows
by `tests/test_supervisor_channel.py`. The store-backed checks run against isolated stores this
package's tests create. That is evidence about this source. It is not an installed runtime, an
activated service, a receiver running the step, or any packet reaching any task on any host.
The round trip in `test_child_packets.py` is a read-only replay over records held in memory;
the round trip in the supervisor channel's tests opens a store and a fake host, and what it
establishes is bounded in [the channel document](supervisor-channel.md).
