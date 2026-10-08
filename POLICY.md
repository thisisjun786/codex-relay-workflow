# Repository policy

This repository owns its contribution, CI, merge and release rules. See
[AGENTS.md](AGENTS.md) for implementation boundaries,
[CONTRIBUTING.md](CONTRIBUTING.md) for contribution steps and
[CI operation](docs/CI.md) for commands and activation.
Linear owns product decisions; private task records hold raw operational evidence.

## Branches and authority

| Target | Purpose | Required evidence | Integration method |
| --- | --- | --- | --- |
| `dev` | Default branch; integrate completed work | A `verification-record/1` PASS for the merged tree | Fast-forward by the integrator |
| `main` | Mirror of the released source commit | The release workflow's own verification of the SHA | Fast-forward by the release workflow |

Start a short-lived `codex/` branch from `dev`. Keep all current branches and
worktrees until their work and dependencies have been reconciled. Do not rewrite
another task's history or discard dirty work.

Internal work integrates by one integrator. The integrator prepares the candidate,
merges it into a local integration tree over the current `dev`, obtains the
repository's local full verification, a `verification-record/1` with `result: pass`
for that merged tree, and fast-forwards `dev` to it. A pull request, a hosted gate,
a human approval and an external bot review are not integration conditions.
Independent review, the pre-merge evaluation, the local full verification and the
pre-push secret scan are kept, and a candidate whose verification fails does not land.

External contributions may still arrive as pull requests against `dev`. The
repository reads a contribution, verifies the tree it delivers and integrates it;
a PR is how one is proposed, not a condition every change has to satisfy.

The integrator re-reads the candidate and the destination tip immediately before the
push and verifies the landed commit. Refresh invalidated evidence after either input
changes. Serialize integrations into the same target. Never force-push, and never push
an unverified tree to an integration or release branch.

Release one exact, verified commit already on `dev`, with explicit owner
authorization, a version tag and user-facing release notes. The owner runs the
[Release workflow](docs/releases.md) from `dev`; it verifies the selected SHA itself
inside the workflow, publishes a GitHub source release, and fast-forwards `main` to
that same commit. There is no `dev -> main` promotion PR or new release merge
commit to reconcile. `main` accepts no development or promotion PRs; urgent fixes
first land on `dev` under the ordinary policy. A passing verification is not
authorization.

The release workflow defaults to a read-only dry-run. Publication needs the
repository's explicitly provisioned `RELEASE_TOKEN`. Existing tags may be reused
only for their original commit; tags and published releases are never rewritten.
An older or divergent source cannot replace `main`. Source release tags identify
repository commits independently of plugin and imported-package version numbers.

Public visibility, package publication, installation, service activation and
deployment remain separate operations. An ordinary integration authorizes none of
them and does not publish a source release.
If a push is known to trigger deployment, obtain deployment approval before
that push. Source checkout changes can affect linked skill reads immediately;
state that separately from installation and successful live operation.

## Verification

Local full verification is the primary route: `crw-dev ci local` runs every job and
step of `.github/workflows/ci.yml` locally, in a clean worktree of the commit being
verified, and writes a `verification-record/1`. The record is evidence about that
tree only; it names the commit, the tree, the pinned tools, the dependency digests,
the OS and the architecture, and its `result` is a pass only when every step
succeeded. See [CI operation](docs/CI.md).

GitHub Actions is an optional remote run. `.github/workflows/ci.yml` starts only on a
manual `workflow_dispatch` and runs the same jobs, so a developer can ask for the full
hosted verification of a commit; it is not integration or release evidence, because
integration no longer runs through a hosted event. The release workflow verifies the
SHA it will publish itself, inside the workflow.

