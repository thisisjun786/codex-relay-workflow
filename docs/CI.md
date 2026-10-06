# CI operation

[POLICY.md](../POLICY.md) owns the rules. This page maps them to commands and the steps that
activate GitHub enforcement. It is developer-only: every check runs from a checkout.
Installing and operating the runtime is [runtime installation](runtime-install.md).

## Checks

| Command | Where CI runs it |
| --- | --- |
| `crw-dev ci validate` | `validate`: skill metadata, local Markdown links, and that Python sits only in skill assets: a `.py` file or a python-shebang script, tracked or untracked and not ignored, fails it unless it is below `<skill>/scripts/` or `<skill>/examples/` of `plugins/crw/skills` or `port/cxc/skills` (`TestTrackedPythonStaysInSkillAssets` holds the tracked files for `make test`); CI installs no Python and runs no skill script (`TestWorkflow_installs_no_python`); and that no blob over 2 MiB comes into the history unless the allow list names it ([large blobs](#large-blobs)) |
| `crw-dev ci plugin` | `validate`: plugin package shape, payload hygiene and the recorded version digest ([below](#plugin-package)) |
| `crw-dev ci contracts` | `validate`: the offline contract checks built into `crw-dev`: the hook replay, the operations shape check (`crw-dev ci operations`), the component definition, the start-policy self-test and the parent-title replay |
| `crw-dev ci refactor-backlog` | `validate`: the generated refactor backlog: assembles `docs/port/refactor-backlog.md` from the fragments under `docs/port/refactor-backlog.d` and refuses when the committed file differs from the fragments (`--write` regenerates it) |
| `bash scripts/ci/secrets.sh` | `secrets`: checksum-pinned Gitleaks scan: the commits a pull request adds to its base on a pull request, all fetched history on any other event ([scope](#secret-scanning)) |
| `node --test port/cxc/skills/*/tests/*.test.mjs` | `skill-scripts-node`: the staged skills' own Node tests, on Node 24.20.0, only when a staged skill path changed; a run that skips them is success |
| `make lint` | `go-product` leg `lint`: vet (also of the `dev` and `integration` tagged packages), staticcheck and gofmt |
| `make test-part TEST_PART=<n>` | `go-product` legs `test-<n>` and `test-rest`: the Go tests and the contract corpus; together the parts are `make test` |
| `CGO_ENABLED=0 make dist` per target | `go-product` leg `dist`: static `crw` for linux/amd64, linux/arm64 and darwin/arm64, uploaded with `SHA256SUMS` |
| `CRW_TEST_BINARY=<crw> go test -tags integration ./internal/runtime/integration/...` | `dist`: installs release archives of the linux/amd64 binary it built (and a relinked second one) into an isolated home with `crw install` and runs the plugin's declared Stop hook and MCP server against them (IS-1, IS-7, IS-8) |

`crw-dev` is the development binary (`make crw-dev`); it builds only with the `dev` tag, so
`make dist` and the release archives never contain it. Its checks have no Python twin: the copies
under `scripts/ci` and `scripts/check_operations_contract.py` left in refactor R3 (decision R3R-1).
See the [workflow](../.github/workflows/ci.yml) for the exact job inputs.

## The workflow

Every job runs on every event: a pull request (GitHub's merge candidate), a push to `dev` (the
integrated commit, the evidence a release needs) and a manual dispatch (which is not release
evidence). There is no path selection, except [the temporary light mode](#the-temporary-light-mode)
below. The Go product legs always ran whatever changed, so
selecting the rest by changed paths saved little and put a job before every other one.

The second exception is `skill-scripts-node`'s own path gate: it runs the staged skills' Node
tests only when a staged skill path changed, and ends successfully without installing Node
when none did.

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

`dev-gate` is the one required check. It runs after every other job, `if: always()`, so a failed
or cancelled prerequisite still produces a failing gate rather than a skipped one. The workflow
writes `toJSON(needs)` into a file in the step's own script (a quoted heredoc: the text is never
expanded by the shell and never travels in the environment, whose size the runner bounds) and
the gate passes only when that object names at least one job and every job's `result` is
`success`: a failure, a cancellation, a skip or anything unreadable fails it. It also fails a pull
request whose base is `main`. `internal/dev/ci`'s workflow tests hold the structure: `dev-gate`
needs exactly the other jobs, no job continues on error, every action is pinned by commit, every
Makefile part has a leg and every package runs in exactly one ([the test legs](#the-test-legs)), the
integration step runs in `dist` after the build, and the gate's script, run with a written results
file, passes only an all-success run.

Until wave R1 of the post-port refactoring a `selection` job classified the changed paths
(`crw-dev ci scope`) and the gate (`crw-dev ci gate`) re-checked that selection; a `tests` job ran
the Python twins' own tests on two Python versions. The selection's changed-path list reached
the gate in one environment variable, which made the gate fail with "Argument list too long" once
a pull request changed about a thousand paths. All three left; the release workflow's tests
became Go tests (`internal/contracttest` `TestReleaseWorkflow_*`), and the twins were compared with
their Go checks by `internal/dev/ci` until refactor R3 deleted them.

During iteration run the affected tests and reuse valid evidence for unchanged source, criteria
and environments. CI concurrency cancels obsolete runs within the same PR or branch. An
interrupted dev push is not release evidence: rerun that exact push run if the owner later
chooses its commit.

## The body-only edit mirror

No job reads a pull request's title or body, but a title or body edit fires the `edited` trigger
again and used to rerun all eleven jobs on the same commit. Such a run now mirrors what the
head has already proved.

An `edited` event whose base did not change (`github.event.action == 'edited' &&
!github.event.changes.base`) joins the pull request's own concurrency group, so it waits behind a
running run instead of cancelling it, and a later push cancels it in turn. Every other event,
a retarget included, still cancels obsolete runs.

`validate`, `secrets`, `skill-scripts-node` and each `go-product` leg then run `scripts/ci/edit_mirror.sh` as their first step,
and only on such an edit. The script reads, with `gh api`, the newest created run of this workflow,
of this pull request, of this repository, for the same `head_sha`, other than the run it is in, and
mirrors the job when that run's same-named job's newest attempt concluded `success`. Creation order
is the run's `run_number`, and the larger `id` when two runs share one: those two values are what a
rerun leaves alone. `run_started_at` is deliberately not the order, because rerunning only the
failed jobs moves it forward and would let an older run outrank a newer one whose same-named job
failed. It answers `mirrored=true` with the run id in its step output and its step summary, or
`mirrored=false`.

Every later step of those jobs carries `steps.mirror.outputs.mirrored != 'true'`, joined with any
condition the step already had. The full checkout is one of them, and the sparse checkout of
`scripts/ci` above the mirror is what has to exist before the script can decide. A mirrored job
succeeds without running its steps, and nothing is skipped at job level: GitHub reports a skipped
job's check as success, so a skipped `dev-gate` could hide an earlier red run.

The lookup never fails the job, and an older run is never consulted: the newest created run alone
answers for the job. No candidate, a failure, a cancellation, a skip, a missing job,
another head, workflow or repository, an unreadable API and the run itself all answer
`mirrored=false`, and the job runs in full. `dev-gate`, the job names and the required check are
unchanged, and the four jobs add only `actions: read` to the workflow's `contents: read`, which is
what reading the runs and jobs endpoints needs.

Mirroring is safe because it repeats a result this head already has. `dev` is strict, so a merge
candidate contains dev's tip, and the same `head_sha` is the same tree: the run being mirrored is a
run of the same pull request's own head, never another tree's.

## The temporary light mode

Until the porting and improvement projects finish, the repository variable `CRW_CI_MODE` can be
set to `light` by the repository owners alone. While it is, a pull request run that does not
carry the `crw-lane` label skips the work of the five `go-product` test legs. Child pull request
pushes are most of the concurrent Actions jobs and the merge lane waits for runners behind them;
the lane's local `make test` was measured too slow to stand in for a runner, so the full run is
moved to the one event that needs it. Reverting the mode is deleting the variable, and Jun
decides when it ends.

The condition is one job-level `env` on `go-product`, `CRW_LIGHT_LEG`, holding
`github.event_name == 'pull_request' && vars.CRW_CI_MODE == 'light' &&
!contains(github.event.pull_request.labels.*.name, 'crw-lane') && startsWith(matrix.part, 'test-')`.
A dev push and a manual dispatch fail the first term, a labeled pull request fails the third, and
`lint` and `dist` fail the fourth, so only the five test legs of an unlabeled pull request can
be light. `validate`, `secrets`, `lint` and `dist` always run in full, and so does every
event other than an unlabeled pull request, whatever the variable says.

A light leg keeps its own name and its own success. Its first step writes
`light mode: this leg's tests run in full when the merge lane labels the pull request crw-lane`
to the step summary, and every other step carries `env.CRW_LIGHT_LEG != 'true'` joined with the
condition it already had, the mirror steps included. The guard is a step condition and never a
job-level `if`, because GitHub reports a skipped job's check as success and a skipped
`dev-gate` could hide an earlier red run; no check name moves and no job opts out of its result.

`pull_request.types` gains `labeled` after its five earlier types, so when the merge lane
labels the pull request the labeled event starts a full run on that head, and every later push
while the label stays runs in full too. The concurrency expression is unchanged: a labeled run is
not a body-only edit, so it joins the pull request's main group with `cancel-in-progress: true`
and cancels the light run still in progress. Adding any other label also starts a run; this
repository uses no other label.

[The body-only edit mirror](#the-body-only-edit-mirror) refuses to carry a light leg forward. A
`go-product` test leg is mirrored only when the chosen run's same-named job concluded `success`
and that job's step `Test and replay the contract corpus (<part>)` also concluded `success`.
A skipped or missing test step answers `mirrored=false` and the leg runs in full, so a body edit
right after the label, or after the variable is cleared, cannot replace a full run with an
untested one. `validate`, `secrets` and the `lint` and `dist` legs keep mirroring on the
job's conclusion alone, as they did before.

The merge evidence is still the hosted `dev-gate`, and the lane's local `make test` is not
evidence. While the variable is `light`, a green `dev-gate` of a run without the `crw-lane`
label is not merge evidence, because that run's test legs did not run their tests: the evidence
is a run of the same head, started after the label was added, that finished in success. The lane
adds `crw-lane` when it takes its turn, before it refreshes the base, and removes it when it
returns the turn without merging.

## The test legs
`make test-part TEST_PART=<n>` runs one leg on its own runner, so the slowest leg sets how long a
pull request waits. Parts 1 to 4 name their packages in the Makefile and `rest` is every other
package plus the `dev`-tagged tests, so a package runs in exactly one leg. `internal/dev/ci` holds
that: it refuses a package named by two parts, a part missing from `TEST_PARTS` (what `rest`
subtracts, so its packages would run again in `rest`), a named path with no tests, a pattern, and a
package of the `dev`-tagged set, which only `rest` runs, with the tag.

A runner spends about 55 s before its tests (checkout, toolchain, the one `crw` build), and the leg
compiles its own test binaries before it starts. It then runs a few packages at a time on four CPUs,
in the order a part lists them, so a leg takes about that plus its slowest package, or its packages'
total over four CPUs if that is longer; list a slow package first. The legs as rebalanced after the
slow packages' tests ran in parallel, with the hosted job time before (median of six dev push runs on
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

What is scanned follows the event. On a `pull_request` run the workflow passes the event's base tip
as `PR_BASE_SHA`, and the script scans the range from it to the checked-out merge candidate, still
with merge-parent diffs (`-m`): the commits the pull request adds to its base. A finding that sits
only on another branch does not fail the pull request; it still fails the runs that scan every ref.
A pull request run whose `PR_BASE_SHA` is missing, is not a full hex SHA or is not in the checkout is
refused before the scanner is downloaded, so a wiring fault cannot change the scope unnoticed. Every
other event (a push to `dev`, a manual dispatch, a run outside Actions) scans every fetched ref
(`--all -m`), so what reaches `dev` is checked in full and a finding on any branch fails those runs.
A pull request run reads no other branch, so a branch without a pull request is checked only by the
runs that scan every ref (a push to `dev`, a manual dispatch); a scheduled scan of every remote
branch is recorded in the [refactor backlog](port/refactor-backlog.md).

Secret scanning is not proof that every private fact or credential was detected. Review fixtures
and publication history separately. A PR can change the scanner and workflow, so review those
changes as changes to the gate itself. Do not upload secret-bearing findings; keep output
redacted and repair with the owner.

## Large blobs

This repository merges pull requests with merge commits, so every blob of every commit on a branch
becomes part of `dev`'s public history, and a public history cannot drop it again: deleting or
shrinking the file in a later commit leaves the blob behind. `crw-dev ci validate` therefore refuses
any blob over 2 MiB (2,097,152 bytes, measured uncompressed) that the range under judgment brings
into the history, unless [`.large-blob-allowlist.json`](../.large-blob-allowlist.json) names its path
with a ceiling that covers it.

The range follows the event, and `validate` learns it from two variables (its workflow step has no
flag, as the secrets script has none). On a `pull_request` run the workflow passes the event's base tip
as `BLOB_RANGE_BASE` and the check judges `base..HEAD`: the commits the pull request adds,
intermediate commits included, so a blob that a later commit deletes is still refused. A pull request
run without a usable base is refused, as the secrets scan refuses it. On a push to `dev` the variable
carries the commit the push replaced, and the check judges the commits the push adds. With no base (a
manual dispatch, a push that created the branch, a base that does not resolve, a local run) every blob
reachable from `HEAD` is judged: stricter, never weaker. The job fetches full history, and a shallow
checkout is refused, because its boundary commit's whole tree would look new. To check a branch before
pushing it, run `BLOB_RANGE_BASE=origin/dev go run -tags dev ./cmd/crw-dev ci validate`; without the
variable the whole history of the branch is judged. A repository with no commit has nothing to judge.

A blob is new when `HEAD` reaches it and the base does not: a blob the base's history held and dropped
before the base is not new again. The refusal names each offending path, the exact size, the commit that
brought the blob first and that commit's subject. It then says how to shrink the file (regenerate a
generated input deterministically in the test that uses it; check a large record by its hash and
count), how to name it in the allow list, and how to rebuild the branch from its base: a new branch
from the base, `git merge --squash` of the old branch, the file shrunk or removed, a commit, a push of
the new branch and a replacement pull request with the old one closed unmerged, so that no commit that
carries the blob reaches `dev`.

The allow list starts empty (`{"entries": []}`). An entry is `{"path": ..., "max_bytes": ...,
"reason": ...}`: the exact repository path as git records it, a ceiling above 2 MiB and the reason the
file has to be committed. The file is read from the working tree and strictly: unknown keys, duplicate
or unclean paths, an empty reason and a ceiling that is not an integer above the limit are refused. Keep
an entry for as long as its blob is in the history, because a run that judges the whole history (a local
run, a manual dispatch) judges the blob again. The list is part of the change, so a pull request can add
a file and its own entry; review the entry's reason as the gate itself is reviewed.

Limits. The size is the object's uncompressed size. On a push to `dev` the blob is already public when
the check runs; the failure is the report. `dev`'s history cannot be rewritten, so shrinking the file
in a follow-up changes only its later versions: the old blob stays reachable, and a run that judges the
whole history keeps seeing it until the allow list names it with a reason. `base.sha` is the base tip at
the time of the event, so commits that reached `dev` between that moment and the run count as the pull
request's own, as in the secrets scan. Files no commit holds yet are not judged.


## Activation

Use `dev` as default and the normal PR target. `main` is a release mirror, advanced by the
owner-authorized [Release workflow](releases.md) to the exact verified dev SHA. There is no
promotion PR or release-specific CI gate.

Land CI and policy changes through the existing protected dev PR route first. Confirm that the new
PR gate and the merged dev-push gate both actually succeeded, then apply the authorized protection
changes and read back the effective rules; never loosen a current gate merely to land its
replacement. The required check keeps its name, `dev-gate`, through the changes above.

| Setting | `dev` | `main` |
| --- | --- | --- |
| PR required | Yes | No; release workflow advances the ref |
| Required check | `dev-gate` | `dev-gate` from the selected commit |
| Strict current-base requirement | Yes | No; unchanged verified commit is fast-forwarded |
| Required human approvals | 0 | Not a PR workflow |
| Resolved PR conversations | Yes | Not applicable |
| Force push, deletion and bypass | Disallowed | Disallowed |
| Update method | Merge commit through PR | Non-forced fast-forward |

Require stale-review dismissal on dev and bind checks to the observed GitHub Actions producer.
Protect version tags (`v*`) against update, deletion and non-fast-forward with no bypass actors.
The main rules protect ancestry and CI, while owner-only workflow checks govern its release route;
they do not prevent a repository administrator from changing policy or using other authorized
write credentials. Read before and after configuration:

```sh
gh api repos/OWNER/REPO/rulesets
gh api repos/OWNER/REPO/rules/branches/dev
gh api repos/OWNER/REPO/rules/branches/main
gh pr checks PR_NUMBER --repo OWNER/REPO
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
GitHub's [PR event reference](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request)
and [secure use reference](https://docs.github.com/en/actions/reference/security/secure-use)
cover merge-candidate execution, pinned Actions and untrusted PR input; the scanner is
[Gitleaks 8.30.1](https://github.com/gitleaks/gitleaks/releases/tag/v8.30.1).
