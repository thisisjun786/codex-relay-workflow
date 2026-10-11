# Agent execution: the readers of the frozen tables

An agent node (R04) is released on the agent surface of `dag-release` and its rows are in `dag_agent_executions` and `dag_agent_closures` (and, from R05, `dag_agent_acceptances` and `dag_agent_integration_stages`). Nothing of it is ever written into a frozen table (`dag_releases`, `dag_node_executions`, `dag_acceptances`, `dag_verified_heads`, `dag_integration_stages`, `dag_acceptance_premerge`, `dag_acceptance_verifications`), so a reader of those tables that answers a question about a NODE sees only the relationship side of it. This page classifies every such reader, so the issue that moves one to the union readers (`executionOf`, `releaseOf`, `acceptanceOf`, `agentIntegratedAt`, in `internal/relay/dagsched/agent_union.go`) has the list, and a reader that nobody classified fails a test.

## The set

The set is the non-test Go files of `internal/relay` and `internal/manage` that (a) spell one of the seven frozen tables in a string literal, or (b) use a function, constant or variable that reads one, to a fixed point: a function whose body or value spells a table in a query reads, and so does a function that uses a function, method, constant or variable that reads, at any depth (`ProgressSnapshotOf` calls `Progress`, which calls `nodeFacts`, which reads), so a file that only calls the outer wrapper of a chain is in the set. A statement that creates, drops or alters a table, trigger or index (the zone ledger, run at every open of the store) is not a read: its file is in the set, but the callers of the store's open are not. The calls followed are the uses `go/types` resolved; a call through an interface value or a function value is not followed. It is computed from the code with `go/ast` and `go/types` (the packages of the two trees are type-checked; any other package is an empty stub), for linux/amd64, and it is not a grep: a file that reads a table through `currentRelationshipOf`, `loadActiveAcceptance`, `commitAccepted`, `integrationRefsOf` or `standOf` never spells it. The number of files is not a criterion. `TestEveryZoneReaderClassified` (`internal/relay/dagsched/reader_classification_test.go`) recomputes the set and fails on a file without a row, a row for a file outside the set, a row without an owner, and a row whose class or owner differs from the design's classification table for the files that table names; `TestReaderSetIncludesHelperCallers` pins that a file that only calls a helper is in the set, `TestReaderSetFollowsWrappersToAFixedPoint` that a chain of wrappers (in a package, across packages, through methods, constants and a cycle) is followed to its end and that the zone ledger's statements are not reads, and `TestReaderSetOfTheTreeHoldsIndirectReaders` that `progress_replay.go` is in the set of the tree.

## The columns

* **class**: `node-scoped-release` and `node-scoped-acceptance` (joined by `+` when both) are readers that answer a question about a node and must see both sides; `agent-writer` is a path that writes the agent kind (R04 the release, R05 the acceptance and the integration); `agent-lifecycle` is R07's (handover, stale rework, restart, cancel); `relationship-scoped` names a relationship and stays as it is; `union` is the union readers themselves; `caller` reads no table of its own and follows the classified reader it calls; `store` is the zone.
* **owner**: the issue of the design that changes the file or confirms that it stays (R04 to R07); two issues are joined by a comma.
* **migrated**: `yes` or `no` for a node-scoped row, `n/a` for every other class. R06 sets the node-scoped rows to `yes` as it moves them to the union readers, and its test fails while one is `no`.

## The rows

