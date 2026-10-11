# CRW workflow scripts

A CRW workflow script is a JavaScript file that `crw flow run` executes. It orchestrates agent calls with ordinary
control flow (loops, conditionals, `await`) and the engine records every call it makes. This page is the reference for
the script format, the loader, the globals, call identity, the call result and the limits. It describes what
`internal/flow/script` (loader and runtime) and `internal/flow/engine` (calls, journal, paths) do.

```text
crw flow run --backend fake [--args JSON | --args-file FILE] [--pool N] [--token-budget N] [--journal] <script.js>
crw flow run --script JS [--plan-only=false --backend fake] [--args JSON] [--journal]
crw flow roles [--json]
crw flow lint [--args JSON | --args-file FILE] [--no-dry-run] [--json] <script.js>...
```

`--backend fake` runs the script against the in-process fake backend: every turn answers immediately with a value that
satisfies the call's schema, including its numeric bounds, `multipleOf` and `pattern` (or a short sentence for a call without one). It is the only backend of this build; the App
Server backend arrives with its own issue. The command prints the run's result as JSON and exits 0 when the script
completed, 1 when the script failed (`status: script_error`) or did not load, and 2 for a command line it cannot use.
The journal sequence numbers of a call (`meta.startSeq`, `meta.endSeq`) depend on the order the host finished concurrent
calls, so under `crw flow run` neither the `CallResult` the script holds nor the printed result carries them (a value,
a log line or an error the script builds from a `CallResult` cannot hold them either) and two runs of one script print the
same bytes; `--journal` adds the journal rows, with those numbers. `testdata/flow/hello.js` is a small script that runs this way.

`--script JS` runs an ad-hoc script given as text instead of a file. Its default is `--plan-only`: the script runs on
the fake backend only, with no backend named, and the result says `"planOnly": true`. A script that is to run on a
backend names it and turns plan-only off (`--plan-only=false --backend fake`); `--plan-only` on a script file is a usage
error. An ad-hoc script has no workflows directory: it imports `crw:lib/<name>` only and nests no workflow.

