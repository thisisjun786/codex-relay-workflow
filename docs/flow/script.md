# CRW workflow scripts

Here the parent (relay role `parent`) and the child (relay role `child`) are relay roles, read through [the level table](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels).

A CRW workflow script is a JavaScript file that `crw flow run` executes. It orchestrates agent calls with ordinary
control flow (loops, conditionals, `await`) and the engine records every call it makes. This page is the reference for
the script format, the loader, the globals, call identity, the call result and the limits. It describes what
`internal/flow/script` (loader and runtime) and `internal/flow/engine` (calls, journal, paths) do.

```text
crw flow run --backend fake [--args JSON | --args-file FILE] [--pool N] [--token-budget N] [--journal] [--summary-document D] <script.js>
crw flow run --script JS [--plan-only=false --backend fake] [--args JSON] [--journal]
crw flow roles [--json]
crw flow recover [--run RUN] CALL (--adopt THREAD|TURN | --absent)
crw flow resume [--state DIR] [--socket PATH] [--repo DIR] [--args JSON | --args-file FILE] RUN SCRIPT
crw flow lint [--args JSON | --args-file FILE] [--no-dry-run] [--json] <script.js>...
crw flow audit (--run RUN | --acceptance ID) [--milestone M] [--state DIR] [--socket PATH] [--json] [--no-record]
```

The commands that reach up from a run - `report-up`, `reports`, `report-ack`, `report-delivered` - and the summary document
`--summary-document` names are in [reporting.md](reporting.md).

