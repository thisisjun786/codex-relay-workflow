# Task packet and coordination record

Use this when writing a launch prompt or handing off work. Fill in actual values;
remove inapplicable items instead of sending placeholders. Embed the acceptance
criteria unless the worker's access to the linked issue has been established;
another task may not have the same connectors.

## Child task titles

Name each managed issue task as `ISSUE-ID · descriptive task title`,
for example `JUN-44 · 가격 조회 결과를 검증한다` or
`JUN-41 · 기간별 사용량을 집계한다`.
These are formatting examples, not issue assignments.

Use only the real Linear identifier and a Korean description that makes the
assigned result clear. The title after ` · ` may be a natural sentence or phrase
of up to 20 characters, including spaces and punctuation; the issue code and
separator do not count. Do not abbreviate away the task's meaning just to make
it shorter, or pad a clear title to reach 20 characters.
Do not append workflow labels (including `CXC Loop`), repository,
PR number, model, CI status, or merge status. Keep the execution mode in the
packet's `Workflow` field and verify runtime behavior separately. A title or a
chat link does not establish the task's native PR association; keep PR linkage
separate from naming. An explicit user-supplied title takes precedence over
this default. This convention names issue children; the management task above them
is named by [Set the app presentation and record](../../crw-plan/references/integrations.md#set-the-app-presentation-and-record),
including the product-family prefix that no child title carries.
Each implementation packet names its
one issue and intended PR. A batch retains separate packets and issue/PR pairs;
do not use a primary issue to hide a combined delivery. If no issue is linked,
use the known project name instead of inventing an issue number and reconcile
the mapping through `crw-plan` before new implementation dispatch.

The title names the Codex task and nothing else. The packet's `Title:` field carries it so
that the creation call and its read-back have one value to compare, and it is not the pull
request's title. The child titles its own pull request in English, as `Language:` requires,
under the rules of the repository it targets; where those rules state no format, the packet
names one, such as `CRW-275: <short English summary>`. Put that into the packet beside the
`Title:` line, because a child handed a Korean title and no sentence on what it names can
publish it as the pull request's title: six of the pull requests #278 to #294 carry the
Korean task title (#282, #285, #287, #292, #293 and #294), recorded as S23 in
[Dispatch verification](dispatch-verification.md).

The coordinator passes the title through the creation tool's supported title/name
field and includes it in the packet. Prompt text alone does not prove the app title
was set. Read back the actual title using the returned task ID. If the host generates
or changes it, correct that same managed task through a supported title tool within
the creation/management assignment, then verify it. Keep a compliant title on routine
repair follow-ups; normalize a legacy workflow or status suffix on the same task
rather than creating a replacement task to fix a name. If title control or
read-back is unavailable, record the requested title and limitation and continue
authorized work. Task identity and recovery always use stable IDs, not title matches.

## Launch packet

This packet targets a verified independent implementation task for one issue/PR
pair, or an explicitly non-PR result. Follow [Independent implementation tasks](../SKILL.md#independent-implementation-tasks)
before dispatch. A packet's wording cannot turn an internal subagent into that
task. Record the existing owner and creation/reuse authorization before sending.
For non-PR work, remove inapplicable Git/worktree/PR/OPS delivery fields and steps
below. Carry the input baseline and delivered output identity separately: stable
link plus output revision/updated-at evidence, or durable file locator plus digest.
Retain a verified snapshot when old linked revisions cannot be recovered. Retain task identity, permissions and recovery information.

```text
Task: [one issue ID and bounded result]
Parent: [one Linear project ID and verified coordinator task ID, or no project
  for a standalone issue; retain the real coordinator task ID if delegated.
  Initiative membership does not assign another project]
Supervisor: [the initiative ID and task ID of THIS project's designated execution supervisor,
  where one exists; context only. A project contributing to several initiatives still has exactly
  one, and the others are not named here because they only reference its outcome.
  A supervisor works through your parent and is not a route into this task]
Issue/PR mapping: [one implementation issue ID, target repository, and intended PR scope
  or existing PR URL; related issues are dependencies, not additional deliveries.
  For non-PR work, state the result and how it will be verified]
Title: [the Codex task title: issue ID · descriptive title of up to 20 characters, following
  Child task titles]
  This names the Codex task only. Your pull request's title is yours to write, in English as
  the `Language:` line requires and under the repository's rules for titles; it is never a
  copy of this field
Workflow: [effective workflow per Default independent execution]
Language: English for everything you write: your messages and final return, commit messages,
  the pull request's title, body and review replies, and receipt text. Keep exact identifiers,
  quotations and code as they are

Context:
- Code target: [verified GitHub owner/repo or URL; explicit none for non-code work]
- Reference-only repositories: [if any, not assigned edit targets]
- Repository and existing worktree: [absolute paths, when applicable]
- Remote name/URL and intended integration branch: [verified repository-policy values]
- Issue branch and full baseline commit: [actual values; new assignment or preserved resume]
- Prerequisites: [verified contract/commits and how included]
- Effective model/effort: [values from the request or Default independent execution;
  actual configuration is supplied by creation]
- Coordinator: [task ID; context only, never a CXC session binding]
- Responsible child: [existing independent task/host ID, or newly created task from the launch receipt]
- Assignment route: [reuse / create; current writer state and authorization source]
- Management marker: [the stable creation request id this launch was issued under, per
  [Prepare and dispatch](../SKILL.md#prepare-and-dispatch)]
- Relay assignment, when one holds this issue: [shared state directory, set as BOTH
  `--state` and `CODEX_SESSION_RELAY_STATE` on every process that reaches the socket,
  and the exact issue identity registration was given, not a display key that differs from it;
  these depend on nothing task creation returns; add the relationship id and current
  generation only where the assignment is already registered, since a newly created
  child's registration needs the task id creation has not returned yet and resolves its
  own relationship by issue lookup]
- Determined execution mode: [relay-managed or explicitly direct, with the reason where it is
  direct; the resolved state directory and socket, determined as
  [One shared state directory](relay.md#one-shared-state-directory) says; the store identity
  the determination's own reading reported; the delivery owner; and what availability was
  measured rather than assumed.
  Determined at managed start or resume per
  [Determine the execution mode](../SKILL.md#determine-the-execution-mode). Carried here because
  a child that is told only the state directory cannot tell an agreed direct assignment from a
  relay one nobody decided about, and those two call for different behaviour on completion]
- Canonical criteria, when registered: [criterion ids and the document they came from]
- Canonical Linear documents: [IDs/URLs and observed revision/date]

Authorized execution:
- Sandbox/permission profile and approval policy: [agreed values]
- Delivery: [where the assignment covers publication, a child-owned PR opened for review, not
  left in draft: branch, pushed head, and that PR's own review cycle. Otherwise local commits or
  a frozen diff. Publication is never inferred from the delivery line alone]
- External actions: [actions covered by the assignment and shared defaults, with any narrower user limits]
- Integration owner/target: [coordinator and verified destination; copy the applicable dev default or explicit delivery limit]
- Operations clauses carried to this child: [OPS-5.5 and OPS-9 from
  [Operations contract](operations.md), cited by id where this child can read this
  repository and quoted in full where it cannot. Carry the clause text rather than a
  summary of it: a quotation can be diffed against its source and regenerated when the
  clause moves, and it is the paraphrase that drifts unnoticed. A packet carrying neither
  sends the child the previous workflow]
- Runtime/test data access: [agreed sources and operational limits]

Workspace ownership:
- Existing resources at dispatch: [everything already present, each entry labelled as already
  owned by this assignment or owned elsewhere, since predating the run and belonging to it are
  separate facts and a resume already owns some of what it finds: the worktrees, branches and
  current writers for this repository with their dirty/untracked state, and the temporary
  artifacts, evidence roots, shared originals and running processes already present at the
  locations you are assigned. Do not adopt what is labelled owned elsewhere; a convention path
  matching your task's name is not evidence that it is your path. This is the baseline the
  close delta below is measured against, so anything missing from it reads afterwards as
  something this run created]
- Your resources: [the checkout, branch and evidence root this assignment owns, with the
  OPS-5.2 columns: created by, editing owner, git metadata owner, retention owner and
  cleanup authorization, `none automatic` where no cleanup is authorized. For a child this
  call is creating, name the editing owner by the management marker this launch was issued
  under, since the native task id does not exist until creation returns it; the coordinator
  binds that marker to the id from the creation receipt and reads the record back, and
  ownership is established there rather than by this line]
- Write capability: [first what the coordinator measured on the paths themselves, which does
  not depend on this task existing yet: the checkout, its resolved git metadata
  (`git rev-parse --absolute-git-dir`, `--git-common-dir`, `--git-path index`) and the evidence
  root, with any OS permission, read-only mount or live writer found on them. Then the profile
  this task is being created with and the paths it is meant to reach, which its creation receipt
  confirms rather than this packet, and the recorded OPS-5.3 fallback where it will not reach
  them. A refused write is answered on this same checkout, by clearing a refusal a supported
  route can clear or by that recorded fallback where the refusal is this task's own profile,
  never by `GIT_DIR`, a throwaway clone, an improvised proxy commit or reset/stash]
- Capacity and large artifacts: [the destination volume to check before a large clone,
  install, build or download, and the permitted shared read-only originals, per-task
  temporary paths and other volumes to use instead of copying a large original in here.
  A temporary directory the packet names for a tool, such as `TMPDIR`, is a short path (for
  example `/var/tmp/crw-<n>`, on a volume with free space), and the packet states why: a Unix
  socket's whole path must stay under 104 bytes, since Linux refuses a path of 108 bytes or
  more and macOS one of 104 or more, and a test that binds a socket under `TMPDIR` adds its own
  directory and file names to it, so a per-task scratch path used as `TMPDIR` can be too
  long, and even a short one is no proof that a test's socket binds. A test that fails with
  `bind: invalid argument` under a short `TMPDIR` is reported with its path length, not worked
  around. Large disposable output goes under a scratch path the packet names separately]
- Processes you start: [carry this rule in the packet's own words, because the child works from
  the packet and may never read this reference. Other tasks build and test on this host at the same
  time, and a command line does not say whose process it is, so a kill that selects by pattern ends
  their runs along with yours: their checks stop without a word, and their owner can spend a long
  time looking for a failure no change of theirs caused. Stop only an OS process you started: by the
  pid you recorded when you started it, by a process group you created for it (for example by
  starting it under `setsid`; a signal to the lone pid of a shell or of `make` can leave its
  children running), or through the handle the execution tool returned for it. Having started a
  process is necessary and not enough: the relay's delivery service is shared, and a parent that
  started it still does not end it early ([OPS-4.1](operations.md#ops-41-ownership-is-the-operating-scope-not-a-parent)).
  Never select a target by pattern or name: no `pkill`, no `killall`, no `fuser -k`, and no pid
  taken from a `pgrep`, `ps` or `lsof` lookup by name, command line, directory or port, because a
  listing shows what is running and not whose it is (reading one to report a process is fine).
  Record the pid of a long command when you start it, or run it under `timeout`, and confirm a
  recorded pid is still your process before you signal it, since a pid is reused after its
  process exits. A process you did not start is not yours to stop, whatever it holds: report it
  with its pid and working directory and leave it running]
- Resource delta to report at close: [measured against the baseline above, what this task
  created, changed, retained, shared or cleaned, each with its owner, release condition and
  next action; the working directory, purpose, handle and running state of any process it
  started; and any capacity actually reclaimed, kept apart from what was only proposed.
  Only this task can see the temporary paths and processes it makes during execution]

Outcome and scope:
[User-visible behavior, acceptance criteria, intended edit surfaces]
[Shared contracts, coordination boundaries, and necessary exclusions]

Execution:
- Read applicable project instructions and relevant source.
- You orchestrate this one issue and its internal helpers. Do not absorb another
  issue into this task or PR. The parent orchestrates one project and owns coordination,
  delivery validation, and authorized integration. Do not adopt the parent's CXC binding.
  Where an initiative above it has a supervisor, that supervisor works through your parent:
  it does not instruct you, and you report to your parent. See
  [Supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope).
- Where the assignment covers publication and you can push, own the delivery end to
  end: implement, test, commit, push, open the pull request, then triage, fix, reply
  to and recheck its reviews. Report once the current head's required checks and
  reviews have finished and their blockers are resolved, not when the code is written.
  That publication scope is the explicit push approval CXC `DEV-GIT-PUSH-01` requires, the
  standing authorization of
  [Default dev integration](../../crw-plan/references/integrations.md#default-dev-integration)
  carried by this packet: push your task branch and open the pull request without stopping
  to ask, and never merge; no force-push, no tag, no push to `dev` or `main`.
  Without that authorization, or without the access to use it, commit locally or
  return the frozen diff and say which publication you did not perform.
- Finish your own independent review before you open the pull request. The review meant here is the
  one your workflow runs on the candidate inside this task; whether and how often it runs is that
  workflow's decision, which this packet does not change. Where one runs, it ends on the head the
  pull request is opened from, and the hosted review then follows on the open pull request, so the
  hosted review reads a change your review has already answered instead of overlapping it on one
  head. The handoff says which head your review covered and lists the commits made after it ([what a
  handoff discloses](#what-a-handoff-discloses)).
- A finding of your own review that you reject is not yours to close. When the review rated a
  finding High or blocker and you would not apply it, because you rebutted it or place it outside
  this issue, list it in the handoff as a decision request, or ask your parent first when the answer
  decides what you build next: end the turn `blocked_needs_input` with a blocked receipt. An
  internal finding has no pull request thread, so a rejection the handoff does not show is one
  nobody can find.
- Open that pull request non-draft, or transition an existing draft to Ready for review
  as soon as the implementation is reviewable, then request the review the repository
  requires and this assignment authorizes, and confirm it actually started. An optional
  reviewer that cannot start or stalls is recorded as a gap and does not hold you, except that you
  wait for the one run each of Devin and Codex makes on the open pull request to end, or to be skipped,
  before you emit ([Devin and Codex reviews are references, not merge gates](merge-readiness.md#devin-and-codex-reviews-are-references-not-merge-gates));
  a required gate does. Findings, pending CI and your own revision pushes do not send
  it back to draft; fix on the open pull request and refresh only the review evidence
  invalidated by the change. Apply the [reviewer policy](merge-readiness.md#reviewer-policy)
  before requesting or waiting for a review. Ready is review entry, not merge permission. A criterion or gate line in the packet that
  reads "Devin has no red or security finding" means that if a Devin review exists, its red and security
  findings are resolved, and that no new Devin review is awaited
  ([what the record says](merge-readiness.md#what-the-record-says)). A thread that still reaches the head
  after your receipt is not yours to chase and needs no new review: the coordinator triages a minor one itself,
  and where its relay cannot record that it asks you only to read the review threads again and emit again, and
  a red, P0, P1 or security one comes back as an ordinary correction, both only while a correction can still
  reach you: once your result is accepted the coordinator holds the candidate instead, and after the merge it is
  new work ([Late review threads](merge-readiness.md#late-review-threads)). See
  [Publish for review when the work is reviewable](../../crw-plan/references/integrations.md#publish-for-review-when-the-work-is-reviewable).
- Finishing the review is part of finishing the work. Read every applicable review to the
  end of its pagination on the CURRENT head, judge each finding against the code, fix what
  needs fixing, reply where a finding does not apply and say why, and recheck. Then state
  that result rather than summarising it: a handoff record naming the pull request, the head
  it is about, the base you verified, the check runs by id and attempt, the review coverage
  you actually read, and a judged disposition for every thread you saw. Resolving a thread is
  a button; `fixed`, `accepted`, `not_applicable`, `duplicate`, `already_resolved` and
  `disputed` are judgments. `fixed` names the commit that did it and `accepted` names the
  parent decision that accepted it and the follow-up it left. Fix what blocks under
  [Judge a finding by its impact](merge-readiness.md#judge-a-finding-by-its-impact), propose
  the rest instead of granting your own acceptance, and never record `fixed` or
  `not_applicable` for a defect that is real and still there: the gate counting unresolved
  threads measures the button, so clearing it with a judgment nobody reached buys a green
  badge with a false record. If a required check has not passed or a mandatory review has
  not finished, that is BLOCKED and is reported as blocked. Do not report completion with a
  note about what is still open, because the note is what gets skimmed past.
- A correction that asks only for the base to be brought up to date is not new scope. Merge the
  current base into your branch with a merge commit, name the kind of each merge you made, and rerun
  what that kind needs and no more: the gates for a `clean` refresh, the gates plus the
  deterministic checks over the listed hunks for a `mechanical` one, and the gates plus an
  independent check of the hand-resolved hunks only for a `manual` one ([the
  kinds](#what-a-handoff-discloses)). Do not audit again what the refresh did not change.
- Maintain CXC: load current cxc-dev and relevant surface skills, and follow
  the configured CXC protocol for helpers and review within this task.
- Work in the assigned existing worktree; preserve unrelated changes.
- [Only when a relay holds this assignment:] before acting on the assignment, correction or
  resume packet you received, run the store-backed `packet-check` described in
  [the typed form these fields travel in](#the-typed-form-these-fields-travel-in) with your
  own task id and reception ledger. A packet that names an artifact - every correction and
  every resume does - is checked with `--observation` holding your own reading of that
  artifact, because no store holds one. Act only on an accepted answer whose `act` is true.
  An unavailable answer is not permission to act: read what it lists as unread and check
  again, which for an artifact means observing it yourself. Once you have acted, record it
  with the same command and `--applied`, without `--observation` (refused unless a check of
  that packet said `act`).
- [Only when a relay holds this assignment:] emit your completion receipt for this
  generation over the actual deliverable paths, from inside your own turn, against
  the shared state directory. Offline that receipt is STAGED until an independent
  observation sees your turn end; that is expected and is not a failure. When you
  re-emit inside the SAME generation, state which revision your new one replaces, so
  the parent finds one current revision rather than two with neither current.
  A correction arrives as a NEW generation, and its first receipt names no predecessor:
  lineage is read within one generation, so naming the previous generation's revision
  declares a predecessor this generation does not contain and leaves it with no
  current head at all.
- [Only when a relay holds this assignment:] know which turn your receipt is emitted from.
  The relay accepts a receipt from the generation's anchor turn (the dispatch turn it
  bound for that generation) and from a turn already admitted against that anchor, which in
  the first generation of a managed start includes the business turn that delivered this
  assignment once managed-start has confirmed it. Under CXC Loop the work usually ends in a
  different turn: a goal-continuation turn, or one a restarted App Server opened. An emit from
  such a turn is refused `unassigned_turn` unless it carries a continuation claim, so attach
  the claim to that first emit instead of learning it from the refusal:
  `--continues-anchor <the generation's anchor turn> --continuation-actor <your own task id>
  --continuation-reason '<why this turn continues the same execution>'`. Only
  `--continues-anchor` is required; the actor defaults to the turn thread and the reason to a
  generic text, so state both. Read the anchor from the relay, not from a value this packet or
  your coordinator typed: `status --relationship <relationship id>` prints it as
  `observation.anchors.<relationship id>.turnId` for the generation that is open now, the
  managed routing record's `standbyTurnId` is the first generation's anchor and no other
  generation's (a correction opens a new generation anchored to the turn its revision request
  arrived in, and the routing text's instruction to name `standbyTurnId` does not carry over to
  it), and the `unassigned_turn` refusal names the anchor it expects. `assignment-show` and
  `assignment-find` do not report it. The relay first checks that the turn belongs to your own
  task, so a turn of another task is refused whatever it claims. When it then admits a turn that
  was not admitted before, it checks that the claim names the anchor bound for that generation
  and records your actor and reason without verifying them, so the claim is a statement you
  answer for. A turn admitted once needs no claim again within that generation.
- [Only when a relay holds this assignment:] if you cannot emit because of the assignment's
  own state rather than your artifact, whether the issue lookup finds no assignment, the
  lookup is refused `store_absent` because no store exists there yet, or the
  emit is refused with something like `unbound_generation`, stop there and report the
  completion as UNEMITTED with what you actually saw: the exact lookup result where the
  lookup came back empty or was refused, the exact refusal where an emit was rejected.
  Preserve the artifact as produced and
  return your own task id, the issue identity and the state directory you were given. Do not claim
  a receipt you could not write, do not guess a relationship id, and do not wait for the state
  to change: the coordinator closes that gap and recovers the receipt from you, on this same
  task.
- [Only when a relay holds this assignment:] if the lookup instead finds an assignment owned
  by a DIFFERENT task, that is a conflict rather than a delay and it is not recovered through
  you. Report it naming the owner the lookup returned, preserve your artifact, and write
  nothing into that relationship; the coordinator reconciles your work with that owner.
- If you need something only a person can give, such as a decision, a credential or an
  approval this packet does not carry, do not ask with `request_user_input`: CXC denies it
  while your goal is active. Write the question out, record `blocked_needs_input` on your
  turn and, where a relay holds the assignment, emit that outcome over a file that states
  it; where none does, return the CXC status the case takes (BLOCKED, UNSAFE or NEEDS_HUMAN)
  with the question. Then end the turn. Your parent takes the question to Jun. See
  [Default dev integration](../../crw-plan/references/integrations.md#default-dev-integration).
- [When CXC Loop is the effective workflow:]
  `$codexclaw:cxc-loop` — invoke the installed skill, or attach it through the
  creation tool's supported skill field, and run this bounded objective under it and
  `cxc-pabcd` using your own session binding, host goal, and goalplan.
  If a required loop capability is absent, report the exact gap before starting.
  CXC `LOOP-DOCS-FIRST-01` applies to your own issue as CXC states it: a single-cycle issue
  skips the docs-only first cycle, and a child that plans two or more work-phases opens with
  one, with CXC's roadmap debt for scope found later.
  A correction generation, a base-refresh generation and a separated publication step are not
  the first work-phase of new work, so they do not open `LOOP-DOCS-FIRST-01`'s docs-only cycle.
  On a correction or a resume, read whether that goal and goalplan exist before making
  either. Usually they do, and the work is to continue them: opening a second goal for
  the same assignment is a duplicate rather than a resume, and the coordinator reads it
  as one. Where activation never succeeded there is nothing to continue, and starting it
  then is the activation that was owed rather than a duplicate. Say which of the two
  happened, because from the outside they produce the same new goal.
  Confirming identity means reading it rather than assuming it: your own native task id,
  the native working directory you are actually in, the source checkout that directory
  belongs to, and your real recorded workflow state. Those four can disagree after a
  resume or a compaction, and the disagreement is the thing worth catching. A session
  binding is identity, not proof that a hook ran. Where a relay holds the assignment its
  generation is read from the assignment by lookup, never inferred from your own state
  and never copied from the coordinator's.
- [For an explicit non-Loop or no-goal alternative:] omit that invocation and keep
  the agreed workflow. Assignment alone does not create a goal or activate a Loop.
- Apply the agreed permissions and delivery scope. Do not modify global model or
  role settings to match the requested model.

Verification:
[Specific commands, invariants, negative cases, and real UI/API behavior. Every make target,
CI check and repository script it names exists, confirmed against the packet's baseline
commit when the packet is written; say so where an issue text or an earlier packet names one
that does not exist. A command that runs a tool directly, such as a focused `go test` run,
is fine once the writer has confirmed that the top-level test it names exists in the same
build, with an anchored listing such as `go test -list '^Name$' ./pkg` that prints it]
[Allowed test data and runtime boundaries]

Return:
- Actual task ID, worktree, branch, baseline SHA, and final commit SHA if committed.
  For diff-only delivery, return the frozen diff/file bundle path and SHA-256.
- Where a relay holds the assignment and you emitted: receipt event id, revision hash, the
  generation it was emitted under and the turn it was emitted from. When that turn was
  neither the generation's anchor nor one already admitted, also the anchor your continuation
  claim named and where you read it.
- Where no other task owns the assignment and you still could not emit: the completion marked
  UNEMITTED, the preserved artifact paths, your task id, the issue identity and the state
  directory, and what you actually saw. An empty lookup is reported as the lookup result itself,
  since there is no refusal to quote; a rejected emit is reported as its exact refusal, and a
  refusal like `unbound_generation` means the lookup did find this relationship and generation,
  which the coordinator needs to bind the anchor, so report both. There is no receipt to report.
  A lookup naming another owner is the next case, not this one.
- Where the lookup found a DIFFERENT owner: the same preserved artifact and identity, the
  owner the lookup returned, and that nothing was written into that relationship.
- Requested/observed task title, or the exact title-verification limitation.
- Resulting behavior and scoped changed files.
- Acceptance-criterion evidence, commands/results, and artifact paths.
- Changed contracts and what dependent tasks need.
- Requested/actual model and effort, the role they were checked against, and whether that role's
  pair was declared on this host or the check went unmade; disclose whether served-model proof
  exists. A report that says verified when nothing was compared is the failure this line exists
  to prevent.
- For CXC Loop: goal/goalplan identifiers, final FSM state, and completion evidence.
- Delivery artifact: [PR URL, pushed head SHA, and the state of its required checks and
  reviews, including how each finding was resolved; or the frozen diff bundle for a
  restricted or narrowed delivery].
- For a pull request: URL, title as published, base and head SHAs, `isDraft`, the review
  receipts for the current head, and any unresolved finding. Record the relay receipt's own outcome
  separately; `ready_for_review` there is not `isDraft=false` here.
- Merge-readiness handoff, for a pull request you are handing over: the repository and pull
  request number, the head all of this evidence is about, the base you verified and when,
  the check names this branch declares required, the check runs as
  `{runId, name, headSha, conclusion, attempt}`, the review coverage as
  `{hasNextPage, pagesRead, totalCount, threadsSeen, unresolved}`, a judged disposition with
  evidence for every thread in `threadsSeen`, where an `accepted` one also names the parent
  decision behind it in `addressedBy`, its `followUpOwner`, and the `reopenTrigger` that
  brings it back. Those last two are separate fields because they are separate facts: one
  string naming an owner and saying nothing about what reopens the finding is a waiver
  wearing a follow-up's name. Then your per-criterion evidence and the limitations that
  remain. State every field: an absent one used to read as satisfied, so a record that said
  nothing passed every check. The parent restates these values immediately before merging
  rather than collecting them again, which is why they are yours to produce, and it confirms
  each acceptance against a decision it actually made.
- Disclosures, for a pull request you are handing over, as the `disclosures` member of the handoff
  file: what your internal review found and how each High or blocker finding was disposed of, each
  one you rejected as a decision request, the head the review covered and the commits after it, one
  `baseRefresh` entry for each merge of the base, the changed paths compared with the declared edit
  regions, and the sibling impact. [What a handoff discloses](#what-a-handoff-discloses) defines
  each item and the kinds of refresh. State every item, with `none` where there is none, because an
  item left out reads as not checked.
- Remaining defects, unverified behavior, and possible integration conflicts.
- Proposed changes to a Linear record, returned rather than written: the document or issue ID,
  the revision you read, the reason, the smallest sufficient change and its evidence.
Stop after this assigned result; do not auto-start another issue.
```

The workspace-ownership fields are dispatch observations rather than a grant: what constrains
a write is the sandbox, the permission profile and the filesystem, so a field naming a path
the child cannot write is a mismatch to reconcile on that task before the work starts, not
something for the child to work around. The check that produces them is
[Check who already owns the workspace](../SKILL.md#check-who-already-owns-the-workspace),
and what the run leaves behind is accounted for by
[Account for the resources this run leaves behind](../SKILL.md#account-for-the-resources-this-run-leaves-behind).

### First full assignment required fields

The fields below are defined above; this is the check that the first full assignment
actually carries them, because it is the request that starts the work and a later message
cannot retroactively be the one the child started from. Each line names where the field
is already specified rather than restating it, giving the [Non-PR packet](#non-pr-packet)
field in brackets where that reduced shape names it differently.

- Effective workflow and the skills to apply — the Loop and non-Loop branches under
  `Execution:`, plus `Workflow:` [`Workflow/settings:`]. Where CXC Loop is effective the
  packet carries the literal installed-skill invocation and names the applicable surface
  skills; naming the skills descriptively is not the invocation. Where an explicit
  non-Loop or no-goal alternative is effective, that workflow is named in its place.
  Where Loop is effective the packet also says how `LOOP-DOCS-FIRST-01` applies to the
  child's own issue, as
  [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution)
  states it.
- Issue scope — `Issue/PR mapping:` and `Outcome and scope:` [`Scope:` with its
  `Input baseline:`], including the exclusions.
- Verification boundary — `Verification:`, covering what this child verifies itself and
  what it does not.
- Handoff and completion boundary — the delivery contract under `Authorized execution:`
  and the `Return:` block [`Scope:` write authority and `Return:`]: what finishing means
  here, what the coordinator owns after it.
- Publication scope — the publication sentence under `Execution:` and the `Delivery:` line
  under `Authorized execution:`. The packet says that its publication scope is the explicit
  push approval CXC `DEV-GIT-PUSH-01` requires (push the task branch, open the pull request,
  never merge), or that its scope excludes publication and the child pushes nothing.
  [Default dev integration](../../crw-plan/references/integrations.md#default-dev-integration)
  owns the rule; this line checks that the packet carries it. [A Non-PR packet has no
  publication to approve.]
- Escalation route — the `Execution:` bullet on a question only a person can answer:
  `blocked_needs_input` with the question written out, never `request_user_input`. [A Non-PR
  packet carries it in `Workflow/settings:`.]
- Model and effort — `Effective model/effort:` [`Workflow/settings:`], applied through
  the creation tool's real arguments and read back from the receipt.
- Language — `Language:`, the same in both shapes: what the child writes is English from its
  first message, since a later correction cannot rewrite the commits and pull request text
  already published.
- Title — `Title:`, in both shapes: the Codex task title. In a pull request packet a sentence
  follows it saying that it is not the pull request's title, which the child writes in English
  under the target repository's rules ([Child task titles](#child-task-titles)), and the pull
  request's title comes back in `Return:`.
- Temporary path — a `TMPDIR` or other temporary directory the packet names is short, and the
  packet states the socket path limit, in the `Capacity and large artifacts:` line.
- Process rule — the `Processes you start:` line under `Workspace ownership:` carries it in the
  packet's own words: stop only processes the child started, by a recorded pid or its own process
  group, never by pattern or name, and record a long command's pid or run it under `timeout`. The
  child works from the packet, so that line is the only place the rule is guaranteed to reach it.
  [A Non-PR packet carries the same line in its `Workspace ownership:`.]
- Verification targets — every make target, CI check and repository script under
  `Verification:` was confirmed to exist when the packet was written, against the checkout at
  the packet's baseline commit. A make target is
  confirmed by its rule in the Makefile or in a file the Makefile includes. Read `make -n
  <target>` beside it as supporting evidence: `No rule to make target` means the name is no
  target, and `Nothing to be done for '<target>'` with exit status 0 means make found nothing
  to run, which is what a name without a rule prints when a file or directory of that name
  exists. `make contract` does that beside a `contract/` directory, and a gate that ran it would
  pass with nothing checked. A target with a recipe prints it, or `'<target>' is up to date`. A CI
  check is confirmed by the command in the workflow file or the repository's CI document, run
  with its help or dry-run mode where it has one. A target nobody could confirm is replaced by
  the real one, or the packet says that check has no command; it is not passed along. A command
  that runs a tool directly, such as a focused `go test ./pkg -run '^Name$'`, is not a target and
  is allowed once its test is confirmed to exist. Confirm it with an anchored listing run with the
  verification's own package and build tags (the Makefile passes `-tags dev` for
  `./cmd/crw-dev/...` and `./internal/dev/...`): `go test -list '^Name$' ./pkg` must succeed and
  print `Name`. `-list` and `-run` take a regular expression, so an unanchored `Name` also lists
  `NameOld`. An empty listing (only the `ok` line) means no matching top-level test function in
  that build, and a listing that fails (a missing package, a build error) confirms nothing. The
  listing does not print subtests: confirm one from the source or by running it with each
  slash-separated part of `-run` anchored. A `-run` that matches nothing still exits 0 with
  `[no tests to run]`, so the packet gives the anchored pattern.

One obligation is new rather than a restatement. Where a workflow with its own goal and
state is effective, the child's first execution leaves its own activation evidence and
reports it, instead of the goal and goalplan identifiers surfacing only in the final
`Return:`. A loop that never armed is otherwise discovered when the work is over, or not
at all, and by then the same reading cannot distinguish a child that never armed from one
that armed and lost it.

None of this adds a turn. These travel in the same first request as the work and the child
starts the assigned work from it; a separate turn that collects a readiness report before
the body is the removed handshake rather than a check, and a capability the creation path
cannot apply is settled before the task exists instead.
[Dispatch verification](dispatch-verification.md) holds what each field establishes, what
it does not, and the classes a missing loop falls into.

### The typed form these fields travel in

The sections above say what an assignment, a correction and a return have to contain. What
makes that checkable rather than habitual is `relay-packet/1`, defined in the relay's own
`docs/relay/packets.md` and read there rather than restated here, exactly as `relay-envelope/1`
is. It sits on that same identification region and adds
the part the envelope deliberately does not know: which typed data a particular occasion
cannot do without.

Eleven occasions, where there used to be two words. Downward: assignment, revision request,
resume, receipt confirmation, acceptance, integration result. Upward: completion, review
ready, blocked, decision request, progress. Each declares its own required data, and a packet
missing any of it is refused by field name rather than accepted and discovered three rounds
later. Two of the declarations are restraints rather than requirements and both matter here:
an assignment does not carry a generation, because a child this dispatch is creating has no
registration until creation returns its task id; and a progress note carries no artifact,
because demanding a head from a child with nothing to show is how a plausible one gets
invented.

A resume is the opposite case. It must state the effective workflow, because model, effort,
sandbox and approval travel as settings a receipt reads back and the workflow has no
transport field at all - so a resume that omits it has dropped it, not deferred it. The relay
refuses a restore section that states anything else and not that.

Three shapes carry more than a word. A correction's body has five sections - violated
criterion, what changed, fix scope, preserve, reverify and return - and the correction carries
the evidence it rests on, so a correction that says nothing about what it corrects is refused
by name. The callback is an object of the task to answer and the model and effort that task
is authorized to run now, because that pair is what goes stale when the user changes a
parent's model. The policy states the execution mode (`loop`, `non_loop` or
`coordination`) beside the workflow. And a first assignment, written before the child
exists, names the dispatch request it is sent under as its relation and states its recipient
as an absence; registration is what binds both.

**What a receiver does with one.** Before acting on an assignment, a correction, a resume or
a result, the receiver runs the store-backed check against the shared state directory, naming
its own task id and its own reception ledger:

```bash
codex-session-relay --state "$RELAY_STATE" packet-check --packet <file> \
  --receiver <your task id> --ledger <your reception ledger> [--observation <file>]

# A packet that names an artifact: a correction, a resume, an acceptance, an integration
# result, or a completion or review_ready report. Write your own reading of the artifact first,
# with where and when you read it, and check with it. For a file or other deliverable:
#   {"source": "<who read it, how, when>", "artifactPath": "<path>",
#    "artifactDigest": "<its digest, in the form the packet states it>"}
# For a pull request:
#   {"source": "<who read it, how, when>", "repository": "<owner/name>", "prNumber": <n>,
#    "headSha": "<head>"}
codex-session-relay --state "$RELAY_STATE" packet-check --packet <file> \
  --receiver <your task id> --ledger <your reception ledger> --observation <your reading>

# After acting on it, and only then. No --observation here: --applied reads the packet and
# your ledger, never the store, and refuses one.
codex-session-relay --state "$RELAY_STATE" packet-check --packet <file> \
  --receiver <your task id> --ledger <your reception ledger> --applied
```

It gets one of three answers. Accepted, where every field its own store reading could answer
agreed. Refused, naming the field and both values: another parent, another child, another
relationship, a superseded or ended relationship, a stale generation, an old criteria digest,
a head that moved, another callback or a callback pair that has changed, a policy (model,
effort, sandbox or approval) or mode the receiver's reading contradicts, or a model and effort
pair the record holds as refused for this role. Or unavailable, where its reading could not
answer at all - which is neither of the
other two, because a receiver that could not check something has not checked it, and
treating that as acceptance is how an unverifiable instruction becomes an applied one. Act
only where the answer is accepted and `act` is true; report a refusal by its field, and
resolve an unavailable one by reading what was missing rather than proceeding. An artifact is
never a store fact - neither a pull request's repository, number and head nor a file's path
and digest - so it comes only from an observation file you wrote from your own reading,
naming where and when you read it, with the digest in the form the packet states it (the
check compares the two as written). A packet naming an artifact checked without one comes
back unavailable on those fields (`artifact.path` and `artifact.digest`, or the pull
request's `artifact.repository`, `artifact.number` and `artifact.headSha`): read the
artifact and check the same packet again with `--observation`. That is neither acting on
it nor a refusal to report. On a paused relationship the answer is accepted with `act` false and
`actHeld`: wait for `relationship-resume` and check the same packet again then.

A correction arriving twice is applied once. The ledger keeps each answered message id beside
the content it asked for and whether you recorded acting on it, so a repeat is answered from
today's reading with the earlier disposition beside it, while one id asking for something
different is refused as a collision. An accepted answer is not an applied one: `act` stays
true on a repeat until you record the application with `--applied` (while the relationship
is paused it is held instead, as above), so a check you made just before a restart does not
lose the instruction. If you stopped after acting but before
recording it, read your own work first; where the instruction is already in it, record it
applied instead of acting again. The ledger also keeps the mode the accepted assignment gave,
which is how a later resume or report is checked against it.

This step is the reception boundary because no relay code carries these packets: they arrive
inside prompts. The same fact is its limit. A receiver that skips the step has checked
nothing, and nothing in the relay runs the check on its behalf.

**Seven states, kept apart.** A message read, a send the transport accepted, a relay
acknowledgement, a criteria verdict, the parent's acceptance, the merge landing and the
Linear record reaching Done are seven facts with seven different records behind them, and
`read` has no record at all - nothing in the relay says a recipient read anything. A model
writing that it has reported is prose and promotes nothing. The packet document holds which
record answers each, and a state standing above one that is not held is reported as the
promotion it is. Linear Done is the issue's own status, which the relay's store never holds:
its outbox confirming a coordination summary is a different fact.

**Activation is three fields.** The invocation being in the assignment, the child having
armed it, and a host goal being active are separate facts produced by separate parties, and
the packet carries them separately so a mode with no evidence reads unverified rather than
failed. A coordination parent and an authorized non-Loop assignment answer not applicable for
the loop field by design, and neither is a finding. The reading carries the mode it was read
under, and that mode has to agree with the policy and with the mode the receiver already
holds.

## Non-PR packet

Use this reduced shape for research, design or verification without repository changes.
Keep the shared authorization, task settings and recovery rules above, and the `Processes you start:`
line of the Launch packet in `Workspace ownership:`; omit code-only
fields and OPS publication clauses. Relay-specific fields apply only when used. When using the relay, freeze the result
and its verification evidence in a file under an authorized artifact root and emit
that file; link-only completion has no manifest and is not a valid ready receipt.

```text
Task: [one stable issue ID, bounded result, existing owner]
Title: [the Codex task title: issue ID · descriptive title of up to 20 characters, following
  Child task titles]
Coordinator: [actual task/host IDs if delegated; project ID only if one exists]
Scope: [accepted question/outcome, exclusions, dependencies and write authority]
Input baseline: [source IDs, revisions/updated-at evidence and known gaps]
Workflow/settings: [effective workflow, model/effort and actual permission profile; a
  question only a person can answer goes to the parent as `blocked_needs_input` with the
  question written out, never `request_user_input`]
Language: English for everything you write, as in the launch packet
Working location: [permitted cwd/artifact roots; no invented Git repository]
Workspace ownership: [what is already present at that working location and those artifact
  roots, each entry labelled as already owned by this assignment or owned elsewhere, since
  predating the run and belonging to it are separate facts: the temporary artifacts, evidence
  roots, shared originals and running processes there. Do not adopt what is owned elsewhere.
  Then this task's own working directory and artifact roots, each with the owners OPS-5.1
  requires on this path: creator, editing owner, retention owner and cleanup authorization,
  `none automatic` where no cleanup is authorized. Git metadata ownership is inapplicable
  here, and so are checkout and branch ownership]
Resource delta to report at close: [measured against that baseline, what this task created,
  changed, retained, shared or cleaned, each with its owner, release condition and next action;
  the working directory, purpose, handle and running state of any process it started; and any
  capacity actually reclaimed, kept apart from what was only proposed]
Verification: [observable acceptance criteria and independent evidence needed]
Return: [actual task ID, result link plus delivered revision/updated-at evidence,
  or durable artifact locator plus digest; verified output snapshot if needed;
  requested/observed title or title-verification limitation;
  criterion evidence, unresolved limitations and next handoff]
Recovery: [issue-linked record or private receipt, dispatch/turn IDs and actual owner]
Relay, if used: [exact issue identity, scope reference, real coordinator/child IDs,
  state directory and authorized recipients; frozen result/evidence artifact path
  and digest under an authorized root; current generation/receipt outcome]
Stop after this issue; do not start another issue or create an empty PR.
```

## Project handoff

Use this when a supervisor hands one of its approved projects to that project's parent. It is the
brief for one project and it stops there: it carries no issue plan, no child assignment and no
review of anything below that parent. The supervisor's own procedure is
[Initiative supervision](initiative-supervision.md).

Which carrier it travels on, the first prompt of a new parent or a
[Coordination message](#coordination-message) of kind project handoff to an existing one, is chosen
in [Initiative supervision](initiative-supervision.md#hand-a-project-to-its-parent) and not
repeated here. Either way it carries the [restoration block](#restoration-block), and that block
travels whether the parent is running or idle: a task idle since its last result has usually lost
as much context as one that has been working for ten turns, and a handoff it cannot place is
answered from whatever it happens to remember. Write it in English, like every instruction that
travels between tasks.

Keep it short and point at what the existing records already hold.

```text
Initiative / Supervisor: [stable initiative ID, the revision of its record that the approved set
  and the completion boundary were fixed at, and this supervisor's task id]
Project: [stable project ID and URL, and the parent task id where one already exists]
Criteria: [the record this project's criteria were read from and its revision, naming which one it
  is, since the contribution lives in the project record or in the initiative body depending on how
  the plan was written; and this project's contribution to the initiative's finish condition.
  Issue-level criteria stay in the issues]
Scope as read: [this project's membership in the approved set, the completion boundary, and the
  designation's limits and exclusions as they bear on this project, all as they stood at the
  initiative revision above. They are stated rather than left implicit so the receiver has both
  sides of the comparison instead of only the current record and a revision it cannot read
  backwards. The limits belong here because a handoff that quietly drops one reads as ordinary
  authority to a parent that already holds it]
Prerequisites: [cross-project prerequisites by relation, each with where its verification will
  appear; any shared-target order already decided for this project, which is the supervisor's
  decision and not something the parent can infer; and the peer parents this project shares a
  surface with, to settle with directly]
Authority: [the designation and its date, the limits in force, child-creation authority, and the
  effective delivery, integration, release and deployment scope for this parent: the standing
  defaults, narrowed by every explicit limit, computed once here rather than left for the parent
  to derive. A designation silent about merging leaves the standing integration default in place,
  so that parent merges its own project's pull requests; an explicit no-merge or review-only
  designation arrives as that limit; release and deployment stay the user's unless this
  designation carries them]
Current state: [locators only: the project's coordination record, its live children, its open
  pull requests, and any outcome already verified]
Retained resources: [locators and ownership for the checkouts, branches, evidence roots,
  temporary artifacts and running processes this project's work is keeping, each with its owner,
  the reason it is retained and the next action. Locators only, as above: the coordination record
  holds the detail. A resource whose owner this supervisor could not establish is named as
  unknown rather than assigned to this parent]
Report back: [a result or blocked coordination message carrying the outcome, the evidence per
  criterion, the unresolved problems and the decisions needed. Detailed logs stay where they are]
Workflow: [the parent's effective workflow, restated because no transport carries it]
```

A handoff that was sent is not a parent bound, and a handoff that was accepted is not a project
delivered. The parent's own binding record establishes the first. Nothing here establishes the
second: a returned result can be blocked, or carry unresolved problems rather than satisfied
criteria, so delivery is the outcome verified against the completion boundary and never whichever
result happens to arrive first.

### Corroborate a project handoff

Check who sent it before acting on it. This is the one kind here that carries authority and it
travels on a channel peers use too, where the kind, the sender and the supervisor identity are all
text the sender wrote, and no bundled store enforces these levels
([OPS-7.4](operations.md#ops-74-three-levels-and-their-routing-identity)). The receiving parent
matches the claimed supervisor against what it already holds: its own record, and the initiative's
record read directly rather than quoted back to it inside the message, since a quotation proves
only what the sender wrote.

A project record naming no supervisor is the ordinary state of a project that predates the
supervision, and it is not a disagreement. The initiative's own record settles that case: it is the
supervisor's record to write while the project record is the parent's, so a claim that record
confirms is accepted as the first handoff, and the parent then records the relationship in its own
record. A claim it does not confirm stays a peer request. Where the parent cannot read that record
itself, it does not accept a first handoff on the sender's word: it raises it, because the whole
weight of this case rests on a record the receiver read rather than on text the sender supplied.

Identity is half of it, because a handoff naming the right supervisor can still carry the wrong
scope, by forgery, by mistake, or by arriving after the scope moved. The same read settles the
rest: that the initiative named is the one whose record this is, that this parent's own project is
in that initiative's approved set, and that the membership, the completion boundary and the
designation's limits and exclusions it states it was written from are the ones the record carries
now. The handoff states them with the revision it read them at, so the receiver has each side of
the comparison; the test is those values and not revision equality, since an authorized edit
elsewhere in the initiative moves the revision without moving them, and refusing a handoff over
that would turn ordinary record-keeping into a stall. Where those values have moved, the handoff is
out of date, and what it needs is the scope decision rather than a refusal.

The contribution is read from whichever record owns it for this project, and the receiver decides
which that is rather than the sender: the project record where it carries one, and the initiative
body where it does not. The handoff names the source it was written from, but that is context for
the comparison and not the choice of oracle, because a sender free to name its own source can
always name the one its value matches. Where the named source is not the owning one, or the two
records disagree about this project, that is a discrepancy raised rather than something the message
settles, and a handoff naming no source at all is a proposal.

The limits are compared for the same reason as the rest: a handoff that omits a narrowing the
designation made, by forgery, mistake or age, reads as ordinary authority to a parent that already
holds it, and the later refusal only catches authority being widened. For the same reason the
handoff's two statements of those limits are compared with each other: Authority says what this
parent may do and Scope as read says what the record said, so a handoff whose Authority is wider
than its own Scope as read is inconsistent on its face, and it is refused and raised rather than
followed at whichever of the two is more convenient.

The prerequisites and the shared-target order are read the same way and in both directions, because
these are the supervisor's to set and need not appear anywhere in this parent's own baseline. One
the handoff states and the record does not carry is unconfirmed, and work depending on it does not
start on the message's word. One the record carries and the handoff omits matters more, since
omission is how a stale or altered handoff removes a blocker: the comparison is against the
complete set the record holds for this project rather than against what the message happened to
include, and a handoff missing any of it is out of date and is raised. The peer parents and shared
surfaces named in the same field are read the same way too, because that field asks this parent to
open contact: a peer it cannot corroborate from a record it read itself is a proposal rather than
an instruction, and it raises that instead of writing project context to a task outside its scope.

Where any of those disagree it answers the message as a peer request to be decided rather than as
an instruction, and says so in the reply. One parent cannot assign work to another, so a handoff
whose sender cannot be confirmed as this project's supervisor is not a handoff.

That comparison narrows mistakes rather than defeating a forgery, and the difference is worth
stating. The reason is the one the [Coordination message](#coordination-message) rule gives for
every kind here, that nothing in a message is evidence of itself, so a sender willing to write
another task's identity into it passes this check. What keeps
that from becoming an escalation is that a handoff directs work and never widens authority: the
parent's own limits, permissions and bindings are what bound what it can do, and no delivery
widens a recipient's permissions to make itself succeed
([OPS-7.3](operations.md#ops-73-isolation-between-parents)). A handoff that appears to grant merge,
release or deployment authority this parent did not already hold is therefore the one to refuse and
raise, whoever it claims to be from. Authenticating the sender itself needs a transport that
carries caller identity; that is a property of an installation rather than of this instruction, and
it is recorded here as unmeasured rather than assumed. What a forged handoff can still do is worth
naming exactly instead of leaving to inference: a peer that can read the same records can copy
every corroborated value and pass this check, and while it cannot widen this parent's authority or
reach another parent's children, it can misdirect which of this project's authorized work happens
and when. Closing that needs a store recording the relationship and a transport carrying caller
identity, which is the registration work rather than this entry. Until then a parent that finds two
handoffs disagreeing, or one it cannot corroborate, raises it rather than choosing between them.

## Coordination message

Use this between parents coordinating directly, between two supervisors coordinating across their
initiatives, for a parent's escalation to its supervisor, for a supervisor's decision returning to
them, and for a supervisor handing one of its approved projects to that project's own parent. It
carries coordination between owners; it is never a route into anyone else's children.
It is not a delivery channel: it carries no receipt, no acknowledgement and no verdict, and it
never instructs another parent's child. The handoff is the one kind here that assigns anything, and
even it assigns only a project to the parent that owns it; that parent binding the project and
returning its result are separate facts the message does not establish. The path and the rules it follows are
[Direct coordination between parents](../../crw-plan/references/integrations.md#direct-coordination-between-parents).

Keep it short. Name what identifies this message, and reference what the existing relationship
already holds instead of recopying it, exactly as the restoration block carries pointers rather
than contents.

What identifies it, and what the recipient owes because it arrived, are the shared form rather
than this file's invention:
[the message both relations are read by](../../crw-plan/references/integrations.md#the-message-both-relations-are-read-by),
implemented as `relay-envelope/1`. Take the words from there - request, notification, decision,
status response - so a parent and a supervisor mean the same thing by them, and keep the five
delivery states apart in the same way.

One rule covers every value that decides what a receiver does, and it is worth stating once here
rather than per kind. These transports carry opaque text and no authenticated caller identity, so
nothing in a message is evidence of itself. Who sent it, the scope it names, a decision returned on
an escalation, a correction, and the pointers a restoration block supplies are each corroborated
against a record the receiver reads itself; where one cannot be, it is a proposal the receiver
decides on rather than an instruction it follows. A supervisor's later decision carries exactly as
much authority as its first handoff and exactly as little proof, so it is corroborated the same
way. A restoration block is read as a locator for records the receiver then reads, never as their
contents: that is exactly what lets a task which has lost its context find its own record again, so
a locator is followed rather than refused for being unfamiliar. What the receiver checks is the
record it finds there, which has to be its own and name this task and this assignment. A locator
leading to a record belonging to somebody else, or to none, is refused and raised, and so is a
block whose stated workflow, child or pull request the records it points at do not bear out. The
block never supplies those values; it says where to look for them.

A repeat is not a second instruction. The receiver keeps each request beside the disposition it
gave, keyed on the corroborated sender and scope together with the id rather than on the id alone,
because senders choose their own ids and two peers can easily pick the same one. A message arriving
again under a key already answered is answered with that same disposition rather than acted on
twice. One that repeats a key while carrying different content is neither a replay nor a new
instruction but a collision, and it is raised rather than silently given the earlier answer, which
is also what stops a predictable id from being spent in advance to suppress the real request.

A message the receiver could not corroborate has no corroborated sender to key on, and it is still
decided, so it is keyed on what the receiver can determine by itself: the identity the message
actually arrived under, together with its id. That key is fixed when the proposal is first decided
and does not move afterwards, because a sender that becomes corroborated later would otherwise hand
a retry a fresh key and a second application of a decision already made. That is what makes an uncertain send safe to reconcile by
asking rather than by sending again, and it is why a replayed handoff, correction or decision
cannot restart work that already ran.

```text
Request: [id the sender chose for this message]
Reply to: [on a reply, the id it answers; omit on a first message]
Kind: [proposal | acceptance | conditional acceptance | rejection | correction | result | blocked |
  merge turn request | merge turn assignment | merge turn return | recovery update |
  project handoff.
  The three merge-turn kinds are about the order into a shared target and nothing else: no kind
  here lets one parent assign work to another, because no parent can. Project handoff is the one
  downward assignment, it belongs to a supervisor alone, and only that project's own parent
  receives it]
From / To: [each side's role, task id and Linear scope]
Scope: [the issues, files, interfaces or behaviour this is about, and the base revision]
Asking: [the action or decision required, or the decision being returned]
Because: [where the reason lives: the finding, the pull request, the criterion, the receipt]
Next: [who owns the next step, and what would settle it]
```

A worked pair. The long values stay as references, and the reply is conditional, so it is recorded
as conditional rather than as evidence that anything was applied:

```text
Request: shared-surface-1
Kind: proposal
From / To: parent of project A, task 01a0...a1; to parent of project B, task 01a0...b7
Scope: A's CRW-127 and B's CRW-131 both edit references/operations.md; base dev 89c2c58
Asking: B holds OPS-7 until A's clause lands, and A leaves OPS-8 untouched
Because: overlapping edit surfaces recorded in both projects' coordination records
Next: B, to accept or to name its own constraint

Request: shared-surface-1-r1
Reply to: shared-surface-1
Kind: conditional acceptance
From / To: parent of project B, task 01a0...b7; to parent of project A, task 01a0...a1
Scope: same two issues, same base revision
Asking: nothing yet; accepted on the condition that A lands before B's own review opens
Because: B's child already has a branch at that base
Next: A, to report its landing. Until then this is conditional and is not applied evidence
```

The record of what happened next belongs in the coordination record below, not in another message:
B's parent instructing its own child, that child's change, and the verification are three further
facts, and none of them follows from this reply.

### What a result returns

A `result` says what now holds and where to check it, and it stays short because the receiver
reads the records itself. Five values are what the next decision needs, and the template above
already has a place for each. The **result**, a line or two of what is true now that was not
before, and the **artifacts** at the revision they are at, a pull request with its head or a
locator with its digest, go in `Scope` and `Asking`. The **per-criterion verification** goes in
`Because` as the disposition each accepted criterion already carries and the record holding it,
named as a claim the receiver corroborates against that record rather than as a verdict this
message issues. The **unresolved problems**, a failure, a missing observation or a conflict with
another scope, go in `Asking` beside the result. The **decisions the receiver owns** go in `Next`,
and a proposed Linear record change is one of them: it travels under
[record writes and returned proposals](../../crw-plan/references/integrations.md#record-writes-and-returned-proposals)
and the sender has not applied it.

The detail behind those values stays where it already is. A child returns its issue-level delivery
in the launch packet's return list above, and this shape is what its parent condenses when the
result travels a level up. A receiver accepting a result reads the records it names rather than
repeating the investigation or verification behind them at the same revision and criteria set, and
reads further exactly where a criterion is unmet, an artifact has moved, or the evidence it needs
is not there. Condensing never costs an accepted criterion or the independent review behind one: a
result short enough to hide an unmet criterion is raised rather than accepted.

When the result condenses a child's pull request delivery, none of its disclosures is dropped on the
way up ([what a handoff discloses](#what-a-handoff-discloses)): a decision request still open is an
unresolved problem and goes in `Asking`, the reviewed head and the base refresh kinds go with the
artifact's head in `Scope`, and a sibling impact that needs another scope's work is a conflict with
another scope, which `Asking` already names.

```text
Request: shared-surface-1-r2
Reply to: shared-surface-1
Kind: result
From / To: parent of project A, task 01a0...a1; to parent of project B, task 01a0...b7
Scope: A's CRW-127 landed in dev at 3f9a1c2; same two issues and the same shared surface
Asking: nothing to decide. OPS-8 is untouched as agreed, and one criterion is unverified because
  installation was never observed
Because: CRW-127's per-criterion record, and PR #142's checks on the head that landed
Next: B, whose condition is now met, to open its own review
```

### What a handoff discloses

This defines the `disclosures` member of the handoff file that the `Return:` bullet for a
merge-readiness handoff names. The child writes it and its receipt carries it; the parent's
[verdict](merge-readiness.md#what-the-handoff-discloses-checked-at-the-verdict) reads it.

The handoff records what the checks and the hosted review found on the head. It cannot show what
became of the child's own internal review, whether that review finished before the pull request was
opened, what kind of base refresh the branch went through, or what the change touches that a sibling
issue also touches. Each of those has gone unseen before: of the three defects one analysis found a
parent sending back, two had been rated High by the child's own review and rejected without a trace
in the receipt. An internal finding has no pull request thread, so the thread coverage never sees
it, and this section is the only record it has.

`disclosures` is a top-level key of the handoff file, beside `handoff` and not inside it. The relay
reads only the keys of the record it grades and carries the file as an artifact without parsing it,
so nothing in the relay checks this section; the parent's verdict does. Every item is stated:
`none`, or `ran: false` for a review that did not run, is an answer, and an item left out reads as
not checked.

```json
{"handoff": {"...": "the record merge-evidence produced"},
 "disclosures": {
   "internalReview": {"ran": true, "reviewedHead": "<sha>",
     "findings": [{"id": "r1", "summary": "...", "disposition": "rebutted", "evidence": "..."}],
     "commitsAfter": [{"sha": "<sha>", "cause": "hosted-review fix", "note": "..."}]},
   "decisionRequests": [{"findingId": "r1", "effect": "...", "why": "...", "proposal": "..."}],
   "baseRefresh": [{"kind": "clean", "previous": "<sha>", "head": "<sha>", "merged": "<sha>",
     "trees": {"expected": "<tree>", "actual": "<tree>"}}],
   "changedPaths": {"declared": ["..."], "changed": ["..."], "outside": []},
   "siblingImpact": {"sharedInterfaces": [], "registryEntries": [], "siblingWork": []}}}
```

- **`internalReview`**: whether an independent review ran inside this task (`ran`) and the head it
  covered (`reviewedHead`: the latest head such that the independent reviews so far cover every
  change up to it. A later generation that reviews its correction on top of the earlier review names
  the new head. A check limited to some hunks or commits, such as the independent check of a manual
  refresh's resolved hunks, is recorded with its entry and does not move it. A task whose reviews so
  far cover no head completely, because none ran or only checks limited to some hunks ran, has no
  `reviewedHead`: it omits the field, `ran` says whether any independent check ran, and its entries
  start at the assignment's baseline commit). For every finding it rated High or blocker, or the top
  tier of whatever scale it used, one entry with the `id`, a one-line summary and exactly one
  disposition: `applied` with the commit that applied it, `rebutted` with the evidence that it is
  not a defect, or `out_of_scope` with the boundary that excludes it and where it goes instead. A
  review that ran and raised no such finding says so, and a task that ran none says `ran: false`.
  The internal independent review ends on the head the pull request is opened from and the hosted
  review follows on the open pull request, so `commitsAfter` lists what the review did not see: the
  commits after `reviewedHead` on the branch's first-parent line that are not merges of the base,
  each with its cause, such as a hosted-review fix or a digest re-record. Where `reviewedHead` is
  omitted, `commitsAfter` still lists any merge that is not a merge of the base. Whether and how
  often a review runs is its workflow's decision and not this procedure's; this item only makes what
  happened visible.

- **`decisionRequests`**: every finding above whose disposition is `rebutted` or `out_of_scope`,
  unless the parent has already ruled on it, in which case the entry names that ruling. A rejection
  is the parent's decision, so the child does not close such a finding on its own authority. Each
  entry gives the finding, the child's reading of its effect, why it is rejected and what the child
  proposes, which is the exchange [conditional
  acceptance](merge-readiness.md#conditional-acceptance-and-what-recording-one-costs) already holds
  before a handoff and not a second path. Where the answer decides what the child builds next, it
  asks before handing over: the turn ends `blocked_needs_input` with a blocked receipt over a file
  that carries the request. Otherwise it hands over with the list and the verdict answers each
  entry. The review's label only selects what is listed; the parent classifies each finding by
  [impact](merge-readiness.md#judge-a-finding-by-its-impact), so a High that falls in a blocking
  class is a fix whatever the child concluded.

- **`baseRefresh`**: one entry for each merge of the base on the first-parent line after
  `reviewedHead` (since the assignment's baseline commit where there is no `reviewedHead`), whoever
  made it and in whichever generation. The window runs through every generation, so an entry stays
  in the list while its merge lies after `reviewedHead` and is dropped once a later review has
  covered it. A merge the parent made with the forge's update-branch call is an entry too, which the
  child checks with the same helper. An entry gives the `kind` below, the merge's first parent
  (`previous`), the merge commit (`head`) and its second parent, the base commit merged (`merged`,
  in full). Consecutive merges are consecutive entries, each starting at its own first parent. A
  commit made after a merge, such as a digest re-record, is not part of the entry: it is listed in
  `commitsAfter` with its cause, so the merge head and the final head are two facts. A branch with
  no merge of the base after the review says `[]`.

- **`changedPaths`**: where the assignment declares edit regions, the paths the branch changed, from
  `git diff --name-only origin/dev...<head>` (the merge-base form, so merging the base does not show
  a sibling's files as this branch's), each placed in a region the assignment declared, in a file it
  names as an expected overlap, or outside every region, with the paths outside and their count. A
  region declared for part of a file, a section of it for example, is invisible to a path
  comparison, so for such a file the entry lists the hunk ranges changed
  (`git diff -U0 origin/dev...<head> -- <path>`) and places each in or outside the section. Where
  the assignment declares no regions the item says so.

- **`siblingImpact`**: the shared interfaces the change altered (a command, a field or a function
  the work of another issue may call); the registry entries it added (rows of a table, ids, list
  entries, version lines, which are the entries a union check keeps when two siblings both add to
  one list); and any finding that needs a sibling's work to change, naming the sibling by pull
  request or title. The child does not message or coordinate with a sibling about it; the parent
  routes it.

The kinds of base refresh, and what reruns for each, which is all that a refresh asks:

| Kind | The entry shows | What reruns, and nothing more |
|---|---|---|
| `clean` | the merge's expected and actual trees: the tree of `git merge-tree --write-tree <previous> <merged>` and the tree of the merge commit, which are equal | the gates: the repository's local checks for the paths now in the branch and a digest re-record where the merge touched the plugin, then every required job and the threads on the final head, which [OPS-9.4](operations.md#ops-94-a-new-head-invalidates-the-review-it-outran) reads again on any new head. No review of the change and no audit of the plan |
| `mechanical` | each resolved hunk with the rule the assignment names for that overlap (its wording or identifier), its path and lines, and the check that reproduces it with its output (for a union or a regeneration, `crw skill base-refresh mechanical` run on the merge) | the gates, and the deterministic checks that read those hunks: that check, the repository's validators and link check, the digest re-record, `git diff --check`. No model review |
| `manual` | each hand-resolved hunk with its path and lines, and the independent check of those hunks | the gates, and an independent check of the hand-resolved hunks only, run by the child's own independent reviewer on those hunks and what they merge. The rest of the diff is not reviewed again |

A named rule is one the assignment or the restoration block states for that overlap, such as keeping
both sides' rows or lines, renumbering a clashing id, or re-recording a digest last. A rule the
child chose itself is not one, and a hunk no named rule covers is `manual`; a merge with both kinds
of hunk is `manual`. The hunks an entry lists are exactly the ones `git show --remerge-diff <head>`
prints for that merge (git 2.36 or newer): it prints nothing for a clean merge, and it prints an
unlisted resolution, or an edit riding in the merge, as a hunk.

A base refresh is not new scope. The kind says how much of it is checked again and does not say what
a failing gate means: a job that fails on the final head is a defect like any other and is fixed.
The parent refreshes a candidate itself when only the base moved ([Refresh the base yourself when
only the base moved](merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved)), and a
refresh the parent made is `clean` by the same measure, so the two rules draw one line: a merge of
the base with no conflict and no hand-resolved hunk reruns the gates, whoever merged it. A refresh the parent made by
settling a conflict with a declared mechanical rule is `mechanical` in the same way: it carries the output of `crw skill
base-refresh mechanical` ([Resolve a mechanical conflict
yourself](merge-readiness.md#resolve-a-mechanical-conflict-yourself)). What still goes back to the child, a conflict the
parent does not settle by such a rule, a base that moved after `dag-accept`, or an installed runtime without the helper,
arrives as a correction for the refresh alone, and the child's own merge of the base before a handoff is the same act.

## Restoration block

These messages carry this block: a needs-changes correction, a review fix, a resume the coordinator
publishes after handling something on the task's behalf, a restart after that task was compacted,
a project handoff into an existing parent whether it is running or idle, and a designation
delivered to an existing supervisor. Each of them asks a task to pick work back up, and each has a
sender holding assignment facts the recipient may no longer have. Ordinary coordination between
peers is not in that set: a proposal or an acceptance passes between owners who each keep their
own record, and the sender holds neither the recipient's workflow nor its durable locators to
restate. The first work
prompt stated the assignment once. Ten turns, one compaction and three review rounds
later, none of it is reliably still in the task's context, and a correction that
assumes otherwise is answered from whatever the task still happens to remember.

Keep it short. It restates what the COORDINATOR holds and what the task cannot
reconstruct alone. The list below is written for a task bound to an issue; a block travelling to a
parent or to a supervisor carries the same kinds of fact at that level, as the paragraph after it
says, so read the level first and the fields second:

- The skills this task runs under, as pointers to the installed skill, not their text.
  On context loss the task re-reads the owning skill from those pointers; it does not
  reload every skill it once had, and an unrelated reference is not part of recovery.
- The effective workflow, restated. A transport carries model and effort as settings
  and has no field for the workflow, so a send that omits it has silently dropped it. Where that
  workflow is CXC Loop, the restatement carries the rule that a correction generation, a
  base-refresh generation and a separated publication step are not the first work-phase of new
  work, so they do not open `LOOP-DOCS-FIRST-01`'s docs-only cycle; CXC's own rule, which a task
  that lost its first assignment is left with, counts work-phases and not what a phase does.
- The language the task writes in, restated: English for an issue child under
  [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution),
  or the explicit override its assignment carried. Like the workflow it has no transport field,
  and a child that lost its first assignment answers a review in whatever language it drifts to.
- The publication scope and the escalation route, restated. Like the workflow and the language
  they have no transport field, and a child that compacted away its first assignment is left with
  CXC's own rules, which say never to push without approval and point a question at the
  user. Say whether its publication scope is still the explicit push approval
  `DEV-GIT-PUSH-01` requires (push its task branch, update the pull request, never merge, nothing
  wider) or that it is not, and that a question only a person can answer goes to the parent as
  `blocked_needs_input` with the question written out. These are pointers to
  [Default dev integration](../../crw-plan/references/integrations.md#default-dev-integration),
  not a copy of it.
- Assignment identity: the issue, this task's own id, and where a relay holds the
  assignment its relationship id and the generation to emit under, read from the
  assignment rather than copied from the coordinator's own state. On a needs-changes
  correction that is the generation the verdict opens and not the one being superseded,
  because the verdict is what opens it: a block composed beforehand that names the
  current generation names the one the child has just stopped working in, and a receipt
  emitted under it is refused. The relay carries the superseded event and its digest
  itself, so the block does not repeat them.
- The delivery artifact as it stands now: pull request URL, base and head, and which
  required checks and reviews are outstanding on that head. Include the current
  [reviewer policy](merge-readiness.md#reviewer-policy) when it changed;
  supersede stale review-wait instructions without discarding unresolved findings.
- When the parent updated the branch itself after the child's report
  ([refreshing the base](merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved)),
  the head named above is the parent's, and the child's local worktree is behind its remote
  branch. Say so: name the remote head and the head the child reported, and make the first
  action of the generation to fetch and fast-forward the local branch to that remote head
  (`git fetch origin`, then `git merge --ff-only origin/<branch>`) before reading, editing or
  running anything. A local branch that cannot fast-forward holds commits the remote does not
  have: they are merged with the remote head and reported, never reset, rebased or pushed over
  the parent's merge. The child's next push is then built on the parent's head, so it keeps the
  update instead of repeating it.
- The siblings' landings and the conflicts the parent expects, stated as relay facts: which pull
  requests of this project's other issues have landed in the base since the head the task holds
  (pull request, merge commit, and the record each was read from, such as the relay's landing
  record, `merge-turn-show`, or an assignment's `merged` mark, or the forge where the relay holds
  none), and each conflict the parent expects when the task merges the base: the path, the sibling's
  pull request and the resolution rule the assignment names for that overlap. A fact the parent
  could not establish is named as not established and is never filled in, and where nothing landed
  the line says so, with the time it was read. Without it a correction that says "merge the base"
  leaves the task to guess what the base now contains, and a task that guesses audits again to be
  safe. When the correction is a base refresh, this bullet also says that it is not new scope and
  points at the [kinds and what reruns for each](#what-a-handoff-discloses): the receipt names the
  kind.
- The unresolved findings, each with what would settle it.
- The single next action this message is asking for.
- Durable locators for the work the task itself owns: where its plan, its ledger and its
  evidence live, with the identifiers and current revisions the coordinator already holds.
  These are pointers to that task's own records rather than copies of them, and they are what
  lets a task that lost its context find its record instead of starting a second one.
- The host each task identifier belongs to, where the interface supplied it or it was
  independently established, carried beside the ids for routing and audit rather than as part of
  them and named as unestablished where neither happened. An issue, a project and a pull request
  are identified without one.
- When the sender observed each of these facts. They are observations with a time, so a recipient
  can tell which lines are current and which were carried forward before it acts on them.
- The observation each setting was read at, so the recipient compares the values against its own
  host instead of adopting them, and settles an unsupported one under
  [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution).
- Where the block travels to a parent rather than to an issue child, the recorded `run_mode` and
  `observation_path` — spelled as the literals
  [Start policy](start-policy.md#what-is-settled-and-what-is-recorded-with-it) declares, because a
  paraphrase will not restore — with the readiness facts behind an `event-driven-idle` one, and the event
  state still outstanding: which relationships are owed a result, the last generation and revision
  applied, and any delivery observed holding terminally. A goal-free parent that ended its turn
  because only waiting remained cannot tell, on restart, whether that was a decision or an
  interruption, and these are the fields that answer it. Without them the parent either resumes
  work that already has an owner or waits for an event that was already handled.

What the block carries about those records is their location and identity, never their
contents. The plan, the ledger, the phase and the goal belong to the task and to its own
workflow skills, they are durable on that task's side, and this repository deliberately
does not define their shape
([OPS-10.3](operations.md#ops-103-where-the-packet-and-report-formats-are-defined)).
Pointing at a record is not defining it; rewriting one is. A coordinator that reconstructs
a child's plan from its own view has replaced that child's record with a guess, and the
child will trust the guess over the record it could have re-read.

At the other two levels those bullets read across as follows. For a parent: its project, the criteria
revision in force, the locator of its coordination record, the children and pull requests still
outstanding, and the one next action. For a supervisor: the initiative, the revision its approved
set was fixed at, the locator of its supervision record, the handoffs and results still
outstanding, and the one next action. A sender that does not hold a field at the recipient's level
omits it and says which it omitted, rather than inventing one; the issue-shaped fields are not
filled in with a project or an initiative to make the shape match.

The block is context for resuming work already authorized. It requests no new approval,
asks for no readiness-only turn spent confirming receipt, and re-opens nothing the
assignment already settled.

Where a relay holds the assignment there is no second channel to put it on: the
needs-changes verdict is the correction, so the block travels in that verdict's own
findings and notes. Compose it before recording the verdict, because afterwards the
only remaining routes are the ones this workflow forbids, and write the generation the
verdict is about to open rather than the one still current as you write. Those two are
never the same on a correction, and the child acts on the one it was given.

Declare which finding carries it, and put it in the first one. The declaration is what
makes a failure visible; the position is what makes failure unlikely, and the two are
different jobs. A relay that recognises the declaration refuses a correction it cannot
carry BEFORE that correction opens the next generation, so the transaction the coordinator
used to be unable to get back to is one it can now simply not enter;
[codex-session-relay](relay.md#the-parent-verifies) records the flag and the outcomes it
names.

What that establishes is what the relay will put in the bytes, which is a different claim
from the child having read them, so delivery of the block still needs evidence rather than
following from placement. Confirm it from a dispatched attempt and what the child actually
received, never from a queued rendering, which is the bytes a next attempt would send rather
than proof of a send. A relay that records what each attempt froze reports that as
`restoration_attempted` beside the bytes, which is the one measurement about a send rather
than about the next one. Where the block did not arrive, say so plainly: there is still no
supported way to send it again, because the verdict does not resend and the parallel route
stays forbidden. Record it as an undelivered correction on that assignment and hand it to the
coordinator, whose decision it is: to let the child proceed on the context it has, or to
change the relay. Do not invent a transport to close the gap, and do not describe that
correction as delivered. Whether an installed relay recognises a declared restoration block
at all is a property of that installation, and it is asked rather than assumed: the command
either offers the declaration or rejects it as unknown, which is what
[codex-session-relay](relay.md#the-parent-verifies) tells you to check before relying on it.
A version cannot answer, since it stays the same while contents change, so `unmeasured`
belongs to an installation nobody asked, not to every installation but one.

## Coordination record

Use the project's linked canonical Linear coordination document as part of the
management assignment. For a standalone issue, use its existing linked document
or an owned section in that issue and private issue/task recovery receipts. Where this task
supervises an initiative, its record is the initiative's own, under the
[initiative body standard](../../crw-plan/references/integrations.md#initiative-body-standard)
with its comments and updates, kept against the stable initiative link: a multi-project initiative
has no one project document, and supervision state left in a contributing project's record is
both outside that project's scope and somewhere recovery will not look. A
project binding is optional; the actual coordinator identity remains required
when delegating or routing relay delivery. Follow [Integrations](../../crw-plan/references/integrations.md#completion-follow-up-in-an-existing-execution-workflow).
For explicit read-only scope or unavailable access, return an unsynced update;
retain private recovery receipts so an interrupted task can still be reconciled. Record only what is
needed to resume:

- Coordinator task ID and fixed project or standalone issue link.
- Each implementation issue's one current PR, repository, and integration target;
  retain superseded PR links as history. Record non-PR results separately.
- Each task's scope, dependency edges, overlap decisions, and code baseline SHA
  or non-PR source revision.
- Per re-evaluation pass: what triggered it, the store identity and readability each read
  answered at, and per candidate the decision from
  [Decisions and what clears them](reevaluation.md#decisions-and-what-clears-them) with the
  condition that clears it and the row it rests on. The latest pass replaces the previous one;
  this is the outstanding-items field a checkpoint reads, not a growing transcript. Raw reader
  payloads stay in private receipts.
- Where a relay holds the assignment: relationship id, current generation, current
  revision, assignment state, and the synchronisation jobs still owed. Beside each, the last
  generation and revision this parent actually applied, which advances only behind a durable
  apply, and any delivery observed holding terminally, which is the one state no retry revisits.
- The recorded start policy this run is operating under: `run_mode` and `observation_path` with
  the pairing they form, and for `event-driven-idle` the readiness facts behind it or the one that
  was missing. This is what a parent waking from idle reads before it does anything else, and what
  a restart reads to know whether ending the turn was a decision or an interruption.
- What remains outstanding at the moment the turn ends: the scope not yet dispatched, the
  relationships and children still owed a result, the unresolved verdicts, and the PR and head each
  one stands on. A parent that ends its turn when only waiting remains is relying on this record
  entirely, so a field left stale here is indistinguishable from work nobody owes.
- Per direct agreement with a peer parent: the request and reply ids, the counterpart parent and
  its project, the agreed area and base revision, whether the agreement is still conditional and
  on what, and whether a follow-up owner has explicitly accepted. Beside it keep what is still
  open, because that is what recovery reads first: each request sent and not yet answered with
  what would settle it, the revision the agreement stands on, who owns the next step, and each
  follow-up still marked unassigned, raised as a blocker where it is a required dependency. For a
  condition this project accepted, record the child that was instructed and the revision where the
  change took effect, which is the adoption evidence the peer is owed.
- Where this task supervises an initiative: the stable initiative ID with the body revision its
  finish condition was read at, the approved project set with each project's parent task id and
  observed state, the handoff request id sent to each parent and the last result id returned, the
  decisions still owed upward, and the limits the designation imposed. See
  [Initiative supervision](initiative-supervision.md).
- Actual worktree/branch ownership and how local-only prerequisites are preserved.
- Per mutation: stable request ID, actual task/host/turn IDs, receipt location.
- Independent task creation/reuse route, authorization source, and verified
  responsible owner; keep internal-subagent receipts distinct.
- Requested/observed child title; retain stable task IDs across title changes.
- Requested/actual settings and independent fields for launch, loop, delivery,
  verification, integration, and deployment evidence.
- Last observed status/cursor, final commit, acceptance evidence, and next action.
- For integration: candidate base/head and landed revisions, CI attempt links,
  review sources/coverage, and finding dispositions per [Merge readiness](merge-readiness.md).
- The current temporary target, where this task was asked to handle another project or issue: its
  stable ID, the request that asked for it, and the limit that request carried, recorded beside the
  fixed binding and never in its place. The binding rule is
  [Resolve the project target](../../crw-plan/references/integrations.md#resolve-the-project-target);
  what the record owes recovery is the evidence that this target was the temporary one.
- Per state line, when it was observed, whether this turn refreshed it or carried it forward, and
  which register it was read from; and beside each task identifier this record keeps, the host
  where the interface supplied it or it was independently established, recorded as unknown where
  neither happened. A restart composes its restatement and any restoration block from this record
  rather than from context it no longer has, so a field the record never held is one the next
  sender omits and names as unestablished rather than guessing.
- Where management moved: the outgoing task, the incoming task, what transferred and what did not,
  and the revision it took effect at. A handoff nobody recorded leaves the next recovery reading two
  owners for one project.

Do not create a new database, daemon, or competing local planning document to hold
this table. Each worker writes its own reproducible implementation evidence; the
coordinator writes the authorized Linear summary and verifies it by reading it
back. Keep exact prompts/receipts in private evidence when they contain personal
data, and publish only the necessary sanitized summary.

For diff-only delivery, freeze a complete in-scope diff/file bundle against the
recorded baseline, including added or untracked files needed to reproduce it.
Store its SHA-256 and keep it unchanged while reviewed; a later edit is a new
artifact and needs its own evidence. A working-directory path alone is not a
stable revision.

The coordinator's review compares the exact final commit or hashed diff bundle
against the task's declared baseline and acceptance criteria. A new tip or changed dependency can
invalidate earlier proof. Record findings without silently rewriting the task's
acceptance criteria to make it pass.
