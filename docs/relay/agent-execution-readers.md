# Agent execution: the readers of the frozen tables

An agent node (R04) is released on the agent surface of `dag-release` and its rows are in `dag_agent_executions` and `dag_agent_closures` (and, from R05, `dag_agent_acceptances` and `dag_agent_integration_stages`). Nothing of it is ever written into a frozen table (`dag_releases`, `dag_node_executions`, `dag_acceptances`, `dag_verified_heads`, `dag_integration_stages`, `dag_acceptance_premerge`, `dag_acceptance_verifications`), so a reader of those tables that answers a question about a NODE sees only the relationship side of it. This page classifies every such reader, so the readers that look on both sides (`executionOf`, `releaseOf`, `acceptanceOf`, `agentIntegratedAt`, in `internal/relay/dagsched/agent_union.go`) have the list, and a reader that nobody classified fails a test.

## The set

The set is the non-test Go files of `internal/relay` and `internal/manage` that (a) spell one of the seven frozen tables in a string literal, or (b) use a function, constant or variable that reads one, to a fixed point: a function whose body or value spells a table in a query reads, and so does a function that uses a function, method, constant or variable that reads, at any depth (`ProgressSnapshotOf` calls `Progress`, which calls `nodeFacts`, which reads), so a file that only calls the outer wrapper of a chain is in the set. A statement that creates, drops or alters a table, trigger or index (the zone ledger, run at every open of the store) is not a read: its file is in the set, but the callers of the store's open are not. The calls followed are the uses `go/types` resolved; a call through an interface value or a function value is not followed. It is computed from the code with `go/ast` and `go/types` (the packages of the two trees are type-checked; any other package is an empty stub), for linux/amd64, and it is not a grep: a file that reads a table through `currentRelationshipOf`, `loadActiveAcceptance`, `commitAccepted`, `integrationRefsOf` or `standOf` never spells it. The number of files is not a criterion. `TestEveryZoneReaderClassified` (`internal/relay/dagsched/reader_classification_test.go`) recomputes the set and fails on a file without a row, a row for a file outside the set, a row without an owner, and a row whose class or owner differs from the design's classification table for the files that table names; `TestReaderSetIncludesHelperCallers` pins that a file that only calls a helper is in the set, `TestReaderSetFollowsWrappersToAFixedPoint` that a chain of wrappers (in a package, across packages, through methods, constants and a cycle) is followed to its end and that the zone ledger's statements are not reads, and `TestReaderSetOfTheTreeHoldsIndirectReaders` that `progress_replay.go` is in the set of the tree.

## The columns

* **class**: `node-scoped-release` and `node-scoped-acceptance` (joined by `+` when both) are readers that answer a question about a node and must see both sides; `agent-writer` is a path that writes the agent kind (R04 the release, R05 the acceptance and the integration); `agent-lifecycle` is R07's (handover, stale rework, restart, cancel); `relationship-scoped` names a relationship and stays as it is; `union` is the union readers themselves; `caller` reads no table of its own and follows the classified reader it calls; `store` is the zone.
* **owner**: the issue of the design that changes the file or confirms that it stays (R04 to R07); two issues are joined by a comma.
* **migrated**: `yes` or `no` for a node-scoped row, `n/a` for every other class. Every node-scoped row is `yes`: the reader answers about a node from the union readers (R06), and `TestEveryZoneReaderClassified` fails while one is `no`. The `note` of a row says how the reader looks on both sides.

## The rows