| Evidence | What it establishes |
| --- | --- |
| `verification-record/1` from `crw-dev ci local` | Every job and step of `ci.yml` ran and succeeded on the verified tree, with the pinned tools |
| Skill metadata, local links and no Python outside skill assets | Repository structure, and that Python sits only in skill assets, so the runtime, installer and CI do not depend on it |
| Large-blob guard: no blob over 2 MiB that a commit brings into the history, unless `.large-blob-allowlist.json` names it | No oversized file enters the public `dev` history, which cannot drop it afterwards |
| Installer and skill-linker tests in temporary destinations (Go) | Idempotence and preservation of conflicting files, directories and links |
| CI-control negative tests | Missing, malformed, failed, cancelled or skipped prerequisites cannot pass the gate; an invalid release source is rejected |
| Go lint, the Go test suite with the contract corpus, static release binaries and the isolated-home install | The Go runtime builds, passes its tests and installs and wires from this checkout |
| Pinned secret scan over the commits a change adds, and the pre-push scan | No finding under the reviewed scanner configuration in the commits scanned |
| Owning offline contract checks, when present | Their documented parser, fixture or shape behavior |
| The `gui` job: the screens build on Node 24.20.0 and match the committed `internal/gui/assets` tree byte for byte | The screens embedded in `crw` are the build of `web/`, so a Node-free checkout serves the current screen |
| Independent scenario review | Instruction consistency and consequential edge cases within its scope |

Record exact commands, candidate revisions, tool versions and limitations in the
handoff or the coordination record, and reuse evidence only while its bytes, criteria
and environment remain applicable. A structural test does not prove the workflow's
meaning, and a fixture replay does not prove an actual Codex hook, relay delivery or
Desktop behavior.

The Go checks need only the Go toolchain `go.mod` names and temporary synthetic
data. The runtime, installer and CI do not depend on Python: CI installs none. Node
runs in the screen build and the staged skills' Node tests; no hook path or crw binary
needs Node, and a helper script in a skill's `scripts/` or `examples/` is an original
asset an agent runs when it needs it. Pin any
downloaded tooling by version, commit and checksum, and keep fixtures synthetic
and local. Ordinary CI does not need a
contributor's Codex, CXC, Linear account, App Server socket or user
skill installation. `crw-dev skills link` in the development binary links a checkout's skills
(its Python predecessor, `scripts/install.py`, left in todo 44). Cross-platform
symlink behavior and actual host compatibility need their own evidence before claiming support.

Use hosted Linux runners, pinned Action commits, bounded timeouts and a
read-only token where a workflow runs at all. Release runs serialize and do not
cancel each other. Do not expose production secrets, shared operational data or
self-hosted runners. Pin downloaded tooling and verify its checksum. CI-control edits
require review: a workflow a person can dispatch is still a change to a trust
boundary, so a green badge is not an immutable one.

The secret scanner reads the commits a change adds, and a local pre-push scan covers
what is about to be pushed.
Repository ignore files and inline allow comments must not suppress findings.
An exception requires an exact synthetic value and exact path with review;
never baseline away an unexplained finding. Keep private receipts, session
transcripts, personal paths and credentials out of commits and public reports.
Do not bundle dependencies or runtime state to make CI green. The one committed
generated output is the GUI deployment build under `internal/gui/assets`: the screens
are built with Node and embedded in the `crw` binary, so a Node-free checkout serves
them. `node_modules`, dependency caches and installed runtimes are never committed, the
lockfile stays pinned and the secret scan is unchanged. Source: the GUI port's approval
scope (Jun, 2026-10-06, "Node only builds the screens and runs the gui CI job").

## Issues, dependencies and release scope
One coherent result per issue. An issue is useful for coordinated product work,
but an external contributor need not access private Linear records to propose
a fix. A contribution must explain the expected behavior and acceptance criteria in
text; a private link alone is insufficient. Public GitHub issues can hold redacted
reproductions and contribution discussion. Product decisions remain in Linear.
For public repositories, keep private Linear references link-only and disable
automatic copying of private descriptions and attachments. Verify the integration's
public-repository linkback settings; repository prose does not enforce them.
Review public comments and their edit history as well as the Git diff before
publishing an existing private repository.

