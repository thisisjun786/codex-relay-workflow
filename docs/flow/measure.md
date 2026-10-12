# Trace, shadow and measure: reading a recorded flow run offline

Three commands read what a flow run left in a copy of the relay store, and none of them calls an agent or writes to a store.

```text
crw flow trace export --store-copy PATH [--run RUN] --out FILE
crw flow shadow --trace FILE --script W [--run RUN] [--args JSON | --args-file FILE] [--out FILE]
crw flow measure --trace FILE --out FILE [--run RUN] [--oversized-tokens N]
```

`internal/flow/trace` is the `crw-flow-trace/1` file (its writer from a store copy, its strict reader, the live-store refusal)
and `internal/flow/shadow` is the comparator, the measurement reporter and the three commands. `internal/flow/trace/tracetest`
builds the synthetic store copies the tests use. The variant-screening simulator CRW-188b describes is a separate offline tool
decided after the first real pilot trace exists; it reads `crw-flow-trace/1` and nothing else, and the format below is its contract.

Exit status: 0 when the command did what it was asked; 1 when the script of `shadow` does not load or a replayed run ends in
`script_error` (the report is printed for the second); 2 for a command line it cannot use or a path it refuses.

## What the commands read, and what they never touch

- **The store copy.** `--store-copy` is a file made from a relay store in a temporary directory, with SQLite's own `VACUUM INTO`
  or its backup API (a plain `cp` of a store that is being written can leave committed frames in a `-wal` file that the copy
  does not have; a copy whose log holds frames without their index is refused). It is opened in place and read-only
  (`store.OpenInPlace`: `mode=ro`, or `immutable=1` when the copy has no log; no lock, no schema script, no sidecar file is
  created). The SHA-256 of the file is taken before and after the export and the export fails if they differ; the trace names that
  digest. Tables a store from before a zone does not have read as empty.
- **A live store is refused.** Every path a command is given (the store copy, the trace, the script, every `--out`) is judged by
  `trace.RefuseLive` before anything is read or written, and a refusal is status 2 naming the reason. A path is live when it lies
  under a live relay state directory (the selection the relay makes with no flags: `CODEX_SESSION_RELAY_STATE`, the configured
  `relay_state`, `XDG_STATE_HOME` or `~/.local/state`, `codex-session-relay`, scoped by the default socket, and the
  directory above it), or when it is the same file (device and inode) as a regular file under one of them: a hard link or a
  symbolic link to the live store is refused wherever it is placed. A store id cannot be the test, because a faithful copy has the
  id of the store it was made from. A path is judged in the spelling it was given: a relative path is taken from the working
  directory as written and a symbolic link followed by `..` leads where the filesystem leads (the relay keeps the same spelling
  of a configured `relay_state`), so neither the configured root nor an output path is cleaned into another directory first. The
  place a write lands (the directory the spelling leads to, then the last name) is judged as well as the file the path resolves to.
- **Outputs are never inputs.** `--out` may not be the store copy, the trace or the script. A file is written whole under a
  temporary name in its directory and renamed.

## crw-flow-trace/1

One JSON document (indented, the same bytes for the same store copy; unknown members, a second document after it, another
schema and journal rows out of order are errors of the reader):

