# Linear model

How Linear holds the work: how goals, projects and issues relate, which document is canonical where, which level writes which record, and when an issue is Done. The levels themselves are in [hierarchy.md](hierarchy.md).

## Linear operating model

Keep goals, product classification, and repository identity separate:

- An initiative describes a goal and its completion condition, not a permanent
  product bucket. A project may have no initiative or contribute to several.
  Preserve its stable ID and issues across those relations; do not clone them
  or sum shared progress as separate output. Verify connector support and read
  back relations before claiming that a requested link exists.
- Create a project when the user requests one, including an accepted proposal
  or a request to create/update a full Linear plan from goal through issues.
  That write request covers the needed hierarchy in its agreed scope; a narrow plan update, large backlog, or multiple
  repositories alone does not authorize extra projects. Consultation and
  draft-only planning do not authorize Linear writes. Reuse existing IDs and meaningful scope. Name the
  result naturally without a fixed product prefix. Do not rename existing
  projects or migrate their relations merely by loading these instructions.
- Product family is a single-choice project label group. Reuse the existing
  workspace values; a common project without one owning product may leave it
  empty and describe its shared scope. Do not split the project automatically
  or treat that empty classification as an error.
- Repository classification belongs on issues, not projects. Use one actual
  edit-target label per implementation issue in the `저장소` issue-label group.
  Name the value after the repository, adding the owner when names collide;
  keep its description to the exact repository URL. Verify label identity,
  group membership, and the resulting assignment. Keep the explicit owner/repo
  or URL in the issue body too. Reference-only and legacy repositories belong
  in context links, not additional execution labels. Non-code work and unresolved
  targets may leave the group empty; resolve a code target before execution.
- Reuse agreed labels. Discuss any additional label scheme when a need arises
  instead of creating it automatically. Preserve unrelated labels; do not copy
  all project labels to issues. Keep descriptions brief. Views, filters, and
  default screens belong to the user and are configured only when requested.

For example, a requested “Complete installation and first launch” outcome
across the core, desktop, and installer repositories is one initiative holding
three projects, one per repository, each with its own single integration target
and its issues carrying that repository's edit-target label. A shared planning
project can omit a product family, and a non-code planning issue can omit a
repository label. A product prefix in a project name is required by none of
these examples.

Do not migrate existing labels merely by loading this model. When migration is
authorized, classify each issue from its accepted scope and delivery evidence,
not by copying its former project's repository labels. Preserve historical
multi-repository exceptions without forcing a false single target; reconcile
them before new execution. Remove only the superseded repository project labels,
preserving product labels, context links, unrelated fields and history.

### Issue-to-PR mapping

