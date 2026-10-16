# Tracking and implementation workflow integration

Here the parent and the child are layers of [the level table](../../crw-canon/references/hierarchy.md#the-levels).

The rules that planning and run setup keep to themselves: how a task resolves its milestone target, how it binds to a project and recovers that binding, and how an issue's child pair is chosen and recorded. What more than one skill reads (the levels, the tracking model, the release gate, the schedule contract, delivery and the execution roles) is in [crw-canon](../../crw-canon/SKILL.md). Apply these rules within the user's assignment and current host permissions.

## Resolve the project target

Use an explicit target for the current operation. When it is omitted, use the
current assignment and the verified management binding for this task. A
temporary question or link for another project does not change the persistent
binding; a change to that binding needs an explicit designation or switch.
Distinguish milestone refs (`OWNER/NAME#N`), Codex task/host IDs, Desktop project IDs, and
repository identity. One repository may support several milestones, and
a project delivers into one integration target, so work in several repositories is one
project per repository. Resolve names to stable milestone
refs using the supplied link, verified binding, and semantic scope. If same-name
candidates remain ambiguous, ask before writing or binding; do not pick by
title alone. A rename or changed initiative relation does not change a binding, and a title is not
read the other way either. The management title carries its project's product-family label by the
rule in [Set the app presentation and record](#set-the-app-presentation-and-record), so the label
is derived from the binding and is never evidence of one: a task is not bound to a project because
its title names that project's family, and a title carrying a different family is a presentation
gap to report rather than a binding to correct. An initiative is
itself a target only for an explicit designation to execute its approved projects, which binds at
that level through [Initiative supervision](../../crw-run/references/initiative-supervision.md);
cited as context by any other operation it changes no binding.

Use [Project parent binding](integrations.md#project-parent-binding) to designate, record, restore,
or switch the fixed management task, including its app title and pin. Refresh
volatile state before acting. An old or copied record locates context but does
not transfer another task's ownership or execution permissions. Keep binding
setup in that shared procedure and the requested operation with its existing owner.
Run, goal mode included, performs this setup itself. A binding-only request does not execute work.

## Project parent binding

Run, goal mode included, uses this shared setup and recovery procedure before execution.

Make this Codex task the continuing management point for one milestone.
Preserve that target across follow-up requests and context recovery. This child
orchestrates one project; its independent `child` sessions (a code id, not a layer) each orchestrate one issue
under the shared [scope of each level](../../crw-canon/references/hierarchy.md#scope-of-each-level), and a
supervisor above it, where an initiative has one, works through this child rather than through
its `child` sessions.

### Establish the link

Resolve the actual current task ID, host when available, and cwd from the host's
current identity and supported task tools. Resolve the supplied milestone
to its stable ref and URL; read its description, its linked canonical documents and relevant
current work. Inspect repository identity, applicable guidance, branch,
worktrees, and dirty changes when a repository is involved. A folder, task
title, or Desktop project ID is not a milestone ref.

Look for an existing management binding in the milestone's coordination
issue before changing anything. Match task, host, and project IDs. Reuse the
same binding on repeated requests. If another task is already the coordinator,
read its recorded ownership and current status without waking it. Ask only
when the user has not resolved a material ownership conflict.

An explicit replacement can update the coordinator link while preserving the
previous binding and its history. It does not transfer active workers or
execution permissions, message the old task, or unpin/archive it. A request to
inspect another project temporarily does not replace the persistent focus.

### Set the app presentation and record

A request to make this the fixed management task covers its matching title,
sidebar pin, and a compact management record in the milestone's coordination issue. Respect an explicit title,
no-rename, unpinned, or read-only constraint.

Use a concise project summary as the management task title, led by the linked
project's product-family label in brackets, as in `[CRW] 설치형 플러그인 전환`: the
short outcome the project exists for, named so the title also reads as the task
that manages it. Write the family the project actually carries, exactly as the tracking repository
spells it, without expanding, translating, abbreviating or changing its case, and
without a table of families kept beside the workspace's own. Initiative membership
is still not a prefix, and no initiative or multiple initiatives needs no
title-choice question. Preserve an explicit user title: the user's wording stays as
the body and takes the prefix in front of it, while an explicit fixed title, a
no-rename or a read-only constraint leaves the title as it is. Resolve same-name
projects by stable ID under the shared target rules before binding. If their
management titles would be indistinguishable, append a short project-ID suffix. Do
not rename the initiative or project itself, whose own name needs no product
prefix. This convention names the child's Codex task, its row of the one
[thread title table](../../crw-canon/references/hierarchy.md#thread-titles), which also holds the title of the
repository session and says who names a thread and when; the title of a `child` session is the rule of
[Child task titles](../../crw-run/references/task-packet.md#child-task-titles).

Take the family from the product-family labels of the milestone's issues, which are
spelled `family:NAME`. The prefix is the group, so the value carries its own provenance
— the `family:` label itself, or the family already recorded in this binding with its
source — and a label without the prefix is not that evidence, because a name alone is
not membership. Exactly one family among them is the family. None is a legitimate classification rather than an error, and more than one
is a question; in both cases leave the title unprefixed, record what is missing as
part of the unverified state below, and ask instead of choosing. Never read the
family from the repository, the working directory, the task's model or the tracking
repository's name, which often spells the same word and is a different field.

Apply the prefix once. A title already carrying this family keeps it — that is this
bracket alone, or this bracket separated from the body by a space, since a label may
itself contain a bracket and a longer one can begin the same way — and a family name
standing separably in front of the title is absorbed rather than repeated.
Otherwise the body is left exactly as it is: a label joined to what follows it is
part of the sentence, so the subject in `CRW를 설치형 …` and the issue code in
`CRW-137 · …` are text rather than prefixes. A leading bracket that is not this
family is neither overwritten nor stacked behind a second one; report it and take a
decision on that exact bracket, keeping it as body or replacing it, because an
obsolete family and a user's own words are indistinguishable from here.
`crw skill parent-title decide` settles these cases offline, and
`crw skill parent-title replay` holds it to recorded expectations; it proposes a title and writes
nothing. Any later surface that shows or edits this prefix reads the same label
through the same rule and the same helper rather than keeping a mapping of its own.

Discover the supported task rename and sidebar tools and their current schemas.
Apply changes only to the verified current task, and check its resulting title
and pin state in the app listing. If a capability is unavailable, complete the
supported parts and report the gap. Do not edit a session database or global
configuration to simulate a successful binding.

A rename that fails or is unsupported leaves the title unwritten: record the
requested title and the limitation, and carry on with the authorized work. A rename
that is accepted proves transport and nothing more. The title is verified when a
read by task ID returns the requested string; a read returning anything else is a
mismatch to report with both strings rather than a success with a caveat, and no
read at all is unread rather than a failed write.

Reuse a suitable coordination issue of the milestone. When only a canonical planning
issue exists, a compact management comment there is sufficient. Read the paginated
issue list of the milestone and candidate contents before creating an issue.
Create a small coordination issue in the milestone only if none is suitable.
Preserve specifications, unrelated content, and existing human edits.

Record only what supports recovery:

- Stable milestone ref/URL and canonical document links.
- Actual management task ID, host when known, and observed task title.
- Repository identity and checkout path when relevant.
- Assignment, delivery limits, and the source/date of the user's designation.
- Verified app title/pin results and any unsynced or unverified part.
- The task record read by task ID before and after a rename, showing the fields
  that read exposes unchanged. Whatever it does not expose is read from its owning
  surface or recorded unverified, rather than claimed preserved on its behalf.

Read back the saved comment and its milestone. Reconcile uncertain
writes by reading before retrying; an accepted request is not a verified result.
If tracking access is unavailable or writes are outside scope, retain a clearly
unsynced summary in the task's permitted private location and return the missing
step. Never store project bindings in installed skills, repository procedures,
or global memory. A verified title/pin and a verified tracking record are separate
claims; neither proves automatic wakeups or background execution.

### Continue from the fixed project

On recovery, locate this task's recorded binding and refresh live milestone,
issue, and managed-task state before acting. Use current assignment context
and scoped recall to find a lost record. A copied binding for another task does
not assign ownership here. Follow the shared target-resolution rules for
explicit one-off targets and changes to the persistent link.

Keep the recorded milestone ref when its name or product labels change. Existing titles are presentation, whether they carry a
product-family prefix or an initiative one, and neither is a reason to rebind or
rename during recovery. A project whose family has changed does not have its prefix
corrected on sight, and existing children are not renamed in bulk. Refresh the title
only within an authorized presentation change; do not migrate existing bindings
implicitly.

Load the existing owner for the requested operation:

| Request | Owner |
|---|---|
| Where to start or what to do next | [crw-status](../../crw-status/SKILL.md#choose-one-next-action) |
| Settle a milestone and its issues before they are registered | [crw-define](../../crw-define/SKILL.md) |
| Plan, roadmap, milestones, or issue scope | [crw-plan](../../crw-plan/SKILL.md) |
| Turn one short request into one ready issue (a lighter path than a plan) | [crw-add-issue](../../crw-add-issue/SKILL.md) |
| Execute the project, coordinate progress, or follow up on delivery | [crw-run](../../crw-run/SKILL.md) |
| Create/restore a child's native goal, which exists only where the user explicitly asked for one | [crw-run goal mode](../../crw-run/references/goal-mode.md) |
| Carry one task's authorized objective through a scoped PABCD completion loop | `crw-loop`, with `crw-pabcd` for the phases |
| Execute an initiative's approved projects through their existing children, or run its midpoint check | [the section of crw-run for the parent (repository session)](../../crw-run/SKILL.md#the-parent-the-repository-session), which reads [Initiative supervision](../../crw-run/references/initiative-supervision.md) rather than the project binding above |
| Compare delivery with accepted requirements | [crw-check](../../crw-check/SKILL.md) |
| Investigate contradictions or broken invariants | [crw-check](../../crw-check/SKILL.md#find-contradictions) |
| Diagnose structural debt after a verified cycle, read-only, and recommend bounded repairs; a chosen repair is added as an issue through crw-add-issue | [crw-refactor](../../crw-refactor/SKILL.md) |
| Find records that fall short of the agreed authoring rules, and have the owner of the body supplement the clear gaps | [crw-tidy](../../crw-tidy/SKILL.md) |

Keep one operation owner and load only the helpers it needs. Jun need not name
the skills. Binding alone does not launch the backlog, create workers or goals,
activate `crw-loop` or goal mode, change model settings, or install an automation. When the
same request also authorizes execution, finish the link and continue through
the requesting Run owner in that scope. Preserve an existing authorized run and its routine
follow-up; a status question does not pause it. When another operation owner
uses this reference for binding setup or recovery, return the result to that caller
instead of recursively starting its operation.

Return the linked project/document, actual app result, recorded scope, and one
next step or meaningful gap. Distinguish completed binding from work execution.

#### Child pair by issue type

A `child` session runs on one of two pairs, and the issue decides which. `Sonnet` and `SOL` are the names the
role policy gives its two child pairs, and a name here is only that: which model and effort a pair runs
is the policy's, read from the declared policy when the `child` session is created. This section carries no model
and no effort, because a document that carries one is a second source of truth beside the host's policy (see
[Execution settings by role](../../crw-canon/references/execution-roles.md#execution-settings-by-role)).

The axis is where the answer comes from. When a reference implementation, a specification, a golden
output or an existing pattern decides what the right result is, the `child` session's work is to meet it exactly
and to keep every outside rule that goes with it: that is the Sonnet pair. When nothing outside the
issue says what is right, because the answer has to be designed or found, it is the SOL pair.

| Shape of the issue | Pair | Example |
| --- | --- | --- |
| A port or a change of language that has an original, where "same as the original" ends the work | Sonnet | moving a module to another language and checking it against the original's recorded outputs |
| A fix that keeps golden, contract or byte compatibility; names, locations and wording; deletions and moves; wiring or registration in many places | Sonnet | renaming a command across its documentation, specs and tests; registering a subcommand in every list that names it |
| Writing (documents, skills, prompts, reports); UI or images judged by eye; operations where the order of steps is the point; an investigation that opens no PR | Sonnet | rewriting a skill's procedure; an installation runbook; a read-only audit of a record |
| A new mechanism, algorithm or state machine | SOL | a rule that decides which work is ready to start; a retry state machine |
| Performance or parallelism that a measurement ends | SOL | cutting a test suite's wall time by running its parts at once |
| A bug of unknown cause; a fix that has already failed twice or more; flakes, races, concurrency, cache consistency | SOL | a failure that appears only under load |
| An implementation that has to decide the structure of a new module | SOL | a new package whose interfaces nobody has drawn |

When choosing, look at four things. Where the answer comes from: something outside the issue that the
result can be compared with, or the `child` session's own design. How many outside rules the work must keep: the
more a `child` session has to follow exactly (a golden, a schema, a naming convention, a long procedure), the more
the issue belongs to the Sonnet pair. The shape of the end: "same as the original" ends a Sonnet issue,
and "a measurement improved, the cause is gone, a new invariant is proven" ends a SOL issue. And the
failure history: a fix that failed twice is an answer still to be found, whatever the issue first
looked like.

Two cases the shape does not settle:

- **A security judgment at the core.** An issue whose core is a security judgment (deletion protection,
  a permission or sandbox decision, secret detection) goes to the Sonnet pair from the start, whatever
  its shape. A SOL turn on such work was refused by the provider's cyber policy, and the issue should
  not meet that refusal in the middle of its delivery.
- **Half and half.** A clear case follows the table. An ambiguous case goes to the pair with fewer
  working `child` sessions under this child; a plan written before any `child` session runs counts instead the issues of
  the project that already carry a line, per pair. Once one pair holds more than 60 percent of them,
  border cases go to the other pair whatever the count of working `child` sessions says. The line then names
  the axis half-half.

**Precedence**, highest first: the user's explicit choice of model, effort or pair for this scope; the
pair the issue body's child pair line records; this table. Host and tool restrictions and the explicit
limits in force keep the place [Default independent execution](../../crw-canon/references/delivery.md#default-independent-execution) gives
them above all three. A choice the user makes for an issue is written onto its line with the reason, so
the record and the creation agree.

**The line.** An implementation issue carries one line, beside its size statement and under none of the
headings the [size check](issue-boundaries.md#check-the-size-of-an-issue) reads, so the line is
neither counted as a criterion nor read as a deliverable:

    Child pair: SOL (answer must be found) - a new retry state machine

The form is `Child pair: <the policy's pair name, Sonnet or SOL> (<axis>) - <the issue's type and the reason, in a few words>`.
The axis is exactly one of `reference exists`, `answer must be found` and `half-half`. A Korean issue
body may write the label as `자식 pair`; the pair name and the axis words stay as written here, so a
reader or a check can match them. The line records the pair's name (its family), never a model or an effort. An issue with
no line is routed through the table by the child that dispatches it: the child writes the answer as the
line on the issue and reads it back before it prepares the packet or creates the `child` session, so a later
child reading the issue finds the selection there. A note in the child's own coordination record is
only a pending proposal; where the issue cannot be written, the child reports that blocker and creates
nothing. The Launch packet that [crw-run](../../crw-run/SKILL.md#prepare-and-dispatch) keeps for an
issue with no recorded pair is the form of an issue this rule has not reached yet, and it is no default
pair for an issue without a line.
The line may also carry the issue's classification, its bundle and the source of the choice, in the form
given under [The extended line](#the-extended-line); a line without them stays valid.

**The packet.** The first packet of a SOL issue takes the form of the "SOL packet" section of
[task-packet.md](../../crw-run/references/task-packet.md); a Sonnet issue's takes the Launch packet.
The child takes the form from the recorded pair and does not choose it again from the issue text at
dispatch.

**Creating the child.** Read `get_capabilities` and state the entry of the child role's `pairs` (or the
single pair of a role that declares one) that belongs to the line's family, passing `role` so the host
checks the answer. Where the declared list holds exactly one such entry, create with it. Where it holds
none, the child picks no other pair. That is what a SOL line meets while the policy still declares only
the Sonnet pair, and what any line meets when a model change leaves its family with no declared pair;
the family is the test and not the number of pairs, so a policy whose only child pair is the SOL pair
stops a Sonnet line in the same way. Where the list holds two, nothing in the policy ranks them, so the
child does not choose between them. Either way the creation is a reported blocker, and the recovery is
the host declaring the pair or the user choosing a pair for the issue: the child states exactly the
entry the user names, which must be one the declared list holds, and writes the choice onto the line
with its reason; the line still records the family. Under a policy that declares only the Sonnet pair, a
SOL-line issue therefore waits for that choice, because there is no fallback pair.

The host refuses only a pair outside the role (`execution_role_mismatch`). A refusal before any call
sends nothing, and the same request id is used again with the corrected pair (see
[Bridge launch and recovery](../../crw-run/references/bridge.md)). Both pairs are valid for the child
role, so the host cannot see that a pair differs from the line, and that comparison belongs to the
child: it issues no creation whose pair is of another family than the line.

**After creation.** Read the model, reasoning effort and role back from the creation receipt: they are
the pair that was stated, and they belong to the line's family. The relay records the receipt's pair as
the `child` session's authorization and accepts any pair of the role
([codex-session-relay](../../crw-run/references/relay.md)), so this read-back is where a difference is
caught. A receipt of the other family is a mismatch observed after creation, and the `child` session does not go
on under a pair its line does not name. It ends in one of three ways: the user changes the pair and the
authorization is re-recorded from a user-attributed source; or the user accepts the pair the `child` session came
up on and the child writes it onto the line with the reason; or, where the recovery rules allow a
replacement, the `child` session is replaced on the right pair. A resend never changes a running task's model.
The receipt shows what was requested and applied; the served model stays unproven where no
served-model evidence exists.

**When SOL fails.** A SOL creation or turn that fails on a provider limit or a policy refusal is not
repeated. The child releases the issue on the Sonnet pair and records why: it rewrites the line as
`Child pair: Sonnet` with the cause and the date, then creates the `child` session on the declared Sonnet pair. A
refusal before any call leaves no task, so that creation is made under the same request id with the
corrected pair. A failure after the call may have left a task or a ledger row: the earlier task is first
proven inactive and its work and receipts kept, and the new `child` session is a replacement with a new id under
the recovery rules of [Bridge launch and recovery](../../crw-run/references/bridge.md). That
replacement moves the pair, which is the one change of settings
[Completion follow-up in an existing execution workflow](../../crw-canon/references/delivery.md#completion-follow-up-in-an-existing-execution-workflow)
allows.

##### Classify the issue by its kind of work

The shape table names shapes; the classification names the features that make an issue one shape or
another, so that two planners reading one issue write the same tags and a later review can say which tag
was wrong. Every tag is a feature visible when the issue is planned: it needs no result, no run and no
`child` session. The tags and their values are fixed English words, written the same in an issue of any language, so
a reader or a check can match them. No rule here names a programming language, a product or a repository;
examples stand only in the last column.

| Tag | Value | Read as | Example |
| --- | --- | --- | --- |
| `answer` | `reference` | something outside the issue decides what is right: an original, a specification, a golden output, a contract, a naming convention, an existing pattern, or a procedure whose order is the point | moving a module to another language and checking it against the original's outputs; an installation runbook |
| `answer` | `found` | nothing outside the issue decides; the `child` session designs the answer or has to find it | a retry state machine; a failure of unknown cause |
| `answer` | `mixed` | one part has an outside reference and another part has none, and the [boundary rules](issue-boundaries.md) do not split them | a port that also replaces its slowest part with a new mechanism |
| `output` | `code` | a change to code or its tests; documents that only accompany it do not change this | a new subcommand with its tests |
| `output` | `writing` | documents, skills, prompts, reports | rewriting a skill's procedure |
| `output` | `screen` | screens, layouts, images, design | a settings page judged by eye |
| `output` | `data` | transformation or reconciliation of recorded data | matching two ledgers row by row |
| `output` | `investigation` | opens no pull request: a finding, a measurement or an observed state, including an operational check | a read-only audit of a record |
| `procedure` | `goal` | one goal and the check that ends it, both stated up front: a machine check, or a reading against stated criteria | cutting a suite's wall time to a target; rewriting a page until stated criteria hold |
| `procedure` | `steps` | the work is to carry out a long procedure that the issue gives, where the order or the exactness of its steps is the point; writing a procedure is not this | a migration with a fixed order of operations |
| `procedure` | `open` | exploratory: no end is stated up front and the next step depends on what the last one showed | finding why a failure appears only under load |
| `reach` | `one`, `several` | the number of modules the work touches | a fix inside one module; a rename across a command's documentation, specs and tests |
| `check` | `machine`, `read`, `look` | how the result is verified: a machine check (tests, a build, a measurement, a comparison), reading and judging, or looking at screens or images | a test suite; a read-through against a checklist; a look at rendered pages |
| `history` | `fresh`, `retried` | `retried` when an earlier fix of this same problem had already failed twice or more when the issue was planned | a flake that two fixes did not remove |
| `core` | `plain`, `security` | `security` when the core of the work is a security judgment: deletion protection, a permission or sandbox decision, secret detection | a rule that decides what a sandbox may be asked to do |

Write the seven tags when the issue is written or refined, from what the issue states; where its text does
not settle a tag, amend the issue first and do not guess. Each tag takes one value: an issue that has both
kinds of answer is `mixed`, and every other tag takes the value of the work that ends the issue. Where two
values of `procedure` fit, take the one the issue's stated end names: `goal` when a check is stated up
front, `open` when none is, and `steps` only when following a given procedure is the work. `reach`
and `check` are recorded although no row below reads them yet, because a later change to the table can
cite only the features that were written down when the issue was planned.

##### Bundles

The tags map to three bundles: **Sonnet fixed** (the issue runs on the Sonnet pair), **SOL fixed** (the SOL
pair) and **flexible** (either works, and the count rule below picks). Read the rows in order; the first
that applies decides.

| Order | When the tags read | Bundle | Axis on the line | It restates |
| --- | --- | --- | --- | --- |
| 1 | `core=security` | Sonnet fixed | `reference exists` for `answer=reference`, `answer must be found` for `found` or `mixed` | the security rule |
| 2 | `history=retried` | SOL fixed | `answer must be found` | the failure history: a fix that failed twice is an answer still to be found |
| 3 | `answer=reference` | Sonnet fixed | `reference exists` | the shapes of the first three rows wherever something outside the issue decides |
| 4 | `answer=found`, `output=code`, `procedure=goal` or `open` | SOL fixed | `answer must be found` | the shapes of a new mechanism, a measured result, an unknown cause and a new structure |
| 5 | anything else: `answer=mixed`; `answer=found` with an `output` other than `code`; `answer=found`, `output=code`, `procedure=steps` | flexible | `half-half` | the ambiguous case |

Row 5 is an extension that reads the ambiguous case of the half-and-half rule. The shape table's third row
sends writing, screens, operations and investigations to Sonnet by shape, while the axis sends an answer
that nobody outside the issue decides to SOL; for such an issue the two disagree. The row's text is
unchanged: its examples have an answer from outside, which row 3 sends to Sonnet. Row 5 also holds a
disagreement one level down: an unknown-cause failure that comes with a fixed order of investigation steps
is SOL by its answer and Sonnet by its long procedure, while the SOL shapes name only `goal` and `open`.
The 180 combinations of `answer`, `output`, `procedure`, `history` and `core` (3 x 5 x 3 x 2 x 2) are
each caught by exactly one row.

**A flexible issue's pair.** The aim is roughly half of a project's issues on each pair, and the flexible
bundle is the part that can move toward it. The half-and-half rule names the counts; for a flexible issue
apply them in this order. First, once one pair holds more than 60 percent of the project's lines, the other pair, whatever
the working `child` sessions say. Otherwise the pair with fewer working `child` sessions under this child. Where those tie,
or no `child` session is working, the pair that fewer of the project's lines name. Where that ties too, either pair is
correct: the child picks one and records the pick. Working `child` sessions 1:1 with lines 6:5 are a tie on the
`child` sessions and go to the pair with five lines. A fixed bundle is never moved to balance the count; only a
flexible issue moves.

**A pair of the other family.** A pair of the other family than a fixed bundle's stands only with its reason
on the line: the user's choice, or the move to Sonnet under [When SOL fails](#child-pair-by-issue-type)
with its cause and date; or its provider is exhausted in a readable quota snapshot or explicitly unusable,
with the cause and date recorded before replacement. Under row 1 the Sonnet pair is the security rule's own
and needs no further reason. These exceptions change the pair, never the tags or bundle.

##### The extended line

The line may carry the classification, the bundle and the source of the choice after its reason:

    Child pair: Sonnet (reference exists) - rewriting a skill's procedure [class: answer=reference, output=writing, procedure=goal, reach=one, check=read, history=fresh, core=plain; bundle: Sonnet fixed; source: table]

A line in the form given under "The line" stays valid. It is a prefix of the extended form and states the
pair, the axis and the reason without tags, bundle or source:

    Child pair: SOL (answer must be found) - a new retry state machine

The bracketed group is the last bracketed segment of the line and starts with `[class:`. It holds the seven
tags in the order of the table above, then `bundle:` (`Sonnet fixed`, `SOL fixed` or `flexible`), then
`source:`, the step that decided the pair the line names:

- `user choice`: the user named the pair for this scope, and the reason says so (precedence above).
- `table`: this section's rules decided: the family of a fixed bundle, the count rule for a flexible issue,
  or the move to Sonnet under When SOL fails. After that move the tags and the bundle stay as classified,
  the source reads `table` and the reason names the cause and the date. A flexible issue whose source is
  `table` was chosen by the count, unless its reason names such a move.
- `quota`: a readable snapshot decided the pair under [the quota rule](#the-place-for-a-quota-rule),
  including a snapshot-confirmed exhaustion move. An unreadable snapshot never supplies this source.

The axis is the one the Bundles row gives. A Korean issue body may keep the label `자식 pair`; the key
words after it stay as written here.

The line sits on a line of its own after a blank line, outside any list, in a section the size check does
not read. The check keeps a heading that is deeper than a section it already reads inside that section, so
the line gets a heading of the same level as the last heading the check reads, or a shallower one, whose
words the check does not know: `## Child pair` after `## Verification` closes that section. The report then
lists the heading under `unread_headings`, a note that changes no count. With the line there, the check
returns the same decision, counts, edit regions and verification kinds for the body with the extended line,
with the line in the older form and without any line. The tag, bundle and source words match none of the
check's word lists; the reason, being free text, can, so a line left in a section the check reads, or at the
end of one of its lists, is read as that text and can change the counts.

##### The record at release

When a `child` session is released, the dispatching child records the pair choice in its coordination record, one row
per release, beside the launch record
([Record the pair choice at release](../../crw-run/SKILL.md#record-the-pair-choice-at-release) gives the
steps).

**Where, and why there.** The relay's release request has fixed fields and refuses one it does not know, and
its settings record holds a model and an effort and accepts any pair of the role, so it cannot say why a pair
was chosen; a field there would be a change to the relay, which this rule does not make. The coordination
record already holds each issue's launch facts (request id, task id, settings), outlasts the `child` session, and is
read by the child and, across projects, by the parent and by Jun. The issue's line states the choice made when the
plan was written; the row states what was acted on at release, and the two can differ: an older-form line
classified only at release, a user's choice made after planning, a move to Sonnet under When SOL fails.

**What the row holds.** The issue; the line as read, quoted, which restores the plan-time statement after
the issue or the rules change; the seven tags; the bundle; the pair as a family, with its model and effort
left in the launch record's settings entry, read from the creation receipt; the source at release, one of
`user choice`, `issue body`, `table` and `quota`, where `issue body` means a line already stated the pair
and the child followed it, and `table` also covers a move under When SOL fails applied at this release; the mark `derived at release` when the line carried no tags; for a flexible
issue whose pair the child itself chose, the counts it used; for a move under When SOL fails, its cause and
date; the version of the plugin manifest the rules were read from, which says which revision of these rules
classified the issue; the request id and the date. The command report adds `pair`, `source`, `rule`,
`default_pair`, `quota` (values and headroom, or unreadable with its reason), `window` (start, end, counts),
`limits`, and any `cause`, `date` and `classification_reason`. Keep the full report beside the row, including
stale decoded values when present; they are evidence of the read, not current capacity. A later change of the pair (a user's choice, a move under
When SOL fails, a replacement) adds a row and overwrites none.

**A line without tags.** The child classifies the issue by the tags above, takes the bundle from the Bundles
rows, and marks the row `derived at release`. Derived tags are not a selection: the pair stays the line's
(precedence above), the line is not rewritten, and where the family of the derived bundle differs from the
pair the row says so. That difference is the one exception to the rule that a pair of the other family
stands only with its reason on the line: a line in the older form was written before bundles existed, so its
difference is recorded and not corrected. A tag the issue's text does not settle at release is left blank
in the row with the reason, and where that tag decides the bundle the row reads `undetermined`; a blank tag
never holds a dispatch, and the issue is amended when its owner next writes it. For an issue with no line at all the rule under "The line" holds: the child writes the
line, in the extended form, and reads it back first.

##### Review the classification and change the table

**Result metrics never change the table.** Quality, speed, cost, review rounds, check failures during the work, quota used and
which model did better are not inputs to a tag, a Bundles row or a shape row, and no counter, score or
automation edits any of them. Judging a result is qualitative, and a table steered by results could tip all
work to one model. The table is refined only by what was visible when the issue was planned, and a
classification is judged against that: a success or a failure after release never reclassifies an issue.

**The review.** A review asks one question of each released issue: was the classification right? It reads the
issue as planned, its line, its release row, and what the delivered change and its packet show about the kind
of work: the modules the change touched, the procedure the packet had to spell out, the form of verification
the criteria used. Examples of a wrong tag: `procedure=goal`, but the packet needed long procedural
instructions; `reach=one`, though the issue's own criteria named several modules; `answer=reference`, though
no reference existed or was named; `check=machine`, though only a reading could judge the criteria. A
feature that only the work revealed, such as a module nobody could name when the issue was planned, is not a
wrong tag: the review records it as not visible at planning, since a change to the table can still ask
whether a new tag could have shown it. A user's choice and a move under When SOL fails are not
classification errors. The output lists the issue, the tag that was wrong, the value that
held and the evidence, a pull request or a packet line, and says nothing about which model did better. The
child runs it for its own project at close, from the release rows; the parent runs it across
projects when a change is considered; Jun can ask for it.

**Changing the table.** A change to a tag, a Bundles row or a shape row is a decision of the management
session or Jun. The decision is recorded with its evidence: the issues whose classification was wrong, the
tag that was wrong in each and the feature that shows it, and what the change would have given each of
them. The change is then an edit of this section, made as an ordinary commit on the task branch. It applies to lines
written after it, and the lines and release rows already written keep their values. No number of wrong
cases triggers a change by itself; the cases are evidence to read.

##### The place for a quota rule

Run `crw skill pair-choice choose [--snapshot quota.json] [request.json]` at release. The request is a JSON
file or stdin; the optional snapshot is a separate file. The command reads only these supplied inputs:
it never runs OCX, calls its API, reads its operating files or reads the usage ledger. No snapshot producer
is approved yet. Until a cache-only OCX projection exists, run without `--snapshot`: an eligible flexible
issue takes the count rule's default, with `quota.readable: false`, reason `no_snapshot_producer` and source
`table`. Creating a snapshot with a probe-capable OCX command is not this procedure.

**Release facts.** Supply `bundle`, `as_of` (RFC3339), `working` and `lines` (each `{sonnet, sol}`,
nonnegative integers up to 1,000,000), optional `tie` (Sonnet or SOL; omitted picks Sonnet and records it),
and `releases` (`{at, pair, bundle}` rows). Use one row per flexible issue in the window, excluding the
release now being decided; replacements update that issue's contribution rather than count it twice.
Fixed rows are ignored. The child serializes choosing and recording, so two choices do not share old
counts. Supply `recorded_pair` and `source` for a line: legacy `issue body` and `user choice` preserve it;
an extended flexible `table` or `quota` line is eligible for reselection. An `undetermined` legacy bundle
requires a preserved pair and `classification_reason`; it is never quota-reclassified. A fixed bundle
keeps its recorded pair or table family. None of this classifies tags or edits the table.

For example, a request without a snapshot (the two count objects default to zero when omitted):

```json
{"bundle":"flexible","as_of":"2026-01-01T01:00:00Z","working":{"sonnet":0,"sol":0},"lines":{"sonnet":0,"sol":0},"tie":"Sonnet","releases":[]}
```

**Snapshot contract.** `crw-pair-quota/1` is CRW-owned input, not a claim that OCX produces it. Sonnet
reads `claude`; SOL reads `openai`. Each side has `state` (`available`, `exhausted`, `unknown`),
`observed_at` (RFC3339) and named `windows` with `utilization` (used percent, 0–100) and `reset_at`.
This example is synthetic:

```json
{"schema":"crw-pair-quota/1","claude":{"state":"available","observed_at":"2026-01-01T01:00:00Z","windows":[{"name":"five-hour","utilization":70,"reset_at":"2026-01-01T06:00:00Z"}]},"openai":{"state":"available","observed_at":"2026-01-01T01:00:00Z","windows":[{"name":"five-hour","utilization":20,"reset_at":"2026-01-01T06:00:00Z"}]}}
```

Headroom is 100 minus the highest utilization among windows whose reset is after `as_of`. Expired
windows do not constrain it. Both sides must be complete, with an active window; `exhausted` must agree
with zero headroom and `available` with positive headroom. A missing/unknown side, malformed or partial
snapshot, unknown field, invalid window, future observation, or observation older than 30 minutes makes
the whole snapshot unreadable. Thirty minutes matches OCX's retained last-good report bound found in the source comparison, not its
older ordering-cache age. The parser accepts at most 1 MiB of UTF-8 JSON per input. A failed snapshot-file read
also takes the table default and records its reason. Token consumption never becomes remaining quota. Decimal utilization is compared exactly; numeric
literals are bounded to 128 characters and exponent magnitude 128.

**Decision and guards.** Start with the ordered count default under Bundles. With readable quota, switch
away from it only when the other side has strictly more than 10 percentage points of headroom. The
project's more-than-60-percent line rule still forces the opposite pair. Then cap the projected share of
flexible issues at 60 percent within an epoch-aligned UTC five-hour routing window (distinct from each
provider's quota reset window). If the candidate exceeds it and the other choice fits, use the other.
When neither fits (the first issue, or prior counts 1:1), retain the candidate and record
`window_rounding`: indivisible issues cannot meet 60 percent at those totals. If a feasible window cap
and the project rule force opposite pairs, hold with `guards_conflict`, rather than bypass either.

Confirmed exhaustion takes the other side and exempts balance guards; both exhausted holds. A supplied
`unusable: {sonnet: "cause", sol: "cause"}` is an authoritative provider-failure fact, not a quota estimate:
it applies the table's failure exception even without a snapshot, recording source `table`, cause and
UTC date. Two unavailable sides hold. A fixed bundle moves only for its own exhaustion/unusability;
positive headroom never moves it for balance. An explicit user choice, a legacy flexible line or an
undetermined line holds when its recorded provider is unavailable rather than silently rewriting it.
A fixed legacy line retains the fixed-provider failure exception. Host pair authorization still applies.

**Report and release.** Output is `crw-pair-choice/1`. `pair` is Sonnet or SOL (null on hold), `source`
is the step that decided it, `rule` lists applied rule codes, and the fields under The record at release
retain its quota evidence and counts. Exit 0 chooses; 1 holds; 2 rejects the request; 3 reports request
or output I/O failure. An unreadable snapshot is a reported fallback, not a command failure. Only a
readable quota decision writes source `quota`; preserved fixed choices from earlier `quota` lines now
record `issue body`. Write a changed eligible line and read it back before the packet, keeping tags and
bundle unchanged. Add the release row and report without overwriting earlier rows. The relay enforces
neither using this command nor the truth of supplied counts; missing rows remain review findings.
