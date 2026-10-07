# CI operation

[POLICY.md](../POLICY.md) owns the rules. This page maps them to commands and the steps that
activate GitHub enforcement. It is developer-only: every check runs from a checkout.
Installing and operating the runtime is [runtime installation](runtime-install.md).

Local full verification is the primary route: `crw-dev ci local` runs every job and step below
in a clean worktree of the commit being verified and writes a `verification-record/1`, whose
`result` is a pass only when every step succeeded. GitHub Actions is an optional remote run: the
workflow starts only on a manual dispatch, and integration does not wait on it.

## Checks

| Command | Where CI runs it |
| --- | --- |
| `crw-dev ci validate` | `validate`: skill metadata, local Markdown links, and that Python sits only in skill assets: a `.py` file or a python-shebang script, tracked or untracked and not ignored, fails it unless it is below `<skill>/scripts/` or `<skill>/examples/` of `plugins/crw/skills` or `port/cxc/skills` (`TestTrackedPythonStaysInSkillAssets` holds the tracked files for `make test`); CI installs no Python and runs no skill script (`TestWorkflow_installs_no_python`); and that no blob over 2 MiB comes into the history unless the allow list names it ([large blobs](#large-blobs)) |
| `crw-dev ci plugin` | `validate`: plugin package shape, payload hygiene and the recorded version digest ([below](#plugin-package)) |
| `crw-dev ci contracts` | `validate`: the offline contract checks built into `crw-dev`: the hook replay, the operations shape check (`crw-dev ci operations`), the component definition, the start-policy self-test and the parent-title replay |
| `crw-dev ci refactor-backlog` | `validate`: the generated refactor backlog: assembles `docs/port/refactor-backlog.md` from the fragments under `docs/port/refactor-backlog.d` and refuses when the committed file differs from the fragments (`--write` regenerates it) |
| `bash scripts/ci/secrets.sh` | `secrets`: checksum-pinned Gitleaks scan: on a manual dispatch every fetched ref ([scope](#secret-scanning)) |
| `node --test port/cxc/skills/*/tests/*.test.mjs` | `skill-scripts-node`: the staged skills' own Node tests, on Node 24.20.0, only when a staged skill path changed; a run that skips them is success |
| `npm ci`, `npm test`, `npm run build -- --outDir "$RUNNER_TEMP/gui-built" --emptyOutDir`, `crw-dev ci gui-drift --built "$RUNNER_TEMP/gui-built"` | `gui`: the screens under `web/` build and match the committed `internal/gui/assets` tree byte for byte, on Node 24.20.0, only when a watched path changed ([below](#the-gui-job)); `make gui` runs the same three commands locally |
| `make lint` | `go-product` leg `lint`: vet (also of the `dev` and `integration` tagged packages), staticcheck and gofmt |
| `make test-part TEST_PART=<n>` | `go-product` legs `test-<n>` and `test-rest`: the Go tests and the contract corpus; together the parts are `make test` |
| `CGO_ENABLED=0 make dist` per target | `go-product` leg `dist`: static `crw` for linux/amd64, linux/arm64 and darwin/arm64, uploaded with `SHA256SUMS` |
| `CRW_TEST_BINARY=<crw> go test -tags integration ./internal/runtime/integration/...` | `dist`: installs release archives of the linux/amd64 binary it built (and a relinked second one) into an isolated home with `crw install` and runs the plugin's declared Stop hook and MCP server against them (IS-1, IS-7, IS-8) |

`crw-dev` is the development binary (`make crw-dev`); it builds only with the `dev` tag, so
`make dist` and the release archives never contain it. Its checks have no Python twin: the copies
under `scripts/ci` and `scripts/check_operations_contract.py` left in refactor R3 (decision R3R-1).
`crw-dev ci local` runs this same table locally and records the result as a
`verification-record/1` (CRW-964's command and format, documented in its own section).
See the [workflow](../.github/workflows/ci.yml) for the exact job inputs.

## The workflow

`ci.yml` starts only on `workflow_dispatch`: a person asks for the full hosted
verification of a commit. Integration no longer runs through a hosted event, so a
dispatch run is a developer's remote check and never integration or release
evidence. The job bodies are the ones `crw-dev ci local` runs locally (the local full
verification CRW-964 adds), so a dispatch exercises the same checks.

Two jobs gate themselves on changed paths. `skill-scripts-node` runs the staged skills' Node
tests only when a staged skill path changed, and ends successfully without installing Node when
none did. `gui` runs the screen verification only when `web/`, `internal/gui/assets/` or the gui
definition changed ([the gui job](#the-gui-job)); it always exists, `dev-gate` waits on it, and
anything its decision cannot read selects the full run.

`validate`, `secrets` and the `go-product` legs start at once and run on separate runners.
`make test` builds one `crw` for the run (`dist/test/crw`, release-shaped with `-trimpath`) and
passes it as `CRW_TEST_BINARY`, so no package links its own; `internal/testsupport` `CRW` builds
one per test process when the variable is unset, and `CRWDevPath` and `BuildCRW` build `crw-dev` and
the seam builds a few tests need (a link-time clock, a build tag, an overlay) the same way. Every
package's `TestMain` is `testsupport.Main`, which points the homes, the XDG directories and the
relay's roots at one temporary tree (`crw-relay-test-*` in `TMPDIR`), keeps `CRW_REFUSE_LIVE_STATE=1`, and
removes the tree and every binary the process built after the tests. A helper process a test starts from
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
full run waits. Parts 1 to 4 name their packages in the Makefile and `rest` is every other
package plus the `dev`-tagged tests, so a package runs in exactly one leg. `internal/dev/ci` holds
that: it refuses a package named by two parts, a part missing from `TEST_PARTS` (what `rest`
subtracts, so its packages would run again in `rest`), a named path with no tests, a pattern, and a
package of the `dev`-tagged set, which only `rest` runs, with the tag.

A runner spends about 55 s before its tests (checkout, toolchain, the one `crw` build), and the leg
compiles its own test binaries before it starts. It then runs a few packages at a time on four CPUs,
in the order a part lists them, so a leg takes about that plus its slowest package, or its packages'
total over four CPUs if that is longer; list a slow package first. The legs as rebalanced after the
slow packages' tests ran in parallel, with the hosted job time before (median of six runs on
2026-10-06) and after (median of three runs of the change that moved packages between the parts):

| Leg | Packages | Before | After |
| --- | --- | --- | --- |
| `test-1` | `relay/dagsched`, `relay/cli` | 245 s | 229 s |
| `test-2` | `runtime/install`, `relay/registry`, `relay/hook` | 261 s | 259 s |
| `test-3` | `relay/service`, `contracttest`, `relay/mergeturn`, `relay/supervisor`, `skill`, `relay/managed`, `relay/adapter` | 188.5 s | 246 s |
| `test-4` | `relay/delivery`, `relay/store`, `relay/sync`, `relay/faults`, `relay/dag`, `role`, `pyjson`, `relay/linkage`, `recall`, `relay/routing`, `relay/childcleanup` | 131.5 s | 184 s |
| `test-rest` | the other 77 packages and the `dev`-tagged tests | 246 s | 145 s |

`runtime/install` is the floor of the longest leg: its tests run one after another for about 175 s, so
the leg that holds it takes about 260 s however the others are split, and a sixth leg would not
shorten the longest one. The packages that do not parallelize well are kept apart for the same
reason: `relay/delivery` leads `test-4` beside the medium packages, and `skill`, `relay/managed` and
`relay/adapter` sit in `test-3` with the middle tier, so no leg holds two of them. To rebalance
again, read the `ok <package> <seconds>` lines and the job times of several hosted runs of one commit
(they differ by 20 s or more from run to run), move packages, and compare medians.

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

## Plugin package

`crw-dev ci plugin` reads `.agents/plugins/marketplace.json` and the manifest under
`plugins/crw/.codex-plugin/`, then rebuilds the release payload from a Git revision with
`git ls-tree` and `git cat-file` rather than from the working tree: that revision is the oracle.
It refuses what installs silently wrong. Installation copies the plugin root verbatim, so a
symlink inside it (dropped), an untracked or ignored file (published), an empty directory,
anything outside `.codex-plugin/`, the declared components and `LICENSE`, operational state,
credential names and a personal home path in shipped instructions are all rejected; each declared
skill must carry `SKILL.md` and `agents/openai.yaml`, and the manifest may carry only keys the
ingestion validator knows, in the shapes it accepts. The working tree is checked with the same
rules, because a local marketplace installs it.

The manifest version carries the payload's digest as build metadata, `0.4.0+640ccf7eadf4`. The
check re-derives that suffix from the bytes and rejects a stale one, naming the value to record,
for the release payload, the working tree and, with `--payload <dir>`, an installed cache
directory. `--record-version` writes the derived suffix into the working-tree manifest, and
`--json` prints the digest and the namespaced skill names. Passing is evidence about this source,
not that a plugin installed or a skill loaded on a host; see
[plugin packaging](plugin-packaging.md#the-version-names-the-payload).

## Scope of the checks

All of this is evidence about this source. It is not evidence about an installed runtime, an App
Server socket, an MCP registration or delivery on any host; those remain separate operations with
their own authorization. The installer tests use temporary destinations and leave the user's
installed skills alone. The validator is repository-owned structural validation, not the Codex
validator or proof of instruction quality: its metadata checker reads this repository's
single-line frontmatter and interface strings (not general YAML), and its link check covers inline
local paths outside fenced examples, not remote URLs, reference-style links or heading fragments.
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

The management session changes the branch protection when the push-only procedure is switched on:
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