`--backend fake` runs the script against the in-process fake backend: every turn answers immediately with a value that
satisfies the call's schema, including its numeric bounds, `multipleOf` and `pattern` (or a short sentence for a call without one). It is the only backend the
command selects; the App Server backend is the package `internal/flow/backend/agent` (see [The App Server backend](#the-app-server-backend)),
which the engine drives through the same `Backend` interface and which no command selects yet. The command prints the run's result as JSON and exits 0 when the script
completed, 1 when the script failed (`status: script_error`) or did not load, and 2 for a command line it cannot use.
The journal sequence numbers of a call (`meta.startSeq`, `meta.endSeq`) depend on the order the host finished concurrent
calls, so under `crw flow run` neither the `CallResult` the script holds nor the printed result carries them (a value,
a log line or an error the script builds from a `CallResult` cannot hold them either) and two runs of one script print the
same bytes; `--journal` adds the journal rows, with those numbers. `testdata/flow/hello.js` is a small script that runs this way.

`--script JS` runs an ad-hoc script given as text instead of a file. Its default is `--plan-only`: the script runs on
the fake backend only, with no backend named, and the result says `"planOnly": true`. A script that is to run on a
backend names it and turns plan-only off (`--plan-only=false --backend fake`); `--plan-only` on a script file is a usage
error. An ad-hoc script has no workflows directory: it imports `crw:lib/<name>` only and nests no workflow.

`crw flow run` (like every `crw flow` command that resolves calls) reads its role table once, when it starts, from
the helper role store `CRW_HOME/subagents.json`: the pairs of `architect`, `executor`, `executor-t1`, `reviewer`,
`reviewer-final`, `explorer` and `critic`, with the code's alias table (see
[role-execution-policy.md](../role-execution-policy.md#the-agent-roles-of-a-flow-run)). The execution policy's
`agent-roles` and `agent-aliases` sections are not read; a file that still declares them is a warning, and one whose
sections do not parse still loads, with the sections ignored and the loader's reason as another warning. The run record
keeps the store's path and the canonical digest of the seven pairs, and the warnings of the table (a flow-only key at
its compiled default, an undecided role, a reviewer on an author's model family, a critic on the architect's). A test with no store runs on the
fake backend's placeholder table. `crw flow roles` prints the table, the source, the digest and the warnings.

## File format

A file has a header and a body.

```js
import { RECEIPT } from 'crw:lib/schemas'      // header: zero or more static imports ...
import { greeting } from './hello-lib.js'
export const meta = { name: 'hello', phases: ['greet'] }   // ... then exactly one meta
phase('greet')                                  // body: everything after the meta statement
const hi = await agent(greeting('crw'))         // top-level await
return { hi }                                   // top-level return: the run's result
```

- The header is zero or more static `import` statements followed by exactly one `export const meta = <object literal>`.
  An import has one of the forms `import 'm'`, `import d from 'm'`, `import {a, b as c} from 'm'`,
  `import * as ns from 'm'` and `import d, {a} from 'm'`.
- `meta` must evaluate to a JSON-serialisable object. It is evaluated as an expression in a fresh runtime with no host
  globals, so it cannot call `agent` or read `args`, and under the same determinism stubs as the body (below), so
  `Date.now()`, `Math.random()`, `new Date()` and `Date()` in a `meta` are load errors. It is recorded in the run record.
- The body may use top-level `await` and top-level `return`. These are a CRW extension and the script is not standard
  ESM: the body runs as the body of an async function, and the value it returns is the run's result. A script in the
  shape Claude Workflow accepts (a `meta` and a body with top-level await and return, no imports) is a subset of this
  format.
- An `import` or `export` after the header, a dynamic `import()`, `import.meta`, and any export other than `meta` are
  load errors. The body is checked with a lexer that tracks strings, template literals (including their substitutions),
  comments and regular expression literals, so the words inside them are not taken for statements. A property call
  (`o.import(x)`, also with whitespace or a comment between the dot and the name) and a method named `import` or `export`
  (`{ import(x) { ... } }`) are not imports; a method declaration is told from a call by the `{` that follows its
  parameter list on the same line.
- Script errors keep the body's line numbers: the header is replaced by blank lines in the executed text and the
  transform's source map is given to the runtime.

## Helpers

A helper is a module the header imports. Two kinds are admitted, and nothing else:

- `crw:lib/<name>`: the helper library embedded in the binary (`internal/flow/workflows/lib/<name>.js`).
- A relative path (`./x.js`, `../lib/y.js`, with or without the `.js`) that stays inside the script's workflows
  directory, the directory the script file is in. A path that leaves it, an absolute path, a bare package name, a URL and
  a symbolic link that points out of the directory are load errors.

Helpers are ES modules with static `import` and `export`. A helper with top-level `await` is a load error (the bundle is an
IIFE), and so is a dynamic `import()` in a helper, literal or computed, direct or in a helper it imports: a helper reaches
other modules only through static imports the resolver checks. Every helper source is read by the same lexer as the body
for this check, and a helper the lexer cannot read is a load error, so the check never passes a source it did not see. There is no `require`, `fs`, `net` or `exec` in a script or a helper.

The run record holds the SHA-256 digest of the script's bytes and, for every bundled helper, the digest of the sources it
was built from.

## How a script is loaded

goja, the JavaScript runtime (a pinned commit; direct dependency), has no modules. The loader (`script.Load`) therefore
builds an executable script in four steps:

1. A header lexer finds the end of the imports and of the `meta` object literal.
2. `meta` is evaluated in a fresh runtime with no host globals, under the determinism stubs, and recorded as JSON.
3. Each imported helper is bundled by esbuild's Go API (a pinned release; direct dependency) with `Bundle`, `Format` IIFE,
   `GlobalName` `__crw_lib_<n>` and `Target` ES2017. The resolver plugin admits only the embedded `crw:lib` files and the
   script's workflows directory.
4. The body is wrapped as `(async function __crw_main(){ const {a, b:c} = __crw_lib_<n>; ...body... })`, syntax-checked and
   lowered with esbuild `Transform` (target ES2017), compiled by goja and called.

goja at the pinned commit runs ES2017 async functions, so the target is ES2017 and `async`/`await` are not lowered to
generators (`TestLoaderRunsMetaHelperAwaitReturn` runs `export const meta`, a `crw:lib` import, a relative import,
top-level await and top-level return in one file against that runtime). If a later pin stops passing that test, the
target drops to ES2016 (esbuild then lowers async/await to generators) and this paragraph says so.

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
| `workflow(nameOrRef, args)` | Runs another script inline as one step and resolves with what it returns. The called script shares the run's pool, journal and budget, and its calls live on a child path. Nesting is one level: `workflow()` inside a called script throws. `crw flow run` resolves a name to `<name>.js` and `{scriptPath}` to that relative path, inside the script's directory. |
| `crw.classify(result)` | Maps an executor `CallResult` to one outcome state (below). |
| `crw.clock()` | The time as an RFC 3339 string, journaled. It occupies a position of its path like a call. |
| `crw.tierStep(node, evidence)` | Mints the step decision: it journals the call and resolves with `{id, checkpoint}`; a fresh `executor-t1` call carries `id` as `stepDecision`. It occupies a position of its path. The engine's own call holds the id only (its `checkpoint` is `null` and the evidence is recorded by its digest); the project lane's `crw.tierStep` ([below](#the-project-lane)) wraps it, appends the `flow_step_decisions` row with the evidence and the checkpoint, and resolves with the checkpoint filled in. |
| `crw.callFacts(result)` | The facts of a call's result that the signal path of `crw.dag` rests on: its `digest` (the canonical digest of its output), its `seq` (the place of its terminal row in the journal) and `live` (false for a result a resume replayed from the journal of an earlier process). It also takes the result of an operation that names the position it ran at (`call_id`, as `crw.planRead` does). A read: it takes no position and writes no row. |
| `crw.recordedSignal(milestone, k)` | The signal `k` of the milestone as an earlier process recorded it and the journal still stands on it (its triggers, the digest of the signal before it and the `attempt` it was bound with, `"live:<n>"` or empty), or `null`. A read, like the next one. |
| `crw.priorRelease(node)` | The release of the node that an earlier process recorded and the journal still stands on, with the revision it was made under, the number of the signal that made that revision and `refused`, the releases of the lane's path before it that compared another plan (`plan_changed`), or `null`. |

`opts` of a call: `agentType` (`architect`, `executor`, `executor-t1`, `reviewer`, `reviewer-final`, `explorer`, `critic`), `schema`,
`label`, `phase`, `node`, `worktree` or `cwd`, `head`, `resume`, `stepDecision`, `qa` (below), and the Claude-style `model`, `effort` and
`isolation`. The other `crw.*` members are the operations and composites of [the project lane](#the-project-lane); each operation is an
`engine.Op` registered in `Config.Ops`, takes a position of its path like a call, and a resume replays it only while its effect still
holds ([journal.md](journal.md#ops)). The `crw.*` member a later issue owns (the delivery of the report) is not here.

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

## Roles, pairs and aliases

A fresh call names a role with `agentType`. A role has a fixed model and effort (its pair), a role family (`executor` and
`executor-t1` form one, `reviewer` and `reviewer-final` form one, `architect`, `explorer` and `critic` are alone) and a sandbox:
`workspace-write` for `executor` and `executor-t1`, the only roles that write (to their call's own cwd), `read-only` for
the others. The role table is the helper role store's seven pairs, read once per run (see the policy paragraph of the
run options above); a test with no store runs on the fake backend's placeholder pairs and the same alias table.

- With a role key as `agentType`, a `model` option must be absent, the role's model or an alias of its model family
  (`haiku`: `executor` and `explorer`; `sonnet`: `executor-t1`; `opus`: `architect`; `ocx-gpt-6-1-sol`: `reviewer`,
  `reviewer-final` and `critic`); otherwise the call is `refused` with reason `pair_refused`.
- A call without a role key is looked up in the alias table by (a non-role `agentType` string, else the `model` alias;
  `effort`; `isolation`), the most specific matching row deciding: `opus` is `architect`; `haiku` with
  `isolation: 'worktree'` is `executor`; `sonnet` is `executor-t1`; `ocx-gpt-6-1-sol` (as `model` or as `agentType`)
  with `effort: 'xhigh'` is `reviewer-final`, otherwise `reviewer`. No alias row names `critic`: a critic call names it by
  `agentType`.
- `haiku` without isolation is ambiguous. Claude runs it in a shared checkout; a CRW call has a write cwd of its own that
  no other call shares, so the call is `refused` with reason `policy_refused`, cause `alias_ambiguous` (the intent row
  and the refusal row carry `cause`), and the detail names both fixes: `isolation: 'worktree'` for a build (role
  `executor`) and `agentType: 'explorer'` for an investigation. Anything else outside the table is `policy_refused` too.
- A fresh call's requested `effort` is never applied and never refused; a differing one is kept in `meta.requestedPair`
  while `meta.configuredPair` is the pair the thread reads back (the role's), for an alias call and for an explicit
  `agentType` call alike.
- A fresh `executor-t1` call needs `stepDecision` (the id `crw.tierStep` returned), otherwise it is `refused` with reason
  `missing_step_decision`.
- The thread's configured pair is read back; a mismatch is `refused` with reason `pair_refused`.
- The run record (the `started` row) keeps `policy_source` and `policy_digest`, and every intent row the digest and the
  call's `sandbox`. A run that resumes a journal under a policy of another digest writes `policy_repinned` (`old`, `new`)
  before its first call; a new thread then takes the new pair, a continuation keeps the pair of its handle.

## Call identity: path and position

A call is identified by `(path, position)`. The root path is the script's top-level sequence (`root`). Each item of a
`parallel` or `pipeline` runs on a child path named by the join's position and the item's key, `root/3:0` for item 0 of the
join at position 3 (nested items extend it: `root/3:0/1:5`). The characters `%`, `/` and `:` in a key are percent-encoded
(`%25`, `%2F`, `%3A`) in the path, so two different sequences of positions and keys never name the same path, call id or
handle. A `workflow()` child runs on the path `<path>/<n>:workflow`.
`position` is the ordinal of the `agent()`, `agent.call()` or `crw.*` call on that path (a join counts as one position),
starting at 1. Completion order is never an identity source, and the call id (`c_` and twelve hex digits) is derived from the
run id, path and position.

**A path has at most one outstanding position.** Opening a call or a join on a path while an earlier call or join on that path
has not settled throws `CrwPathConcurrencyError` (it names both labels, as `e.openLabel` and `e.requestedLabel`)
synchronously, before any journal row or host request for the refused call, and the run ends with status `script_error` and
error kind `path_concurrency` even if the script catches the error. So every position is a function of the script and the
settled results. Concurrency comes only from constructs that open child paths:

```js
// stable: each item is its own path
await parallel(xs.map((x) => async () => { const a = await agent(...); return agent(f(a)) }))
// refused: two calls on one path
await Promise.all(xs.map((x) => agent(...)))
const p = agent.call(...); await agent.call(...)   // refused: p has not settled
```

The calls of a run share an agent pool (`--pool`, default 6): at most that many calls hold a thread at once, and the others wait.

## CallResult

`agent.call` resolves with a frozen object with separate fields:

| Field | Meaning |
| --- | --- |
| `callId` | The journal's call id. |
| `handle` | Opaque `crwh1:<run>:<call_id>`; non-null once a thread exists for the call. It resolves through the journal to the call's thread and last turn. |
| `status` | `completed`, `null_model`, `held`, `refused`, `unknown` or `superseded`. |
| `output` | The schema-valid object (or the text of a call without a schema); non-null only when `status` is `completed`. |
| `reason` | Closed set, `null` when completed: `schema_invalid_after_repair`, `context_window_exceeded`, `host_infra_<kind>`, `host_config_<kind>`, `host_policy_<kind>`, `host_interrupted_external`, `engine_paused`, `budget_exhausted`, `pair_refused`, `policy_refused`, `independence_refused`, `missing_step_decision`, `schema_not_strict`, `foreign_handle`, `dispatch_unknown`, `superseded_by_attempt`, `superseded_by_resume`, `cancelled`, `worktree_failed`, `restore_snapshot_mismatch`, `accepted_node_changed`, `recovery_hold`. |
| `meta` | `role`, `requestedPair`, `configuredPair`, `threadId`, `turnIds`, `repairUsed`, `tokens`, `startSeq`, `endSeq` (the last two are absent under `crw flow run`, see above). |

A failed or interrupted turn, and an RPC error to the thread or turn request, is classified by its `codexErrorInfo`: the
quota, rate, overload, connection, stream, rollback, steering and `other` kinds are `held` (`host_infra_<kind>`; a usage or
rate limit also pauses the run), `sessionBudgetExceeded` is `held` with `budget_exhausted`, `badRequest`, `unauthorized` and
`sandboxError` are `refused` (`host_config_<kind>`), `cyberPolicy` and `misalignmentPolicyViolation` are `refused`
(`host_policy_<kind>`), `contextWindowExceeded` is `null_model`, and a failure with no `codexErrorInfo` or one the table does
not list is `held` with `host_infra_unknown`. An interrupt the engine did not send is `held` with `host_interrupted_external`. A turn the App Server backend did not send because the
flow binding of its thread could not be written (the spawn hook's file, `~/.local/state/crw/flow-bound/<thread_id>.jsonl`; F14) is `held`
with `host_config_flow_binding`: the flow-bound directory is the current user's, mode 0700 and no symlink, and the thread id has the form
of a thread id (lower-case hexadecimal and `-`).
A request that failed without an answer (a lost connection, a timeout) ends `unknown` with `dispatch_unknown`; on the fake
backend the call writes `outcome_unknown`, and on the App Server backend it writes no terminal row at all, so its last row
stays the intent that was sent and the reconcile pass decides it ([Reconcile and recovery](#reconcile-and-recovery)). The node
of such a call is in `recovery_hold`: every later call of the node (a fresh call, a continuation of the unknown call's handle)
ends `held` with `recovery_hold` and sends nothing, and `crw.tierStep` for the node throws. Only `null_model` can be
evidence for a tier step.

A turn the engine interrupts - the turns in flight when a usage or rate limit pauses the run - is journaled first
(`interrupt_requested`, reason `engine_paused`) and ends `held` with reason `engine_paused`, whatever the host reports for
the interrupted turn (failed, interrupted or completed with a valid answer); a turn the host reports interrupted with no such row is `held` with `host_interrupted_external`. A
usage or rate limit also writes one flow row `escalation` with reason `quota_or_rate`.

### Schemas

An `outputSchema` is sent as a strict `json_schema`, so a schema is checked against the strict structured-output rules
before any thread starts: the root is an object; every object has `additionalProperties: false` and lists every one of its
properties in `required` (an optional field is a required nullable union such as `type: ['string', 'null']`); only `type`,
`properties`, `required`, `additionalProperties`, `items`, `enum`, `const`, `anyOf`, `$defs`, `$ref`, `description` and the
`pattern`, `format`, `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum`, `multipleOf`, `minItems` and `maxItems`
constraints appear (no `oneOf`, `allOf`, `not`, `if`/`then`, `patternProperties` or `unevaluatedProperties`); and the schema
stays within 10 levels of nesting, 5000 properties and 1000 enum values.

- Every export of `crw:lib/schemas` is checked when a script imports the module (directly or through another helper); a
  violation is a load error.
- A schema passed to `agent.call` that fails the check ends the call `refused` with reason `schema_not_strict`, before any
  thread starts.
- The result of every call with a schema is validated in Go (`google/jsonschema-go`). A message is valid only if it is one JSON
  document with nothing after it (a closing bracket, a second value or text after the value is invalid). An invalid result
  gets one repair turn on the same thread with the same pair; a result that is still invalid is `null_model` with reason
  `schema_invalid_after_repair`. A third turn is never sent.

## Continuing a call

`agent.call(prompt, {resume: handle})` continues the thread of a call. `resume` takes a handle string or a `CallResult`; a
bare object, a copy of a `CallResult`, `null` and anything else is a `TypeError` (`crw.classify` is the same: there is no
implicit "last call"). The continuation inherits role, pair, node and `stepDecision` from the handle's journal record, adds one
turn to the same thread and gets a handle of its own. An explicit value that differs from the inherited one is refused:

- A handle from another run, another role family or another node, and a handle that names no call with a thread in this run,
  is `refused` with reason `foreign_handle`. The run of a handle is judged by the journal row of the call it names, not by the
  run id written in the handle's text.
- The same role family on another role, a `model` that is not the thread's model or an alias of it, an `effort` that is not
  the thread's effort, or another `stepDecision` is `refused` with reason `pair_refused`.

- A thread whose pair read back wrong when it started (its call was `refused` with `pair_refused`) stays refused: continuing
  it is `refused` with `pair_refused` too. A continuation carries the thread's read-back pair, not the role's.

A refused continuation sends nothing to the host.

## crw.classify

`crw.classify(result)` takes an executor `CallResult` or the handle of one of this run's results and maps it to one state: `ready` (completed, `ready_for_review`), `research`
(`needs_research`), `scope` (`blocked_scope`), `self_failed` (`failed` or `budget_exhausted`), `null_model`, `host_held`,
`null_unknown`, `refused` or `superseded`. Each classification reads only its own result, so failures mixed on one path or
across parallel paths are attributed to the exact call. A result of another role family than the executor, and a completed
result whose output is not a complete `RECEIPT` (every field of `RECEIPT` in `crw:lib/schemas`, with the types it lists),
is a `TypeError`; so are a bare object, a copy of a `CallResult`, `null`
and a string that is no handle of a result of this run. Like every `crw.*` call, `crw.classify` takes a position of its path
(journaled as an `Op: crw.classify` row), so calling it while another call or join of the path has not settled throws
`CrwPathConcurrencyError` and ends the run `script_error`.

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
`background`, a background explorer that did not complete halts the milestone (`crw.milestoneHalt`, source `explorer`), and a re-plan has
no such phase; then, in the phase `design`, the architect answers `DESIGN` once for the milestone (SP-ARCH-1) over a packet that carries the
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
pinned plan on a resume), which the answer carries into the design audit, and a command that failed, ran no test or was not reported is a
warning of the `crw.exploreRecord` row (CRW-1382; never a stop), and a first round that spawns no explorer (a DESIGN with no open
question to explore) runs none of them, which its `crw.exploreRecord` row records once as the warning `end_condition_not_run` naming each
command and its issue (CRW-1397; warn only); when a question of the round got an answer (an explorer's findings, or an answer the run has
reused), the answers go back to the architect on its own thread before the critic reads the DESIGN (`design-answers:<tag>r<round>`, CRW-1466): the
packet renders each answered question by its id, question, answer and findings, never by its route, and the architect answers with the DESIGN again,
dispositions null; that DESIGN is the one the critic audits and the one a design-fix and the ledger resume, and a question the architect raises in it
reaches the critic with no answer, which is a P1 finding, and is routed in the next round (a round whose questions got no answer has no such turn);
the critic (`agentType: 'critic'`, a role of its own, of another model family than the architect;
the `AUDIT` spawn decision names it) audits the DESIGN (`DESIGN_AUDIT`, recorded by `crw.designAudit`, which judges the independence by the model
families of the two calls' pairs, never by the role), and `needs_changes` returns the DESIGN to the architect on its own thread: the design-fix packet
names each finding by its id `r<round>.<index>` (the index 1-based in the audit's findings) and the architect answers the DESIGN with `dispositions`,
one for each id, `folded` or `rebutted` with its reason (null in the first DESIGN, in a design-answers answer and in a re-plan answer; the schema
requires the member of every answer, and a DESIGN a run recorded before the member existed is read with it null wherever the run reads a recorded
DESIGN: the audit's candidate, the ledger, the release of a node, `crw.issuePlan` and the final gate's decisions); from the second round on the critic's packet
carries `PRIOR FINDINGS`, every finding of the earlier rounds with what the fix answer reported of it (`folded` or `rebutted` and the reason, `not reported`,
or `null (no dispositions reported)`), and the critic raises a rebuttal it does not accept again under the same ref (CRW-1452); only an approved DESIGN becomes revision
`r0` of the design ledger (`crw.designLedger`); `crw.dag` runs one lane for each node and the signal path of the milestone
([The architect's continuity and the re-plan](#the-architects-continuity-and-the-re-plan)); when every lane landed, `crw.closeMilestone` closes the milestone over
evidence the run makes itself ([The milestone close](#the-milestone-close)); the architect reflects on the integrated range, on its own thread, and a
reason to re-plan asks it for the whole DESIGN of the milestone again; `crw.settleReviews` audits the reviews behind the
acceptances against the reviewer threads on the host and archives the threads whose audit passed, and `crw.reportUp` records the report
(its delivery is the seam of the issue that owns it). A milestone that cannot go on says why (`crw.milestoneHalt`): a call of the design phase, of the close or of
a re-plan that did not complete is never read as an approval, a design, a gate, a review or an alignment (`held` and `unknown` hold the milestone, `refused` and
`null_model` escalate, a gate that failed or cannot be settled and a QA that is INCONCLUSIVE escalate, `superseded` stops). A crosscheck
that does not verify the range escalates with its cause: `crosscheck_pending` (a `needs_changes` verdict), `crosscheck_refused` (ask
`configuration`), `crosscheck_failed` and `crosscheck_no_verdict`; one that held holds the milestone ([The milestone close](#the-milestone-close)).

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
edit regions overlap (an undeclared region overlaps every other); the regions of a node are those of the plan together with the
`edit_regions` of the approved DESIGN (`crw.admissionRegions`), because the scheduler's own gate reads the plan's and the executor
is scoped to the DESIGN's; there is no barrier. The relay is told the same regions before the release, each once by its cleaned
path, and a DESIGN region that repeats a planned one in another spelling (blanks around it, `./path`) is that planned region, with
the plan's change and exclusive flag. A node whose predecessor did not land is
`blocked`. Each node runs on a path of its own (its id), and its first position is `crw.release`, which asks the relay for the
slot and records the design revision and digest the node is released under. The lane builds its packet from that revision
(`crw.issuePlan` reads the journal and nothing the script holds), and every call of the node carries the revision, the node's slice
digest and its criteria digest in its input snapshot, so a ledger that later grows a revision changes nothing for a node that
started under the revision it was released under. The lane function is called with a third argument, `sig`, whose `receipt(result)` reports the RECEIPT of each
executor call to the signal path.

Before the relay is asked for the slot, `crw.release` declares the node's admission regions to the relay (`dag_node_regions`, as
`dag-region-declare` does; CRW-1391), because the relay's release judgment reads only declarations and an undeclared node overlaps
every other: without it the second of two nodes `crw.dag` admitted together was held back `defer:edit_overlap`. It declares the
admission regions of the milestone's other designed nodes that have none as well, since the judgment counts a ready node ranked before
the released one as a holder. A plan region is declared as the plan states it, its path, kind and symbol taken from the plan's fields
and never read from its text again (a planned file or tree whose path holds `#` stays one, CRW-1408); an `edit_regions` entry is a file, a tree when it ends in
`/` and a symbol after `#`. A node with a declaration in force keeps it (the parent's graded declaration made before the run, or one an
earlier release made), and a node with no regions on either side is left undeclared. A declaration the relay refuses is a warning, never
a refusal: the release row records `regions`, `regions_declared` (`declared`, `kept` or `failed`) and the warning `regions_not_declared`
with the relay's reason, and the release goes on as for an undeclared node.

| `crw.classify` state | The one transition |
| --- | --- |
| `ready` | unit check, commit, verify, land (the unit check below judges the call's uncommitted work before the commit) |
| `self_failed` (the executor reports `failed` or `budget_exhausted`) | as `ready`: the run runs the unit check of the worktree anyway, then commits and verifies it; the report is no evidence |
| `research` | `crw.explore` answers the question (SP-EXPL-2: an answer of the run is reused, at most two research continuations on a tier), and the same executor thread goes on with the answer; never a tier step |
| `scope` | `crw.escalate` with `blocked_scope`: the relay closes the node's execution and takes its slot back (R07) |
| `null_model` | the first one: a fresh thread of the same tier in a new attempt worktree at the node's last commit; the second in a row is tier evidence |
| `host_held` | `crw.hold`: the node is held and the call goes on in place when `crw flow resume` runs after the cause is gone; never a new thread, never tier evidence |
| `null_unknown` | `crw.hold` with `dispatch_unknown` (a recovery hold): nothing is sent again |
| `refused` | `crw.escalate` as a configuration fault, then `crw.decide`: the lane goes on, with a fresh thread of the same tier, only when the run journal has the `decision_applied` row of that escalation and node |
| `superseded` | the lane stops |

**Unit check.** `crw.unitCheck(node, call, opts)` judges a node's work before `crw.commit` commits it (CRW-1465): the run runs the
review tool itself, in Go through the review adapter, over the uncommitted changes the executor call `call` left in the node's
worktree, so no reviewer agent is called and no reviewer thread is made for a node (row `crw.unitCheck`,
[journal.md](journal.md#the-proofs)). The tree it judges is the call's pending tree, the one `crw.commit` then commits, so the
acceptance takes the check as the judgment of the head that commit makes; a worktree that is no longer that tree is not checked
(`tree_moved`), nor one whose `HEAD` is not the commit the node's attempt is at, the start of its worktree or the node's last commit
since (`head_moved`: the executor committed its work itself, whatever the backend). The prompt carries the node's criteria and, when the backend
assigned the executor an evidence receipt, the receipt's accepted copy; a receipt that cannot be handed over holds the node (below). A
check with any finding, blocking or not, goes back to the same executor thread (`fix:<node>:<tier>:r<round>`, the findings in the fix
packet), and the fixed tree is checked again, at most two round trips on a tier (`UNIT_CHECK_ROUNDS`); the check after them is
`final` and its findings are carried, before any tier step: when none blocks (verdict `verified`) the node is committed, verified and
landed with them as pre-merge defects that need no disposition (`crw.land` hands their ids and the checks that carried them to
`crw.accept`, which reads each from that check's record into the premerge record, also when the check the acceptance rests on judged
only a later fix), and the lane result names them (`carried`); a blocking one escalates
`unit_check_carried` (ask `land`, the carried ids and each finding's file and text) and nothing lands. A finding is named by its file
and its text, never its line (`reviewadapter.FindingID`). A check the tool gave no judgment in (`quota`, `no_answer`) or that had no
executor receipt to hand over (`receipt_missing`, `receipt_unverified`, `receipt_not_copied`) holds the node through `crw.hold` with
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

**Review.** `crw.review(node, plan, head, ci, opts)` is the old reviewer path, kept for the journals of old runs: neither the project
lane nor `land-head` calls it, nor `crw.reviewHalt` or `crw.exploreEvidence`. It is a fresh reviewer thread over the committed head (SP-REV-1), with the record
of its answer (`crw.review` row: node, head, criteria digest, state). The state is `verdict` only for a completed, valid `VERDICT` of a
reviewer whose model family differs from every author of the node (the node's executors and the architect); a `verified` verdict
names no unmet criterion and no defect that blocks (`premerge.Blocks`: a defect of P0 to P2 whatever its impact, or one whose impact
is other than `minor_separable`), and a defect of kind `evidence_missing` carries its `evidence_question`. The
criteria of a plan are `c1` to `cn` in the plan's order (the packets label them so); a `verified` verdict has one entry for each, once
(`ops.CheckCoverage`), because the premerge record is made of the entries it names and the digest of the whole set is attached to it.
An answer that breaks those rules is `null_model` (`verdict_invalid`). The other states - `held`, `unknown`, `refused`
(`independence_refused` included), `null_model`, `superseded` - are the call's own status; a `null_model` reviewer is asked once more
on a fresh thread, and only the second result is the result. `crw.reviewHalt` takes every state that is not a verdict: `held` and
`unknown` hold the node, `refused` and a second `null_model` escalate, `superseded` stops; none of them is an executor fix or tier
evidence. A `needs_changes` verdict with a defect of kind `evidence_missing` sends the question to an explorer (SP-EXPL-3, at most
two a node) before the next round, read at the committed head the reviewer judged (the call carries `head`), and its `FINDINGS` go into the next fix packet and the next reviewer packet; a verdict without such
a defect records the skip.

**Tier evidence (SP-TIER-1).** `crw:lib/evidence` keeps the evidence of one node and tier from the results the lane feeds it: (a)
`needs_changes` in two consecutive reviewed rounds with a finding digest in both (`FindingDigest`: kind, file, line and the words of
the finding), and in the node lane two consecutive unit checks that rule `needs_changes` and share a finding id (file and text, no
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

**Decisions.** `crw.spawnDecision(rule, decision, info)` appends a `flow_spawn_decisions` row for every decision to spawn, do the work
inline (SP-INL-1), reuse an answer (SP-INL-2), skip, refuse or hand a question to the owner thread; the explorer routing
(`crw.exploreRoute`) writes its own rows. A script names a rule by its key (`EXEC_1`, `TIER_1`, ...) and the id comes from
`internal/flow/ops/rules.go`, the one place the `SP-*` strings are written.

**Operations and composites.** The operations (`internal/flow/ops`, one `engine.Op` each, rows `crw.<name>`):

| Operation | Records and returns | Replayed while |
| --- | --- | --- |
| `milestone(plan, milestone)` | the nodes of the milestone with edges, regions and slice digests | always (a read) |
| `designAudit(milestone, round, candidate, architect, call)` | the audit row: state, candidate digest, auditor and architect models | always |
| `designLedger(milestone, entry)` | the next revision (`r0`, `r1`, ...): the DESIGN, the answers, the digest, the architect handle, the signal that made it and the slice digest of every node of the plan it was made against; refuses a DESIGN with no approved audit - for a revision after `r0` an audit made after the revision before it | always |
| `release(plan, node, milestone)` | the release under the revision of the ledger that stands (`revision`, `design_digest`, `slice_digest`, `execution_id`); a node sized for a child or a split is not released to an executor; a plan that cannot be read, or that has dropped the node (`node_not_in_plan`), releases nothing | the relay says the execution stands, and the revision it was made under stands or the node's slice is the one the revision that stands was made against (`release_rebind`) |
| `issuePlan(node)` | the node's entry of the DESIGN of its release revision | always |
| `commit(node, call, regions)` | the commit of the call's recorded `pending_tree` (`commit`, `parent`, `tree`, `out_of_scope`, the receipt's outcome and `plan_mismatch`) | the commit is on the history of the node's worktree |
| `verify(node, head, opts)` | the verification (`ok`, `command`, `digest`, the record); a verification that could not run is `ran: false`, not a failure. With no node, `opts` names the milestone, the command and the base: the verification of the integrated head for the final gate, in a worktree of the repository | always while it ran |
| `unitCheck(node, call, opts)` | the unit check of the node's work by the review tool, run in Go (no agent call): uncommitted mode over the changes executor call `call` left, or, with `opts.commit` and `opts.base`, committed mode over `base..commit`; the `reviewadapter.Result`, the `hold` the lane is to write (`unit_check_quota`, `unit_check_no_answer`, `receipt_*`), `retried`, the judged call and the check's id | always, but a check that names a `hold` (asked again) |
| `review(node, head, criteriaDigest, call, opts)` | the `ReviewResult` (the old reviewer path) | always |
| `accept(node, head, opts)` / `integrate(node, head, acceptanceId)` | the acceptance id (`opts.carried`: the findings the node lane carried, each `{id, check}`, read from the record of the check that carried it into the premerge record's defects) / the batch commit and the target tip | the acceptance is active and not stale / the batch commit is on the target |
| `escalate(node, info)` / `hold(node, info)` | the escalation flow row and, for `tiers_exhausted`, `blocked_scope` and `size_split`, the relay's close of the execution; the hold (the relay is told, nothing is closed) | always |
| `decide(node, ask)` | whether a `decision_applied` row answers the ask | no applied row has appeared since |
| `spawnDecision`, `stepRecord`, `exploreRoute`, `exploreRecord` | the decision rows and the answers the reuse reads | always |
| `finalGate(milestone)` | the gate over the pinned head: the state of the gate and of each piece of evidence (row `crw.finalGate`: `head`, `command`, `range`, `ok`) | the integration target is still the head the gate judged |
| `qaReceipt(milestone, gate, capture, passes, receipt)` | the QA receipt: its state (`PASS`, `REVISE`, `FAIL`, `INCONCLUSIVE`), the head, the calls with their roles and read-back pairs | always |
| `qaCapture(milestone, gate, capture)` | the capture of the QA call as the later calls read it: each capture with the path it can be opened at (the relative path resolved against the worktree the capture ran in), the identity of the tree the call ended in, and `ok` or the reason the capture cannot be handed on | always |
| `crosscheck(milestone, gate, opts)` | one round of the crosscheck over the gate's range, run in Go with no agent call (row `crw.crosscheck`): X = `{state, reason, hold, detail, verdict, questions}`, the state `verdict` (V: `verified` or `needs_changes` by `ops.CrosscheckRule`), `held` (`hold`), `refused`, `failed` or `no_verdict` (`reason`); round 1 only | the row is of the same milestone, head, round and evidence round and did not hold |
| `reviewerFinal(milestone, gate, call, opts)` | the review of reviewer-final over the range (`range`, `criteria_digest`, the alignment); a `verified` answer is a verdict only when it evaluated every criterion of the milestone once, all met, with no defect that blocks. The shipped close does not call it | always |
| `milestoneClose(milestone, evidence)` / `reportUp(milestone, close)` | the range and the tip the close judged over `{gate, crosscheck}`, and whether the target moved on after the gate; evidence with no verified crosscheck V of the gate's milestone and head is refused `close_unverified` / the report row | the integration target is still the tip the close judged / always |
| `replanSignals(milestone, opts)` | the two numbers of SP-ARCH-2 and the reasons that stand (`plan_mismatch`, `slice_change`, and at the close `reflection_misaligned`, and `reviewer_final_misaligned` for a reviewer-final alignment, which the shipped close does not hand it) | always |
| `currentDesign(milestone)` | the revision that stands, its DESIGN, the handle of the architect thread and the heads the nodes landed with | always |
| `reflection(milestone, gate, call)` | the architect's REFLECTION over the integrated range: a verdict only for a completed, valid answer; the row moves the architect's handle on | always |
| `planRead(milestone, node)` | the plan revision a lane reads before its release: the digest of the milestone as the relay has it, its replay-stable observed occurrence, the plan the audited DESIGN stands on (`base`: the plan the current revision was made for, or the one a later signal judged and kept it for), whether it changed, the signal in flight that read this very plan (`judging`), and the position it ran at | stands once the node was released after it, or once a signal row that stands took that occurrence of the changed revision as a trigger; else it reads again |
| `signalBind(milestone, k, prev, members, attempt)` / `signalDone(milestone, k, outcome)` / `signalClose(milestone, summary)` | the signal row (its triggers, what had started and what had not, the tip of the target, the plan revision read), its result (continue, refresh, halt; the revision it made) and the end of the signal path | always; a resume replays a bind only with the same arguments (`attempt` is `live:<n>` for a recorded signal one of whose triggers ran again, `n` one more than the attempt of the recorded row, so that its arguments are never the recorded ones, also when that row is itself the live attempt of an earlier resume) |
| `audit(milestone)` / `archive(milestone)` | the audit results of the milestone's acceptances (row `crw.audit`) / the reviewer threads archived and kept | always |

The composites (`crw:lib/lane` and `crw:lib/close`, made members of `crw` by `install(crw)`; they take no position of their own): `crw.dag`,
`crw.review`, `crw.reviewHalt`, `crw.explore`, `crw.exploreEvidence`, `crw.land`, `crw.tierStep`, `crw.evidence`, `crw.skip`,
`crw.closeMilestone`, `crw.settleReviews` and `crw.milestoneHalt`; `crw.review`, `crw.reviewHalt` and `crw.exploreEvidence` are the old
reviewer path, kept for the journals of old runs, and no shipped script calls them. `crw.dag` takes an `onSignal` function (below). The seams the operations are built on - `Relay` (the scheduler: milestone, release, agent
acceptance and integration, the close of an execution), `Verifier` (`CILocalVerifier` runs `crw-dev ci local` through the verifier's tools of the configuration, [the verifier's tools](../runtime-install.md#the-verifiers-tools)), `Decisions`
(`StoreDecisions` writes the relay store), `Threads` (the host the close reads the reviewer threads from and archives them on) and
`Reporter` (F11) - are interfaces in `internal/flow/ops/seams.go`; the dagsched-backed `Relay` belongs to the run process.

## Landing a head made outside a run

The child decides how the work of an issue is made: a project-lane run, or its own subagents outside any run (CRW-1316 §18.3). A head made
outside a run lands through the gates of a run, never past them: `internal/flow/workflows/land-head.js` (CRW-1354) is the gate-only
workflow, `crw flow start land-head --handoff FILE --args '{"plan": P, "milestone": M, "issue": KEY, "head": COMMIT-OR-BRANCH, "authors":
[THREAD, ...]}'`, where `authors` are the subagent threads that wrote the head. It makes no executor, architect, design-audit or reviewer
call and fixes nothing. Its steps:

- `crw.release(plan, node, milestone, {pinned: true, land: {head, authors}})` releases the node to the run with no DESIGN: the head is
  resolved in the run's repository, each author thread is read back from the host (`thread/read`, the configured pair; `Threads` provides
  `ThreadConfig`), the plan the relay holds is compared with the plan the run's `crw.milestone` row read (`plan_changed` otherwise), and
  `Relay.Release` is the agent release with the node's slice and the pair the authors ran on, which the execution row records
  (`author_model`, `author_effort`; `AgentReleaseOptions` of `ReleaseAgent`, the stored form unchanged). The head is then checked out,
  detached, in `<state>/flow/<run>/land-wt/<node>-<commit>`, the worktree the row names and the execution's handle points at, and the row
  records the commit, its tree, its base (the merge-base with the integration tip), the paths it changes and the authors with their pairs.
- The gates are the lane's: `crw.verify` of that head (verification-record/1 of the exact tree), one unit check in the committed mode,
  `crw.unitCheck(node, null, {commit: head, base: head.base})` (CRW-1468): the review tool run in Go over `base..head` of the land
  checkout, with no reviewer call; a head made outside a run has no executor, so the check's row records the receipt `no_executor`. The
  committed profile's reader takes an answer only when it names the requested range `base..head` by its commit ids and no other range,
  so until that profile is replaced a clean answer that names no range or another one fails closed as `unit_check_invalid`. Then
  `crw.land` (`crw.accept` on that check, the relay's agent acceptance, which judges the check's model family against the pair the
  execution records: `independence_refused` for a check of the authors' family, `independence_unproven` when the pair of any author it
  names was not read back; then `crw.integrate`). They write the rows they write for a node of a project lane.
- The only refusals are input errors (no head, a head git does not resolve, authors of more than one model family - one execution row
  records one author pair - and an issue that is not a node of the milestone) and the gates. Named limit: authors of more than one model
  family are refused `land_author_families_mixed` before any gate, even where a check of a third family would be independent of all of
  them, because the execution row holds one author pair; lifting it changes the relay's stored form, so such a head is landed one family's
  work at a time, or made in a project-lane run (CRW-1397). A land one of whose named authors is not read back records no author pair on
  the execution (`land_author_pair_unrecorded`, a warning of the row: the observed pair would stand for authors the run does not know),
  and the acceptance is `independence_unproven`; the pairs the host reads back are trimmed. A verification that fails, a check that
  finds a blocking defect and a land the relay refuses end the run escalated for the child (`land_verification_failed`,
  `land_review_needs_changes` with the ids of the check's findings carried on the escalation, the relay's reason); a check that gives no
  judgment (`unit_check_<state>`) and a check that is not independent escalate as a configuration fault. The findings of a verified check
  that block nothing land as premerge defects. An author whose pair is not read back, no
  author named and a path outside the node's regions are warnings of the release row (`land_author_unobserved`, `land_authors_unnamed`,
  `land_out_of_scope`). A lost epoch ends the run `stopped` with `epoch_lost`.
- The gate-only lane fixes nothing: the child changes the head and lands it again, in a new run. The fix rounds (CRW-1490):
  `args.fix_round` (0 when not given) is the round of the child's fix this land is, a non-negative integer; any other value (a string,
  a negative or fractional number, null) ends the run `refused` with `fix_round_invalid` at its start, before the milestone is read or
  the node released. A check that needs changes at a round under
  `LAND_FIX_ROUNDS` (2) ends `land_review_needs_changes` with no ask (the child acts), the round and the carried ids on the escalation
  and in the run's value, and the value hands the child a fix packet (`fix`, `landFixPacket`): the findings by id with file, line and
  text, the range as a `git diff` command, the node's criteria, and the land of the fix, a new `crw flow start land-head` run of the
  same node with `fix_round` one more and the authors of this head together with the fixer's thread, since the fix branches from this
  head and its range holds this head's work too. A check that needs changes at `LAND_FIX_ROUNDS` ends `land_fix_rounds` with ask
  `land_fix_rounds`, a question for the parent, and no fix packet. A land that ends escalated gives its
  release back - the escalation carries `end_land`, and `crw.escalate` asks the relay to close the node's execution with `land_ended`
  (`cancelled`, the slot returned in the same transaction) - so the next land is a new release: a new execution of its run, with its
  worktree and the pair of the authors it names. Only the relay's refusal to close an execution that has to stay (its accepted head
  landed or may be in the target, a `disposition_conflict`) is the row's `close_refused`; any other relay error is asked again in place,
  and one that keeps failing is the op's error, so the run ends on it rather than leaving its execution current under a run that has
  ended (CRW-1397). A hold - a verifier the host cannot run now (`verify_unavailable`), or a check the tool gave no judgment for its
  quota or no answer (`crw.hold` with source `unit_check`) - keeps the release, and the run goes on with `crw flow resume`, which asks
  the tool again, or gives the release up with `crw flow cancel`. A node whose current execution another run holds is not released
  to the land: `crw.release` answers `land_node_held` with that run and its execution, before the run claims the plan's coordinator
  epoch, and no gate runs. The answer names what settles it: `crw flow resume` of a run that has not ended, and `crw flow cancel` of
  one that has (the row records `held_run_state` and the warning `land_node_held_by_ended_run`), whose cancel gives up the executions
  the run still holds - the ones its rows show, and the ones the relay records under its run id that its rows do not name.
- A land-head run is a run of the plan: its release claims the plan's coordinator epoch like any run's, so a run of the same plan that
  is still going stops `epoch_lost` at its next fenced write (one run orchestrates one plan). A head made outside a run is landed when
  no other run of its plan is going: `crw flow start land-head` warns of one with the preflight fact `plan_live_run`, which names the
  run, is printed and is recorded in the run's spec, and the start goes on (CRW-1397).

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
decisions its `decision_refs` name; a node whose issue was sized for a split or a child started too (its lane recorded the design row and escalated it), is
listed with that plan and is no unstarted node of the re-plan - and a started node whose issue differs from it in any field, or a decision such a plan names that the DESIGN drops or changes
in any field, is a P1 finding), and the nodes that have not started are
released under a revision only after that, because the signal that re-plans keeps every new release back until it is decided. A lane that started keeps the
revision it was released under; it ends under it and lands or is held as usual. Each revision records the slice digest of every node of the plan it was made
against - for a revision after the first, the plan the architect and the auditor evaluated (`plan` of `crw.replanSignals`, handed to `crw.designLedger`), so that a plan revision made while
the re-plan's DESIGN was audited is a change the next signal counts against the revision - and a release records the slice of the node as the plan is when the node is released (read again at the release: a plan that
cannot be read, or that has dropped the node, releases nothing).

**The numbers (SP-ARCH-2).** `crw.replanSignals` writes two numbers to the journal: the RECEIPTs that report a `plan_mismatch`, of lanes released under the revision that stands (each call
once; a RECEIPT of a lane released under an older revision does not count in the new revision's number), and the share of the milestone's nodes whose slice
digest of the plan now is not the one the revision that stands was made against. The reasons are two plan mismatches, more than a quarter of the slices changed
(exactly a quarter is not a reason), and, once the milestone closed, a MISALIGNED REFLECTION. The op also counts a MISALIGNED reviewer-final alignment
(`reviewer_final_misaligned`) when a close hands it one; the shipped close runs the crosscheck and hands it none. One reason is enough, whatever the
alignment says. Every decision - to re-plan, and to skip it with its reason - is a row of `flow_spawn_decisions` (`ARCH_2`).

**The signal path.** `crw.dag(plan, lane, {nodes, milestone, cap, onSignal})` runs the signal path next to the lanes when it is given `onSignal`. The
RECEIPT of an executor call of a lane is a trigger (`sig.receipt`), and so is a plan revision a lane reads before its release (`crw.planRead` answers `changed` when the plan is another than the one the audited DESIGN stands on, and `judging` when a signal in flight read this very plan; `sig.plan`; the trigger is `plan:<occurrence>:<digest>`, once per observed occurrence). The occurrence names the newest standing observation of another digest, or the first ledger row (journal.md); concurrent reads of the same occurrence coalesce, but P0 → P1 → P2 → P1 takes a new signal for the return to P1. Its lane waits for that signal's decision and any required DESIGN audit, not the earlier signal for P1. Replayed reads and recorded signal members preserve the same identity, including the legacy key of a read recorded without an occurrence. The path `signals:<milestone>` takes the triggers that have arrived, in the order of the
journal and not the order they finished in, into one signal at a time, and the triggers that arrive while a signal runs are taken together by the next one.
A signal is `crw.signalBind` (its input: the number `k`, the triggers as (call, digest) and the result digest of the signal before it; the row records what had started and
what had not - a node has started when a release of it to an executor stands, or the design row that stands sized its issue for a split or a child
(its lane escalated it), the predicate `crw.releasedPlans` lists by too, so a milestone whose other nodes are all sized so skips the re-plan
(`no_unstarted_node`) -, the tip of the integration target and the plan revision read), a join of one item, the child path `signal:<milestone>:<k>`, which runs
`onSignal`, and `crw.signalDone` (continue, refresh or halt, and the revision it made). While a signal is decided no lane is released and the scheduler admits no node: a node that read a changed plan revision waits until the signal that took that trigger is decided, live or replayed, and is released under the revision that stands then. Every release of `crw.dag` compares the plan (`crw.release(plan, node, milestone, {pinned: true})`): the plan the relay has when `crw.release` reads it, in the flow and before `Relay.Release`, is compared with the plan the audited DESIGN stands on, and a node whose plan is another is not released (`plan_changed`). The release is a compare-and-swap on that plan (CRW-1288): `Relay.Release` carries the node's slice of the compared plan, and the relay compares it with the node's slice of the plan's head when it judges the release and again in the transaction that reserves the slot and records the release, so a plan change between the flow's comparison and that commit releases nothing either (`plan_changed`; the relay's refusal is `plan_revision_conflict`). Either way the lane reads the plan again - a trigger - and asks again once its signal re-planned and audited a DESIGN for that plan or judged it and kept the revision. A dag without `onSignal` leaves such a node `unreleased`. A relay refusal while the plan the flow reads again is still the pinned one (`relay_plan_changed`) is a plan revision the run does not follow: the run reads its plan at the revision it pinned (CRW-1277 c) and the relay releases from the plan's head, so no signal comes of that change. The release raises the run-level hold `plan_superseded` (CRW-1291): an escalation of the node that asks the parent and names the revision the run pinned, the relay's head revision and the node, which the run tells the child's thread like any escalation ([reporting.md](reporting.md)). While the hold stands no node of the run is released (`plan_superseded`, the relay is not asked) and nothing restarts on its own, a resume included. The execution the relay still holds for the node (a release the run never recorded, whose replay the relay refused) is closed with the hold when it is the run's, which returns its slot, and recorded with its run otherwise. The actions are the parent's: end or cancel the run (`crw flow cancel`) and start a new run on the new revision, or answer the hold's question (`crw flow answer`, then `crw flow resume`), which closes that node by escalation (`plan_superseded`) and lifts that hold: the other held nodes ask the relay again, and each one the new revision changed raises a hold of its own, so answering takes one round per changed node (cancelling the run and starting a new one is the shorter way). Lanes released before the hold go on to their integration. The project lane ends `held` (`plan_superseded`) while the hold stands. The operating rule follows: a plan is not changed while a trial run runs on it. The relay answers the replay of a release only to a request that expects the slice the release was committed under, so a release the relay committed before the run recorded it is not answered as a release under the slice of a plan that changed since. `onSignal` of the project lane
counts the numbers and, when a reason stands and a node has not started, runs the re-plan: the whole DESIGN, its audit rounds, the revision; a re-plan that cannot be done halts the milestone,
and no node that has not started is released. A refresh gives the signal's outcome the admission regions of the nodes under the revision it made (`regions`: the plan's regions with the `edit_regions` of that DESIGN), which the scheduler reads for the nodes that have not been admitted and for the nodes it admitted that have not yet been released: before its release a node is checked once more against the nodes that hold their regions (released, or about to be), and waits for the end of the one it overlaps. When the reason stands after the last node started the close judges the same numbers.

**A resume.** A signal is replayed with the membership its row recorded (`crw.recordedSignal`), whatever order the lanes replay in now, and only when each of its triggers was
replayed from the journal (`crw.callFacts`: not live) with the digest it answered then and the signal before it was replayed. The first signal that does not replay (also one whose triggers all answer the digests they answered, when one of them ran again: the script binds it as a live attempt numbered one more than the row it does not replay, so a second resume that runs a trigger again supersedes what the first one made, and a later resume replays that row with the attempt it names - `crw.recordedSignal` returns it as `attempt` - so an unchanged resume after it makes no call), and every
signal after it, are the suffix: the engine ends their rows and the calls they made superseded, the revisions they made get a flow row `revision_superseded`, and the triggers that
replayed enter the first signal that runs again, in the journal's order. A trigger whose lane ran a call live is settled at once (what the lane does after that is live, so a recorded trigger of it that has not come does not come): the live signal does not wait for the end of that lane. A read of the plan whose changed revision a recorded signal took stands (read again, it would answer that signal's own plan as its base and bring no trigger), so the signal can replay, and the node waits for it before its release. Until the signals the earlier process recorded are decided, a node that no recorded signal concerns (a node that was not released, and is not a lane a recorded trigger or a released lane waits for) is not admitted, so it is released under the revision that stands after them and not under one they are about to supersede. A lane that was released under a superseded revision waits until that signal is decided, then is compared with the
revision that stands: a node whose slice is the one that revision was made against keeps its replay and a flow row `release_rebind` says so (its packet is still the one of the
revision it was built from); any other node is released again under the revision that stands - with a new packet when it had not started, from its first position (the position is live, so
is every later one of the lane) when it had started and was not accepted - and a node an acceptance rests on is refused (`accepted_node_changed`, with the route: a plan revision with
`update_node` or `dag-correct`). A milestone that closed with a reason to re-plan is `replanned`: the revision is recorded, the owner thread is asked for the correction issues
(`crw-add-issue` and a plan revision), and the milestone does not report.

**The run process's scheduler (CRW-1277).** The shipped binary installs the lane's operations in the run process (`cmd/crw` assigns
`cli.OpsFor = cli.SchedulerLane`): `ops.NewLane` over `cli.SchedRelay` (the `Relay` over `internal/relay/dagsched`), `CILocalVerifier`,
`StoreDecisions`, the host's reviewer threads and the reporter of F11 (`report.Reporter`: the report's rows in the run's journal, the notice
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
`close` is `AgentClose`. A node sized `child` is released to no one: it goes up as a `size_split` escalation with detail
`child_retired` (the evidence carries the plan's `size_reason`), and no child is started.

## The milestone close

`crw.closeMilestone(ms)` (`crw:lib/close`) closes a milestone over evidence the run makes itself, in this order; the order is what the
close guarantees: the final gate, the crosscheck, then the close. The Go side is `internal/flow/ops/{finalgate,qa,crosscheck,lane_close,audit}.go`,
`internal/flow/reviewadapter` and `internal/flow/audit`.

**The final gate.** `crw.finalGate(milestone)` pins the integrated head H (the tip of the integration target now) and judges the
evidence that stands on it: the test evidence, a verification of H for each command of the plan's required set (`crw.verify` with no node;
a plan that names none requires `crw-dev ci local`), and, when a criterion of the milestone has the surface `web`, `tui` or `desktop`,
the QA receipt (`crw.qaReceipt`). Each piece is `fresh` (it stands on H), `missing` (there is none) or `stale` (it stands on another head,
or on an identity git could not resolve: that verifies nothing); the identities are compared with `source.Compare` of
`internal/pabcd/source`, the rule of the native final gate (`source.CommitIdentity` is the identity of a pinned commit), and the
judgement of every piece is in the `crw.finalGate` row (the call row of the gate). The gate is `pass` when every piece is fresh and
passes, `failed` when a fresh piece does not pass (a verification that ran and failed, a receipt that is `REVISE`, `FAIL` or
`INCONCLUSIVE`), and `pending` while a piece is missing or stale. A verification whose record says no test package ran tests
(`testPackagesRun` 0 in verification-record/1, CRW-1328; a record without the member is unknown, never zero) is recorded on its
`crw.verify` row with the warning `no_tests_run`, and the gate lists it in `warnings` and on its evidence; its state is the evidence's as
before. The gate makes no evidence. The close makes it, at H: `crw.verify(null node)` for each
command named in `need_tests`, and the QA calls below when `need_qa`; then the gate is judged again. A verification that cannot run holds
the milestone (`verify_unavailable`). A head that moves while evidence is made is judged again from the new head, at most three rounds.
A gate that failed ends the close with an escalation (`final_gate_test_failed`, `qa_failed`, `qa_inconclusive`) and the crosscheck does not
run; neither does it when the gate cannot be settled (`final_gate_unsettled`). The native final-gate hook (`[CRW-FINAL-GATE]`, goalplan
`finalGate`) and the standalone `crw-qa` skill are unchanged and read none of this.

**The QA evidence.** The QA capture call (SP-QA-1) is one `executor` call with `qa: true` over the head H: a scratch worktree at H, the
matrix and the captures of sections 1 to 4 of the `crw-qa` skill, the edits discarded, no native helper (`QA_CAPTURE`: the captures with
path, kind and what each shows, the scenarios driven, the teardown). A work grade of C2 (the milestone's `Grade`): the same call's
self-review, one more turn on its thread, is the receipt (`QA_RECEIPT`). From C3 (a grade nobody gave is read as C3), the run starts the two
oracle passes itself (SP-QA-2): two read-only `explorer` calls over the same capture packet, run at the same time - Pass A the design and
functional integrity, Pass B the visual fidelity and the CJK precision (`QA_PASS`) - and the synthesis of the two results is the next turn
of the QA call's thread (`QA_RECEIPT`: `PASS`, `REVISE` or `FAIL`). Why the run starts the passes: section 5 of `crw-qa` asks for two
read-only reviews in parallel, a native thread of the flow may spawn one explorer per call (SP-NAT-1), and a call the run owns satisfies
both; the QA overlay tells the call not to start them and the binding line of the call allows no native spawn, so the hook refuses it.
`crw.qaReceipt` judges what the calls answered and writes the receipt: it is `INCONCLUSIVE` when the capture did not complete or lists no
capture, when the tree the capture call ended in is not the clean head the gate pinned (`capture_source_modified`; `capture_source_unknown`
when the engine recorded none), when a relative artifact path cannot be resolved (`capture_root_unknown`, `capture_path_outside`), when a pass did not complete (the synthesis is then not asked for), is not an explorer call or answers for the other pass, or
when the synthesis is not a completed `QA_RECEIPT` of a call that continues the QA call (the journal's `resume_of`: after a resume the continuation runs on a fork of the thread); a `PASS` over a P0 or P1 finding is not a valid answer,
and the receipt is as bad as the worst of the verdicts it rests on. The spawn decisions of the three calls (SP-QA-1, SP-QA-2 twice) are
rows of `flow_spawn_decisions`; the roles and the pairs the threads read back are in the receipt.

The capture call may edit its scratch worktree, so the head it was started at is not what it exercised. When a QA call completes, the
engine asks the backend for the source identity of the directory the call ended in (`engine.SourceReader`; the agent backend runs
`source.Capture` with the run's own writes under `.crw/` left out) and records it on the completed row (`qa_source`, or `qa_source_error`).
`crw.qaReceipt` compares it with the clean identity of the pinned head and keeps it in the receipt (`source`); a PASS stands only over the
head the gate pinned, and the gate compares the identity of a fresh receipt again. The artifacts of the capture are files of its own
worktree, named relative to it (`.crw/evidence/<session>/qa/...`) while the passes and the self-review read checkouts of their own, so
`crw.qaCapture` resolves each path against the directory the capture ran in (the call-private worktree persists with the run) and the
packets of the passes and of the self-review name that path. The QA mark is part of the input digest of a call: the same call with another
mark is another call on a resume.

**The crosscheck.** After a passed gate, `crw.crosscheck(milestone, gate, opts)` (CRW-1473) reviews the integrated range of the gate
(`base..head`) in one round of two passes: pass A judges coverage (every criterion of the milestone and every decision of the approved
DESIGN, met or not, with its evidence), pass B hunts bugs. The script builds the text of each pass (`crosscheckPacket`: the range, the
criteria, the decisions, which live in the journal's ledger row and not in the checkout, the lens of the pass and the close rule word for
word), and the op runs both passes itself, in Go, through the review adapter (`reviewadapter.Adapter` over the engine's command line,
answering `reviewadapter.AnswerSchema`): no agent call, no reviewer thread and no spawn decision (the close does not call SP-REVF-1).
The op appends the EXECUTOR RECEIPTS section to each prompt: for every node of the milestone it reads the newest completed `crw.unitCheck`
row of role `unit-check` in the run's journal and its executor receipt (an `accepted` copy is read and its sha256 checked; `not_assigned`,
and `no_executor` for a node with no unit check in the run, are listed and block nothing), writes the receipt bytes to
`<run dir>/crosscheck/<milestone>-r<round>-receipts.md`, outside both checkouts, and names that file, its sha256 and each node's state and
digest, so the engine checks each criterion's red-first and mutation-red lines against the receipts. A receipt that cannot be handed over
holds the round before any pass (`crosscheck_receipt_missing`, `crosscheck_receipt_unverified`, `crosscheck_receipt_not_copied`,
`crosscheck_receipt_mismatch`, the row naming the node). The prompt carries refs, never diff bytes: one over `CrosscheckPromptLimit`
(120 KiB, since the engine takes it as one command-line word) is not sent (`crosscheck_prompt_too_large`). The op serves round 1 only: a
prior verdict, or another round (an explicit 0 included; a round not given is 1), is refused `crosscheck_prior_invalid`.

**The run directory and the checkouts.** Every file the crosscheck writes lies under the run's directory (`Deps.LandRoot`,
`<state>/flow/<run>` in the run process), which must be an absolute path: an empty or relative one fails the round before anything is
written or checked out (`crosscheck_no_run_dir`). Each pass runs in a checkout of its own of the gate head,
`<run dir>/crosscheck-wt/<milestone>-r<round>-e<evidence round>-<pass><attempt>-<head12>`, made with `git worktree add --detach` in the
run's repository, and both checkouts of a round are made and checked before either pass starts. A checkout is used only while its `HEAD`
is the head and its working files are the head's tree (`ops.Snapshot` equal to `ops.TreeOf` of the head), a fresh one as much as one a
resume finds again: one at another commit fails the round `crosscheck_worktree_moved`, one with a modified, deleted or untracked file
`crosscheck_worktree_dirty`; no pass runs, and the directory is kept as it is for the child to read. A checkout a pass ran in is removed
after the pass (`git worktree remove --force`, then `git worktree prune`; a path outside `<run dir>/crosscheck-wt/` is never removed). An
ignored file is outside both trees and is not judged: the engines read the tracked tree.

**The engine.** The lane ships two engine profiles, tried in this order: astra (`codex exec -m gpt-6-astra`, family gpt, answering under a
strict form of the answer schema the op writes to `<run dir>/crosscheck/answer.strict.json`) and grok (`grok -m grok-4.7`, family grok),
each pass with a limit of 40 minutes. A round runs on the first profile whose family is no author family of the milestone: the crosscheck
is made by a review engine of another model family than every author, the independence every review of the flow keeps. The authors are
read from the run's own pairs, never from a compiled default and never guessed: the pairs of `architect`, `executor` and `executor-t1` in
the run's role table (a resumed run has the pairs it pinned) and the models of the milestone's author calls in the run's journal. When one
of them is undecided (a role whose pair is empty, an author call whose row names no model) the round is refused
`crosscheck_author_undecided` before any checkout or pass, the detail naming each; when no profile is left the round is refused
`crosscheck_no_independent_engine`. CRW builds with Claude models and reviews on the two shipped profiles, so the supported configuration is
a milestone whose authors are all of family claude; that narrowing is the parent's decision, and a third engine profile, which would widen
it, is an engine change. Round 1 by the authors' families:

| Authors of the milestone | Round 1 |
| --- | --- |
| claude only (the supported configuration) | astra (gpt) |
| a gpt author, with or without claude authors | grok (grok) |
| a grok author, with or without claude authors | astra (gpt) |
| gpt and grok authors | refused `crosscheck_no_independent_engine` |
| an undecided author pair | refused `crosscheck_author_undecided` |

**The verdict.** The two passes run together, each one check of the review adapter; an `invalid` answer is asked once more. A pass that
read no repository is void (`not_read`: under the floors of its engine profile on the commands it ran that succeeded, the tokens it read and the time it
took, CRW-1504): when both passes are void the round gives no verdict at once (`no_verdict`, `crosscheck_no_verdict`); when one is, that
pass alone runs once more on a fresh checkout (attempt 2; the first attempt stays in V's passes as `voided`), and a second void attempt
gives no verdict too. A pass that failed (`worktree_changed`, `tool_failed`, `invalid` after its retry, or a state the op has no rule for,
`crosscheck_pass_<state>`) fails the round, and one that gave no judgment (`quota`, `no_answer`) holds it. The findings of the two passes
are merged into one list (`reviewadapter.MergeFindings`: one finding for each id, the first copy's file, line and text, the higher
severity, a blocking impact over a non-blocking or empty one), and one rule, `ops.CrosscheckRule`, makes the verdict V; the packets of
both passes say it word for word:

> The impact of each finding is one of core_function, safety_or_data, permission_boundary, criterion_unmet, regression_introduced, minor_separable: a finding of P0 to P2 blocks whatever its impact, and of the impacts only minor_separable does not block. The milestone closes only on a verified crosscheck: verified means no finding blocks; a finding that does not block is carried as a follow-up.

A finding that blocks (`premerge.Blocks`) makes V `needs_changes`; the others are carried, by id (`carried`) and as one line each
(`follow_ups`: `id [severity/impact] file:line text`), and no issue is registered for them. The engine's word follows the same rule: an
answer of verified over a finding that blocks is invalid, not overridden. V is `verified` only when both passes answered verified too: a
pass whose own answer is needs_changes makes V `needs_changes` even when no finding blocks (each attempt's word is its pass's `verdict`). V (row `crw.crosscheck`, [journal.md](journal.md#the-proofs))
names the word, the findings, the engine family and model, the round, the milestone and its plan, the gate's head and base, the run, the
author families and each author with its source, name, model and family, the receipts and every attempt of each pass.

**The close.** Only a V of `verified` lets the milestone close: `crw.milestoneClose(milestone, {gate, crosscheck: V})` records the tip it
judged, and refuses, recording no close, evidence whose crosscheck is not a verified V of the gate's milestone and head (and, for a journal
of a close made before the crosscheck, evidence with no crosscheck whose reviewer-final review is not verified, or evidence with neither):
`close_unverified`. When the target is no longer the head the gate judged, the close is judged again from the new head, at most three
times. A crosscheck that does not verify the range keeps the milestone open, and the close stops:

- `needs_changes`: the run ends escalated `crosscheck_pending`, ask `crosscheck_pending`, with V in the escalation's evidence and in the
  run's value (`crosscheck`, V as the op recorded it) and the fix packet (`fix`, `crosscheckFixPacket`: the blocking findings by id, the
  range as `git diff <base>..<head> -- <files>`, the milestone's criteria and the route of the correction). Every node of the milestone has
  landed, so the route is a correction node of a new plan revision: a correction issue of the milestone naming the finding ids
  (`crw-add-issue`, a plan revision), fixed by a new executor subagent on a branch from the integration tip, landed with
  `crw flow start land-head`, and the verdict and the landed head reported up. The child hands V and the fix packet up to the parent
  (repository session) with `crw flow report-up`; the run makes no executor call for it. The escalation's id is of the run, the call and
  V's canonical digest: a resume that judges the range again (the target moved, or a proof error ran the gate again at the same head)
  and gives another V is a new escalation with the new V, the current question of the milestone, told again, while the earlier
  escalation keeps its V; the same V again is the same escalation.
- `refused` (`crosscheck_author_undecided`, `crosscheck_no_independent_engine`, `crosscheck_prior_invalid`): `crosscheck_refused`, ask
  `configuration`, a question for the parent.
- `failed` (`crosscheck_no_run_dir`, `crosscheck_worktree_dirty`, `crosscheck_worktree_moved`, `crosscheck_prompt_too_large`, a pass that
  failed): `crosscheck_failed`, ask `crosscheck_failed`; `no_verdict`: `crosscheck_no_verdict`, ask `crosscheck_no_verdict`. Both go up to
  the parent.
- `held`: the milestone is held (`crw.hold` with the round's `hold`), and a resume runs the round again once the cause is gone.

A closed milestone's result carries V as `crosscheck`, and its report names the gate, the crosscheck and the reflection's alignment
([reporting.md](reporting.md)). What the alignment of the range does is in
[The architect's continuity and the re-plan](#the-architects-continuity-and-the-re-plan).

**The audit and the archive.** `crw.settleReviews(ms)` runs `crw.audit`, then `crw.archive`. For each acceptance of the milestone on the
agent path, the audit reads the reviewer call's persistent thread from the host (`thread/turns/list` with `itemsView` full, and
`thread/items/list` for a turn the list returned in summary) and checks that the journal recorded the thread, a turn id and the result
digest; that the turn is `completed` on the host; that the digest of the last `agentMessage` is the result digest of the journal; that the
packet the thread was sent names the head and the criteria digest of the acceptance (the `HEAD:` and `CRITERIA DIGEST:` lines of the
reviewer packet); and that the pair of the executors that wrote the head was read back (the calls the recorded commits of the node name; an executor attempt that
never created a thread or never completed wrote nothing, and a retry that replaced it is judged by its own pair). A reviewer call that continues
another on its thread records that thread on its prepared row and creates none, so the thread is the continued call's; its packet is the first
turn of the call that started the thread. The result of an acceptance is `ok`, `mismatch`
(the record and the host differ, or the record is incomplete) or `unobserved` (the host does not list the turn, does not return its items,
does not answer, or the executor's pair was never read back); unobserved is not a pass. The results are a row `crw.audit` in the journal.
A mismatch is escalated (`audit_mismatch`). `crw.archive` then archives the reviewer thread of an acceptance only when the newest audit
row about it says `ok`, reading the rows and not the script; a thread already archived is not archived again, and a host that refuses
keeps the thread. `crw flow audit` is the same audit on demand.

**`crw flow audit`.** `crw flow audit (--run RUN | --acceptance ID) [--milestone M] [--state DIR] [--socket PATH] [--json] [--no-record]`
opens the relay store the journal of the run is in (an acceptance given alone is looked up in the store), connects to the App Server
control socket, and prints the result of each acceptance. It exits 0 when every acceptance is `ok`, 1 when one is a `mismatch` or is
`unobserved`, and 2 for a command line, a store or a run it cannot use. It appends its result to the journal as a `crw.audit` row with a
call id of its own (`audit_...`), unless `--no-record` is given or the run has not ended: the journal has no `finished` or `run/paused` row
after its last `started` row (a run that is still writing owns its journal, and so does a resumed process after an earlier process's end).
The row is appended under the run lock, taken without waiting after the host reads, on the journal opened again under it, where the
end is read again: while a process holds the run (one that wrote its end row and still reconciles what goes up) nothing is recorded.

**The scenario harness** (`internal/flow/scenario`) runs this script on the engine over the fake App Server host, a temporary relay
store (the journal and the decision rows), a temporary git repository with an integration branch, a scripted model, a fake relay and
a fake verifier. Its scenarios are the tests of the project lane named in CRW-1201; `go test ./internal/flow/scenario/` is the
command.

## crw flow lint

`crw flow lint` checks scripts for what the engine cannot refuse early. It reads the text of each script (the body parsed
with its scopes resolved) and runs it on the fake backend, reading the journal; a finding both find is reported once. It
exits 0 when every script is clean, 1 when one has a finding or does not load, 2 for a command line it cannot use.
`--no-dry-run` checks the text only; `--args` gives the dry run its `args`. A dry run that ends in an error of the
script's own (it reads an arg it was not given, it calls a `crw.*` operation this build does not have) is a note in the
report, not a finding.

| Rule | What it finds |
| --- | --- |
| `lifecycle_land_twice`, `lifecycle_land_without_review`, `lifecycle_order` | A lifecycle automaton per executor node and per function that calls `crw.commit`, `crw.verify`, `crw.review`, `crw.unitCheck`, `crw.land` or a land release: build once (a rebuild is allowed until the node has landed), then rounds of commit, verify, review, then land at most once, in one of two ways: from a completed review, or, the second way, from verified when a completed unit check of the node's current tree stands (build, unit check, commit, verify, land; also release(land), verify, unit check, land). A `crw.unitCheck` is legal while the node is built (before the commit, also after a fix) or verified (a committed head) and changes no phase: it sets the node's checked flag, which a build, a resumed executor call of the node (a fix changes the tree) and a land release clear and a commit and a verify keep; where paths meet, a node is checked only when it is checked on every path. A land release is a `crw.release` whose fourth argument is an object literal with a `land` key (in the journal, a completed `crw.release` row with `kind` `land` and `released` true): `land-head` checks out a committed head, so it moves a node before the build or built to committed; any other release is no event, and an identifier as the fourth argument is none either. A call starts its event where it runs and completes where its promise is used: awaited, returned, passed on or kept by the expression around it (the right side of `&&`, `||` or `??`, the left side of `||` or `??`, the last part of a sequence, a branch of `?:`, the value of an assignment), or read (passed on, closed over, `{ p }`) through a name that holds it. A promise whose value is discarded never completes: a call used as a statement, under `void` or another unary operator, in a sequence but its last part, on the left of `&&`, in a test, a comparison, arithmetic or an untagged template, and a held name read only in such a place; a stored promise that nothing reads leaves the node where it was, so `crw.review(...)` without `await` is not a review a later `crw.land` can follow. Started is enough for two rules: a second `crw.land` is a land twice whether or not the first was awaited, and a `crw.commit` started after a review leaves the node built (a change no round has committed), so the land needs a new round. A build, commit or land of a node, started, begins a new generation of it: a promise of an earlier call of that node that settles afterwards belongs to the head before it, so it neither makes the new head committed, verified or reviewed nor takes a started land back (an old `crw.review` awaited after a new `crw.commit` or after `crw.land` changes nothing). Where paths meet, a name keeps every call it may hold there, each with its own generation, and reading it puts the node in any phase one of those paths reaches, so the result does not depend on which branch the text lists first: `let r = crw.review(...)`, then a new round on one branch and `r = crw.review(...)` of the current head on the other, then `await r` and `crw.land`, has a reviewed path and is no finding in either branch order. A `break` or `continue` with a label goes to the statement the label names. A node is named by the `node` option of its executor call and by the first argument of the `crw.*` calls, so the phase of one node never stands for another's; a node the function does not build is taken as built. A violation is reported when it is illegal on every path that reaches it; the finally block of a try is checked from the state at the end of the body or handler and from the state of each completion that leaves through it (a throw no handler takes, a return, a break or continue to a loop outside the try), and that completion then goes on where it was going with the state the block leaves, unless the block returns, throws, breaks or continues itself. A handler starts from the state before the body or after any call in it, and an exception it takes no longer reaches the finally block. |
| `reviewer_family_same` | A review call (its prompt is `crw.reviewPacket(...)` or its label starts with `review`) whose model family (`claude`, `gpt`, taken from the model of its role's pair) is the model family of the author call before it. A role is not a family: an `explorer` on the executor's model is no independent review. A continuation (`resume:`) is judged by the pair of the thread it continues. The dry run judges the pair each call was configured with. |
| `label_duplicate` | A label used twice on one path, or on a call that a loop with at least two rounds shown by the text (`for (let i = 0; i < 3; i++)`, `for (const x of [..])`) runs every round: no break, continue, return or throw in the loop, and no condition between the loop and the call. The first round of a `do … while` counts as one run, so `do { … } while (false)` uses its labels once. Any other repeat is judged by the dry run, which sees whether the call really repeats. Calls without a label do not count. |
| `executor_t1_without_step_decision` | An `executor-t1` call without a `stepDecision`, or whose `stepDecision` is not the id `crw.tierStep` returned earlier on the same path and not overwritten since (the walk follows assignments through branches and loops). A plain function call stays on its caller's path, so a helper that reads the id from its closure or a parameter is left to the dry run; a function given to `parallel`, `pipeline` or `crw.dag` runs on a child path, and a decision it reads from the function that starts it is refused. A continuation inherits it from its handle. The dry run refuses an id that no `crw.tierStep` returned on the path. |
| `classify_arg`, `resume_arg` | `crw.classify(...)` or `resume:` given something other than an `agent.call` result or its handle: the output of `agent()`, a `.output`, a bare value. |
| `determinism_stub` | `Date.now()`, `Math.random()`, `new Date()` and `Date()` of the engine; a name the header imports (`import { now as Date } from './clock.js'`) is the script's own binding and is not judged as a global. |
| `lane_reads_mutable_let` | A `crw.dag` lane function or `onSignal` function that reads an outer `let` binding that is assigned again anywhere. |
| `alias_ambiguous` | A `haiku` call without isolation, with the two fixes in the message. |
| `call_refused`, `path_concurrency`, `load`, `parse` | The dry run's own findings: a call the engine refused, a second outstanding call on a path; a script that does not load. |

The static rules see the text only: a role that depends on a variable is judged on the dry-run trace.

## Parity corpus

`testdata/flow/parity/<name>/` holds Claude workflow scripts as `claude.js` (byte for byte) and their CRW ports as
`crw.js`, with a `golden.json` of the port's call graph, return value and, per call, the resolved role key, the sandbox,
`requestedPair` and `configuredPair`. A port differs from the original only by keys added to the options object of
`agent()` calls (`isolation: 'worktree'`, `agentType: 'explorer'`, `stepDecision`) and one `crw.tierStep` line before a
`model: 'sonnet'` call; the corpus README gives the reason for every such line and for every line that names
`ocx-gpt-6-1-sol`. A script that reads `args` gets them from `args.json` beside it, and the `real-*` directories hold
originals with a `source.json` naming the file they were copied from.

## Limits

A step is one uninterrupted stretch of script execution: from the run's start, or from the engine delivering a result, until
the script awaits again or ends. A step that runs longer than the step limit (10 s) without awaiting is interrupted, and so
is a step during which the Go heap grows by more than the heap limit (512 MiB, sampled; the figure covers the whole process,
so it is a ceiling against runaway allocation and not an exact account). The run ends with status `script_error` and error
kind `step_limit` or `heap_limit`. The call stack depth is bounded as well. Rendering a thrown value and serialising the
returned one run the script's own `toString`, `toJSON` and getters, so they run under the same limits; one that throws ends
the run with error kind `js_error`.

The run also ends as soon as the main script's promise is rejected (not after the work still queued has run: a
continuation of another path that has not started does not start) and as soon as the context of `crw flow run` is
cancelled (error kind `cancelled`). The value or error of a `workflow()` child is rendered in the child's VM under the
child's own step and heap limits, also when the child had settled at once. A child that awaits something nothing will
settle does not keep the run waiting: it ends `unsettled`.

A run that ends with an error (a limit, `path_concurrency`, an engine error) stops at once, in a `workflow()` child as in the
main script: no completion resumes the script, nothing it still awaits keeps the run waiting, and every call in flight is
cancelled and writes its terminal row (`superseded`) before the run's own last row. A script that catches the error and
goes on may finish the statements it is running, but the first `agent()`, `agent.call()`, `crw.*`, join or `workflow()`
it opens after that stops it for good: no position, journal row or host request follows the error.

## Journal

`crw flow run` keeps the journal in memory; a run that must survive its process writes it to the relay store's flow tables
([journal.md](journal.md) says how, and what a worktree proof reads from it). Each call journals the
rows `prepared` (written before anything is sent, with the role, pair, node, `stepDecision` and, for a continuation, the
thread it continues and what it inherited), `thread_requested`, `thread_started`, `turn_requested`, `turn_started` and one
terminal row (`completed`, `null_model`, `held`, `refused`, `outcome_unknown` or `superseded`); a session call writes `step_published`
in place of `thread_requested` and `turn_requested`, `session_attempt_lost` for an attempt it lost, and `evidence_placed` before it
writes a flow evidence record under a directory no row of the call's newest generation names yet ([Session calls](#session-calls)).
A held row records whether the
call's packet reached the host (`input_state`: `delivered` or `not_delivered`) and the `codexErrorInfo` verbatim; on a backend
that places calls an executor's held row also carries `p_held`, the working snapshot of its worktree when the failure was
classified, and its completed row `pending_tree`, the working snapshot when the turn ended. The run record holds the workflow name, the script digest, the helper digests, the digest of `args`, the meta, and
the path of the helper role store with the canonical digest of the seven role pairs (`policy_source`, `policy_digest`),
the role table's warnings (`policy_warnings`) when it has any, and the `backend` the run's calls run on (`session` or
`app-server`).

The `prepared` row also carries what the resume walk compares: `input_digest` (the prompt, schema, role, pair, node, options,
context boundary and the joins before the position), `prompt_digest`, `context` and, with `Config.InputSnapshot`, `input_snapshot`.
A join or `workflow()` position writes `join_opened` and `join_settled` flow rows, the latter with the `digest` of its item paths'
results; a resume writes `hold_cleared`, `worktree_preserved`, `worktree_restored` and `checkpoint_started` flow rows
([journal.md](journal.md#the-resume-walk)).

A backend that places calls adds to a call's rows: the `allocation` member of `prepared` (below), `fork_requested` and
`fork_started` in place of `thread_requested` and `thread_started` for a continuation that runs in a restored worktree, and
`interrupt_requested` before a turn the engine interrupts. Flow rows named `escalation` carry a `reason` (`quota_or_rate`,
`restore_snapshot_mismatch`, `dispatch_unknown`) with the call and node they stopped.

The run surface (`crw flow`, [flow.md](../../plugins/crw/skills/crw-canon/references/flow.md#the-crw-flow-command-table)) adds the rows it writes
and reads. A pause asked for from outside the engine (`crw flow pause`, a run deadline, the drain of `crw flow answer`) is one `run_paused` flow row (`by`, `reason`)
and then the calls that would send end held `engine_paused`. The operator's pause (SIGUSR1) and a deadline interrupt the turns in flight;
the drain of an answer to a run a process holds (SIGUSR2, `by` `answer`) lets them end and holds each call's next request, and is
asked once (a second drain, or a drain after a pause, writes no row). A process the operator or an answer paused (`by` `operator` or
`answer`) that would end completed ends with a `run/paused` row (`status` `paused`, and the `by` and `reason` of the last such ask)
instead of the `finished` row:
the engine's status is `paused`, the run process exits 0 and the hook tells no completion; a deadline, a token budget or a quota
pause ends with the `finished` row as before. A pause asked once the process has decided its end (while it writes its end row or tells
its completion) is refused and writes no row, so the `run_paused` rows and the end row agree. `crw flow show`, the digest and `crw flow
next` say who paused a run that ended paused from the `by` and `reason` of its `run/paused` row, not from a later deadline ask. An answer to a wait is the `decision_applied` flow row `crw flow answer`
appends under the run lock: `ask` (the question's id), `node`, `run`, `revision`, `attempt`, `plan_basis`, `scope`, `target`, `action`,
`class` (`escalation` or `jun_decision`), `applied_by` and the `decision` (`id`, `option`, `text`, `by`, `via`, `answered_at`,
`authority`, `authority_ref`); `crw.decide` reads `ask` and `node`. A run cancelled while no process held it ends with a `finished` row
of error kind `cancelled` written by `crw flow cancel`. The `finished` row of a completed run whose script returned an object with a
string `state` carries `outcome`: its `state`, `reason`, `source`, `escalation` and `milestone` (each one line, at most 120 runes) and
`lanes`, the lanes of its `lanes` array that did not land (`node`, `state`, `reason`, at most 16, with `lanes_more`). A value whose
`milestone` member is itself an object with a string `state` (a night batch returns `{state: closed|incomplete, milestone: <the project
lane's result>, lanes}`) carries the outcome of that object, and its own state as `run_state`, so a night batch tells the milestone's
`held`, `escalated` or `stopped` and its reason. A `milestone` member that carries an `id` is a milestone (what `crw.milestone` hands
a script), not a result, and is not read as one. The outcome state is read in this order: the `milestone` object without an `id`
and with a non-empty string `state` is the wrapped result, and its state wins; a `milestone` member that carries an `id`, or that
has no non-empty string `state`, is not read as one, and otherwise the top-level string `state` is the outcome state. A value whose
own `state` is not a non-empty string has no outcome. The top-level string `state` of a completed script is its outcome
whatever that state means to the script: the engine does not restrict its values (the repository's workflows return the
milestone's state). It is told in place of
`status=completed` (a script that wants `status=completed` returns no `state`). The completion notice and `crw flow ps` read the outcome from there ([reporting.md](reporting.md)). The state of a run (`running`, `paused`, `dead`, `held`, `completed`,
`blocked`, `cancelled`, `failed`) is not a row: it is read from the run lock, the last `started`, `finished` and `run/paused` rows (a
run no process holds whose last process ended with `run/paused` is `paused`, not `dead`), the calls' rows
(`internal/flow/callstate`), the questions no `decision_applied` row has answered and, for `blocked`, the lane's own rows. A run meant
to start the nodes of a plan (a run of `project-lane` or `night-batch`, an old run of an issue child's lane, or a run whose arguments name a `plan`) whose last
process finished `completed` with nothing left to go on with is `blocked` when no process of it started a node (no completed
`crw.release` row released one, no `crw.releaseChild` row an older build recorded released a child, no `crw.integrate` row landed one) and items of its joins threw
in its last process (`item_error` rows: a lane of `crw.dag` whose `crw.release` failed is one). The engine's status of the run stays
`completed`. A node the scheduler held back unreleased in the last process (capacity, a region in use: a wait a resume asks again) keeps the
run `completed`, and so does a node that started. Another script run (a judge panel, a scout) that names no plan never meant to start a
node: an item of it that threw is shown, and the run stays `completed`.

The intent rows say what a later process needs to find the request again. `thread_requested` and `fork_requested` carry the
`cwd` the thread is to run in, the `send` ordinal of the request (1, then one more each time reconcile proved the previous
request absent and sent it again), `at`, the wall time of the intent in milliseconds, and `owner`, the run process that
holds the connection the request goes out on; `fork_requested` also carries the source thread, `through_turn_id` and the
`prefix` the fork must hold (the whole history of the source through the boundary, the turns it inherited when it was itself a fork
included). `turn_requested` carries the turn's input (without its marker line) and `resend` on a turn that
is sent again.

## Reconcile and recovery

A request that was sent and got no answer, or a run process that died after an intent row or after a send, leaves a call whose
last row is an intent: `thread_requested`, `fork_requested`, `turn_requested` or `turn_started`. A call whose last row is
terminal - a held call included - is never looked up on the host. `Reconcile` (`internal/flow/engine`) looks up every other
call of the run, and the calls whose `outcome_unknown` it left standing, before a resume goes on and before a node starts a new
attempt. It finds a call only by what the 0.154 host returns: the exact `cwd` and the `appServer` source of `thread/list`
(every page), `Thread.parentThreadId` null, `Thread.createdAt` not earlier than the intent minus five minutes, for a fork
`Thread.forkedFromId`, and for a turn the `clientId` of its `userMessage` item (then the first input line) read with
`thread/turns/list` (`itemsView: full`, every page) and `thread/items/list`. `developerInstructions` and `threadSource` are not
identity: the host does not return them.

- **`thread_requested` without `thread_started`.** A candidate that holds any turn is foreign: no turn of the call is sent
  before `thread_started` is recorded. Candidates without a turn are interchangeable containers; the one with the earliest
  `createdAt` (then the lowest id) is adopted - `thread_started` with `recovered: true` and the evidence - and the others are
  archived, each with an `orphan_thread` flow row.
- **`fork_requested` without `fork_started`.** The same with candidates whose `forkedFromId` is the recorded source thread. A
  fork carries the copied prefix, so a candidate is adopted only when its turns are exactly the recorded prefix: the same
  number, the same order, each matching by turn id or marker, all ended and all read in full. A turn after the boundary, a
  reordered or different prefix, or a prefix turn that cannot be read in full is foreign or unreadable.
- **No candidate.** The request is absent only when it is proven finished: the connection that carried it is closed (the
  backend's own record of the connection the frame went out on, or the death of the run process that held it) and a second
  complete listing taken at least two minutes after that close - the creation grace of `internal/relay/managed` - is still
  empty. Reconcile records when it first saw the connection closed (`creation_closed_seen`) and waits for the grace. Then, and
  only then, the call sends the request again under the same call id and cwd with the next `send` ordinal
  (`creation_resendable` records the proof); a thread of the first request that shows up later still holds no turn and is
  archived as an orphan at the next pass. A request that was never written needs no grace. While the connection is open and the
  answer pending the pass waits and sends nothing; if the answer is still missing when the wait ends the call stays open.
- **`turn_requested(k)` without `turn_started`.** A turn whose marker is `<call_id>/t<k>` is adopted (`turn_started`, recovered).
  Only when every turn examined is read in full, none carries the marker and the thread is idle is the turn sent again with the
  same identity; a turn that stays `notLoaded` or `summary`, or an active thread whose running turn has no marker, is not an
  absence.
- **`turn_started`.** A running turn is attached to again (the call waits for it, no new turn), an ended turn is read and
  harvested, a failed or interrupted one is classified by `codexErrorInfo`, and a turn the host does not list or cannot read is
  unknown.

What the pass cannot establish - a listing or a history that cannot be completed, a foreign or unreadable candidate, an absence
without the proof, a running turn without the marker - appends `outcome_unknown` (reason `dispatch_unknown`), writes the
`dispatch_unknown` escalation naming the node and puts the node in `recovery_hold`: no request is sent again, no new attempt
starts, no tier step is taken. Each resume runs the pass again over the calls whose outcome is unknown; a pass that establishes the
outcome appends `recovery_cleared` (mode `reconcile`, with the evidence) and then the recovered rows after `outcome_unknown`, and
the hold ends: the call is open again, the resumed call goes on from that outcome and the unknown one is not replayed. This holds
for an outcome that needs no row of its own (a turn already recorded, or a request proven absent) as much as for an adopted thread.

`crw flow recover CALL --adopt THREAD|TURN | --absent` is the only manual way out of a hold. `--absent` reads the host again
with a complete listing and history and accepts the call only when the proofs above hold (turnless candidates only, an
exact-prefix fork, a closed connection with the grace passed, or for a turn no marker and an idle thread); it appends
`recovery_cleared` with the evidence and then the rows the proof establishes, and exits 1 without appending anything but the
sighting of a closed connection when it does not hold. `--adopt` names the thread (at the call's cwd, a root thread of the
backend's source - the `appServer` source reconcile lists by - for a fork a fork of the recorded source) or the turn (on the
call's thread, carrying the call's marker when it has one) that is the call's, and records that an operator chose it; the chosen
turn is written as a `turn_started` row (`replaces` names the turn the call had recorded), so the next resume follows it. The command works on the run's stored journal and its host: the binary opens the relay store (`--state DIR`, else
`CODEX_SESSION_RELAY_STATE`, else the relay's discovery for the control socket) and connects to the App Server control socket
(`--socket PATH`, else the default one); `--run RUN` names the run, and without it the run that holds the call is used. A store
that does not exist is an error, never a new store.

A new attempt starts after the turn of the attempt it replaces has ended: a call the previous process left running on the node
(one whose position the resumed script has not taken over) is interrupted with `turn/interrupt`, the interruption is confirmed with `thread/read`, and only then is its call ended
`superseded` (reason `superseded_by_attempt`) and the new thread started. A turn that cannot be confirmed interrupted holds the
new call. A call is either taken over by its position of the resumed script or ended superseded by a new attempt, never both:
the two decisions are made under one lock, the first stands, and a position that is reached after its call was superseded runs
as a call of its own (the same call id, a new `prepared` row), so a call has one terminal row for each attempt. A new attempt
keeps the call id of its position and so its `<call_id>/t<k>` operation marker; one thread never receives a marker twice, and
two threads that carry the same marker are two attempts.

A run resumes a journal with `Config.Resume` (`ResumeOptions` set the grace and polling timings of the reconcile pass), and
`crw flow resume RUN SCRIPT` runs it on the stored journal and the App Server. The pass runs before the script; the script then
runs again and each position replays or runs live by the rule of [journal.md](journal.md#the-resume-walk): a position replays
only when every earlier position of its path did, its input digest, input snapshot and effect proof are the recorded ones and its
last event is not `held`; a call that reconcile decided goes on from where it stopped (the adopted thread gets its first turn, a
creation proven absent is sent again, an adopted turn is waited for or harvested and then validated and repaired like any turn);
a held call whose conditions hold goes on in place, with `hold_cleared` written. The first position that does not replay and
the rest of its path are the suffix: its calls end `superseded` with the reason `superseded_by_resume`.

`crw flow resume` goes on with every call that is not settled and not in recovery hold (an intent nobody answered, a hold that was
cleared, a call that ended held); `crw flow recover` settles a call in recovery hold, and a call it does not accept is a call
`crw flow resume` takes.

A call that ended held has one resolution command whatever the reason (a spent budget, a refused `thread/start`, a failed turn, an
engine pause, a flow binding that could not be written): `crw flow resume RUN SCRIPT`. A held row asks no operator input
(`--adopt`, `--absent`); what the call needs is the condition outside the run gone and a process that goes on with it. That
process proves on the host what the hold left open before it sends anything, and the proof decides what happens:

- A turn of the call may still run: a pause whose `turn/interrupt` the host ignored, or whose end did not come in time, ends the
  call held `engine_paused` all the same. Every thread a held attempt of the call sent a turn on is read (`thread/read`); a running
  turn of the call is interrupted and its end confirmed with `thread/read`, as a supersede does. While a turn still runs, the
  thread cannot be read or it runs a turn that is not the call's, the call stays held as it is: nothing is sent on its thread,
  and no new attempt starts on its node (no worktree is kept or restored, another call of the node ends held `recovery_hold`).
- A refused request (`thread/start`, `turn/start`) made nothing only if the host shows it: the listing of the call's cwd, or the
  marker of its operation on its thread. When that proof fails (the listing is not complete, a thread with turns stands at the
  cwd, a turn carries the marker), nobody can prove the outcome: the call ends `outcome_unknown` (`dispatch_unknown`) with an
  escalation. It is then in recovery hold, and dag-restart names `crw flow recover` for it.
- The resumed process binds every engine turn as the first process does (`host_config_flow_binding` is written before the turn);
  while the flow-bound directory is still unsafe the call ends held for it again with nothing sent.

When the proof holds, the call goes on in place (the original packet before a thread existed, the continuation instruction after a
delivered turn) and `hold_cleared` is written; where it cannot go on in place (a worktree failure, files that are not the hold
snapshot, an attempt that waited for an older turn and made nothing) the call runs again as a new attempt. A resume while the
condition outside the run still holds (the budget, the host's availability) ends the call held again.

## Session calls

A run whose configuration names a session host (`engine.Config.Session`: the host, the step bus, the run's owner thread) runs every
agent call as a session call (CRW-1316). The owner thread - the child (project session, relay role `parent`, [the level table](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels)) - runs the call's turns on role subagents it
spawns, and the engine starts no thread and no turn of its own: it publishes a step, binds the subagent the owner reports, reads
its answer from the host and records it in the rows relay acceptance reads. Everything around a turn is the engine's as for any
call: the `prepared` row and its allocation, the gate, the schema validation and its one repair, the terminal rows, the pending
tree and the effect proofs.

The run's `started` row records `backend: session` (`app-server` for a run on the App Server; a run that records none is an App
Server run). A resume runs a journal on the backend it records: a resume on the other backend is refused with the reason before
any row is written. `crw flow shadow` replays either from the trace and names the recorded backend in its report (`backend`).

### The step

- Every role's call runs in a directory P of its own, made by the placer after the `prepared` row as for any call: the attempt
  worktree of an executor, a call-private checkout at the `head` option for every other role, the scratch worktree of a QA capture
  call. P is the call's cwd for every purpose (`pending_tree`, `p_held`, `qa_source`); the cwd the host reports for a subagent is
  its parent's, and is recorded only as `host_cwd`.
- A turn is a step. Its intent row `step_published` (k, `op` `<call>/t<k>`, `marker` `<call>/t<k>-a<n>`, `attempt` n, `action`,
  `input`, `input_digest`, `packet` and `packet_digest`, `role`, `agent_type`, `model`, `effort`, `workspace` P, `agent_id` of a
  `send_input`, the `note` about earlier attempts, `previous_answer`, the answer a repair turn repairs (a later attempt of the turn on a new
  subagent names it in its packet, after a restart too), for a `send_input` `tokens_seen` and `call_tokens`, what the turns before it
  counted, and the turn's place in the call's conversation: `conversation_turn`, the conversation turn it is (the call's task turn 1,
  its repair 2, on whatever subagent, attempt and k it runs), and `replaces_turn`, the turn it replaces when it is a turn run again;
  for a spawn step of a gated executor role in a run with an evidence cwd, `evidence_assignment`, the flow evidence record of the
  step, `evidence_cwds`, the directories it was written under before the row, and `evidence_ignored`)
  is committed before the packet `steps/<call>/packet-t<k>-a<n>.md` and the step
  `steps/<call>/t<k>-a<n>.json` are written under `<state>/flow/<run>/` (`engine.FileStepBus`). A step is `spawn` (a first turn, the
  attempt after a lost one, a continuation, a repair whose subagent is closed) or `send_input` (the repair turn on the open
  subagent); `close` steps (`steps/<call>/close-a<n>-<agent>.json`, one file per subagent, so two closes of one attempt never
  overwrite each other) name a subagent to close. A step whose packet or file cannot be written (a full disk) was sent to nobody:
  the run writes the flow warning `step_unwritten` (`marker`, `k`, `attempt`, `action`, `input_state: not_delivered`, `error`)
  and writes the step again at the poll until it can or the turn ends; the call is never ended unknown for it. A failure that does
  not pass shows how long in the record: the warning is written at the 1st, 10th, 100th, ... failure with `failures` and the
  `first_at` and `last_at` times, and a closing one whenever a warning was written: `written: false` when the turn ends
  unwritten, `written: true` when the step is written, after any number of failures.
- The spawn message is short; the packet holds the rest:

  ```
  crw-flow-op: <call>/t<k>-a<n>
  CRW-ROLE: <role>

  Your shell starts in a directory that is NOT your workspace. Your workspace is <P>: run every command with workdir <P> and edit only by absolute paths under <P>.
  Read the task packet <packet> in full and follow it exactly. You are a leaf subagent: never spawn agents.
  Your final message is exactly the JSON object the packet names, and nothing else.
  ```

  A `send_input` message is the marker line and the turn's input. The packet is self-contained: where the subagent works (only
  under the absolute path P; a role that writes nothing changes nothing; nobody runs `git commit`, `add`, `branch`, `reset` or
  `checkout`), the role's overlay of `internal/flow/roles` without its native-spawn section, the earlier attempts, for a
  continuation the packet of the call it continues and a file with that call's answer, the task, and the output contract (the
  schema the final message satisfies).
- The owner reports what it spawned with a dispatch report in `<state>/flow/<run>/inbox/` and reads the run's answer from
  `replies/` (the commands are the CLI's). The run binds a subagent only when the host shows it is the owner thread's
  (`parentThreadId`), carries the marker in a turn's first user message and is configured with the role's pair. The answers:

  | Reply | When |
  | --- | --- |
  | `bound` | the subagent is the step's; its rows are written |
  | `pair_mismatch` | the subagent runs another pair: close it and spawn again with the role's pair (named); the step stays |
  | `retry` | the host or the marker turn cannot be read yet |
  | `refused` | another run, a report without a subagent for a step the run did not publish, or not a subagent of the owner thread |
  | `close` | the report names an attempt that ended (answered, lost), a step the run did not publish (an early report), or a second subagent for a bound attempt |

  A role whose pair the role table leaves to the host is bound without the pair check (`pair_unchecked`); an `agentRole` other
  than the expected agent type is recorded (`session_agent_role_mismatch`) and bound. A role that names a model and leaves its
  effort undecided is bound on its model alone: `thread_started` records the effort the host applied, with `effort_unchecked`, as
  the call's pair. A continuation is judged by the same rule: only the decided parts of the role's pair are compared with the pair
  the call's subagent was bound with, so a call bound `pair_unchecked` or `effort_unchecked` continues on a new subagent whose bind
  records the pair it runs, and a decided part that differs is refused (`pair_refused`); an explicit `effort` on a continuation of a
  role whose effort is undecided is compared with the effort the call's subagent ran. While a step waits, the run also looks the
  marker up on the host - the subagent that carries it for a spawn, the turn that carries it on the subagent for a `send_input` -
  so a subagent spawned, or a repair sent, and never reported is bound all the same.
- A bind writes `thread_started` (the subagent's `thread_id`, the host-read `model` and `effort`, `cwd` P, `host_cwd`, for a step
  with a flow evidence record `evidence_cwd`, the cwd the stop hook claims its receipt under,
  `parent_thread_id`, `agent_role`, `attempt`, `via: session`, `rollout_path`, `marker`, and `prior_pairs`, the pairs earlier
  attempts of the call were bound with when they differ) and `turn_started` (k, `op`, `turn_id`, `attempt`, `marker`, and the
  `conversation_turn` and `replaces_turn` of its step) in one batch;
  a `send_input` turn writes `turn_started` only. The relay reads the call's pair from its newest `thread_started` row and an
  author call's earlier pairs from `prior_pairs`. A bind whose host cwd no row of the call's newest generation names writes
  `evidence_placed` for it before it writes the step's flow evidence record there and before `thread_started`; a record that cannot
  be placed there leaves the subagent unbound, and the call is held `evidence_assignment_failed`.

### The harvest

- The run reads each bound subagent until its marker turn ends. The answer is the turn's last `final_answer` message, or from a
  provider that sets no phase the last message without one when no commentary came before it; a commentary message is no
  answer. When prose precedes the JSON, the object that ends the message is taken out of it: the completed row keeps the canonical
  `result`, its `result_digest`, and the message as it was (`raw_text`, `extracted`). The rest is the engine's: schema validation,
  one repair, `null_model`, `completed`.
- A turn that ended `failed` or `interrupted` with an error is `TurnOutcome.Failed` with its `codexErrorInfo`: a usage or rate limit
  pauses the run, an exceeded context window is a `null_model`, a host failure holds the call, as for any call.
- Tokens are the rollout's last `token_count` (thread/read `path`), less what the subagent's earlier turns counted, in all its parts: the input and its
  cached part, the output and its reasoning part, the total (`tokens`: `input`, `cached_input`, `output`, `reasoning_output`, `total`; a part that is 0 is left
  out of the row, and a row written before the parts reads them as 0). A recovered `send_input` turn takes that and the call's tokens, in the same parts, from
  its `step_published` row (`tokens_seen`, `call_tokens`), so it counts only what it adds. The call's tokens it takes are in no terminal row (the process that
  counted them died before it ended the call), so the resumed process counts them against the run's figure when it takes them, and the call's terminal row
  carries them: `RunSpent` and the process's `spent()` count them once. The App Server backend records the same parts from
  `thread/tokenUsage/updated`. A rollout that cannot be read leaves the turn unmeasured: the `tokens_unmeasured` row says `pauses: false`,
  and a session run's token ceiling counts the calls it measured.
- A pause, a cancellation or a deadline interrupts the bound subagent's running turns and reads the thread until it is no longer
  active; only then is the call ended `held` (or `superseded`). A call that waited for a dispatch retracts its step. A step whose
  subagent is known (a `send_input`, an adopted one) and was not bound yet has the turn that subagent started for it bound first
  (`turn_started`), so the rows name the turn the interrupt reaches; a spawn step no subagent was bound to has no turn to interrupt.
  The rows before an interrupt, that `turn_started` among them, are stored before it is sent: a journal that batches its rows
  commits them first, so a process that dies once the interrupt is sent leaves the turn the `interrupt_requested` row decides.
- An interrupt goes with the turn it was asked for. In the process that asked for it, that is the turn in flight. For a process
  that resumes the run it is the turn the rows bound to the step and attempt of the `interrupt_requested` row (its `op`, the step
  published last before it): that turn, if the resumed call goes on with it, ends `held` for the row's reason whatever it came to. Every
  other turn starts with no intent: a new attempt after a lost one, a step the resumed process publishes again, a turn the host
  started for a step after the interrupt (a `send_input` the owner had not taken). The intent is compared where the outcome is
  read; it is never moved onto another turn.
- Write isolation is recorded, never enforced (the host gives a subagent its parent's sandbox). With `SessionConfig.Trace` the run
  takes digests of P, of the owner's checkout and of the run's shared state when it binds and when it harvests, and writes a flow
  warning when they differ: `read_role_wrote` (a role that changes nothing), `executor_wrote_outside`, `shared_state_changed`.
- An executor's `HEAD` is compared with the commit its worktree is to be at (the node's last `crw.commit` after the allocation the
  worktree was made or restored for, else that allocation's start). A subagent that committed moved it: the commit is kept on `refs/crw/flow/<run>/stray/<node>/a<attempt>-<call>` (the
  executor calls of one allocation, a build and its fix rounds, each keep their own; the ref is created by a compare-and-swap
  against no ref, and moved only by a fast-forward to a descendant, the same call's later `HEAD`, with the commit it holds as the
  compare-and-swap's old value) and the
  completed row records `executor_head_moved` (`start`, `head`, `ref`); `crw.commit` commits the pending tree on `start` with the
  observed `HEAD` as the compare-and-swap's old value, and the proof takes the recorded stray `HEAD` whose tree is the pending tree
  as the call's state ([journal.md](journal.md#the-proofs)). The subagent is closed when the call has its terminal row.

### Attempts, restarts and continuations

- An attempt is lost only when its marker turn is not completed, its thread is `notLoaded`, `systemError` or unknown to the host,
  and the turn ended interrupted or failed without an error, or has not changed for `SessionConfig.LostAfter` (ten minutes); the
  thread is read once more before. A thread the host cannot be asked about is not judged, and an active one is waited for.
  `session_attempt_lost` (`attempt`, `agent_id`, `k`, `turn_id`, `reason`, `host_status`, `turn_status`) is written, the old
  subagent is closed, and the next attempt spawns in the same allocation; its packet says the previous attempt was cut off, and
  an executor's says to read `git -C <P> status` and the diff first.
- One allocation serves every attempt of a call: the App Server reconcile, the live resend and the retirement of a worktree are not
  used for session calls. A resumed run goes on with an open session call in place, from its last row:

  | Last row | The resumed call |
  | --- | --- |
  | `prepared` | publishes the first step (the directory an earlier process made is the call's) |
  | `step_published` | answers first a dispatch report the owner wrote that no process answered (it waits in the inbox), then looks the step up on the host - the subagent that carries the marker for a spawn, the marker turn on the subagent for a `send_input` (a repair is never sent twice) - and binds what it finds; else publishes the same step again |
  | `thread_started` | adopts the subagent the row names and writes only its `turn_started` (nothing is spawned or published again; one the host does not know, or shows `notLoaded` or `systemError`, for `LostAfter` loses the attempt, and the gate decides the next; a read the host cannot answer judges nothing and leaves that time as it is, as in the harvest), then reads it as after `turn_started` |
  | `turn_started` | retracts the step file the dead process left and reads the bound subagent: harvests a completed turn, waits for a running one, loses a lost one |
  | `session_attempt_lost` | closes the old subagent and spawns the next attempt |

- A held session call goes on in place after the subagents of its held attempts are shown not running (a running turn is
  interrupted and the thread read until it is not active): a delivered turn is taken again by a new attempt whose packet says why
  the previous one stopped (a call held in its repair turn goes on with that repair: the packet names the task once, the answer
  repaired and the repair instruction, and the call has no second repair, as on the App Server path); a step that was published and not bound is looked up by its marker first.
- A new attempt of a node supersedes the open session calls of the previous process only after every subagent they were bound to,
  or that took a step they published, runs nothing.
- A continuation (`resume: handle`) runs on a new subagent and writes its own `thread_started` row, so the handle chain and the pair
  the relay reads name it. Inside a resume's suffix it never forks: it takes the checkpoint branch (`checkpoint_started`), and its
  packet carries the journal's record of the handle's turns, each with the input of the step or request it took: for a session
  subagent the conversation it goes on from, the turns of the calls its handle continues before its own and the call's conversation
  before the turn its subagent started at. A session call's conversation is its turns at the conversation turn each records
  (`conversation_turn` of its `turn_started` row), each conversation turn once: a turn run again (the next attempt of a lost turn;
  the turn a held call goes on with in place on a new subagent, at the next k) is recorded at the conversation turn of the turn it
  replaces and names that turn (`replaces_turn`), and supersedes it, and the packet is built from that list alone. The place is
  recorded when the step is published and the turn bound, never read back from held rows or k; a `turn_started` row of a journal
  written before the place was recorded (no `conversation_turn`) takes it from the `repair` member of the `step_published` row at
  its k (the task turn 1, the repair 2), and a later such turn at that place replaces the earlier one. The continuation's input digest keeps the handle's own thread chain: a position replays only after every one before it.

| Row | In a session call |
| --- | --- |
| `prepared` | as for any call, with `backend: session` and the allocation every attempt shares |
| `step_published` | the intent of a step, committed before the step file and the packet; `conversation_turn` and `replaces_turn` record the turn's place in the call's conversation |
| `thread_started` | a subagent bound to an attempt (one row per attempt and per continuation) |
| `turn_started` | the turn of a step the subagent took, with the place its step records (`conversation_turn`, `replaces_turn`) |
| `interrupt_requested` | the engine's interrupt of the turn in flight (`reason`, `op`); a resumed process applies it only to the turn bound to that step and attempt |
| `session_attempt_lost` | an attempt whose subagent will not finish its turn |
| `evidence_placed` | the intent to write a step's flow evidence record under a directory no row of the call's newest generation (its rows from the newest `prepared` on) names yet: `evidence_assignment`, `evidence_cwd`, `attempt`, `marker`; committed before the record is written, so the release of a superseded or prepared-again call and the restore of a resumed one find every copy from the rows; it changes no reading of the call's state |
| terminal rows | as for any call |
| `session_wake` (flow row) | a wake of the owner thread the run staged: `batch`, `wake_id`, `renotify`, `ops`, `owner`, `owner_status`, `waiting_seconds`, `staged` (and `error`, or `reason` when the relay already held the message and staged nothing) |
| `session_host_warning` (flow row) | what the session host adapter's marker search could not tie as it should: `op`, `warnings` (`no_ledger_line`, `ledger_op_differs`, `several_spawns`, `window_ended`) |
| `role_store_changed` (flow row) | written by no command: the run's process is the one party that records the pairs a run runs on and their change (its started row, `policy_repinned`); the journal of an older run may hold one (`pinned_digest`, `store_digest`) |
| `escalation` (flow row) | `reason: session_unresponsive`: the steps waited `UnresponsiveAfter` without a dispatched report (`owner`, `ops`, `batch`, `waiting_seconds`); the run goes on |
| `step_unwritten` (flow row) | a step whose packet or file could not be written: nothing was sent (`input_state: not_delivered`, `marker`, `error`, `failures`, `first_at`, `last_at`, and `written` on the closing row); the run writes it again |

### The owner's commands

A session run is started, stepped and resumed from its owner thread with the commands of `internal/flow/cli`; every one reads the
run's directory `<state>/flow/<run>/` and none writes the journal, which only the run process writes.

- `crw flow start WORKFLOW [--backend session|app-server]` starts a session run by default; `--backend app-server` opens a thread
  per call, for the tests and shadow runs. The spec records the `backend`. A session run's pool is `min(pool, agents.max_threads - 1)`
  (the owner keeps a slot for a helper of its own; `agents.max_threads` is read from `<CODEX_HOME>/config.toml`, default 6), the
  preview lists the spawn format of each of the seven roles (`spawn_agent(agent_type, message, model, reasoning_effort,
  fork_context=false)`) and the six session facts below, and the answer names the command the steps are taken with
  (`next`). Before the process starts the owner thread is bound (below).
- `crw flow next RUN [--wait S] [--json]` answers the run's `state` (`running`, `waiting_session`, `waiting_decision`, `paused`,
  `ended` with the `ended` status and state, `starting` below), its `steps`, the `slots` (`limit`: `agents.max_threads`, `open`: the subagents of
  calls not ended and of close steps not taken, `agents`: their ids), the `spawn_formats` and a `hint`; with `--wait` it waits up to S seconds for a
  step or a change of state. Each step carries the step file's members (call, k, attempt, op, role, action, agent_type, model,
  reasoning_effort, message, packet, workspace, agent_id) and the host call to make with the live argument names: a spawn step is
  `spawn_agent` and then `crw flow dispatched`, a `send_input` step is `send_input(target, message)` and then `crw flow
  dispatched`, a close step is `close_agent(target)`; the hint says to alternate `wait_agent(targets, timeout_ms)` with `next
  --wait` (its `targets` are the open subagents' ids), which subagents to close first and when the slots are full (`agent thread limit reached`; the step stays). A close
  step's file is removed once next has handed it out. A run no process holds that is neither paused nor over is resumed as
  `crw flow resume RUN --detach` does, and the hint says so (`resumed_pid`). next writes neither the owner's orchestrator line nor a
  row of the run (CRW-1372): while a process holds the run, that process is the one party that records the pairs it runs on (the
  `role_pairs` and `policy_digest` of its started row, which names its process lock as `process`) and writes the owner's line from
  them right after that row (below), and next reads both. The spawn formats are the pairs of the holder's started row: the newest
  started row when the process lock it names is held, or a newest row that names no process (one of a process of a crw before the
  process lock, which bound before it recorded its row) when no process lock of the run is held. A run whose holder has not recorded
  its started row yet (the newest row names a process lock that was let go, or names none while a process lock of the run is held:
  the holder is coming up, or a command that writes a row holds the run lock for a moment: an answer holds it from the end of the
  process it drained until its row is stored, and the resume it starts takes the lock after it), or whose start holds its
  start lock, is `starting`: next reads it again, up to 15 seconds whatever `--wait` says, never takes an earlier process's pins for
  the holder's and resumes nothing; still starting then, it answers `starting` with no spawn formats and a hint to ask again (at once
  without `--wait`, else when the state changes or `--wait` runs out). A holder that took its process lock less than 15 seconds ago,
  recorded its started row and has not bound the owner's line under that row's digest yet is read again the same way, and its line
  is then reported as it is. A run no process holds answers the role store's spawn formats
  (a resume reads the store). next warns, and records nothing, when the owner's last line is not the holder's (another live run's,
  another digest's, or none: the hook gives the owner's spawns no pair of the run until the run's process, or its next process when
  it is resumed, binds it), when the role store reads other pairs than the holder pinned (the process runs on its pins; a resume
  takes the new ones and records them in its started row and the change in a `policy_repinned` row) and, for a run next resumed,
  when that process repinned (its `policy_repinned` row: it runs on the role store as it reads now, not on the pairs the earlier
  process pinned). A run that recorded no pairs answers the store's, as before.
- `crw flow dispatched RUN CALL --agent ID --k K --attempt N [--wait S] [--json]` writes the dispatch report into the inbox (a
  temporary name and a rename), waits up to S seconds (120) for the run's reply and prints it (`bound` exits 0, the other replies
  exit 1). A report of a run no process holds, or one the run has not answered in time, is answered `queued`: it stays in the inbox
  and the run answers it when it reaches the step (a resumed run included).
- A resume (`crw flow resume RUN`, the process `next` starts, and the engine's `crw flow resume RUN SCRIPT` through
  `journal.ResumeEnv`) goes on with the backend the run's started row records (`engine.StartedBackend`): a session run gets its
  session environment again (the session host adapter of `internal/flow/sessionhost`, the run's `FileStepBus`, the owner of its
  record). A spec that names another backend than the started row is a mixed resume, refused with the reason as an input error
  (exit 2); a run whose first process stored no started row goes on with its spec's. Every one of them holds a session run alike
  (the foreground command through `engine.ResumeHold`): at most `agents.max_threads - 1` calls in flight whatever `--pool` says,
  and the owner thread bound to the run while the resume runs and its line closed when the run ends.
- The owner thread is bound to a session run by the `orchestrator` line of its flow-bound file (`internal/flow/bound`
  `BindOrchestrator`: `kind: orchestrator`, `run_id`, the `policyDigest` of the role table and the `helperPairs` of the seven roles
  architect, executor, executor-t1, reviewer, reviewer-final, explorer, critic; `Line.Validate` takes no orchestrator line without
  the first six; the critic is optional, so the open line of a run an earlier crw bound stays readable and its critic spawns are
  undecided).
  A pair carries the decided parts of its role only: an undecided role has the empty pair, a role that names a model and leaves its
  effort undecided has the empty effort, and an effort without a model is no role's pair.
  The start writes it before any process of the run exists. While a process holds the run, that process is the one party that writes
  it (CRW-1372): right after its started row is in the journal it writes the line again with the table that row pinned when it is
  not the last line (`engine.SessionConfig.Started`; the foreground `crw flow resume RUN SCRIPT` through `engine.ResumeHold`), and it
  closes it when the run ends (`CloseOrchestrator`). `crw flow next` only reads it, so a line that is missing while a process holds
  the run (its bind failed, or a process of an older crw left it) is reported and stays missing until the run's next process, a
  resume, binds it after its own started row. A session run's process holds its process lock, `<state>/flow/<run>/process.<token>.lock`,
  from before it takes the run lock until after it lets the run lock go, and its started row names the token (`process`); it
  removes the file when it ends (the file of a process that died stays, held by nobody). A process lock that cannot be taken is
  warned about and the process runs with a started row that names no process. A live run is one whose process holds its run lock, or whose start holds its
  start lock (`<state>/flow/<run>/start.lock`, held from before the start binds until the run's process holds the run lock, since the
  start binds before its process exists): the open line of a run with neither is closed before the new one is written, and a thread
  holds the line of one live run, so a second start while the first lives, or is still starting, warns and does not bind. The read
  of the thread's last line and the append are one step under a lock of the flow-bound directory, so two near-simultaneous starts of
  one thread cannot both bind, and neither can their two run processes. A thread whose binding cannot be written is warned about; the run starts.
- The spawn hook decides the owner thread's spawns by that line. It refuses two: a spawn by a subagent (a leaf) and a full-history
  fork (`full_history_fork_refused`: the fork keeps the parent's pair). Every other spawn is allowed with `model` and
  `reasoning_effort` overwritten from the decided parts of the pair of its role (an undecided effort keeps the `reasoning_effort` the
  spawn was sent, and an undecided role keeps the whole spawn, with the warning `role_undecided`): the role the first `CRW-ROLE:` line of the message header names when it
  is exactly one of the seven (a parser of the flow-bound path; the ordinary path's oracle parser is unchanged), else the role of
  the `agent_type` (worker is executor), else explorer. The ledger line (`decisions/<owner>.jsonl`) records the `role`, the `op` of
  the `crw-flow-op:` first line, and a `warning` where the table has one: `role_agent_type_mismatch` (the header role does not pair
  with the agent_type: executor-t1 with executor, reviewer-final and critic with reviewer, the others with their own name), `unrecorded_spawn`
  (no step marker) or `unrecorded_author` (no marker, an executor's or architect's type). A ledger that cannot be written allows the
  spawn and says so on stderr, and a flow-bound directory or file that cannot be trusted is read as no binding, with a stderr
  warning: no spawn is refused for it. The run joins the ledger's `op` with the owner turn's `collabAgentToolCall` items to find the
  subagent of a published step after a restart ([session-host.md](session-host.md)).
- The run wakes its owner. When it publishes a step and the owner thread is idle or notLoaded (the host's `thread/read`), it stages
  a `session_step_waiting` message through `run:<id>` (`journal.StageWake`), which the supervisor channel delivers to the run's
  owner (`flow_runs.actor_task_id`): a new turn on an idle thread, a resume and a turn on a notLoaded one, after the turn on a busy
  one. The logical id is `(run, process, batch)`, the batch being the set of waiting steps, so a wake of the same steps staged again
  is one message: the run's n-th process (its n-th started row) wakes under `<batch>/e<n>` (the first under `<batch>`), because the
  relay keeps a message id for good while a process counts its re-notifications from 0 again, so a resume stages new messages. A
  wake the relay already held stages nothing, and its row says `staged: false` with the relay's `reason`. The message's delivery is
  a `message_delivery` row the daemon's settle writes ([reporting.md](reporting.md#through-the-supervisor-channel)). While steps wait without a dispatched report the run notifies again after `WakeAfter` (15 minutes), each interval twice
  the one before up to `RenotifyMax` (an hour), whatever the owner's status; after `UnresponsiveAfter` (2 hours) it writes the
  `session_unresponsive` escalation once and goes on waiting. Each wake is a `session_wake` row. A session run's started row pins
  its role pairs (`role_pairs`), and the session host adapter's warnings are `session_host_warning` rows.
- With `SessionConfig.Trace` (the session backend's `Trace`) the digests are: the workspace, P's `git status --porcelain` and
  `HEAD`; outside, the owner's working directory's when it is a git checkout other than P; shared, the refs under `refs/crw/` of the
  run's repository other than the run's own `refs/crw/flow/<run>/` (the run moves those itself) and the binding files of the
  flow-bound directory (the hook's ledger and claims are left out: the owner's spawns write them while a subagent works).
- The session facts of the preview are warnings, never a refusal (CRW-1318): `owner_bound` (the owner can be bound: a thread id, a
  readable binding, no other live run, live as the binding reads it: its process holds the run lock or its start holds the start lock), `role_models` (the seven pairs, a role without one or without an effort, a reviewer of an author's model family, a critic of the architect's),
  `owner_state_writable` (the run directory, the flow-bound directory, the control socket and the repository), `subagent_slots`
  (`agents.max_threads` leaves a slot for a call) and `multi_agent_v1` (`features.multi_agent_v2` is off: under V2 the protocol
  stops); `verifier_prerequisites`, the sixth, is judged for every run (CRW-1319).

## The App Server backend

`crw flow start` runs a run as session calls ([Session calls](#session-calls)) by default; this backend, a Codex thread of
its own for each call, is the backend of tests and shadow runs and of a run started with `--backend app-server`.
`internal/flow/backend/agent` runs a call as a persistent Codex 0.154 thread. A thread is never ephemeral: Codex refuses
`includeTurns` and `thread/turns/list` for an ephemeral thread, which would take away pull recovery and the audit of a run.

- `thread/start` carries the call's own `cwd`, the role's model, `config.model_reasoning_effort`, the role's sandbox (workspace-write
  for `executor` and `executor-t1`, read-only for every other role), `approvalPolicy: never`, `ephemeral: false` and the role
  prompt as `developerInstructions`. Nothing that names the call is written there: the 0.154 `Thread` has no such member, so
  it could never be read back. A pair's `autoCompactTokenLimit` travels in the same `config`.
- The pair the thread is configured with is read with `thread/read` (`includeTurns: false`): `Thread.model` and
  `Thread.reasoningEffort`. A different or null value ends the call `refused` with `pair_refused`, and the thread, with the pair it
  read back, stays in the journal. Every `turn/start` repeats the thread's model and effort, so nothing changes a thread's pair.
- A turn's operation is `<call_id>/t<k>`. It is sent as `clientUserMessageId` and as the first input line `crw-flow-op: <op>`; a
  turn read back from the host is identified by its `userMessage` item's `clientId` first and by that first line second. The
  call's schema is the turn's `outputSchema`.
- A turn's end comes over the notification bus. A completed turn's answer is its last `final_answer` message on the bus; from a
  provider that sets no message phase (0.154 `MessagePhase` is nullable) it is a last message without a phase when no
  `commentary` message came before it. A completed turn with neither is read in full, and the host's items decide its answer.
  When no end comes, or the feed may have lost some, the backend reads the turn it
  recorded: it pages `thread/turns/list` with `itemsView: full` up to that turn id and reads a turn the list gave only as a
  summary with `thread/items/list`. It never hydrates with `includeTurns`. A turn whose items cannot be read in full is
  unreadable, not empty: the call stays with the reconcile pass.
- An executor turn never carries a `cwd`: a turn `cwd` moves the thread's working directory for that turn and every later one, so
  an executor's directory is fixed by `thread/start` or `thread/fork` alone. An architect turn carries its call's directory.
- If a run pauses, the turns in flight are interrupted with `turn/interrupt`; the same request is sent for a cancelled run.
- The backend is the host side of reconcile (`engine.ReconHost`): it lists threads by cwd and `appServer` source to the end of the
  cursor, reads turns in full, archives an orphan, and says what is known about a creation request
  (`CreationState`). For every `thread/start` and `thread/fork` it records the connection the frame went out on before it is
  written (`Client.ConnectionID`) and names its own process on the intent (`Config.Owner`; by default the pid and the kernel
  start time). A request of this process is closed when that connection is no longer the client's; a request of another process
  is closed when the process is gone (`Config.OwnerAlive`; a process it cannot prove gone is alive).

### Working directories

One allocator makes every directory a call runs in. An executor's is an attempt worktree
`<state>/flow/<run>/wt/<node>/a<attempt>-l<lineage>`, where the attempt is 1 plus the allocations of the node already journaled.
There are two ways to get one, and the call's `prepared` row records which (`allocation`: `kind`, `attempt`, `lineage`, `path`,
`start_commit`):

- `fresh`: a call that creates a thread and adopts no restore - the first build, the rebuild after a `null_model` and the
  `executor-t1` build - gets a new worktree made with `git worktree add --detach` at C, the result commit of the node's last
  `crw.commit` row (else the base recorded for its first attempt). The `prepared` row is written first, the worktree is made
  after it, and `thread_requested` follows. The node's previous worktree is retired: its HEAD and working snapshot are committed
  to `refs/crw/flow/<run>/retired/<node>/a<n>`, and the edits of a `null_model` attempt stay there.
- `restore`: when a resume suffix has restored the node's worktree (a flow row `worktree_restored` with the node, lineage, attempt,
  path, `C`, `P` and the verified `snapshot_digest`), the node's first executor call adopts it as its cwd - as a fresh thread, as
  an `executor-t1` thread, or, for a continuation, by forking its handle's thread (`thread/fork` with `lastTurnId` and the
  restored `cwd`, rows `fork_requested` and `fork_started`). Nothing is made, checked out or reset. The live snapshot is compared
  with the digest before `thread_requested` (or `fork_requested`) and again before the call's first `turn_requested`; a
  difference sends nothing more, leaves the call's last row where it was, writes one `escalation` with reason
  `restore_snapshot_mismatch` and ends the call `unknown`. The node is stopped for the rest of the run: every later call of
  the node (a fresh executor call, a continuation of its handle) ends `held` with the same reason, makes no worktree and
  sends nothing; the next resume restores the boundary in a new lineage.

The `cwd` and `worktree` options of a call are journaled in `prepared` but do not choose its directory: no two calls share one, so
the allocator does. An architect turn that continues its thread and names a `head` gets a call-private detached worktree at that head
(`calls/<call_id>/wt`, a later allocation of the same call `wt-1`, ...), named as the turn's cwd; the call's terminal row names it (`own_directory`) and the backend removes it after the row with a context of its own, and a resume removes every such directory an earlier process left (journal.md); inside a resume's suffix such a turn runs on a
fork of the previous process's thread in the same kind of directory, which is its own as well (made again at its recorded head when a held fork
goes on, removed after its terminal row, kept while its outcome is unknown). A continuation that runs in no restored worktree makes nothing and keeps the directory its thread started in. Every other role
runs in `<state>/flow/<run>/calls/<call_id>/wt`, a detached worktree at the `head` option (else the node's base); a reviewer
reads it through a read-only thread. The run commits and reads the head from git (`Allocator.Commit`, `Allocator.Head`); a model
never does. Every git command of the allocator runs in the directory it is given: the variables that route git to a
repository, a work tree or an index (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE` and the rest of `git rev-parse
--local-env-vars`) are removed from its environment. The working snapshot (`Allocator.Snapshot`) is the tree id of every file the
worktree holds that git does not ignore - a tracked file is always held, whatever an ignore rule says - computed in a throwaway
index that starts as a copy of the worktree's own. The copy's assume-unchanged flags are cleared, and so are its skip-worktree
flags on files the worktree holds, so a flag never hides a file's working content; a skip-worktree entry whose file is absent (a
sparse checkout) is held with its index content. The worktree's own index keeps its flags.

## Dependencies and their supply-chain check

The loader and runtime add two direct dependencies and promote one. Each was checked as the slopsquatting gate
(`plugins/crw/skills/crw-dev-security`, section 6.5) asks: the module exists on the Go module proxy with a real release
history, its maintainer and source repository are the plausible ones, a Go module runs no install script (`go build` runs
only the compiler on the imported packages; the module's `go:generate` lines never run), and the `go.mod` and `go.sum`
diff was read line by line.

| Module | Pinned to | Why this pin | License | Check |
| --- | --- | --- | --- | --- |
| `github.com/dop251/goja` | `v0.0.0-20260917113740-793a2a65c13b` (commit `793a2a65c13b`, 2026-09-17) | The module publishes no tags, so the pin is a commit: the maintainer's merge of pull request 739 (2026-09-17), more than three weeks old at the time of the change. Two later commits older than two weeks (`for await` support and a `PromiseResolve` fix for promise subclasses) are not relied on. | MIT (Dmitry Panov, from Robert Krimen's otto) | History since 2016-11-04 (623 commits), maintainer `dop251`, source `github.com/dop251/goja`. |
| `github.com/evanw/esbuild` | `v0.28.2` (2026-08-08) | The newest tagged release older than two weeks. Only `pkg/api` is imported, so the build uses esbuild as a pure Go library. | MIT (Evan Wallace) | 438 tagged releases since v0.8.0 (2020-10-28), maintainer `evanw`, source `github.com/evanw/esbuild`. |
| `github.com/google/jsonschema-go` | `v0.4.3` (already in `go.mod`) | Moved from indirect to a direct dependency: the engine validates every call result with it. | MIT | Unchanged pin. |

goja brings four indirect modules: `github.com/dlclark/regexp2/v2` v2.5.2 (MIT), `github.com/go-sourcemap/sourcemap` v2.1.3
(BSD-2-Clause), `github.com/google/pprof` (Apache-2.0, a test-time dependency of goja) and `golang.org/x/text` v0.3.8
(BSD-3-Clause). Both direct modules build with `CGO_ENABLED=0`, so the release matrix needs no change.