| file | class | owner | migrated | note |
|---|---|---|---|---|
| `internal/manage/capacity.go` | `caller` | `R06` | n/a | calls branchAttach and capacityZoneReason of capacity_branches.go |
| `internal/manage/capacity_branches.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | no | branch candidates count the nodes a release or an execution owns and ask whether a node's execution landed |
| `internal/manage/capacity_signals.go` | `caller` | `R06` | n/a | reads the zone tables of capacity_branches.go through branchZoneTables and follows it |
| `internal/manage/checkpoint.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | no | the checkpoint of a plan's nodes: executions, acceptances, integration |
| `internal/manage/dagreview.go` | `node-scoped-release` | `R06` | no | asks whether a node's execution exists and reads its lanes |
| `internal/manage/dagreview_store.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | no | the review store's read of a node's execution and acceptance |
| `internal/manage/improve.go` | `caller` | `R06` | n/a | the improvement run: calls improveRunCollect and improveRunRoadmap |
| `internal/manage/improve_collect.go` | `caller` | `R06` | n/a | collects the measurements of a plan through Scheduler.Measure (measure.go) |
| `internal/manage/improve_run.go` | `caller` | `R06` | n/a | calls improveRunCollect |
| `internal/manage/pump_signals.go` | `caller` | `R06` | n/a | the pump signals call DagReview (dagreview.go) and capacityEvaluate |
| `internal/manage/relayread.go` | `node-scoped-release` | `R06` | no | the manage read of a node's executions |
| `internal/relay/acceptance/stand.go` | `node-scoped-acceptance` | `R06` | no | the head an acceptance stands on (standOf); an agent acceptance stands on itself |
| `internal/relay/childcleanup/command.go` | `caller` | `R06` | n/a | the cleanup command runs the gate of gate.go through Execute |
| `internal/relay/childcleanup/gate.go` | `relationship-scoped` | `R06` | n/a | asks ExecutionIntegrated of a relationship's merged mark, not of a node |
| `internal/relay/dagsched/accept.go` | `agent-writer` | `R05` | n/a | dag-accept: the agent path reads the current agent execution and writes the agent acceptance row |
| `internal/relay/dagsched/accept_agent.go` | `agent-writer` | `R05` | n/a | the agent path of dag-accept: the reviewer verdict and the author pairs of the run (flow_calls), the commit proof read from git, the pre-merge and verification bodies stored in the agent acceptance row |
| `internal/relay/dagsched/accept_commit.go` | `relationship-scoped` | `R06` | n/a | the commit acceptance of a relationship; agents use accept_agent |
| `internal/relay/dagsched/accept_premerge.go` | `relationship-scoped` | `R06` | n/a | the pre-merge record of a relationship's acceptance; the agent body is judged by the same function and stored in the agent row |
| `internal/relay/dagsched/accept_refresh.go` | `node-scoped-acceptance` | `R06` | no | the proof dag-accept records for a head a ruling verified |
| `internal/relay/dagsched/activity.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | no | the evidence of a node's activity (acceptance, verification, premerge, stages, verified heads) |
| `internal/relay/dagsched/activity_cli.go` | `caller` | `R06` | n/a | dag-activity: calls Scheduler.ReadActivity (activity.go) |
| `internal/relay/dagsched/agent_union.go` | `union` | `R04` | n/a | executionOf and releaseOf: the readers that look on both sides |
| `internal/relay/dagsched/baserefresh.go` | `node-scoped-acceptance` | `R06` | no | a base refresh of an accepted node (standOf); an agent acceptance stands on itself |
| `internal/relay/dagsched/bundles.go` | `node-scoped-release` | `R06` | no | bundle candidates count the nodes a release owns |
| `internal/relay/dagsched/candidates.go` | `agent-writer` | `R05` | n/a | the integration candidates: the closure-free current agent execution and its acceptance are candidates |
| `internal/relay/dagsched/cli.go` | `agent-writer` | `R04` | n/a | dag-release --surface agent |
| `internal/relay/dagsched/commit_targets.go` | `agent-writer` | `R05` | n/a | commitAccepted is true for an agent acceptance; integrationRefsOf joins agent node intents to the batch rows |
| `internal/relay/dagsched/correction.go` | `relationship-scoped` | `R06` | n/a | the next generation of the same relationship; agents use agent_correction |
| `internal/relay/dagsched/correction_decision.go` | `relationship-scoped` | `R06` | n/a | the decision-opened generation of a relationship |
| `internal/relay/dagsched/edges.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | no | edge satisfaction: currentRelationshipOf, integratedEdge, commitIntegratedEdge, standing |
| `internal/relay/dagsched/epoch.go` | `agent-lifecycle` | `R07` | n/a | Adopt appends a parent_handover execution for an agent node |
| `internal/relay/dagsched/epoch_cli.go` | `caller` | `R07` | n/a | dag-adopt: calls Adopt |
| `internal/relay/dagsched/handopen_guard.go` | `relationship-scoped` | `R06` | n/a | a generation opened by hand on a relationship |
| `internal/relay/dagsched/inputs.go` | `node-scoped-acceptance` | `R06` | no | reads the acceptance a manifest input rests on by its id (not named by the design's table) |
| `internal/relay/dagsched/integration.go` | `agent-writer` | `R05` | n/a | the integration observation: the agent path reads the agent marked row and returns the slot |
| `internal/relay/dagsched/integration_batch.go` | `agent-writer` | `R05` | n/a | dag-integrate: node-level stage rows of an agent candidate go to the agent stage table |
| `internal/relay/dagsched/integration_push.go` | `agent-writer` | `R05` | n/a | VerifiedMoveOnto reads the batch-level rows only; no code change, a test pins it |
| `internal/relay/dagsched/integration_stale.go` | `node-scoped-acceptance` | `R06` | no | the nodes of a pushed head whose acceptance no longer stands (not named by the design's table) |
| `internal/relay/dagsched/invalidation.go` | `node-scoped-acceptance+agent-lifecycle` | `R06,R07` | no | reads stale acceptances (R06); the route of an agent node is agent_correction (R07) |
| `internal/relay/dagsched/landingresult.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | no | the results a parent records after a landing |
| `internal/relay/dagsched/lifecycle.go` | `node-scoped-acceptance` | `R06` | no | pause, cancel and archive of a node read its acceptance |
| `internal/relay/dagsched/liveheads.go` | `node-scoped-release` | `R06` | no | the heads of the nodes that are running |
| `internal/relay/dagsched/livesweep.go` | `node-scoped-release` | `R06` | no | the conflict sweep of the running nodes |
| `internal/relay/dagsched/manifest.go` | `node-scoped-acceptance` | `R06` | no | builds the manifest input of a predecessor's acceptance (not named by the design's table) |
| `internal/relay/dagsched/measure.go` | `node-scoped-release` | `R06` | no | the measurements of the nodes' executions |
| `internal/relay/dagsched/mergecheck.go` | `relationship-scoped` | `R06` | n/a | the merge checks of an accepted pull request |
| `internal/relay/dagsched/mergechecks.go` | `node-scoped-acceptance` | `R06` | no | the history of merge checks per acceptance |
| `internal/relay/dagsched/mergejudge.go` | `node-scoped-acceptance` | `R06` | no | merge judgement of an accepted node |
| `internal/relay/dagsched/mergeorder_derive.go` | `node-scoped-release` | `R06` | no | the merge order among the nodes that hold a release or an execution |
| `internal/relay/dagsched/nodestate.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | no | the derived state of a node (stateOf); a current agent execution already holds the node and its regions there (R04), the rest of the union is R06 |
| `internal/relay/dagsched/optimism.go` | `node-scoped-acceptance` | `R06` | no | local-optimistic release reads landings |
| `internal/relay/dagsched/outbox.go` | `caller` | `R06` | n/a | the summary outbox calls Scheduler.Progress |
| `internal/relay/dagsched/outbox_cli.go` | `caller` | `R06` | n/a | dag-summary commands: call EnqueueSummary and SummaryStatus (outbox.go) |
| `internal/relay/dagsched/pass.go` | `caller` | `R06` | n/a | a dag-ready pass calls Scheduler.Ready (ready.go) |
| `internal/relay/dagsched/policy_cli.go` | `caller` | `R06` | n/a | dag-landing-result-record: calls RecordLandingResult |
| `internal/relay/dagsched/progress.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | no | dag-progress |
| `internal/relay/dagsched/progress_cli.go` | `caller` | `R06` | n/a | dag-progress: calls Scheduler.ReadProgress (progress.go) |
| `internal/relay/dagsched/progress_replay.go` | `node-scoped-acceptance` | `R06` | no | the view over the plan log and acceptance state: ProgressSnapshotOf calls Progress, which reads the acceptances; named by the design's table |
| `internal/relay/dagsched/ready.go` | `node-scoped-release+node-scoped-acceptance` | `R06` | no | the ready set (stateOf, resourceHold) |
| `internal/relay/dagsched/regions.go` | `caller` | `R06` | n/a | DeclareRegions asks stateOf whether a node holds its regions |
| `internal/relay/dagsched/release.go` | `agent-writer` | `R04` | n/a | ReleaseAgent; the relationship release refuses live_agent_execution |
| `internal/relay/dagsched/release_recovery.go` | `node-scoped-release` | `R06` | no | skips agent rows: an agent execution is not a managed recovery target |
| `internal/relay/dagsched/revalidation.go` | `agent-lifecycle` | `R07` | n/a | the SAME-child rule applies to relationship nodes only; an agent node's next attempt is another execution row |
| `internal/relay/dagsched/stagnation.go` | `node-scoped-acceptance` | `R06` | no | the stagnation counter of a node reads its acceptance |
| `internal/relay/dagsched/targets.go` | `node-scoped-acceptance` | `R06` | no | nodeTargets: where a node's accepted head has to land |
| `internal/relay/dagsched/withdraw.go` | `relationship-scoped` | `R06` | n/a | withdraws a generation of a relationship |
| `internal/relay/delivery/ack.go` | `relationship-scoped` | `R06` | n/a | delivery acknowledgement records the verified head of a relationship |
| `internal/relay/delivery/cli.go` | `caller` | `R06` | n/a | delivery commands call Ack.RecordVerdict (ack.go) and the receive path |
| `internal/relay/delivery/receive.go` | `caller` | `R06` | n/a | a received verdict is recorded through PreparedVerdict.Record (ack.go) |
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

The design's classification table (relay.added) also names `internal/relay/dagsched/capacity.go` (the slots a node holds) and `internal/relay/dagsched/digest.go` (the `Acceptance` record and its digest) as node-scoped readers for R06. Neither spells a frozen table or uses a function that reads one, so they are not in the computed set and have no row above; R06 moves them with the rows it owns. (`progress_replay.go`, the third file the table names, reaches the tables through `Progress` and is in the set, with its row.)
