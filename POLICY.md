# Repository policy

This repository owns its contribution, CI, merge and release rules. See
[AGENTS.md](AGENTS.md) for implementation boundaries,
[CONTRIBUTING.md](CONTRIBUTING.md) for contribution steps and
[CI operation](docs/CI.md) for commands and activation.
Linear owns product decisions; private task records hold raw operational evidence.

## Branches and authority

| Target | Purpose | Required check | Merge method |
| --- | --- | --- | --- |
| `dev` | Default branch; integrate completed work | `dev-gate` | Merge commit |
| `main` | Mirror of the released source commit | Exact-commit `dev-gate` from dev push CI | Fast-forward by the release workflow |

Start a short-lived `codex/` branch from `dev`; ordinary PRs target `dev`.
Dependent PRs may target another task branch when the dependency is explicit;
they receive development CI too. Native GitHub stacks are not the default.
Keep all current branches and worktrees until their work and dependencies have
been reconciled. Do not rewrite another task's history or discard dirty work.

The implementation owner carries the change through basic checks, commit, push,
Ready for review, and review resolution on the same PR. A draft is useful while
work is incomplete; mark it ready **before** requesting review. Keep it ready
during ordinary corrections. A restricted child can hand off an exact patch and
hashes for a capable coordinator to publish; restrictions do not create another
user approval requirement for an already authorized action.

The coordinator checks product criteria and the current PR diff, base, head,
CI and review findings, then may merge into `dev` within the authorized delivery
scope without asking again. Review-only, local-only and explicit merge holds
remain limits. Required human approval count is zero. Obtain independent review
appropriate to the change; inspect applicable hosted reviews through completion
and resolve confirmed defects. No particular bot is mandatory unless separately
configured. If an optional review is unavailable, record the limitation and use
sufficient independent evidence under the shared merge-readiness procedure.

Before merging, require the current candidate's green gate, a current base, no
conflicts, Ready status, resolved review conversations and no material unresolved
finding. Re-read head/base immediately before merging and use an expected-head
guard. Refresh invalidated checks after either input changes. Serialize merges
into the same target and verify the landed commit. Never bypass protection,
force-push, or push changes directly to an integration/release branch.

Release one exact, verified commit already on `dev`, with explicit owner
authorization, a version tag and user-facing release notes. The owner runs the
[Release workflow](docs/releases.md) from `dev`; it verifies the latest dev-push
CI for that SHA, publishes a GitHub source release, and fast-forwards `main` to
that same commit. There is no `dev -> main` promotion PR or new release merge
commit to reconcile. `main` accepts no development or promotion PRs; urgent fixes
first land on `dev` under the ordinary policy. A green gate is not authorization.

The release workflow defaults to a read-only dry-run. Publication needs the
repository's explicitly provisioned `RELEASE_TOKEN`. Existing tags may be reused
only for their original commit; tags and published releases are never rewritten.
An older or divergent source cannot replace `main`. Source release tags identify
repository commits independently of plugin and imported-package version numbers.

Public visibility, package publication, installation, service activation and
deployment remain separate operations. An ordinary dev merge authorizes none of
them and does not publish a source release.
If a merge is known to trigger deployment, obtain deployment approval before
that merge. Source checkout changes can affect linked skill reads immediately;
state that separately from installation and successful live operation.

## Verification

CI runs every check on every event, in parallel, and reports one result-only
`dev-gate`: validation, plugin identity, offline contracts, secret scanning and the
Go product checks (lint, the test parts, the release binaries and the isolated-home
install). Nothing is selected by changed paths. See [CI operation](docs/CI.md).

Every PR base receives the checks and the final gate, including explicit dependent
PRs; main-target PRs fail. A push to `dev` also runs CI on the integrated commit,
providing the exact-SHA evidence used for release. PR merge-candidate evidence
cannot replace it. No live-service suite or automatic publication/deployment runs.
The release workflow is manual and owner-controlled.

| Evidence | What it establishes |
| --- | --- |
| Skill metadata, local links and no Python outside skill assets | Repository structure, and that Python sits only in skill assets, so the runtime, installer and CI do not depend on it |
| Installer and skill-linker tests in temporary destinations (Go) | Idempotence and preservation of conflicting files, directories and links |
| CI-control negative tests | Missing, malformed, failed, cancelled or skipped prerequisites cannot pass the gate; main-target PRs and invalid release sources are rejected |
| Go lint, the Go test suite with the contract corpus, static release binaries and the isolated-home install | The Go runtime builds, passes its tests and installs and wires from this checkout |
| Pinned secret scan of available Git history | No finding under the reviewed scanner configuration in that fetched history |
| Owning offline contract checks, when present | Their documented parser, fixture or shape behavior |
| Independent scenario review | Instruction consistency and consequential edge cases within its scope |

