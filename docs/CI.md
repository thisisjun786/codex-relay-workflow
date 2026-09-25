# CI operation

[POLICY.md](../POLICY.md) owns the rules. This page maps them to commands and
the steps needed to activate GitHub enforcement.

## Checks

| Command | Purpose |
| --- | --- |
| `python3 scripts/ci/validate.py` | Skill metadata, local links and Python syntax |
| `python3 scripts/ci/plugin.py` | Plugin package shape, payload hygiene and the release digest |
| `python3 -m unittest discover -s scripts/ci/tests -v` | Installer behavior and CI-control tests |
| `python3 scripts/ci/contracts.py` | Run the owning hook replay and operations shape check when present; reject incomplete script/contract pairs |
| `python3 scripts/ci/packages.py` | Install, test, run and build the two packages under `packages/` from the root lock file |
| `bash scripts/ci/secrets.sh` | Checksum-pinned Gitleaks scan of all fetched history |
| `python3 scripts/ci/gate.py` | Aggregate prerequisite results supplied by the workflow |
| `make lint test contract` then `CGO_ENABLED=0 make dist` per target | `go-product` job: vet, staticcheck, gofmt, Go tests and the contract corpus, then static `crw` binaries for linux/amd64, linux/arm64 and darwin/arm64 uploaded with `SHA256SUMS` |

See the [workflow](../.github/workflows/ci.yml) for exact job inputs and Python
versions. PR validation uses GitHub's combined merge candidate; pushes to `dev`
validate the integrated commit. Retargeting invalidates prior coverage. Manual
CI dispatch runs all checks but does not produce release-eligible push evidence.

## Selection and aggregation

The standard-library [selector](../scripts/ci/scope.py) owns the path map. It
records the actual comparison base, candidate, event, changed paths, unknown
paths, selected jobs and reason. Renames include both old and new paths; mode and
file-type changes cannot obtain a prose exemption. Candidate inventory is checked
too, so an existing unregistered component cannot hide behind a docs-only diff.

| Change | Selected work |
| --- | --- |
| Named root prose files and Markdown directly under `docs/` | Validation, plugin identity, offline contracts and secrets |
| `plugins/crw/skills/**` or the root `skills` link | Above, plus installer/CI tests on Python 3.10 and 3.13 |
| Runtime, package, wiring, manifest, shared configuration or CI-control paths | All checks, including both package suites on Python 3.11 and 3.13 |
| Go product and contract corpus paths: `go.mod`, `go.sum`, `tools.go`, `Makefile`, `.goreleaser.yaml`, root `conftest.py`, `cmd/**`, `internal/**`, `contract/**`, `docs/port/**`, `scripts/port/**` | All checks |
| Mixed paths | Union of their coverage |
| Empty/unavailable diff or manual dispatch | Full coverage |
| Unmapped changed or candidate path | Full coverage; gate fails until the path is registered |

Skill Markdown contains executable instructions. A manifest version update paired
with a skill edit still selects full coverage; this selector does not infer a
version-only exemption from JSON contents. The PR template lives under `.github/`
and conservatively selects full coverage too.

`selection` runs first. Selected test and package jobs and `validate` then run
independently; secret scanning is independent. `contracts.py` runs once in
`validate`, not in both test-matrix legs. `dev-gate` always runs, requires selection
and the always-on producers to succeed, and accepts skipped jobs only when that
selection explicitly did not request them. Missing, malformed, failed, cancelled
and unexpected-skipped results fail. It rejects PRs targeting `main`.

`go-product` always runs, like `validate`: it needs no uv or Python packages, and
the darwin/arm64 binary it builds is not validated on a macOS host. Its plugin
payload step runs only once the native wiring launcher
`plugins/crw/wiring/crw-bridge.sh` exists; until then `validate` covers the
payload.

`test_gate.py` compares the gate's prerequisite inventory with the real workflow
and refuses omitted/extra jobs or `continue-on-error`. Selector tests use real Git
histories, including renames and unknown candidate files. Release tests execute
the release workflow's actual shell steps with disposable repositories and fake
GitHub responses; they never create a real release.

During iteration use the affected tests, and reuse valid evidence for unchanged
source, criteria and environments. Pure prose needs reading and link checks, not
assertions that freeze its wording. CI concurrency cancels obsolete runs within
the same PR or branch. An interrupted dev push is not release evidence: rerun
that exact push run if the owner later chooses its commit for release.

## Plugin package

`scripts/ci/plugin.py` runs as a step of the `validate` job, so the gate job set is
unchanged. It reads `.agents/plugins/marketplace.json` and the manifest under
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

`packages/codex-thread-bridge` and `packages/codex-session-relay` are one uv
workspace whose root `pyproject.toml` and `uv.lock` live at the repository root.
The relay declares the bridge with `tool.uv.sources` set to `workspace = true`, so
the dependency resolves to this checkout instead of an index.

The `packages` job runs on Python 3.11 and 3.13, which are the versions those
packages support. It installs with `--locked`, so a `pyproject.toml` edit without a
refreshed lock fails there. Three results are treated as failures rather than
successes, because each of them otherwise reads as a pass:

- A suite that collected nothing. `unittest discover` answers a wrong directory with
  "Ran 0 tests ... OK" and exit 0, so both suites run under pytest and their JUnit
  reports are read back for a nonzero count.
- A skipped case. The relay skips its real-bridge seams when `codex_thread_bridge`
  cannot be imported, which is the integration this repository now owns, so that skip
  is named explicitly and any other skip fails too. The relay's own conformance gate
  runs with `RELAY_CONFORMANCE_REQUIRED=1` for the same reason.
- An import satisfied by another copy. `codex_thread_bridge.__file__` and
  `codex_session_relay.__file__` are resolved and required to sit under
  `packages/<name>/src` before any test runs.

The job then runs both CLIs with `--help` and builds both wheels. All of this is
evidence about this source. It is not evidence about an installed bridge or relay,
an App Server socket, an MCP registration or delivery on any host; those remain
separate operations with their own authorization.

The bridge's worktree tests create Git repositories under the pytest temporary
directory, and a surrounding repository changes what they observe. The check refuses
to run when the temporary directory is inside a checkout and names
`CRW_PACKAGES_TMPDIR` as the override. Hosted CI is the authoritative run.

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
its ruff, ty, pytest and build steps informed the `packages` job, which currently
runs the pytest and build parts for both packages.

GitHub's [PR event reference](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request)
documents merge-candidate execution. Its [secure use reference](https://docs.github.com/en/actions/reference/security/secure-use)
describes pinned Actions and untrusted PR input. The scanner is pinned to
[Gitleaks 8.30.1](https://github.com/gitleaks/gitleaks/releases/tag/v8.30.1).