Keep the skills and their shared references consistent. CXC v0.2.40 (lidge-jun/codexclaw,
MIT) is being self-ported into the Go runtime with its MIT notice kept: [NOTICE](NOTICE)
carries the notices, [provenance](docs/port-cxc/provenance.md) the origin and
[known defects](docs/port-cxc/known-defects.md) the upstream defects the port records, one file per issue in [docs/port-cxc/known-defects/](docs/port-cxc/known-defects/).
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

## Work units, review and integration

### The units

A feature or acceptance unit, a parallel work packet and an integration bundle of prepared
changes are three different units, and policy does not force them into one. It no longer
requires issue = child = pull request = that pull request's own full CI = its own merge turn.
Every issue is traced from its acceptance criteria to a reviewed candidate to the actual
integration commit on its intended target; an implementation that delivers only part of the
criteria never closes the issue.

An issue carries one coherent unit of work: the implementation, the tests it requires, the
documentation directly tied to it and the review fixes it directly raises. Being verifiable on
its own permits a split; it does not require one. Line counts and file counts are references
only — never an assignment gate, never a forced split and never a gate a user must waive. Split
for a contract with value of its own, for a different responsibility, risk or deployment
boundary, or for a real benefit from parallel ownership. A direct defect found in a change is
fixed in that change rather than split off.

### Several packets in one feature

Policy allows several parallel packets for one feature with explicit owners: overlapping code,
shared types and fixtures each get an owner, and existing ids, branches and contracts are kept.
The capability is switched on per support, not by this policy text.

**Activation is per support.** Today the relay registers one child per issue relationship.
Separating a feature issue from a packet id, reserving and owning packets, and covering them are
the multi-packet support issue's; bundle integration and criteria mapping are the merge-train
issue's. Multi-packet work is switched on only after both are merged and installed and a real
acceptance has shown two packets of one feature with duplicate prevention, a final candidate and
a refusal to close the whole on a partial completion. Until then one issue runs as one packet.
A successful first single-packet bundle run is not multi-packet support. Never work around this
with wording or invented issue ids.

### Integration

Preparation and review run in parallel. The integrator takes the prepared changes, merges them
into one integration tree over the current `dev`, verifies that tree with the local full
verification, and fast-forwards `dev` to it. A risky or urgent change goes alone. A failure
removes or fixes the cause and what depends on it and re-verifies the changed tree; when the
destination moves, only the invalidated evidence is refreshed.

The relay still carries the PR path it was built with — a merge lane, a merge train and
`merge-evidence` — and that path is unchanged by this policy. While it runs, its lane is the
installed tool's route, not the repository's integration rule; where the collector or the lane
refuses the new flow, that is missing official support to report, and the integrator's route is
the manual one. Nothing lands without a passing verification.

### Review

The required independent review stays, and the revision it read and the disposition of every
important finding it raised are recorded. For the optional Devin and Codex reviews the fixed
time is a waiting budget of 45 minutes from the review's first signal; it is only a waiting
budget, never evidence that a review stalled or finished. Past that budget a
review still running is recorded as pending, not complete, and the wait is released only when the
current candidate's required independent review and its finding dispositions already exist;
missing independent review is obtained through the existing approved path. A late important
finding is judged against the current candidate. External bots are never re-requested,
interrupted or bypassed. The integrator checks valid evidence and the integration part and does
not rerun everything without reason or add duplicate approvals.

### Audit

Audit, scores and records are strengthened: repeated recording of one fact and approval round
trips are reduced, while criteria, history, model roles, review revisions and finding
dispositions are kept. Quantitative proof of the effect is not a condition for progress.

### Unchanged

No force push and no unverified integration. The candidate's local full verification and the
release workflow's own verification of the published SHA stay separate.

## Activation

Checked-in rules and successful local checks do not enable GitHub enforcement.
Follow [CI activation](docs/CI.md#activation),
and read back repository settings before claiming protection is active. Refresh invalidated
evidence after target changes. During iteration, use the affected tests. Do not repeat passing
checks for unchanged bytes, criteria and environments merely for extra confidence.
