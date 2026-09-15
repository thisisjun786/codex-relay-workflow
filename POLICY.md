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
| `main` | Release promotion from this repository's `dev` | `release-gate` | Merge commit |

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

Every `dev -> main` promotion requires explicit owner authorization for that
release and user-facing release notes. Only a same-repository `dev` PR may target
`main`; urgent fixes use the same path. A gate cannot establish the owner's
authorization. After promotion, reconcile its merge commit back into `dev` with
a PR before the next promotion.

Tags, public visibility, package publication, installation, service activation
and deployment are separate operations. Source merges do not authorize them.
If a merge is known to trigger deployment, obtain deployment approval before
that merge. Source checkout changes can affect linked skill reads immediately;
state that separately from installation and successful live operation.

## Verification

The graph is independent validation, tests and secret scanning, followed by a
result-only `dev-gate` or `release-gate`. Every PR runs all inexpensive checks;
skill Markdown changes executable instructions and is not a docs-only exemption.
Intermediate PR bases receive the same checks. There is no duplicate push suite,
live-service suite, automatic release or deployment workflow.

| Evidence | What it establishes |
| --- | --- |
| Skill metadata, local links and Python syntax | Repository structure and readable source |
| Installer subprocess tests in temporary destinations | Idempotence and preservation of conflicting files, directories and links |
| CI-control negative tests | Missing, malformed, failed, cancelled or skipped prerequisites cannot pass the aggregator; invalid promotions are rejected |
| Locked package install, resolved import locations, full suites, CLIs and wheel builds | The imported packages build, import and test from this checkout, with no empty collection and no skipped case |
| Pinned secret scan of available Git history | No finding under the reviewed scanner configuration in that fetched history |
| Owning offline contract checks, when present | Their documented parser, fixture or shape behavior |
| Independent scenario review | Instruction consistency and consequential edge cases within its scope |

The stable gate must run after failures and require every prerequisite to
succeed. A missing or skipped job is not success. Gate jobs do not rerun source
tests. Record exact commands, candidate revisions, check attempts and limitations
in the PR; reuse evidence only while its bytes, criteria and environment remain
applicable. A structural test does not prove the workflow's meaning, and a fixture
replay does not prove an actual Codex hook, relay delivery or Desktop behavior.

The skill and installer checks use Python's standard library and temporary
synthetic data. The `packages` check additionally needs uv and the dependencies
resolved in the root `uv.lock`; pin that tooling by version, commit and checksum,
and keep its own fixtures synthetic and local. Ordinary CI does not need a
contributor's Codex, CXC, Paperthin, Linear account, App Server socket or user
skill installation. Keep `scripts/install.py` standard-library-only. Validate the
documented minimum Python version in CI; cross-platform symlink behavior and
actual host compatibility need their own evidence before claiming support.

Use hosted Linux runners, pinned Action commits, bounded timeouts and a
read-only token. Cancel obsolete runs only within the same PR. PR code runs on
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

Keep the skills and their shared references consistent. CXC and Paperthin remain
external runtime dependencies; do not vendor their source. The task bridge and the
session relay are imported source under `packages/`, each keeping its own
`pyproject.toml`, tests and module names, and the root `uv.lock` resolves the
relay's bridge dependency to this checkout. Record tested versions, consumer
interfaces, old/new behavior and unresolved host observations. An upstream green
build is not this repository's compatibility proof. Do not copy private runtime
stores or update running installations as a CI side effect.

The `packages` check carries the install, test and compatibility burden for that
imported source, and every package change runs it. It installs from the lock file,
resolves both import locations inside `packages/`, runs each suite under pytest,
exercises both CLIs and builds both wheels. It rejects an empty collection and any
skipped case, because a suite that collected nothing and a suite that skipped its
real-bridge seams both report success otherwise. Passing it is evidence about this
source; it establishes nothing about an installed runtime, a live App Server, or
delivery on any host. Importing source does not change what is installed anywhere.

This repository uses the [MIT license](LICENSE). Preserve source attribution and
applicable notices for adapted material. Licensing does not authorize publication;
review history, private reporting and contributor readiness before making a
private repository public or publishing a release.

## Activation

Checked-in rules and successful local checks do not enable GitHub enforcement.
Follow [CI activation](docs/CI.md#activation), verify the first hosted gate,
and read back repository settings before claiming protection is active. Keep
in-progress PRs on the correct base and rerun checks after target changes.
