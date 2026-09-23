# What a message has to carry, once the envelope has said what it is

`relay-envelope/1` answers which relation a message belongs to, which direction it travels,
what the recipient owes because it arrived, and how far it got. It stops there on purpose.
It does not know that an assignment without a criteria digest is an instruction nobody can
be judged against, that a correction without the generation it opens produces a receipt the
relay then refuses, or that a resume omitting the workflow has dropped the one field no
transport carries.

`packets.py` is the other half. `relay-packet/1`, for the parent-child relation and for what a
parent owes the level above; a supervisor's instruction downward is still carried by the
envelope alone.

## Fourteen occasions, not two

The envelope shipped with one purpose in each direction, so five different child reports and
six different parent messages rendered as two words. A blocked turn and a candidate offered
for review both said completion; the message that STARTS the work had no name at all.

| Direction | Purpose | Kind | Required beyond the envelope |
|---|---|---|---|
| parent to child | assignment | request | issue, criteriaDigest, policy, callback, body |
| parent to child | revision_request | request | issue, generation, criteriaDigest, callback, artifact |
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

The three upward rows are the ones [the supervisor channel](supervisor-channel.md) sends. A
completion upward is NOT required to carry an artifact, because a noop completion has none and
a research assignment may have only a locator; it carries the pull request when the work report
names one whole. `status_response` has no row at all, and the absence is deliberate: `body`
here is a dispatch instruction checked against DISPATCH-TASK-01, so requiring it of an answer
would refuse every real answer, and an occasion with no field it cannot do without would be a
row that admits anything. That occasion is carried by the envelope alone.

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
**itself** - its relationship row, its current generation, its registered criteria, the head
its own forge reading reports. Nothing in a message is evidence of itself: these transports
carry opaque text and no authenticated caller, so a title, a self-description or a plausible
task id establishes nothing.

- **accepted** - every field the record could answer agreed with it.
- **refused** - a field contradicted it. The mismatch names the field, both values and why it
  matters: `wrong_sender`, `wrong_recipient`, `wrong_relation`, `superseded_relation`,
  `wrong_issue`, `stale_generation`, `stale_criteria_digest`, `stale_head`,
  `refused_settings`.
- **unavailable** - the record could not answer. This is neither of the others. A receiver
  that could not check the generation has not checked it, and folding that into acceptance is
  how an unverifiable instruction becomes an applied one.

`refused_settings` is worth naming separately. A model and effort pair the record holds as
refused for this role is a settings answer, not a provider failure, and it is not worked
around by creating a second child.

## A message arriving twice

`repeat` answers **first**, **replay** or **collision**, keyed on the message id together
with a digest of what the packet actually asks for. Identity alone is not enough: the id is
derived from the direction, relation, purpose and subject, so a sender can spend one in
advance. A replay is given back the disposition it already got, which is what makes an
uncertain send safe to settle by asking rather than by sending again. A collision - one id
asking for something else - is raised rather than given the earlier answer.

## Seven states, and the first one can never be held

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
| linear_done | `sync_outbox` |

`read` has no mechanism at all. No row in this store says a recipient read anything, and a
model writing "received, understood" is prose. Saying so in the table is what stops it being
the state everybody assumes. `unsupported_promotions` returns the states standing above one
that is not held, which is what a false promotion looks like from outside.

## Activation is three facts, not one

The prompt naming a workflow, the child having actually armed it, and a host goal being
active are produced by three different parties and none implies another.
`activation_class` reads the triple into the classes the crw-run reference already names,
L0 through L6. Two of those are not defects: L5 is working and L6 is an unknown.

The mode decides which facts can answer. A coordination parent holds a native goal and no
implementation FSM **by design**, and an authorised non-Loop assignment arms neither, so both
answer `not_applicable` rather than absent. Reading either as an unarmed child is reading a
normal state as a finding.

## Scope of these claims

Source-implemented and covered by `tests/test_child_packets.py`, and for the upward rows by
`tests/test_supervisor_channel.py`. That is evidence about this source. It is not an installed
runtime, an activated service, or any packet reaching any task on any host. The round trip in
the first file is a read-only replay over records held in memory: no store is opened, no
delivery is claimed and no acknowledgement row is written. The round trip in the second opens a
store and a fake host, and what it establishes is bounded in
[the channel document](supervisor-channel.md).
