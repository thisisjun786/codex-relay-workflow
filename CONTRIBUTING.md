# Contributing

Read [POLICY.md](POLICY.md) for branches, review and release rules and
[AGENTS.md](AGENTS.md) for source boundaries. Installation and skill usage are
in [README.md](README.md).

## Make a change

The skills live in `plugins/crw/skills/`, which is the payload of the published
plugin; `docs/plugin-packaging.md` describes the package and what may sit in its
root. Start from `dev` on a short-lived branch. Use a separate worktree when another
task owns the checkout, and preserve its uncommitted work. Read the target skill
and linked references before editing. Shared workflow rules belong in
`plugins/crw/skills/crw-plan/references/integrations.md`; operation-specific guidance
belongs with that skill.

Keep each PR focused on one outcome. Explain the triggering problem, expected
behavior and acceptance example in the PR even when there is a Linear link.
Access to the maintainer's private project is not a contribution prerequisite.
Korean and English contributions are welcome. Use only material you have the
right to contribute, preserve imported notices, and omit private task records
and credentials. Contributions are provided under this repository's
[MIT license](LICENSE). External dependencies retain their own licenses.

For bugs, open a GitHub issue with the source commit, relevant skill, host version,
expected behavior, and a minimal redacted reproduction. Use [SECURITY.md](SECURITY.md)
for vulnerabilities. Private Linear links may provide context, but keep enough
public detail in the issue or PR for contributors to understand the change.
Do not copy private Linear descriptions, attachments, or task transcripts into
public reports. Check integration settings before linking: a bot linkback may
copy an entire issue description, not just its URL.

## Check locally

The repository checks run from the development binary, with the Go toolchain `go.mod`
names:

```sh
go run -tags dev ./cmd/crw-dev ci validate
go run -tags dev ./cmd/crw-dev ci plugin
go run -tags dev ./cmd/crw-dev ci contracts
git diff --check
```

Changes to the runtime (`cmd/`, `internal/`, `contract/`) also need `make lint test`;
[CI operation](docs/CI.md) lists the parts CI splits that into.

The CXC v0.2.40 behaviour corpus (`contract/fixtures/cxc`) is recorded, not written by hand:
edit a spec under `contract/schema/cxc/specs/` and run
`go run -tags dev ./cmd/crw-dev cxc record --oracle <extracted v0.2.40 tree>`, which drives
the Node oracle in an isolated temporary root and refuses to write a fixture whose two
recordings differ; `ci contracts` runs `cxc lint` over it. See
[the corpus README](contract/schema/cxc/README.md).

The Python packages the runtime was ported from left the repository in todo 44; what stays of
them is the bridge's licence and provenance under `packages/codex-thread-bridge` and the relay's
documents under `docs/relay`. The CI checks are Go only (`crw-dev ci`); their Python twins left in
refactor R3. The port checkers, the last Python files, left in todo 48, so the repository tracks
no Python: `crw-dev ci validate` refuses a `.py` file or a python-shebang script.

Installer tests use temporary destinations; do not point test runs at your real
Codex skill directory. The bundled Codex skill validator, when installed, is an
additional check described in the README, not a hosted CI dependency.

Instruction changes need realistic scenario review as well as structural
validation. Reuse existing evidence for unchanged bytes and criteria. Keep live
Linear writes, worker creation, hook registration and service changes within
their separately authorized task scope. See [CI operation](docs/CI.md) for
scanner setup and the difference between offline checks and live proof.

## Publish for review

Open ordinary PRs against `dev`. When basic checks pass and the change can be
reviewed, open it Ready for review or mark the draft ready, then request review.
Handle findings, fixes and replies on that same PR, keeping it ready during
normal corrections. Include exact checks and their results, and identify
untested behavior. An absent check or review is not a pass.

The implementation owner handles reviews through resolution; the coordinator
checks the latest candidate before integration. A normal contribution does not
publish or deploy anything. The owner releases a verified dev commit with the
[Release workflow](docs/releases.md), which fast-forwards main to that exact SHA;
do not open a promotion PR. Release approval, a version tag and notes are required.

During implementation, run focused tests for the changed behavior. Reuse passing
evidence while its source, criteria and environment remain applicable. Hosted CI
runs every check on every PR and dev push, whatever the change touches, and
`dev-gate` passes only when all of them succeeded.
