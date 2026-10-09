---
name: crw-add-issue
description: "Turn one short request into one ready Linear issue that passes the release gate: criteria, edit regions, a decided answer, the test cases to write first and the done condition, with an open design question left under the design-first label. Use for 'add an issue', a one-paragraph bug or change request, or a finding to register; it is lighter than crw-plan, which decomposes a goal into projects and issues."
---

# CRW Add Issue

Write one issue a child can start from. A request of a paragraph becomes an issue whose body already holds what the [release gate](../crw-plan/references/integrations.md#the-release-gate) asks, so the issue is released and not sent back for missing items, and the child implements an answer instead of designing one. Registering the issue is the whole operation: it plans no project, milestone or initiative, and it releases nothing. A request that needs several deliveries, a new project or a schedule is a plan and goes to [crw-plan](../crw-plan/SKILL.md).

Consultation and draft-only requests return the issue body and write nothing. Invoking this skill implicitly does not supply write intent; a request to add, register or create the issue does, for that one issue.

## Read before writing

Read [Integrations](../crw-plan/references/integrations.md) for Linear access, document authority and the [repository resolution](../crw-plan/references/integrations.md#resolve-the-implementation-repository). Resolve the project from the request or the active project, and reuse it: never create a project to hold one issue. Search the project's open issues for the same problem first (paginate before concluding there is none). A duplicate is reported with its key, and the request becomes a comment proposal on the existing issue instead of a second issue.

Inspect the repository read-only to ground the issue: where the behavior lives, the tests that already cover it, the contract or invariant it must keep, and the neighbouring owner of the same concept. A guess about a file is not an edit region, and a path the repository does not have is not written.

## Fill the five items

Write the body in Korean, under these headings (the English headings of the gate are read the same). Add a short `## 배경` first: the problem or the user-visible outcome, and the source (the request, the finding or the evaluation it came from).

| Heading | What to write |
|---|---|
| `## 기준` | Observable criteria as a list: the behavior, the state that must survive, the important failure cases. One concept, one delivery ([issue boundaries](../crw-plan/references/issue-boundaries.md)). |
| `## 편집 영역` | The files or directories the change edits, each in backticks, and what stays out of it. |
| `## 정한 답` | The answer to every design question the request raises, checked against the repository's constraints (what the code already enforces, the packages it may not import, the records it may only append to). The child implements this and does not redesign it. |
| `## 먼저 실패하게 쓸 시험` | The test cases written first and seen failing, named by the behavior each pins. |
| `## 끝 조건` | Which command must give which result on the final head, each command in backticks. |

Add the repository label and the explicit owner/repo, the prerequisite relations that truly hold, and the child pair line as [Child pair by issue type](../crw-plan/references/integrations.md#child-pair-by-issue-type) sets it. Reuse existing labels and the existing team conventions; add no label scheme.

## When a design question remains

Do not invent the answer to make the item look filled. A question the repository and the request cannot settle goes under `## 열린 결정`, the `정한 답` states what is already decided, and the issue gets the label `설계 먼저` so the state shows in Linear. Return the questions to the user or the parent as the design to obtain: an architect-role proposal that checks the repository's constraints, and the parent's decision. The issue is released only after that result stands in `정한 답`, the open decisions are empty and the label is removed. Choosing the model or role for the design is outside this skill.

## Check, write, read back

Build the issue as JSON (`id` or a placeholder, `title`, `description`, `labels`) and run `crw skill issue-ready check` on it before any write. Fix every `missing` item it names that the evidence can fill. What remains is either a `설계 먼저` issue with its open decisions listed or a stated gap in the report; do not write an issue claiming a ready state the check did not give.

With write intent, create the one issue (title naming the result, in the existing Korean title convention), then read it back from Linear and run the check on the returned record. Report the key and URL, the check's decision as the readback gave it, the open decisions, and anything the request left that this skill did not cover. After an uncertain write, look up the issue by title in the project before retrying, so a retry does not create a second one.
