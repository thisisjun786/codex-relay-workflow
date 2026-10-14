---
name: crw-tidy
description: "Diagnose, read-only, which documents and issues fall short of already-agreed authoring and management rules; Supplement the clear gaps only as the owner of that body. Use for a rule-conformance sweep or tidy pass; crw-check owns delivery and contradictions, crw-plan the rules themselves."
---

# CRW Tidy

Here the parent (repository session, relay role `supervisor`) and the child (project session, relay role `parent`) are layers of [the level table](../crw-canon/references/hierarchy.md#the-levels).

Compare tracking records against the rules already agreed for writing and managing them, and close the gaps whose evidence is unambiguous. Tidy judges a record against a rule that already exists; it never decides what the rule should be. It has two jobs, kept under two headings because they have different actors:

| Job | Who runs it | Writes |
|---|---|---|
| [Diagnose](#diagnose) | any actor | nothing |
| [Supplement](#supplement) | the owner of the body being written; the parent's store-wide sweep sends each supplement to its owner | the one field being supplemented, in the owner's own record |

A request to check, review or diagnose stays at the first job. A request to find and tidy also carries the second, for the bodies the caller owns, without asking again for each clear gap.

## Diagnose

Diagnose is read-only and any actor may run it: the user, the top session, the parent, a child, or a task asked to ([the level table](../crw-canon/references/hierarchy.md#the-levels)). It ends in a disposition for every judged item, the gaps written up as proposals, and a report.

### Connect the sources

Read the shared [tracking model](../crw-canon/references/tracking-model.md) first. It owns the rules this skill judges against, and restating them here would create the second copy that goes stale. Use `gh` as [Use the tracking access](../crw-canon/references/tracking-model.md#use-the-tracking-access) describes, exhausting pagination before calling anything absent. Use `crw-recall` to locate a decision whose record you cannot find, and restate the requested scope and confirm it when it is genuinely ambiguous. A missing or failing tracking access produces a partial audit that names its unread scope, never an assumed clean result.

### Pin the scope and the rules

Record the initiative, project, issues and documents you were asked about, each by stable ID with its updated-at, and for every implementation issue its delivery (the pull request when one exists, otherwise the task branch and its head) and the existing assignment alongside its body. Those three inputs are what the repository surfaces are judged on, so read them before judging rather than after.

Fix the rule source in the same pass. A rule is already agreed when Integrations states it, when an accepted tracking decision carries it with a ref and a date you can cite, or when the user's own later correction supersedes either of those with a traceable source and date, which [Tracking holds canonical documents](../crw-canon/references/tracking-model.md#tracking-holds-canonical-documents) allows even before the canonical document catches up. Anything else is a candidate rule: name it, route it to [crw-plan](../crw-plan/SKILL.md), and judge no record against it. An issue status, an assistant proposal and a newer local draft are none of them decisions.

### Judge the surfaces

Six surfaces are in scope: the `저장소` repository label, the explicit repository address in the body, the required body sections and linked documents, the project an implementation issue belongs to, an approved decision the record has not yet reflected, and the release readiness of an unreleased implementation issue. [Surfaces](references/surfaces.md) holds each one's applicable target, its named exceptions, and the condition that makes a gap clear enough to supplement.

Give every judged item one disposition:

| Disposition | What it means |
|---|---|
| Conforms | The rule applies and the record already satisfies it |
| Gap | The rule applies and the record falls short of it. Whether the gap is closed or only proposed is the next job's decision, under [Supplement](#supplement) |
| Exception | The rule's own text exempts this record: non-development work, an unresolved target, or a preserved historical multi-repository case |
| Conflict | Sources that should agree do not, so no supplement follows from the evidence |
| Unverified | A partial listing, a read failure, an unknown-result write, missing access, or corroborating evidence the rule requires that does not exist yet, left the question open |

Where more than one row could apply, take them in this order: Exception when the rule expressly exempts the record, then Conflict when binding sources already disagree, then Unverified when the evidence a judgement needs is absent or unread, and only then Conforms or Gap. A shortfall whose sources disagree is a conflict, never a gap to be filled with whichever passage you happen to be able to quote.

An exception is not a defect and is reported as the exception it is. A conflict is recorded with the disagreeing sources and the decision that would settle it; do not resolve it by taking whichever reading needs the smallest edit. Unverified is the honest outcome when a read failed or the evidence a judgement needs is simply not there yet, and it never becomes "no violation found".

## Supplement

Supplement writes, and only the owner of the body being written runs it, under the levels' record ownership in [scope of each level](../crw-canon/references/hierarchy.md#scope-of-each-level):

- the parent for a milestone description it opened;
- the child for a project or issue body (an issue child returns its proposal to it);
- the parent runs a store-wide sweep, which is requested through `crw manage`: the parent holds the management seat, with the store seat read through it, and no tracking body, so it sends each supplement to the owner of that body through `crw manage` and writes none itself. A body with no owner bound stays a proposal in the sweep report for Jun.

Any other actor that finds a gap hands the owner the proposal Diagnose produced, carrying the passage it proposes, and writes nothing, as [record writes and returned proposals](../crw-canon/references/tracking-model.md#record-writes-and-returned-proposals) says.

The request wording sets the authority. Check-only wording returns proposals and writes nothing at all. A request to find and tidy, or a tidy step inside an assignment that already authorizes record updates under [Completion follow-up](../crw-canon/references/delivery.md#completion-follow-up-in-an-existing-execution-workflow), authorizes the clear supplements across the pinned scope for the bodies its owner holds, so do not re-ask item by item for permission already given. Explicit read-only, report-only or narrower limits win. No wording extends tidy to closing, deleting or reorganising issues, or to introducing a rule.

Every gap is either written or proposed, and none is dropped for being awkward. Write one only when its content is quotable from an accepted source and its target is a body its owner holds. Quotable but held by another level is a proposal rather than a write: a child does not write the initiative's body, which belongs to its parent. Content you would have to compose is a proposal too, carrying the passage you propose so its owner can accept it without starting over.

Write in this order, one item at a time. Read the record again immediately before the write and judge against that read; where the intended end state already holds, record that it conforms and write nothing. Change only the field being supplemented, preserving the body, the other labels, the relations and the history. Read the record back and confirm from the returned record rather than by searching its text. After an ambiguous result, read existing state before repeating a create, and report an unknown result as unknown. Re-running the same request over unchanged records therefore writes nothing and creates no duplicate label, comment or document. Where the pre-write read differs from the pinned scan someone else has moved the record: re-judge it on what you just read, and leave the field they touched alone for this pass rather than overwriting their change.

## Report and route

Report the inspected scope, the rule source behind each finding, the actual before and after of every write, the exceptions, the conflicts with the decision each one needs, the unverified items, and the scope you did not reach. Keep those last two visible. A pass that quietly drops them reads as a clean sweep of ground nobody looked at.

Route what tidy does not own. Two currently binding sources that require incompatible things is a contradiction for [crw-check](../crw-check/SKILL.md#find-contradictions). Whether delivered work meets its accepted criteria, including [Implementation Done](../crw-canon/references/tracking-model.md#implementation-done), belongs to [crw-check](../crw-check/SKILL.md). Anything whose fix needs wording that no existing source carries is a change and goes to [crw-plan](../crw-plan/SKILL.md), and so does an unresolved target that execution is now waiting on: report it as the exception it is and send the decision there, rather than settling it here to close a row. Live execution and worker correction stay with [crw-run](../crw-run/SKILL.md); tidy dispatches nothing and wakes no task.

Under a management assignment that already authorizes record updates, record the summary in the coordination issue of the milestone and read it back. A check-only or standalone pass returns that record as a proposal instead, because the wording that made the pass read-only covers this write too. Tidying a record never authorizes changing what that record requires.
