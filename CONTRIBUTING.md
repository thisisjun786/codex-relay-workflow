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

Until the Python execution path is removed, the Python checks remain and CI runs them
too. They are developer-only, need Python 3.10 or newer and no dependency installation:

```sh
python3 scripts/ci/validate.py
python3 scripts/ci/plugin.py
python3 -m unittest discover -s scripts/ci/tests -v
```

The Python packages the runtime was ported from left the repository in todo 44; what stays of
them is the bridge's licence and provenance under `packages/codex-thread-bridge` and the relay's
documents under `docs/relay`.

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
selects checks by path; root prose and `docs/*.md` avoid expensive suites, while
executable skills and runtime changes retain their owning checks. A manifest change
still selects full coverage, including a derived plugin-version update. Unknown
paths need an explicit verification mapping before the gate can pass.
