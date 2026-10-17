# CI operation

[POLICY.md](../POLICY.md) owns the rules. This page maps them to commands and the steps that
activate GitHub enforcement. It is developer-only: every check runs from a checkout.
Installing and operating the runtime is [runtime installation](runtime-install.md).

The repository verification is selected by `verifier.kind`: `local` (the default) runs
`crw-dev ci local`, and `github-actions` dispatches the configured GitHub workflow.
Both produce a `verification-record/1`; a passing record is the integration evidence.
The node, integration and milestone-close paths use the same configured verifier.

## Choosing the verifier

Existing configurations keep the local verifier and its `go_root` and `crw_dev` settings.
To select GitHub Actions:

```sh
crw config verifier --kind github-actions \
  --repository OWNER/REPO --workflow 'CRW CI' --branch-prefix ci/
```

The corresponding configuration section is:

```json
{
  "verifier": {
    "kind": "github-actions",
    "repository": "OWNER/REPO",
    "workflow": "CRW CI",
    "branch_prefix": "ci/"
  }
}
```

`repository` is required in GitHub mode. The workflow defaults to `CRW CI` and the
branch prefix to `ci/`. An unknown kind or invalid remote configuration stops verification;
it does not fall back to a local run. `crw config verifier --kind local` selects local
verification again. Prerequisite checks inspect the selected mode: GitHub mode needs
`git`, `gitleaks`, an authenticated `gh`, repository write permission, and an active
workflow. It does not require local Go or `crw-dev`. Authentication uses the caller's
normal GitHub CLI configuration or token.

The common command can verify a committed head directly:

```sh
crw flow verify --checkout /path/to/repository \
  --head FULL_HEAD_SHA --base FULL_BASE_SHA --record /path/to/verification-record.json
```

The integration queue can set its existing `verify_command` to
`["crw", "flow", "verify"]`. The command reads `CRW_VERIFY_HEAD`,
`CRW_VERIFY_BASE` and `CRW_VERIFY_RECORD` supplied by that queue. The lane's
integration adapter and `crw.verify` use the same selection. Existing plans keep
`crw-dev ci local` as their repository-CI requirement string; the selected verifier
satisfies that requirement, while the receipt's `runner` and `verifierKind` say where
verification actually ran.

Before a GitHub push, Gitleaks must report the version the verified commit declares
as `scan_version` in `scripts/ci/secrets.sh`; an absent pin or a version mismatch
stops verification. It scans the current `base..head` commit range, including merge
deltas, with redaction and repository ignore suppression. A finding, missing
scanner or unreadable range stops the operation before anything is pushed.
Every push URL of `origin` must match the configured repository. The verifier pushes the
exact head to an existing work branch when available, otherwise to
`ci/<label>-<sha8>` (or the configured prefix). It never force-pushes or deletes a
remote branch, and it never uses `dev`, `main` or `master` as the verification branch.
Automatic tag following is disabled for this push.

The verifier dispatches the workflow at that branch and waits for a new run with the
exact head, branch, workflow and `workflow_dispatch` event. A successful run needs
nonempty job results with every job successful. A failed run gets one
`gh run rerun --failed`; the verifier waits for the new attempt, retains the original
failures, and records `flake_rerun_passed: true` when the rerun passes. Cancellation,
missing results and infrastructure errors cannot become a pass.

A GitHub receipt uses `runner: github-actions`, `verifierKind: github-actions` and
`verifierRules: github-actions/1`. It carries the run URL, run head, workflow identity
and digest, job conclusions, failed test lines, attempt history and the current
range's secret-scan evidence. Its `pins` are declarations from the commit; empty
`tools`, `os` and `arch` fields do not claim local measurements of remote runners.
The workflow definition identifies the remote execution environment. Local receipts
keep their measured tools and platform checks.

Successful remote records are cached below the Git common directory, so linked
worktrees can reuse them. The key includes the tree, repository, workflow identity
and digest, CI and dependency digests, declared pins, verifier kind and rule version.
The current range is scanned again on every reuse. Only new commit objects may have
appeared since the original run: new tree or blob objects require a full workflow
run, including when a file was added and removed before returning to the same tree.
An empty commit or a same-tree integration merge can reuse the passing tree evidence.
The new receipt names its current head and base and retains the original run as
`reusedFrom` provenance. Failed, malformed and incompatible records are not reused,
and local records cannot supply remote evidence.

## Checks