`crw flow run` reads the execution policy file `CODEX_THREAD_BRIDGE_EXECUTION_POLICY` names once, when it starts, and
resolves every call against its `agent-roles` and `agent-aliases` sections (see
[role-execution-policy.md](../role-execution-policy.md#the-agent-roles-of-a-flow-run)). The run record keeps the file's
path and digest. Without the variable the fake backend's placeholder table applies; a file that does not load, or that
declares no `agent-roles`, is a usage error and never replaced by the placeholders. `crw flow roles` prints the table,
the source and the digest.

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
| `budget` | `{total, spent(), remaining()}`: the run's token ceiling (`total` is `null` without one), the tokens spent and the tokens left (`Infinity` without a ceiling). At 80% the engine journals an info row; at 100% the run is paused and a call that would send a request ends `held` with reason `budget_exhausted`. |
| `workflow(nameOrRef, args)` | Runs another script inline as one step and resolves with what it returns. The child shares the run's pool, journal and budget, and its calls live on a child path. Nesting is one level: `workflow()` inside a child throws. `crw flow run` resolves a name to `<name>.js` and `{scriptPath}` to that relative path, inside the script's directory. |
| `crw.classify(result)` | Maps an executor `CallResult` to one outcome state (below). |
| `crw.clock()` | The time as an RFC 3339 string, journaled. It occupies a position of its path like a call. |
| `crw.tierStep(node, evidence)` | Journals a step decision and resolves with `{id, checkpoint}`; a fresh `executor-t1` call carries `id` as `stepDecision`. It occupies a position of its path. In this build it holds the interface only: the evidence rules, the append-only `flow_step_decisions` row and the checkpoint worktree arrive with the issue that owns the operation, so `checkpoint` is `null` and the evidence is recorded by its digest. |

`opts` of a call: `agentType` (`architect`, `executor`, `executor-t1`, `reviewer`, `reviewer-final`, `explorer`), `schema`,
`label`, `phase`, `node`, `worktree` or `cwd`, `head`, `resume`, `stepDecision`, and the Claude-style `model`, `effort` and
`isolation`. The remaining `crw.*` operations (`dag`, `milestone`, the packet builders, `commit`, `verify`, `review`, `land`
and the rest) arrive with the issues that own them.

`parallel` and `pipeline` accept `{keys: [...]}` (one explicit key per item, in place of the input index) and
`{label}` (the label the join carries in a concurrency error).

### Determinism

`Date.now()`, `Math.random()`, `new Date()` and `Date()` throw a `DeterminismError`. A `Date` built from arguments
(`new Date(0)`, `Date.UTC`, `Date.parse`) works. Time comes from `args` or `crw.clock()`.

## Roles, pairs and aliases

A fresh call names a role with `agentType`. A role has a fixed model and effort (its pair), a role family (`executor` and
`executor-t1` form one, `reviewer` and `reviewer-final` form one, `architect` and `explorer` are alone) and a sandbox:
`workspace-write` for `executor` and `executor-t1`, the only roles that write (to their call's own cwd), `read-only` for
the others. The role table is the operator's execution policy file, loaded once per run; the fake backend runs against
placeholder pairs and the same alias table.

- With a role key as `agentType`, a `model` option must be absent, the role's model or an alias of its model family
  (`haiku`: `executor` and `explorer`; `sonnet`: `executor-t1`; `opus`: `architect`; `ocx-gpt-6-1-sol`: `reviewer` and
  `reviewer-final`); otherwise the call is `refused` with reason `pair_refused`.
- A call without a role key is looked up in the alias table by (a non-role `agentType` string, else the `model` alias;
  `effort`; `isolation`), the most specific matching row deciding: `opus` is `architect`; `haiku` with
  `isolation: 'worktree'` is `executor`; `sonnet` is `executor-t1`; `ocx-gpt-6-1-sol` (as `model` or as `agentType`)
  with `effort: 'xhigh'` is `reviewer-final`, otherwise `reviewer`.
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
| `reason` | Closed set, `null` when completed: `schema_invalid_after_repair`, `context_window_exceeded`, `host_infra_<kind>`, `host_config_<kind>`, `host_policy_<kind>`, `host_interrupted_external`, `engine_paused`, `budget_exhausted`, `pair_refused`, `policy_refused`, `independence_refused`, `missing_step_decision`, `schema_not_strict`, `foreign_handle`, `dispatch_unknown`, `superseded_by_attempt`, `superseded_by_resume`, `cancelled`. |
| `meta` | `role`, `requestedPair`, `configuredPair`, `threadId`, `turnIds`, `repairUsed`, `tokens`, `startSeq`, `endSeq` (the last two are absent under `crw flow run`, see above). |

A failed or interrupted turn, and an RPC error to the thread or turn request, is classified by its `codexErrorInfo`: the
quota, rate, overload, connection, stream, rollback, steering and `other` kinds are `held` (`host_infra_<kind>`; a usage or
rate limit also pauses the run), `sessionBudgetExceeded` is `held` with `budget_exhausted`, `badRequest`, `unauthorized` and
`sandboxError` are `refused` (`host_config_<kind>`), `cyberPolicy` and `misalignmentPolicyViolation` are `refused`
(`host_policy_<kind>`), `contextWindowExceeded` is `null_model`, and a failure with no `codexErrorInfo` or one the table does
not list is `held` with `host_infra_unknown`. An interrupt the engine did not send is `held` with `host_interrupted_external`.
A request that failed without an answer (a lost connection, a timeout) ends `unknown` with `dispatch_unknown`. Only
`null_model` can be evidence for a tier step.

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

## crw flow lint

`crw flow lint` checks scripts for what the engine cannot refuse early. It reads the text of each script (the body parsed
with its scopes resolved) and runs it on the fake backend, reading the journal; a finding both find is reported once. It
exits 0 when every script is clean, 1 when one has a finding or does not load, 2 for a command line it cannot use.
`--no-dry-run` checks the text only; `--args` gives the dry run its `args`. A dry run that ends in an error of the
script's own (it reads an arg it was not given, it calls a `crw.*` operation this build does not have) is a note in the
report, not a finding.

| Rule | What it finds |
| --- | --- |
| `lifecycle_land_twice`, `lifecycle_land_without_review`, `lifecycle_order` | A lifecycle automaton per executor node and per function that calls `crw.commit`, `crw.verify`, `crw.review` or `crw.land`: build once (a rebuild is allowed until the node has landed), then rounds of commit, verify, review, then land at most once, from a completed review. A call starts its event where it runs and completes where its promise is used: awaited, returned, passed on or kept by the expression around it (the right side of `&&`, `||` or `??`, the left side of `||` or `??`, the last part of a sequence, a branch of `?:`, the value of an assignment), or read (passed on, closed over, `{ p }`) through a name that holds it. A promise whose value is discarded never completes: a call used as a statement, under `void` or another unary operator, in a sequence but its last part, on the left of `&&`, in a test, a comparison, arithmetic or an untagged template, and a held name read only in such a place; a stored promise that nothing reads leaves the node where it was, so `crw.review(...)` without `await` is not a review a later `crw.land` can follow. Started is enough for two rules: a second `crw.land` is a land twice whether or not the first was awaited, and a `crw.commit` started after a review leaves the node built (a change no round has committed), so the land needs a new round. A `break` or `continue` with a label goes to the statement the label names. A node is named by the `node` option of its executor call and by the first argument of the `crw.*` calls, so the phase of one node never stands for another's; a node the function does not build is taken as built. A violation is reported when it is illegal on every path that reaches it; the finally block of a try is checked from the state at the end of the body or handler and from the state of each completion that leaves through it (a throw no handler takes, a return, a break or continue to a loop outside the try), and that completion then goes on where it was going with the state the block leaves, unless the block returns, throws, breaks or continues itself. A handler starts from the state before the body or after any call in it, and an exception it takes no longer reaches the finally block. |
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

The journal is in memory in this build; the store persistence arrives with the flow journal tables. Each call journals the
rows `prepared` (written before anything is sent, with the role, pair, node, `stepDecision` and, for a continuation, the
thread it continues and what it inherited), `thread_requested`, `thread_started`, `turn_requested`, `turn_started` and one
terminal row (`completed`, `null_model`, `held`, `refused`, `outcome_unknown` or `superseded`). A held row records whether the
call's packet reached the host (`input_state`: `delivered` or `not_delivered`) and the `codexErrorInfo` verbatim. The run record
holds the workflow name, the script digest, the helper digests, the digest of `args`, the meta, and the path and digest of
the execution policy file.

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
