# Contributing

Read [POLICY.md](POLICY.md) for branches, review and release rules and
[AGENTS.md](AGENTS.md) for source boundaries. Installation and skill usage are
in [README.md](README.md).

## Make a change

The skills live in `plugins/crw/skills/`, which is the payload of the published
plugin; `docs/plugin-packaging.md` describes the package and what may sit in its
root. Start each issue from `origin/dev` in its own worktree and task branch.
Preserve uncommitted work, do not edit files another agent owns, and limit shared
document changes to your own section. Read the target skill
and linked references before editing. Shared workflow rules belong in
`plugins/crw/skills/crw-canon/references/` (indexed by `plugins/crw/skills/crw-canon/SKILL.md`);
operation-specific guidance belongs with that skill.

Keep each change focused on one coherent unit under [the work-unit rules](POLICY.md#work-units-review-and-integration). A PR explains the triggering problem,
expected behavior and acceptance example even when there is a Linear link; internal
work carries the same explanation in its coordination record.
Access to the maintainer's private project is not a contribution prerequisite.
Korean and English contributions are welcome. Use only material you have the
right to contribute, preserve imported notices, and omit private task records
and credentials. Contributions are provided under this repository's
[MIT license](LICENSE). External dependencies retain their own licenses.

For bugs, open a GitHub issue with the source commit, relevant skill, host version,
expected behavior, and a minimal redacted reproduction. Use [SECURITY.md](SECURITY.md)
for vulnerabilities. Private Linear links may provide context, but keep enough
public detail in the issue or PR for contributors to understand the change, and keep
the same public detail in a change that is integrated directly.
Do not copy private Linear descriptions, attachments, or task transcripts into
public reports. Check integration settings before linking: a bot linkback may
copy an entire issue description, not just its URL.

## Check locally

Follow the [development loop](AGENTS.md#development-loop). First add and run a failing
test, then fix its cause. Run tests for the changed packages and the packages that
import them with `go test -count=1` and the relevant package paths and build tags.
Then run `make lint-changed` and `git diff --check`. Do not skip tests or merely
increase waits to obtain a pass.

Use the Go toolchain named by `go.mod`. When skill documentation changes, run
`go run -tags dev ./cmd/crw-dev ci validate`; for plugin package changes, run
`go run -tags dev ./cmd/crw-dev ci plugin`, and for contract changes run
`go run -tags dev ./cmd/crw-dev ci contracts`. Set `TMPDIR` to a host-appropriate
temporary work directory with no Git repository (`.git`) at or above it.

Run one independent review, fix P0/P1 findings without a second review, and commit.
Before full verification, fetch and merge `origin/dev` into the task branch;
the branch's author resolves conflicts. On shared development machines, use the
configured `github-actions` verifier for full verification rather than running full
`crw-dev ci local` or full `make lint` locally. See
[Choosing the verifier](docs/CI.md#choosing-the-verifier) for configuration and the
common verification command.

Task branches may be pushed without another approval and remote branches are retained.
Do not push `dev` directly; the integration queue updates it after verification.
Its required `dev-gate` status rejects an update whose check is missing or unsuccessful;
a passing check does not grant direct-push authority.

The CXC v0.2.40 behaviour corpus (`contract/fixtures/cxc`) is recorded, not written by hand:
edit a spec under `contract/schema/cxc/specs/` and run
`go run -tags dev ./cmd/crw-dev cxc record --oracle <extracted v0.2.40 tree>`, which drives
the Node oracle in an isolated temporary root and refuses to write a fixture whose two
recordings differ; `ci contracts` runs `cxc lint` over it. See
[the corpus README](contract/schema/cxc/README.md).

The Python packages the runtime was ported from left the repository in todo 44; what stays of
them is the bridge's licence and provenance under `packages/codex-thread-bridge` and the relay's
documents under `docs/relay`. The CI checks are Go only (`crw-dev ci`); their Python twins left in
refactor R3. The port checkers, the last Python files, left in todo 48. The runtime, installer
and CI do not depend on Python: `crw-dev ci validate` refuses a `.py` file or a python-shebang
script anywhere but the `scripts/` and `examples/` directories of a skill (`plugins/crw/skills/*`,
the skills ported from CXC among them), whose helper scripts are original assets an
agent runs when it needs them, and CI installs no Python and runs none of them.

Installer tests use temporary destinations; do not point test runs at your real
Codex skill directory. The bundled Codex skill validator, when installed, is an
additional check described in the README, not a hosted CI dependency.

Instruction changes need realistic scenario review as well as structural
validation. Reuse existing evidence for unchanged bytes and criteria. Keep live
Linear writes, worker creation, hook registration and service changes within
their separately authorized task scope. See [CI operation](docs/CI.md) for
scanner setup and the difference between offline checks and live proof.

## Integrate

A contribution is proposed as an ordinary PR against `dev`, and that is the route an
external contributor uses. The maintainer reads it, verifies the tree it delivers with
the configured verifier (`local` or `github-actions`, a `verification-record/1` PASS), and
fast-forwards `dev` to the verified tree. The local verifier runs the checks locally;
GitHub mode dispatches the workflow for the exact head and waits for its job results.
See [Choosing the verifier](docs/CI.md#choosing-the-verifier). Include exact checks and their results, and identify
untested behavior. An absent check is not a pass.

The integrator owns the integration and handles review through resolution. A normal
contribution does not publish or deploy anything. The owner releases a verified dev
commit with the [Release workflow](docs/releases.md), which verifies that SHA itself
and fast-forwards main to it; do not open a promotion PR. Release approval, a version
tag and notes are required.

During implementation, run focused tests for the changed behavior. Reuse passing
evidence while its source, criteria and environment remain applicable.
