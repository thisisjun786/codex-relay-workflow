# Issue boundaries and plan completeness

Where one implementation issue ends, what it must carry, and how to tell that the split
is finished. The mapping itself belongs to the shared
[issue-to-PR rule](integrations.md#issue-to-pr-mapping); this file is the judgment
`crw-plan` applies while writing a plan, and it cites that rule instead of restating it.
Read it with the planning rules in [crw-plan](../SKILL.md), which the cases below assume.

This is the default output standard for every implementation issue this skill plans. A
short request, "PR 단위로 정리해줘" and nothing more, gets it in full. A narrower request,
one milestone or a draft, gets the same standard applied to whatever it actually writes,
and no widening of its scope.

## Decide the boundary

The unit is the merge, not the diff. One issue is the work that one merge into one
integration target finishes, and its criteria are the ones that merge can satisfy.

A second issue and delivery is forced when any of these holds, because each is a different merge:

- the change lands in a different target repository, which is a different merge; a
  repository that is only read, validated or depended on is not a target
  ([repository resolution](integrations.md#resolve-the-implementation-repository));
- some part must already be landed before the rest can be written or verified — a
  schema, an API or message shape, a published artifact, an installed package, a
  migrated store — so that part becomes a prerequisite issue and the rest depends on it;
- part of the outcome is already delivered, or is owned by a live issue, so it is a
  prerequisite or evidence rather than new work;

A part that could merge and be verified on its own while the rest stays unfinished, and whose
rest still has an observable criterion of its own, **may** be split off; being verifiable apart
permits the split, it does not require one. The concept decides first: two parts of one concept
stay in one issue even where each could be verified apart.

Criteria that no merge establishes, such as installation, deployment, live behaviour or
an external decision, are tracked as their own result under
[Implementation Done](integrations.md#implementation-done) rather than riding along in a
delivery PR's criteria.

Splitting is forbidden when the parts cannot pass verification apart: a change and the
caller update, migration, configuration or test that keeps the target green land in the
same merge. Where the target repository has no required checks, read this as reviewable
apart. It is equally forbidden when the second part would have no observable result of
its own; an issue whose only criterion is "the rest" names no deliverable.

State the contrast once, because the two rules meet here. If the earlier part passes its
checks alone and the later part passes only after the earlier one lands, split and record
the relation. If neither part passes alone, they land together.

The unit is the concept bundle, and the concept decides before the size does. Bundle parts
that belong to one concept:

- the same fault class or the same contract fixed in several files;
- work that collects in one package, or in packages that share a test fixture;
- follow-ups from one merged pull request (review or pair-evaluation findings);
- pieces that have no independent value alone and continue in order;
- one package's test reinforcement or parallelization.

Do not bundle:

- an open decision, or a decision document mixed with its implementation;
- work unrelated to a security fix attached to that fix (a security fix stays small and fast; a
  caller update, migration, configuration or test it needs to pass verification stays with it);
- a part that reaches another project's exclusive area;
- a part whose blocking would block the merge of the rest.

Nothing else splits an issue: not file count, diff size, directory or module boundaries,
equal sizing between issues, the number of review rounds, or a wish to run work in
parallel. A line count is only a rough reference, never a boundary; the [work-unit
rules](../../../../../POLICY.md#work-units-review-and-integration) own that rule and this page
applies it. Real parallelism comes
from the independence described above. If the only thing separating two candidate parts is
that they touch different files, they are one delivery. A large single-landing change is one
issue, and three one-line changes in three repositories are three.

Where the two rules seem to point opposite ways, the concept decides. The independent-verifiability
rule above forces a second issue only across a concept boundary: two parts of one concept stay in one
issue even where each could be verified apart, and the fact that each could pass its own checks is not
on its own a reason to split them. The forced-split list and the bundle list are read together, with
the bundle list deciding whether the parts are one concept at all.

A direct defect found in a pull request that is about that pull request's own change, a test
gap in it or a document or pointer that contradicts it, is fixed in that pull request rather
than split off. Only a finding about a different concept becomes a separate issue.

Sibling issues that each add files to one Go package share a namespace the diff never shows: every
pull request builds on its own base, git merges the files without a conflict, and two declarations
of one identifier break only the merge. So when a split gives two or more issues new files in one
package, each of those issues states the top-level names already used in that package (read from the
current tree, not from memory) and a naming prefix of its own for what it adds, and each new
identifier is named in one issue's criteria only: a type or helper that two issues both ask for is
either a prerequisite issue the others depend on or part of one issue. The parent builds the merge
before each verdict to catch what still slips through ([build and vet the merged
tree](../../crw-run/references/merge-readiness.md#build-and-vet-the-merged-tree-before-the-verdict)).

## Check the size of an issue

Run `crw skill issue-size check` on each implementation issue this skill writes or refines, giving it the issue as
Linear returns it (`id`, `title`, `description`) and, where the plan knows them, `deliverables` (the separately
deliverable outputs, one item each: a new package, a command family, a store table, a skill document) and `depends_on`
(which criterion needs which earlier one; the planner writes it and the command never derives it). The command counts what
the body states under the headings it knows (completion criteria, research reinforcement, deliverables, verification, scope, in Korean or
English; the report names the headings it left unread, so write the criteria as list items or pipe-table rows under one of them, not under a bold label) and answers `ok`, `over_line` or `over_line_accepted` with its reasons. It is a count against limits read from recorded
delivery, not a judgment of the work: the same issue gets the same answer, and the report prints the limits it applied, so
this page states none. It also prints the concept-boundary questions and, when the body states one, the scaled line estimate
with the CRW-739 ratio correction. Write an issue's separately deliverable outputs under a Deliverables heading at the same level as its
other sections, so the count has something to read; an issue that declares none is reported as not measured, which is not zero.

The answer is advisory: it never blocks a dispatch and never exits non-zero on the count alone, and the concept boundary above
decides, not the count. `ok` changes nothing. `over_line` says the issue is over a baseline, and `over_line_accepted` says
the same with a `--bundle-reason` recorded in the report, so the parent records why it accepted a bundle over the baseline.
Read the draft as a guess at where the criteria fall apart and apply the boundary rules to each bundle:
it becomes an issue only with an observable result of its own, verifiable apart or landing after a prerequisite, and the
closing check records the order edges. A bundle that fails those rules is not an issue. Where the parts cannot pass
verification apart, the issue stays one issue, and the plan reports the answer, the draft and that reason to the user instead of
writing a split. An `exception` object the command validates records the user's own statement naming this issue; it is kept in
the report and passed on to `crw-run` with its record. Check an issue again after it is changed,
and check every issue a split writes as an issue.

## Record the child pair

Every implementation issue this skill writes or refines carries one child pair line, beside its size
statement and under none of the headings the size check reads, so the line is neither counted as a
criterion nor read as a deliverable. The line records which pair a child runs on for this issue.
[Child pair by issue type](integrations.md#child-pair-by-issue-type) owns the routing table, the form of
the line, the precedence and the cases the table does not settle; this page only places the duty in the
plan.

Write the line with the issue, and write it again when a change to the issue changes its type. A split
writes each new issue its own line, because the parts can differ in type: a port that has an original and
the new mechanism that replaces its slowest part are one plan and two pairs. A non-PR issue that a child
will run, an investigation or an operational check, carries the line too. The read-back of the closing
check below reads it with the other fields.

Classify the issue when the line is written: tag it by [the kind of work and its procedure](integrations.md#classify-the-issue-by-its-kind-of-work),
read the bundle off [the Bundles rows](integrations.md#bundles), and add the tags, the bundle and the
source after the reason in [the extended line](integrations.md#the-extended-line), refining the tags
whenever a change to the issue changes the work they describe. The tags come from what the issue states and
use no result, run or child; where the issue's text does not settle one, amend the issue and do not guess.
A line in the older form stays valid and is not rewritten for this; the dispatching parent classifies it at
release ([the record at release](integrations.md#the-record-at-release)). Put the line on a line of its own
after a blank line, under a heading of its own such as `## Child pair` that is not deeper than the last
heading the size check reads, so the check does not read it.

## Non-PR work and unresolved targets

A research question, a contract or design decision, or an operational verification names
the artifact it delivers and where that artifact lives: a document revision, a recorded
decision, a measurement with the method that produced it, an observed state. Its
completion criterion is that artifact existing, and its verification is something a
second person can repeat. It carries no repository label and no placeholder PR
([issue-to-PR rule](integrations.md#issue-to-pr-mapping)).

When code work has no resolvable target repository, plan the issue anyway with its
deliverable and criteria, leave the repository label empty
([operating model](integrations.md#linear-operating-model)), and record the decision that
would resolve it — which repository to create or choose, and who decides — as its own
non-PR decision issue blocking it. The implementation issue is not ready for dispatch
until that decision issue is settled. Do not point it at a convenient checkout, and do
not invent a repository or a placeholder issue to fill the shape; an invented target is
discovered by the worker at assignment, after the plan claimed to be complete.

## Align a delivered issue

An issue that already has a delivery is reconciled before anything new is assigned to it. Read
three things first: the scope agreed when it was assigned, including its history; the
criteria still open, measured against what its linked PRs actually merged; and its
current owner and status. Then write one of these four outcomes before any reassignment.

- Its delivery PR has not merged. Reconcile with its owner before changing anything, and
  read the PR's usability and its ownership as two separate facts. Where the PR is open and
  its owner active, the remainder stays in it unless the remainder is a separate deliverable
  by the boundary rule above; where it is, agree the new boundary with the owner first, then
  narrow this issue and make the new one blocked by it. Where the PR was closed without
  merging or can no longer deliver, designate one replacement delivery PR and keep the
  superseded link ([issue-to-PR rule](integrations.md#issue-to-pr-mapping)). Where the owner
  is no longer active, recover ownership or obtain an authorized reassignment before any
  scope change or dispatch. A live assignment is never narrowed or split before its owner
  has seen the change.
- Its delivery merged and criteria are still open. A criterion inside the issue's own promise reopens the same issue key ([After the merge](../../crw-run/references/merge-readiness.md#late-review-threads)); move only the criteria outside it to a new issue with the merged one as its prerequisite, keep the criteria inside it open on this same issue key, record on the issue what actually merged, and only then may it be Done once every criterion it keeps is met. A PR that
  satisfied part of the accepted scope belongs here, and what it delivered stays
  delivered.
- The merged PR satisfies none of the accepted delivery criteria, such as instrumentation
  that only makes a failure observable, or a reference change. Link it as a contributing
  PR, leave the delivery link empty, keep the issue's status, and treat none of its
  criteria as met ([Implementation Done](integrations.md#implementation-done)).
- The issue is an existing multi-PR exception. Record the exception on the issue,
  inventory its required deliveries against the criteria still open, and reconcile with
  its current owner before dispatch.

Leaving the issue open with its criteria unchanged is not alignment. It defers the
decision to the next worker, who inherits an issue whose stated scope is larger than the
PR it may open. Check what a linked PR does to the issue on merge as well, and record the
intended link type when a PR will not complete it
([Implementation Done](integrations.md#implementation-done)).

Narrowing a delivered issue is a material change to agreed scope. The authorization the
request already gave carries it when that request covers alignment, and it is returned as
a proposal when it does not. There is no second approval step for a write the request
already authorized.

## Close the plan

Run these in order against the written items, not the draft. The order matters: each step
changes what the next one sees.

1. Coverage. Every outcome the requested scope promises maps to at least one issue
   criterion. A gap becomes an issue now where the request authorizes it, and is reported
   as unplanned scope where it does not. This runs first because a new issue changes every
   later step.
2. Duplication. No two issues claim the same deliverable in the same repository, matched
   by deliverable and target rather than by title. Keep one and move the other's relations
   onto it under the relation rule below, dropping nothing that was promised. This runs
   before relations are checked because consolidating a duplicate moves relations.
3. Relations. Every prerequisite stated in prose exists as a real blocking relation, and
   walking the edges finds no cycle. A cycle means the boundary is wrong, so reapply the
   boundary test to the issues in it rather than dropping an edge. Where the part both sides
   need has an observable result and can be verified alone, it becomes their first issue and
   its relations replace the edges the invalid boundary created. Where it does not and the
   coupled issues share one integration target, they consolidate into one merge that keeps
   every criterion. Where it does not and they land in different repositories, no boundary
   satisfies both rules: report the plan blocked on the contract decision or redesign that
   would separate them, and do not consolidate across targets. Where two issues change the
   same surface, exactly one ordering exists and it is a relation, not a note. Two issues
   may hold the same file; two issues holding the same region at once is a collision.
4. Read-back. After writing, fetch every item the plan touched — the issues and their
   relations, and any project, milestone, document or label it wrote — and compare what is
   observed against what was intended. An implementation issue's project is one of those fields:
   an issue observed with no project, or in a project other than the one intended, is an
   incomplete write, and a team, a product-family label or a relation does not stand in for it. The child pair line is another of those fields: an implementation issue read back without it is an incomplete write too.
   Report both lists with IDs. An intended relation or
   field that is absent is an incomplete write: repair it on the same IDs rather than
   creating the item again. Where the connector cannot read something back, report it
   written, unverified, and do not claim it exists.

A boundary change carries its relations with it. Keep every relation to work outside the
change, and drop one whose endpoints both fall inside it: it described a boundary that no
longer exists, and keeping it makes an issue block itself.

A draft-only or advisory request runs steps 1 to 3 against its draft items and their
intended relations, omits step 4, and reports the result as checked but unwritten. Writing
nothing is what that request asked for; skipping the check is not.

Re-running the same request converges because the split follows deliverables, and a
deliverable comes from accepted criteria that read the same on every run and match an
existing issue by scope. A split derived from the current file layout does not converge:
the layout moves with every commit, nothing matches it back to a criterion, and the second
run creates new issues beside the old ones instead of recognising them.

## Judgment cases

Apply the rules above to each Given without reading its outcome, then compare. Outcomes
are counts, relations and writes, so an independent run either matches or differs visibly,
and each row names the clause it exercises, so an edit to a rule shows which case it
breaks. Agreement between an independent application and the recorded outcome is the
check; matching words in this file is not. A divergence is either an ambiguous rule or a
wrong row, and both are fixed here.

| # | Given | Plan outcome | Clause |
| --- | --- | --- | --- |
| 1 | An outcome needs a change in the core repository and in the installer repository; the installer consumes the new core interface. | 2 issues, 1 repository label each, installer blocked by core, combined outcome on the milestone, 0 extra issues for reading core. | Different target repository. |
| 2 | One schema field, three consumers; one fails to build unless it lands with the schema, the other two can merge and be verified separately. | 1 issue: the schema field and its three consumers are one contract change in one repository, so they bundle; the coupled consumer must land with the schema, and the two consumers that could merge apart stay in the same issue because being verifiable apart permits a split and does not require one. 0 splits on verifiability alone; a consumer is split off only where its own repository, deployment, owner or risk boundary separates it, and then it is blocked by the schema issue. | Concept bundle: one contract in several files; verifiable apart permits a split, never requires one. |
| 3 | Half the scope merged last month under a Done issue, no PR is open, and the undelivered half builds on what merged. | 1 reopened issue for the undelivered half when it is inside the Done issue's accepted criteria, otherwise 1 new issue blocked by the Done issue; criteria not restated. | Already delivered or owned elsewhere. |
| 4 | "Decide whether the relay can share one database", with no code change. | 1 issue now, 0 repository labels, 0 PRs; deliverable is the recorded decision with its measurement, verified by the document revision holding it. Implementation the decision may later require is planned then, as its own issue blocked by this one, and is not created now. | Non-PR work. |
| 5 | The target repository does not exist yet and two candidates are disputed. | 2 issues: 1 non-PR decision issue naming its owner, and 1 implementation issue blocked by it. Both repository labels empty, implementation not ready for dispatch, 0 invented repositories. | Unresolved target. |
| 6 | The project was created; its label write and one relation failed. | Label and relation repaired on the created ID, 0 new projects; the report names the written IDs, the unset fields, and the intended and observed lists after re-reading. | Read-back; repair on the same ID. |
| 7 | The same request again on the same milestone, two commits later, deliverables unchanged. | 0 new issues, 0 renames, 0 re-splits; matching by stable ID and scope; only genuinely moved items change. | Convergence. |
| 8 | The user asks to move the milestone boundary and nothing else. | 1 milestone write, 0 issue writes; the closing check runs and reports the coverage gap the new boundary creates. | Narrow scope keeps its scope. |
| 9 | The delivery PR is open, its owner is active, and the remaining scope needs its own merge. | New boundary agreed with the owner first, then the issue narrowed to what that PR delivers and 1 new issue blocked by it. 0 reassignments, 0 changes made before that agreement, PR scope unchanged. | Align a delivered issue: unmerged delivery. |
| 10 | The delivery PR merged part of the accepted scope carrying a closing keyword, automation moved the issue to Done, two criteria are still open. | Conflict recorded and the issue corrected within the assignment, the open criteria inside the accepted promise reopen the same issue key, and any outside it become 1 new issue blocked by the merged one, the criteria inside the promise stay open on the same key until met, the narrowing to what merged is recorded, then Done once they are met. | Align a delivered issue: merged with criteria open. |
| 11 | A PR that only makes the failure observable merged; it satisfies no accepted criterion and the fix is unwritten. | Linked as contributing, delivery link empty, status unchanged, 0 criteria marked met, the issue's status confirmed against what the merge actually did. | Align a delivered issue: no accepted criterion satisfied. |
| 12 | An old issue carries five PRs from an agreed exception, some merged. | Exception recorded, required deliveries inventoried against the open criteria, reconciled with the current owner before dispatch, links and owner and history preserved, 0 silent splits or reassignments, pattern not copied into new issues. | Align a delivered issue: existing exception. |
| 13 | A bare "PR 단위로 정리해줘" on a milestone with no prior plan. | Every issue written carries the full issue fields, each boundary decided by the rule above, the closing check run; issue count is whatever the boundary rule yields, standard unchanged by the request's length, scope unwidened. | Default output standard. |
| 14 | A milestone outcome has no issue that delivers it, and the request authorizes the full plan. | 1 new issue created now for that outcome, relations recomputed after it, 0 promised outcomes left unplanned. | Closing check: coverage. |
| 15 | Two planned issues name the same deliverable in the same repository under different titles, and one of them is blocked by a third issue. | 1 issue kept, the relation to the third issue moved onto it before relations are checked, 0 duplicate deliverables, 0 promised work dropped. | Closing check: duplication. |
| 16 | A contract was split across two issues, each now blocks the other, and the shared part can merge and be verified alone. | The shared part becomes 1 new first issue blocking both, the 2 edges between them replaced by those relations, 0 external edges dropped. | Closing check: relations. |
| 17 | The connector writes relations but cannot read them back. | Relations reported written, unverified; 0 relations claimed to exist; the intended list still reported with IDs. | Closing check: read-back unavailable. |
| 18 | Two issues block each other, neither passes its checks without the other, and no smaller shared part can be verified alone. | The two consolidate into 1 issue keeping every criterion and every external relation, the 2 edges between them removed with the boundary that created them, 0 invented prerequisites, 0 external edges dropped. | Closing check: relations, coupled parts. |
| 19 | The delivery PR was closed without merging and its branch is gone; the owner is active and the accepted scope is unchanged. | 1 replacement delivery PR designated with the superseded link kept, 0 new issues, scope unchanged. | Align a delivered issue: PR no longer usable. |
| 20 | An unmerged delivery issue's owner is no longer active. | Ownership recovered or reassignment authorized first; 0 scope changes and 0 dispatch before that. | Align a delivered issue: owner inactive. |
| 21 | Two duplicate issues name the same deliverable and one blocks the other. | 1 issue kept with its relations to outside work, the 1 edge between the duplicates dropped, 0 self-blocking issues. | Boundary change carries its relations. |
| 22 | Two issues in different repositories block each other, neither verifies alone, and no standalone contract artifact exists. | 0 consolidations across targets, 0 invented prerequisites, plan reported blocked on the contract decision or redesign that would separate them. | Closing check: cross-target cycle. |
| 23 | The target repository runs no required checks, one part can be reviewed and accepted on its own, and the unfinished remainder has an observable criterion of its own. | The split is permitted on reviewability apart where the concept boundary also separates the parts; 0 issues split on the absence of a gate alone. | Reviewable apart where checks are absent: a permission, not a duty. |
| 24 | A proposed second issue would carry only the remainder of the work, with no result of its own. | 0 new issues; the work stays with the issue that names a deliverable, 0 issues whose only criterion is the rest. | Forbidden: no observable result of its own. |
| 25 | A prior plan split the work by directory and the directories have since been reorganized. | Issues rematched by deliverable against accepted criteria, 0 issues recreated beside the old ones, 0 splits derived from the new layout. | Convergence: layout is not a deliverable. |
| 26 | The size check answers `over_line` for an implementation issue and drafts four bundles; two of them can be verified apart and the other two only together. | The concept boundary decides: each bundle that is its own concept may become its own issue, the coupled two stay 1 issue keeping every criterion, order edges where the regions overlap; being verifiable apart permits the split and does not require it; 0 issues written from the draft as given. | Size check: the draft is a guess, the boundary rules decide. |
| 27 | `over_line` and no bundle can pass verification apart. | 1 issue, 0 splits; the answer, the draft and the reason reported to the user; it dispatches normally, with the `--bundle-reason` recorded when the parent accepts it over the baseline. | Size check: parts that cannot pass apart stay one issue; the answer never blocks. |
| 28 | The size check answers `ok` for an issue that also needs a change in a second repository. | 2 issues by the repository rule; the answer changes nothing in the boundary rules. | Size check: `ok` changes nothing. |
| 29 | An issue ports a module to another language, and the original's recorded outputs decide when it is done. | 1 issue with 1 child pair line, Sonnet (reference exists); 0 lines that name a model or an effort. | Record the child pair: the answer comes from a reference. |
| 30 | A test fails about one run in twenty, and two earlier fixes did not remove the failure. | 1 issue with 1 child pair line, SOL (answer must be found): the cause is unknown and a measurement ends the work. | Record the child pair: the answer has to be found. |
| 31 | A new mechanism decides what a sandbox may be asked to do, so its core is a permission decision. | 1 issue with 1 child pair line, Sonnet, whose reason names the security rule although the shape says SOL. | Record the child pair: a security judgment at the core goes to Sonnet. |
| 32 | An issue written before this rule has no child pair line, and the parent is about to dispatch it. | The parent routes it by the table, writes the line on the issue and reads it back, and only then prepares the packet; 0 children created before the line exists. | Record the child pair: an issue with no line. |
| 33 | A plan writes an issue that rewrites a skill's procedure under the structure the existing documents already have. | 1 issue with 1 child pair line in the extended form: `answer=reference`, `output=writing`, bundle Sonnet fixed by row 3, axis `reference exists`, source `table`; the size check gives the same decision, counts and regions as for the same body with the older line or with no line. | Record the child pair: classify with the issue. |
| 34 | An issue reads as a wiring change that an existing pattern decides (`answer=reference`), but two earlier fixes of the same fault did not remove it. | 1 issue with `history=retried`: bundle SOL fixed by row 2 before row 3 is read, axis `answer must be found`; 0 lines that stay Sonnet because the first reading was a wiring change. | Record the child pair: failure history comes before the reference. |
| 35 | A plan writes an issue for a new report format that nothing outside the issue decides (`output=writing`, `answer=found`). | 1 issue, bundle flexible by row 5, axis `half-half`, pair by the count rule, source `table`; the shape table's third row is not reworded. | Record the child pair: the ambiguous case. |
| 36 | Two flexible issues are planned in a project with no line yet and no child working. | The first goes to either pair and the pick is recorded; the second goes to the pair the first did not take, because fewer of the project's lines name it. | Record the child pair: the count rule's tie. |
| 37 | Two planned issues each add a new file to one Go package, and both would declare a `tokenize` helper. | 2 issues, no order edge for the files alone; each states the names already used in the package and a prefix of its own, and the shared helper is either 1 prerequisite issue both depend on or part of 1 issue; 0 criteria that name one identifier in two issues. | Decide the boundary: sibling issues in one package. |