| Command | Where CI runs it |
| --- | --- |
| `crw-dev ci validate` | `validate`: skill metadata, local Markdown links (and, for the documents under `plugins/crw/skills` and `docs`, the `#fragment` of each link: it must be a heading slug of the file it leads to, by GitHub's rule, or an explicit `id` or `name` attribute of an HTML element outside code (the slug is taken from the rendered heading text, in which an inline link, and a full or collapsed reference link whose label a complete CommonMark link reference definition (destination, optional title, nothing after them on the line) defines, is its text; a setext heading is its whole paragraph; the attribute value is read with its character references decoded and an HTML comment, which opens only where markup can and never inside a quoted attribute value, holds no element), and a target that cannot be read is an error, not a pass; `TestSkillAnchorsResolve`), the base rule of the skill inventory map ([docs/skills/README.md](skills/README.md)), and that Python sits only in skill assets: a `.py` file or a python-shebang script, tracked or untracked and not ignored, fails it unless it is below `<skill>/scripts/` or `<skill>/examples/` of `plugins/crw/skills` (`TestTrackedPythonStaysInSkillAssets` holds the tracked files for `make test`); CI installs no Python and runs no skill script (`TestWorkflow_installs_no_python`); and that no blob over 2 MiB comes into the history unless the allow list names it ([large blobs](#large-blobs)); and that `go.mod` and `go.sum` are what `go mod tidy` writes, checked offline (`GOPROXY=off`, `GONOPROXY=none`, `GOSUMDB=off`: no module download, also none for a module an inherited `GOPRIVATE` or `GONOPROXY` names, and no checksum database lookup) by `go mod tidy -diff`, which prints a note instead of failing when the module cache lacks a module the import graph needs |
| `crw-dev ci plugin` | `validate`: plugin package shape, payload hygiene, the manifest's fixed release version, and the staged artifact's derived version ([below](#plugin-package)) |
| `crw-dev ci contracts` | `validate`: the offline contract checks built into `crw-dev`: the hook replay, the CXC corpus lint (which also holds the plugin's hook declarations to K1; `crw-dev cxc hooks` regenerates them), the operations shape check (`crw-dev ci operations`), the component definition, the start-policy self-test and the parent-title replay |
| `crw-dev ci refactor-backlog` | `validate`: the generated refactor backlog: assembles `docs/port/refactor-backlog.md` from the fragments under `docs/port/refactor-backlog.d` and refuses when the committed file differs from the fragments (`--write` regenerates it. The file ends in a comment that seals everything above it: a file that still matches its seal is replaced freely, because every line the fragments no longer produce is the old text of an edited or removed fragment; a file that does not match (edited by hand, or written before the seal) is refused when it holds an entry, compared as its whole line, that no fragment produces. It also refuses when the file or the fragments cannot be read, or when both are missing from this repository, whatever is left of `docs/port`) |
| `bash scripts/ci/secrets.sh` | `secrets`: checksum-pinned Gitleaks scan: on a manual dispatch every fetched ref ([scope](#secret-scanning)) |
| `node --test plugins/crw/skills/*/tests/*.test.mjs` | `skill-scripts-node`: the skills' own Node tests (the skills ported from CXC carry them), on Node 24.20.0, only when a skill path changed from the run's own base (a pull request from its merge base); a run that skips them is success |
| `npm ci`, `npm test`, `npm run build -- --outDir "$RUNNER_TEMP/gui-built" --emptyOutDir`, `crw-dev ci gui-drift --built "$RUNNER_TEMP/gui-built"` | `gui`: the screens under `web/` build and match the committed `internal/gui/assets` tree byte for byte, on Node 24.20.0, only when a watched path changed ([below](#the-gui-job)); `make gui` runs the same three commands locally |
| `make lint` | `go-product` leg `lint`: vet (also of the `dev` and `integration` tagged packages), staticcheck and gofmt |
| `make test-part TEST_PART=<part>` | `go-product` legs `test-1` through `test-5`, `test-rest`, and `test-dev`: the Go tests and the contract corpus; together the parts are `make test` |
| `CGO_ENABLED=0 make dist` per target | `go-product` leg `dist`: static `crw` for linux/amd64, linux/arm64 and darwin/arm64, uploaded with `SHA256SUMS` |
| `CRW_TEST_BINARY=<crw> go test -tags integration ./internal/runtime/integration/...` | `dist`: installs release archives of the linux/amd64 binary it built (and a relinked second one) into an isolated home with `crw install` and runs the plugin's declared Stop hook and MCP server against them (IS-1, IS-7, IS-8) |

`crw-dev` is the development binary (`make crw-dev`); it builds only with the `dev` tag, so
`make dist` and the release archives never contain it. Its checks have no Python twin: the copies
under `scripts/ci` and `scripts/check_operations_contract.py` left in refactor R3 (decision R3R-1).
`crw-dev ci local` runs this same table locally and records the result as a
`verification-record/1` (CRW-964's command and format, documented in its own section).
See the [workflow](../.github/workflows/ci.yml) for the exact job inputs.

## The local run

`crw-dev ci local` (and `make ci-local`) runs every job and step of
[the workflow](../.github/workflows/ci.yml) locally, at the same pinned
versions, and writes a `verification-record/1`. This is the execution path when
`verifier.kind` is `local`. GitHub mode instead obtains that evidence through
`workflow_dispatch`, as described in [Choosing the verifier](#choosing-the-verifier).

Independent jobs and matrix legs run concurrently. `-jobs N` bounds the number in flight;
`-jobs 1` runs them serially. The default (`-jobs 0`) is the selected CPU budget divided by
`-parallel` (Go package parallelism, default 4), rounded down with a minimum of one:
the default four-core budget selects one job, and eight cores select two. An explicit job
count is capped to the budget. Steps within a job remain sequential. The aggregate `dev-gate`
waits for every preceding job, including failed jobs. Output and record entries stay in
workflow order, with each job's output kept together.

### The CPU budget

One verification runs inside one CPU budget: `--cpus N`, default 4 or `CRW_CI_CPUS` (an
explicit `--cpus` wins over the environment; the value must be positive). On Linux the
whole verifier re-executes inside a `systemd-run --user --scope` cgroup whose
`CPUQuota=<N>00%` bounds every descendant — all jobs and steps share the single scope,
never one quota per job, and descendant Go processes read it themselves. A launcher that
cannot start the scope (no user manager, no systemd-run) falls back to the Go settings:
each job gets a per-job `GOMAXPROCS` share of the budget and the resolved `-parallel`
(clamped to that share), an approximation of the limit, not a kernel cap over arbitrary
native programs. A child verifier that already entered and then fails reports its own
status and is never retried. The budget seals the step environment: neither the caller's
nor a step's own `GOMAXPROCS`/`GOFLAGS`/`GODEBUG` can widen it. The record's `cpu` names
the limit, the method (`systemd-cgroup-v2` or `gomaxprocs`), the job count and the
parallelism the budget resolved to; a record made under a different budget — or a legacy
record with none — is not reused. This bounds one verification; it is not a host-wide
verification queue.

The run makes **clean worktrees** of the commit being verified under the work root (the XDG state directory, or --work-root)
(`git worktree add --detach`), with `HOME` and the XDG directories pointed into a
temporary home and `TZ=UTC`, so uncommitted changes in the caller's checkout and the host's
caches cannot change the result. The record names the commit and its tree, never the caller's
working state. The worktrees are removed afterwards. The engine that decides the steps is the caller's own source (`internal/dev/ci`, `cmd/crw-dev`, `Makefile`, `internal/crwconfig`, whose tool-root code the engine runs to build every step's environment, and `internal/gitprobe`, whose repository marker check refuses a step TMPDIR before the steps run), so the run refuses with `engine_differs_from_commit` when the working tree under any of those paths differs from the commit's files byte for byte: an edited, deleted or extra file, whatever the index marks (assume-unchanged, skip-worktree) or the ignore files say. An ignored file is let through only when Go does not compile it (a log, a coverage file); a FIFO, device or other special file in place of an engine file is a difference and is never opened. A prebuilt `crw-dev` is held to the same rule: `go build` stamps the revision and the modified flag in it, and the run refuses with `engine_differs_from_commit` when the binary was built from a modified tree or from a revision whose engine paths differ from the verified commit's (`make crw-dev` from a clean checkout of the commit builds the right one). A binary with no revision (`go run`, `go test`, `-buildvcs=false`) is compiled from the working tree that the byte comparison has just checked, so the comparison is all there is to bind it. No record is written or reused from such a checkout. The `secrets` step fetches the linux x64 Gitleaks archive and calls `sha256sum`, so a plan that carries it refuses any other host with `unsupported_platform` before a step runs.

Each job has its own checkout, home/XDG directories, `TMPDIR`, runner output/scripts,
and `GOCACHE`. Job caches override the caller's inherited or configured build cache.
`GOTMPDIR` is removed from job environments so Go build work and test temporaries use
that job's `TMPDIR`, including when a test temporarily changes it. Concurrent builds
cannot replace another job's test binary.
`NPM_CONFIG_CACHE` may still be inherited. The module cache is not inherited, not even
`paths.module_cache`, and GOFLAGS is set by the engine
([where crw keeps its files](runtime-install.md#where-crw-keeps-its-files)).

### The local step to GitHub job mapping

| ci.yml job | ci.yml step | local |
| --- | --- | --- |
| `validate` | the sparse `scripts/ci` checkout and the edit mirror | not run: hosted-only step |
| `validate` | checkout | the clean worktree of the verified commit |
| `validate` | `setup-go` | the pinned Go is resolved and recorded |
| `validate` | `go build -tags dev -o "$RUNNER_TEMP/crw-dev" ./cmd/crw-dev` | run |
| `validate` | `"$RUNNER_TEMP/crw-dev" ci validate` | run, over the record's range (`BLOB_RANGE_BASE`) |
| `validate` | `"$RUNNER_TEMP/crw-dev" ci plugin` | run |
| `validate` | `"$RUNNER_TEMP/crw-dev" ci contracts` | run |
| `secrets` | the mirror pair, checkout | as above |
| `secrets` | `bash scripts/ci/secrets.sh` | run, with `GITHUB_EVENT_NAME=pull_request` and `PR_BASE_SHA`, so it scans base..head as a pull request run does |
| `skill-scripts-node` | the mirror pair, checkout | as above |
| `skill-scripts-node` | the changed-path decision | run: the same script, its answer recorded; the local run performs every step either way |
| `skill-scripts-node` | `setup-node` | the pinned Node is resolved and recorded |
| `skill-scripts-node` | the staged skill-script tests | run |
| `gui` | the mirror pair, checkout | as above |
| `gui` | the changed-path decision (`scripts/ci/gui_paths.sh`) | run, its answer recorded |
| `gui` | `setup-go`, `setup-node` | as above |
| `gui` | `npm ci`, `npm test`, `npm run build -- --outDir "$RUNNER_TEMP/gui-built" --emptyOutDir` | run (in `web/`) |
| `gui` | `ci gui-drift --built "$RUNNER_TEMP/gui-built"` | run |
| `go-product` | the light mode notice | not run: hosted-only step |
| `go-product` | the mirror pair, checkout, `setup-go` | as above |
| `go-product` | `make lint` (leg `lint`) | run |
| `go-product` | `make test-part TEST_PART=<part>` (legs `test-1`..`test-5`, `test-rest`, `test-dev`) | run, subject to `-jobs` |
| `go-product` | the three `make dist` builds and `sha256sum` (leg `dist`) | run |
| `go-product` | `CRW_TEST_BINARY=... go test -tags integration ./internal/runtime/integration/...` (leg `dist`) | run |
| `go-product` | the four `upload-artifact` steps | not run: nothing is uploaded locally |
| `dev-gate` | "Require every prerequisite to succeed" | run: every prerequisite job must have passed |

`internal/dev/ci/local_plan_test.go` holds the table to the workflow job by job and step by
step, and the runner refuses a ci.yml the table does not cover.

### The verification record

The record is `verification-record/1`: `repository`, `baseCommit`,
`headCommit`, `treeHash`, `ciDigest` (the sha256 of ci.yml),
`tools` (Go as the module at the commit selects it, probed with the steps' isolation, and Node as observed on the host; gitleaks and staticcheck at the version
their step runs, which is the pin, because secrets.sh runs its own pinned Gitleaks and the lint
leg runs the staticcheck the tree requires), `pins` (the versions the tree
pins: go.mod's toolchain and staticcheck, ci.yml's Node, secrets.sh's Gitleaks),
`pinMismatch`, `goFlags` (the GOFLAGS the steps inherit, without the flags that select or skip tests, so a full run runs every test; a record whose GOFLAGS name a modfile or an overlay is never reused), `range` (the sha256 of the commits between base and head, the input of the blob and secret steps), `goEnv` (GOENV is unset in the
steps, so Go reads its default file under the run's own empty home, and the host's GOENV never
applies),
`dependencies` (the sha256 of `go.sum` and
`web/package-lock.json`), `os`, `arch`, `result`, `runner`, and `jobs`
— one entry per GitHub check, each with its steps' `command`, `scope`,
`result`, `seconds` and `reason`, and `testPackagesRun` (CRW-1328, additive): the count of go test packages whose tests
ran, read from the package result lines go test prints (`[no tests to run]`, `[no test files]` and a build failure ran none); it is
absent when no step printed such a line and from every record written before it, which a reader takes as unknown, never as zero, and
a count of 0 changes no result (the flow's gate warns `no_tests_run`), and `runEnv` (CRW-1489, additive): where the run ran,
as `cwd` (the directory `crw-dev` ran in), `root` (the repository root it verified), `callerTmpDir` (the caller's `TMPDIR`,
empty when it was unset), `tmpDir` and `home` (the run's own temporary and home roots).
Multiple jobs use separate children of these roots; a single-job plan uses the roots directly.
These are paths,
written as they are. A failed checkout's record carries it too. It is absent from every record written before it, which a
reader takes as unknown, and it is not a reuse key: a reused record keeps the `runEnv` of the run that verified it. `cpu` (CRW-1591, additive) names the budget
the steps ran under — `limit`, `method`, `jobs`, `parallel`, and the per-job `gomaxprocs` under the fallback;
absent means a legacy or unbudgeted run, which a budgeted run's record cannot answer and vice versa. `digest` is the sha256 of the
canonical serialization: the record as JSON with its keys sorted, no indentation and no HTML
escaping, and the `digest` member removed. A tool whose observed version differs from its
pin is named in `pinMismatch`, and such a record is never reused.

A step runs under `bash --noprofile --norc -eo pipefail`, as the runner does, so its first
failed command fails it. A failed step's `reason` gives the exit status, the failing test lines
of its output (a `--- FAIL`, `FAIL` or `panic:` line), their associated diagnostic lines,
and the last 40 lines of the output. Selected lines are preserved in full, including long
UTF-8 messages; failure blocks are not dropped after a fixed number of failures.
A Go test that outlived its `-timeout` is named with a `timeout:` prefix,
so a load timeout reads as a timeout, not as a verdict on the code. The steps inherit `PATH`.
They do not inherit `TMPDIR`: a run makes a fresh, empty root of its own (CRW-1489),
`ci<8 hex>` directly below the caller's temporary directory, gives each job a separate child,
and removes the root after the run (`--keep`
keeps and prints it; a run killed before its cleanup leaves it, see
[Leftover verify trees](#leftover-verify-trees)). The temporary directory is `TMPDIR`, or `/tmp`
(source `default`) when `TMPDIR` is unset or empty; it is taken at its physical place, resolved from
the value as written the way the kernel resolves it (links followed before a `..`, not a lexical
clean first, so `/a/link/../tmp` is the `tmp` beside link's target, not `/a/tmp`), and the record's
`callerTmpDir` keeps the raw value while `tmpDir` names the directory made. There is one place and
no second one (CRW-1569): the steps are never moved below the work root or anywhere else, and the
length of `TMPDIR` is not judged, because no test socket lies below it (tests bind their sockets
below a short socket folder under `/tmp`, CRW-1568).

Before any checkout or step, the run checks, in this order, and is refused with the reason and no
record when one fails:

1. the temporary directory is absolute (`tmpdir_not_absolute`) and not in the account's real home,
   judged before anything is written there;
2. it exists, is a directory and is writable (`temp_dir_unusable`, naming the directory and its
   source);
3. no repository is at or above it, because the repository's own tests find a checkout by walking up
   from `TMPDIR` for a `.git` (a sandbox's stray `/tmp/.git` made them judge the wrong tree): a `.git`
   counts only when it resolves to a Git directory — a directory holding a regular `HEAD` and an
   `objects` directory (the objects reached through `commondir` for a linked worktree), or a
   `gitdir: ` file naming one — so an empty or garbage `.git` is no marker and is ignored (CRW-1588):
   `temp_dir_unusable: <dir> is below <.git>`, set `TMPDIR` to a directory outside every repository;
4. a socket folder made (and removed) below `/tmp` leaves room for the longest socket path a test puts
   below it within the 103-byte bound of a Unix socket path: `socket_dir_unusable`, naming the folder
   and, when it does not fit, the bytes needed, the bound and how far over.

A step's result is `passed`, `failed`, `missing_tool`,
`skipped` or `not_applicable`. The whole result is `pass` only when no step
is `failed`, `missing_tool` or `skipped`: one step that fails, does not run
or lacks its tool fails the run, and the record names it with its reason.

### Sealed runs

A step runs in a sealed environment built from an allowlist, not from the caller's environment
minus a denylist: HOME and the XDG directories point into the run's own home, TZ is UTC,
GOTOOLCHAIN is local, and GOFLAGS is set by the engine alone (its parallelism, from
--parallel), and TMPDIR is the run's own step directory, set by the engine (above); the tool-version
probes keep the caller's TMPDIR. The caller's other variables do not reach a step, and the record names the Go,
cgo, Node and npm variables it left out in ignoredEnv (names only). A caller's GOFLAGS with
-exec, -toolexec, -overlay, -modfile, -run or -count therefore cannot change what a step runs.

Each job's Node pin is read from its own setup-node step in the commit's ci.yml, not from a
shared first match. The host's Node is observed in the same isolation, and a job whose pin
differs is named in pinMismatch as `job:node`; such a record is kept but never reused. The
gitleaks and staticcheck pins are the ones secrets.sh and go.mod fix, and the steps fetch
or run those pins, so they are recorded at the pin.

The clean worktree is made under a work root: the directory given by --work-root, or the XDG
state directory of the real user. A root must be absolute and outside the account's real home; where
it lies is not judged, so a root inside TMPDIR, /tmp or /var/tmp is accepted. A `.git` above the
directory the tests make worktrees in is judged by content, on the steps' `TMPDIR` (above).

### Reuse validation

A record answers a run only when every key above matches and the record is sound: its digest
is valid, it is sealed, it was made from the plan the commit's table gives (planDigest), it
matches that plan one job and one step at a time (name, command and scope), every job and
step passed (or is a not-applicable step the plan marks as such), its result equals the result its steps recompute to, and it has
no pinMismatch. Any other record is not reused: the engine runs the table again and says why.

### Reuse

The verifier finds sealed PASS records in
`<git-common-dir>/crw-ci/<tree>/<rules-key>/<record-digest>.json`, shared across linked worktrees.
`--reuse <record>` supplies a preferred candidate; `--fresh` disables reuse and still publishes a
new pass. The tree, command/plan digest, ci.yml, tools and pins, dependency digests, platform,
GOFLAGS, GOENV, heavy gate, CPU budget and verifier identity must match. Every candidate still faces the
record digest, plan structure and recomputed pin checks above. Failed records are never cached
or reused. An unavailable cache falls back to execution without choosing another storage location.

`verifierKind` identifies the implementation and `verifierRules` its contract version; legacy
records mean `local` and `ci-local/1`. `runner` remains a caller label. Other verifier kinds need
explicit compatibility support before their records can answer a local run.

Only **full** (tree-scoped) steps are reused. Every **range** step runs on the current base/head,
even when the range is unchanged. Earlier setup steps run too, to recreate files those checks
need: for example, the verifier binary used by `ci validate`. The aggregate gate runs after all
jobs and can fail despite reused tree checks. Each reused step has zero `seconds` and a
`reusedFrom` containing the source record path, digest, head, verifier kind, job and step index.
The new record names the current head/base/range and records the fresh execution environment;
only a plan requiring no fresh execution keeps the original run environment and package count.

### The heavy-check gate and TMPDIR

A heavy step (a Go build, vet, test, the dist builds, the integration test, `npm ci`) runs
through the command named by `CRW_CI_HEAVY_GATE` when it is set — `<gate> <command>`
— and directly when it is unset. The gate is the host's; no host path is written into the
repository. Temporary files go under the step's own `TMPDIR`, whose root the caller still sets: it is
made directly below the caller's `TMPDIR` (or `/tmp` when that is unset), and nowhere else (above).
Only the gate's executable is resolved, once, against the directory the run starts in. Its other
arguments are not resolved: a relative argument is read in each step's own directory, so give absolute paths.

### The pre-push hook

`crw-dev ci local hook install` writes a `pre-push` hook into the repository the
working directory belongs to (`git rev-parse --git-path hooks/pre-push`), and
`... hook status` reports `installed`, `absent` or `foreign`. The hook
refuses a push whose range (`<remote sha>..<local sha>`, or the whole local sha for a new
ref) brings in a blob over 2 MiB or a secret Gitleaks finds — the same two checks the hosted
`secrets` and `validate` jobs run, moved before the push because a public history
cannot drop what has already been pushed. It fails closed: a missing Gitleaks or an unreadable
range blocks the push. An existing `pre-push` this tool did not write is left untouched and
the install refuses; the tool writes no git config. The hook's size rule carries no allow list,
unlike `crw-dev ci validate`: the list is empty today, and a future entry must be paired
with the hook at that time. Tests install the hook only into temporary repositories; the real
shared checkout is the operator's to install.


## The workflow

`ci.yml` starts only on `workflow_dispatch`: a person or the configured GitHub
verifier requests the full hosted verification of a commit. In GitHub mode the
verifier binds that run to the requested head and writes the integration receipt.
The release workflow still verifies the SHA it publishes itself. The job bodies are
the ones `crw-dev ci local` runs locally, so a dispatch exercises the same checks.

Two jobs gate themselves on changed paths. `skill-scripts-node` runs the skills' Node
tests only when a path under `plugins/crw/skills` changed, and ends successfully without installing Node when
none did. A pull request is judged from its merge base — the three-dot range `base...head`, the
commits the branch adds to its base — so a staged-skill change that only the base branch carries is
not this pull request's, and a branch that carries the same change as its base is still selected;
the job's checkout fetches full history for that reason. A push to `dev` compares the commit it
replaced with the one it added, where the range is already the pushed commits, and a manual
dispatch has no base and runs the tests. `gui` runs the screen verification only when `web/`,
`internal/gui/assets/` or the gui definition changed ([the gui job](#the-gui-job)); it always
exists, `dev-gate` waits on it, and anything its decision cannot read selects the full run.

`validate`, `secrets` and the `go-product` legs start at once and run on separate runners.
`make test` builds one `crw` for the run (`dist/test/crw`, release-shaped with `-trimpath`) and
passes it as `CRW_TEST_BINARY`, so no package links its own; `internal/testsupport` `CRW` builds
one per test process when the variable is unset, and `CRWDevPath` and `BuildCRW` build `crw-dev` and
the seam builds a few tests need (a link-time clock, a build tag, an overlay) the same way. Every
package's `TestMain` is `testsupport.Main`, which points the homes, the XDG directories and the
relay's roots at one temporary tree (`crw-relay-test-*` in `TMPDIR`), keeps `CRW_REFUSE_LIVE_STATE=1`, and
removes the tree and every binary the process built after the tests. The tree's crw configuration (`xdg-config/crw/config.json`, `testsupport.SandboxConfig`) names `TMPDIR` as
`paths.sandbox_root`, so the sandbox-root rule (CRW-1265; a warning and a record since CRW-1308) judges the directories a test makes the same
whatever `TMPDIR` is; a harness that starts a process with an environment of its own passes that file as
`CRW_CONFIG` or writes the same document into the configuration home it chose; `crw-dev parity fire` and
`latency`, which isolate nothing, write it for their `--scratch` directory and hand it to every case. The fake
homes and configuration `sandboxroottest` installs for a test binary lie in its tree as well. A helper process a test starts from
its own test binary (a crash child, a lock holder) passes through that `TestMain` as well; it makes its
tree inside its starter's, named by `CRW_TEST_ISOLATION_ROOT`, so a helper killed by a signal or ended by
`os.Exit` leaves nothing once its starter removes its own tree. `CRW_TEST_KEEP_ROOT=1` keeps the trees of a
run and prints their paths, for debugging on your own machine ([leftover trees](#leftover-isolation-trees)).
The Makefile names the slowest packages as parts
and `rest` takes every other package plus the `dev`-tagged tests, so the parts are disjoint, add
up to `make test`, and a new package lands in `rest`; a renamed package makes its part fail in
`go list`, never skip. The `dist` leg builds the release binaries, then runs the isolated-home
integration test against the linux/amd64 one with `CGO_ENABLED=0` and `-trimpath`, so the test
reuses the dist build's compiled packages and only relinks.

`dev-gate` is the one aggregate check: a manual dispatch is refused when any job fails. It runs
after every other job, `if: always()`, so a failed
or cancelled prerequisite still produces a failing gate rather than a skipped one. The workflow
writes `toJSON(needs)` into a file in the step's own script (a quoted heredoc: the text is never
expanded by the shell and never travels in the environment, whose size the runner bounds) and
the gate passes only when that object names at least one job and every job's `result` is
`success`: a failure, a cancellation, a skip or anything unreadable fails it. `internal/dev/ci`'s
workflow tests hold the structure: `dev-gate`
needs exactly the other jobs, no job continues on error, every action is pinned by commit, every
Makefile part has a leg and every package runs in exactly one ([the test legs](#the-test-legs)), the
integration step runs in `dist` after the build, and the gate's script, run with a written results
file, passes only an all-success run.

Until wave R1 of the post-port refactoring a `selection` job classified the changed paths
(`crw-dev ci scope`) and the gate (`crw-dev ci gate`) re-checked that selection; a `tests` job ran
the Python twins' own tests on two Python versions. The selection's changed-path list reached
the gate in one environment variable, which made the gate fail with "Argument list too long" once
a change touched about a thousand paths. All three left; the release workflow's tests
became Go tests (`internal/contracttest` `TestReleaseWorkflow_*`), and the twins were compared with
their Go checks by `internal/dev/ci` until refactor R3 deleted them.

During iteration run the affected tests and reuse valid evidence for unchanged source, criteria
and environments. CI concurrency cancels obsolete runs within the same branch.

## The gui job

The screens are a Vite + React package under `web/`, and their build is committed under
`internal/gui/assets` and embedded in the `crw` binary (`//go:embed all:assets`), so a source
checkout with no Node still produces a `crw` that serves the dashboard. That makes the committed
tree a build artifact that must track its source, and the `gui` job keeps the two from drifting
apart.

The job always exists and `dev-gate` waits on it. Its first step decides from the changed files:
`scripts/ci/gui_paths.sh` compares a pull request's base with its head, or a push's replaced
commit with the one it added, over `web/`, `internal/gui/assets/`, `.github/workflows/ci.yml`,
`Makefile`, `scripts/ci/gui_paths.sh` and `internal/dev/ci/gui_drift.go`, and writes
`changed=true` to its step output when any of them moved. With no pull request or push event
left, a manual dispatch and any git failure also answer `changed=true`: the safe direction is
the full run, because a screen change that is skipped is a committed tree that no longer matches
its source.

When the answer is `true` the job installs `actions/setup-go` (the drift check is a `crw-dev ci`
subcommand) and `actions/setup-node` pinned by commit at Node 24.20.0, runs `npm ci` from the
committed lockfile, `npm test`, a build into `$RUNNER_TEMP/gui-built`, and then
`crw-dev ci gui-drift --built "$RUNNER_TEMP/gui-built"`. When it is `false` the job ends
successfully without installing Node. `make gui` runs the same three commands locally, and
`make gui-assets` regenerates the committed tree after a `web/` change: it builds into
`internal/gui/assets` (vite's configured `outDir`), so the result is what a contributor commits.

The drift check compares three trees. The committed tree comes from git (`git ls-tree -r -l` and
`git cat-file blob` at the named revision, `HEAD` by default) and never from the working tree. The
fresh side is the directory `--built` names, which is required and must hold an `index.html`;
naming the committed tree itself, a directory that was never built, or a tree that cannot be read
is refused rather than passed, so a run without a build cannot compare the commit against itself.
The third is the working tree at `internal/gui/assets`, which is what `//go:embed all:assets`
actually compiles: an untracked file, a modification or a deletion there is refused even when the
built and committed trees agree, so a local edit cannot be approved and then embedded. The trees
are compared as sorted path-and-bytes pairs: a built file the commit does not hold, a committed
file the build does not produce, a renamed asset (which is both) and a byte difference are each
refused and named, and a screen asset at or over 2 MiB (2,097,152 bytes) is refused before the
comparison. The size bound is exclusive and applies to both sides, so an asset of exactly
2,097,152 bytes, whether the commit holds it or a rebuild produces it, is refused rather than
passed, while 2,097,151 bytes pass on both sides. The refusal names `make gui-assets` as the
regeneration command.
`internal/dev/ci/gui_drift_test.go` pins each rejection and the pass;
`internal/dev/ci/gui_paths_test.go` pins the decision.

The job never reads the repository's CI-mode variable, so the screen verification is never
skipped by a mode: a manual dispatch runs this job exactly as any other event does.

## The test legs
`make test-part TEST_PART=<n>` runs one leg on its own runner, so the slowest leg sets how long a
full run waits. Parts 1 to 5 name their packages in the Makefile and `rest` is every other
default-tag package. `dev` runs `./cmd/crw-dev/... ./internal/dev/...` with the dev tag.
Each package/tag invocation runs in exactly one leg. `internal/dev/ci` holds
that: it refuses a package named by two parts, a part missing from `TEST_PARTS` (what `rest`
subtracts, so its packages would run again in `rest`), a named path with no tests, a pattern, and a
package of the `dev`-tagged set, which only `dev` runs with the tag. A recipe-level check
also resolves the actual `go test` arguments and verifies their disjoint union against
both invocations in `make test`.

CRW-1584 split the previous `test-rest` into `test-5` (flow cli, scenario and engine),
`test-rest` (remaining default-tag packages), and `test-dev` (the dev-tagged tooling).
The matrix and the local plan use this same split. Parts 1–4 are unchanged.

A runner spends about 55 s before its tests (checkout, toolchain, the one `crw` build), and the leg
compiles its own test binaries before it starts. It then runs a few packages at a time on four CPUs,
in the order a part lists them, so a leg takes about that plus its slowest package, or its packages'
total over four CPUs if that is longer; list a slow package first. Historical measurements before
the CRW-1584 split follow: the legs as rebalanced after the
slow packages' tests ran in parallel, with the hosted job time before (median of six runs on
2026-10-06) and after (median of three runs of the change that moved packages between the parts):

| Leg | Packages | Before | After |
| --- | --- | --- | --- |
| `test-1` | `relay/dagsched`, `relay/cli` | 245 s | 229 s |
| `test-2` | `runtime/install`, `relay/registry`, `relay/hook` | 261 s | 259 s |
| `test-3` | `relay/service`, `contracttest`, `relay/mergeturn`, `relay/supervisor`, `skill`, `relay/managed`, `relay/adapter` | 188.5 s | 246 s |
| `test-4` | `relay/delivery`, `relay/store`, `relay/sync`, `relay/faults`, `relay/dag`, `role`, `pyjson`, `relay/linkage`, `recall`, `relay/routing`, `relay/childcleanup` | 131.5 s | 184 s |
| `test-rest` | the other 77 packages and the `dev`-tagged tests | 246 s | 145 s |

In those measurements, `runtime/install` was the floor of the longest leg: its tests ran one after another for about 175 s, so
the leg that held it took about 260 s; another leg would not then have shortened the longest one.
The packages that do not parallelize well are kept apart for the same
reason: `relay/delivery` leads `test-4` beside the medium packages, and `skill`, `relay/managed` and
`relay/adapter` sit in `test-3` with the middle tier, so no leg holds two of them. To rebalance
again, read the `ok <package> <seconds>` lines and the job times of several hosted runs of one commit
(they differ by 20 s or more from run to run), move packages, and compare medians.

`relay/dagsched` is the largest test package (about 38k lines of tests), and under host load it once
ran past the 10 minute budget of its binary (CRW-1190). Its time went to building a store for every
fixture (the frozen schema, its fence and the DAG zone's DDL, each statement parsed again by every
connection) and to about 160 tests that ran one after another; no test of it sleeps. A fixture without
an App Server socket is now a copy of one prepared store with its takeover mirror re-homed, the stub
verifier carries its helper variables in the script instead of `t.Setenv`, and the tests that write an
executable do so under `syscall.ForkLock`, so 121 more tests run in parallel; the 43 that remain
sequential set the process environment, change the directory or assign a package variable. Measured on one host (load 16 to 25, the test binary of the package, three runs each,
alternating): wall clock 103, 83, 82 s before and 47, 52, 37 s after; user plus system CPU 142, 156, 146 s
before and 114, 128, 117 s after. Under five `nice -n 19` spinners, `make test-part TEST_PART=1` took 69, 75
and 40 s with `relay/dagsched` at 53, 60 and 25 s, against the 600 s of its budget, so the budget stays.

### The `runtime/install` test time

`runtime/install` grew back from the 90 s of CRW-734 to 513 s on a loaded host (CRW-1263), and three
integration runs hit the 10 minute budget while two tests failed on wall-clock limits. The time was
data, not waiting: every test that installs wrote the whole release binary (72 MB) into a fresh
archive and hashed it again for each runtime directory it named, and on a host at load 30 to 40 those
writes held up every small sync of the tests running beside them (a CPU
profile of the package: 171 s of CPU, most of it SHA-256, copying and read/write system calls on those
bytes). Each release archive is now written once per run, read-only under the isolation root, and
linked into the test's own directory with its digest known, so a test pays for the archive it installs
and not for building it. No test, name or assertion was removed, and no product time bound changed.
The two timing failures were wall-clock verdicts and are now decided without one:
`TestFeaturesEnableRecordsVerifiedProbeEvidence` read a record the enable writes under its 8 s
evidence deadline (the product's 8 s, unchanged), which a codex slow to start on a loaded host missed
(marker nil after 44 s); the package now records under a context that no clock ends, through the
`featureEvidenceContext` seam (default: the context ended by the 8 s deadline, which
`TestFeatureEvidenceContextIsBoundedByTheEvidenceDeadline` holds to the product's value). The one
test of the stall, `TestFeaturesEnableIsNotHeldByAStalledEvidenceProbe`, waits until the probe says it
has begun to stall, ends the recording's context itself, and requires the enable to return with no
evidence recorded; the probe sleeps 5 minutes, so an enable that ignores the end fails. A 17 s start
delay of the probe, which failed the earlier retry-with-a-doubled-deadline form, passes.
`TestWriteExecutableSurvivesConcurrentForks` asserted that 320
spawned processes finish within 30 s (51 s under load) and now asserts that no stretch of 60 s passes
without one finishing. Measured on one host with the package's own method (`go test -count=1 -json`
through the heavy-check gate at `nice 12`, `GOFLAGS=-p=4`, the crw binary built once):

| Run | Load1 (start, end) | Package | Slowest test |
| --- | --- | --- | --- |
| before, dev `e537281c6` | 33, 24 | 349 s | 184 s (`TestTheRouteDoesNotCarryAnythingButTheZoneArrivingAlone`) |
| before, as reported in CRW-1263 | 27 to 36 | 513 s | 167 s |
| after | 21, 18 | 65 s | 15 s (`TestFeaturesAndConfigCommandsKeepEveryTomlFormValid`) |
| after, under five `nice -n 19` spinners of six busy threads | 24, 69 | 268 s | 112 s (`TestTheBackupDestinationIsRefusedWhereItCouldNotSurvive`) |

Both packages ran 362 tests with no failure. The timing tests were repeated with `-count=20` under
five `nice -n 19` spinners of eight busy threads, killed afterwards, through the same gate and Go:

| Code | Load1 (start, end) | Passed |
| --- | --- | --- |
| an earlier form of the stall test, which doubled its deadline on each retry | 66, 70 | 60 of 60 (three tests) |
| `13ad71d83`, the stall test ended by the `featureEvidenceContext` seam, spinners warmed 30 s | 33, 43 | 80 of 80 (four tests, the deadline test included) |
| `13ad71d83`, again, spinners warmed until load1 reached 48 | 48, 49 | 80 of 80 |

After `13ad71d83` the code changes only in the failure-path cleanup of `TestFeaturesEnableIsNotHeldByAStalledEvidenceProbe` (it ends the recording and waits for the enable before the test home is removed); the timed assertions and the product code are the ones proven above, and the branch also merged dev. The spinners at `nice 19` do not slow a `nice 12`
test as much as the host's other runs did, so this is a load proof, and the failure itself was seen
only in the unspun runs above (44 s for the evidence test, 51 s for the fork test). The temporary root
is the disk (`TMPDIR` under `/scratch`), never `/dev/shm`; under `crw-dev ci local` the caller's `/scratch` `TMPDIR` is
still the root of the step's own `TMPDIR`.

### The `internal/manage` test time

`go test ./internal/manage/` ran its 1,014 registered tests (1,011 PASS and 3 SKIP) one after another and took 829 s on the shared host
(load 30 to 50 on 20 CPUs, `nice` 12, `-json` log in `/scratch/crw/diag-manage/manage.json`), so the integration's
10 minute bound for a package was close. The time was not CPU (a 79-test slice used 16% of a CPU): a run makes about 15,500 fsyncs and 5,800
processes, and each fsync waits behind the other writers of the disk. Method: read the per-test
`Elapsed` of `go test -json`, take the biggest costs first, count the processes and fsyncs a test makes
(`strace -f -y -e trace=execve,fsync`, test directories are named after the test), change, and measure the whole
package again. Whole-package times (before the memory-backed root of CRW-1262 below),
same host, same command (`go test -count=1`, through `heavy_slot.sh`), `load1` read before and after the command:

| Step | Package time |
| --- | --- |
| before (`TestPumpQueueOverlappingLegacyRecordsKeepTheInvariants` alone took 289 s) | 829 s at load 40 |
| the 375 overlap combinations as parallel subtests | 146 s at load 19 |
| and the pre-merge tests copy a built repository pair | 128 s and 138 s at load 20 to 37 |
| the same, after merging dev at `fc5e5bd87` (1,106 registered tests: 1,103 PASS and 3 SKIP; CRW-692 added 87 of the 92 new ones) | 211 s at load 17 to 25 |
| the same plus this lane's last test (1,107 registered), run 1 | 591 s at load 20 to 25 |
| the same, run 2 | 241 s at load 31 to 18 |

The spread is the disk, not the suite. In run 1 one test, `TestPumpSignalCapacityStateWrittenAgainByteForByteEndsTheProposal`,
took 229 s and its neighbours 17 to 33 s, where run 2 and the earlier runs took under 1 s each: the host's I/O pressure
(`/proc/pressure/io`, `full avg10` 19 to 41) stalled every fsync for minutes. A run on an idle disk is a fraction of
the 10 minute bound; one that meets a stall can come within seconds of it (run 1 ended at 591 s of 600 s).
Measured with the pointer check off, `go test -race -gcflags=all=-d=checkptr=0 -count=1` takes longer than the default
`-timeout` here (it timed out at 600 s once, and with `-timeout 30m` took 1,351 s at load 18 to 20); the race run is a
developer check, not a CI step, so pass a larger `-timeout` to it.

- The overlap combinations (CRW-991) are parallel subtests: each has its own state directory, fake bridge and
  clock, and the parent test sets the homes once, because `t.Setenv` is refused in a parallel test
  (`pumpTestEnvHomeless`). Names, the 375 combinations and every assertion are unchanged. Run it with
  plain `-race` after a change; the relay store's pointer access passes the pointer check, so a test that opens a
  store runs under it.
- `premergeTestNewRepo` built a git remote and a clone with sixteen git commands per test (the pre-merge tests made 2,917 of
  the 5,833 processes of a run). It builds each repository spec once, under a directory the run removes, and copies the
  pair, then sets the copy's origin with `git remote set-url origin`, so a `TMPDIR` whose name holds a backslash, a tab or
  a newline (git writes those escaped in its configuration) still works
  (`TestPremergeTestCopyRepoKeepsOddTemporaryPaths`). The commits carry a fixed identity and date, so the
  copy is the repository the build made.
- An earlier version of this lane put the tests' `TMPDIR` on `/dev/shm` (47 s and 43 s before the dev merge, 52 s after).
  It was removed: a helper process re-run by a test made its own `/dev/shm` root that nothing removed, and a small or
  full tmpfs that passed a probe still failed every fixture write. CRW-1262 brings it back, test-only, with both fixed
  (next section).

#### The memory-backed temporary root (CRW-1262)

The `TestMain` of `internal/manage` (`core_tmpfs_test.go`) runs the package in `crw-manage-tmp-<pid>` under `/dev/shm` when that is fit,
so the roughly 16,000 fsyncs of a run (a stalled disk made one test take 229 s and the package 591 s) do not wait for the disk. The
production store and its fsync are not touched; the tests assert no durability a sync gives. The root is chosen only if the base has at
least 2 GiB and 20,000 inodes free (`statfs`) **and** a script written in it runs; otherwise the tests stay on the `TMPDIR` the caller set (under `crw-dev ci local`, the step's own `TMPDIR`, made below the caller's `/scratch` one). A
test binary a test starts again (the supervisor child, any helper that goes through `TestMain`) reads the root its starter named in
`CRW_MANAGE_TEST_TMPFS_ROOT` (`<pid>:<root>`; honoured only for a live pid and a directory of ours named `crw-manage-tmp-<that pid>`),
uses it, and makes and removes none; a process that chose no tmpfs names that as `<pid>:-`, and a helper of a live such process chooses none either (it would make a root and leave it through `os.Exit`); the process that made the root removes it after `m.Run`. A run that a timeout or a signal killed
leaves its root (it is memory), and the next run removes the `crw-manage-tmp-<pid>` roots whose pid is gone. To see them:
`ls -d /dev/shm/crw-manage-tmp-*`. Pinned by the `TestTmpfsRoot*` tests.

Measured on the shared host with `go test -count=1 ./internal/manage/` through `heavy_slot.sh` (one run each, at the same head apart from this
change; the disk was not stalled, so this is the floor of the gain): 87.8 s on the disk `TMPDIR` at load5 24.7 to 21.8, and 61.9 s in the
memory root at load5 18.6 to 16.8. `strace -f -c -e trace=fsync,fdatasync` on the test binary counts 21,857 `fsync` calls both ways (the
production code still syncs; the calls go to memory), with 7.00 s spent in them on the disk and 0.68 s in memory. A stalled disk
(229 s for one test, 591 s for the package) is the cost the memory root takes out, as the fsyncs no longer wait for it.

## Temporary locations

One package decides every temporary location: `internal/tmploc` (CRW-1566). It holds the one read of
`TMPDIR` and its default, and nothing else in the program chooses a temporary place.

- The temporary directory is `TMPDIR` as written, or `/tmp` (`tmploc.DefaultDir`, source `default`)
  when `TMPDIR` is unset or empty. A `TMPDIR` that is not absolute stops with
  `tmpdir_not_absolute`, and one that cannot be used stops with `temp_dir_unusable`, naming the
  directory, its source and why. Nothing moves to a second place: no site picks `/var/tmp`,
  `/dev/shm` or the work root because the first place is long, unusable or below a `.git`. The default
  `/tmp` is a location only, never a ground for a safety judgement.
- Code that needs a temporary directory or file takes `tmploc.Dir`, `tmploc.MkdirTemp`,
  `tmploc.CreateTemp`, or `tmploc.MkdirTempIn(parent, pattern)` when the parent is a variable;
  `MkdirTempIn` refuses an empty or relative parent (`temp_parent_not_absolute`), so an empty parent
  never means "`TMPDIR`, decided inside `os`". crwconfig's `temp_root` takes the raw value from
  `tmploc.Lookup` and keeps its own order (the environment, then `paths.temp_root`, then the strict
  absolute check) and its own error text.
- Sockets never lie under `TMPDIR`. A test socket lies below a socket folder made under
  `tmploc.SocketRoot` (`/tmp`), a fixed root for every run, and its path is measured against
  `tmploc.SocketTailMax` within the 103-byte bound (`tmploc.SocketBound`); `tmploc.SocketFits` is the
  one budget calculation. `crw-dev ci local` checks the temporary directory and the socket folder
  before any step ([The local run](#the-local-run)).
- A test takes `t.TempDir()`, or `tmploctest.SocketDir`, `tmploctest.SocketPath` and
  `tmploctest.CheckSocket` for a socket.
- A shell script of the plugin or of `scripts/` that makes a temporary directory writes the same rule in one
  form: `TMPBASE="${TMPDIR:-/tmp}"`, then a `case` that stops a relative value with
  `tmpdir_not_absolute: TMPDIR=<value>; set TMPDIR to an absolute directory` on stderr and exit status 2, before
  any directory is made (`diagram-to-html.sh`, `scaffold-audit.sh`; their behaviour is pinned by
  `tmp_shell_sites_test.go`). A document writes the default as `${TMPDIR:-/tmp}`.

`internal/dev/ci/tmp_decisions_test.go` (dev tag) enforces this. It greps every tracked file for the
direct deciders (`os.TempDir`, a quoted `/tmp`, `/var/tmp` or `/dev/shm` literal, `MkdirTemp` or
`CreateTemp` with an empty parent, and a direct read of `TMPDIR`), and every hit must be one of these
exceptions:

- E1 `internal/tmploc` (non-test files): the one place. It holds `DefaultDir`, `SocketRoot` and the
  read of `TMPDIR`, matched by file and exact line text.
- E2 `*_test.go` and `web/test`: test code, pending CRW-1580, which removes this entry.
- E3 `testdata` and `contract/fixtures`: recorded data and the oracle's own programs, not locations
  this program chooses.
- E4 the recorded hook payload line in `internal/contracttest/hook.go` (`cwd`, `transcript_path`), the
  same text as `contract/fixtures/hook`, matched by exact line text.
- E5 the recorded scratch parent of the cxc replay's 9 `path_length_dependent` fixtures
  (`cxccorpus.RecordedScratch`), the recorded oracle condition; it is added with that constant.
- E6 port history prose under `docs/port-cxc` and `docs/port` that quotes code or Python literals, and
  the visualizer's port record, which quotes the original output-directory line its recorded edit replaces,
  listed by file.

A second rule judges every non-test `os.MkdirTemp` and `os.CreateTemp` whose parent is a variable: unless the
parent is `filepath.Dir(...)` or a literal, the call is listed with the reason its parent is never empty or
relative, or it moves to `tmploc.MkdirTempIn`. A third judges the shell: a `${TMPDIR` or `$TMPDIR` read in a
`.sh` file under `plugins/` or `scripts/` must be the one `TMPBASE` line, and its file must hold the whole form
verbatim. An exception or a listed call that no longer matches a line fails the check, so the lists cannot go
stale. There is no list of sites still to move: `TestNoPendingTempDecisions` fails if one returns.

## Leftover isolation trees

Nothing removes a `crw-relay-test-*` directory a run leaves: a test binary killed by `go test -timeout` or by
SIGKILL skips its removal, `CRW_TEST_KEEP_ROOT` keeps them on purpose, and the suite before the helper trees
were nested left about six per full run. To see the ones in `TMPDIR` that are more than a day old (a younger
one may belong to a run in progress), which removes nothing:

```sh
find -H "${TMPDIR:-/tmp}" -mindepth 1 -maxdepth 1 -type d -name 'crw-relay-test-*' -mtime +0
```

To remove them, repairing permissions first because a tree may hold read-only directories, once you have
checked the list and no test run of yours is still going:

```sh
find -H "${TMPDIR:-/tmp}" -mindepth 1 -maxdepth 1 -type d -name 'crw-relay-test-*' -mtime +0 \
  -exec sh -c 'chmod -R u+rwX -- "$@" && rm -rf -- "$@"' sh {} +
```

## Leftover verify trees

A `crw-dev ci local` run removes its trees in a deferred cleanup, so a run killed by SIGKILL or by a timeout
leaves them, and `--keep` keeps them on purpose (it prints each path). Two kinds can remain:

- `crw-ci-local-*`, the run's worktree and home, directly in the work root (default `~/.local/state/crw-ci-local`, or the
  `--work-root` you passed);
- `ci<8 hex>`, the steps' `TMPDIR`, directly in the caller's `TMPDIR` (`/tmp` when it is unset).

Nothing removes them. In the default work root only this command works, so a `crw-ci-local-*` name there is a
run's own. A `--work-root` can name a directory other tools share, and there a name that only looks like these is not
safe to assume. The `ci<8 hex>` name is short and generic, and the caller's `TMPDIR` is often shared (`/tmp`), so the code
never deletes by that name: another tool's directory could match. Read the list before you remove anything. The `ci` pattern
matches the step tree by its exact name, `ci` followed by eight lowercase hex digits, never `ci` and any eight characters,
so a directory such as `cinema` is not listed. The pattern spells out the eight hex digits as character classes, which
`find` reads the same everywhere.

Set `root` to the `--work-root` you passed, if any. To see the ones more than a day old (a younger one may belong to a run in
progress), which removes nothing:

```sh
root="${XDG_STATE_HOME:-$HOME/.local/state}/crw-ci-local"  # or the --work-root you passed
find -H "$root" -mindepth 1 -maxdepth 1 -type d -name 'crw-ci-local-*' -mtime +0
find -H "${TMPDIR:-/tmp}" -mindepth 1 -maxdepth 1 -type d -name 'ci[0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef]' -mtime +0
```

The removal recipe is for the default work root, the one dedicated to this command. With a `--work-root` that other tools
share, remove only the paths the list shows that you have checked are this command's, one by one, and do not run the recipe
over the root. A step's test may leave read-only directories. To remove the ones the lists show in the default root,
repairing permissions first, once you have checked them and no run of yours is still going (use the second line for the
`ci<8 hex>` list only if every name in it is yours):

```sh
root="${XDG_STATE_HOME:-$HOME/.local/state}/crw-ci-local"
find -H "$root" -mindepth 1 -maxdepth 1 -type d -name 'crw-ci-local-*' -mtime +0 \
  -exec sh -c 'chmod -R u+rwX -- "$@" && rm -rf -- "$@"' sh {} +
find -H "${TMPDIR:-/tmp}" -mindepth 1 -maxdepth 1 -type d -name 'ci[0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef][0123456789abcdef]' -mtime +0 \
  -exec sh -c 'chmod -R u+rwX -- "$@" && rm -rf -- "$@"' sh {} +
```

A killed run's worktree may still be registered in the checkout it came from; `git worktree prune` there drops the
registrations whose directory is gone.

## Plugin package

`crw-dev ci plugin` reads `.agents/plugins/marketplace.json` and the manifest under
`plugins/crw/.codex-plugin/`, then rebuilds the release payload from a Git revision with
`git ls-tree` and `git cat-file` rather than from the working tree: that revision is the oracle.
It refuses what installs silently wrong. Installation copies the plugin root verbatim, so a
symlink inside it (dropped), an untracked or ignored file (published), an empty directory,
anything outside `.codex-plugin/`, the declared components and `LICENSE`, operational state,
credential names and a personal home path in shipped instructions are all rejected; a hook
timeout over 20 seconds (the longest K1 timeout) is refused; each declared
skill must carry `SKILL.md` and `agents/openai.yaml`, and the manifest may carry only keys the
ingestion validator knows, in the shapes it accepts. The working tree is checked with the same
rules, so source edits are checked before they become a committed packaging input.

The committed manifest names the fixed release (CRW-1582); the check requires that fixed source
version, and the derived version - the release plus the payload's digest suffix - is what
`--stage <new-directory>` stamps into a generated local marketplace. `--payload <dir>` validates
an installed or staged directory's stamped suffix against its bytes, and `--record-version` is
retired: it refuses and writes nothing. `--json` prints the digest and the namespaced skill
names. Passing is evidence about this source, not that a plugin installed or a skill loaded on a
host; see [plugin packaging](plugin-packaging.md#the-version-names-the-payload).

## Scope of the checks

All of this is evidence about this source. It is not evidence about an installed runtime, an App
Server socket, an MCP registration or delivery on any host; those remain separate operations with
their own authorization. The installer tests use temporary destinations and leave the user's
installed skills alone. The validator is repository-owned structural validation, not the Codex
validator or proof of instruction quality: its metadata checker reads this repository's
single-line frontmatter and interface strings (not general YAML), and its link check covers inline
local paths outside code (fenced, indented, code spans) and raw HTML blocks, and the `#fragment` of a link in `plugins/crw/skills` and `docs` (not remote URLs, reference-style links or the fragments of other documents).
The ordinary CI environment has no Linear credentials, CXC installation, App Server or private
runtime state.

## Secret scanning

The Linux scanner launcher pins Gitleaks 8.30.1 and verifies the downloaded archive's SHA256. It
scans the Git database from a temporary directory with an explicit configuration and an empty
ignore file; inline allow comments and repository ignore files do not suppress findings. Fetch
complete history: a shallow checkout cannot establish full-history coverage, and network,
download or checksum failures fail the scan. No broad allowlist is supplied.

What is scanned follows the event. A manual dispatch has no base to compare with, so the script
scans every fetched ref (`--all -m`), and a finding on any branch fails the run. Integration does not
depend on a hosted run at all: the integrator scans locally before the push, over the commits the
push adds, with the same pinned scanner. A scheduled scan of every remote branch is recorded in the
[refactor backlog](port/refactor-backlog.md).

Secret scanning is not proof that every private fact or credential was detected. Review fixtures
and publication history separately. A change can alter the scanner and workflow, so review those
changes as changes to the gate itself. Do not upload secret-bearing findings; keep output
redacted and repair with the owner.

## Large blobs

This repository integrates with merge commits, so every blob of every commit on a branch
becomes part of `dev`'s public history, and a public history cannot drop it again: deleting or
shrinking the file in a later commit leaves the blob behind. `crw-dev ci validate` therefore refuses
any blob over 2 MiB (2,097,152 bytes, measured uncompressed) that the range under judgment brings
into the history, unless [`.large-blob-allowlist.json`](../.large-blob-allowlist.json) names its path
with a ceiling that covers it.

The range follows the event, and `validate` learns it from two variables (its workflow step has no
flag, as the secrets script has none). A manual dispatch has no base, so every blob reachable from
`HEAD` is judged: stricter, never weaker. The job fetches full history, and a shallow checkout is
refused, because its boundary commit's whole tree would look new. To check a branch before pushing
it, run `BLOB_RANGE_BASE=origin/dev go run -tags dev ./cmd/crw-dev ci validate`; without the
variable the whole history of the branch is judged. A repository with no commit has nothing to judge.

A blob is new when `HEAD` reaches it and the base does not: a blob the base's history held and dropped
before the base is not new again. The refusal names each offending path, the exact size, the commit that
brought the blob first and that commit's subject. It then says how to shrink the file (regenerate a
generated input deterministically in the test that uses it; check a large record by its hash and
count), how to name it in the allow list, and how to rebuild the branch from its base: a new branch
from the base, `git merge --squash` of the old branch, the file shrunk or removed, a commit, a push of
the new branch and, where the change is an external contribution, a replacement pull request with
the old one closed unmerged, so that no commit that carries the blob reaches `dev`.

The allow list starts empty (`{"entries": []}`). An entry is `{"path": ..., "max_bytes": ...,
"reason": ...}`: the exact repository path as git records it, a ceiling above 2 MiB and the reason the
file has to be committed. The file is read from the working tree and strictly: unknown keys, duplicate
or unclean paths, an empty reason and a ceiling that is not an integer above the limit are refused. Keep
an entry for as long as its blob is in the history, because a run that judges the whole history (a local
run, a manual dispatch) judges the blob again. The list is part of the change, so a pull request can add
a file and its own entry; review the entry's reason as the gate itself is reviewed.

Limits. The size is the object's uncompressed size. `dev`'s history cannot be rewritten, so
shrinking the file in a follow-up changes only its later versions: the old blob stays reachable, and
a run that judges the whole history keeps seeing it until the allow list names it with a reason.
Files no commit holds yet are not judged.


## Activation

Use `dev` as default. `main` is a release mirror, advanced by the
owner-authorized [Release workflow](releases.md) to the exact verified dev SHA. There is no
promotion PR or release-specific CI gate.

[The parent](../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels) (repository session) changes the branch protection when the push-only procedure is switched on:
the `dev` ruleset stops requiring a pull request and stops requiring a status check, while
deletion and force-push stay disallowed. Read back the effective rules before claiming the
protection is active; never loosen a protection merely to land a change.

| Setting | `dev` | `main` |
| --- | --- | --- |
| PR required | No | No; release workflow advances the ref |
| Required status check | None | None |
| Deletion | Disallowed | Disallowed |
| Force push | Disallowed | Disallowed |
| Required human approvals | 0 | Not a PR workflow |
| Update method | Non-forced fast-forward by the integrator | Non-forced fast-forward |

Protect version tags (`v*`) against update, deletion and non-fast-forward with no bypass actors.
The main rules protect ancestry, while owner-only workflow checks govern its release route;
they do not prevent a repository administrator from changing policy or using other authorized
write credentials. Read before and after configuration:

```sh
gh api repos/OWNER/REPO/rulesets
gh api repos/OWNER/REPO/rules/branches/dev
gh api repos/OWNER/REPO/rules/branches/main
gh run list --repo OWNER/REPO --workflow ci.yml --json databaseId,event,headSha,conclusion
```

Checked-in rules are not proof of server enforcement. If protection or release credentials are
missing, state the exact remaining prerequisite. Policy activation does not authorize publishing a
release, installation or deployment.

## Public repository activation

This is separate from branch and CI activation and requires explicit owner authority. When
private history contains operational receipts, keep the private repository and publish a reviewed
source snapshot into a new one: do not transfer old Git objects, PR comments, logs or refs, and do
not mirror-push. Before switching the destination public, review every ref it exposes, the source,
attribution, license, issues, comments and their edit histories, Actions logs, artifacts and other
publication surfaces, and verify that public Linear linkbacks cannot copy private descriptions.
Secret scanning alone does not establish privacy.

Enable private vulnerability reporting as part of the visibility operation and require
`enabled: true`; if GitHub needs public visibility first, apply it immediately after the change
and verify it before announcing availability:

```sh
gh api --method PUT repos/OWNER/REPO/private-vulnerability-reporting
gh api repos/OWNER/REPO/private-vulnerability-reporting
```

Read back visibility after any ambiguous API failure before retrying. Afterwards verify
unauthenticated source access, MIT license detection, default `dev`, both branch rulesets and the
Security page's private reporting. Publishing a repository does not authorize a source release, a
tag, a package release or runtime activation.

## Adaptation sources

The policy and CI structure were adapted from Jun's Lina checkout at
`522101ff99e356b0ea6d27b4ea03ec7e599ee4b3` (`POLICY.md`, `docs/CI.md`, the CI workflow and
`scripts/ci/secrets.sh`), without Lina's builds, dependencies, deployment or personal data. The
bridge's provenance is in [its PROVENANCE.md](../packages/codex-thread-bridge/PROVENANCE.md).
GitHub's [workflow dispatch reference](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#workflow_dispatch)
and [secure use reference](https://docs.github.com/en/actions/reference/security/secure-use)
cover manual execution and pinned Actions; the scanner is
[Gitleaks 8.30.1](https://github.com/gitleaks/gitleaks/releases/tag/v8.30.1).