| Member | Content |
| --- | --- |
| `schema` | `crw-flow-trace/1` |
| `source` | `digest` (SHA-256 of the store copy), `bytes`, `storeId` (the id in the copy's `schema_meta`) |
| `runs[]` | one per `flow_runs` row: `runId`, `workflow`, `scriptDigest`, `argsDigest`, `planId`, `planRevision`, `resumeFrom`, `startedAt`, then the members below |
| `runs[].events[]` | every journal row of the run in the order it was appended: `seq` (the event ordinal, run-wide), `kind` (`run`, `call` or `flow`), `event`, `callId`, `path`, `position`, `op`, `label`, `at` (when it was recorded) and `data` (the row's data as the engine journals it) |
| `runs[].nodes[]` | per node, its stage transitions oldest first: `stage`, `at`, `seq`, `callId`, `source`, `detail`. A call of a role starts `build` (`executor`, `executor-t1`), `review` (`reviewer`), `final_review`, `plan` (`architect`) or `explore`; `released` comes from `dag_agent_executions`, `accepted` from `dag_agent_acceptances`, `integrated` from the integration stage rows that mark the acceptance, `escalated` from `dag_agent_closures` |
| `runs[].calls[]` | per agent call, restored from its `flow_calls` rows alone (`engine.RestoreCallResult`): identity (`callId`, `path`, `position`, `label`), `role`, `family`, `node`, `model`, `effort`, `resumeOf`, `stepDecision`, `schemaDigest`, `threadId`, `status` (`open` for a call with no terminal row), `reason`, `tokens` (`input`, `output`, `total`), `tokensUnmeasured`, `repairUsed`, `turns`, the prepared and terminal rows and times, and `result` (the last agent message of a completed call) |
| `runs[].decisions[]` | the decisions recorded outside the call rows: `flow_spawn_decisions` rows, `flow_step_decisions` rows, and the escalations of `dag_agent_closures` |
| `journal[]` | the rows of the relay's own `journal` table (the log `journal-tail` reads) that belong to the lifecycle the trace follows, by `seq`: kinds that begin with `delivery_`, `dag_` or `flow_`, and `event_accepted`, `acknowledged`, `ack_verified`, `verdict_recorded`, `verdict_superseded`, `sync_enqueued`, `reconciled`. Each has `seq`, `at`, `kind`, `subject`; the detail is not carried |
| `deliveries[]` | per delivery: `eventId`, `relationshipId`, `recipientTaskId`, `state`, `holdReason`, `createdAt`, `firstSentAt` (the first send that counts), `sends`, `busyAnswers` |
| `acceptances[]` | agent acceptances (with `executionId` and the `reviewerCallId` they stand on) and relationship acceptances, with the time of each |
| `integration[]` | per batch: `intentAt`, `movedAt`, `landed`, and its members with `intentAt`, `markedAt`, `markPending` |
| `aux` | `syncOutbox` (rows by state) and `mergeTrains` (trains, members, events): the counts the R11 and R12 decisions look at |

A send is a delivery attempt that did not settle as withheld before the send or deferred busy (`store.RelationshipSendsSQL`
counts the same). A busy answer is a `delivery_deferred_busy` journal row or a busy-deferred attempt, summed as the delivery
policy sums them. A batch landed when it moved the target (a `ref_moved` stage or a batch row) or marked a member; a batch that
did neither failed.

`--run RUN` keeps one run, in `export`, `shadow` and `measure` alike (`export --run` gives what `--run` on the reader gives of the
whole export). The run's own record stays whole; the agent acceptances that stand on one of its reviewer calls, the acceptances of its
plan and nodes, and the integration batches of its plan (cut to its nodes) stay with it, so the audit of a healthy run is not failed
by the acceptances of another run. The deliveries, the relay journal rows and `aux` are the store's, not a run's (no row of them
names a run), and a selection keeps them whole.

## crw flow shadow

`shadow` replays the workflow script `W` over each recorded run and compares the decisions the script makes with the decisions the
run made. The script runs on the engine (`engine.Run`) with the run's own id, so a call has the id it had when it was recorded (the
call id is a function of the run id, the call's path and its position), and the backend (`shadow.Replay`) starts no thread and sends
no request: a turn of a call the trace recorded is answered with what the call came to (its last message and tokens, or the failure
the host reported, or no answer at all for an unknown outcome). The script sees what the recorded run saw: the message as the host
sent it (a call without a schema gets its text back as it was, not encoded a second time), a call that was repaired replays as
repaired (a first turn that does not satisfy the schema, then the answer on the repair turn, so `meta.repairUsed` and the turn
count are the recorded ones), and `crw.clock()` answers with the times the recorded run read, in the order it read them, never the
wall clock; a call the trace has no record of is answered by the engine's
deterministic fake and counted as `invented` in the report, and what follows an invented call is not evidence. A call that the
engine itself ended (a refusal of its own, a supersession) is decided again by the engine. `--args` are the script's arguments;
`argsMatch` says whether they are the ones the run recorded. The script of the run and `W` may differ: that is the question.

### Decisions

| Kind | The script's decision | The run's decision |
| --- | --- | --- |
| `spawn` | the `prepared` row of an agent call that does not continue a handle, with its role, role family and pair | the same rows of the recorded journal |
| `flush` | the same row when the call is a reviewer call whose output schema is the batch verdict (`VERDICT_BATCH`) | the same rows, and a `flow_spawn_decisions` row of kind `flush` that no journal row accounts for |
| `tier` | a `crw.tierStep` call, on the node of the `executor-t1` call that carries its step id, stepping to `executor-t1` | the same, and a `flow_step_decisions` row that no journal row accounts for |
| `escalation` | an escalation row of the engine, with its node and reason | the same, and an escalated `dag_agent_closures` row that no journal row accounts for |

A batch verdict is told by its schema digest, which `shadow` learns by sending the library's schema through the engine once, so a
recorded batch review is a flush whatever the script being replayed does. A recorded row is counted once per node and kind: it is
extra only when there are more rows than journal decisions.

### The closed set

Decisions are paired by key: a spawn by (node, role family, the nth of that in the node), or for a call with no node by (label, role
family, the nth); a flush, a tier step and an escalation by (kind, node, the nth). Each pair, and each decision that has no pair, is
exactly one of:

| Class | When |
| --- | --- |
| `same` | paired, with the same role and pair (spawn, flush), the same target (tier) or the same reason (escalation), and the same place among the decisions of its node |
| `different_order` | paired and otherwise equal, but its place among the paired decisions of its node differs. The order of the decisions of calls with no node is not compared: parallel calls finish in any order |
| `extra_spawn` | the script spawns (or flushes) and the run did not |
| `missing_spawn` | the run spawned (or flushed) and the script does not |
| `different_tier` | a spawn or flush paired with another role or pair (an `executor` where the run had `executor-t1`), or a tier step on one side only or with another target |
| `different_escalation` | an escalation on one side only, or paired with another reason |

The report (`crw-flow-shadow/1`) has, per run, `replay` (`status`, `replayed`, `invented`), the number of decisions on each side,
a `counts` object with all six classes (zero included) and the classified `items`, each with its `actual` and `computed`
decision; the top level sums the counts. The comparison is a Jun decision input (CRW-188, CRW-476) and changes no policy table.

## crw flow measure

`measure` reads only the trace and writes `crw-flow-measure/1`. Every figure is computed from the trace; a ratio whose
denominator is 0 is `null`.

The nine measures F17 reads:

| Key | Content |
| --- | --- |
| `tokens_per_accepted_issue` | nodes with an `accepted` transition, the tokens of all their calls, the mean, and the tokens by `run/node` |
| `time_to_first_acceptance_seconds` | per run, the seconds from `startedAt` to the first acceptance, and their distribution |
| `tier_failure_rates` | per role: calls, calls whose status is not `completed` and not `superseded`, the rate, and the count by status |
| `outputschema_compliance_ocx` | calls with an output schema that reached validation (completed, or `null_model` after the repair turn): answered at the first turn, after the repair turn, or not; `all`, `via_ocx` (a model whose name does not start with `claude`: this host runs Claude models directly and the others through ocx) and `by_model` |
| `oversized_threshold_hits` | per node, its tokens and whether it struggled (escalated, or an `executor-t1` call); with `--oversized-tokens N`, the nodes where "above N tokens" and "struggled" agree. Without it `threshold_tokens` and `hits` are `null` |
| `orchestrator_turns_per_receipt` | turns of `architect` calls over completed executor-family calls |
| `executor_thread_cache` | executor-family calls that started a thread, calls that continued a handle, turns, the reuse ratio and the turns per thread. The journal holds no cached input tokens; these are the evidence of thread reuse |
| `recovery_holds_per_run` | per run, the nodes that hold a call with an unknown outcome or whose later calls ended `recovery_hold` |
| `flow_audit_result` | the trace-level audit: every agent acceptance names a reviewer call that the trace holds, completed, with a reviewer role (`observed`, `unobserved`, `mismatch`, `ok`). `crw flow audit` itself reads the live store and is run by F17 (`live_audit_run` is `false`) |

The evidence F17 reads for its decisions:

| Key | Content |
| --- | --- |
| `contention` | exactly three values. `cas_retries`: batches that ran after a failed batch of their plan that shared a node with them. `wait_seconds`: the distribution, over landed members, of the time from the member's first frozen intent to its mark. `failed_batches`: batches that moved nothing and marked nothing |
| `r2` | `deliveries`, `undelivered` (no counted send) and `delivery_latency_seconds`, the distribution of the time from queueing a delivery to its first send |
| `r6` | `sends_per_relationship` (distinct events with a counted send), `sends_total`, `not_loaded_queued` (deliveries whose hold reason names `notLoaded`). The time from `report_undelivered` to the load needs the F11 delivery rows, which this build does not have |
| `r3b` | `busy_backoffs`, `events_with_backoff`, `max_backoffs_per_event` and `recipients_with_several_backing_off` |
| `r11` | `sync_outbox_rows` and their count by state |
| `r12` | the merge-train row counts |

A distribution is `count`, `sum`, `min`, `p50`, `p90`, `max` (nearest rank; the last four are `null` for `count` 0), in seconds.

## Tests

`tracetest.NewScenario` records a workflow with the flow journal on the engine's fake backend in a temporary store (an executor
build and a reviewer, a tier step, a build whose host never answers) and adds the DAG, delivery, integration, sync and merge-train
rows around it, and `Copy` makes a standalone copy with `VACUUM INTO`; no operational database is ever opened. The scripts in
`testdata/flow/shadow/` are the workflows the comparison tests replay, and `trace.golden.json` is the export of the scenario
(`CRW_UPDATE_GOLDEN=1` rewrites it).

- `TestTraceExportContract` (`internal/flow/trace`): the golden export, the strict reader, event ordinals, stage transitions, tokens, times.
- `TestShadowClassifiesMismatch`, `TestShadowRefusesLiveStore`, `TestShadowLeavesCopyUnchanged`, `TestIntegrationContentionReport`,
  `TestDeliveryLatencyReport` and `TestMeasureReportHasNineKeys` (`internal/flow/shadow`).

Real values come from a real run's trace after the 0.5.0-alpha install (F17).
