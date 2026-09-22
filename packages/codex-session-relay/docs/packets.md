# What a message has to carry, once the envelope has said what it is

`relay-envelope/1` answers which relation a message belongs to, which direction it travels,
what the recipient owes because it arrived, and how far it got. It stops there on purpose.
It does not know that an assignment without a criteria digest is an instruction nobody can
be judged against, that a correction without the generation it opens produces a receipt the
relay then refuses, or that a resume omitting the workflow has dropped the one field no
transport carries.

`packets.py` is the other half, for the parent and child relation. `relay-packet/1`.

## Eleven occasions, not two

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

The restraint is load-bearing in two rows. An **assignment** does not require a generation,
because a newly created child's registration needs a task id creation has not returned yet
and the assignment is what gets sent before it exists; demanding one would refuse every
legitimate first dispatch. A **progress** note requires nothing but the issue, because
demanding a head from a child with nothing to show yet is how a plausible value gets made up.

A **resume** requires the policy for the opposite reason. Model, effort, sandbox and approval
travel as settings and a receipt reads them back. The workflow has no transport field
anywhere, so a message that does not say it has not deferred it - it has dropped it, and the
compacted child it reaches resumes under whatever it still happens to remember.

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

## What check refuses, and why a constructor was not enough

Two entry points reach this contract and they are not the same. `compose` builds a packet
here, where every constructor has already run. `packet-check` reads one back from JSON, where
none of them has. Every rule that lived only in a constructor was a rule that second path did
not have, so `check` is a complete validator rather than a finishing touch, and `reception`
calls it before it compares anything.

**Shape before meaning.** A packet is not always a mapping. A JSON array reaching `one.get()`
raised `AttributeError`, which is a host failure rather than a producer being told what it
sent, and a validator that crashes on malformed input is not validating it. The packet, the
region, the policy and the activation reading are each checked for shape first.

**Derivation before comparison.** Three of the region's fields are computed from the others,
so `_rederive` recomputes them rather than reading them: the `kind` from the direction and
purpose, both endpoint roles from the direction, and the `messageId` from the direction,
relation, purpose and subject. Comparing copies would catch nothing, because the packet
carries no duplicate of anything - the same reason `envelope.contradiction` checks a directive
pointer by re-derivation. The `messageId` is the one that matters: it keys the replay and
collision reading, so a caller able to write its own could hand a second correction the
disposition given to the first, or spend an id in advance that a later real request collides
with.

**Both versions are matched.** The packet's own version says how to read the typed data and
the region's says how to read the identification. Checking one and not the other let a
`relay-packet/1` carry an envelope nobody here has mapped and be compared field by field
anyway.

**Typed fields have shapes, not just values.** `FIELD_TYPES` makes the issue, criteria digest,
callback and body text and the generation a number, and `ARTIFACT_REQUIRED` makes a pull
request name its repository, number and head and a locator its path and digest. Non-empty was
the whole test before, and an object-valued generation compared equal to an equally malformed
record value - two wrong answers agreeing, and the reading coming back accepted. The policy's
own fields are text for the same reason: the refused-pair reading compares by string form,
where two values that are not settings can agree with each other and neither is a setting.

**Identity before currency.** `_artifact_agreement` compares the repository and number of a
pull request, and the path of a locator, before it compares the head or the digest. Comparing
only currency accepted a packet naming another repository or another pull request that
happened to sit on the same commit, which is the second writer this whole reading exists to
keep out. Both go through `_compare`, so a record that does not name them is a gap rather
than a pass.

**A mode may not excuse itself.** `activation_class` refuses a `loop` reading that answers
`not_applicable` for its own activation. That answer belongs to a mode which arms nothing,
and accepting it from a loop read an unarmed one as working - the L3 misreading in the
direction that hides a defect rather than inventing one.

One restraint runs the other way, and it is deliberate. `report.child_purpose` derives the
child direction's purpose from the receipt's outcome and from nothing else, because the
purpose is an input to the message id and `relay-envelope/1` promises that id stays put across
retries, restarts and second readings. An earlier version also read whether a merge-readiness
handoff had been recorded, which is not a property of the event: a resubmission can add one,
so the same event derived a second id and the recipient owed two obligations for one fact.
The candidate-versus-result distinction lives here instead, where `review_ready` is its own
purpose, and in the delivered bytes, where a candidate renders the merge-readiness block and a
plain result renders none of it.

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

Source-implemented and covered by `tests/test_child_packets.py`. That is evidence about this
source. It is not an installed runtime, an activated service, or any packet reaching any task
on any host, and the round trip in that file is a read-only replay over records held in
memory: no store is opened, no delivery is claimed and no acknowledgement row is written.
