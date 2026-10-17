
## Globals

`agent`, `agent.call`, `parallel`, `pipeline`, `phase`, `log`, `args`, `budget`, `workflow`, and `crw`.

| Global | Behaviour |
| --- | --- |
| `agent(prompt, opts)` | Claude-compatible wrapper, defined as `(await agent.call(prompt, opts)).output`: the schema-valid output, the final text for a call without a schema, and `null` when the call did not complete. |
| `agent.call(prompt, opts)` | The CRW primitive: resolves with a `CallResult`. |
| `parallel(thunks, opts?)` | Runs the functions concurrently and resolves with their results in input order. A function that throws contributes `null`. It is a barrier: it settles when every function has. |
| `pipeline(items, ...stages, opts?)` | Runs each item through all stages independently and with no barrier between stages. A stage receives `(previous, item, index)`; a stage that throws makes that item `null` and skips its remaining stages. |
| `phase(title)`, `log(message)` | Journal a phase or log row. |
| `args` | The `--args` value. |
| `budget` | `{total, run, spent(), remaining()}`: the run's token ceiling (`total` is `null` without one), the token budget the run's first process started with (`run`, `null` without one), the tokens spent and the tokens left (`Infinity` without a ceiling). A resumed run counts what its earlier processes spent (`Config.SpentBefore`, from the terminal rows of its journal), with a spec or without one, so `spent()` is the run's figure (the `--token-budget` of a resume without a spec is what that resume may spend on top of it). `total` is this process's ceiling, which a resume can narrow; `run` is what the first started row of the run records (`token_budget`, each process's started row records its own ceiling), so it is the same in every process of the run and a script decision made on it replays (CRW-1327). At 80% the engine journals an info row (`budget_warning`, once for the run: a resumed run whose journal holds it writes no second one); at 100% the run is paused and a call that would send a request ends `held` with reason `budget_exhausted`. A completed turn whose usage the backend never received counts as unmeasured, not as zero: the engine journals a flow row `tokens_unmeasured`, and under a ceiling pauses the run the same way. |
| `workflow(nameOrRef, args)` | Runs another script inline as one step and resolves with what it returns. The called script shares the run's pool, journal and budget, and its calls live on a child path. Nesting is one level: `workflow()` inside a called script throws. `crw flow run` resolves a name to `<name>.js` and `{scriptPath}` to that relative path, inside the script's directory. The child's `workflow_opened` row records its digest (`script_digest`) and how the run referred to it (`ref`: `name` or `script`), and a resume resolves the child the same way and refuses a run whose child changed or is gone (CRW-1461); a row written before `ref` was recorded is resolved both ways, and when a name and a scriptPath of the same text read different sources the resume is refused with both named. The engine makes this refusal itself, before anything is written, and `crw flow`'s run process has no check of its own beside it: a night batch's project lane under a binary whose project lane changed would otherwise run its architect and critic live again. |
| `crw.classify(result)` | Maps an executor `CallResult` to one outcome state (below). |
| `crw.clock()` | The time as an RFC 3339 string, journaled. It occupies a position of its path like a call. |
| `crw.tierStep(node, evidence)` | Mints the step decision: it journals the call and resolves with `{id, checkpoint}`; a fresh `executor-t1` call carries `id` as `stepDecision`. It occupies a position of its path. The engine's own call holds the id only (its `checkpoint` is `null` and the evidence is recorded by its digest); the project lane's `crw.tierStep` ([below](project-lane.md#the-project-lane)) wraps it, appends the `flow_step_decisions` row with the evidence and the checkpoint, and resolves with the checkpoint filled in. |
| `crw.callFacts(result)` | The facts of a call's result that the signal path of `crw.dag` rests on: its `digest` (the canonical digest of its output), its `seq` (the place of its terminal row in the journal) and `live` (false for a result a resume replayed from the journal of an earlier process). It also takes the result of an operation that names the position it ran at (`call_id`, as `crw.planRead` does). A read: it takes no position and writes no row. |
| `crw.recordedSignal(milestone, k)` | The signal `k` of the milestone as an earlier process recorded it and the journal still stands on it (its triggers, the digest of the signal before it and the `attempt` it was bound with, `"live:<n>"` or empty), or `null`. A read, like the next one. |
| `crw.deferredAsks(node)` | The number of the node's standing `crw.release` rows that the relay held back `defer:edit_overlap` and that the lane asked again after (a later `crw.release` row of the node's path, completed or refused), or 0 for a run that does not resume: the held-back answers a resume replays, which `crw.dag` asks again after as the record did (CRW-1467). A read: it takes no position and writes no row. |
| `crw.priorRelease(node)` | The release of the node that an earlier process recorded and the journal still stands on, with the revision it was made under, the number of the signal that made that revision and `refused`, the releases of the lane's path before it that compared another plan (`plan_changed`), or `null`. |

`opts` of a call: `agentType` (`architect`, `executor`, `executor-t1`, `reviewer`, `explorer`, `critic`), `schema`,
`label`, `phase`, `node`, `worktree` or `cwd`, `head`, `resume`, `stepDecision`, `qa` (below), and the Claude-style `model`, `effort` and
`isolation`. The other `crw.*` members are the operations and composites of [the project lane](project-lane.md#the-project-lane); each operation is an
`engine.Op` registered in `Config.Ops`, takes a position of its path like a call, and a resume replays it only while its effect still
holds ([journal.md](../journal.md#ops)). The `crw.*` member a later issue owns (the delivery of the report) is not here.

`head` names the commit a call reads. An `architect` call that continues its thread and names a `head` (the REFLECTION at the close, a re-plan)
runs in a detached read-only worktree of its own at that commit, `<state>/flow/<run>/calls/<call_id>/wt`, and its turn names that directory as
its cwd; the worktree is removed when the call has its terminal row, and a resume that runs the call again makes it again at the head the call's
`prepared` row records. A continuation that names no head keeps the directory its thread started in, as before.

`qa: true` marks the QA capture call (SP-QA-1) of a milestone's final gate: an `executor` call, refused with `policy_refused` on any
other role and with `foreign_handle` on a handle that is not a QA thread. The mark is in the call's prepared row (`qa`), reaches the
backend in `ThreadSpec.QA`, `ForkSpec.QA` and `TurnSpec.QA`, and is inherited by every continuation of the thread (the synthesis turn).
The App Server backend gives such a call the QA overlay of the executor (`roles.Options{QA: true}`), a call-private scratch worktree at
the `head` it names (an executor call with no mark gets a worktree of its node; the scratch worktree is made as it is for the roles that
do not write), no pending tree (what the call edits there is discarded; it is never a commit), and a binding line with
`nativeAllowance` 0, so the spawn hook refuses every native spawn of the thread.

`parallel` and `pipeline` accept `{keys: [...]}` (one explicit key per item, in place of the input index) and
`{label}` (the label the join carries in a concurrency error).

### Determinism

`Date.now()`, `Math.random()`, `new Date()` and `Date()` throw a `DeterminismError`. A `Date` built from arguments
(`new Date(0)`, `Date.UTC`, `Date.parse`) works. Time comes from `args` or `crw.clock()`.
