
## The project lane

`internal/flow/workflows/project-lane.js` is the built-in workflow that runs the issues of a milestone (`args`: `plan`, `milestone`,
and `cap`, the executors at once, default 3). Its steps, in order: `crw.milestone` reads the nodes of the milestone in the order the
scheduler offers them; in the phase `explore`, the run samples one design head, the integration tip of the run's plan (`crw.designHead`, recorded once,
so a resume reads the same head): every background explorer, the architect's first call and every call of the first DESIGN's audit loop (the open
questions' explorers, design-answers, the critic and its retry, design-fix) name it as their `head`, so they read one commit whatever lands on the target meanwhile, the architect's checkout, and so the head `r0` records the DESIGN ran at, is made at it, and the explorer's packet
(`READ AT:`) and the architect's `BACKGROUND FINDINGS` name it; before the architect's first DESIGN, one background question for each node goes to an
explorer (`crw.explore('background', ...)`, CRW-1466): the question names the node's id, its issue key and its title, its `files_needed` are the
node's declared regions (none declared: not named), and `crw.exploreRoute` spawns it (`spawned_background`) or reuses an answer of the same
digest the run has, never inline, never to the owner thread and outside the open questions' cap of the milestone; the explorer calls are
labelled `explore-bg:<milestone>:<digest12>` (an `explore:` reader never takes one for its own), their route rows carry `inputs.kind`
`background`, and a re-plan has no such phase. In every kind of `crw.explore` (background, open questions, research, evidence;
CRW-1571) an explorer whose answer is `null_model` is asked once more on a fresh thread, with the same packet, schema, node and head,
labelled with `:retry` in a join of its own after its wave (no second route row, no spawn decision, no cost to the open questions' cap;
its tokens count into the design loop's); a question whose explorer is `null_model` twice is missing: its answer stays empty with no call
in the `crw.exploreRecord` row, as before, and the answer `crw.explore` returns names both calls (`missing`, a member the record is never
given), so the call goes on without it; the call has failed only when every explorer it spawned is missing (its failure is the last
retry), and any other status that did not complete (held, unknown, superseded, refused, a thrown call) is not asked again and is the
failure, as before. A background call that failed halts the milestone (`crw.milestoneHalt`, source `explorer`), and a
missing background exploration is named in the architect's packet under `MISSING EXPLORATIONS`, never among the findings; then, in the phase `design`, the architect answers `DESIGN` once for the milestone (SP-ARCH-1) over a packet that carries the
background answers as `BACKGROUND FINDINGS`, each by its id, question, answer and the explorer's findings and never by its route (the packet is the
one it was before CRW-1466 when none is given); the open questions of the DESIGN go to
explorers (SP-EXPL-1), and the ones whose scope carries an out-of-authority notation, or beyond the explorer cap, to the owner thread
thread in a non-blocking `design_question` escalation: an authority question in the audit round that routed it, approved or not, a
question beyond the cap once the DESIGN is approved; the escalation's evidence names the question digests it asked, so a later round
or a re-plan does not ask one again on its own (`asked_in_run`); an explorer's answer is reused for the same question at the same head only: the explorers
of a re-plan read the head it pins, and the digest of an open question asked there is made of its text and that head (the first DESIGN's open questions
are read at its design head and keep the digest of their text: every answer of that loop reads the one head), so a question of the first DESIGN,
or of a re-plan at another head, goes to an explorer again and counts against the cap, while a question that goes to the owner thread keeps the digest
of its text, so it is asked once in the run and its answer holds at every head; because only the latest design ask of a run is current for `crw flow answer`,
an ask carries the questions of the DESIGN the run asked already that no answer has reached, and the questions the run asked that no answer has reached and the
DESIGN no longer raises (`carried` of `crw.exploreRoute`, CRW-1381), and an answer of the owner thread applied to it is the answer of the question from the
next round on (`answered_by_orchestrator`): the part `crw flow answer --answer-for DIGEST=TEXT` gave the question when the ask joins several,
else the whole answer; a reused answer names the route it came from (`origin`, written only with a value); the first explorer of the first
DESIGN's first audit round is also given the commands of the done conditions of the milestone's issues (`end_condition` of a node, read
from the body the handoff carries), runs each once and reports its exit code and the tests it ran in the `end_conditions` of its
answer (`FINDINGS_END`, the one explorer whose schema has the member; every other explorer answers `FINDINGS` with the packet it had
before, and a milestone's digest leaves `end_condition` out, so a run recorded before CRW-1382 replays its explorer calls and keeps its
pinned plan on a resume), which the answer carries into the critic's design check, and a command that failed, ran no test or was not reported is a
warning of the `crw.exploreRecord` row (CRW-1382; never a stop), and a first round that spawns no explorer (a DESIGN with no open
question to explore) runs none of them, which its `crw.exploreRecord` row records once as the warning `end_condition_not_run` naming each
command and its issue (CRW-1397; warn only); when a question of the round got an answer (an explorer's findings, or an answer the run has
reused), the answers go back to the architect on its own thread before the critic reads the DESIGN (`design-answers:<tag>r<round>`, CRW-1466): the
packet renders each answered question by its id, question, answer and findings, never by its route, and the architect answers with the DESIGN again,
dispositions null; that DESIGN is the one the critic audits and the one a design-fix and the ledger resume, and a question the architect raises in it
reaches the critic with no answer, which is a P1 finding, and is routed in the next round (a round whose questions got no answer has no such turn);
an open question whose explorer is missing is no answer: design-answers names it under `NOT ANSWERED` after the answers, the critic's
answers give it the route line `an explorer was asked twice and did not answer` with both calls, and the round halts the milestone
(source `explorer`) only when its call failed as above (every explorer it spawned missing, or one that did not complete otherwise), a
question asked of the owner thread being no exploration;
the critic (`agentType: 'critic'`, a role of its own, in a separate review execution;
the `AUDIT` spawn decision names it) audits the DESIGN (`DESIGN_AUDIT`, recorded by `crw.designAudit`). The result may include optional
`warnings: [review_same_thread]`; the journal records that warning and the observed `critic_thread` and `architect_thread`. An observed
same-thread review does not block the audit. `needs_changes` returns the DESIGN to the architect on its own thread: the design-fix packet
names each finding by its id `r<round>.<index>` (the index 1-based in the audit's findings) and the architect answers the DESIGN with `dispositions`,
one for each id, `folded` or `rebutted` with its reason (null in the first DESIGN, in a design-answers answer and in a re-plan answer; the schema
requires the member of every answer, and a DESIGN a run recorded before the member existed is read with it null wherever the run reads a recorded
DESIGN: the audit's candidate, the ledger, the release of a node, `crw.issuePlan` and the final gate's decisions); from the second round on the critic's packet
carries `PRIOR FINDINGS`, every finding of the earlier rounds with what the fix answer reported of it (`folded` or `rebutted` and the reason, `not reported`,
or `null (no dispositions reported)`), and the critic raises a rebuttal it does not accept again under the same ref (CRW-1452); only an approved DESIGN becomes revision
`r0` of the design ledger (`crw.designLedger`); `crw.dag` runs one lane for each node and the signal path of the milestone
([The architect's continuity and the re-plan](design-replan.md#the-architects-continuity-and-the-re-plan)); when every lane landed, `crw.closeMilestone` closes the milestone over
evidence the run makes itself ([The milestone close](milestone-close.md#the-milestone-close)); the architect reflects on the integrated range, on its own thread, and a
reason to re-plan asks it for the whole DESIGN of the milestone again; `auditAcceptances` of `crw:lib/close` audits every
acceptance against the record it rests on (`crw.audit`, [The milestone close](milestone-close.md#the-milestone-close)) and archives nothing, and `crw.reportUp` records the report
(its delivery is the seam of the issue that owns it); for a bundle, `crw.mergeRequest` asks the parent to merge the pull request of the closed
milestone ("The bundle's pull request" below). A milestone that cannot go on says why (`crw.milestoneHalt`): a call of the design phase, of the close or of
a re-plan that did not complete is never read as an approval, a design, a gate, a review or an alignment (`held` and `unknown` hold the milestone, `refused` and
`null_model` escalate, a gate that failed or cannot be settled and a QA that is INCONCLUSIVE escalate, `superseded` stops). A crosscheck
that does not verify the range escalates with its cause: `crosscheck_pending` (a `needs_changes` verdict), `crosscheck_refused` (ask
`configuration`), `crosscheck_failed` and `crosscheck_no_verdict`; one that held holds the milestone ([The milestone close](milestone-close.md#the-milestone-close)).

**The caps of the design loop.** The design loop is one `auditedDesign` of `project-lane.js` (the DESIGN or the re-plan's whole DESIGN it starts from, its audit rounds,
the architect's design-answers turns and its fixes), and it has two caps, both written in `project-lane.js` beside the loop. The round cap is `AUDIT_ROUNDS` (3): a DESIGN not approved in
that many rounds stops the milestone with the `design_audit_rounds` escalation, which the run tells the owner thread (CRW-1347). The token cap is
`DESIGN_TOKEN_CAP` (5,000,000 tokens), or `DESIGN_TOKEN_WARN` (0.8) of the run's token budget (`budget.run`, the budget its first process started with)
when that is lower, so a run without a budget has the fixed cap. The cap is made of what the journal fixes, the calls' tokens and `budget.run`, not of
`budget.total`: a resume under another ceiling (a narrower `--token-budget`, or none) reaches the cap at the same call and raises the escalation with the
same input, so the loop and what follows it replay. The tokens of the loop are those its calls report: the architect call it starts from (the DESIGN or the re-plan), the audit
calls, the architect's design-answers turns and fixes, and the explorers `crw.explore` spawns for the open questions of each round (its result's `tokens`). When they reach the cap, the
loop raises the `design_loop_tokens` escalation, which asks nothing and goes up to the owner thread, once per loop, and goes on; the engine pauses the
run at the whole budget (`budget_exhausted`). Reaching either cap writes a log row `warn design_loop_cap rounds: ...` or `warn design_loop_cap tokens: ...`
(CRW-1327). Neither cap is a block of its own: a warning and a record, the stop at the round cap being CRW-1347's. The background explorers
of the phase `explore` are outside the loop and its token cap (one for each node bounds them); the engine's budget counts them like every call.

**The lane of a node.** `crw.dag` starts a node when the nodes it waits for (a dependency or an order edge; the members of a
group are chained by order edges) have landed, in the scheduler's order, at most `cap` nodes at once and never two nodes whose
admission regions overlap; there is no barrier. The admission regions of a node are those of the plan together with the
`edit_regions` of the approved DESIGN (`crw.admissionRegions`), because the scheduler's own gate reads the plan's and the executor
is scoped to the DESIGN's; a planned region keeps the plan's change and exclusive flag (the milestone node's `planned_regions`, which
the relay passes when one of the node's regions is not a plain edit; CRW-1461). Two nodes overlap as `crw.regionsOverlap` judges
them: the relay's own rule (`dagsched.Overlaps` over the normal forms, so `./a.go` is `a.go`, a symbol overlaps its file, and a
delete, a rename or a hold of the plan's makes the file one place), together with Scope's reading of a region, and an undeclared node
overlaps every other. So the lane never admits together two nodes the relay would hold back `defer:edit_overlap`. `crw.dag` judges
every pair of nodes neither of which waits for the other once, in the order of the nodes and before it admits the first, keeps the
answers by the content of the two lists, and reads them without an operation of its own while it schedules; a pair it has no answer
for overlaps. The judgments are positions of the caller's path (and of the signal path, below) whose order and arguments only the plan,
the DESIGN and the signals' results decide, so a resume replays them. The relay is told the same regions before the release, each once by its
normal form. A planned region is declared from its fields. A request text, the milestone's handoff spelling or a DESIGN edit, names every
planned region that one of its readings equals: the whole text read as a path (a tree when it ends in `/`), or a symbol at each `#`. Those
regions get the plan's change and exclusive flag. A text that names no planned region is read as an edit region (`RegionOf`), and a planned
file and a planned symbol with the same text are both declared (CRW-1411). A node whose predecessor did not land is
`blocked`. Each node runs on a path of its own (its id), and its first position is `crw.release`, which asks the relay for the
slot and records the design revision and digest the node is released under. The lane builds its packet from that revision
(`crw.issuePlan` reads the journal and nothing the script holds), and every call of the node carries the revision, the node's slice
digest and its criteria digest in its input snapshot, so a ledger that later grows a revision changes nothing for a node that
started under the revision it was released under. The lane function is called with a third argument, `sig`, whose `receipt(result)` reports the RECEIPT of each
executor call to the signal path.

**A release held back by a node of the same dag (CRW-1467).** The relay judges a release by the same region rule, but against the nodes
it holds released, and those include nodes `crw.dag` admitted beside this one. A release it holds back `defer:edit_overlap` while another
node of the dag runs, or after one has ended since the node asked, is the dag's own doing, and the node is re-admitted: it gives up its
claim, wakes the nodes waiting in theirs, and asks again. It asks at once when another node ended since its ask, because the count it
compares with is the one of its ask: a node that ended after the answer came, but before the lane read it, counts. Otherwise it goes back
to pending (it holds no slot of the cap and no regions) and is admitted again when another node has ended. A release held back while no
other node runs and none has ended since the ask is held back by something outside the dag, such as a node of another milestone, and the
node stays `unreleased` as before. Each re-ask needs another node to have ended, so a node asks at most once more for each other node.
The record decides on a resume: the held-back answers that the lane asked again after (`crw.deferredAsks(node)`) replay and are each
followed by the next ask, in whatever order the other lanes replay. After each of them the node is pending again and is admitted, after
the other pending nodes, as soon as the cap and the regions let it, without waiting for a node to end; a node that ran in its place, and
the signal that node made, replay first. The recorded release is the answer of the ask after them, so only
that ask waits for the signal of the revision it was released under. Such an answer is proven when a later `crw.release` row of the node's
path, completed or refused, was written after it. The last held-back answer with no ask after it (a kill inside the relay call leaves no
row) runs again and asks the relay. The positions are the same either way: the held-back release, `crw.planRead` when there is a signal
path, then the next release.

Before the relay is asked for the slot, `crw.release` declares the node's admission regions to the relay (`dag_node_regions`, as
`dag-region-declare` does; CRW-1391), because the relay's release judgment reads only declarations and an undeclared node overlaps
every other: without it the second of two nodes `crw.dag` admitted together was held back `defer:edit_overlap`. It declares the
admission regions of the milestone's other designed nodes that have none as well, since the judgment counts a ready node ranked before
the released one as a holder. A plan region is declared from its fields, its path, kind and symbol as the plan holds them, and never read
from its text again (a planned file or tree whose path holds `#` stays one, CRW-1408). A request text names every planned region that one of
its readings equals (CRW-1411), so a planned region spelled unclean (`./path`, a doubled slash, a `#` in its path) is that region with the
plan's change and exclusive flag. A text that names no planned region is an `edit_regions` entry: a file, a tree when it ends in `/` and a
symbol after `#`. A node with a declaration in force keeps it (the parent's graded declaration made before the run, or one an
earlier release made), and a node with no regions on either side is left undeclared. A declaration the relay refuses is a warning, never
a refusal: the release row records `regions`, `regions_declared` (`declared`, `kept` or `failed`) and the warning `regions_not_declared`
with the relay's reason, and the release goes on as for an undeclared node.

| `crw.classify` state | The one transition |
| --- | --- |
| `ready` | unit check, commit, verify, land (the unit check below judges the call's uncommitted work before the commit) |
| `self_failed` (the executor reports `failed` or `budget_exhausted`) | as `ready`: the run runs the unit check of the worktree anyway, then commits and verifies it; the report is no evidence |
| `research` | For repository facts only: `crw.explore` receives the node's edit regions, the original findings of the current fix round, and the asking executor call. Its explorer reads a retained snapshot of that call's pending tree, including uncommitted changes, in its own read-only checkout; the packet names that snapshot. An answer is reused only for the same question, source tree/base, node, regions and findings (SP-EXPL-2: at most two research continuations on a tier). The same executor thread continues with the answer; never a tier step. A fix needing a file outside its assigned regions returns `blocked_scope` with files and reasons in `gap` directly; an explorer grants no editing permission. The close-fix research path follows the same rule. A `null_model` explorer is asked once more on a fresh thread at the same snapshot (CRW-1571); when that one is `null_model` too, or the explorer did not complete otherwise, `crw.hold` holds the node (source `explorer`) |
| `scope` | `crw.escalate` with `blocked_scope`: the relay closes the node's execution and takes its slot back (R07) |
| `null_model` | the first one: a fresh thread of the same tier in a new attempt worktree at the node's last commit; the second in a row is tier evidence |
| `host_held` | `crw.hold`: the node is held and the call goes on in place when `crw flow resume` runs after the cause is gone; never a new thread, never tier evidence |
| `null_unknown` | `crw.hold` with `dispatch_unknown` (a recovery hold): nothing is sent again |
| `refused` | `crw.escalate` as a configuration fault, then `crw.decide`: the lane goes on, with a fresh thread of the same tier, only when the run journal has the `decision_applied` row of that escalation and node |
| `superseded` | the lane stops |

**Unit check.** `crw.unitCheck(node, call, opts)` judges a node's work before `crw.commit` commits it (CRW-1465): the run runs the
review tool itself, in Go through the review adapter, over the uncommitted changes the executor call `call` left in the node's
worktree, so no reviewer agent is called and no reviewer thread is made for a node (row `crw.unitCheck`,
[journal.md](../journal.md#the-proofs)). The tool is a Codex review by default (`codex review --uncommitted`, CRW-1517; its model and
effort are the `unit-check` key of the helper role store — `crw role helper set unit-check --mode model --model <model>
--effort <effort>` (CRW-1589; with no entry, or one in mode default, the tool default `gpt-6-astra` at `high`, and a stored
model with no effort takes `high`) — which a non-blank `CRW_UNIT_CHECK_MODEL` or `CRW_UNIT_CHECK_EFFORT` overrides per field
in the run's role snapshot, each applied override named in a warning and recorded in the started row's `unit_check_overrides`,
and a setting codex cannot be given stops the run when the lane is built, naming the field or the environment variable that supplied it), and Bugbot on a Codex quota: a check whose Codex call ends `quota` is asked once
more, with the same request, of Bugbot, the row's `result` is Bugbot's and the row keeps the Codex call (`fallback`, `fallback_from`,
`fallback_state`, `fallback_model`); a Bugbot call that ends `quota` too holds the node as below. Codex prints no mark of a finished
review, so a Codex answer with no findings list is clean when its summary is neither Codex's own text for a review that gave no answer
or was interrupted nor a wording the reader knows for a review that did not happen, finish or cover the whole change, a very short clean
summary such as `Clean.` reads clean, a summary with no letter (punctuation or digits only) reads invalid, and a summary that says the review did not happen in other words still reads clean. The tree it judges is the call's pending tree, the one `crw.commit` then commits, so the
acceptance takes the check as the judgment of the head that commit makes; a worktree that is no longer that tree is not checked
(`tree_moved`), nor one whose `HEAD` is not the commit the node's attempt is at, the start of its worktree or the node's last commit
since (`head_moved`: the executor committed its work itself, whatever the backend), and a check during which `HEAD` moved, even to a
commit of the same tree, is no judgment (`head_moved`: `HEAD` is read before and after each call of the tool, also one that failed, a
retry and a Bugbot fallback too). During the check, as at its start, a moved `HEAD` escalates (`unit_check_head_moved`, below), and a resume keeps it escalated. The prompt carries the node's criteria and, when the backend
assigned the executor an evidence receipt, the receipt's accepted copy; `codex review --uncommitted` takes no prompt, so a Codex check is
given no prompt and the row's receipt records `handed` false (read and kept, not handed to the tool), while a Bugbot fallback is handed
it (`handed` true); a receipt that cannot be read and kept holds the node (below). A
check with any finding, blocking or not, goes back to the same executor thread (`fix:<node>:<tier>:r<round>`, the findings in the fix
packet), and the fixed tree is checked again, at most two round trips on a tier (`UNIT_CHECK_ROUNDS`); the check after them is
`final` and its findings are carried, before any tier step: when none blocks (verdict `verified`) the node is committed, verified and
landed with them as pre-merge defects that need no disposition (`crw.land` hands their ids and the checks that carried them to
`crw.accept`, which reads each from that check's record into the premerge record, also when the check the acceptance rests on judged
only a later fix), and the lane result names them (`carried`); a blocking one escalates
`unit_check_carried` (ask `land`, the carried ids and each finding's file and text) and nothing lands. A finding is named by its file
and its text, never its line (`reviewadapter.FindingID`). A check the tool gave no judgment in (`quota`, `no_answer`) or that had no
executor receipt to read and keep (`receipt_missing`, `receipt_unverified`, `receipt_not_copied`) holds the node through `crw.hold` with
source `unit_check`, as a verification that could not run does. After `quota` or `no_answer` a resume asks the tool again; after
`receipt_not_copied` it reads the receipt again, its step-bus copy or, in its place, the receipt where the hook accepted it when that
still hashes to its digest, so the hold clears once the receipt can be read; `receipt_missing` and
`receipt_unverified` stand on the executor call's own record, which a resume does not change, so a resume holds the node again until
the child acts on the hold. Any other check that judged nothing (`tree_moved`, `head_moved`, `worktree_changed`, `tool_failed`, an invalid answer asked twice) escalates
`unit_check_<state>` as a configuration question. A fix turn that answers `needs_research` goes through the research route on the
same thread. A failed verification of the committed head goes back to the same thread as well, and its fix is checked again before
the next commit; while round trips are left, standing tier evidence steps the tier before a fix round. A commit holds only checked
work, so a fresh thread (a tier step, a rebuild) starts at the node's last commit and the uncommitted trees of the attempt it leaves
are abandoned there (its packet carries their last findings and receipt); a resume replays the calls that made them as recorded facts,
since nothing reads their effect again, and runs none of them live (journal.md, the proofs).

**A split node (CRW-1459).** A node the architect sized `split` names its sub-tasks in the DESIGN (`sub_tasks`, each with an `id`, a `summary`, `edit_regions` and a `done_condition`), and `crw.release` releases it to one executor as any node. Its sub-tasks run in their order on the node's one executor thread: the build does the first (its packet carries `SUB-TASKS`, each marked done or to do, and says to do the first one not done, and only it), and each next one is a turn of the same thread, `subtask:<node>:<tier>:s<k>` (the packet of `subTaskPacket`: the sub-task, its edit regions and its done condition). A `ready` or `self_failed` receipt of a sub-task before the last gets no unit check: the lane commits it as a checkpoint, `crw.commit(node, call, regions)` with the sub-task's `edit_regions` (its `out_of_scope` is recorded against them), with no `crw.unitCheck`, no `crw.verify` and no `crw.land`, and the same thread goes on to the next sub-task. A checkpoint commit is thus the one commit that holds work no check has judged yet; the check of the node's whole change judges it with the rest, and nothing of the node lands before that check. A split node makes one checkpoint `crw.commit` per sub-task before the last and is judged once: the receipt of the last sub-task, and every receipt after it (a fix round), goes, with no uncommitted check, through the node's commit against the plan's regions and its verification, then the one check of the node's whole change, `crw.unitCheck(node, call, {round, final, commit: head, base})` in the committed mode over the range from the verification's `base` to the head against every criterion of the node (the call is the executor call the range ends on, whose receipt is read as an uncommitted check's; the receipt of every other executor call a commit of the range was made from, the checkpoint calls among them, is read too, and one that is not taken holds the node on that call), then its land. A finding of that check goes back to the same thread as a fix round (the round trip cap `UNIT_CHECK_ROUNDS` and the carried findings of the node lane hold for it as for any check), and the fix's work is committed, verified and checked as a whole again; two checks of the whole change in a row that share a finding are the evidence of a tier step. The acceptance rests on that check alone. A sub-task that reported failed is named in the next packet (`REPORTED FAILED`, with the receipt's summary and gap) and is judged with the node; it is no tier evidence. The other states keep their transitions. A fresh thread (a rebuild, the `executor-t1` build after a tier step) starts in a new attempt worktree at the node's last commit, which holds the sub-tasks done, and its packet starts at the first sub-task not done, or, with every sub-task done, is the node's packet over that checkpoint; the uncommitted edits of the sub-task in flight are abandoned with the attempt, as those of every abandoned attempt are. A split with no sub-tasks (a DESIGN recorded before CRW-1435) is released to no executor and goes up as `size_split`.

**Review.** A `crw.review` row is the record of the old reviewer path, which only old runs wrote: a fresh reviewer thread over a
node's committed head and the record of its `VERDICT` (node, head, criteria digest, state). No operation of that name is registered,
and no shipped script calls a reviewer for a node. The rows of old runs are read: `crw flow show` and the run readers list the review,
`crw.accept` takes the newest judgment of the head among the old review rows and the unit checks, and the audit reads an old review's
acceptance on its reviewer thread ([The milestone close](milestone-close.md#the-milestone-close)). A run whose journal holds such a row is refused for every resume and every
write ([Old runs of the retired review paths](recovery.md#old-runs-of-the-retired-review-paths)).

**Tier evidence (SP-TIER-1).** `crw:lib/evidence` keeps the evidence of one node and tier from the results the lane feeds it: (a)
`needs_changes` in two consecutive reviewed rounds with a finding in both: in the node lane two consecutive unit checks that rule
`needs_changes` and share a finding id (`reviewadapter.FindingID`: file and text, no
line; a verified check's findings block nothing and are no evidence: what is left of them after the round trips is carried), (b) the required verification failing on two consecutive committed heads, (c) `null_model` on two consecutive executor
calls of the tier, (d) four reviewed rounds (reviews or unit checks) without a verified verdict. `crw.tierStep` then mints the step decision and
`crw.stepRecord` appends the `flow_step_decisions` row (the rule, the letter of the evidence, the evidence ids, the head and tree of
the node's last commit as the checkpoint; the step id is the engine's, or that id with a digest of the decision when the stored row of
that id describes another checkpoint, which a suffix that runs again at the same position can reach); only then does a fresh `executor-t1` call start, in a new worktree at that commit,
carrying the id. The same evidence on `executor-t1` is SP-TIER-2: the node escalates as `tiers_exhausted`, the relay closes its
execution and returns its slot. A self-reported failure, `needs_research`, `blocked_scope`, a single `null_model`, `null_unknown` and
every held, refused or superseded result are never evidence.

**Land.** `crw.land` is `crw.accept` and then `crw.integrate`. `crw.accept` needs the passing verification record of the head in the
journal and its judgment: the completed `crw.unitCheck` row of role `unit-check` on the head (its commit, or its tree equal to the
head's) with result state `ok` and verdict `verified` (`ops.UnitCheckVerdict`), or, on the old reviewer path, the verified review of the
head. It assembles the premerge-record/1 from that verdict (a code step: `ops.AssemblePremerge`), judges it
as the relay's acceptance will, and asks the relay for the agent acceptance (R05); every finding that needs a disposition is
`blocking` by default, and only the owner thread separates or carries one, so such a head is `land_refused` (`premerge_blocked`) and
the lane escalates. `crw.integrate` lands the accepted head on the integration target. A refusal of either is an op error that carries
a closed reason and leaves a refusal row, not a completed row: a resume asks again, and the engine's accepted-node check reads only
completed `crw.accept` rows.

**The bundle's pull request.** A handoff whose project target names `pr_base` (the branch the pull request goes to, `dev` in practice) is a
bundle: its run lands its nodes on the target's own `base_ref`, a bundle integration branch that lives in the target checkout, and a
`base_ref` that equals `pr_base` is refused at compile. `crw flow pr branch --checkout DIR --milestone M [--base dev] [--name BRANCH]`
makes that branch (`--name` is the handoff's `base_ref`; `bundle/M` by default) at the tip of the local base branch once, reuses a branch that already contains that tip and
never moves one that does not (exit 2); `crw flow start` refuses a bundle whose branch is not in the target checkout, naming that command,
before it writes a run, the preview included, and a project without `pr_base` is not checked. When the milestone has closed,
`crw flow pr open --run RUN --push DIR` opens the one pull request from the run's journal: it refuses unless the run closed its milestone
and the milestone's crosscheck is verified at the close tip, it takes the issues from the milestone's nodes, builds the body from the run,
refuses a title or body that leaks a host path, the host name or a thread, call or run id, and pushes exactly the crosschecked tip, never
forced; it reads the crosscheck of the close from the run's `crw.crosscheck` rows (CRW-1442), so a run with no such row is refused with `crosscheck_missing` and the pull request is opened from a text with `--issue` as before.

The built-in `project-lane` does the same natively for a bundle: after the milestone's close is reported, `crw.prOpen(milestone)`
proves the closing acceptance from the journal — the latest unsuperseded `crw.milestoneClose` at the live integration tip, its gate
a `crw.finalGate` pass recorded before it at the same tip, the closing review's verified verdict (or the legacy Final) bound to the
tip and the milestone, and the `crw.reportUp` of that close — and opens or reuses the pull request the plan's `pr_base` names
(`crw flow pr open`'s `OpenBundle`): the close's tip pushed once, never forced, an existing pull request at the same head and base
reused (a resume after the POST finds it by lookup and never opens a second), and the persisted review record refreshed for the
head. A plan without `pr_base` answers `not_required` and the run ends `closed` as before; a milestone that did not close, a gate
that is not the close's pass or a report that did not happen answers `refused` (`milestone_not_closed`,
`final_gate_not_accepted`, `head_moved`) and opens nothing. The lane then calls `crw.mergeRequest(milestone, {pr, review: {record}})`, which judges that pull request in order (open and not merged, the same closing acceptance at its head, its checks green, no unresolved review comment while the review bots are connected) and either answers
`{state: "refused", reason}` as a result of the op, writing a row of its own (`pr_not_open`, `milestone_not_closed`,
`final_gate_not_accepted`, `head_moved`, `checks_failed`, `checks_pending`, `checks_none`, `comments_unresolved`), or hands a merge request to the
reporter, which asks the parent to merge. The request goes once per head: a second call at the same head of the same pull request answers
again with `repeated` and sends nothing, and a head that moved needs a new close and a new request. The run never merges the pull
request; the dev queue does. `open --run` stays the standalone path: it reads a *finished* run's journal, while `crw.prOpen` consumes
the live journal of the run it is in — a run that has not yet produced its finished row uses the native op, not the command.

`crw flow pr comments --pr REF --head SHA --record PATH --bots connected|not_connected --round 1|2`
collects Devin and Codex comments from the issue, inline-review and review surfaces. Identical findings share an item key;
`--dispositions FILE` supplies a JSON array of `{key, disposition, reason}`, with `disposition` either `applied` or `rejected`
and a nonblank reason for each decided item. The `crw-pr-review/1` record retains unresolved items across rounds.
An unresolved second round answers `round_limit`; a third round is refused. A refresh at the same head can complete that round's
dispositions, while a changed head needs the next round. With no connected bots and no recorded bot comments,
the record says `리뷰 봇 미연결`. `mergeRequest` reads this file and binds it to the exact PR head; missing, malformed or
stale records fail, and caller-supplied `bots` or `unresolved` scalars cannot approve a request.

The parent runs `crw flow pr decide --pr REF --head SHA --release-impact TEXT` against `manage.integrate`.
It checks the expected head, the remote base tip, successful checks and overlapping earlier pending PRs, without reading diff content.
An outdated base answers `needs_base`; `--update-base` merges it into the PR head with `--no-ff`, pushes without force while
checking the advertised old head, and answers `needs_ci`. Overlapping earlier PRs answer `hold_order`.
An all-green decision records the supplied release impact, posts an idempotent readiness comment and enqueues the pinned head.
`--record PATH` selects the decision record; the default is `<runs_dir>/decisions/pr-N-SHA.json`.

**Decisions.** `crw.spawnDecision(rule, decision, info)` appends a `flow_spawn_decisions` row for every decision to spawn, do the work
inline (SP-INL-1), reuse an answer (SP-INL-2), skip, refuse or hand a question to the owner thread; the explorer routing
(`crw.exploreRoute`) writes its own rows. A script names a rule by its key (`EXEC_1`, `TIER_1`, ...) and the id comes from
`internal/flow/ops/rules.go`, the one place the `SP-*` strings are written.

**Operations and composites.** The operations (`internal/flow/ops`, one `engine.Op` each, rows `crw.<name>`):

| Operation | Records and returns | Replayed while |
| --- | --- | --- |
| `milestone(plan, milestone)` | the nodes of the milestone with edges, regions and slice digests | always (a read) |
| `designAudit(milestone, round, candidate, architect, call)` | the audit row: state, candidate digest, auditor and architect models, optional `warnings: [review_same_thread]` when the host reports the critic and architect thread are the same; the journal records `critic_thread`, `architect_thread` and warnings, and the warning never blocks. A candidate that breaks a design rule the DESIGN schema cannot express (a split node's 2 to 4 sub-tasks, none for another node, unique sub-task ids, sub-task regions within the node's, `after` naming only earlier sub-tasks; CRW-1435) is audited `needs_changes` with a P1 finding that names the node and the rule (`design_rule` on the row), whatever the critic ruled, so the DESIGN goes back to the architect; a candidate the schema refuses is an error | always |
| `designLedger(milestone, entry)` | the next revision (`r0`, `r1`, ...): the DESIGN, the answers, the digest, the architect handle, the signal that made it and the slice digest of every node of the plan it was made against; refuses a DESIGN with no approved audit - for a revision after `r0` an audit made after the revision before it - and a revision after `r0` that does not keep what the started nodes stand on (a started node with no issue, or whose issue differs in any field from the plan `crw.releasedPlans` gives for it, or a decision that plan names missing or changed in any field) or that plans no issue for a node the run read and the plan of the revision holds | always |
| `release(plan, node, milestone)` | the release under the revision of the ledger that stands (`revision`, `design_digest`, `slice_digest`, `execution_id`); a node sized `split` with sub-tasks is released to one executor as any node is (CRW-1459), and a node sized for a `bundle_split`, for a child or for a split with no sub-tasks (a DESIGN recorded before CRW-1435) is not released to an executor (an unreleased row of that `kind`); a plan that cannot be read, or that has dropped the node (`node_not_in_plan`), releases nothing | the relay says the execution stands, and the revision it was made under stands or the node's slice is the one the revision that stands was made against (`release_rebind`) |
| `issuePlan(node)` | the node's entry of the DESIGN of its release revision | always |
| `commit(node, call, regions)` | the commit of the call's recorded `pending_tree` (`commit`, `parent`, `tree`, `out_of_scope`, the receipt's outcome and `plan_mismatch`) | the commit is on the history of the node's worktree |
| `verify(node, head, opts)` | the verification (`ok`, `command`, `digest`, the record); a verification that could not run is `ran: false`, not a failure. With no node, `opts` names the milestone, the command and the base: the verification of the integrated head for the final gate, in a worktree of the repository | always while it ran |
| `unitCheck(node, call, opts)` | the unit check of the node's work by the review tool, run in Go (no agent call): uncommitted mode over the changes executor call `call` left, or, with `opts.commit` and `opts.base`, committed mode over `base..commit` (with a `call`, the executor call the range ends on: its receipt is read as in uncommitted mode); the `reviewadapter.Result`, the `hold` the lane is to write (`unit_check_quota`, `unit_check_no_answer`, `receipt_*`), `retried`, the judged call and the check's id | always, but a check that names a `hold` (asked again) |
| `accept(node, head, opts)` / `integrate(node, head, acceptanceId)` | the acceptance id (`opts.carried`: the findings the node lane carried, each `{id, check}`, read from the record of the check that carried it into the premerge record's defects) / the batch commit and the target tip | the acceptance is active and not stale / the batch commit is on the target |
| `escalate(node, info)` / `hold(node, info)` | the escalation flow row and, for `tiers_exhausted`, `blocked_scope` and `size_split`, the relay's close of the execution; the hold (the relay is told, nothing is closed) | always |
| `decide(node, ask)` | whether a `decision_applied` row answers the ask | no applied row has appeared since |
| `spawnDecision`, `stepRecord`, `exploreRoute`, `exploreRecord` | the decision rows and the answers the reuse reads | always |
| `finalGate(milestone)` | the gate over the pinned head: the state of the gate and of each piece of evidence (row `crw.finalGate`: `head`, `command`, `range`, `ok`) | the integration target is still the head the gate judged |
| `qaReceipt(milestone, gate, capture, passes, receipt)` | the QA receipt: its state (`PASS`, `REVISE`, `FAIL`, `INCONCLUSIVE`), the head, the calls with their roles and read-back pairs | always |
| `qaCapture(milestone, gate, capture)` | the capture of the QA call as the later calls read it: each capture with the path it can be opened at (the relative path resolved against the worktree the capture ran in), the identity of the tree the call ended in, and `ok` or the reason the capture cannot be handed on | always |
| `crosscheck(milestone, gate, opts)` | one round of the crosscheck over the gate's range, run in Go with no agent call (row `crw.crosscheck`): X = `{state, reason, hold, detail, verdict, questions}`, the state `verdict` (V: `verified` or `needs_changes` by `ops.CrosscheckRule`), `held` (`hold`), `refused`, `failed` or `no_verdict` (`reason`); round 1 only | the row is of the same milestone, head, round and evidence round and did not hold |
| `milestoneClose(milestone, evidence)` / `reportUp(milestone, close)` | the range and the tip the close judged over `{gate, crosscheck}`, and whether the target moved on after the gate; evidence with no verified crosscheck V of the gate's milestone and head is refused `close_unverified` / the report row | the integration target is still the tip the close judged / always |
| `replanSignals(milestone, opts)` | the two numbers of SP-ARCH-2 and the reasons that stand (`plan_mismatch`, `slice_change`, and at the close `reflection_misaligned`, and `reviewer_final_misaligned` for a MISALIGNED `opts.final`, which the shipped close does not hand it) | always |
| `currentDesign(milestone)` | the revision that stands, its DESIGN, the handle of the architect thread and the heads the nodes landed with | always |
| `reflection(milestone, gate, call)` | the architect's REFLECTION over the integrated range: a verdict only for a completed, valid answer; the row moves the architect's handle on | always |
| `planRead(milestone, node)` | the plan revision a lane reads before its release: the digest of the milestone as the relay has it, its replay-stable observed occurrence, the plan the audited DESIGN stands on (`base`: the plan the current revision was made for, or the one a later signal judged and kept it for), whether it changed, the signal in flight that read this very plan (`judging`), and the position it ran at | stands once the node was released after it, or once a signal row that stands took that occurrence of the changed revision as a trigger; else it reads again |
| `signalBind(milestone, k, prev, members, attempt)` / `signalDone(milestone, k, outcome)` / `signalClose(milestone, summary)` | the signal row (its triggers, what had started and what had not, the tip of the target, the plan revision read), its result (continue, refresh, halt; the revision it made) and the end of the signal path | always; a resume replays a bind only with the same arguments (`attempt` is `live:<n>` for a recorded signal one of whose triggers ran again, `n` one more than the attempt of the recorded row, so that its arguments are never the recorded ones, also when that row is itself the live attempt of an earlier resume) |
| `audit(milestone)` / `archive(milestone)` | the audit results of the milestone's acceptances (row `crw.audit`) / the reviewer threads of old review acceptances archived and kept; no shipped script calls `crw.archive` | always |

The composites (`crw:lib/lane` and `crw:lib/close`, made members of `crw` by `install(crw)`; they take no position of their own): `crw.dag`,
`crw.explore`, `crw.land`, `crw.tierStep`, `crw.evidence`, `crw.skip`, `crw.closeMilestone` and `crw.milestoneHalt`; the close's
`auditAcceptances` and `reportClose` are functions `crw:lib/close` exports. `crw.dag` takes an `onSignal` function (below). The seams the operations are built on - `Relay` (the scheduler: milestone, release, agent
acceptance and integration, the close of an execution), `Verifier` (`CILocalVerifier` runs `crw-dev ci local` through the verifier's tools of the configuration, [the verifier's tools](../../runtime-install.md#the-verifiers-tools); the command leads its own process group, and a cancel kills the whole group, so a cancelled `crw.verify` and a cancelled integration batch end within `execgroup.KillGrace`, 5 s, CRW-1573), `Decisions`
(`StoreDecisions` writes the relay store), `Threads` (the host the audit reads an old review's reviewer thread from, and the one
`crw.archive` archives on) and
`Reporter` (F11) - are interfaces in `internal/flow/ops/seams.go`; the dagsched-backed `Relay` belongs to the run process.