| file | class | owner | migrated | note |
|---|---|---|---|---|
| `internal/manage/capacity.go` | `caller` | `R06` | n/a | calls branchAttach and capacityZoneReason of capacity_branches.go |
| `internal/manage/capacity_branches.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | yes | branch candidates count the nodes a release or an execution of either side owns (dagsched.ExecutedNodes) and ask the scheduler whether a node landed, through the current acceptance of either side (dagsched.CurrentAcceptance, AgentAcceptanceIntegrated) |
| `internal/manage/capacity_signals.go` | `caller` | `R06` | n/a | reads the zone tables of capacity_branches.go through branchZoneTables and follows it |
| `internal/manage/checkpoint.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | yes | the checkpoint counts the integrations of relationships and of agent acceptances (dagsched.CurrentAcceptance, AgentAcceptanceIntegrated), each with its instant |
| `internal/manage/dagreview.go` | `node-scoped-release` | `R06` | yes | the review takes a node's release, executions and stage from the scheduler's reading, which lists an agent node's executions; nothing of its own reads a frozen table for a node |
| `internal/manage/dagreview_store.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | yes | the review store reads the current acceptances and the observations of either side; the executions that tie a child to the plan are relationships' (an agent node has no child) |
| `internal/manage/improve.go` | `caller` | `R06` | n/a | the improvement run: calls improveRunCollect and improveRunRoadmap |
| `internal/manage/improve_collect.go` | `caller` | `R06` | n/a | collects the measurements of a plan through Scheduler.Measure (measure.go) |
| `internal/manage/improve_run.go` | `caller` | `R06` | n/a | calls improveRunCollect |
| `internal/manage/pump_signals.go` | `caller` | `R06` | n/a | the pump signals call DagReview (dagreview.go) and capacityEvaluate |
| `internal/manage/relayread.go` | `node-scoped-release` | `R06` | yes | the manage read of a plan is the scheduler's Progress, which reads both sides; the selection of relationships by plan is relationship-scoped (an agent node has none) |
| `internal/relay/acceptance/stand.go` | `node-scoped-acceptance` | `R06` | yes | the head an acceptance stands on (standOf): OwnOfAgent is what an agent acceptance stands on, itself; no base refresh is recorded for it |
| `internal/relay/childcleanup/command.go` | `caller` | `R06` | n/a | the cleanup command runs the gate of gate.go through Execute |
| `internal/relay/childcleanup/gate.go` | `relationship-scoped` | `R06` | n/a | asks ExecutionIntegrated of a relationship's merged mark, not of a node |
| `internal/relay/dagsched/accept.go` | `agent-writer` | `R05` | n/a | dag-accept: the agent path reads the current agent execution and writes the agent acceptance row |
| `internal/relay/dagsched/accept_agent.go` | `agent-writer` | `R05` | n/a | the agent path of dag-accept: the reviewer verdict and the author pairs of the run (flow_calls), the commit proof read from git, the pre-merge and verification bodies stored in the agent acceptance row |
| `internal/relay/dagsched/accept_commit.go` | `relationship-scoped` | `R06` | n/a | the commit acceptance of a relationship; agents use accept_agent |
| `internal/relay/dagsched/accept_premerge.go` | `relationship-scoped` | `R06` | n/a | the pre-merge record of a relationship's acceptance; the agent body is judged by the same function and stored in the agent row |
| `internal/relay/dagsched/accept_refresh.go` | `node-scoped-acceptance` | `R06` | yes | the proof dag-accept records for a head a ruling verified reads the node's current acceptance of either side (an agent acceptance stands on itself, so no refresh stands) |
| `internal/relay/dagsched/activity.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | yes | the evidence of a node's activity: the acceptance, verification and pre-merge entries of an agent acceptance beside those of a relationship, and its commit link |
| `internal/relay/dagsched/activity_cli.go` | `caller` | `R06` | n/a | dag-activity: calls Scheduler.ReadActivity (activity.go) |
| `internal/relay/dagsched/agent_union.go` | `union` | `R04` | n/a | executionOf and releaseOf: the readers that look on both sides |
| `internal/relay/dagsched/baserefresh.go` | `node-scoped-acceptance` | `R06` | yes | standOf and stoodOn: an agent acceptance stands on itself; dag-base-refresh refuses an agent acceptance; the contributor heads of other nodes include agent acceptances |
| `internal/relay/dagsched/bundles.go` | `node-scoped-release` | `R06` | yes | bundle candidates count the nodes a release of either side owns (an agent release is a row of its chain of kind initial or correction) |
| `internal/relay/dagsched/candidates.go` | `agent-writer` | `R05` | n/a | the integration candidates: the closure-free current agent execution and its acceptance are candidates |
| `internal/relay/dagsched/cli.go` | `agent-writer` | `R04` | n/a | dag-release --surface agent |
| `internal/relay/dagsched/commit_targets.go` | `agent-writer` | `R05` | n/a | commitAccepted is true for an agent acceptance; integrationRefsOf joins agent node intents to the batch rows |
| `internal/relay/dagsched/correction.go` | `relationship-scoped` | `R06` | n/a | the next generation of the same relationship; agents use agent_correction |
| `internal/relay/dagsched/correction_decision.go` | `relationship-scoped` | `R06` | n/a | the decision-opened generation of a relationship |
| `internal/relay/dagsched/edges.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | yes | edge satisfaction through the current acceptance of either side: standing (agentStanding), consumedStanding, recordedEvidence, integratedAt (agentIntegratedAt) and the integrated edges of a commit-accepted node |
| `internal/relay/dagsched/epoch.go` | `agent-lifecycle` | `R07` | n/a | Adopt appends a parent_handover execution for an agent node |
| `internal/relay/dagsched/epoch_cli.go` | `caller` | `R07` | n/a | dag-adopt: calls Adopt |
| `internal/relay/dagsched/handopen_guard.go` | `relationship-scoped` | `R06` | n/a | a generation opened by hand on a relationship |
| `internal/relay/dagsched/inputs.go` | `node-scoped-acceptance` | `R06` | yes | reads the acceptance a manifest input rests on by its id of either side; an agent acceptance hands over a commit and no list of artifacts |
| `internal/relay/dagsched/integration.go` | `agent-writer` | `R05` | n/a | the integration observation: the agent path reads the agent marked row and returns the slot |
| `internal/relay/dagsched/integration_batch.go` | `agent-writer` | `R05` | n/a | dag-integrate: node-level stage rows of an agent candidate go to the agent stage table |
| `internal/relay/dagsched/integration_push.go` | `agent-writer` | `R05` | n/a | VerifiedMoveOnto reads the batch-level rows only; no code change, a test pins it |
| `internal/relay/dagsched/integration_stale.go` | `node-scoped-acceptance` | `R06` | yes | the nodes of a pushed head whose acceptance no longer stands: the marked rows of agent acceptances beside the relationship's, and the node's current acceptance of either side |
| `internal/relay/dagsched/invalidation.go` | `node-scoped-acceptance+agent-lifecycle` | `R06,R07` | yes | reads stale acceptances of either side (R06); the route of an agent node is agent_correction (R07) |
| `internal/relay/dagsched/landingresult.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | yes | the results a parent records after a landing: a node that ever had a head or an execution on either side can be stated about |
| `internal/relay/dagsched/lifecycle.go` | `node-scoped-acceptance` | `R06` | yes | pause, cancel and archive of a node read its acceptance of either side, and the acceptances its manifest rests on |
| `internal/relay/dagsched/liveheads.go` | `node-scoped-release` | `R06` | yes | the heads of the nodes that are running: an agent node's accepted head while its execution stands, else the HEAD of the worktree its execution records; the base of its manifest |
| `internal/relay/dagsched/livesweep.go` | `node-scoped-release` | `R06` | yes | the conflict sweep of the running nodes: the checkout is also one an agent acceptance of the plan was proved in |
| `internal/relay/dagsched/manifest.go` | `node-scoped-acceptance` | `R06` | yes | builds the manifest input of a predecessor's acceptance of either side (an agent input names the acceptance and the execution, dag.InputSurfaceAgent) |
| `internal/relay/dagsched/measure.go` | `node-scoped-release` | `R06` | yes | the measurements of the nodes' executions: the landings of both sides, the corrections of both chains, the nodes cancelled after an execution of either side |
| `internal/relay/dagsched/mergecheck.go` | `relationship-scoped` | `R06` | n/a | the merge checks of an accepted pull request |
| `internal/relay/dagsched/mergechecks.go` | `node-scoped-acceptance` | `R06` | yes | the history of merge checks per acceptance: an agent acceptance has none, because the merge lane refuses it (mergejudge.go) |
| `internal/relay/dagsched/mergejudge.go` | `node-scoped-acceptance` | `R06` | yes | merge judgement of an accepted node: an agent acceptance is refused, since an agent node lands through dag-integrate and never as a pull request; a predecessor's landing is asked of either side |
| `internal/relay/dagsched/mergeorder_derive.go` | `node-scoped-release` | `R06` | yes | the merge order among the nodes that hold a release or an execution of either side; an agent node is in the lane by its accepted head and never has a turn |
| `internal/relay/dagsched/nodestate.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | yes | the derived state of a node (stateOf) from the current execution and acceptance of either side |
| `internal/relay/dagsched/optimism.go` | `node-scoped-acceptance` | `R06` | yes | local-optimistic release reads the landings of both sides |
| `internal/relay/dagsched/outbox.go` | `node-scoped-acceptance` | `R06` | yes | the summary outbox calls Scheduler.Progress and reads an issue's integration batch from the stage rows of the acceptance of either side (acceptanceOf's id); the surface of a node is executionOf's |
| `internal/relay/dagsched/outbox_cli.go` | `caller` | `R06` | n/a | dag-summary commands: call EnqueueSummary and SummaryStatus (outbox.go) |
| `internal/relay/dagsched/pass.go` | `caller` | `R06` | n/a | a dag-ready pass calls Scheduler.Ready (ready.go) |
| `internal/relay/dagsched/policy_cli.go` | `caller` | `R06` | n/a | dag-landing-result-record: calls RecordLandingResult |
| `internal/relay/dagsched/progress.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | yes | dag-progress: the acceptance, the executions and the integration of an agent node, and the nodes with an agent acceptance outside the denominator |
| `internal/relay/dagsched/progress_cli.go` | `caller` | `R06` | n/a | dag-progress: calls Scheduler.ReadProgress (progress.go) |
| `internal/relay/dagsched/progress_replay.go` | `node-scoped-acceptance` | `R06` | yes | the view over the plan log and acceptance state: ProgressSnapshotOf calls Progress, which reads both sides, and carries the agent node's facts |
| `internal/relay/dagsched/ready.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | yes | the ready set (stateOf, resourceHold) over the nodes of either side |
| `internal/relay/dagsched/regions.go` | `caller` | `R06` | n/a | DeclareRegions asks stateOf whether a node holds its regions |
| `internal/relay/dagsched/release.go` | `agent-writer` | `R04` | n/a | ReleaseAgent; the relationship release refuses live_agent_execution |
| `internal/relay/dagsched/release_recovery.go` | `node-scoped-release` | `R06` | yes | skips agent rows: an agent execution is not a managed recovery target (OpenReleaseNodes, dag-release-close), and the manifest it rests on is relied on |
| `internal/relay/dagsched/revalidation.go` | `agent-lifecycle` | `R07` | n/a | the SAME-child rule applies to relationship nodes only; an agent node's next attempt is another execution row |
| `internal/relay/dagsched/stagnation.go` | `node-scoped-acceptance` | `R06` | yes | the stagnation counter of a node reads its acceptance of either side |
| `internal/relay/dagsched/targets.go` | `node-scoped-acceptance` | `R06` | yes | nodeTargets: where a node's accepted head has to land, for an acceptance of either side (an agent acceptance is a commit acceptance) |
| `internal/relay/dagsched/withdraw.go` | `relationship-scoped` | `R06` | n/a | withdraws a generation of a relationship |
| `internal/relay/delivery/ack.go` | `relationship-scoped` | `R06` | n/a | delivery acknowledgement records the verified head of a relationship |
| `internal/relay/delivery/cli.go` | `caller` | `R06` | n/a | delivery commands call Ack.RecordVerdict (ack.go) and the receive path |
| `internal/relay/delivery/receive.go` | `caller` | `R06` | n/a | a received verdict is recorded through PreparedVerdict.Record (ack.go) |
| `internal/relay/delivery/sync.go` | `relationship-scoped` | `R06` | n/a | asks whether a plan runs the relationship (a dag_node_executions row of that relationship on a node dag_nodes still holds live) to skip the sync_outbox row of its verdict (the summary outbox carries it), and which confirmed summary entry carries a verdict (PlanSummaryCarriesVerdict, read by the supervisor's discharge) |
| `internal/relay/integrate/cli.go` | `caller` | `R05,R06` | n/a | dag-integrate and its readers call Scheduler.IntegrateBatch (integration_batch.go, R05), StaleIntegratedNodes (integration_stale.go) and VerifiedMoveOnto (integration_push.go) |
| `internal/relay/mergeturn/check.go` | `relationship-scoped` | `R06` | n/a | the merge turn refuses an under-correction relationship |
| `internal/relay/mergeturn/commands.go` | `caller` | `R06` | n/a | the merge-turn commands call Service.Check, Land, Open, TrainLand and Verify |
| `internal/relay/mergeturn/outcome.go` | `relationship-scoped` | `R06` | n/a | the outcome of a merge turn |
| `internal/relay/mergeturn/train.go` | `relationship-scoped` | `R06` | n/a | the merge train |
| `internal/relay/mergeturn/undercorrection.go` | `relationship-scoped` | `R06` | n/a | a relationship under correction |
| `internal/relay/mergeturn/undercorrection_match.go` | `relationship-scoped` | `R06` | n/a | the match of a relationship's standing acceptance |
| `internal/relay/mergeturn/undercorrection_standing.go` | `relationship-scoped` | `R06` | n/a | the standing acceptances of a relationship |
| `internal/relay/registry/cli.go` | `caller` | `R06` | n/a | registry commands call Registry.CloseMerged and the linkage commands |
| `internal/relay/registry/linkage_cli.go` | `caller` | `R06` | n/a | the linkage commands call Registry.HandoverScopeKind |
| `internal/relay/registry/linkage_write.go` | `caller` | `R06` | n/a | a linkage write asks whether a relationship is settled (readSettled, settled.go) |
| `internal/relay/registry/restson.go` | `relationship-scoped` | `R06` | n/a | what a relationship's registration rests on |
| `internal/relay/registry/settled.go` | `relationship-scoped` | `R06` | n/a | whether a relationship is settled |
| `internal/relay/store/dag_integration_stages.go` | `store` | `R05` | n/a | the row writer of dag_integration_stages: batch-level rows of every batch |
| `internal/relay/store/dag_zone.go` | `store` | `R04,R05` | n/a | the zone ledger: DDL, appended by R04 and R05 |
| `internal/relay/store/domain_rows.go` | `store` | `R06` | n/a | frozen row types for port parity; unchanged |

## Named by the design and outside the set

The design's classification table (relay.added) also names `internal/relay/dagsched/capacity.go` (the slots a node holds) and `internal/relay/dagsched/digest.go` (the `Acceptance` record and its digest) as node-scoped readers for R06. Neither spells a frozen table or uses a function that reads one, so they are not in the computed set and have no row above. `digest.go` is where the `Acceptance` record carries the row of an agent acceptance (its unexported `agent` field), which is how the readers above that need only the fields the two kinds share keep one code path; `AcceptanceDigest` is the digest of a relationship's row, and an agent acceptance's id is the key `AgentAcceptanceID` computes. `capacity.go` needs no change: the slot of an agent release is an `execution_slots` row of the same subject kind as a relationship's, so the capacity the reading counts, and `slotOf`, already hold it (`TestAgentReleaseVisibleToReadyAndCapacity`). (`progress_replay.go`, the third file the table names, reaches the tables through `Progress` and is in the set, with its row.)

## How a node-scoped reader looks on both sides

* The node's current execution and release are `executionOf` and `releaseOf`; its current acceptance is `acceptanceOf`, or `activeAcceptanceOf` when the reader needs only the fields the two kinds share (the head, the manifest, the criteria, the repository, who accepted it and when). A reader whose failure would take the reading of a whole plan down with it (the derived state, the facts of `dag-progress`, the staleness judgement) reads a node that holds both kinds as neither (`tolerantAcceptanceOf`, an ambiguous state), as the candidates do.
* An agent acceptance stands on itself (`acceptance.OwnOfAgent`). Its integration is `agentIntegratedAt` (the agent marked row and the observations of its head), reached from `integratedAt`, and the refs it completes on are `integrationRefsOf`, so `nodeIntegrated`, the edges and `dag-progress` judge it as they judge a relationship.
* The merge lane is a relationship's: an agent acceptance is refused by the merge judgement and never has a merge check or a turn, and a base refresh is never recorded for it.