Today's operation is one implementation issue per packet, and that packet delivers that one
issue: several deliveries for one issue, or one delivery covering several issues, are reconciled
through `crw-plan` before new dispatch. Split work requiring several deliveries into separate
issues with explicit dependencies, even within one repository. A project has one integration
target, so a multi-repository outcome is one project per repository, held together by their
initiative, with one issue per delivery.
Batches coordinate separate issue/delivery pairs; they do not combine issues into one
delivery. Referencing a related issue is not claiming to deliver or close it. This is today's
operation under the [work-unit rules](../../../../../POLICY.md#work-units-review-and-integration),
not a standing one-to-one rule: several packets for one feature are allowed by policy and
switch on when their support lands.

Keep review fixes on the same issue and delivery. A necessary replacement PR retains
the superseded link and names the one current delivery; it does not create a
second simultaneous delivery for the issue. A new change after that delivery
has landed gets a new issue and delivery, except a defect inside the finished issue's own specification, criteria or promise: that reopens the same issue key ([After the merge](../../crw-run/references/merge-readiness.md#late-review-threads)), and only a defect outside the issue gets a new issue. Research, design, or operational work with
no repository change uses an explicit non-PR result and verification; do not
create an empty PR merely to fit the rule.

When existing work breaks this mapping, reconcile its scope and ownership
through `crw-plan` before new dispatch. Preserve active work, IDs, and history;
do not silently split, close, or reassign live issues. Editing these instructions
does not migrate existing work or alter the relay's runtime contracts.

### Record writes and returned proposals

A Linear write belongs to the task whose own record it is, under
[scope of each level](hierarchy.md#scope-of-each-level), and that holds for every
sub-task an owner opens rather than for a registered child alone. A planning sub-task drafting a
specification, an implementation child, a review or audit helper and an internal helper agent all
reach the same boundary: they read what their assignment allows, and they create, edit and delete
nothing there, no document, no issue field, no comment, no relation and no status. Whether a relay
holds the assignment decides where a receipt goes, not who writes.

What a sub-task returns instead is the document or issue ID, the revision or updated-at it read,
why the change is needed, the smallest change that would do, and links to the evidence and
artifacts behind it. A local draft of the proposed wording is useful and is still a draft: it is
reported as a proposal waiting on its owner, never as applied in Linear, and a file, a branch or a
message holding that text somewhere else is the same draft.

Exposure is not authorization. Linear write tools appearing in a sub-task's profile, a broad
permission profile, and an assignment naming the very document it is about are none of them a
grant, and an owner's authority over its own record does not travel to a sub-task with the work:
authority comes from the assignment, never from the reachable surface. Nor is there a route around
it, since another connector, a script, a CLI, a scheduled job or the sub-task's own helper agent
writing on its behalf is the same write. An explicit later instruction from the user is decided on
its own terms, for the records it names.

The owner that receives a proposal reads the record as it stands now before applying anything. The
proposal names the revision it was built on, so a record that moved since is reconciled against
the evidence rather than overwritten: the parts that still hold are applied, and a part another
actor has already changed or contradicted is raised instead of being reverted. Apply only what
this assignment's own authority covers, preserve unrelated fields, history and other people's
edits under [Linear holds canonical documents](#linear-holds-canonical-documents), and read the
result back before reporting it, as
[Use the available Linear capability](#use-the-available-linear-capability) requires. A write the
assignment already authorizes is simply made, not sent back to the user for an approval already
given.

A write that fails or returns an ambiguous result stays with that owner: read the current state,
record what happened, and retry the write itself. It is never a reason to re-run the sub-task's
completed implementation or verification, which is finished work at a revision the failed write
never touched. Until a readback confirms it, the record is reported as unwritten with its proposal
still open.

### Resolve the implementation repository

Resolve the issue repository label and its explicit GitHub owner/repo or URL
against its accepted scope, current delivery (its pull request when one exists, otherwise its task branch and head) and existing assignment.
Project context links and any remaining legacy project labels do not assign
repositories to its issues. Identify reference-only repositories separately.
If the issue's label or explicit target conflicts with its PR or ownership record, reconcile the conflict
before writes; a label, folder name or convenient checkout does not break the tie.
Ask only when the current evidence cannot settle a material target choice.

Before new execution, verify the actual remote URL, intended integration branch
from repository policy, and full fetched baseline commit. A default branch and a
remote named `origin` are not universal integration targets. Record the selected
remote name and distinguish a contribution fork from its integration repository.
Use the existing workspace-assignment interface after this identity check; do not
create a new checkout allocator or a fake repository/project to fill missing labels.

On resume, recover the responsible task, checkout and issue branch first. Inspect
its worktrees, dirty state, local-only commits, remote identity and recorded
baseline. Fetching current truth does not authorize resetting, rebasing, cleaning,
stashing, moving or recreating that work. Reconcile ancestry and prerequisites
without replacing an existing assignment with a freshly cloned default branch.
For a new task, use a task-owned checkout under the applicable workspace policy.

Work needing PRs in multiple repositories becomes one project per repository, each with its
own single integration target, and separate dependent issue/PR pairs across them; reading or validating another repository alone does not make it a code
target. Common projects and standalone issues follow the same resolution rule.
Research or design with no repository change may explicitly have no code target;
do not require a remote, branch or Git baseline for that non-PR result.
Keep its source baseline and delivered output identity under the non-PR evidence rules.

### Implementation Done

An implementation issue is Done when every accepted criterion maps to a commit that actually landed on the
intended integration target, and a partial implementation never closes it. In today's operation one issue runs as
one packet and one delivery, so that mapping is its one current delivery's landing commit
([the work-unit rules](../../../../../POLICY.md#work-units-review-and-integration)). Under the push-only procedure
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
observed live behavior, the project's completion and the initiative's completion are seven
separate claims in the same way: one child's finished goal, or a few projects marked Done, does
not establish the level above it.
For an existing issue whose accepted criteria already require installation or
live verification, preserve those obligations until fulfilled or explicitly
re-scoped within authorization; a merge alone does not erase them. Operational
work that is genuinely outside the issue's criteria does not delay its Done.
Any accepted non-PR work, including research, design, verification or operations,
completes on its agreed observable result.
Completion evidence does not supply merge, closure, release or deployment authority.

Read the team's current GitHub status automation when reconciling it with this
rule. Distinguish closing/delivery links from contributing/reference links and
retain one current delivery PR when replacing a PR. A generic merge-to-Done rule
may not establish the intended branch or entire scope; check GitHub evidence even
when Linear already says Done. Record any conflict and correct the owned issue
only within the assignment. Do not test by changing unrelated live issues, silently
change workspace automation, or count a settings screenshot as event-delivery proof.
Respect agreed stale-issue cancellation and archival settings; Canceled is not Done.

## Linear holds canonical documents

Jun keeps product intent, specifications, plans, accepted decisions, and human-readable coordination records in Linear. The initiative page body owns its goal definition and connection to contributing projects; linked project/issue documents hold supporting detail. Use stable item/document IDs and URLs with available revision or updated-at evidence. Repositories remain authoritative for source, executable configuration, repository policy, and reproducible implementation evidence.

An issue status, assistant proposal, or newer local draft does not silently supersede an accepted document. The user's latest explicit correction can supersede it; preserve that decision source and mark the canonical document stale until updated within authorization. Report conflicts with repository contracts rather than silently rewriting either source.

Local artifacts are drafts, snapshots, or private raw evidence with a link back to Linear, not a competing permanent document source. Preserve unique content and history. Do not bulk migrate/delete repository documents merely to establish this convention. If an audit is read-only, return a proposed Linear update; do not write it automatically.

One fact has one writable canonical location. Every other place that states it carries a summary and a link to that location and is never read as an independent norm, so a passage that disagrees with its canon is out of date rather than a second rule. Three words classify what a document, or a passage inside one, is claiming: `current` is the version in force, `proposal` is text nobody has accepted yet, and `superseded` is a version that once held and is kept for its history. Recency does not decide between them, and an old implementation record never becomes a new product's `current` contract by being the most detailed document anyone can find.

### Where each document type is canonical

Each row fixes one document type's canonical location, the level whose record it is, the scope that location covers, what makes it change, and how the other side refers to it. Owner names who is answerable for keeping that location current, not a separate grant: write authority still comes from the assignment under [scope of each level](hierarchy.md#scope-of-each-level), where a standalone issue keeps its own existing owner and no parent is invented for it. This table creates no second ownership rule.

| Document type | Canonical location | Owner | Applies to | Change trigger | Referenced from the other side as |
| --- | --- | --- | --- | --- | --- |
| Initiative definition: goal, finish condition, contributing projects' roles | The Linear initiative page body, written to the [initiative body standard](#initiative-body-standard) | The initiative's execution supervisor where one is designated, otherwise the user | The initiative and every project contributing to it | The goal, finish condition or scope changes, or a contributing project's role does | Its stable ID and URL |
| A project's or issue's own requirements, scope, priority, acceptance criteria and open questions | The Linear project or issue body | The parent, for the project record and the issues it owns; a standalone issue keeps its own existing owner | That project or issue, at the revision read | An accepted requirement, scope or criteria change | The item link, beside the expected behaviour and criteria stated in the commit or handoff that carries the change, because a private link alone does not carry them |
| Accepted decisions and dispositions | Linear comments on the item the decision binds | The level whose record that item is | The item and anything citing it | A decision is accepted, or a later explicit correction supersedes it | The comment cited by ID and date from the commit, pull request or repository document it settles |
| Progress and status | Linear updates on the project or initiative | The level whose record it is | That subject's reporting period | Material progress, under the record rule in [scope of each level](hierarchy.md#scope-of-each-level) | Not copied into the repository at all |
| Architecture, API, schema and module contracts, and the rationale for the technical decision behind them | The implementing repository, versioned with the code that implements them | The implementation owner of the issue whose delivery carries them | That repository at the commit carrying them | The implemented contract changes | The Linear issue citing path and commit, never a copy of the contract |
| Install, run, test and recovery procedures; development, CI and security rules | The same repository, in its own policy and contribution documents | That repository's maintainer | That repository, at that revision | A command, path, prerequisite or rule changes | A Linear link, which does not restate them |
| Research and verification verdicts | Linear, in the issue or the linked coordination document | The level that commissioned the work | The question asked, under the conditions recorded | A verdict is reached, or later evidence overturns it | Links to the repository's reproduction and to the raw evidence |
| Raw, large or private evidence | The durable private evidence root recorded under [OPS-5.4](../../crw-run/references/operations.md#ops-54-evidence-location) | The task that produced it | The run that produced it | A new run replaces it; nothing is edited in place | A citation carrying version or digest, date and access scope, never pasted into Linear or the repository |

The first two rows are levels rather than rivals. The initiative body carries the initiative's own scope, finish condition and each contributing project's role as recorded there, and a project or issue body carries that project's or issue's own requirements and criteria. Project detail is not copied upward, and the initiative's definition is not restated downward.

A research or verification result splits three ways rather than living in one place: the verdict and what was decided from it belong to Linear, the code, configuration and parameters that reproduce it belong to the implementing repository, and the raw output belongs to the evidence store. Work with no code target at all keeps its reproduction in the delivered output identity that [Resolve the implementation repository](#resolve-the-implementation-repository) already holds under the non-PR evidence rules, so having no repository costs it no canonical home. Naming its version, its date and who can reach it is what makes that third part usable later, since a path nobody can open is not evidence. Cite a revision where the source exposes one and its updated-at where it does not; promising a version a connector does not expose produces a reference no one can check.

A project or repository may hold its own explicit rule, and within its own scope that rule wins, because this policy fixes where a type of document lives rather than what a product decides about itself. Read such a rule as narrower rather than competing: a repository's own policy naming its branch, review and release authority is the repository-owned rows above working as intended. Where a project rule genuinely contradicts this one, record the conflict with both sources and route it to [crw-plan](../../crw-plan/SKILL.md) instead of settling it by following whichever document is nearer to hand. Loading this policy migrates nothing on its own.

### Classify an existing document

Before proposing that anything move, write down what exists, one row per document:

| Current location | Content and scope | Canonical location | Disposition (keep / split / migrate / preserve) | Rationale | Conflicts and unverified items |
| --- | --- | --- | --- | --- | --- |

`keep` is for a document already sitting in its canonical location. `split` is for one document mixing types whose canonical locations differ: each part goes to its own, and what stays behind becomes a summary and a link. `migrate` is for a document that belongs elsewhere whole; move it within authorization and leave a link behind. `preserve` is for history and superseded versions: they stay where they are, marked `superseded` with a date and a link to what replaced them, and are never promoted to `current` to fill a gap in a newer product.

The last column is not an overflow for anything awkward. Two binding sources that disagree is a conflict and takes no disposition until someone settles it, and a document nobody could read is unverified with the read that would settle it named. Both words already carry these meanings for [crw-tidy](../../crw-tidy/SKILL.md), which judges records against the rules stated here, so this table defines no second vocabulary. A classification is a reading rather than an authorization: it moves, rewrites and deletes nothing by itself, and a migration still needs a scope that covers it.

### Keep the two sides consistent

When one side changes, the other is checked before the change is called done, and which side moved decides what follows. Two cases look alike in a diff and are not the same thing.

A **requirements change** is the canonical Linear side moving: an accepted change to scope, criteria or a decision. The implementation and the repository documents describing it are now behind, which is the expected consequence rather than a defect. Check the impact by naming the repository contracts, procedures, tests and open task branches the changed requirement reaches. The owner of the changed record raises it, and the implementing issue's owner updates the repository side inside its own assignment, while a sub-task returns the proposal rather than writing it, under [record writes and returned proposals](#record-writes-and-returned-proposals). Where a review is already bound to the old criteria, the registered set is updated before the next claim rather than after it, because a verdict recorded against superseded wording rules on the wrong obligation.

An **implementation mismatch** is the repository side diverging from a requirement nobody changed. It is a defect to route and never a licence to rewrite the requirement to match the code. Check the impact by establishing first whether the behaviour is wrong or only the repository's description of it is, since those have different fixes and different owners. [crw-check](../../crw-check/SKILL.md) carries the finding to the task that owns the implementation; where two currently binding sources require incompatible things, that is a contradiction for [crw-logic](../../crw-logic/SKILL.md) rather than a mismatch.

Dated evidence decides which case applies, never which side is easier to edit. Read both sources with their revision or updated-at and compare them with the accepted decision that last moved either one. Where no such record can be found, the classification is unverified with the missing record named, and the requirement stands until something supersedes it.

Verification closes either case. Every cross-reference names the stable ID or path and the revision or updated-at it was read at, and once either side moves, the other is read again to confirm that the reference still resolves and still says what the citing document claims it says, because a link that resolves is not a link that still agrees. Progress and decision records keep following the rule already stated under [scope of each level](hierarchy.md#scope-of-each-level), where decisions and dispositions are comments and material progress is an update; this procedure adds no record shape of its own.

### When the implementing repository changes

A repository-owned document's canonical home is the repository that currently implements the contract it describes, resolved through [Resolve the implementation repository](#resolve-the-implementation-repository) at the time it is read, and never the repository it was first written in. When implementation moves, whether by a split, a merge, a rewrite in another language or a successor product, each repository-owned document is resolved again: it migrates with the contract it describes, one whose contract the new repository does not implement stays where it is as `superseded` with its date and a link to its successor, and none of them becomes the new implementation's `current` contract until an issue there accepts it by criteria. Where a split leaves one contract implemented by more than one repository, the document is split along the boundary each repository actually implements; where the contract itself is genuinely shared, the repository that defines and publishes it is canonical for it and the consuming repositories carry a summary and a link. Until a plan names that repository, the document is a conflict with the decision it needs written beside it, never a canon assigned by guess. The Linear rows do not move, because intent belongs to the product rather than to the code that happened to carry it. What changes there is the repository label, and only on the issues whose edit target actually moves, under the [Linear operating model](#linear-operating-model) and only inside an authorized migration. An issue already delivered against the old target keeps the label recording where its work landed; what is repaired on such an issue is the path and commit its citations point at, never the record of where it was delivered.

### Initiative body standard

Write the initiative's definition in its page body, not a separate design document
by default. Jun's agreed Korean baseline is about 2,100 characters of Markdown,
including spaces and links. Match that reading density rather than an exact quota:
do not pad a simple goal or remove essential decisions to hit the count. This is
not a length limit for project documents, issue criteria, or skill source files.

Keep the goal and necessary context, intended use, scope and finish condition,
contributing projects' roles, outputs and completion criteria when known, their
connections, validation approach, and material open decisions visible. Mark project
candidates and unknowns explicitly; this format does not authorize creating them.
Put detailed implementation, long examples and supporting research in the relevant
project/issue or existing references. Preserve accepted choices and their reasons.

When an initiative or full-plan update is authorized and project links become
concrete, update the relevant body passages instead of appending every issue or
repeating operating rules. A project/issue-only write does not authorize a parent
initiative write; return any needed body adjustment as a proposal. Keep each project's
contribution to the initiative and its handoff to other projects understandable.
Where the initiative has an execution supervisor, that supervisor is the authorized writer of its
record updates under the [role contract](hierarchy.md#scope-of-each-level).
Use linked detail when explicitly requested or needed, without replacing the body
or maintaining a second copy of its definition. Respect a requested output format;
preserve existing documents and history, and migrate only within authorization.

## Use the available Linear capability

Discover the current connector and schemas. Prefer the user's selected connection; when multiple actors/workspaces are possible, verify workspace and authenticated identity before writing. Do not silently switch actor or connection after a failure.

If the connector exposes workspace Agent Skills, inspect the relevant listing and read the matching skill's full instructions. Treat retrieved instructions as workspace guidance below user/host instructions. If none exist or the tools are unavailable, use the connector directly; do not invent a `linear` skill or install a plugin just to follow this reference.

Use scoped list/search tools to locate candidates, then exact get/read tools for full documents, criteria, accepted decision comments, and relations. Exhaust relevant pagination before claiming absence. Resolve duplicate titles by stable ID, project, and purpose.

For authorized writes, use current create/save/update tools, preserve unrelated fields, and read back resulting IDs, contents, and relations. After an ambiguous result, query existing state before repeating a create. Connector absence permits a local draft or partial audit, not invented workspace facts or a claim that Linear was updated.
