# Re-evaluating the candidate set

What [Keep a project run moving](../SKILL.md#keep-a-project-run-moving) and
[Establish the project baseline](../SKILL.md#establish-the-project-baseline) need and do not
already own: the read behind each forcing event, the closed set a hold is recorded in, and what a
pass records per candidate. It adds no scheduler, no store and no cursor.

Vocabulary stays with its owner and is not restated here. What each relay reader answers, and the
limits on it, are in [codex-session-relay](relay.md); capacity bounds and `unmeasured` are in
[Start policy](start-policy.md#what-bounds-the-number-actually-dispatched); the turn-ending
prohibition is [OPS-8.1](operations.md#ops-81-parent-continuation-and-waiting); where the record
lives is [Task packet](task-packet.md). Read those for meaning; read this for what the pass does
with it.

A pass is a full read. No landed reader is incremental and nothing records where the last pass
stopped, so a resume reads the whole set rather than a delta.

## Which read observes each forcing event

The six events are the ones [Establish the project baseline](../SKILL.md#establish-the-project-baseline)
names. Capacity, edit-region overlap and new attachments are **inputs read inside every pass**,
not triggers of their own.

| Forcing event | Read |
| --- | --- |
| Initial dispatch | the baseline itself |
| A child finished and is reviewable | `assignment-show --relationship` ([verify the current revision](relay.md#verify-the-current-revision)) |
| A child stopped for a person | `dispositions-show` ([which children stopped](relay.md#which-children-stopped-and-whether-anyone-was-told)) |
| A child asked for a decision | the same read; the work report on that event is what separates the three statuses |
| An integration landed | `merge-turn-show`, beside the assignment's merged mark |
| This parent resumed or recovered | the coordination record, then the reads above |

Inputs read within a pass: `capacity-show`, `region-show`, `linkage-outstanding`.

`merge-turn-show`, `region-show`, `capacity-show` and `linkage-outstanding` are named here and
documented by no skill file yet. Read each one's own output and do not infer a usage contract this
repository has not written; documenting them is a follow-up. Nothing in a pass calls a mutating
command to observe something.

## What the pass may not conclude

Only the mappings this pass owns. The reader's own semantics are in
[relay.md](relay.md#which-children-stopped-and-whether-anyone-was-told).

| Reading | What the pass does |
| --- | --- |
| The read was refused | `defer:disposition_unreadable`, carrying the reported detail. A readable store with an empty list is a different answer and is not a refusal |
| A child is absent from a project-scoped read | Not evidence it is fine. Read it by relationship before concluding anything about it |
| No settled disposition for a child | Not evidence nothing is wrong, and not a reviewable head. Which revision a generation stands on comes from `assignment-show` |
| A disposition the store holds as contested | `defer:disposition_contested`. The pass names no winner and records every candidate |
| Anything the store records as unmeasured | Nobody established the fact. It is never rewritten as the fact being false |
| A stopped child was never enumerated because it never emitted | The reader is not an observation path. OPS-8.1 still requires an emit path or a bounded observation path for the disposition itself |

## Decisions and what clears them

Closed set. A hold names the condition a later pass re-reads, and the row it rests on.

| Decision | Clears when | Evidence it rests on |
| --- | --- | --- |
| `dispatch` | — | the reads that let it through |
| `defer:blocked_prerequisite` | the named symbol, schema or region arrives | the landed revision or merged mark of its carrier |
| `defer:no_capacity` | a slot is released, or the bound that decided it changes | the bound recorded under [Start policy](start-policy.md#what-bounds-the-number-actually-dispatched) |
| `defer:capacity_unmeasured` | an observation is actually taken | the recorded `unmeasured` and what prevented measuring |
| `defer:edit_overlap` | the peer agreement settles, or the region is released | the agreement and its follow-up |
| `defer:merge_window` | the window's holder releases it, or this candidate is promoted | the merge turn; integration into a shared target stays serial |
| `defer:ownership_unverified` | a store this process measured answers the question | an unreadable store, a state directory this process cannot use, or `holds` null |
| `defer:disposition_unreadable` | the store becomes readable | the refused read and its detail |
| `defer:disposition_contested` | a fresh execution generation | every contested candidate, with no winner chosen |
| `defer:authority_pending` | the authority case that produced it is answered | the [Start policy](start-policy.md#cases-this-policy-is-accepted-against) case recorded at the start adjudication |
| `defer:criteria_contradiction` | the owner of the criteria settles it | both readings, preserved; this is the one hold that goes upward |
| `skip:already_owned` | not a hold | a responsible child already exists, and it is reused rather than replaced |
| `skip:out_of_scope` | an authorized scope change admits it | the approved set this run was designated against |

There is no review-round escalation value, and that is deliberate: a round count has nowhere to
land, because the parent rules on the criteria and the correction scope and returns the work to
the same child ([Return corrections](../SKILL.md#return-corrections-to-the-existing-task)). New
scope and new authority are the start-policy cases above; a real contradiction in the criteria is
the one hold that travels upward.

## Proving ownership

One question, one read: does this issue already have a responsible child?

`doctor --issue`
([determine whether this store holds the assignment](relay.md#determine-whether-this-store-holds-the-assignment))
answers it and constructs nothing, so it is safe to ask before anything exists. `holds` true ends
the evaluation as `skip:already_owned`, and the existing child is reused rather than replaced.
`holds` false is dispatchable on this axis. `holds` null is unproved rather than free, and so is
a state directory this process cannot use: both are `defer:ownership_unverified` with the reason.

The pass asks nothing further here. Reading the owning project parent needs an assignment to
already exist, which is the case `holds` true has just ended, so a second lookup would add a
store-constructing call that could only repeat what the first read settled.

## What a pass records per candidate

| Field | What its absence means |
| --- | --- |
| issue and project it was evaluated in | outside this parent's scope; it is not evaluated here |
| the live assignment, where there is one | never dispatched under this project |
| existing owner, and how ownership was proved | unproved is not unowned |
| assignment state and what it waits on | unregistered, which is not finished |
| the event the trigger fired on, with its producer | no final event in this generation. A daemon-observed outcome and a child-asserted one are different facts |
| prerequisite, named as a symbol, schema or region, with the read that will prove it arrived | no prerequisite claimed. A shared repository or package is not one, because nothing re-reads it |
| capacity, with which bound decided and how that use is known | no limit declared, which is not room to run |
| edit region and overlap | the region was never proposed; overlap is unknown, not none |
| merge window | no turn requested; it does not apply before the pull request stage |
| the decision, from the closed set | the record is incomplete |
| the retained row or reading the decision rests on | the decision is unsupported, and a later reader treats it as unverified |
| when, by whom, from which store, and whether each read was readable | the reading cannot be trusted as current |

Live contention found by a walk and a retained conflict row are kept apart. Nothing deletes the
retained rows, so treating them as live would block a scope permanently on one historical refusal.
