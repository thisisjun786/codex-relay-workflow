# CRW workflow scripts

A CRW workflow script is a JavaScript file that `crw flow run` executes. It orchestrates agent calls with ordinary
control flow (loops, conditionals, `await`) and the engine records every call it makes. This page is the reference for
the script format, the loader, the globals, call identity, the call result and the limits. It describes what
`internal/flow/script` (loader and runtime) and `internal/flow/engine` (calls, journal, paths) do.

```text
crw flow run --backend fake [--args JSON | --args-file FILE] [--pool N] [--token-budget N] [--journal] <script.js>
crw flow run --script JS [--plan-only=false --backend fake] [--args JSON] [--journal]
crw flow roles [--json]
crw flow recover [--run RUN] CALL (--adopt THREAD|TURN | --absent)
crw flow resume [--state DIR] [--socket PATH] [--repo DIR] [--args JSON | --args-file FILE] RUN SCRIPT
crw flow lint [--args JSON | --args-file FILE] [--no-dry-run] [--json] <script.js>...
```

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
| `budget` | `{total, spent(), remaining()}`: the run's token ceiling (`total` is `null` without one), the tokens spent and the tokens left (`Infinity` without a ceiling). At 80% the engine journals an info row; at 100% the run is paused and a call that would send a request ends `held` with reason `budget_exhausted`. A completed turn whose usage the backend never received counts as unmeasured, not as zero: the engine journals a flow row `tokens_unmeasured`, and under a ceiling pauses the run the same way. |
| `workflow(nameOrRef, args)` | Runs another script inline as one step and resolves with what it returns. The child shares the run's pool, journal and budget, and its calls live on a child path. Nesting is one level: `workflow()` inside a child throws. `crw flow run` resolves a name to `<name>.js` and `{scriptPath}` to that relative path, inside the script's directory. |
| `crw.classify(result)` | Maps an executor `CallResult` to one outcome state (below). |
| `crw.clock()` | The time as an RFC 3339 string, journaled. It occupies a position of its path like a call. |
| `crw.tierStep(node, evidence)` | Mints the step decision: it journals the call and resolves with `{id, checkpoint}`; a fresh `executor-t1` call carries `id` as `stepDecision`. It occupies a position of its path. The engine's own call holds the id only (its `checkpoint` is `null` and the evidence is recorded by its digest); the project lane's `crw.tierStep` ([below](#the-project-lane)) wraps it, appends the `flow_step_decisions` row with the evidence and the checkpoint, and resolves with the checkpoint filled in. |

`opts` of a call: `agentType` (`architect`, `executor`, `executor-t1`, `reviewer`, `reviewer-final`, `explorer`), `schema`,
`label`, `phase`, `node`, `worktree` or `cwd`, `head`, `resume`, `stepDecision`, and the Claude-style `model`, `effort` and
`isolation`. The other `crw.*` members are the operations and composites of [the project lane](#the-project-lane); each operation is an
`engine.Op` registered in `Config.Ops`, takes a position of its path like a call, and a resume replays it only while its effect still
holds ([journal.md](journal.md#ops)). The `crw.*` members a later issue owns (the final gate and reviewer-final of the milestone close,
the delivery of the report, the revisions of the design ledger and the re-plan signals) are not here.

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
scheduler offers them; the architect answers `DESIGN` once for the milestone (SP-ARCH-1); the open questions of the DESIGN go to
explorers (SP-EXPL-1); a reviewer of another model family than the architect audits the DESIGN (`DESIGN_AUDIT`, recorded by
`crw.designAudit`), and `needs_changes` returns the DESIGN to the architect on its own thread; only an approved DESIGN becomes revision
`r0` of the design ledger (`crw.designLedger`); `crw.dag` runs one lane for each node; when every lane landed, `crw.milestoneClose`
judges the integrated range (the final gate and reviewer-final are the seam of the issue that owns them) and `crw.reportUp`
records the report (its delivery is the seam of the issue that owns it). A milestone that cannot go on says why: a call of the design
phase that did not complete is never read as an approval or as a design (`held` and `unknown` hold the milestone, `refused` and
`null_model` escalate, `superseded` stops).

**The lane of a node.** `crw.dag` starts a node when the nodes it waits for (a dependency or an order edge; the members of a
group are chained by order edges) have landed, in the scheduler's order, at most `cap` nodes at once and never two nodes whose
edit regions overlap (an undeclared region overlaps every other); the regions of a node are those of the plan together with the
`edit_regions` of the approved DESIGN (`crw.admissionRegions`), because the scheduler's own gate reads the plan's and the executor
is scoped to the DESIGN's; there is no barrier. A node whose predecessor did not land is
`blocked`. Each node runs on a path of its own (its id), and its first position is `crw.release`, which asks the relay for the
slot and records the design revision and digest the node is released under. The lane builds its packet from that revision
(`crw.issuePlan` reads the journal and nothing the script holds), and every call of the node carries the revision, the node's slice
digest and its criteria digest in its input snapshot, so a ledger that later grows a revision changes nothing for a node that
started under `r0`.

| `crw.classify` state | The one transition |
| --- | --- |
| `ready` | commit, verify, review, land |
| `self_failed` (the executor reports `failed` or `budget_exhausted`) | the run commits and verifies the worktree anyway and reviews it when the verification passes; the report is no evidence |
| `research` | `crw.explore` answers the question (SP-EXPL-2: an answer of the run is reused, at most two research continuations on a tier), and the same executor thread goes on with the answer; never a tier step |
| `scope` | `crw.escalate` with `blocked_scope`: the relay closes the node's execution and takes its slot back (R07) |
| `null_model` | the first one: a fresh thread of the same tier in a new attempt worktree at the node's last commit; the second in a row is tier evidence |
| `host_held` | `crw.hold`: the node is held and the call goes on in place when `crw flow resume` runs after the cause is gone; never a new thread, never tier evidence |
| `null_unknown` | `crw.hold` with `dispatch_unknown` (a recovery hold): nothing is sent again |
| `refused` | `crw.escalate` as a configuration fault, then `crw.decide`: the lane goes on, with a fresh thread of the same tier, only when the run journal has the `decision_applied` row of that escalation and node |
| `superseded` | the lane stops |

**Review.** `crw.review(node, plan, head, ci, opts)` is a fresh reviewer thread over the committed head (SP-REV-1), with the record
of its answer (`crw.review` row: node, head, criteria digest, state). The state is `verdict` only for a completed, valid `VERDICT` of a
reviewer whose model family differs from every author of the node (the node's executors and the architect); a `verified` verdict
names no unmet criterion and no defect of P0 to P2, and a defect of kind `evidence_missing` carries its `evidence_question`. The
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
the finding), (b) the required verification failing on two consecutive committed heads, (c) `null_model` on two consecutive executor
calls of the tier, (d) four reviewed rounds without a verified verdict. `crw.tierStep` then mints the step decision and
`crw.stepRecord` appends the `flow_step_decisions` row (the rule, the letter of the evidence, the evidence ids, the head and tree of
the node's last commit as the checkpoint; the step id is the engine's, or that id with a digest of the decision when the stored row of
that id describes another checkpoint, which a suffix that runs again at the same position can reach); only then does a fresh `executor-t1` call start, in a new worktree at that commit,
carrying the id. The same evidence on `executor-t1` is SP-TIER-2: the node escalates as `tiers_exhausted`, the relay closes its
execution and returns its slot. A self-reported failure, `needs_research`, `blocked_scope`, a single `null_model`, `null_unknown` and
every held, refused or superseded result are never evidence.

**Land.** `crw.land` is `crw.accept` and then `crw.integrate`. `crw.accept` needs the passing verification record and the verified
review of the head in the journal, assembles the premerge-record/1 from the verdict (a code step: `ops.AssemblePremerge`), judges it
as the relay's acceptance will, and asks the relay for the agent acceptance (R05); every finding that needs a disposition is
`blocking` by default, and only the orchestrator separates or carries one, so such a head is `land_refused` (`premerge_blocked`) and
the lane escalates. `crw.integrate` lands the accepted head on the integration target. A refusal of either is an op error that carries
a closed reason and leaves a refusal row, not a completed row: a resume asks again, and the engine's accepted-node check reads only
completed `crw.accept` rows.

**Decisions.** `crw.spawnDecision(rule, decision, info)` appends a `flow_spawn_decisions` row for every decision to spawn, do the work
inline (SP-INL-1), reuse an answer (SP-INL-2), skip, refuse or hand a question to the orchestrator thread; the explorer routing
(`crw.exploreRoute`) writes its own rows. A script names a rule by its key (`EXEC_1`, `TIER_1`, ...) and the id comes from
`internal/flow/ops/rules.go`, the one place the `SP-*` strings are written.

**Operations and composites.** The operations (`internal/flow/ops`, one `engine.Op` each, rows `crw.<name>`):

| Operation | Records and returns | Replayed while |
| --- | --- | --- |
| `milestone(plan, milestone)` | the nodes of the milestone with edges, regions and slice digests | always (a read) |
| `designAudit(milestone, round, candidate, architect, call)` | the audit row: state, candidate digest, auditor and architect models | always |
| `designLedger(milestone, entry)` | revision `r0`: the DESIGN, the answers, the digest and the architect handle; refuses a DESIGN with no approved audit and any revision after `r0` | always |
| `release(plan, node, milestone)` | the release under the ledger revision (`revision`, `design_digest`, `execution_id`); a node sized for a child or a split is not released to an executor | the relay says the execution stands |
| `issuePlan(node)` | the node's entry of the DESIGN of its release revision | always |
| `commit(node, call, regions)` | the commit of the call's recorded `pending_tree` (`commit`, `parent`, `tree`, `out_of_scope`, the receipt's outcome and `plan_mismatch`) | the commit is on the history of the node's worktree |
| `verify(node, head)` | the verification (`ok`, `command`, `digest`, the record); a verification that could not run is `ran: false`, not a failure | always |
| `review(node, head, criteriaDigest, call, opts)` | the `ReviewResult` | always |
| `accept(node, head)` / `integrate(node, head, acceptanceId)` | the acceptance id / the batch commit and the target tip | the acceptance is active and not stale / the batch commit is on the target |
| `escalate(node, info)` / `hold(node, info)` | the escalation flow row and, for `tiers_exhausted`, `blocked_scope` and `size_split`, the relay's close of the execution; the hold (the relay is told, nothing is closed) | always |
| `releaseChild(node, plan)` / `decide(node, ask)` | the issue child that was started / whether a `decision_applied` row answers the ask | always / no applied row has appeared since |
| `spawnDecision`, `stepRecord`, `exploreRoute`, `exploreRecord` | the decision rows and the answers the reuse reads | always |
| `milestoneClose(milestone)` / `reportUp(milestone, close)` | the range and the tip the close judged / the report row | the integration target is still the tip the close judged / always |

The composites (`crw:lib/lane`, made members of `crw` by `install(crw)`; they take no position of their own): `crw.dag`, `crw.review`,
`crw.reviewHalt`, `crw.explore`, `crw.exploreEvidence`, `crw.land`, `crw.tierStep`, `crw.evidence` and `crw.skip`. The seams the
operations are built on - `Relay` (the scheduler: milestone, release, agent acceptance and integration, the close of an execution),
`Verifier` (`CILocalVerifier` runs `crw-dev ci local`), `Decisions` (`StoreDecisions` writes the relay store), `Closer` (F13) and
`Reporter` (F11) - are interfaces in `internal/flow/ops/seams.go`; the dagsched-backed `Relay` belongs to the run process.

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
| `lifecycle_land_twice`, `lifecycle_land_without_review`, `lifecycle_order` | A lifecycle automaton per executor node and per function that calls `crw.commit`, `crw.verify`, `crw.review` or `crw.land`: build once (a rebuild is allowed until the node has landed), then rounds of commit, verify, review, then land at most once, from a completed review. A call starts its event where it runs and completes where its promise is used: awaited, returned, passed on or kept by the expression around it (the right side of `&&`, `||` or `??`, the left side of `||` or `??`, the last part of a sequence, a branch of `?:`, the value of an assignment), or read (passed on, closed over, `{ p }`) through a name that holds it. A promise whose value is discarded never completes: a call used as a statement, under `void` or another unary operator, in a sequence but its last part, on the left of `&&`, in a test, a comparison, arithmetic or an untagged template, and a held name read only in such a place; a stored promise that nothing reads leaves the node where it was, so `crw.review(...)` without `await` is not a review a later `crw.land` can follow. Started is enough for two rules: a second `crw.land` is a land twice whether or not the first was awaited, and a `crw.commit` started after a review leaves the node built (a change no round has committed), so the land needs a new round. A build, commit or land of a node, started, begins a new generation of it: a promise of an earlier call of that node that settles afterwards belongs to the head before it, so it neither makes the new head committed, verified or reviewed nor takes a started land back (an old `crw.review` awaited after a new `crw.commit` or after `crw.land` changes nothing). Where paths meet, a name keeps every call it may hold there, each with its own generation, and reading it puts the node in any phase one of those paths reaches, so the result does not depend on which branch the text lists first: `let r = crw.review(...)`, then a new round on one branch and `r = crw.review(...)` of the current head on the other, then `await r` and `crw.land`, has a reviewed path and is no finding in either branch order. A `break` or `continue` with a label goes to the statement the label names. A node is named by the `node` option of its executor call and by the first argument of the `crw.*` calls, so the phase of one node never stands for another's; a node the function does not build is taken as built. A violation is reported when it is illegal on every path that reaches it; the finally block of a try is checked from the state at the end of the body or handler and from the state of each completion that leaves through it (a throw no handler takes, a return, a break or continue to a loop outside the try), and that completion then goes on where it was going with the state the block leaves, unless the block returns, throws, breaks or continues itself. A handler starts from the state before the body or after any call in it, and an exception it takes no longer reaches the finally block. |
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
terminal row (`completed`, `null_model`, `held`, `refused`, `outcome_unknown` or `superseded`). A held row records whether the
call's packet reached the host (`input_state`: `delivered` or `not_delivered`) and the `codexErrorInfo` verbatim; on a backend
that places calls an executor's held row also carries `p_held`, the working snapshot of its worktree when the failure was
classified, and its completed row `pending_tree`, the working snapshot when the turn ended. The run record holds the workflow name, the script digest, the helper digests, the digest of `args`, the meta, and
the path and digest of the execution policy file.

The `prepared` row also carries what the resume walk compares: `input_digest` (the prompt, schema, role, pair, node, options,
context boundary and the joins before the position), `prompt_digest`, `context` and, with `Config.InputSnapshot`, `input_snapshot`.
A join or `workflow()` position writes `join_opened` and `join_settled` flow rows, the latter with the `digest` of its item paths'
results; a resume writes `hold_cleared`, `worktree_preserved`, `worktree_restored` and `checkpoint_started` flow rows
([journal.md](journal.md#the-resume-walk)).

A backend that places calls adds to a call's rows: the `allocation` member of `prepared` (below), `fork_requested` and
`fork_started` in place of `thread_requested` and `thread_started` for a continuation that runs in a restored worktree, and
`interrupt_requested` before a turn the engine interrupts. Flow rows named `escalation` carry a `reason` (`quota_or_rate`,
`restore_snapshot_mismatch`, `dispatch_unknown`) with the call and node they stopped.

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

## The App Server backend

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
the allocator does. A continuation that runs in no restored worktree makes nothing and keeps the directory its thread started in. Every other role
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
