# CI operation

[POLICY.md](../POLICY.md) owns the rules. This page maps them to commands and the steps that
activate GitHub enforcement. It is developer-only: every check runs from a checkout.
Installing and operating the runtime is [runtime installation](runtime-install.md).

## Checks

| Command | Where CI runs it |
| --- | --- |
| `crw-dev ci validate` | `validate`: skill metadata, local Markdown links, the syntax of the Python developer tools left (`scripts/dev/ALLOWED_PYTHON.txt`, which todo 48 removes) |
| `crw-dev ci plugin` | `validate`: plugin package shape, payload hygiene and the recorded version digest ([below](#plugin-package)) |
| `crw-dev ci contracts` | `validate`: the offline contract checks built into `crw-dev`: the hook replay, the operations shape check (`crw-dev ci operations`), the component definition, the start-policy self-test and the parent-title replay |
| `bash scripts/ci/secrets.sh` | `secrets`: checksum-pinned Gitleaks scan of all fetched history |
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
evidence). There is no path selection. The Go product legs always ran whatever changed, so
selecting the rest by changed paths saved little and put a job before every other one.

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
Makefile part has a leg, the integration step runs in `dist` after the build, and the gate's
script, run with a written results file, passes only an all-success run.

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

Secret scanning is not proof that every private fact or credential was detected. Review fixtures
and publication history separately. A PR can change the scanner and workflow, so review those
changes as changes to the gate itself. Do not upload secret-bearing findings; keep output
redacted and repair with the owner.

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
