---
name: crw-plan
description: "Decompose an agreed goal into Linear projects, milestones and issues, each issue one concept bundle. Use for product planning, a roadmap or 기획, and scoped plan updates. Settling an unclear milestone before registration is crw-define's, dispatch crw-run's, drift crw-check's."
---

# CRW Plan

Build a usable product plan from what exists and what the user wants next. Linear holds the canonical product, planning, and decision documents; repositories hold implementation and reproducible evidence, under [Linear holds canonical documents](../crw-canon/references/linear-model.md#linear-holds-canonical-documents).

Use an agreed initiative definition as input, then cover its requested scope through executable issues in one operation: projects → useful milestones → issues and dependencies. If a milestone or its issues are still unclear, settle them with [crw-define](../crw-define/SKILL.md) first; a request for both definition and planning chains the two without another invocation. An existing project or standalone issue can supply the agreed goal without an initiative. Reuse existing levels and create missing items within that request without requiring a separate invocation at each level. A scoped project/issue update stays scoped; do not invent an initiative or extra projects just to fill the hierarchy. Consultation, plan-only, draft-only, and read-only requests do not authorize Linear writes; invoking this skill implicitly does not supply write intent.

## Connect the workflow

Read the shared [Linear model](../crw-canon/references/linear-model.md) for Linear access and document authority, [delivery](../crw-canon/references/delivery.md) for implementation-workflow ownership, and [Integrations](references/integrations.md) for resolving the project target and the child pair. Load relevant installed skills and use their outputs in the plan; naming a skill is not using it.

- Use `crw-recall` to recover missing decisions, and brief the product's current state from refreshed sources when it is unclear. Verify old state against current sources.
- Restate a bundled request to resolve it before splitting it. Ask only about a material fork the evidence cannot settle.
- Use `crw-dev` to assess development scope, repository boundaries, and meaningful verification. Planning does not start `crw-loop` or execution tasks.
- If roadmap facts conflict or are duplicated, settle them by [where each document type is canonical](../crw-canon/references/linear-model.md#where-each-document-type-is-canonical). Check that success criteria cannot reward their own assumptions, and rewrite revised descriptions so they read as the current plan.
- Hand executable work to [crw-run](../crw-run/SKILL.md). Use [crw-check](../crw-check/SKILL.md) for intent-versus-implementation uncertainty and for contradictions within the proposed plan.
- Keep Linear writes with this operation. A delegated sub-task returns the record ID, the revision it read, the reason, the smallest sufficient change and its evidence, under [record writes and returned proposals](../crw-canon/references/linear-model.md#record-writes-and-returned-proposals).

## Establish the baseline

Read the user's goal, existing Linear initiative/project/milestones/issues, full acceptance criteria, dependencies, canonical documents, and relevant accepted decisions. Inspect repository guidance, branch/worktrees, dirty changes, and relevant code/PR evidence. Paginate scoped results before concluding an item is absent.

Distinguish proposed, implemented, reviewed, merged, deployed, and behaviorally verified work. A Done label or local commit cannot establish every later state. If evidence disagrees, preserve the source links and record the disagreement.

Identify the goal, product classification, and finishable outcome separately using the shared [Linear operating model](../crw-canon/references/linear-model.md#linear-operating-model). Reuse existing initiative/project IDs and team conventions. Do not infer product identity from a checkout folder name or merge distinct products because their repositories are related.

## Explain the design

When the request is to understand or review a system rather than to plan one, what a component is for, whether an existing owner can be left where it is, how the parts fit, or whether a proposed change actually helps, follow [Design explanation](../crw-canon/references/design-explanation.md). It fixes what the answer must carry: the visible result first, then which process runs and which store keeps, what actually refuses, how far each claim is built with the evidence that earned the label, and how an improvement would be judged before anyone measures it. The same standard applies to design reasoning written into a plan for a later reader.

Two rules hold without opening it. A registered hook, a line in a prompt and a merged source edit establish no enforcement, no installation and no live behaviour; each of those is a separate fact needing its own evidence. And an explanation-only request executes nothing, while a Linear reflection the request or its existing scope already covers is applied, read back, and not sent for the same approval again.

This standard governs how an explanation is delivered. Checking delivered work against what was agreed stays with [crw-check](../crw-check/SKILL.md), which owns that comparison whether or not an explanation is what produced the question.

## Shape the plan

- **Initiative input:** the agreed goal, finish condition, scope and open decisions from an existing accepted definition, and a milestone's definition from `crw-define`. Preserve its identity. When the request covers initiative or full-plan updates, link contributing projects and align the page body using the shared [initiative body standard](../crw-canon/references/linear-model.md#initiative-body-standard): explain each project's contribution and handoff, leaving issue-level detail below. A project/issue-only update reads the parent as context and proposes any needed parent change without writing it. Return material goal changes to definition within the request's scope.
- **Project:** a finishable outcome created when the user requests a project. Name the result naturally; do not require a product prefix. A product name can appear when it helps explain the result.
- **Milestone:** an observable result or coherent delivery boundary. Follow explicit user grouping, such as one milestone per character or module.
- **Issue:** one coherent unit of work that one merge finishes, stating the problem or user-visible outcome, the deliverable that will exist when it is finished (each separately deliverable output as its own item under a Deliverables heading beside the criteria), its PR target repository, the scope and what it deliberately leaves out, acceptance criteria, canonical document links, prerequisite issues, one child pair line (its pair family and axis, [chosen by issue type](references/integrations.md#child-pair-by-issue-type), and the issue's [tags, bundle and source](references/integrations.md#the-extended-line) where the plan classifies it), and meaningful verification. An issue handed to `crw-run` also passes the shared [release gate](../crw-canon/references/release-gate.md#the-release-gate). A design question the plan cannot answer stays under the issue's open decisions with the `설계 먼저` label, and the issue is not released until the design result is in its decided answer. Apply the shared [issue-to-PR rule](../crw-canon/references/linear-model.md#issue-to-pr-mapping), including its non-PR work exception; a non-PR issue names its agreed result where the PR target would be. The work-unit rule itself is the repository's work-unit section (`POLICY.md#work-units-review-and-integration` in the CRW repository), not restated here.

An issue body names no model and no effort: the role policy (`crw role helper`, read with `crw flow roles`) alone decides them, and the child pair line names a pair family of that policy, not a model. When the done condition searches the whole tree (an `rg` or `grep` over the repository, or a test over every package), run it once when the issue is written. Compare the paths it matches with `## 편집 영역`; a path outside it, or a listed region it does not reach, is a warning in the report and a line recorded in the issue, and its owner corrects the edit regions or the done condition. The issue is still written: the check warns and records, and refuses nothing.

Use the smallest structure within the requested scope. An authorized full-plan write includes the projects needed for that agreed outcome; issue count, repository count, or estimated size alone does not authorize extra projects. Planning or updating issues can reuse a project or keep standalone issues without creating missing upper levels. A project may have no initiative or contribute to several; reuse its ID instead of duplicating the project or its issues. Execution ownership for a shared project follows the shared [scope of each level](../crw-canon/references/hierarchy.md#scope-of-each-level): one [child](../crw-canon/references/hierarchy.md#the-levels) (project session, relay role `parent`) per project, ordered by its repository's parent, with the other initiatives referencing its outcome. Do not invent dates, owners, status transitions, or a team per product. Record unknowns plainly.

Apply product-family labels to projects and actual edit-target repository labels to issues under the shared operating model. Reuse existing labels and keep descriptions short. Additional label schemes need discussion with the user; leave views and default screens to the user unless requested.

Carry the definition’s accepted direction and unresolved decisions into the plan. Develop project-specific design detail when needed; Strategy/Scope/Structure/Skeleton/Surface are document lenses, never required initiative/project/milestone/issue levels.

Split by the concept boundary and independent verifiability, and decide each boundary with [issue boundaries](references/issue-boundaries.md). An implementation issue is the concept bundle one merge finishes: work that belongs together because it is one concept, not because it is a similar size. Bundle when the parts are the same fault class or contract fixed in several files, collect in one package (or packages sharing a test fixture), are follow-ups of one merged pull request, have no independent value alone and continue in order, or are one package's test reinforcement. Do not bundle an open decision or a decision document with its implementation, work unrelated to a security fix (the fix stays small and fast, while a caller update, migration, configuration or test it needs to pass verification stays with it), another project's exclusive area, or a part whose blocking would block the merge of the rest. A second issue still exists because a second merge must happen: a different target repository, a part that must already be landed before the rest can be written or verified, or a part already delivered or owned by a live issue. Parts that cannot pass verification apart land together, and a direct defect found in a pull request that is about that pull request's own change (a test gap in it, a document or pointer that contradicts it) is fixed in that pull request rather than split off. Issue count, line count and the wish to parallelize never split an issue; a line count is only a rough reference. The [size check](references/issue-boundaries.md#check-the-size-of-an-issue) runs on every issue this plan writes or refines and is advisory: it shows the estimate and the concept-boundary questions, and the boundary rules above decide. Derive order from dependency edges and overlapping edit surfaces. A schema/API contract may precede several apparently independent issues. Avoid dependency cycles and distinguish speculative backlog ideas from approved requirements.

Today one issue runs as one packet and one delivery, so an outcome needing several deliveries is planned as a separate implementation issue for each, with prerequisites connected ([issue-to-PR mapping](../crw-canon/references/linear-model.md#issue-to-pr-mapping)). Put the combined outcome in the project or milestone. A project has one integration target, so work in several repositories is one project per repository, with the initiative holding the projects and their prerequisites between them; code changes needing a PR in each repository require separate issues, each in the project of its repository. Reading a dependency repository alone does not require another issue. A request to organize work by PR, however short, applies that boundary rule and the closing check in full to its requested scope; a milestone-only or draft-only request applies them within that scope and writes nothing outside it.

Make criteria observable: user behavior, data/state that must survive, and important failure cases. Do not weaken criteria to match code already written. Reuse completed work as evidence or a prerequisite instead of reopening it as a new implementation task.

Before handing off code issues, apply [repository resolution](../crw-canon/references/linear-model.md#resolve-the-implementation-repository):
name each issue's actual owner/repo and separate reference repositories. Project
labels do not substitute for that target. Leave a genuinely unresolved code target
as an explicit planning dependency, not an invented checkout assignment.

## Size rules

Size is judged by concept; the numbers say when to look again.

- A project is what one child finishes. A project has one integration target; work in several repositories is one project per repository, each project with its own single integration target and an initiative holding the projects, and an issue never carries a target of its own. The plan the child runs holds at most 64 live issues: `MaxNodes` (`internal/relay/dag/model.go`) bounds the live nodes of one plan revision, and a retired node leaves that count, so issues the plan has retired do not count against it. Of a Linear project's issues, 4-20 is recommended, and that recommendation is advice, not the runtime bound. Split a project when its edit-region map divides into groups joined only by named artifacts; keep heavily overlapping regions in one project.
- A milestone is one observable result, at most 8 issues recommended; a milestone over 12 issues is split.
- An issue is a concept bundle, decided with [issue boundaries](references/issue-boundaries.md); the wish to parallelize never splits an issue.
- For every issue, run `crw skill issue-size check` and record its answer (`ok`, `over_line` or `over_line_accepted`) in the issue as its `issue_size`, beside the `size_fit` and `size_reason` it sets ([Set the size fields](#set-the-size-fields)), with the exception record the report returns when there is one. Answer the report's concept questions and judge by them: no count alone splits an issue, keeps one together or sends it to a child.
- The issue line carries no model name and no effort of its own: its child pair line names only a pair family, and the declared policy supplies the model and effort when the child is created.

These sizes belong to the levels in [hierarchy.md](../crw-canon/references/hierarchy.md#sizes-at-each-level). `crw-add-issue` tells its caller when the issue it adds would pass the project or milestone limit.

## Set the size fields

Each implementation issue carries these fields beside its criteria. The handoff to the run reads them, and `crw flow compile` refuses a value outside its set.

| Field | Closed set | What it says |
|---|---|---|
| `size_fit` | `executor`, `child`, `split` | where the run takes the issue: an executor call of the run, an issue child that only coordinates an issue-lane run, or an issue that is not yet one issue and is divided first |
| `size_reason` | `regions_gt3`, `packages_contract_gt2`, `steps_gt5`, `verify_gt20m`, `own_goal`, `fits_executor` | the one reason for the `size_fit`: more than 3 edit regions, more than 2 packages whose contract changes, more than 5 ordered steps, verification over 20 minutes, a goal of its own, or work that fits one executor |
| `issue_size` | `ok`, `over_line`, `over_line_accepted` | the answer of `crw skill issue-size check`, recorded as the report gave it |
| `group` | a name, at most 4 issues | issues whose edit regions overlap and that run in the order the handoff declares them, each on its own executor |

1. Run `crw skill issue-size check` on the issue and record the answer as `issue_size`, with the exception record when the report returns one.
2. `ok` gives `size_fit` `executor` and `size_reason` `fits_executor`. `over_line` is answered by the report's concept questions: either divide the issue along the report's proposal under the second-merge rules and check each new issue, or keep the bundle with a bundle reason, which makes the answer `over_line_accepted`. A kept bundle, and any other issue, gets `size_fit` `child` only with a `size_reason` of the set other than `fits_executor`; without one it is `executor` with `fits_executor`. `split` marks an issue that is divided through [crw-add-issue](../crw-add-issue/SKILL.md) and a plan revision before its node is released; a node that reaches a run still `split` escalates as `size_split`.
3. Run `crw skill size-fit` with the report and the counts the body states (edit regions, packages whose contract changes, ordered steps, minutes of verification) and the own-goal flag. Its proposal is advice: it shows the issue-size answer and the exception record beside the counts and names the first reason of the set whose count crosses its line. Read it as a question put to the planner. Never divide an issue, or send it to a child, because a number crossed its line: the concept boundary decides whether an issue splits, and a child is for an issue that needs a coordinator of its own. Write the count that crossed beside the reason in the issue body.
4. Name a `group` for at most four issues that overlap in their edit regions and must run in order; the handoff declares the order, and compile chains the members. Issues that overlap with nothing ordering them are refused (`region_order_missing`), so the order is a prerequisite or a group, never a note.

The architect of a milestone reads these fields as the starting values of its DESIGN and may set `child` for a node with a `size_reason` of the set. The issue line carries no model name and no effort.

## Write the Orchestration section of the project

The body of the Linear project carries an `## Orchestration` section, written with the plan and refreshed with it, that the child and its run read before the first milestone starts:

- **Parallel width:** how many issues the plan lets run at once, and why (the number of groups of issues whose edit regions are disjoint).
- **Region map:** for each group the edit regions its issues hold, the groups joined only by a named artifact, and the order that settles each pair of issues sharing a region.
- **Integration target:** the repository and the base branch the project's issues are integrated into, one for the project.
- **Expected review rounds:** how many review rounds an ordinary issue should take, and which issues are expected to need more, a high-risk (C4) issue first among them.

The section states facts of the plan. It names no model and no effort, which the declared policy supplies.

## Hand the plan to the run

The handoff the child takes to the run is one JSON document, built from the issues as they were read back. The compiler reads it strictly: an unknown key is refused and `schema` is required, so write exactly these keys.

- Top level: `schema` (always `crw-flow-compile-input/1`), `projects`, and optionally `initiative` and `author_task_id`.
- Each project: `key`, the binding scope key: the project's UUID, as the scope key of the orchestrator's parent binding holds it, not the Linear identifier (`P-…`), because the run claims the plan under that binding and another key is refused at its first claim (`crw flow start` warns before it); optionally `plan_id` (the key when absent); `target` (an object with `repository`, the absolute path of the local checkout the run integrates into, the same as its `--repo`, and `base_ref`, the short branch name such as `dev`; compile warns on a repository that is not an absolute path and removes a `refs/heads/` prefix with a warning; an issue ordered before another needs it); `issues`; and `milestones`.
- Each issue: `key`, `title`, `criteria` (a list of strings), `milestone` (the key of a milestone entry), `regions` (the issue's edit regions as paths; a trailing slash makes a tree, otherwise the path is a file), `after` (prerequisite issue keys), `group`, `size_fit`, `size_reason`, `issue_size` and, where there are one, `bundle_reason` and `exception_record`.
- Each milestone entry: `key`, `title`, `criteria`, `surfaces` (a subset of `logic`, `web`, `tui`, `desktop`), `grade` (`C1` to `C5`) and `required_commands` (empty means the repository's CI). When any entry is given, every milestone an issue names has one and every entry is named by an issue.

`crw flow compile --dry-run FILE` checks the document as the last step of the [closing check](references/issue-boundaries.md#close-the-plan), and `crw flow compile --help` repeats the rules. The child puts the compiled revision into the plan and starts the run of a milestone from the same file ([crw-run](../crw-run/SKILL.md#run-a-milestone-with-a-flow-run)).

## Before a flow run starts

Creating the project is not yet what a run needs. A milestone runs as a flow run only when three things exist, and the plan that creates the project says so to the child that will run it:

1. the handoff above, read back from Linear, with the project's `milestones` and every issue naming its milestone;
2. the plan revision compiled from that handoff with `crw flow compile` (`--dry-run` first, then without it) and put into the relay with `dag-plan-put`;
3. `crw flow start` for the milestone, as `crw flow start project-lane --handoff FILE` naming that same handoff file, with the plan and the milestone in `--args`.

A start without the handoff is refused: its preview says `blocked`, because the project lane reads its milestone from the compiled handoff, and a handoff that does not name the plan or the milestone is refused the same way. A project without `milestones` therefore has no flow run until the plan is revised.

## Schedule the plan

An authorized project or initiative plan write carries its schedule as part of the default result: the project's start and target dates, milestones named after the result they finish with their target dates, each issue's milestone link and the target date it actually needs, the prerequisites that really hold, and the areas that can proceed in parallel. Use the field names, sources and value words in the shared [Schedule baseline contract](../crw-canon/references/schedule-contract.md#schedule-baseline-contract), and follow [Schedule and dependency roadmap](references/scheduling.md) to build, check and record them.

Dates come from an authority, from delivery evidence, or from a record that already exists, never from an estimate: no working day, working week, sprint, default period, per-issue day count or model-produced remaining time supplies one. Where the request states none, read the target from the most recent records of the same class of work at the same stage, place independent subjects together in the earliest window their prerequisites and the stated concurrency allow, mark it provisional, and name whose decision set it; a class with no comparable record gets that same earliest target with its evidence gap named instead of a padded date. A subject stays awaiting authority when its date needs a decision nobody has made, and undetermined when its prerequisite carries no date at all. Preserve each real start and finish from evidence with its timezone, and leave completed or canceled work out of the new schedule.

Keep the schedule baseline and each change's time, reason, scope and before and after dates in the records Linear already holds, so a later plan stays comparable with the first. At a checkpoint an earlier result pulls its successors in during that same turn, an unfinished subject moves no date on its own, and a later target names the scope change, the real blocker or the predecessor's own recorded move behind it while both baselines stay readable. Use a project-level prerequisite only when the whole project must finish, and express a wait for particular outputs at issue or milestone level so the remaining work proceeds. Schedule writes happen under the authority in Reconcile and apply below; a check-only or draft-only request returns the schedule as a proposal, and registering a date is not authority to assign, execute, install or restart anything.

## Reconcile and apply

For an existing plan, compute a compact change set: reuse, create, update, or leave unresolved. Match stable IDs and semantic scope before titles. Re-running the same request converges on the same items because the split follows deliverables derived from accepted criteria rather than a file layout that moves with every commit.

A request to create, update, or apply the full plan in Linear authorizes its scoped hierarchy and document/item writes, including needed projects; an explicit project request or already accepted proposal also authorizes that project. Resolve write intent from the full conversation and existing authorization, not from the skill name or hierarchy depth. An advisory planning request remains a proposal until an apply/create/update request covers it. A narrower update does not authorize unrelated new projects. Prepare concrete changes, apply them within scope, and read back the resulting documents, items, and relations. Do not add a second approval step for routine authorized writes. A draft-only request stays a draft. Deletion, archival, issue closure, messages to others, or a material change to agreed scope need authorization covering that action.

Preserve unrelated content, labels, history, and human edits. Refresh before updating if another actor may have changed an item. After an uncertain write, look up the existing result before retrying; report partial completion with actual IDs. If a project was created but its labels or relations failed, repair those fields on that ID. Do not repeat the project create. Resolve same-name candidates by IDs and semantic scope; ask only if the supplied target and current binding cannot distinguish them.

Before reassigning any remaining scope of an issue that already has a PR, align its boundary under [delivered issues](references/issue-boundaries.md#align-a-delivered-issue): compare the scope agreed at assignment, the criteria still open and the current owner, then either keep the remainder in that same unmerged PR or move it to a new issue with the merged one as its prerequisite. Remaining scope that stays inside the issue's own accepted criteria reopens the same issue key instead of moving to a new issue ([After the merge](../crw-run/references/merge-readiness.md#late-review-threads)). An instrumentation or reference PR that satisfies no accepted criterion is not a delivery, a live assignment is never narrowed before its owner has agreed the new boundary, and leaving the issue open with its boundary undecided is not alignment. Narrowing a delivered issue is carried by the authorization the request already gave; it is not a second approval step.

Link issues to the canonical Linear item body or supporting document instead of copying the whole specification into every issue. Include the acceptance criteria needed to act. Local drafts remain explicitly unsynced until the Linear write is verified; do not create a parallel permanent planning source. A write that changes an accepted requirement runs the requirements-change case in [Keep the two sides consistent](../crw-canon/references/linear-model.md#keep-the-two-sides-consistent), so the repository contracts, procedures and open reviews it reaches are named with it.

## Deliver

Return Linear document/item links, meaningful changes, unresolved decisions, and the next ready issue or batch with prerequisites. Close the plan in the order the [closing check](references/issue-boundaries.md#close-the-plan) sets: every promised outcome maps to an issue criterion, no two issues claim one deliverable, every stated prerequisite is a real blocking relation with no cycle and one ordering per shared surface, each written item and relation is read back and reported as observed rather than as sent, and the handoff built from what was read back compiles with `crw flow compile --dry-run`.

For a full plan, do not stop at an initiative, project outline, or the first batch. Cover the entire agreed scope with issue-level criteria, repository/delivery boundaries for implementation, dependencies, and verification. When discovery is necessary, define its question, output, and which later work it blocks instead of inventing implementation details. Report any unplanned scope or incomplete writes explicitly; an outline is not a completed plan. Creating the plan does not execute its issues, and aligning a canonical document, its milestones and its issues in one authorized pass is still planning: it starts no task, opens no branch or pull request, and approves no merge.

Provide a standalone handoff to `crw-run` with source IDs and embedded criteria when worker connector access is unproven. Do not launch work unless execution was requested.
