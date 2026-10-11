# Parity corpus: Claude scripts and their CRW ports

Each directory holds one workflow script in the shape the Claude `Workflow` tool accepts (`meta` and a body, no imports) as `claude.js`, kept byte for byte, and its CRW port `crw.js`. `golden.json` fixes, for the port run on the fake backend against `policy.json`, the call graph and the return value together with the role key, the sandbox, `requestedPair` and `configuredPair` that each call resolved to. A build that fell to `read-only` fails the test. `golden.json` also pins the SHA-256 of `claude.js`, so an edit to an original is a test failure.

## Real originals

The directories named `real-*` are scripts that Claude Code sessions of this host ran, copied byte for byte from the Workflow tool's own copy of each script. `source.json` of each names the file, the session, the SHA-256 and the size of the original, and `TestParityCorpusProvenance` requires the SHA-256 to be the one of `claude.js`. The set has at least six; they were chosen among the 162 scripts of this host that `crw flow` loads, for the shapes the design lists that run on the fake backend without `args`: an explicit `effort`, a verification step with `agentType: 'ocx-gpt-6-1-sol'`, a fix on the second tier, a `parallel` panel of judges, a `parallel` search on the cheap model, and a `pipeline`.

None of them is edited to make it pass. The engine refuses a call whose `schema` is not strict (`schema_not_strict`: an object needs `additionalProperties: false` and every property in `required`), and the schemas of Claude scripts are not, so the calls of a real original that carry a schema are refused by the engine today, however their role resolves. The golden records that: such a call stays in the golden with status `refused`, reason `schema_not_strict`, and the role, sandbox and pairs it resolved to; `TestParityCorpus` accepts no other refusal for a real original. The scripts that loop on the verdict of a call or nest a workflow depend on a schema-bearing result that is refused, so no real original reaches those branches on the fake backend; the loop and the nested workflow are covered by the written reductions below.

## Reductions

The directories without the `real-` prefix are scripts written in the shapes the Claude scripts of this host have, with strict schemas so that every call completes on the fake backend: they show the loops, the nested `workflow()` and the verification rounds that the real originals cannot reach. They are not copies of any one session's script and do not count towards the real originals.

## Ports

The port differs from the original in two ways only. It adds a key to the options object of an `agent()` call (`isolation: 'worktree'`, `agentType: 'explorer'` or `stepDecision`), and it adds one `crw.tierStep` line before a `model: 'sonnet'` call. `TestParityPortDiffOptionsOnly` compares the files line by line and requires an entry here for every line the port changed and for every line that names `ocx-gpt-6-1-sol`. Each entry is `crw.js:<line>` (or the shared file named), what was done to the line, and why.

### real-routing-smoke-test

a one-item `pipeline` of a build on `model: 'haiku'` with `effort: 'medium'` and its verification by `agentType: 'ocx-gpt-6-1-sol'` with `effort: 'high'`. It has no schema, so both calls complete on the fake backend.

- `crw.js:8` ported (`claude.js:8`): adds `isolation: 'worktree'`. This call builds. In Claude a `model: 'haiku'` call without `isolation` edits the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. A build is `isolation: 'worktree'` (role `executor`, sandbox `workspace-write`); an investigation is `agentType: 'explorer'`. Resolving it to the explorer would let the build finish without editing, and to the executor without an isolation would leave the edits outside the run's commit path. The control flow is unchanged.
- `crw.js:9` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.

### real-register-fixpass

a fix on the second tier (`model: 'sonnet'`, no schema) and a readback by `agentType: 'ocx-gpt-6-1-sol'`.

- `crw.js:17` added: Sonnet is the second tier of the executor (role `executor-t1`). CRW starts that thread only after a recorded step decision, so the stage asks `crw.tierStep` for one first (this line) and the next line passes its id as `stepDecision`. The stage's control flow is unchanged.
- `crw.js:18` ported (`claude.js:17`): adds `stepDecision: step.id`. This is the `agent()` options line of the second-tier call; the only difference from `claude.js` is the added `stepDecision: step.id` key, which carries the decision of the `crw.tierStep` line above.
- `crw.js:21` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.

### real-wave1-judge-per-lane

one adversarial judge per lane in `parallel`, each on `agentType: 'ocx-gpt-6-1-sol'` with `effort: 'xhigh'` (role `reviewer-final`).

No line is ported; the script runs as it is.

- `crw.js:17` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.

### real-project-scout

a scout per project in `parallel` on `model: 'haiku'`, then a check by `agentType: 'ocx-gpt-6-1-sol'`.

- `crw.js:29` ported (`claude.js:29`): adds `agentType: 'explorer'`. This call only reads and reports. In Claude a `model: 'haiku'` call without `isolation` is a call in the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. An investigation is `agentType: 'explorer'` (role `explorer`, sandbox `read-only`); a build is `isolation: 'worktree'`. The control flow is unchanged.
- `crw.js:33` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.

### real-backlog-scout

a scout per half of the backlog in `parallel` on `model: 'haiku'`, then a check by `agentType: 'ocx-gpt-6-1-sol'`.

