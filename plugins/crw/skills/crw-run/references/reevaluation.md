# Re-evaluating the candidate set

The pass [Keep a project run moving](../SKILL.md#keep-a-project-run-moving) and
[Establish the project baseline](../SKILL.md#establish-the-project-baseline) require: which read
observes each forcing event, what the pass records per candidate, and the closed set a hold is
recorded in. It adds no scheduler, no store and no cursor. Every word that already has an owner
stays there: capacity bounds and `unmeasured` in
[Start policy](start-policy.md#what-bounds-the-number-actually-dispatched), reader vocabulary in
[codex-session-relay](relay.md), the turn-ending prohibition in
[OPS-8.1](operations.md#ops-81-parent-continuation-and-waiting), and where the record lives in
[Task packet](task-packet.md).

A pass is a full read. No landed reader is incremental, and nothing records where the last pass
stopped, so a resume reads the whole set rather than a delta.

## Which read observes each forcing event

| Event | Read | Skill-level usage |
| --- | --- | --- |
| Child finished and is reviewable | `assignment-show --relationship` | [relay.md](relay.md#verify-the-current-revision) |
| Child stopped for a person, or asked for a decision | `dispositions-show --project` or `--relationship`; the decision is `workReport.cxcStatus` on the same event | [relay.md](relay.md#which-children-stopped-and-whether-anyone-was-told) |
| An integration landed | `merge-turn-show`, beside the assignment's merged mark | named here only |
| A new assignment attached to this project | `linkage-outstanding`, `linkage-attach` | named here only |
| A peer's edit-region agreement changed | `region-show` | named here only |
| Capacity changed | `capacity-show` | named here only |
| This parent resumed or recovered | the coordination record, then the reads above | [SKILL.md](../SKILL.md#resume-and-handoff) |

"Named here only" means the command exists and no skill file documents its use yet. Name it, read
its own output, and do not infer a usage contract this repository has not written. Documenting
those four is a follow-up, not something this pass invents.

### What the stopped-children read does and does not answer

| Limit | Consequence for the pass |
| --- | --- |
| `--project` returns only assignments with a live attachment to that project | An unattached child is absent by project and readable only by `--relationship`. A standalone issue is read per relationship |
| `counts.blocked` counts a settled `blocked_needs_input` outcome | A contested child has no outcome and is **not** in that count; read `counts.contested` beside it |
| `reviewable.head` is `not_derived_here` | Which revision a generation stands on is answered by `assignment-show`, never by this read |
| `basis: none` | No execution-only disposition in this generation, which is not the same as nothing being wrong |
| It reads what the child **emitted** | It is not an observation path. OPS-8.1 still requires an emit path or a bounded observation path for the disposition itself |
| `deliveryUnmeasured`, `recipientUnmeasured` | Nobody established the fact. Never "it was not delivered" and never "the recipient did not see it" |
| `workReportMissing` | For those events `BLOCKED`, `UNSAFE` and `NEEDS_HUMAN` cannot be told apart, and none of them is inferred |
| Unreadable store exits refused with the whole payload | That is `defer:disposition_unreadable`, carrying the reported detail. A readable store with an empty list exits 0 and is a different answer |

## Decisions and what clears them

Closed set. A hold names the condition a later pass re-reads, and the row it rests on.

| Decision | Clears when | Evidence it rests on |
| --- | --- | --- |
| `dispatch` | — | the reads below that let it through |
| `defer:blocked_prerequisite` | the named symbol, schema or region arrives, proved by the landed revision or merged mark of its carrier | the carrier's merge turn or assignment mark |
| `defer:no_capacity` | a slot is released, or the bound that decided it changes | the bound recorded under [Start policy](start-policy.md#what-bounds-the-number-actually-dispatched), plus the retained refusal where one exists |
| `defer:capacity_unmeasured` | an observation is actually taken | the recorded `unmeasured` and what prevented the measurement |
| `defer:edit_overlap` | the peer agreement settles, or the overlapping region is released | the agreement and its follow-up |
| `defer:merge_window` | the window's holder releases it, or this candidate is promoted | the merge turn; integration into a shared target stays serial |
| `defer:ownership_unverified` | a store this process measured answers the ownership question | `doctor --issue` with `storeAgreement` `same`; `holds` null, an unreadable store, or a state directory this process cannot use all land here |
| `defer:ownership_ambiguous` | the store reports one responsible child | the ordered pair below |
| `defer:disposition_unreadable` | the store becomes readable | the refused read and its detail |
| `defer:disposition_contested` | a fresh execution generation, or a declared supersession | the contested dispositions, all of them, with no winner chosen |
| `defer:authority_pending` | the authority case that produced it is answered | the [Start policy](start-policy.md#cases-this-policy-is-accepted-against) case recorded at the start adjudication |
| `skip:already_owned` | not a hold; the issue has a responsible child | `holds` true, and the existing owner is reused rather than replaced |
| `skip:out_of_scope` | an authorized scope change admits it | the approved set this run was designated against |

There is no escalation value. A review round count has nowhere to land, because the parent rules
on the criteria and the correction scope and returns the work to the same child
([Return corrections](../SKILL.md#return-corrections-to-the-existing-task)); only new scope, new
authority, or a real contradiction in the criteria goes upward.

### Proving ownership, and proving it is not ambiguous

One read answers whether the issue is held. A second is needed only to tell one owner from
several, and it is ordered because the second one constructs a store.

1. `doctor --issue` first. It constructs nothing, carries `storeAgreement`, and is deliberately
   narrow: it reports that a live relationship exists and which child owns it, and says nothing
   about a second.
2. Only where step 1 reported a readable store with `storeAgreement` `same`,
   `assignment-find --issue` reports the scoped, unscoped and ambiguous readings. Compare the
   store identity it carries against what step 1 measured; a disagreement is
   `defer:ownership_unverified`, never a choice between them.

A process that cannot use the state directory cannot run step 2 at all. That is
`defer:ownership_unverified` with the reason, not an absence of ambiguity.

## What a pass records per candidate

| Field | What its absence means |
| --- | --- |
| issue and project it was evaluated in | outside this parent's scope; it is not evaluated here |
| the live assignment, where there is one | never dispatched under this project |
| existing owner, and how ownership was proved | unproved is not unowned |
| assignment state and what it waits on | unregistered, which is not finished |
| the event the trigger fired on, with its producer | no final event in this generation. A daemon-observed outcome and a child-asserted one are different facts |
| prerequisite, named as a symbol, schema or region, with the read that will prove it arrived | no prerequisite claimed. A shared repository or package is not a prerequisite, because nothing re-reads it |
| capacity, with which bound decided and how that use is known | no limit declared, which is not room to run |
| edit region and overlap | the region was never proposed; overlap is unknown, not none |
| merge window | no turn requested; it does not apply before the pull request stage |
| the decision, from the closed set | the record is incomplete |
| the retained row or reading the decision rests on | the decision is unsupported, and a later reader treats it as unverified |
| when, by whom, from which store, and whether each read was readable | the reading cannot be trusted as current |

Live contention found by a walk and a retained conflict row are kept apart. Nothing deletes the
retained rows, so treating them as live would block a scope permanently on one historical refusal.

