---
name: linear-focus
description: "Designate, restore, or switch the current Codex task as a Linear project's fixed management task. Use for '이 세션을 이 프로젝트 고정용으로', '프로젝트 진행 창구로', or restoring that link; route planning, execution, and checks to their existing Linear skill owners."
---

# Linear Focus

Make this Codex task the continuing management point for one Linear project.
Preserve that target across follow-up requests and context recovery.

Read [Integrations](../linear-plan/references/integrations.md) for shared target
resolution, document authority, access, and inherited authorization.

## Establish the link

Resolve the actual current task ID, host when available, and cwd from the host's
current identity and supported task tools. Resolve the supplied Linear project
to its stable ID and URL; read its linked canonical documents and relevant
current work. Inspect repository identity, applicable guidance, branch,
worktrees, and dirty changes when a repository is involved. A folder, task
title, or Desktop project ID is not a Linear project ID.

Look for an existing management binding in the project's linked coordination
record before changing anything. Match task, host, and project IDs. Reuse the
same binding on repeated requests. If another task is already the coordinator,
read its recorded ownership and current status without waking it. Ask only
when the user has not resolved a material ownership conflict.

An explicit replacement can update the coordinator link while preserving the
previous binding and its history. It does not transfer active workers or
execution permissions, message the old task, or unpin/archive it. A request to
inspect another project temporarily does not replace the persistent focus.

## Set the app presentation and record

A request to make this the fixed management task covers its matching title,
sidebar pin, and a compact Linear management record. Respect an explicit title,
no-rename, unpinned, or read-only constraint.

Use the actual linked initiative name followed by a concise project summary:
`Initiative name - Project summary`. Read the project's initiative relation;
do not infer it from the repository or duplicate the initiative prefix in the
project summary. Preserve an explicit user title. If there is no initiative,
use the project summary alone; if multiple initiative relations make the choice
unclear, ask one focused question before renaming. Do not change the
initiative/project names themselves. Continue independent binding preparation
while the title choice is unresolved. This convention names the management
task; execution-task titles follow [Child task titles](../linear-run/references/task-packet.md#child-task-titles).

Discover the supported task rename and sidebar tools and their current schemas.
Apply changes only to the verified current task, and check its resulting title
and pin state in the app listing. If a capability is unavailable, complete the
supported parts and report the gap. Do not edit a session database or global
configuration to simulate a successful binding.

Reuse a suitable linked coordination document. When only a canonical planning
document exists, a compact management section there is sufficient. Read scoped,
paginated document listings and candidate contents before creating a document.
Create a small project-linked coordination document only if none is suitable.
Preserve specifications, unrelated content, and existing human edits.

Record only what supports recovery:

- Stable Linear project ID/URL and canonical document links.
- Actual management task ID, host when known, and observed task title.
- Repository identity and checkout path when relevant.
- Assignment, delivery limits, and the source/date of the user's designation.
- Verified app title/pin results and any unsynced or unverified part.

Read back the saved document and its project relation. Reconcile uncertain
writes by reading before retrying; an accepted request is not a verified result.
If Linear access is unavailable or writes are outside scope, retain a clearly
unsynced summary in the task's permitted private location and return the missing
step. Never store project bindings in installed skills, repository procedures,
or global memory. A verified title/pin and a verified Linear record are separate
claims; neither proves automatic wakeups or background execution.

## Continue from the fixed project

On recovery, locate this task's recorded binding and refresh live project,
document, and managed-task state before acting. Use current assignment context
and scoped recall to find a lost record. A copied binding for another task does
not assign ownership here. Follow the shared target-resolution rules for
explicit one-off targets and changes to the persistent link.

Load the existing owner for the requested operation:

| Request | Owner |
|---|---|
| Where to start or what to do next | [linear-next](../linear-next/SKILL.md) |
| Plan, roadmap, milestones, or issue scope | [linear-plan](../linear-plan/SKILL.md) |
| Run work, coordinate progress, or follow up on delivery | [linear-run](../linear-run/SKILL.md) |
| Compare delivery with accepted requirements | [linear-check](../linear-check/SKILL.md) |
| Investigate contradictions or broken invariants | [linear-logic](../linear-logic/SKILL.md) |

Keep one operation owner and load only the helpers it needs. Jun need not name
the skills. Binding alone does not launch the backlog, create workers or goals,
activate a CXC Loop, change model settings, or install an automation. When the
same request also authorizes execution, finish the link and continue through
`linear-run` in that scope. Preserve an existing authorized run and its routine
follow-up; a status question does not pause it. When another operation owner
calls this skill for binding setup or recovery, return the result to that caller
instead of recursively starting its operation.

Return the linked project/document, actual app result, recorded scope, and one
next step or meaningful gap. Distinguish completed binding from work execution.
