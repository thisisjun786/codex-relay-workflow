
## The architect's continuity and the re-plan

**One thread.** The architect has one thread for a milestone. The design ledger (`crw.designLedger`, rows `crw.designLedger`) holds the handle of the
thread, and every completed architect turn moves it on: the DESIGN (and its design-answers turns and the fixes the audit asked for), the REFLECTION at the close
(`crw.reflection`) and the whole DESIGN of a re-plan are the turns of that thread, one after the other (`agent.call` with `resume`, which
`crw.currentDesign` hands the script). A call that did not complete is never read as ALIGNED or as a DESIGN: `held` and `unknown` hold the milestone
(the resume goes on with the same call as its next turn), `refused` and `null_model` escalate (`crw.milestoneHalt`). The REFLECTION turn
(SP-ARCH-3) and the re-plan name the head they judge (`head`, above): the REFLECTION the integrated head the final gate pinned, a re-plan during the run
the tip of the integration target that its signal row recorded when it decided to re-plan. The packet names the head, the range from the head the
DESIGN ran at (for the first revision the commit the architect's checkout was made at, which a lane that lands during the audit does not move),
and the head each node landed with.

**Revisions.** The ledger holds revisions `r0`, `r1`, ...: a revision is recorded only over an approved audit of its own, made after the revision before it
(the re-plan's DESIGN goes through the same audit rounds as the first DESIGN, `needs_changes` returning it to the architect on the same thread; the critic's packet
carries `RELEASED PLANS`, the plan each started node was released with as `crw.releasedPlans` reads it from the revision of its release - the whole issue, every field, and the whole
decisions its `decision_refs` name; a node whose issue was sized for a child, a bundle split or a split with no sub-tasks started too (its lane recorded the design row and escalated it), is
listed with that plan and is no unstarted node of the re-plan; a bundle split is listed with the id of the `bundle_split_request` escalation its lane raised for it
(`escalation_id`; before that escalation is recorded, as one its lane escalates so), and the packet says nothing of whether that request was delivered or to whom, since the result of
`crw.releasedPlans` is replayed on a resume and a delivery can change after it was read - and a started node whose issue differs from it in any field, or a decision such a plan names that the DESIGN drops or changes
in any field, is a P1 finding). `crw.designLedger` holds the revision to the same rule and refuses it otherwise: a started node (the one predicate of
`crw.releasedPlans`) has no issue, its issue differs in any field from its released plan (an absent list reads as an empty one), or a decision that plan
names is missing or differs in any field from the decision of the revision the node started under; it refuses a revision that drops a decision of the
revision before it (the ledger's current one), whatever node names it; and it refuses a revision that has no issue for a node
the run read that the plan of the revision (the evaluated plan, else the live one) still holds. A node that has not started changes freely, and so do the fields of a
decision no started node names, which is kept all the same; a node the plan dropped and that never started is not demanded, and a started node is demanded whatever the plan. The nodes that have not started are
released under a revision only after that, because the signal that re-plans keeps every new release back until it is decided. A lane that started keeps the
revision it was released under; it ends under it and lands or is held as usual. Each revision records the slice digest of every node of the plan it was made
against - for a revision after the first, the plan the architect and the auditor evaluated (`plan` of `crw.replanSignals`, handed to `crw.designLedger`), so that a plan revision made while
the re-plan's DESIGN was audited is a change the next signal counts against the revision - and a release records the slice of the node as the plan is when the node is released (read again at the release: a plan that
cannot be read, or that has dropped the node, releases nothing).

**The numbers (SP-ARCH-2).** `crw.replanSignals` writes two numbers to the journal: the RECEIPTs that report a `plan_mismatch`, of lanes released under the revision that stands (each call
once; a RECEIPT of a lane released under an older revision does not count in the new revision's number), and the share of the milestone's nodes whose slice
digest of the plan now is not the one the revision that stands was made against. The reasons are two plan mismatches, more than a quarter of the slices changed
(exactly a quarter is not a reason), and, once the milestone closed, a MISALIGNED REFLECTION. The op also counts a MISALIGNED
`opts.final` (`reviewer_final_misaligned`) when a caller hands it one; the shipped close runs the crosscheck and hands it none. One reason is enough, whatever the
alignment says. Every decision - to re-plan, and to skip it with its reason - is a row of `flow_spawn_decisions` (`ARCH_2`).

**The signal path.** `crw.dag(plan, lane, {nodes, milestone, cap, onSignal})` runs the signal path next to the lanes when it is given `onSignal`. The
RECEIPT of an executor call of a lane is a trigger (`sig.receipt`), and so is a plan revision a lane reads before its release (`crw.planRead` answers `changed` when the plan is another than the one the audited DESIGN stands on, and `judging` when a signal in flight read this very plan; `sig.plan`; the trigger is `plan:<occurrence>:<digest>`, once per observed occurrence). The occurrence names the newest standing observation of another digest, or the first ledger row (journal.md); concurrent reads of the same occurrence coalesce, but P0 → P1 → P2 → P1 takes a new signal for the return to P1. Its lane waits for that signal's decision and the critic's design check of any DESIGN it requires, not the earlier signal for P1. Replayed reads and recorded signal members preserve the same identity, including the legacy key of a read recorded without an occurrence. The path `signals:<milestone>` takes the triggers that have arrived, in the order of the
journal and not the order they finished in, into one signal at a time, and the triggers that arrive while a signal runs are taken together by the next one.
A signal is `crw.signalBind` (its input: the number `k`, the triggers as (call, digest) and the result digest of the signal before it; the row records what had started and
what had not - a node has started when a release of it to an executor stands, or the design row that stands sized its issue for a child, a bundle split or a split with no sub-tasks
(its lane escalated it), the predicate `crw.releasedPlans` lists by too, so a milestone whose other nodes are all sized so skips the re-plan
(`no_unstarted_node`) -, the tip of the integration target and the plan revision read), a join of one item, the child path `signal:<milestone>:<k>`, which runs
`onSignal`, and `crw.signalDone` (continue, refresh or halt, and the revision it made). While a signal is decided no lane is released and the scheduler admits no node: a node that read a changed plan revision waits until the signal that took that trigger is decided, live or replayed, and is released under the revision that stands then. Every release of `crw.dag` compares the plan (`crw.release(plan, node, milestone, {pinned: true})`): the plan the relay has when `crw.release` reads it, in the flow and before `Relay.Release`, is compared with the plan the audited DESIGN stands on, and a node whose plan is another is not released (`plan_changed`). The release is a compare-and-swap on that plan (CRW-1288): `Relay.Release` carries the node's slice of the compared plan, and the relay compares it with the node's slice of the plan's head when it judges the release and again in the transaction that reserves the slot and records the release, so a plan change between the flow's comparison and that commit releases nothing either (`plan_changed`; the relay's refusal is `plan_revision_conflict`). Either way the lane reads the plan again - a trigger - and asks again once its signal re-planned and audited a DESIGN for that plan or judged it and kept the revision. A dag without `onSignal` leaves such a node `unreleased`. A relay refusal while the plan the flow reads again is still the pinned one (`relay_plan_changed`) is a plan revision the run does not follow: the run reads its plan at the revision it pinned (CRW-1277 c) and the relay releases from the plan's head, so no signal comes of that change. The release raises the run-level hold `plan_superseded` (CRW-1291): an escalation of the node that asks the parent and names the revision the run pinned, the relay's head revision and the node, which the run tells the child's thread like any escalation ([reporting.md](../reporting.md)). While the hold stands no node of the run is released (`plan_superseded`, the relay is not asked) and nothing restarts on its own, a resume included. The execution the relay still holds for the node (a release the run never recorded, whose replay the relay refused) is closed with the hold when it is the run's, which returns its slot, and recorded with its run otherwise. The actions are the parent's: end or cancel the run (`crw flow cancel`) and start a new run on the new revision, or answer the hold's question (`crw flow answer`, then `crw flow resume`), which closes that node by escalation (`plan_superseded`) and lifts that hold: the other held nodes ask the relay again, and each one the new revision changed raises a hold of its own, so answering takes one round per changed node (cancelling the run and starting a new one is the shorter way). Lanes released before the hold go on to their integration. The project lane ends `held` (`plan_superseded`) while the hold stands. The operating rule follows: a plan is not changed while a trial run runs on it. The relay answers the replay of a release only to a request that expects the slice the release was committed under, so a release the relay committed before the run recorded it is not answered as a release under the slice of a plan that changed since. `onSignal` of the project lane
counts the numbers and, when a reason stands and a node has not started, runs the re-plan: the whole DESIGN, its audit rounds, the revision; a re-plan that cannot be done halts the milestone,
and no node that has not started is released. A refresh gives the signal's outcome the admission regions of the nodes under the revision it made (`regions`: the plan's regions, with their planned change and hold, and the `edit_regions` of that DESIGN, by `crw.admissionRegions`). Right after `crw.signalDone`, while the signal still holds every admission, each list a node has not had before is judged by `crw.regionsOverlap`, in the order of the nodes, against every list each node it may run beside has had, whatever state that node is in; the scheduler then reads them for the nodes that have not been admitted and for the nodes it admitted that have not yet been released: before its release a node is checked once more against the nodes that hold their regions (released, or about to be), and waits for the end of the one it overlaps. When the reason stands after the last node started the close judges the same numbers.

**A resume.** A signal is replayed with the membership its row recorded (`crw.recordedSignal`), whatever order the lanes replay in now, and only when each of its triggers was
replayed from the journal (`crw.callFacts`: not live) with the digest it answered then and the signal before it was replayed. The first signal that does not replay (also one whose triggers all answer the digests they answered, when one of them ran again: the script binds it as a live attempt numbered one more than the row it does not replay, so a second resume that runs a trigger again supersedes what the first one made, and a later resume replays that row with the attempt it names - `crw.recordedSignal` returns it as `attempt` - so an unchanged resume after it makes no call), and every
signal after it, are the suffix: the engine ends their rows and the calls they made superseded, the revisions they made get a flow row `revision_superseded`, and the triggers that
replayed enter the first signal that runs again, in the journal's order. A trigger whose lane ran a call live is settled at once (what the lane does after that is live, so a recorded trigger of it that has not come does not come): the live signal does not wait for the end of that lane. A read of the plan whose changed revision a recorded signal took stands (read again, it would answer that signal's own plan as its base and bring no trigger), so the signal can replay, and the node waits for it before its release. Until the signals the earlier process recorded are decided, a node that no recorded signal concerns (a node that was not released, and is not a lane a recorded trigger or a released lane waits for) is not admitted, so it is released under the revision that stands after them and not under one they are about to supersede. A lane that was released under a superseded revision waits until that signal is decided, then is compared with the
revision that stands: a node whose slice is the one that revision was made against keeps its replay and a flow row `release_rebind` says so (its packet is still the one of the
revision it was built from); any other node is released again under the revision that stands - with a new packet when it had not started, from its first position (the position is live, so
is every later one of the lane) when it had started and was not accepted - and a node an acceptance rests on is refused (`accepted_node_changed`, with the route: a plan revision with
`update_node` or `dag-correct`). A milestone that closed with a reason to re-plan is `replanned`: the close runs every final step first, the audit and then the report up that carries the gate, the crosscheck and the reflection, and only then the re-plan; the revision is recorded and the owner thread is asked for the correction issues
(`crw-add-issue` and a plan revision).

**The run process's scheduler (CRW-1277).** The shipped binary installs the lane's operations in the run process (`cmd/crw` assigns
`cli.OpsFor = cli.SchedulerLane`): `ops.NewLane` over `cli.SchedRelay` (the `Relay` over `internal/relay/dagsched`), `CILocalVerifier`,
`StoreDecisions`, the host's threads and the reporter of F11 (`report.Reporter`: the report's rows in the run's journal, the notice
through `crw manage send-parent` on the relay the run is in), with the run's owner thread as the actor. The
milestone is the compiled milestone of the crw-plan handoff (`crw-flow-compile-input/1`) the run pinned with `crw flow start --handoff
FILE` (a night batch pins the one its decision names): the nodes of the plan at the revision the run pinned (`flow_runs.plan_revision`)
whose issues are the milestone's in the handoff, in the scheduler's offering order (`dagsched.OfferingOrder`: the longest chain below a
node, then the most nodes below it, then the node id), each waiting for the nodes of the milestone its plan edges come from, with the
criteria, regions and group of its issue; the milestone's title, criteria, surfaces, grade and required commands are its entry in
`milestones[]`, which compile judges (an issue without an entry and an entry without an issue are refused) and the run judges again by the same
entry rules (`compile.MilestoneEntryRefusals`) when it starts and when it reads the milestone; the verifier of the shipped lane runs
`crw-dev ci local` only, so an entry that requires another command is refused before the run starts (`CheckHandoff` with the shipped lane's
predicate `cli.ShippedRunnableCommand`, applied by `crw flow start --handoff`, the start of a night batch and `crw manage batch decide --handoff`)
and again when the run reads it; a verifier that runs other commands (`make e2e`) is the first real project's to decide (CRW-1209). A handoff that is not the
one the pinned revision was compiled from (an issue with no node, criteria whose digest is not the node's) is refused with "compile and
put the handoff first", and nothing is defaulted. Before its first fenced write the run claims the plan's coordinator epoch with its
run id as the session nonce (a resumed process is the same claim); a write after another session claimed the plan is refused
(`stale_coordinator_epoch`), writes nothing and is the error kind `epoch_lost` (`ops.ErrEpochLost`, whose message opens `refused:epoch_lost:`,
CRW-1285): a release answers it as `released: false` with the reason `epoch_lost`, and `project-lane.js` ends the lane whose fenced write it
was (`stopped`, reason `epoch_lost`: no escalation and no further write; a release refused so ends the lane `unreleased` with the reason and
the relay's detail) and, once its lanes have ended, the run with `{state: 'stopped', reason: 'epoch_lost', detail}` (a parallel lane stops at
its own next fenced write, refused the same way). The steps after the lanes (the close, the reflection, a re-plan, the report) end the run
the same way when one of them meets the error kind (CRW-1382), instead of ending it in a script error. `release` is `ReleaseAgent` (dag-release-request/1: the base from
the plan's target, the criteria `c1..cn` of the issue, the pinned handoff as the volatile input, the run's directory as the artifact
root, the rule version of the executor pair, the script and helper digests, the installed plugin's skills digest and the build, and
`expected_slice_digest`, the node's slice the flow compared; the answer names the slice of the manifest the relay committed); a
node the scheduler holds back (no slot, an overlapping region, a host short of memory, a node not ready) is `released: false` with
the reading's reason; the host memory bound is the one `dag-release` reads from `CRW_DAG_HOST_*`, sampled again for each release. `accept` is `Accept` on the agent path: the execution records a link the run points at the node's current
worktree before it asks, the run's buffered rows are stored first, and the run's journal stamps the release's execution on the
node's calls and the reviewer's input criteria digest on the reviewer's completed row, which the acceptance reads. `integrate` names the node's current acceptance and the head it accepted (anything else is refused before the batch, `acceptance_not_current` or `head_not_accepted`) and is one
`IntegrateBatch` of the node with the wiring of `crw relay dag-integrate` (`dagsched.IntegrationDeps`: `crw-dev ci local` over the
merged tree, the merged tree's own crw-dev when the configuration names a Go root, git's compare-and-swap of the branch) onto the plan's target branch, followed by the observation of the landing there;
the batch's crw-dev gets a HOME and TMPDIR of its own, a fresh `crw-<8 hex>` directory (mode 0700, removed after) directly below the
run process's TMPDIR resolved, which is the TMPDIR the run's start recorded (below), so it and the node's land-head `crw.verify`
(whose crw-dev inherits the run process's environment) both work below the run's directory whatever shell relaunched the run and
however long the directory is; there is no length rule and no other parent, and a directory that cannot be used is a host error
naming it;
`close` is `AgentClose`. A node sized `child` is released to no one: it goes up as a `size_split` escalation with detail
`child_retired` (the evidence carries the plan's `size_reason`), and no child is started.
