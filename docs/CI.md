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

See the [workflow](../.github/workflows/ci.yml) for exact job inputs and Python
versions. All PR bases receive the same offline checks. The default checkout is
GitHub's combined merge candidate. Base retargeting invalidates prior evidence;
inspect the new run and candidate before integration. No docs-only selection
exists because the primary product is instructions in Markdown.

Each check has independent state. The final gate runs even after a prerequisite
fails and rejects missing, malformed, failed, cancelled and skipped results.
`dev-gate` covers development targets; `release-gate` additionally requires a
same-repository `dev -> main` promotion. Approval and release notes are reviewed
by the coordinator; the gate does not infer authorization from a branch name.

`scripts/ci/tests/test_gate.py` reads the workflow and requires the aggregator's
job set to equal the jobs the workflow actually defines, both gates to wait for all
of them, and no job to carry `continue-on-error`. Adding a job without requiring it,
or requiring a job that does not exist, fails that test rather than producing a gate
that silently covers less than it appears to.

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

Use `dev` as default and the normal PR target. Preserve `main` as the release
line. Creating `dev` at the existing `main` revision is branch setup, not a
release; moving new product changes into `main` is a release promotion.

1. Publish this policy and workflow in a Ready PR against `dev`; verify actual
   hosted check results, workflow validity and current head/base.
2. Integrate after the candidate and review gates pass. Existing task PRs must
   target `dev` and incorporate the CI revision before their integration.
3. Activate branch protection only after its required check exists. Use one
   source of protection per branch and read back the effective settings.

The intended protection settings are:

| Setting | `dev` | `main` |
| --- | --- | --- |
| PR required, current base, resolved conversations | Yes | Yes |
| Required check | `dev-gate` | `release-gate` |
| Required approving human reviews | 0 | 0 |
| Force push, deletion and bypass | Disallowed | Disallowed |
| Merge method | Merge commit | Merge commit |

Bind the check to its observed GitHub Actions producer. These are desired
settings, not proof that GitHub currently enforces them. Record activation and
readback in the PR. Do not enable a release gate by claiming a release was tested
without running it. Creating a promotion PR to validate CI does not authorize
merging or publishing that release.

Do not delete task branches or retained worktrees automatically during this
transition; open dependencies and private receipts can still refer to them.
Public visibility, automatic branch cleanup and distribution are separate scope.
If protection is unavailable, state that limitation and enforce the documented
checks in coordinator review; never label policy-only checks server-enforced.

Read state before and after activation:

```sh
gh api repos/OWNER/REPO --jq '{default_branch,visibility,allow_merge_commit,allow_squash_merge,allow_rebase_merge}'
gh api repos/OWNER/REPO/rulesets
gh api repos/OWNER/REPO/branches/dev/protection
gh api repos/OWNER/REPO/branches/main/protection
gh pr checks PR_NUMBER --repo OWNER/REPO
```

Replace the placeholders with the repository being operated on. Local passing
checks, hosted passing checks, protection settings, a merge and a live deployment
are distinct facts. Re-read current base/head and new findings before an
expected-head-guarded merge. Do not waive a failed gate in its own failure report.

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
reporting. Publishing a repository does not authorize a `dev -> main` promotion,
a tag, a package release, or runtime activation.

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