The stable gate runs after failures. Every job it needs must succeed; a missing,
malformed, failed, cancelled or skipped result refuses the gate, and no job can
silently opt out. The gate does not rerun source tests. Record exact commands, candidate revisions, check attempts and limitations
in the PR; reuse evidence only while its bytes, criteria and environment remain
applicable. A structural test does not prove the workflow's meaning, and a fixture
replay does not prove an actual Codex hook, relay delivery or Desktop behavior.

The Go checks need only the Go toolchain `go.mod` names and temporary synthetic
data. The runtime, installer and CI do not depend on Python: CI installs none and runs
no skill script, and a helper script in a skill's `scripts/` or `examples/` is an original
asset an agent runs when it needs it. Pin any
downloaded tooling by version, commit and checksum, and keep fixtures synthetic
and local. Ordinary CI does not need a
contributor's Codex, CXC, Linear account, App Server socket or user
skill installation. `crw-dev skills link` in the development binary links a checkout's skills
(its Python predecessor, `scripts/install.py`, left in todo 44). Cross-platform
symlink behavior and actual host compatibility need their own evidence before claiming support.

Use hosted Linux runners, pinned Action commits, bounded timeouts and a
read-only CI token. Cancel obsolete CI runs only within the same PR or branch.
Release runs serialize and do not cancel each other. PR code runs on
`pull_request` merge candidates, never in a privileged `pull_request_target`
job. Do not expose production secrets, shared operational data or self-hosted
runners. Pin downloaded tooling and verify its checksum. CI-control edits
require review: a PR can edit its own workflow, so a green badge is not an
immutable trust boundary.

The secret scanner reads all fetched history including merge-parent diffs.
Repository ignore files and inline allow comments must not suppress findings.
An exception requires an exact synthetic value and exact path with review;
never baseline away an unexplained finding. Keep private receipts, session
transcripts, personal paths and credentials out of commits and public reports.
Do not bundle dependencies or runtime state to make CI green.

## Issues, dependencies and release scope

One coherent result per PR. An issue is useful for coordinated product work,
but an external contributor need not access private Linear records to propose
a fix. PRs must explain the expected behavior and acceptance criteria in text;
a private link alone is insufficient. Public GitHub issues can hold redacted
reproductions and contribution discussion. Product decisions remain in Linear.
For public repositories, keep private Linear references link-only and disable
automatic copying of private descriptions and attachments. Verify the integration's
public-repository linkback settings; repository prose does not enforce them.
Review public PR comments and their edit history as well as the Git diff before
publishing an existing private repository.

Keep the skills and their shared references consistent. CXC v0.2.40 (lidge-jun/codexclaw,
MIT) is being self-ported into the Go runtime with its MIT notice kept: [NOTICE](NOTICE)
carries the notices, [provenance](docs/port-cxc/provenance.md) the origin and
[known defects](docs/port-cxc/known-defects.md) the upstream defects the port records.
Until the port is switched on, installed skills and hooks still run against the
installed CXC plugin. The task bridge and the
session relay began as imported source under `packages/`; their Go ports are the product,
and the bridge's upstream MIT notice and provenance stay in `packages/codex-thread-bridge`.
Record tested versions, consumer
interfaces, old/new behavior and unresolved host observations. An upstream green
build is not this repository's compatibility proof. Do not copy private runtime
stores or update running installations as a CI side effect.

Until todo 44 a `packages` check installed, tested and built the imported Python
source. The Go port under `cmd/` and `internal/` replaced it as the product, the
Python source is removed, and the Go checks carry the test burden: passing them is
evidence about this source; it establishes nothing about an installed runtime, a live
App Server, or delivery on any host. Changing source does not change what is installed
anywhere.

This repository uses the [MIT license](LICENSE). Preserve source attribution and
applicable notices for adapted material; the notices for ported CXC material are in
[NOTICE](NOTICE). Licensing does not authorize publication;
review history, private reporting and contributor readiness before making a
private repository public or publishing a release.

## Activation

Checked-in rules and successful local checks do not enable GitHub enforcement.
Follow [CI activation](docs/CI.md#activation), verify the first hosted gate,
and read back repository settings before claiming protection is active. Keep
in-progress PRs on the correct base and refresh invalidated evidence after target
changes. During iteration, use the affected tests. Do not repeat passing checks
for unchanged bytes, criteria and environments merely for extra confidence.
