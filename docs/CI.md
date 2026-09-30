# CI operation

[POLICY.md](../POLICY.md) owns the rules. This page maps them to commands and
the steps needed to activate GitHub enforcement. It is developer-only: every check
here runs from a checkout, and the Python scripts and tests it names stay until the
Python execution path is removed. Installing and operating the runtime is
[runtime installation](runtime-install.md).

## Checks

| Command | Purpose |
| --- | --- |
| `crw-dev ci scope` | Select checks from Git evidence (the `selection` job) |
| `crw-dev ci validate` | Skill metadata, local links and Python syntax |
| `crw-dev ci plugin` | Plugin package shape, payload hygiene and the release digest |
| `python3 -m unittest discover -s scripts/ci/tests -v` | Installer behavior and CI-control tests (CI runs the discovery once per Python version; see [the installer tests](#the-installer-tests)) |
| `crw-dev ci contracts` | Run the offline contract checks whose contract is present, each built into `crw-dev`: the hook replay, the operations shape check, the component definition's Go-retained fields (licences, the bridge identity tool, the compatibility links), the start-policy self-test and the parent-title replay. No Python checker script is needed (`scripts/ci/contracts.py`, which paired each contract with its script, was deleted in todo 44) |
| `crw-dev ci operations` | The operations fixtures against their contract (the Go port of `scripts/check_operations_contract.py`, also run by `contracts`) |
| `bash scripts/ci/secrets.sh` | Checksum-pinned Gitleaks scan of all fetched history |
| `crw-dev ci gate` | Aggregate prerequisite results supplied by the workflow |
| `make lint`, `make test-part TEST_PART=<1-5, rest>`, `CGO_ENABLED=0 make dist` per target | `go-product` job, one leg each: vet (also of the `dev` and `integration` tagged packages), staticcheck and gofmt; the Go tests and contract corpus as parts that together are `make test`; static `crw` binaries for linux/amd64, linux/arm64 and darwin/arm64 uploaded with `SHA256SUMS` |
| `CRW_TEST_BINARY=<crw> go test -tags integration ./internal/runtime/integration/...` | Install release archives of that binary (and a relinked second one) into an isolated home with `crw install`, and run the plugin's declared Stop hook and MCP server against them: install, cache replacement, a missing runtime, update, a refused archive, rollback and remove (IS-1, IS-7, IS-8). Without `CRW_TEST_BINARY` it builds `./cmd/crw` |

`crw-dev` is the development binary: `go build -tags dev -o dist/crw-dev ./cmd/crw-dev`
(or `make crw-dev`). It builds only with the `dev` tag, so `make dist` and the release
archives never contain it. Each `crw-dev ci` check replaces the Python script of the same
name under `scripts/ci/` with identical exit codes and output (except `contracts`, whose Go
side no longer runs a Python checker or refuses a contract for lacking one, and reports the
component definition by what the Go build takes from it); the remaining scripts and their
tests are developer tools that stay until todo 48 of the Go port.

See the [workflow](../.github/workflows/ci.yml) for exact job inputs and Python
versions. PR validation uses GitHub's combined merge candidate; pushes to `dev`
validate the integrated commit. Retargeting invalidates prior coverage. Manual
CI dispatch runs all checks but does not produce release-eligible push evidence.

## Selection and aggregation

The [selector](../internal/dev/ci/scope.go) owns the path map (ported from
[scope.py](../scripts/ci/scope.py), which keeps the same map until it is deleted). It
records the actual comparison base, candidate, event, changed paths, unknown
paths, selected jobs and reason. Renames include both old and new paths; mode and
file-type changes cannot obtain a prose exemption. Candidate inventory is checked
too, so an existing unregistered component cannot hide behind a docs-only diff.

| Change | Selected work |
| --- | --- |
| Named root prose files and Markdown directly under `docs/` | Validation, plugin identity, offline contracts and secrets |
| `plugins/crw/skills/**` or the root `skills` link | Above, plus installer/CI tests on Python 3.10 and 3.13 |
| Runtime, package, wiring, manifest, shared configuration or CI-control paths | All checks |
| Go product and contract corpus paths: `go.mod`, `go.sum`, `tools.go`, `Makefile`, `.goreleaser.yaml`, root `conftest.py`, `cmd/**`, `internal/**`, `contract/**`, `docs/port/**`, `scripts/port/**` | All checks |
| Mixed paths | Union of their coverage |
| Empty/unavailable diff or manual dispatch | Full coverage |
| Unmapped changed or candidate path | Full coverage; gate fails until the path is registered |

Skill Markdown contains executable instructions. A manifest version update paired
with a skill edit still selects full coverage; this selector does not infer a
version-only exemption from JSON contents. The PR template lives under `.github/`
and conservatively selects full coverage too.

Markdown under `packages/`, the package READMEs included, stays in the full class: every
file there belongs to a package, and the class is a path rule, not a judgement of the
content.

`selection` runs first. The selected test job and `validate` then run independently;
secret scanning is independent. The contract check runs once in
`validate`, not in both test-matrix legs. `dev-gate` always runs, requires selection
and the always-on producers to succeed, and accepts skipped jobs only when that
selection explicitly did not request them. Missing, malformed, failed, cancelled
and unexpected-skipped results fail. It rejects PRs targeting `main`.

`go-product` always runs, like `validate`: it needs only the Go toolchain, and
the darwin/arm64 binary it builds is not validated on a macOS host. Its legs run on
separate runners; the Makefile names the slowest packages as parts 1-5 and `rest` takes
every other package, so a new package is always tested. `test_gate.py` and
`internal/dev/ci` refuse a Makefile part without a workflow leg, and any job that still
syncs a uv workspace or runs the Python package suites. Its `dist` leg also checks the plugin
payload with `crw-dev ci plugin`; the step's condition, that the native wiring launcher
`plugins/crw/wiring/crw-bridge.sh` exists, holds since todo 34. It also runs the isolated-home
integration test against the linux/amd64 binary it built, with `CGO_ENABLED=0` and
`-trimpath` so the test reuses the dist build's compiled packages; `test_gate.py` and
`internal/dev/ci` pin that step, and `make lint` vets the tagged package so it cannot rot
outside `make test`.

`test_gate.py` and the Go tests in `internal/dev/ci` compare the gate's prerequisite
inventory with the real workflow and refuse omitted/extra jobs or `continue-on-error`. Selector tests use real Git
histories, including renames and unknown candidate files. Release tests execute
the release workflow's actual shell steps with disposable repositories and fake
GitHub responses; they never create a real release.

During iteration use the affected tests, and reuse valid evidence for unchanged
source, criteria and environments. Pure prose needs reading and link checks, not
assertions that freeze its wording. CI concurrency cancels obsolete runs within
the same PR or branch. An interrupted dev push is not release evidence: rerun
that exact push run if the owner later chooses its commit for release.

### The installer tests

The `tests` job runs `python3 -m unittest discover -s scripts/ci/tests -v` once per Python
version (3.10 and 3.13), so every module discovery would load runs and a new one needs no list
entry. Until todo 44 the job split its modules into a `heavy` and a `rest` leg per version,
because the Python installer's suites (`test_runtime_install`, `test_install_acceptance`)
made up half its time; those suites were deleted with the installer, and what remains fits one
leg. `test_gate.py` and `internal/dev/ci` check that the step is that one discovery and that no
leg split is left.

## Plugin package

`crw-dev ci plugin` (the port of `scripts/ci/plugin.py`, same flags and output) runs as a
step of the `validate` job, so the gate job set is unchanged. It reads `.agents/plugins/marketplace.json` and the manifest under
`plugins/crw/.codex-plugin/`, then rebuilds the release payload from a Git revision
with `git ls-tree` and `git cat-file` rather than from the working tree. That
revision is the oracle: comparing the working tree against itself would prove
nothing.

The check refuses what installs silently wrong. Installation copies the plugin root
verbatim, so a symlink inside it is dropped and its files disappear, while an
untracked or ignored file is published; both are rejected, along with anything
outside `.codex-plugin/`, `skills/` and `LICENSE`, operational state and credential
names, and a personal home path in any shipped instruction. It also requires each
declared skill to carry `SKILL.md` and `agents/openai.yaml`, and the repository
root link to point at the declared skills directory so the linked and packaged
installations cannot drift apart.

An empty directory is refused for the same reason: it carries no file for any other
rule to inspect and still reaches the cache. Shipped files must sit inside the
declared skills path, beside the files under `.codex-plugin/` and `LICENSE`; the
manifest may carry only keys the ingestion validator knows; and optional interface
fields are checked against its https, colour and asset shapes, so those
ingestion-shape mismatches are rejected here too. The working tree is checked with
the same rules as the revision, because a local marketplace installs it.

It also refuses a version two payloads could share. The manifest version carries the
payload's digest as build metadata, `0.4.0+640ccf7eadf4`, and the check re-derives that
suffix from the bytes and rejects a stale one, naming the value to record. The same rule
applies to the release payload, the working tree and an installed cache directory, which is
what lets `--payload <dir>` answer which bytes a cache holds rather than which name it sits
under. `--record-version` writes the derived suffix into the working-tree manifest, so the
recorded digest is never typed; see [plugin packaging](plugin-packaging.md#the-version-names-the-payload).

`--json` prints the payload digest and the namespaced skill names derived from the
revision, and `--payload <dir>` applies the same rules to an installed cache tree,
so an installed package can be compared with the source it came from. Passing is
evidence about this source. It does not establish that a plugin installed or that
any skill loaded on a host; see [plugin packaging](plugin-packaging.md).

## Packages

Until todo 44 a `packages` job installed the two Python packages under `packages/` from the
root `uv.lock` on Python 3.11 and 3.13, ran both suites under pytest (refusing an empty
collection, any skipped case and an import satisfied by another copy), ran both CLIs and built
both wheels. The Go port under `cmd/` and `internal/` is the product now: its tests and the
contract corpus carry the properties those suites protected ([test map](port/test-map.md)),
and the job, `scripts/ci/packages.py` and its duration table were deleted with the Python
implementation. All of this is evidence about this source. It is not evidence about an
installed runtime, an App Server socket, an MCP registration or delivery on any host; those
remain separate operations with their own authorization.

## Scope of the checks

The installer suite invokes the real CLI against temporary fixtures and leaves
the user's installed skills alone. The validator is repository-owned structural
validation, not the Codex validator or proof of instruction quality. Owning
contract probes, once present, remain limited to their stated offline coverage;
actual hook behavior, socket connectivity and successful handoff need separate
live evidence. The ordinary CI environment has no Linear credentials, CXC
installation, App Server or private runtime state.

The metadata checker supports this repository's single-line name/description
frontmatter and interface string fields; it is not a general YAML parser. Local
Markdown link checks cover inline link paths outside fenced examples, not remote
URLs, reference-style links or heading fragments. Expand the checker and its
tests when introducing another metadata format. Python syntax is checked for
every tracked or non-ignored source file.

## Secret scanning

The Linux scanner launcher pins Gitleaks 8.30.1 and verifies the downloaded
archive's SHA256. It scans the Git database from a temporary directory with an
explicit configuration and empty ignore file. Inline allow comments and
repository ignore files do not suppress findings. Fetch complete available
history; a shallow local checkout cannot establish full-history coverage.
Network/download/checksum failures fail the scan. No broad allowlist is supplied.

Secret scanning is not proof that every private fact or credential was detected.
Review fixtures and publication history separately. A PR can change the scanner
and workflow, so review those changes as changes to the gate itself. Do not
upload secret-bearing findings; keep output redacted and repair with the owner.

## Activation

Use `dev` as default and the normal PR target. `main` is a release mirror,
advanced by the owner-authorized [Release workflow](releases.md) to the exact
verified dev SHA. There is no promotion PR or release-specific CI gate.

Land CI/policy changes through the existing protected dev PR route first. Confirm
the new PR gate and the merged dev-push gate both actually succeeded. Then apply
the authorized protection changes and read back effective rules; never loosen a
current gate merely to land its replacement.

| Setting | `dev` | `main` |
| --- | --- | --- |
| PR required | Yes | No; release workflow advances the ref |
| Required check | `dev-gate` | `dev-gate` from the selected commit |
| Strict current-base requirement | Yes | No; unchanged verified commit is fast-forwarded |
| Required human approvals | 0 | Not a PR workflow |
| Resolved PR conversations | Yes | Not applicable |
| Force push, deletion and bypass | Disallowed | Disallowed |
| Update method | Merge commit through PR | Non-forced fast-forward |

Require stale-review dismissal on dev and bind checks to the observed GitHub
Actions producer. Protect version tags (`v*`) against update, deletion and
non-fast-forward with no bypass actors. The main rules protect ancestry and CI,
while owner-only workflow checks govern its release route; they do not prevent a
repository administrator from changing policy or using other authorized write
credentials. Do not describe that as an unbypassable workflow-only permission.

Read before and after configuration:

```sh
gh api repos/OWNER/REPO/rulesets
gh api repos/OWNER/REPO/rules/branches/dev
gh api repos/OWNER/REPO/rules/branches/main
gh pr checks PR_NUMBER --repo OWNER/REPO
```

Checked-in rules are not proof of server enforcement. If protection or release
credentials are missing, state the exact remaining prerequisite. Policy activation
does not authorize publishing a release, installation or deployment. Do not delete
existing branches or worktrees as part of this transition.

## Public repository activation

This is separate from branch/CI activation and requires explicit owner authority.
When private history contains operational receipts, preserve the private repository
and publish a reviewed source snapshot into a new repository. Do not transfer old
Git objects, PR comments, logs, or refs; do not use mirror pushes. Keep an explicit
local remote for the private archive and verify branch upstreams before pushing.

Before switching the destination public, review all refs it will expose and
verify that public Linear linkbacks cannot copy private descriptions. Review the
current source, source attribution, license, issues, comments and edit histories,
Actions logs, artifacts, and other enabled publication surfaces. Secret scanning
alone does not establish privacy. Keep raw findings outside Git.

Enable private vulnerability reporting as part of the visibility operation:

```sh
gh api --method PUT repos/OWNER/REPO/private-vulnerability-reporting
gh api repos/OWNER/REPO/private-vulnerability-reporting
```

Require `enabled: true`. If GitHub requires public visibility before enabling it,
prepare this operation beforehand, apply it immediately after the authorized
visibility change, and verify before announcing availability. Do not report the
publication complete while the private-report route is unavailable. Read back
visibility after any ambiguous API failure before retrying; an error does not
prove that a mutation had no effect.

Verify unauthenticated source access, MIT license detection, default `dev`, and
both branch rulesets after the change. Check the Security page exposes private
reporting. Publishing a repository does not authorize a source release, a tag, a package
release, or runtime activation.

## Adaptation sources

The policy and CI structure were adapted from Jun's Lina checkout at
`522101ff99e356b0ea6d27b4ea03ec7e599ee4b3`: `POLICY.md`, `docs/CI.md`,
`.github/workflows/ci.yml` and `scripts/ci/secrets.sh`. This repository keeps
its Python/skill checks and existing task ownership. Lina application builds,
Bun dependencies, deployment assumptions and personal operational data are not
part of this adaptation. Preserve upstream notices for any copied source.

The imported packages' provenance is recorded in [packages/README.md](../packages/README.md).
The bridge's own `.github/workflows/ci.yml` was not imported as a nested workflow;
its ruff, ty, pytest and build steps informed the `packages` job, which ran the pytest
and build parts for both packages until todo 44.

GitHub's [PR event reference](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request)
documents merge-candidate execution. Its [secure use reference](https://docs.github.com/en/actions/reference/security/secure-use)
describes pinned Actions and untrusted PR input. The scanner is pinned to
[Gitleaks 8.30.1](https://github.com/gitleaks/gitleaks/releases/tag/v8.30.1).