- `crw.js:21` ported (`claude.js:21`): adds `agentType: 'explorer'`. This call only reads and reports. In Claude a `model: 'haiku'` call without `isolation` is a call in the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. An investigation is `agentType: 'explorer'` (role `explorer`, sandbox `read-only`); a build is `isolation: 'worktree'`. The control flow is unchanged.
- `crw.js:26` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.

### real-port-scout

a scout per group in `parallel` on `model: 'haiku'` with an explicit `effort: 'medium'`.

- `crw.js:46` ported (`claude.js:46`): adds `agentType: 'explorer'`. This call only reads and reports. In Claude a `model: 'haiku'` call without `isolation` is a call in the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. An investigation is `agentType: 'explorer'` (role `explorer`, sandbox `read-only`); a build is `isolation: 'worktree'`. The control flow is unchanged.

### build-verify-fix

build, verify with a second model (`agentType: 'ocx-gpt-6-1-sol'`, `effort: 'high'`), fix.

- `crw.js:5` ported (`claude.js:5`): adds `isolation: 'worktree'`. This call builds. In Claude a `model: 'haiku'` call without `isolation` edits the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. A build is `isolation: 'worktree'` (role `executor`, sandbox `workspace-write`); an investigation is `agentType: 'explorer'`. Resolving it to the explorer would let the build finish without editing, and to the executor without an isolation would leave the edits outside the run's commit path. The control flow is unchanged.
- `crw.js:7` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.
- `crw.js:10` ported (`claude.js:10`): adds `isolation: 'worktree'`. This call builds. In Claude a `model: 'haiku'` call without `isolation` edits the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. A build is `isolation: 'worktree'` (role `executor`, sandbox `workspace-write`); an investigation is `agentType: 'explorer'`. Resolving it to the explorer would let the build finish without editing, and to the executor without an isolation would leave the edits outside the run's commit path. The control flow is unchanged.

### find-dedup-verify

find per area in parallel, drop duplicates, verify each one with a second model.

- `crw.js:6` ported (`claude.js:6`): adds `agentType: 'explorer'`. This call only reads and reports. In Claude a `model: 'haiku'` call without `isolation` is a call in the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. An investigation is `agentType: 'explorer'` (role `explorer`, sandbox `read-only`); a build is `isolation: 'worktree'`. The control flow is unchanged.
- `crw.js:16` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.

### judge-panel

three judges in parallel, then a final judge.

No line is ported; the script runs as it is.

- `crw.js:9` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.

### loop-until-dry

sweep round after round until a round finds nothing new.

- `crw.js:7` ported (`claude.js:7`): adds `agentType: 'explorer'`. This call only reads and reports. In Claude a `model: 'haiku'` call without `isolation` is a call in the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. An investigation is `agentType: 'explorer'` (role `explorer`, sandbox `read-only`); a build is `isolation: 'worktree'`. The control flow is unchanged.

### pipeline-no-barrier

`pipeline` of plan, apply on the second tier, verify, with no barrier between stages.

- `crw.js:10` added: Sonnet is the second tier of the executor (role `executor-t1`). CRW starts that thread only after a recorded step decision, so the stage asks `crw.tierStep` for one first (this line) and the next line passes its id as `stepDecision`. The stage's control flow is unchanged.
- `crw.js:11` ported (`claude.js:10`): adds `stepDecision: step.id`. This is the `agent()` options line of the second-tier call; the only difference from `claude.js` is the added `stepDecision: step.id` key, which carries the decision of the `crw.tierStep` line above.
- `crw.js:13` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.

### nested-workflow

build, then one nested level of `workflow()`.

- `crw.js:4` ported (`claude.js:4`): adds `isolation: 'worktree'`. This call builds. In Claude a `model: 'haiku'` call without `isolation` edits the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. A build is `isolation: 'worktree'` (role `executor`, sandbox `workspace-write`); an investigation is `agentType: 'explorer'`. Resolving it to the explorer would let the build finish without editing, and to the executor without an isolation would leave the edits outside the run's commit path. The control flow is unchanged.
- `child-review.js:5` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.

### explicit-effort-build

a build and a check that both state an explicit `effort`.

- `crw.js:5` ported (`claude.js:5`): adds `isolation: 'worktree'`. This call builds. In Claude a `model: 'haiku'` call without `isolation` edits the shared checkout, but CRW gives every call its own write cwd and calls never share one, so the same call is refused with `policy_refused` (cause `alias_ambiguous`): an isolation-less haiku is either a build or an investigation and CRW will not guess. A build is `isolation: 'worktree'` (role `executor`, sandbox `workspace-write`); an investigation is `agentType: 'explorer'`. Resolving it to the explorer would let the build finish without editing, and to the executor without an isolation would leave the edits outside the run's commit path. The control flow is unchanged.
- `crw.js:7` kept: Kept as written. `ocx-gpt-6-1-sol` resolves to role `reviewer` (sandbox `read-only`, the policy's reviewer pair) and, when the request states `effort: 'xhigh'`, to role `reviewer-final`. The effort the script states is never applied and never refused: it stays in `requestedPair` while `configuredPair` shows the pair the policy gives the role, so the Claude script runs without an effort edit. The value is the same whether it is the `model` or the `agentType`.
