# Tracking model

Here the parent is a layer of [the level table](hierarchy.md#the-levels).

How the tracking repository holds the work: how milestones, issues and labels relate, which document is canonical where, which level writes which record, and when an issue is Done. The levels themselves are in [hierarchy.md](hierarchy.md).

The name a report, a commit or a pull request gives a piece of work is in [naming.md](naming.md).

## Tracking operating model

Keep the tracking repository, the work unit, and the code repository separate:

- The work is tracked in one GitHub repository, set in the `tracking` section of the CRW configuration file (`"tracking": {"kind": "github", "repo": "OWNER/NAME"}`; `crw config show` prints it and `crw config check` reads it back). It is a different repository from the code repository (`manage.repository`) and may be a private one: a record carries operational paths, thread ids and host names that a public code repository must not. An issue or milestone is named `OWNER/NAME#N`. A fact is written once: plans, issues, decisions and progress live in the tracking repository, code, commits and release notes live in the code repository, and a commit or pull request names the issue it serves by its `OWNER/NAME#N`.
- A **milestone** is the unit of the child (project session): one milestone, one child. Its description opens with the end condition on its first line, and a larger goal is written in the description too, because there is no layer above the milestone to hold it. Open a milestone when the user requests one or accepts a proposal for it; a narrow plan update, a large backlog or several repositories alone do not authorize extra milestones. Reuse the existing refs and meaningful scope, name the result naturally, and do not rename or move existing milestones merely by loading these instructions.
- An **issue** is one unit of work. A **sub-issue** splits a larger issue and may live in another repository. Order is written in the body of the issue that waits: a line `Blocked by: #N` (`OWNER/NAME#N` for another repository), which CRW reads (`crw flow plan-input` turns each blocker that is an open issue of the milestone into the order of the plan) and a person reads as written. There is no relation field, and a `Blocked by:` line inside a code fence is not read.
- **Labels** carry priority, kind and area, and state is only open or closed. `P0` is blocking now, `P1` must happen inside the milestone, `P2` and `P3` come next; `defect`, `feature`, `test` and `docs` are the kinds; `area:…` labels name the area; `blocks-real-use` marks what stops a run from finishing without a person, `real-use-caveat` a limit that is known and kept, `found-in-trial` what a trial found; and `설계 먼저` (design first) holds an issue back under the [release gate](release-gate.md#the-release-gate). "In progress" is shown by the assignee and the last comment. An issue is closed as completed or as not planned, and the reason and a comment tell Done, canceled and duplicate apart. Reuse the agreed labels; discuss a new scheme when a need arises instead of creating it, preserve unrelated labels, and keep descriptions brief. Boards and views are for reading and are configured only when requested; the issue is the record and is never written twice on a board.
- The **code repository of an issue** is named in its body (`OWNER/NAME` or a URL), apart from the tracking repository. Reference-only and legacy repositories belong in links, not as a second target. Non-code work and an unresolved target may leave it out; resolve a code target before execution.
- A **comment header** names the writer. Every comment goes up under one account, so the first line of a comment an agent writes is `[layer · session]`: the layer by the name of [the level table](hierarchy.md#the-levels) and a short id of the session, for example `[child · 01a1…]`. The text follows after a blank line.
- A **recorded time** comes from a reading made for that record: run `date -u +%Y-%m-%dT%H:%M:%SZ` immediately before writing an observation, or copy the timestamp of the relay event the record describes and name that event. Keep event time and observation time distinct. Never estimate a time or inherit another layer's time; if neither source can be read, record the time as unknown with the missing source.

For example, a requested "Complete installation and first launch" outcome across the core, desktop and installer repositories is three milestones, one per repository, each with its own single integration target and issues that name that repository. The shared goal is written in each description, and a non-code planning issue names no repository.

Do not rewrite existing issues merely by loading this model. When a migration is authorized, classify each issue from its accepted scope and delivery evidence, and preserve historical multi-repository exceptions without forcing a false single target; reconcile them before new execution.

`tracking.kind` `linear` is a read-only archive setting for looking up the old record, never written to: an issue moved from the archive carries its old number at the head of its body and the label `from-linear`.

### Issue-to-PR mapping

Today's operation is one implementation issue per packet, and that packet delivers that one
issue: several deliveries for one issue, or one delivery covering several issues, are reconciled
through `crw-plan` before new dispatch. Split work requiring several deliveries into separate
issues with explicit `Blocked by:` lines, even within one repository. A milestone has one integration
target, so a multi-repository outcome is one milestone per repository, with one issue per delivery.
Batches coordinate separate issue/delivery pairs; they do not combine issues into one
delivery. Referencing a related issue is not claiming to deliver or close it. This is today's
operation under the work-unit rules (`POLICY.md#work-units-review-and-integration` in the CRW repository),
not a standing one-to-one rule: several packets for one feature are allowed by policy and
switch on when their support lands.

Keep review fixes on the same issue and delivery. A necessary replacement PR retains
the superseded link and names the one current delivery; it does not create a
second simultaneous delivery for the issue. A new change after that delivery
has landed gets a new issue and delivery, except a defect inside the finished issue's own specification, criteria or promise: that reopens the same issue ([After the merge](../../crw-run/references/merge-readiness.md#late-review-threads)), and only a defect outside the issue gets a new issue. Research, design, or operational work with
no repository change uses an explicit non-PR result and verification; do not
create an empty PR merely to fit the rule.

When existing work breaks this mapping, reconcile its scope and ownership
through `crw-plan` before new dispatch. Preserve active work, refs, and history;
do not silently split, close, or reassign live issues. Editing these instructions
does not migrate existing work or alter the relay's runtime contracts.

### Record writes and returned proposals

A tracking write belongs to the task whose own record it is, under
[scope of each level](hierarchy.md#scope-of-each-level), and that holds for every
sub-task an owner opens rather than for a registered child alone. A planning sub-task drafting a
specification, an implementation child, a review or audit helper and an internal helper agent all
reach the same boundary: they read what their assignment allows, and they create, edit and delete
nothing there, no issue body, no label, no comment, no sub-issue link, no `Blocked by:` line and no close. Whether a relay
holds the assignment decides where a receipt goes, not who writes. An executor never writes its result to
an issue itself: the child verifies it and writes on the executor's behalf.

What a sub-task returns instead is the issue ref, the revision (the `updated_at`) it read,
why the change is needed, the smallest change that would do, and links to the evidence and
artifacts behind it. A local draft of the proposed wording is useful and is still a draft: it is
reported as a proposal waiting on its owner, never as applied in the tracking repository, and a file, a branch or a
message holding that text somewhere else is the same draft.

Exposure is not authorization. A token that reaches the tracking repository, `gh` on the path, a broad
permission profile, and an assignment naming the very issue it is about are none of them a
grant, and an owner's authority over its own record does not travel to a sub-task with the work:
authority comes from the assignment, never from the reachable surface. Nor is there a route around
it, since another client, a script, a CLI, a scheduled job or the sub-task's own helper agent
writing on its behalf is the same write. An explicit later instruction from the user is decided on
its own terms, for the records it names.

The owner that receives a proposal reads the record as it stands now before applying anything. The
proposal names the revision it was built on, so a record that moved since is reconciled against
the evidence rather than overwritten: the parts that still hold are applied, and a part another
actor has already changed or contradicted is raised instead of being reverted. Apply only what
this assignment's own authority covers, preserve unrelated fields, history and other people's
edits under [Tracking holds canonical documents](#tracking-holds-canonical-documents), and read the
result back before reporting it, as
[Use the tracking access](#use-the-tracking-access) requires. A write the
assignment already authorizes is simply made, not sent back to the user for an approval already
given.

A write that fails or returns an ambiguous result stays with that owner: read the current state,
record what happened, and retry the write itself. It is never a reason to re-run the sub-task's
completed implementation or verification, which is finished work at a revision the failed write
never touched. Until a readback confirms it, the record is reported as unwritten with its proposal
still open.

### Resolve the implementation repository

Resolve the repository the issue body names (an explicit GitHub `OWNER/NAME` or URL)
against its accepted scope, current delivery (its pull request when one exists, otherwise its task branch and head) and existing assignment.
The milestone description and links do not assign
repositories to its issues. Identify reference-only repositories separately.
If the issue's named target conflicts with its PR or ownership record, reconcile the conflict
before writes; a folder name or convenient checkout does not break the tie.
Ask only when the current evidence cannot settle a material target choice.

Before new execution, verify the actual remote URL, intended integration branch
from repository policy, and full fetched baseline commit. A default branch and a
remote named `origin` are not universal integration targets. Record the selected
remote name and distinguish a contribution fork from its integration repository.
Use the existing workspace-assignment interface after this identity check; do not
create a new checkout allocator or a fake repository or milestone to fill a missing target.

On resume, recover the responsible task, checkout and issue branch first. Inspect
its worktrees, dirty state, local-only commits, remote identity and recorded
baseline. Fetching current truth does not authorize resetting, rebasing, cleaning,
stashing, moving or recreating that work. Reconcile ancestry and prerequisites
without replacing an existing assignment with a freshly cloned default branch.
For a new task, use a task-owned checkout under the applicable workspace policy.

Work needing PRs in multiple repositories becomes one milestone per repository, each with its
own single integration target, and separate dependent issue/PR pairs across them; reading or validating another repository alone does not make it a code
target. Standalone issues follow the same resolution rule.
Research or design with no repository change may explicitly have no code target;
do not require a remote, branch or Git baseline for that non-PR result.
Keep its source baseline and delivered output identity under the non-PR evidence rules.

### Implementation Done

An implementation issue is Done, closed as completed, when every accepted criterion maps to a commit that actually landed on the
intended integration target, and a partial implementation never closes it. In today's operation one issue runs as
one packet and one delivery, so that mapping is its one current delivery's landing commit
(the work-unit rules (`POLICY.md#work-units-review-and-integration` in the CRW repository)). Under the push-only procedure
that commit is the integrator's fast-forward of the verified merged tree; where the delivery is a pull request,
read GitHub's current PR identity, repository, base branch, merged state and landing commit instead. A
related/reference PR, superseded replacement, approval, green CI, merge-ready flag or closed-but-unmerged PR is
not that evidence. Landing a prerequisite branch into another task branch is not integration into the intended
target. Verify the landing rather than treating an accepted integration request as done.

The delivery must carry the issue's accepted implementation scope; a partial landing
cannot hide remaining required implementation. For an already-approved legacy
multi-PR issue, preserve links, owners and history, inventory required deliveries
and reconcile through `crw-plan` before new dispatch. Its completion uses all
reconciled required PRs actually integrated into the intended target and their
combined coverage of that issue's accepted criteria, not a demand that one PR
cover everything. Assess a PR's contribution to each linked issue independently;
a shared PR cannot complete another issue's remaining scope. Do not mark a legacy
issue complete on its first partial merge or retroactively manufacture completed issues.

Release, deployment, installation and live behavior are separate claims. New
plans track those operational results separately from the implementation PR.
A child's delivery, its parent's acceptance, the merge into the intended target, installation,
observed live behavior, the milestone's completion and the repository's completion are seven
separate claims in the same way: one child's finished goal, or a few milestones closed, does
not establish the level above it.
For an existing issue whose accepted criteria already require installation or
live verification, preserve those obligations until fulfilled or explicitly
re-scoped within authorization; a merge alone does not erase them. Operational
work that is genuinely outside the issue's criteria does not delay its Done.
Any accepted non-PR work, including research, design, verification or operations,
completes on its agreed observable result.
Completion evidence does not supply merge, closure, release or deployment authority.

A closing keyword in a commit or pull request, or a repository automation, can close an issue on its own.
It may not establish the intended branch or entire scope, so check the landing evidence even
when the issue already shows closed. Distinguish closing/delivery links from contributing/reference links and
retain one current delivery PR when replacing a PR. Record any conflict and correct the owned issue
only within the assignment. Do not test by changing unrelated live issues or silently
change repository automation. Closed as not planned is not Done.

## Tracking holds canonical documents

Jun keeps product intent, specifications, plans, accepted decisions, and human-readable coordination records in the tracking repository. The milestone description owns the goal and end condition of the work; the issue bodies and comments hold supporting detail. Use stable refs and URLs with the revision evidence the API gives (`updated_at`). Repositories remain authoritative for source, executable configuration, repository policy, and reproducible implementation evidence.

An issue state, assistant proposal, or newer local draft does not silently supersede an accepted document. The user's latest explicit correction can supersede it; preserve that decision source and mark the canonical document stale until updated within authorization. Report conflicts with repository contracts rather than silently rewriting either source.

Local artifacts are drafts, snapshots, or private raw evidence with a link back to the tracking repository, not a competing permanent document source. Preserve unique content and history. Do not bulk migrate/delete repository documents merely to establish this convention. If an audit is read-only, return a proposed update; do not write it automatically.

One fact has one writable canonical location. Every other place that states it carries a summary and a link to that location and is never read as an independent norm, so a passage that disagrees with its canon is out of date rather than a second rule. Three words classify what a document, or a passage inside one, is claiming: `current` is the version in force, `proposal` is text nobody has accepted yet, and `superseded` is a version that once held and is kept for its history. Recency does not decide between them, and an old implementation record never becomes a new product's `current` contract by being the most detailed document anyone can find.

### Where each document type is canonical

Each row fixes one document type's canonical location, the level whose record it is, the scope that location covers, what makes it change, and how the other side refers to it. Owner names who is answerable for keeping that location current, not a separate grant: write authority still comes from the assignment under [scope of each level](hierarchy.md#scope-of-each-level), where a standalone issue keeps its own existing owner and no parent is invented for it. This table creates no second ownership rule.

| Document type | Canonical location | Owner | Applies to | Change trigger | Referenced from the other side as |
| --- | --- | --- | --- | --- | --- |
| Milestone definition: goal, end condition, scope | The milestone description, written to the [milestone description standard](#milestone-description-standard) | The parent (repository session) that opened it, otherwise the user | The milestone and every issue in it | The goal, end condition or scope changes | Its ref and URL |
| An issue's own requirements, scope, priority, acceptance criteria and open questions | The issue body and its labels | The child, for the milestone record and the issues it owns; a standalone issue keeps its own existing owner | That issue, at the revision read | An accepted requirement, scope or criteria change | The issue ref, beside the expected behaviour and criteria stated in the commit or handoff that carries the change, because a private link alone does not carry them |
| Accepted decisions and dispositions | Comments on the issue the decision binds | The level whose record that issue is | The issue and anything citing it | A decision is accepted, or a later explicit correction supersedes it | The comment cited by URL and date from the commit, pull request or repository document it settles |
| Progress and status | The assignee and the latest comments of the issue; the milestone's open and closed counts | The level whose record it is | That subject's reporting period | Material progress, under the record rule in [scope of each level](hierarchy.md#scope-of-each-level) | Not copied into the repository at all |
| Architecture, API, schema and module contracts, and the rationale for the technical decision behind them | The implementing repository, versioned with the code that implements them | The implementation owner of the issue whose delivery carries them | That repository at the commit carrying them | The implemented contract changes | The issue citing path and commit, never a copy of the contract |
| Install, run, test and recovery procedures; development, CI and security rules | The same repository, in its own policy and contribution documents | That repository's maintainer | That repository, at that revision | A command, path, prerequisite or rule changes | A link in the issue, which does not restate them |
| Research and verification verdicts | The tracking repository, in the issue or the linked coordination document | The level that commissioned the work | The question asked, under the conditions recorded | A verdict is reached, or later evidence overturns it | Links to the repository's reproduction and to the raw evidence |
| Raw, large or private evidence | The durable private evidence root recorded under [OPS-5.4](../../crw-run/references/operations.md#ops-54-evidence-location) | The task that produced it | The run that produced it | A new run replaces it; nothing is edited in place | A citation carrying version or digest, date and access scope, never pasted into the tracking or the code repository |

The first two rows are levels rather than rivals. The milestone description carries the work's own scope and end condition, and an issue body carries that issue's own requirements and criteria. Issue detail is not copied upward, and the milestone's definition is not restated downward.

A research or verification result splits three ways rather than living in one place: the verdict and what was decided from it belong to the tracking repository, the code, configuration and parameters that reproduce it belong to the implementing repository, and the raw output belongs to the evidence store. Work with no code target at all keeps its reproduction in the delivered output identity that [Resolve the implementation repository](#resolve-the-implementation-repository) already holds under the non-PR evidence rules, so having no repository costs it no canonical home. Naming its version, its date and who can reach it is what makes that third part usable later, since a path nobody can open is not evidence. Cite a revision where the source exposes one and its `updated_at` where it does not; promising a version the API does not expose produces a reference no one can check.

A project or repository may hold its own explicit rule, and within its own scope that rule wins, because this policy fixes where a type of document lives rather than what a product decides about itself. Read such a rule as narrower rather than competing: a repository's own policy naming its branch, review and release authority is the repository-owned rows above working as intended. Where a project rule genuinely contradicts this one, record the conflict with both sources and route it to [crw-plan](../../crw-plan/SKILL.md) instead of settling it by following whichever document is nearer to hand. Loading this policy migrates nothing on its own.

### Classify an existing document

Before proposing that anything move, write down what exists, one row per document:

| Current location | Content and scope | Canonical location | Disposition (keep / split / migrate / preserve) | Rationale | Conflicts and unverified items |
| --- | --- | --- | --- | --- | --- |

`keep` is for a document already sitting in its canonical location. `split` is for one document mixing types whose canonical locations differ: each part goes to its own, and what stays behind becomes a summary and a link. `migrate` is for a document that belongs elsewhere whole; move it within authorization and leave a link behind. `preserve` is for history and superseded versions: they stay where they are, marked `superseded` with a date and a link to what replaced them, and are never promoted to `current` to fill a gap in a newer product.

The last column is not an overflow for anything awkward. Two binding sources that disagree is a conflict and takes no disposition until someone settles it, and a document nobody could read is unverified with the read that would settle it named. Both words already carry these meanings for [crw-tidy](../../crw-tidy/SKILL.md), which judges records against the rules stated here, so this table defines no second vocabulary. A classification is a reading rather than an authorization: it moves, rewrites and deletes nothing by itself, and a migration still needs a scope that covers it.

### Keep the two sides consistent

When one side changes, the other is checked before the change is called done, and which side moved decides what follows. Two cases look alike in a diff and are not the same thing.

A **requirements change** is the canonical tracking side moving: an accepted change to scope, criteria or a decision. The implementation and the repository documents describing it are now behind, which is the expected consequence rather than a defect. Check the impact by naming the repository contracts, procedures, tests and open task branches the changed requirement reaches. The owner of the changed record raises it, and the implementing issue's owner updates the repository side inside its own assignment, while a sub-task returns the proposal rather than writing it, under [record writes and returned proposals](#record-writes-and-returned-proposals). Where a review is already bound to the old criteria, the registered set is updated before the next claim rather than after it, because a verdict recorded against superseded wording rules on the wrong obligation.

An **implementation mismatch** is the repository side diverging from a requirement nobody changed. It is a defect to route and never a licence to rewrite the requirement to match the code. Check the impact by establishing first whether the behaviour is wrong or only the repository's description of it is, since those have different fixes and different owners. [crw-check](../../crw-check/SKILL.md) carries the finding to the task that owns the implementation; where two currently binding sources require incompatible things, that is a contradiction for [crw-check](../../crw-check/SKILL.md#find-contradictions) rather than a mismatch.

Dated evidence decides which case applies, never which side is easier to edit. Read both sources with their revision or `updated_at` and compare them with the accepted decision that last moved either one. Where no such record can be found, the classification is unverified with the missing record named, and the requirement stands until something supersedes it.

Verification closes either case. Every cross-reference names the stable ref or path and the revision or `updated_at` it was read at, and once either side moves, the other is read again to confirm that the reference still resolves and still says what the citing document claims it says, because a link that resolves is not a link that still agrees. Progress and decision records keep following the rule already stated under [scope of each level](hierarchy.md#scope-of-each-level), where decisions and dispositions are comments and material progress is the latest comment of the issue; this procedure adds no record shape of its own.

### When the implementing repository changes

A repository-owned document's canonical home is the repository that currently implements the contract it describes, resolved through [Resolve the implementation repository](#resolve-the-implementation-repository) at the time it is read, and never the repository it was first written in. When implementation moves, whether by a split, a merge, a rewrite in another language or a successor product, each repository-owned document is resolved again: it migrates with the contract it describes, one whose contract the new repository does not implement stays where it is as `superseded` with its date and a link to its successor, and none of them becomes the new implementation's `current` contract until an issue there accepts it by criteria. Where a split leaves one contract implemented by more than one repository, the document is split along the boundary each repository actually implements; where the contract itself is genuinely shared, the repository that defines and publishes it is canonical for it and the consuming repositories carry a summary and a link. Until a plan names that repository, the document is a conflict with the decision it needs written beside it, never a canon assigned by guess. The tracking rows do not move, because intent belongs to the product rather than to the code that happened to carry it. What changes there is the code repository named in the body, and only of the issues whose edit target actually moves, under the [tracking operating model](#tracking-operating-model) and only inside an authorized migration. An issue already delivered against the old target keeps the repository recording where its work landed; what is repaired on such an issue is the path and commit its citations point at, never the record of where it was delivered.

### Milestone description standard

Write the work's definition in the milestone description, not in a separate design document
by default, and open it with the end condition on its first line. Match a reading density of about a page of Markdown rather than an exact quota:
do not pad a simple goal or remove essential decisions to hit a size. This is
not a length limit for issue criteria or skill source files.

Keep the goal and necessary context, intended use, scope and end condition,
outputs and completion criteria when known, their connections, validation approach, and material open decisions visible. Mark issue
candidates and unknowns explicitly; this format does not authorize creating them.
Put detailed implementation, long examples and supporting research in the relevant
issue or existing references. Preserve accepted choices and their reasons.

When a milestone update is authorized and issues become concrete, update the relevant
description passages instead of appending every issue or
repeating operating rules. An issue-only write does not authorize a milestone
description write; return any needed adjustment as a proposal. Keep each issue's
contribution to the milestone and its handoff to other issues understandable.
The parent (repository session) that opened the milestone is the authorized writer of its
description under the [role contract](hierarchy.md#scope-of-each-level).
Use linked detail when explicitly requested or needed, without replacing the description
or maintaining a second copy of its definition. Respect a requested output format;
preserve existing documents and history, and migrate only within authorization.

## Use the tracking access

Find the tracking repository in the `tracking` section of the CRW configuration file (`crw config show`); `crw config check` reports a missing or unusable section, a missing token and an unreachable repository. The token is `GH_TOKEN`, `GITHUB_TOKEN` or `gh auth token`. Prefer the user's selected account; when several accounts are possible, verify the repository and the authenticated identity before writing, and do not silently switch the account after a failure.

Read with `gh`, scoped to the repository: `gh issue view N --repo OWNER/NAME --json number,title,body,labels,state,stateReason,milestone,url,comments`, `gh issue list --repo OWNER/NAME --milestone TITLE --state all --json number,title,state,labels`, and `gh api repos/OWNER/NAME/milestones/N` for a milestone. `crw flow plan-input --milestone OWNER/NAME#N` reads a milestone and its issues through the tracking adapter without writing. Exhaust pagination before claiming absence (`gh api --paginate`), and resolve duplicate titles by number.

Give the checks of a release the issue as a GitHub issue gives it: `gh issue view N --repo OWNER/NAME --json url,title,body,labels --jq '{id: .url, title: .title, description: .body, labels: [.labels[].name]}'` is the JSON `crw skill issue-ready check` and `crw skill issue-size check` read on stdin.

For authorized writes, preserve unrelated fields. Use `crw publish comment --issue OWNER/NAME#N --header "[layer · session]" --body-file FILE` for an issue or PR comment, with the [comment header](#tracking-operating-model). Label and state operations remain `gh issue edit N --repo OWNER/NAME --add-label LABEL` (it keeps the other labels) and `gh issue close N --repo OWNER/NAME --reason completed` or `--reason "not planned"`. Text writes follow the procedure below. Read back the resulting number, body and labels, and after an ambiguous result query the existing state before repeating a create. An unavailable token or repository permits a local draft or partial audit, not invented tracking facts or a claim that the repository was updated.

### Public text before a write

Every GitHub issue or milestone description, issue or PR comment, PR title and body, and commit message passes `publishpolicy.Scrub`. The default replaces user home, state, work and temporary paths, the local hostname, internal IP addresses, thread/call/run/session identifiers, and email addresses. Ordinary relative source paths, issue references and commit hashes stay intact. Replacement records contain the category and count, never the matched value. A token or secret pattern stops publication before any write; remove the secret from the draft and repeat the preparation. Do not post the diagnostic text returned for a blocked draft.

`crw publish issue`, `crw publish milestone`, `crw publish comment`, `crw flow pr open`, the review summary publisher and the run's generated commits apply this function themselves. Keep their JSON replacement records with the operation's local output. Read back the published text; changed text is the record to inspect.

For edits or another publication path, including migrating existing descriptions, run `crw publish prepare --repo OWNER/NAME --kind KIND --input DRAFT --output SAFE` on each text field. `KIND` is `issue`, `milestone`, `comment`, `pr` or `commit`. Proceed only on exit 0 and use that output file without appending the original draft. For example, `crw publish prepare --repo OWNER/NAME --kind commit --input MESSAGE --output SAFE && git commit -F SAFE` prepares a manual commit message. Prepare a separately supplied title too; a clean body does not make an unfiltered title safe. Preparation writes no remote record and supplies no additional publication authority.

Repository settings live in the CRW configuration's `publication.repositories` map, keyed by `OWNER/NAME` without regard to case. A policy can add `paths`, `hostnames` and `identifiers` as literal lists. Its optional `redact` list chooses among `path`, `hostname`, `ip`, `thread_id`, `call_id`, `run_id`, `session_id` and `email`; omission means all categories, and an explicit empty list disables these privacy replacements. `disabled: true` also disables privacy replacements for a repository that intentionally keeps internal text, such as a private repository. Secret blocking remains active. An unlisted repository uses the defaults. Example:

```json
{
  "publication": {
    "repositories": {
      "OWNER/NAME": {
        "paths": ["/work/private"],
        "hostnames": ["build-box"],
        "identifiers": ["session-local"]
      },
      "OWNER/PRIVATE": {"disabled": true}
    }
  }
}
```
