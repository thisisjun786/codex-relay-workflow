# Parity corpus: Claude scripts and their CRW ports

Each directory holds one workflow script in the shape the Claude `Workflow` tool accepts (`meta` and a body, no imports) as `claude.js`, kept byte for byte, and its CRW port `crw.js`. `golden.json` fixes, for the port run on the fake backend against `policy.json`, the call graph and the return value together with the role key, the sandbox, `requestedPair` and `configuredPair` that each call resolved to. A build that fell to `read-only` fails the test. `golden.json` also pins the SHA-256 of `claude.js`, so an edit to an original is a test failure.

The scripts are written in the shapes the Claude scripts of this host have (strict schemas, `parallel` and `pipeline` for concurrency, templated labels); they are patterns, not copies of any one session's script.

The port differs from the original in two ways only. It adds a key to the options object of an `agent()` call (`isolation: 'worktree'`, `agentType: 'explorer'` or `stepDecision`), and it adds one `crw.tierStep` line before a `model: 'sonnet'` call. `TestParityPortDiffOptionsOnly` compares the files line by line and requires an entry here for every line the port changed and for every line that names `ocx-gpt-6-1-sol`. Each entry is `crw.js:<line>` (or the shared file named), what was done to the line, and why.

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
